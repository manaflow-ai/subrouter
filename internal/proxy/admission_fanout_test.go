package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/selectacct"
	"github.com/manaflow-ai/subrouter/session"
)

func TestTrafficReportsPerSessionFanOutWithoutTheSessionID(t *testing.T) {
	now := time.Now()
	const id = "a@example.com"
	const sessionID = "fanoutse-7c1f-4b0e-9d55-2f1e5a7b9c3d"
	release := make(chan struct{})
	arrived := make(chan struct{}, 4)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		arrived <- struct{}{}
		<-release
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"type":"message","content":[]}`))
	}))
	t.Cleanup(upstream.Close)
	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	control := &admissionControl{limit: 1, queueCap: 4, buckets: make(map[admissionKey]*admissionBucket)}
	handler := Server{
		Sessions:       store,
		ClaudeUpstream: mustParseURL(t, upstream.URL),
		Accounts: []accounts.Account{{
			ID: id, Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "tok-" + id,
		}},
		SchedulerRef: selectacct.NewSchedulerRef(selectacct.NewScheduler([]selectacct.Score{
			scoreFromUsageWindows(accounts.ProviderClaude, id, []accounts.UsageWindow{
				fiveHour(10, now.Add(2*time.Hour)), sevenDay(10, now.Add(72*time.Hour)),
			}),
		})),
		MaxBodyBytes: 1 << 20,
		admission:    control,
	}.Handler()

	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req := httptest.NewRequest(http.MethodPost, "http://subrouter.local/v1/messages",
				strings.NewReader(`{"model":"claude-opus-4-8","messages":[]}`))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-Subrouter-Agent", "claude")
			req.Header.Set("X-Subrouter-Session", sessionID)
			handler.ServeHTTP(httptest.NewRecorder(), req)
		}()
	}
	select {
	case <-arrived:
	case <-time.After(2 * time.Second):
		t.Fatal("no request reached upstream")
	}
	traffic := func() (string, admissionSnapshot) {
		t.Helper()
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "http://subrouter.local/_subrouter/traffic", nil))
		var body struct {
			Admission admissionSnapshot `json:"admission"`
		}
		if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
			t.Fatalf("traffic body: %v: %s", err, recorder.Body.String())
		}
		return recorder.Body.String(), body.Admission
	}
	deadline := time.Now().Add(2 * time.Second)
	var raw string
	var snapshot admissionSnapshot
	for {
		raw, snapshot = traffic()
		if len(snapshot.Sessions) == 1 && snapshot.Sessions[0].Queued == 1 {
			break
		}
		if time.Now().After(deadline) {
			close(release)
			t.Fatalf("sessions = %+v, want one session with 1 active and 1 queued", snapshot.Sessions)
		}
		time.Sleep(5 * time.Millisecond)
	}
	got := snapshot.Sessions[0]
	if got.Agent != "claude" || got.SessionPrefix != sessionID[:8] || got.Active != 1 || got.Peak != 2 {
		close(release)
		t.Fatalf("session = %+v, want claude %q active 1 queued 1 peak 2", got, sessionID[:8])
	}
	if got.AccountHash != admissionAccountHash(id) {
		close(release)
		t.Fatalf("account hash = %q, want %q", got.AccountHash, admissionAccountHash(id))
	}
	if strings.Contains(raw, sessionID) {
		close(release)
		t.Fatalf("unauthenticated traffic endpoint published the full session id: %s", raw)
	}

	close(release)
	wg.Wait()
	_, snapshot = traffic()
	if len(snapshot.Sessions) != 1 {
		t.Fatalf("sessions after drain = %+v", snapshot.Sessions)
	}
	if done := snapshot.Sessions[0]; done.Active != 0 || done.Queued != 0 || done.Served != 2 || done.Peak != 2 {
		t.Fatalf("session after drain = %+v, want active 0 queued 0 served 2 peak 2", done)
	}
}
