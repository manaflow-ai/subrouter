package proxy

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/selectacct"
	"github.com/manaflow-ai/subrouter/session"
)

func singleTokenUsageRow(t *testing.T, recorder *TokenUsageRecorder, match func(TokenUsageRow) bool) TokenUsageRow {
	t.Helper()
	rows, err := recorder.Rows(recorder.clock().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	var found []TokenUsageRow
	for _, row := range rows {
		if match == nil || match(row) {
			found = append(found, row)
		}
	}
	if len(found) != 1 {
		t.Fatalf("rows = %+v, want one match", rows)
	}
	return found[0]
}

// testTokenUsageClock is a settable clock injected at construction.
type testTokenUsageClock struct{ now time.Time }

func (c *testTokenUsageClock) Now() time.Time { return c.now }

func newTestTokenUsageRecorder(path string) (*TokenUsageRecorder, *testTokenUsageClock) {
	clock := &testTokenUsageClock{now: time.Date(2026, 9, 27, 10, 0, 0, 0, time.UTC)}
	return NewTokenUsageRecorder(path, nil, WithTokenUsageClock(clock.Now)), clock
}

func tokenUsageSwitchTotals(t *testing.T, recorder *TokenUsageRecorder) (switches, coldInput, inRequest int64) {
	t.Helper()
	rows, err := recorder.Rows(recorder.clock().Add(-24 * time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		switches += row.AccountSwitches
		coldInput += row.AccountSwitchInputTokens
		inRequest += row.AccountSwitchesInRequest
	}
	return switches, coldInput, inRequest
}

func TestTokenUsageSwitchCountsSessionAccountMoves(t *testing.T) {
	recorder, clock := newTestTokenUsageRecorder("")
	turn := func(account, placed string, input, cached int64) {
		recorder.recordTurn("claude", account, "claude-opus-4-5", "c", tokenUsage{InputTokens: input, CachedInputTokens: cached}, true,
			tokenUsageTurn{sessionKey: tokenUsageSessionKey(accounts.ProviderClaude, "claude", "s1"), sessionModel: "opus", placedAccountID: placed})
	}
	turn("acct-a", "acct-a", 100, 0)   // first turn: nothing to lose
	turn("acct-a", "acct-a", 200, 150) // sticky
	turn("acct-b", "acct-a", 300, 0)   // failover inside the request
	turn("acct-c", "acct-c", 400, 100) // moved at placement; 300 of it cold
	turn("acct-a", "acct-a", 450, 400) // acct-a's cache is still warm
	clock.now = clock.now.Add(2 * time.Hour)
	turn("acct-d", "acct-d", 500, 0) // every cache is past its lifetime
	recorder.recordTurn("claude", "acct-a", "claude-opus-4-5", "c", tokenUsage{InputTokens: 7}, true,
		tokenUsageTurn{sessionKey: tokenUsageSessionKey(accounts.ProviderClaude, "claude", "s2"), sessionModel: "opus"})
	recorder.recordTurn("claude", "acct-a", "claude-opus-4-5", "c", tokenUsage{}, false, tokenUsageTurn{errorStatus: 429})
	if switches, cold, inRequest := tokenUsageSwitchTotals(t, recorder); switches != 2 || cold != 600 || inRequest != 1 {
		t.Fatalf("switches=%d cold=%d in_request=%d, want 2 600 1", switches, cold, inRequest)
	}
}

// Concurrent requests of one session that finish out of order, on the
// accounts a single failover moved between, count that failover once.
func TestTokenUsageSwitchCountsOneFailoverAcrossConcurrentTurns(t *testing.T) {
	recorder, clock := newTestTokenUsageRecorder("")
	turn := func(account string) {
		clock.now = clock.now.Add(time.Second)
		recorder.recordTurn("codex", account, "gpt-5", "c", tokenUsage{InputTokens: 10}, true,
			tokenUsageTurn{sessionKey: tokenUsageSessionKey(accounts.ProviderCodex, "codex", "s1"), sessionModel: "gpt-5"})
	}
	turn("acct-a")
	turn("acct-b") // the failover
	turn("acct-a") // a request that started before it finishes on acct-a
	turn("acct-b")
	turn("acct-a")
	if switches, _, _ := tokenUsageSwitchTotals(t, recorder); switches != 1 {
		t.Fatalf("switches = %d, want 1", switches)
	}
	// The set holds a few accounts; a fifth distinct one pushes the least
	// recently seen out, so returning to it is a switch again.
	for _, account := range []string{"acct-c", "acct-d", "acct-e"} {
		turn(account)
	}
	if switches, _, _ := tokenUsageSwitchTotals(t, recorder); switches != 4 {
		t.Fatalf("switches = %d after three new accounts, want 4", switches)
	}
	turn("acct-b") // acct-e replaced acct-b, the least recently seen
	if switches, _, _ := tokenUsageSwitchTotals(t, recorder); switches != 5 {
		t.Fatalf("switches = %d after returning to an evicted account, want 5", switches)
	}
}

// Each model keeps its own prompt cache, so a session's side calls on
// another model and account are not switches, and fallback turns are not
// counted at all.
func TestTokenUsageSwitchIsPerModelAndSkipsFallback(t *testing.T) {
	recorder, _ := newTestTokenUsageRecorder("")
	key := tokenUsageSessionKey(accounts.ProviderClaude, "claude", "s1")
	turn := func(account, model string) {
		recorder.recordTurn("claude", account, model, "c", tokenUsage{InputTokens: 10}, true,
			tokenUsageTurn{sessionKey: key, sessionModel: model})
	}
	for range 3 {
		turn("acct-a", "fable")
		turn(tokenUsageFallbackAccount, "fable")
		turn("acct-b", "haiku")
	}
	if switches, _, _ := tokenUsageSwitchTotals(t, recorder); switches != 0 {
		t.Fatalf("switches = %d, want 0", switches)
	}
	turn("acct-c", "haiku")
	if switches, _, _ := tokenUsageSwitchTotals(t, recorder); switches != 1 {
		t.Fatalf("switches = %d after a haiku move, want 1", switches)
	}
}

// Past the cap the least recently seen sessions are forgotten one at a
// time; recent sessions keep their accounts.
func TestTokenUsageSwitchSessionMemoryEvictsOldest(t *testing.T) {
	recorder, clock := newTestTokenUsageRecorder("")
	turn := func(session int, account string) {
		recorder.recordTurn("codex", account, "gpt-5", "c", tokenUsage{InputTokens: 1}, true,
			tokenUsageTurn{sessionKey: fmt.Sprintf("codex\x00codex\x00s%d", session)})
	}
	for i := range tokenUsageMaxSessions + 10 {
		clock.now = clock.now.Add(time.Millisecond)
		turn(i, "acct-a")
	}
	recorder.mu.Lock()
	size, ordered := len(recorder.sessions), recorder.sessionOrder.Len()
	recorder.mu.Unlock()
	if size != tokenUsageMaxSessions || ordered != size {
		t.Fatalf("session memory = %d entries (%d ordered), want %d", size, ordered, tokenUsageMaxSessions)
	}
	turn(10, "acct-b") // the oldest kept session
	if switches, _, _ := tokenUsageSwitchTotals(t, recorder); switches != 1 {
		t.Fatalf("switches = %d, want 1: a kept session lost its account", switches)
	}
	turn(0, "acct-b") // evicted, so a first turn again
	if switches, _, _ := tokenUsageSwitchTotals(t, recorder); switches != 1 {
		t.Fatalf("switches = %d, want 1: an evicted session still counted", switches)
	}
}

func TestTokenUsageTrackedSessionSkipsOneShotIDs(t *testing.T) {
	request := func(headers map[string]string) *http.Request {
		r := httptest.NewRequest(http.MethodPost, "http://subrouter.example/v1/responses", nil)
		for name, value := range headers {
			r.Header.Set(name, value)
		}
		return r
	}
	cases := []struct {
		name    string
		r       *http.Request
		id      string
		tracked bool
	}{
		{"session header", request(map[string]string{"Session-Id": "thread-1"}), "thread-1", true},
		{"session header beside idempotency key", request(map[string]string{"Session-Id": "thread-1", "Idempotency-Key": "k1"}), "thread-1", true},
		{"idempotency key only", request(map[string]string{"Idempotency-Key": "k1"}), "k1", false},
		{"idempotency key beside query session", func() *http.Request {
			r := request(map[string]string{"Idempotency-Key": "k1"})
			r.URL.RawQuery = "session_id=thread-1"
			return r
		}(), "k1", false},
		{"connection hash", request(nil), "fallback:0123456789abcdef01234567", false},
		{"body id", request(nil), "prompt-cache-key-1", true},
		{"empty", request(nil), "", false},
	}
	for _, tc := range cases {
		if got := tokenUsageTrackedSession(tc.r, tc.id); got != tc.tracked {
			t.Errorf("%s: tracked = %v, want %v", tc.name, got, tc.tracked)
		}
	}
}

// The constructor compacts with the injected clock, so a log whose rows are
// recent by that clock keeps them whatever the wall clock says.
func TestTokenUsageClockOptionAppliesToStartupCompaction(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token-usage.jsonl")
	line := `{"hour":"2020-01-01T09:00:00Z","provider":"codex","account_id":"a","model":"m","client":"c","requests":1,"requests_without_usage":0,"input_tokens":0,"cached_input_tokens":0,"cache_write_input_tokens":0,"output_tokens":0,"reasoning_output_tokens":0}` + "\n"
	if err := os.WriteFile(path, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2020, 1, 1, 10, 0, 0, 0, time.UTC)
	recorder := NewTokenUsageRecorder(path, nil, WithTokenUsageClock(func() time.Time { return now }))
	rows, err := recorder.Rows(now.Add(-24 * time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %+v, want the 2020 row kept by the injected clock", rows)
	}
}

func TestTokenUsageLatencyAggregatesAndEstimatesQuantiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "token-usage.jsonl")
	now := time.Date(2026, 9, 27, 10, 0, 0, 0, time.UTC)
	clock := WithTokenUsageClock(func() time.Time { return now })
	first := NewTokenUsageRecorder(path, nil, clock)
	record := func(recorder *TokenUsageRecorder, ttfb, duration time.Duration) {
		recorder.recordTurn("codex", "acct-a", "gpt-5", "c", tokenUsage{InputTokens: 1}, true,
			tokenUsageTurn{ttfb: ttfb, ttfbOK: true, duration: duration, durationOK: true})
	}
	for range 18 {
		record(first, 300*time.Millisecond, 3*time.Second)
	}
	record(first, 3*time.Second, 20*time.Second)
	if err := first.Flush(); err != nil {
		t.Fatal(err)
	}
	// A second worker's delta row for the same key sums on load, and the
	// max is the larger of the two.
	second := NewTokenUsageRecorder(path, nil, clock)
	record(second, 9*time.Second, 70*time.Second)
	if err := second.Flush(); err != nil {
		t.Fatal(err)
	}
	row := singleTokenUsageRow(t, NewTokenUsageRecorder(path, nil, clock), nil)
	if row.TTFBCount != 20 || row.TTFBMsSum != 18*300+3000+9000 || row.TTFBMsMax != 9000 {
		t.Fatalf("ttfb = count %d sum %d max %d", row.TTFBCount, row.TTFBMsSum, row.TTFBMsMax)
	}
	if want := []int64{0, 18, 0, 0, 1, 0, 1}; !reflect.DeepEqual(row.TTFBMsBuckets, want) {
		t.Fatalf("ttfb buckets = %v, want %v", row.TTFBMsBuckets, want)
	}
	if row.DurationCount != 20 || row.DurationMsMax != 70000 {
		t.Fatalf("duration = %+v", row)
	}
	if p50, ok := TokenUsageLatencyQuantileMs(row.TTFBMsBuckets, row.TTFBMsMax, 0.5); !ok || p50 != 500 {
		t.Fatalf("p50 = %d %v, want 500", p50, ok)
	}
	if p95, ok := TokenUsageLatencyQuantileMs(row.TTFBMsBuckets, row.TTFBMsMax, 0.95); !ok || p95 != 4000 {
		t.Fatalf("p95 = %d %v, want 4000", p95, ok)
	}
	// Past the last bound the estimate is the observed max.
	if top, ok := TokenUsageLatencyQuantileMs([]int64{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1}, 400000, 0.95); !ok || top != 400000 {
		t.Fatalf("overflow quantile = %d %v", top, ok)
	}
	if _, ok := TokenUsageLatencyQuantileMs(nil, 0, 0.95); ok {
		t.Fatal("empty buckets estimated a quantile")
	}
	// A bucket slice from a build with more buckets folds into the last.
	folded := tokenUsageLatencyFromRow(3, 0, 0, append(make([]int64, tokenUsageLatencyBuckets), 2, 1))
	if folded.Buckets[tokenUsageLatencyBuckets-1] != 3 {
		t.Fatalf("folded buckets = %v", folded.Buckets)
	}
}

func TestTokenUsageStopReasonsFromProviderShapes(t *testing.T) {
	bigReasoning := strings.Repeat("r", tokenUsageLineKeepBytes+100)
	cases := []struct {
		name, contentType, body, want string
		wholeMax                      int
	}{
		{
			name:        "anthropic stream",
			contentType: "text/event-stream",
			body: "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"model\":\"claude-opus-4-5\",\"stop_reason\":null,\"usage\":{\"input_tokens\":3}}}\n\n" +
				"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\",\"stop_sequence\":null},\"usage\":{\"output_tokens\":9}}\n\n",
			want: "tool_use",
		},
		{
			name:        "anthropic json",
			contentType: "application/json",
			body:        `{"type":"message","model":"claude-opus-4-5","stop_reason":"max_tokens","usage":{"input_tokens":3,"output_tokens":4}}`,
			want:        "max_tokens",
		},
		{
			name:        "chat stream finish without usage",
			contentType: "text/event-stream",
			body: "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"},\"finish_reason\":null}]}\n\n" +
				"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"length\"}]}\n\n" +
				"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":2}}\n\ndata: [DONE]\n\n",
			want: "length",
		},
		{
			name:        "responses completed",
			contentType: "text/event-stream",
			body:        "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n",
			want:        "completed",
		},
		{
			name:        "responses incomplete",
			contentType: "text/event-stream",
			body:        "data: {\"type\":\"response.incomplete\",\"response\":{\"status\":\"incomplete\",\"incomplete_details\":{\"reason\":\"max_output_tokens\"},\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n",
			want:        "incomplete:max_output_tokens",
		},
		{
			name:        "responses json",
			contentType: "application/json",
			body:        `{"object":"response","status":"incomplete","incomplete_details":{"reason":"content_filter"},"usage":{"input_tokens":1,"output_tokens":1}}`,
			want:        "incomplete:content_filter",
		},
		{
			name:        "responses truncated terminal event",
			contentType: "text/event-stream",
			body:        "data: {\"type\":\"response.incomplete\",\"response\":{\"status\":\"incomplete\",\"incomplete_details\":{\"reason\":\"max_output_tokens\"},\"output\":[{\"encrypted_content\":\"" + bigReasoning + "\"}],\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n",
			want:        "incomplete:max_output_tokens",
			wholeMax:    tokenUsageLineKeepBytes,
		},
		{
			name:        "text delta quoting a reason is not a stop",
			contentType: "text/event-stream",
			body:        "data: {\"type\":\"response.output_text.delta\",\"delta\":\"\\\"stop_reason\\\":\\\"x\\\"\"}\n\n",
			want:        "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			scanner := newTokenUsageScanner(tc.contentType)
			if tc.wholeMax > 0 {
				scanner = newTokenUsageScannerWithLimit(tc.contentType, tc.wholeMax)
			}
			scanner.Write([]byte(tc.body))
			scanner.Finish()
			if got := scanner.StopReason(); got != tc.want {
				t.Fatalf("stop = %q, want %q", got, tc.want)
			}
		})
	}
	_, _, stop, ok, terminal := tokenUsageTurnFromWebSocketMessage([]byte(`{"type":"response.failed","response":{"status":"failed","error":{"code":"server_error"}}}`))
	if stop != "failed" || ok || !terminal {
		t.Fatalf("websocket stop = %q ok=%v terminal=%v", stop, ok, terminal)
	}
}

func TestTokenUsageStopReasonAndErrorLabelsStayBounded(t *testing.T) {
	recorder, _ := newTestTokenUsageRecorder("")
	for i := range tokenUsageMaxLabelKeys + 5 {
		recorder.recordTurn("codex", "acct-a", "gpt-5", "c", tokenUsage{}, true, tokenUsageTurn{stopReason: fmt.Sprintf("reason_%d", i)})
	}
	recorder.recordTurn("codex", "acct-a", "gpt-5", "c", tokenUsage{}, true, tokenUsageTurn{stopReason: "Free text with spaces"})
	recorder.recordTurn("codex", "acct-a", "gpt-5", "c", tokenUsage{}, false, tokenUsageTurn{errorStatus: 429})
	recorder.recordTurn("codex", "acct-a", "gpt-5", "c", tokenUsage{}, false, tokenUsageTurn{errorStatus: 429})
	recorder.recordTurn("codex", "acct-a", "gpt-5", "c", tokenUsage{}, false, tokenUsageTurn{errorStatus: 500})
	row := singleTokenUsageRow(t, recorder, nil)
	if len(row.StopReasons) > tokenUsageMaxLabelKeys {
		t.Fatalf("stop reasons = %d keys, want at most %d", len(row.StopReasons), tokenUsageMaxLabelKeys)
	}
	var total int64
	for _, count := range row.StopReasons {
		total += count
	}
	if total != int64(tokenUsageMaxLabelKeys+6) || row.StopReasons["other"] == 0 {
		t.Fatalf("stop reasons = %v", row.StopReasons)
	}
	if !reflect.DeepEqual(row.UpstreamErrors, map[string]int64{"429": 2, "500": 1}) {
		t.Fatalf("upstream errors = %v", row.UpstreamErrors)
	}
	// Errors are not turns.
	if row.Requests != int64(tokenUsageMaxLabelKeys+6) {
		t.Fatalf("requests = %d", row.Requests)
	}
}

func TestTokenUsageRowsFromOlderLogsStillParse(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "token-usage.jsonl")
	old := `{"hour":"2026-09-27T09:00:00Z","provider":"codex","account_id":"acct-a","model":"gpt-5","client":"c","requests":2,"requests_without_usage":0,"input_tokens":10,"cached_input_tokens":4,"cache_write_input_tokens":0,"output_tokens":3,"reasoning_output_tokens":1}` + "\n"
	if err := os.WriteFile(path, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	recorder, _ := newTestTokenUsageRecorder(path)
	rows, err := recorder.Rows(time.Date(2026, 9, 27, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Requests != 2 || rows[0].InputTokens != 10 {
		t.Fatalf("rows = %+v", rows)
	}
	encoded, err := json.Marshal(rows[0])
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded)+"\n" != old {
		t.Fatalf("row without new data changed shape:\n%s\nwant\n%s", encoded, old)
	}
}

// Through the proxy: a session moved to another account counts one switch,
// latency and the stop reason land on the row, and an upstream error counts
// under upstream_errors without counting as a turn.
func TestProxyRecordsSwitchLatencyStopAndErrors(t *testing.T) {
	const sse = "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"model\":\"gpt-5-codex\",\"status\":\"completed\",\"usage\":{\"input_tokens\":40,\"output_tokens\":6}}}\n\n"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		if r.Header.Get("X-Test-Fail") != "" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":{"message":"bad request"}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, sse)
	}))
	defer upstream.Close()
	upstreamURL, _ := url.Parse(upstream.URL)
	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	recorder, _ := newTestTokenUsageRecorder("")
	handler := Server{
		Upstream: upstreamURL,
		Accounts: []accounts.Account{
			{ID: "codex-a", AuthMode: accounts.AuthModeOAuth, Token: "token-a", AccountID: "acct-a"},
			{ID: "codex-b", AuthMode: accounts.AuthModeOAuth, Token: "token-b", AccountID: "acct-b"},
		},
		Sessions:     store,
		Scheduler:    selectacct.NewScheduler(nil),
		MaxBodyBytes: 1 << 20,
		TokenUsage:   recorder,
	}.Handler()
	subrouter := httptest.NewServer(handler)
	defer subrouter.Close()

	post := func(account string, fail bool) {
		request, _ := http.NewRequest(http.MethodPost, subrouter.URL+"/v1/responses", strings.NewReader(`{"model":"gpt-5-codex","input":"hello"}`))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set(ClientNameHeader, "leos-mbp")
		request.Header.Set("X-Subrouter-Session", "switch-session")
		request.Header.Set("X-Subrouter-Account-ID", account)
		if fail {
			request.Header.Set("X-Test-Fail", "1")
		}
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, response.Body)
		_ = response.Body.Close()
	}
	post("codex-a", false)
	post("codex-a", false)
	post("codex-b", false)
	post("codex-b", true)

	// Requests named only by a one-shot Idempotency-Key, or by nothing (the
	// connection hash), stay out of the session memory.
	recorder.mu.Lock()
	tracked := len(recorder.sessions)
	recorder.mu.Unlock()
	for index, account := range []string{"codex-a", "codex-b", "codex-a"} {
		request, _ := http.NewRequest(http.MethodPost, subrouter.URL+"/v1/responses", strings.NewReader(`{"model":"gpt-5-codex","input":"hello"}`))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("X-Subrouter-Account-ID", account)
		request.Header.Set(ClientNameHeader, "one-shot")
		if index < 2 {
			request.Header.Set("Idempotency-Key", fmt.Sprintf("once-%d", index))
		}
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, response.Body)
		_ = response.Body.Close()
	}
	recorder.mu.Lock()
	trackedAfter := len(recorder.sessions)
	recorder.mu.Unlock()
	if trackedAfter != tracked {
		t.Fatalf("one-shot requests added %d session entries", trackedAfter-tracked)
	}

	rowA := singleTokenUsageRow(t, recorder, func(row TokenUsageRow) bool { return row.AccountID == "codex-a" && row.Client == "leos-mbp" })
	oneShot := func(row TokenUsageRow) bool { return row.Client == "one-shot" }
	if row := singleTokenUsageRow(t, recorder, func(row TokenUsageRow) bool { return oneShot(row) && row.AccountID == "codex-b" }); row.AccountSwitches != 0 {
		t.Fatalf("one-shot request counted a switch: %+v", row)
	}
	if rowA.Requests != 2 || rowA.AccountSwitches != 0 || rowA.TTFBCount != 2 || rowA.DurationCount != 2 ||
		!reflect.DeepEqual(rowA.StopReasons, map[string]int64{"completed": 2}) {
		t.Fatalf("codex-a row = %+v", rowA)
	}
	rowB := singleTokenUsageRow(t, recorder, func(row TokenUsageRow) bool { return row.AccountID == "codex-b" && row.Client == "leos-mbp" })
	if rowB.Requests != 1 || rowB.AccountSwitches != 1 || rowB.AccountSwitchInputTokens != 40 || rowB.AccountSwitchesInRequest != 0 ||
		!reflect.DeepEqual(rowB.UpstreamErrors, map[string]int64{"400": 1}) {
		t.Fatalf("codex-b row = %+v", rowB)
	}
}

// A WebSocket turn is timed from its response.create to the upstream's first
// message and to its terminal event, and carries its stop reason.
func TestProxyRecordsWebSocketTurnLatencyAndStop(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(_ *http.Request) bool { return true }}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade: %v", err)
			return
		}
		defer conn.Close()
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
			time.Sleep(20 * time.Millisecond)
			_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.output_text.delta","delta":"hi"}`))
			_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.incomplete","response":{"status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"usage":{"input_tokens":11,"output_tokens":3}}}`))
		}
	}))
	defer upstream.Close()
	upstreamURL, _ := url.Parse(upstream.URL)
	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	recorder, _ := newTestTokenUsageRecorder("")
	handler := Server{
		Upstream:     upstreamURL,
		Accounts:     []accounts.Account{{ID: "codex-a", AuthMode: accounts.AuthModeOAuth, Token: "token-a", AccountID: "acct-a"}},
		Sessions:     store,
		Scheduler:    selectacct.NewScheduler(nil),
		MaxBodyBytes: 1 << 20,
		TokenUsage:   recorder,
	}.Handler()
	subrouter := httptest.NewServer(handler)
	defer subrouter.Close()

	wsURL := "ws" + strings.TrimPrefix(subrouter.URL, "http") + "/v1/responses"
	conn, response, err := websocket.DefaultDialer.Dial(wsURL, http.Header{ClientNameHeader: []string{"leos-mbp"}})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer response.Body.Close()
	defer conn.Close()
	if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"gpt-5-codex","input":[]}`)); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, _, err := conn.ReadMessage(); err != nil {
			t.Fatal(err)
		}
	}
	row := singleTokenUsageRow(t, recorder, nil)
	if row.TTFBCount != 1 || row.DurationCount != 1 || row.TTFBMsSum < 20 || row.DurationMsSum < row.TTFBMsSum ||
		!reflect.DeepEqual(row.StopReasons, map[string]int64{"incomplete:max_output_tokens": 1}) {
		t.Fatalf("row = %+v", row)
	}
}
