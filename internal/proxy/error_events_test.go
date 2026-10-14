package proxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/selectacct"
	"github.com/manaflow-ai/subrouter/session"
)

const gatewayHTML406 = `<!doctype html><html><head><title>Only HTML requests are supported here</title></head><body><h1>406</h1></body></html>`

// TestErrorEventsRecordFailedAttemptsWithPoolState drives real requests
// through Server.Handler(): a 429 that fails over, then a gateway HTML 406.
// /_subrouter/errors must name both failures with the account that answered
// and the pool state at that moment, the client must still get the 406 page
// intact, and a new worker reading the same file must report the same events.
func TestErrorEventsRecordFailedAttemptsWithPoolState(t *testing.T) {
	var gateway atomic.Bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		auth := r.Header.Get("Authorization")
		switch {
		case gateway.Load():
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusNotAcceptable)
			_, _ = w.Write([]byte(gatewayHTML406))
		case strings.Contains(auth, "tok-cooked"):
			w.Header().Set("Retry-After", "3600")
			w.Header().Set("Anthropic-Ratelimit-Unified-Status", "rejected")
			w.Header().Set("Anthropic-Ratelimit-Unified-Reset", "1750000000")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(realisticAnthropic429Body))
		default:
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"id":"msg_ok","type":"message"}`))
		}
	}))
	defer upstream.Close()
	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	store, err := session.NewStore(filepath.Join(dir, "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put("claude", "session-errors", "cooked@example.com", ""); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	logPath := filepath.Join(dir, "error-events.jsonl")
	events := NewErrorEventLog(logPath, nil)
	done := make(chan struct{})
	go func() {
		events.Run(ctx)
		close(done)
	}()

	schedulerRef := selectacct.NewSchedulerRef(selectacct.NewScheduler([]selectacct.Score{
		{AccountID: "cooked@example.com", Provider: accounts.ProviderClaude, Headroom: 1, ShortHeadroom: 1},
		{AccountID: "fresh@example.com", Provider: accounts.ProviderClaude, Headroom: 0.6, ShortHeadroom: 0.6},
	}))
	handler := Server{
		ClaudeUpstream: upstreamURL,
		Accounts: []accounts.Account{
			{ID: "cooked@example.com", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "tok-cooked"},
			{ID: "fresh@example.com", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "tok-fresh"},
		},
		Sessions:     store,
		SchedulerRef: schedulerRef,
		MaxBodyBytes: 1 << 20,
		ErrorEvents:  events,
	}.Handler()
	subrouter := httptest.NewServer(handler)
	defer subrouter.Close()

	send := func() (int, string) {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, subrouter.URL+"/v1/messages", strings.NewReader(`{"model":"claude-opus-4-8","messages":[]}`))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Subrouter-Agent", "claude")
		req.Header.Set("X-Subrouter-Session", "session-errors")
		response, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		body, _ := io.ReadAll(response.Body)
		return response.StatusCode, string(body)
	}
	if status, body := send(); status != http.StatusOK {
		t.Fatalf("first request status = %d, want 200 after failover; body=%s", status, body)
	}
	gateway.Store(true)
	if status, body := send(); status != http.StatusNotAcceptable || body != gatewayHTML406 {
		t.Fatalf("gateway request = %d %q, want the 406 page passed through intact", status, body)
	}

	response, err := http.Get(subrouter.URL + ErrorEventsPath + "?provider=claude&since=1h")
	if err != nil {
		t.Fatal(err)
	}
	var report ErrorEventReport
	err = json.NewDecoder(response.Body).Decode(&report)
	response.Body.Close()
	if response.StatusCode != http.StatusOK || err != nil {
		t.Fatalf("GET %s = %d, decode err %v", ErrorEventsPath, response.StatusCode, err)
	}
	if len(report.Events) != 2 || report.Summary.Total != 2 {
		t.Fatalf("events = %+v, want the 406 and the 429", report.Events)
	}
	gatewayEvent, limitEvent := report.Events[0], report.Events[1]
	if gatewayEvent.Status != http.StatusNotAcceptable || gatewayEvent.Account != "fresh@example.com" ||
		gatewayEvent.ErrorType != "non_json" || gatewayEvent.Message != "Only HTML requests are supported here" {
		t.Fatalf("gateway event = %+v, want the 406 on fresh@example.com named by its page title", gatewayEvent)
	}
	if limitEvent.Status != http.StatusTooManyRequests || limitEvent.Account != "cooked@example.com" ||
		limitEvent.ErrorType != "rate_limit_error" || limitEvent.ErrorClass != "rate_limited" ||
		limitEvent.RetryAfter != "3600" || limitEvent.RateLimit["anthropic-ratelimit-unified-status"] != "rejected" {
		t.Fatalf("429 event = %+v, want cooked@example.com rate_limit_error with its reset headers", limitEvent)
	}
	snapshot, ok := report.Snapshots[jsonID(limitEvent.SnapshotID)]
	if !ok || snapshot.Total != 2 || len(snapshot.Accounts) != 2 {
		t.Fatalf("429 snapshot = %+v (found %v), want both Claude accounts", snapshot, ok)
	}
	states := map[string]AccountState{}
	for _, state := range snapshot.Accounts {
		states[state.Account] = state
	}
	if fresh := states["fresh@example.com"]; !fresh.Known || fresh.Headroom != 0.6 || !fresh.UsableNew || fresh.Blocked {
		t.Fatalf("fresh state = %+v, want known 0.6 headroom with room for a new session", fresh)
	}
	if report.Summary.ByStatus["429"] != 1 || report.Summary.ByStatus["406"] != 1 {
		t.Fatalf("summary = %+v, want one 429 and one 406", report.Summary)
	}

	// Live load lowers the placement score of a busy account; it must not
	// make the snapshot report that the account's quota has no room.
	for i := 0; i < 6; i++ {
		defer schedulerRef.BeginInflight(accounts.ProviderClaude, "fresh@example.com")()
	}
	response, err = http.Get(subrouter.URL + AccountStatePath + "?provider=claude")
	if err != nil {
		t.Fatal(err)
	}
	var pools []AccountPoolSnapshot
	err = json.NewDecoder(response.Body).Decode(&pools)
	response.Body.Close()
	if err != nil || len(pools) != 1 || pools[0].Total != 2 {
		t.Fatalf("GET %s = %+v, err %v; want one Claude pool of 2", AccountStatePath, pools, err)
	}
	for _, state := range pools[0].Accounts {
		if state.Account == "fresh@example.com" && (!state.UsableNew || state.Inflight != 6) {
			t.Fatalf("busy fresh account state = %+v, want usable_new with 6 in flight", state)
		}
	}

	// Stop the writer, then read the file the way the next worker does.
	cancel()
	<-done
	events.write(drainQueue(events))
	restarted := NewErrorEventLog(logPath, nil)
	restarted.load()
	reloaded := restarted.Query(ErrorEventQuery{Provider: "claude"})
	if len(reloaded.Events) != 2 || reloaded.Events[0].Status != http.StatusNotAcceptable ||
		reloaded.Snapshots[jsonID(reloaded.Events[1].SnapshotID)].Total != 2 {
		t.Fatalf("reloaded = %+v, want both events and their snapshot after a restart", reloaded)
	}
}

func jsonID(id uint64) string {
	data, _ := json.Marshal(id)
	return string(data)
}

func drainQueue(l *ErrorEventLog) []errorEventRecord {
	var out []errorEventRecord
	for {
		select {
		case record := <-l.queue:
			out = append(out, record)
		default:
			return out
		}
	}
}
