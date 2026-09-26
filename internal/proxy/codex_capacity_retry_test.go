package proxy

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// codexCapacityAttempt is one request the fake pool saw.
type codexCapacityAttempt struct {
	token  string
	marker string
	at     time.Time
	header http.Header
}

// codexCapacityPool answers capacity failures while fail returns true and
// completes otherwise. fail sees the account token and how many attempts the
// pool has seen before this one (0-based).
func codexCapacityPool(t *testing.T, fail func(token string, index int) bool) (*url.URL, func() []codexCapacityAttempt) {
	t.Helper()
	var mu sync.Mutex
	var seen []codexCapacityAttempt
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		marker := ""
		if _, after, ok := strings.Cut(string(body), `"marker":"`); ok {
			marker, _, _ = strings.Cut(after, `"`)
		}
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		mu.Lock()
		index := len(seen)
		seen = append(seen, codexCapacityAttempt{token: token, marker: marker, at: time.Now(), header: r.Header.Clone()})
		mu.Unlock()
		if fail(token, index) {
			codexEgressWriteOverloaded(w)
			return
		}
		codexEgressWriteCompleted(w, token)
	}))
	t.Cleanup(server.Close)
	poolURL, _ := url.Parse(server.URL)
	return poolURL, func() []codexCapacityAttempt {
		mu.Lock()
		defer mu.Unlock()
		return append([]codexCapacityAttempt(nil), seen...)
	}
}

// fastCapacityGaps shrinks every retry gap so the policy's shape can be
// tested without waiting out real jitter.
func fastCapacityGaps(config *CodexOverloadFailoverConfig, gap time.Duration) {
	fixed := func() time.Duration { return gap }
	config.sameAccountGap = fixed
	config.switchGap = fixed
	config.persistGap = fixed
}

func codexCapacityPost(ctx context.Context, t *testing.T, proxyURL, sessionID, marker string, headers map[string]string) (int, string, error) {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, proxyURL+"/responses",
		strings.NewReader(`{"model":"gpt-6-astra","session_id":"`+sessionID+`","marker":"`+marker+`","input":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	return response.StatusCode, string(body), nil
}

func tokens(attempts []codexCapacityAttempt) []string {
	out := make([]string, 0, len(attempts))
	for _, attempt := range attempts {
		out = append(out, attempt.token)
	}
	return out
}

// "Selected model is at capacity" is load shedding: the same request on the
// same account usually gets through a moment later. The first retry stays on
// the session's account (its prompt cache lives there) after a short
// jittered gap.
func TestCodexCapacityRetriesSameAccountFirst(t *testing.T) {
	poolURL, seen := codexCapacityPool(t, func(_ string, index int) bool { return index == 0 })
	server := codexOverloadServer(t, poolURL, 2, true)
	if _, err := server.Sessions.Put("codex", "session-same", "codex-account-0", ""); err != nil {
		t.Fatal(err)
	}
	proxy := httptest.NewServer(server.Handler())
	defer proxy.Close()

	status, body, err := codexCapacityPost(context.Background(), t, proxy.URL, "session-same", "a", nil)
	if err != nil || status != http.StatusOK || !strings.Contains(body, "served-from-oauth-token-0") {
		t.Fatalf("status=%d body=%s err=%v, want the same account to serve the retry", status, body, err)
	}
	got := seen()
	if strings.Join(tokens(got), ",") != "oauth-token-0,oauth-token-0" {
		t.Fatalf("pool saw %v, want account 0 twice", tokens(got))
	}
	if gap := got[1].at.Sub(got[0].at); gap < 250*time.Millisecond || gap > 1500*time.Millisecond {
		t.Fatalf("same-account retry gap = %v, want a 250-750ms jittered pause", gap)
	}
	if assignment, ok := server.Sessions.Get("codex", "session-same"); !ok || assignment.AccountID != "codex-account-0" {
		t.Fatalf("session moved to %+v, want it to stay on account 0", assignment)
	}
}

// When the same account fails again, the default policy moves on to the
// other accounts through the overload failover.
func TestCodexCapacityDefaultMovesToOtherAccountsAfterSameAccountRetry(t *testing.T) {
	poolURL, seen := codexCapacityPool(t, func(token string, _ int) bool { return token == "oauth-token-0" })
	server := codexOverloadServer(t, poolURL, 2, true)
	fastCapacityGaps(server.CodexOverloadFailover, 10*time.Millisecond)
	if _, err := server.Sessions.Put("codex", "session-move", "codex-account-0", ""); err != nil {
		t.Fatal(err)
	}
	proxy := httptest.NewServer(server.Handler())
	defer proxy.Close()

	status, body, err := codexCapacityPost(context.Background(), t, proxy.URL, "session-move", "a", nil)
	if err != nil || status != http.StatusOK || !strings.Contains(body, "served-from-oauth-token-1") {
		t.Fatalf("status=%d body=%s err=%v", status, body, err)
	}
	if got := strings.Join(tokens(seen()), ","); got != "oauth-token-0,oauth-token-0,oauth-token-1" {
		t.Fatalf("pool saw %s, want account 0 twice then account 1", got)
	}
}

// The default policy is bounded by time as well as by accounts: once the
// budget is spent, the failure goes back to the client rather than
// hammering a pool that is shedding load.
func TestCodexCapacityDefaultRetryStopsAtTimeBudget(t *testing.T) {
	poolURL, seen := codexCapacityPool(t, func(string, int) bool { return true })
	server := codexOverloadServer(t, poolURL, 8, true)
	server.CodexOverloadFailover.MaxAccounts = 7
	server.CodexOverloadFailover.RetryBudget = 250 * time.Millisecond
	fastCapacityGaps(server.CodexOverloadFailover, 100*time.Millisecond)
	proxy := httptest.NewServer(server.Handler())
	defer proxy.Close()

	started := time.Now()
	status, body, err := codexCapacityPost(context.Background(), t, proxy.URL, "session-budget", "a", nil)
	if err != nil || status != http.StatusOK || !strings.Contains(body, "server_is_overloaded") {
		t.Fatalf("status=%d body=%s err=%v, want the capacity failure passed through", status, body, err)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("default retry took %v, want it bounded by its budget", elapsed)
	}
	if n := len(seen()); n < 2 || n > 4 {
		t.Fatalf("pool saw %d attempts, want 2-4 inside a 250ms budget with 100ms gaps", n)
	}
}

// Persist mode, asked for per request, keeps retrying past the default
// ladder until the pool lets the request through. The control headers never
// reach the upstream.
func TestCodexCapacityPersistHeaderRetriesUntilSuccess(t *testing.T) {
	poolURL, seen := codexCapacityPool(t, func(_ string, index int) bool { return index < 9 })
	server := codexOverloadServer(t, poolURL, 2, true)
	fastCapacityGaps(server.CodexOverloadFailover, 5*time.Millisecond)
	proxy := httptest.NewServer(server.Handler())
	defer proxy.Close()

	status, body, err := codexCapacityPost(context.Background(), t, proxy.URL, "session-persist", "a", map[string]string{
		CodexCapacityRetryHeader: "persist",
	})
	if err != nil || status != http.StatusOK || !strings.Contains(body, "served-from-") || strings.Contains(body, "server_is_overloaded") {
		t.Fatalf("status=%d body=%s err=%v, want persist mode to reach a completion", status, body, err)
	}
	got := seen()
	if len(got) != 10 {
		t.Fatalf("pool saw %d attempts, want 10 (nine shed, then success)", len(got))
	}
	for _, attempt := range got {
		if attempt.header.Get(CodexCapacityRetryHeader) != "" || attempt.header.Get(CodexCapacityRetryBudgetHeader) != "" {
			t.Fatalf("capacity retry control headers leaked upstream: %v", attempt.header)
		}
	}
}

// Persist mode from the environment behaves the same, and its budget bounds
// the loop.
func TestCodexCapacityPersistEnvStopsAtBudget(t *testing.T) {
	poolURL, seen := codexCapacityPool(t, func(string, int) bool { return true })
	server := codexOverloadServer(t, poolURL, 2, true)
	server.CodexOverloadFailover.CapacityRetryPersist = true
	server.CodexOverloadFailover.CapacityRetryBudget = 400 * time.Millisecond
	fastCapacityGaps(server.CodexOverloadFailover, 20*time.Millisecond)
	proxy := httptest.NewServer(server.Handler())
	defer proxy.Close()

	started := time.Now()
	status, body, err := codexCapacityPost(context.Background(), t, proxy.URL, "session-env", "a", nil)
	if err != nil || status != http.StatusOK || !strings.Contains(body, "server_is_overloaded") {
		t.Fatalf("status=%d body=%s err=%v, want the failure after the persist budget", status, body, err)
	}
	elapsed := time.Since(started)
	if elapsed < 350*time.Millisecond || elapsed > 3*time.Second {
		t.Fatalf("persist loop ran %v, want about its 400ms budget", elapsed)
	}
	if n := len(seen()); n <= 5 {
		t.Fatalf("pool saw %d attempts, want persist mode to go past the default ladder", n)
	}
}

// A per-request header can shorten the persist budget, and "default" opts a
// request out of an environment-wide persist mode.
func TestCodexCapacityRetryHeaderOverrides(t *testing.T) {
	poolURL, seen := codexCapacityPool(t, func(string, int) bool { return true })
	server := codexOverloadServer(t, poolURL, 2, true)
	server.CodexOverloadFailover.CapacityRetryPersist = true
	server.CodexOverloadFailover.CapacityRetryBudget = time.Minute
	fastCapacityGaps(server.CodexOverloadFailover, 10*time.Millisecond)
	proxy := httptest.NewServer(server.Handler())
	defer proxy.Close()

	started := time.Now()
	if _, _, err := codexCapacityPost(context.Background(), t, proxy.URL, "session-h1", "a", map[string]string{
		CodexCapacityRetryBudgetHeader: "300ms",
	}); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Fatalf("budget header ignored: persist ran %v", elapsed)
	}
	before := len(seen())
	if _, _, err := codexCapacityPost(context.Background(), t, proxy.URL, "session-h2", "b", map[string]string{
		CodexCapacityRetryHeader: "default",
	}); err != nil {
		t.Fatal(err)
	}
	if n := len(seen()) - before; n != 3 {
		t.Fatalf("default-mode request made %d attempts, want 3 (first, same account, one switch)", n)
	}
}

// Client cancellation ends a persist loop right away.
func TestCodexCapacityPersistStopsOnClientCancel(t *testing.T) {
	poolURL, _ := codexCapacityPool(t, func(string, int) bool { return true })
	server := codexOverloadServer(t, poolURL, 2, true)
	fastCapacityGaps(server.CodexOverloadFailover, 50*time.Millisecond)
	proxy := httptest.NewServer(server.Handler())
	defer proxy.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, _, err := codexCapacityPost(ctx, t, proxy.URL, "session-cancel", "a", map[string]string{
		CodexCapacityRetryHeader: "persist",
	})
	if err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want the client's own deadline", err)
	}
	if elapsed := time.Since(started); elapsed > 1500*time.Millisecond {
		t.Fatalf("request returned after %v, want the cancel honored promptly", elapsed)
	}
}

// Only one persist loop per client session may be in flight; a concurrent
// request from the same session falls back to the default policy.
func TestCodexCapacityPersistOneLoopPerSession(t *testing.T) {
	poolURL, seen := codexCapacityPool(t, func(string, int) bool { return true })
	server := codexOverloadServer(t, poolURL, 2, true)
	server.CodexOverloadFailover.CapacityRetryBudget = 1500 * time.Millisecond
	fastCapacityGaps(server.CodexOverloadFailover, 20*time.Millisecond)
	proxy := httptest.NewServer(server.Handler())
	defer proxy.Close()

	persist := map[string]string{CodexCapacityRetryHeader: "persist"}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, _, _ = codexCapacityPost(context.Background(), t, proxy.URL, "session-guard", "first", persist)
	}()
	time.Sleep(400 * time.Millisecond)
	if _, _, err := codexCapacityPost(context.Background(), t, proxy.URL, "session-guard", "second", persist); err != nil {
		t.Fatal(err)
	}
	var second, other atomic.Int32
	for _, attempt := range seen() {
		switch attempt.marker {
		case "second":
			second.Add(1)
		case "":
			other.Add(1)
		}
	}
	wg.Wait()
	if other.Load() != 0 {
		t.Fatal("attempt without a marker")
	}
	if n := second.Load(); n > 5 {
		t.Fatalf("concurrent request from the same session made %d attempts, want the default ladder (<=5)", n)
	}
	first := 0
	for _, attempt := range seen() {
		if attempt.marker == "first" {
			first++
		}
	}
	if first <= 5 {
		t.Fatalf("first request made %d attempts, want it to keep persisting", first)
	}

	// Another session is not affected by the first session's loop.
	started := time.Now()
	if _, _, err := codexCapacityPost(context.Background(), t, proxy.URL, "session-other", "third", persist); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed < time.Second {
		t.Fatalf("another session's persist request ended after %v, want it to persist", elapsed)
	}
}

// Over the websocket transport each capacity reroute is a reconnect; a
// persisting session gets a wider reroute allowance than the default three.
func TestCodexCapacityPersistWidensWebSocketRerouteAllowance(t *testing.T) {
	server := Server{
		CodexOverloadFailover:      &CodexOverloadFailoverConfig{Enabled: true},
		codexOverloadRerouteCounts: newCodexOverloadReroutes(),
	}
	fastCapacityGaps(server.CodexOverloadFailover, time.Millisecond)
	allowed := func(session string, persist bool) int {
		n := 0
		for range 30 {
			if server.codexOverloadWebSocketReroute(context.Background(), "codex", session, "codex-account-0", "gpt-6-astra", "", nil, persist) {
				n++
			}
		}
		return n
	}
	if n := allowed("ws-default", false); n != codexOverloadMaxWebSocketReroutes {
		t.Fatalf("default session rerouted %d times, want %d", n, codexOverloadMaxWebSocketReroutes)
	}
	if n := allowed("ws-persist", true); n != codexOverloadMaxPersistWebSocketReroutes {
		t.Fatalf("persist session rerouted %d times, want %d", n, codexOverloadMaxPersistWebSocketReroutes)
	}
}

func TestParseCodexCapacityRetryBudget(t *testing.T) {
	cases := map[string]time.Duration{
		"30s":   30 * time.Second,
		"45":    45 * time.Second,
		"1.5m":  90 * time.Second,
		"2h":    codexCapacityMaxPersistBudget,
		"0":     0,
		"-3s":   0,
		"bogus": 0,
		"":      0,
	}
	for raw, want := range cases {
		if got := parseCodexCapacityRetryBudget(raw); got != want {
			t.Errorf("parseCodexCapacityRetryBudget(%q) = %v, want %v", raw, got, want)
		}
	}
}
