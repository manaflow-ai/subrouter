package proxy

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/selectacct"
)

// Prompt caches are per account, so by default an overloaded request stays on
// its account: switching turns the turn into a full cache miss. These tests
// pin that default and the opt-in switches.

// claudeShortLadder is a short same-account ladder (1s, 2s, 4s, 8s, 10s, 10s:
// six retries inside 35s) for tests about other behavior, so they can count
// attempts without walking the 8m default.
var claudeShortLadder = overloadRetryPolicy{interval: 10 * time.Second, maxWait: 35 * time.Second}

const claudeShortLadderRetries = 6

func claudeOverloaded529(header http.Header) *http.Response {
	if header == nil {
		header = http.Header{}
	}
	return &http.Response{StatusCode: 529, Header: header, Body: io.NopCloser(strings.NewReader(`{"type":"error","error":{"type":"overloaded_error"}}`))}
}

func claudeOverloadTransport(server *Server, sessionID, accountID string, stub http.RoundTripper, waits *[]time.Duration) usageLimitRetryTransport {
	return usageLimitRetryTransport{
		base: stub, server: server, provider: accounts.ProviderClaude,
		agent: "claude", session: sessionID, account: accountID,
		method: http.MethodPost, path: "/v1/messages", maxAttempts: replayablePostMaxAttempts,
		budget: newAttemptBudget(replayablePostMaxAttempts - 1),
		sleep:  recordSleep(waits),
	}
}

func claudeOverloadRequest(token string) *http.Request {
	req, _ := http.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages", bytes.NewReader([]byte(`{"model":"claude-opus-4-8"}`)))
	req.Header.Set("Authorization", "Bearer "+token)
	return req
}

func sumDurations(waits []time.Duration) time.Duration {
	var total time.Duration
	for _, wait := range waits {
		total += wait
	}
	return total
}

// Default: a sustained 529 is retried on the session's own account through the
// whole backoff ladder, never touches another account, and does not mark the
// account. The 529 then goes back to the client.
func TestClaudeOverloadDefaultStaysOnAccount(t *testing.T) {
	server, store := claudeFailoverServer(t)
	if _, err := store.Put("claude", "session-stay", "cooked@example.com", ""); err != nil {
		t.Fatal(err)
	}
	var auths []string
	stub := &stubRoundTripper{responses: func(req *http.Request) *http.Response {
		auths = append(auths, req.Header.Get("Authorization"))
		return claudeOverloaded529(nil)
	}}
	var waits []time.Duration
	transport := claudeOverloadTransport(&server, "session-stay", "cooked@example.com", stub, &waits)
	response, err := transport.RoundTrip(claudeOverloadRequest("tok-cooked"))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != 529 {
		t.Fatalf("status = %d, want the 529 passed through once the ladder is spent", response.StatusCode)
	}
	for i, auth := range auths {
		if auth != "Bearer tok-cooked" {
			t.Fatalf("attempt %d went to %q, want every attempt on the session's account", i, auth)
		}
	}
	// 1s, 2s, 4s, 8s, then a steady 15s until the next wait would end past
	// the 8m cap: 15s of ramp plus 31 steady waits is exactly 480s.
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second}
	for len(want) < 4+31 {
		want = append(want, 15*time.Second)
	}
	if len(waits) != len(want) || len(auths) != 1+len(want) {
		t.Fatalf("waits = %v (%d attempts), want %v", waits, len(auths), want)
	}
	for i := range want {
		if waits[i] != want[i] {
			t.Fatalf("waits = %v, want %v", waits, want)
		}
	}
	if total := sumDurations(waits); total != claudeOverloadDefaultMaxWait {
		t.Fatalf("total hold %v, want exactly the %v cap", total, claudeOverloadDefaultMaxWait)
	}
	if server.SchedulerRef.Get().Exhausted(accounts.ProviderClaude, "cooked@example.com") {
		t.Fatal("overload must not mark the account exhausted")
	}
	if got, ok := store.Get("claude", "session-stay"); !ok || got.AccountID != "cooked@example.com" {
		t.Fatalf("session assignment = %+v, want it to stay on cooked@example.com", got)
	}
}

// Default: an overload that clears late in the ladder is served from the same
// account, well past the old two-retry limit.
func TestClaudeOverloadDefaultRecoversLateOnSameAccount(t *testing.T) {
	server, store := claudeFailoverServer(t)
	if _, err := store.Put("claude", "session-late", "cooked@example.com", ""); err != nil {
		t.Fatal(err)
	}
	var auths []string
	stub := &stubRoundTripper{responses: func(req *http.Request) *http.Response {
		auths = append(auths, req.Header.Get("Authorization"))
		if len(auths) <= 4 {
			return claudeOverloaded529(nil)
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"id":"msg_same"}`))}
	}}
	var waits []time.Duration
	transport := claudeOverloadTransport(&server, "session-late", "cooked@example.com", stub, &waits)
	response, err := transport.RoundTrip(claudeOverloadRequest("tok-cooked"))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	if response.StatusCode != http.StatusOK || !strings.Contains(string(body), "msg_same") {
		t.Fatalf("status=%d body=%s, want the same account's 200", response.StatusCode, body)
	}
	if strings.Join(auths, ",") != strings.Repeat("Bearer tok-cooked,", 4)+"Bearer tok-cooked" {
		t.Fatalf("auth sequence = %v, want five attempts on the session's account", auths)
	}
	if got, ok := store.Get("claude", "session-late"); !ok || got.AccountID != "cooked@example.com" {
		t.Fatalf("session assignment = %+v, want cooked@example.com", got)
	}
}

// Default: a Retry-After is honored per wait but capped at 15s, and cannot
// stretch the hold past its cap.
func TestClaudeOverloadDefaultHoldIsBounded(t *testing.T) {
	server, _ := claudeFailoverServer(t)
	var calls int
	stub := &stubRoundTripper{responses: func(req *http.Request) *http.Response {
		calls++
		header := http.Header{}
		header.Set("Retry-After", "9999")
		return claudeOverloaded529(header)
	}}
	var waits []time.Duration
	transport := claudeOverloadTransport(&server, "s", "cooked@example.com", stub, &waits)
	response, err := transport.RoundTrip(claudeOverloadRequest("tok-cooked"))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != 529 {
		t.Fatalf("status = %d, want 529", response.StatusCode)
	}
	if total := sumDurations(waits); total > claudeOverloadDefaultMaxWait {
		t.Fatalf("waits = %v (total %v), exceed the %v cap", waits, total, claudeOverloadDefaultMaxWait)
	}
	for _, wait := range waits {
		if wait != claudeOverloadMaxRetryAfter {
			t.Fatalf("waits = %v, want every Retry-After wait capped at %v", waits, claudeOverloadMaxRetryAfter)
		}
	}
	if len(waits) != int(claudeOverloadDefaultMaxWait/claudeOverloadMaxRetryAfter) {
		t.Fatalf("waits = %d, want %d 15s waits inside 8m", len(waits), claudeOverloadDefaultMaxWait/claudeOverloadMaxRetryAfter)
	}
}

// Opt-in (SUBROUTER_CLAUDE_OVERLOAD_REROUTE=1): after the short same-account
// retries the request is rerouted exactly once to another account; the
// overloaded account is not marked and the session stays put.
func TestClaudeOverloadRerouteIsOptIn(t *testing.T) {
	server, store := claudeFailoverServer(t)
	server.ClaudeOverloadReroute = true
	if _, err := store.Put("claude", "session-optin", "cooked@example.com", ""); err != nil {
		t.Fatal(err)
	}
	var cookedCalls, freshCalls int
	stub := &stubRoundTripper{responses: func(req *http.Request) *http.Response {
		if strings.Contains(req.Header.Get("Authorization"), "tok-cooked") {
			cookedCalls++
			return claudeOverloaded529(nil)
		}
		freshCalls++
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"id":"msg_fresh"}`))}
	}}
	var waits []time.Duration
	transport := claudeOverloadTransport(&server, "session-optin", "cooked@example.com", stub, &waits)
	response, err := transport.RoundTrip(claudeOverloadRequest("tok-cooked"))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK || cookedCalls != 1+providerOverloadMaxRetries || freshCalls != 1 {
		t.Fatalf("status=%d cooked=%d fresh=%d, want one reroute after %d same-account retries", response.StatusCode, cookedCalls, freshCalls, providerOverloadMaxRetries)
	}
	if server.SchedulerRef.Get().Exhausted(accounts.ProviderClaude, "cooked@example.com") {
		t.Fatal("overload must not mark the first account exhausted")
	}
	if got, ok := store.Get("claude", "session-optin"); !ok || got.AccountID != "cooked@example.com" {
		t.Fatalf("session assignment = %+v, want cooked@example.com", got)
	}
}

// codexStayServer is a Codex pool with the account failover off but test
// gaps installed, which is the default policy with fast timing.
func codexStayServer(t *testing.T, fail func(token string, index int) bool, accountCount int) (Server, func() []codexCapacityAttempt) {
	t.Helper()
	poolURL, seen := codexCapacityPool(t, fail)
	server := codexOverloadServer(t, poolURL, accountCount, false)
	server.SchedulerRef = selectacct.NewSchedulerRef(selectacct.NewScheduler(nil))
	server.CodexOverloadFailover = &CodexOverloadFailoverConfig{}
	fastCapacityGaps(server.CodexOverloadFailover, 5*time.Millisecond)
	return server, seen
}

func assertOnlyToken(t *testing.T, attempts []codexCapacityAttempt, token string) {
	t.Helper()
	for i, attempt := range attempts {
		if attempt.token != token {
			t.Fatalf("attempt %d went to %s (all: %v), want every attempt on %s", i, attempt.token, tokens(attempts), token)
		}
	}
}

// With no configuration at all, a capacity failure is retried on the same
// account and the retry's success is served.
func TestCodexCapacityRetryOnByDefault(t *testing.T) {
	poolURL, seen := codexCapacityPool(t, func(_ string, index int) bool { return index == 0 })
	server := codexOverloadServer(t, poolURL, 2, false)
	// The unconfigured default, exactly as serve builds it without env.
	server.CodexOverloadFailover = nil
	if _, err := server.Sessions.Put("codex", "session-default", "codex-account-0", ""); err != nil {
		t.Fatal(err)
	}
	proxy := httptest.NewServer(server.Handler())
	defer proxy.Close()

	status, body, err := codexCapacityPost(context.Background(), t, proxy.URL, "session-default", "a", nil)
	if err != nil || status != http.StatusOK || !strings.Contains(body, "served-from-oauth-token-0") {
		t.Fatalf("status=%d body=%s err=%v, want the same account to serve the retry", status, body, err)
	}
	got := seen()
	if len(got) != 2 {
		t.Fatalf("pool saw %v, want the failure and one retry", tokens(got))
	}
	assertOnlyToken(t, got, "oauth-token-0")
	if assignment, ok := server.Sessions.Get("codex", "session-default"); !ok || assignment.AccountID != "codex-account-0" {
		t.Fatalf("session moved to %+v, want it to stay on account 0", assignment)
	}
}

// Failover off: a sustained capacity failure is retried on the session's
// account only, bounded, then passed through. The account is not marked, so
// the next turn does not get evicted from its prompt cache either.
func TestCodexCapacityDefaultStaysOnAccountThenPassesThrough(t *testing.T) {
	server, seen := codexStayServer(t, func(string, int) bool { return true }, 3)
	if _, err := server.Sessions.Put("codex", "session-stay", "codex-account-0", ""); err != nil {
		t.Fatal(err)
	}
	proxy := httptest.NewServer(server.Handler())
	defer proxy.Close()

	status, body, err := codexCapacityPost(context.Background(), t, proxy.URL, "session-stay", "a", nil)
	if err != nil || status != http.StatusOK || !strings.Contains(body, "server_is_overloaded") {
		t.Fatalf("status=%d body=%s err=%v, want the capacity failure passed through", status, body, err)
	}
	got := seen()
	if len(got) != 1+codexTestStayRetries {
		t.Fatalf("pool saw %d attempts %v, want %d (first try plus the same-account ladder)", len(got), tokens(got), 1+codexTestStayRetries)
	}
	assertOnlyToken(t, got, "oauth-token-0")
	if marks := server.SchedulerRef.CapacityMarks(accounts.ProviderCodex, "gpt-6-astra", selectacct.CapacityAnyTier); len(marks) != 0 {
		t.Fatalf("capacity marks = %v, want none while failover is off", marks)
	}
	if assignment, ok := server.Sessions.Get("codex", "session-stay"); !ok || assignment.AccountID != "codex-account-0" {
		t.Fatalf("session moved to %+v, want it to stay on account 0", assignment)
	}
}

// Failover off: success on a later same-account retry is what the client gets.
func TestCodexCapacityDefaultServesLaterSameAccountRetry(t *testing.T) {
	server, seen := codexStayServer(t, func(_ string, index int) bool { return index < 4 }, 3)
	if _, err := server.Sessions.Put("codex", "session-later", "codex-account-0", ""); err != nil {
		t.Fatal(err)
	}
	proxy := httptest.NewServer(server.Handler())
	defer proxy.Close()

	status, body, err := codexCapacityPost(context.Background(), t, proxy.URL, "session-later", "a", nil)
	if err != nil || status != http.StatusOK || !strings.Contains(body, "served-from-oauth-token-0") || strings.Contains(body, "server_is_overloaded") {
		t.Fatalf("status=%d body=%s err=%v, want the fifth attempt's completion", status, body, err)
	}
	got := seen()
	if len(got) != 5 {
		t.Fatalf("pool saw %v, want four failures and the success", tokens(got))
	}
	assertOnlyToken(t, got, "oauth-token-0")
}

// Failover off: persist mode works and stays on the session's account for
// its whole budget.
func TestCodexCapacityPersistWithoutFailoverStaysOnAccount(t *testing.T) {
	server, seen := codexStayServer(t, func(_ string, index int) bool { return index < 15 }, 3)
	// Without the failover the persist header needs the operator's opt-in.
	server.CodexOverloadFailover.CapacityRetryHeader = true
	// Persist is bounded by its budget, not the test count bound.
	server.CodexOverloadFailover.stayRetryLimit = 0
	if _, err := server.Sessions.Put("codex", "session-persist-stay", "codex-account-0", ""); err != nil {
		t.Fatal(err)
	}
	proxy := httptest.NewServer(server.Handler())
	defer proxy.Close()

	status, body, err := codexCapacityPost(context.Background(), t, proxy.URL, "session-persist-stay", "a", map[string]string{
		CodexCapacityRetryHeader: "persist",
	})
	if err != nil || status != http.StatusOK || !strings.Contains(body, "served-from-oauth-token-0") {
		t.Fatalf("status=%d body=%s err=%v, want persist mode to reach a completion on account 0", status, body, err)
	}
	got := seen()
	if len(got) != 16 {
		t.Fatalf("pool saw %d attempts, want 16 (fifteen shed, then success)", len(got))
	}
	assertOnlyToken(t, got, "oauth-token-0")
}

// The same-account ladder's shape: gaps double from ~0.5s to ~8s (±20%
// jitter), then hold at ~9s (8-10s), inside a 4m budget; the failover ladder
// keeps its ~10s.
func TestCodexCapacityStayLadderShape(t *testing.T) {
	var config *CodexOverloadFailoverConfig
	if got := config.retryBudget(); got != 4*time.Minute {
		t.Fatalf("unconfigured budget = %v, want 4m", got)
	}
	if got := (&CodexOverloadFailoverConfig{Enabled: true}).retryBudget(); got != codexCapacityDefaultRetryBudget {
		t.Fatalf("failover budget = %v, want %v", got, codexCapacityDefaultRetryBudget)
	}
	for retry, want := range []time.Duration{500 * time.Millisecond, time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second} {
		for range 20 {
			got := config.stayDelay(retry, codexCapacityDefaultStayInterval)
			low, high := time.Duration(float64(want)*0.8), time.Duration(float64(want)*1.2)
			if got < low || got > high {
				t.Fatalf("stayDelay(%d) = %v, want %v ±20%%", retry, got, want)
			}
		}
	}
	for retry := 5; retry < 40; retry++ {
		got := config.stayDelay(retry, codexCapacityDefaultStayInterval)
		if got < 8*time.Second || got > 10*time.Second {
			t.Fatalf("stayDelay(%d) = %v, want a steady 8-10s", retry, got)
		}
	}
	// A shorter interval caps the ramp: 0.5s, 1s, 2s, 2s, ...
	for retry, want := range []time.Duration{500 * time.Millisecond, time.Second, 2 * time.Second, 2 * time.Second, 2 * time.Second, 2 * time.Second} {
		got := config.stayDelay(retry, 2*time.Second)
		low, high := time.Duration(float64(want)*0.8), time.Duration(float64(want)*1.2)
		if got < low || got > high {
			t.Fatalf("stayDelay(%d, 2s) = %v, want %v with jitter", retry, got, want)
		}
	}
}

// With the failover off, the regional egress still gets its turn once the
// same-account ladder is spent.
func TestCodexCapacityDefaultLadderFallsThroughToEgress(t *testing.T) {
	pool := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if region := r.Header.Get(codexEgressHeader); region != "" {
			codexEgressWriteCompleted(w, region)
			return
		}
		codexEgressWriteOverloaded(w)
	}))
	defer pool.Close()
	poolURL, _ := url.Parse(pool.URL)
	var calls atomic.Int32
	fra := codexEgressTestProxy(t, "fra", &calls)
	server := codexEgressServer(t, poolURL, []*url.URL{fra}, 2)
	server.CodexOverloadFailover = &CodexOverloadFailoverConfig{}
	fastCapacityGaps(server.CodexOverloadFailover, time.Millisecond)
	proxy := httptest.NewServer(server.Handler())
	defer proxy.Close()

	status, body := codexEgressPost(t, proxy.URL, "session-stay-egress")
	if status != http.StatusOK || !strings.Contains(body, "served-from-fra") {
		t.Fatalf("status=%d body=%s, want the egress to serve after the same-account ladder", status, body)
	}
	if calls.Load() != 1 {
		t.Fatalf("egress calls=%d, want 1", calls.Load())
	}
}
