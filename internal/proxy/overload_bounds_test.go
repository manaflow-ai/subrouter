package proxy

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Bounds on the stay-on-account overload retries: the fallback cap, operator
// gating of the persist header, wall-clock budgets, prompt cancellation and
// the one-reroute-per-request limit.

// An egress or Azure fallback is the operator's deliberate next step for a
// shedding pool, so with the failover off the same-account ladder gives way
// to it after ~10s instead of ~30s.
func TestCodexCapacityStayBudgetCappedWhenFallbackConfigured(t *testing.T) {
	proxyURL := mustParseURL(t, "http://egress.example:3128")
	azure := &AzureCodexConfig{Endpoints: []AzureCodexEndpoint{{BaseURL: mustParseURL(t, "https://azure.example/openai/v1"), APIKey: "key"}}}
	cases := []struct {
		name   string
		server Server
		want   time.Duration
	}{
		{"no fallback", Server{}, codexCapacityDefaultStayMaxWait},
		{"egress", Server{CodexEgress: &CodexEgressConfig{Proxies: []*url.URL{proxyURL}}}, codexCapacityFallbackStayRetryBudget},
		{"azure", Server{AzureCodex: azure}, codexCapacityFallbackStayRetryBudget},
		{"azure without a usable endpoint", Server{AzureCodex: &AzureCodexConfig{}}, codexCapacityDefaultStayMaxWait},
		{"failover with egress", Server{
			CodexOverloadFailover: &CodexOverloadFailoverConfig{Enabled: true},
			CodexEgress:           &CodexEgressConfig{Proxies: []*url.URL{proxyURL}},
		}, codexCapacityDefaultRetryBudget},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			if got := test.server.codexCapacityRetryBudget(); got != test.want {
				t.Fatalf("budget = %v, want %v", got, test.want)
			}
			if got := test.server.codexDefaultRetryBudget("gpt-6-astra", ""); got != test.want {
				t.Fatalf("request budget = %v, want %v", got, test.want)
			}
		})
	}
	if codexCapacityFallbackStayRetryBudget != 10*time.Second {
		t.Fatalf("fallback stay budget = %v, want 10s", codexCapacityFallbackStayRetryBudget)
	}
}

// The persist header lets a client hold an upstream slot for up to 10m, so
// it counts only when the operator allows it: with the failover or with
// SUBROUTER_CODEX_CAPACITY_RETRY_HEADER=1. The operator's own persist setting
// works without either.
func TestCodexCapacityRetryHeaderNeedsOperatorOptIn(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "/responses", nil)
	request.Header.Set(CodexCapacityRetryHeader, "persist")
	request.Header.Set(CodexCapacityRetryBudgetHeader, "9m")
	cases := []struct {
		name        string
		config      *CodexOverloadFailoverConfig
		wantPersist bool
		wantBudget  time.Duration
	}{
		{"unconfigured", nil, false, codexCapacityDefaultPersistBudget},
		{"failover off", &CodexOverloadFailoverConfig{}, false, codexCapacityDefaultPersistBudget},
		{"header opt-in", &CodexOverloadFailoverConfig{CapacityRetryHeader: true}, true, 9 * time.Minute},
		{"failover on", &CodexOverloadFailoverConfig{Enabled: true}, true, 9 * time.Minute},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			policy := test.config.codexCapacityRetryPolicyFor(request)
			if policy.persist != test.wantPersist || policy.persistBudget != test.wantBudget {
				t.Fatalf("policy = %+v, want persist=%t budget=%v", policy, test.wantPersist, test.wantBudget)
			}
		})
	}

	// The operator's persist setting needs no failover and no header opt-in.
	operator := &CodexOverloadFailoverConfig{CapacityRetryPersist: true, CapacityRetryBudget: 90 * time.Second}
	if policy := operator.codexCapacityRetryPolicyFor(httptest.NewRequest(http.MethodPost, "/responses", nil)); !policy.persist || policy.persistBudget != 90*time.Second {
		t.Fatalf("operator persist policy = %+v, want persist with 90s", policy)
	}
	// Without the header opt-in a client can neither opt out of it nor
	// stretch its budget.
	ignored := httptest.NewRequest(http.MethodPost, "/responses", nil)
	ignored.Header.Set(CodexCapacityRetryHeader, "default")
	ignored.Header.Set(CodexCapacityRetryBudgetHeader, "9m")
	if policy := operator.codexCapacityRetryPolicyFor(ignored); !policy.persist || policy.persistBudget != 90*time.Second {
		t.Fatalf("operator persist policy with ignored headers = %+v, want persist with 90s", policy)
	}
}

// End to end: without an opt-in the persist header is ignored and the request
// gets the bounded same-account ladder.
func TestCodexCapacityPersistHeaderIgnoredByDefault(t *testing.T) {
	server, seen := codexStayServer(t, func(_ string, index int) bool { return index < 15 }, 3)
	proxy := httptest.NewServer(server.Handler())
	defer proxy.Close()

	status, body, err := codexCapacityPost(context.Background(), t, proxy.URL, "session-header-ignored", "a", map[string]string{
		CodexCapacityRetryHeader: "persist",
	})
	if err != nil || status != http.StatusOK || !strings.Contains(body, "server_is_overloaded") {
		t.Fatalf("status=%d body=%s err=%v, want the capacity failure after the default ladder", status, body, err)
	}
	if got := seen(); len(got) != 1+codexTestStayRetries {
		t.Fatalf("pool saw %d attempts, want %d: the header must not turn on persist", len(got), 1+codexTestStayRetries)
	}
}

func codexOverloadedResponse() *http.Response {
	recorder := httptest.NewRecorder()
	codexEgressWriteOverloaded(recorder)
	return recorder.Result()
}

// The Codex same-account budget is wall clock from the first attempt, so
// slow upstream attempts count against it, not only the gaps between them.
func TestCodexCapacityStayBudgetIsWallClock(t *testing.T) {
	var calls atomic.Int32
	base := roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		time.Sleep(30 * time.Millisecond)
		return codexOverloadedResponse(), nil
	})
	config := &CodexOverloadFailoverConfig{RetryBudget: 100 * time.Millisecond}
	fastCapacityGaps(config, time.Millisecond)
	transport := codexOverloadFailoverTransport{base: base, server: &Server{CodexOverloadFailover: config}, account: "codex-account-0", poolModel: "gpt-6-astra"}
	request, _ := http.NewRequest(http.MethodPost, "https://chatgpt.example/responses", bytes.NewReader([]byte(`{}`)))
	started := time.Now()
	response, err := transport.RoundTrip(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	// ~30ms per attempt inside a 100ms budget: about four attempts, well
	// below the count bound the 1ms gaps alone would allow.
	if got := calls.Load(); got < 2 || got > 5 {
		t.Fatalf("upstream attempts = %d, want 2-5 inside a 100ms wall-clock budget", got)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("ladder took %v, want it bounded by the 100ms budget plus one attempt", elapsed)
	}
}

// A client cancel during a same-account gap ends the Codex ladder at once.
func TestCodexCapacityStaySleepHonorsCancel(t *testing.T) {
	var calls atomic.Int32
	base := roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return codexOverloadedResponse(), nil
	})
	config := &CodexOverloadFailoverConfig{sameAccountGap: func() time.Duration { return 5 * time.Second }}
	transport := codexOverloadFailoverTransport{base: base, server: &Server{CodexOverloadFailover: config}, account: "codex-account-0", poolModel: "gpt-6-astra"}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	request, _ := http.NewRequestWithContext(ctx, http.MethodPost, "https://chatgpt.example/responses", bytes.NewReader([]byte(`{}`)))
	time.AfterFunc(50*time.Millisecond, cancel)
	started := time.Now()
	response, _ := transport.RoundTrip(request)
	if response != nil && response.Body != nil {
		response.Body.Close()
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("RoundTrip returned after %v, want it to stop promptly on cancel instead of sleeping the 5s gap", elapsed)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("upstream attempts = %d, want no retry after the cancel", got)
	}
}

// A client cancel during a Claude ladder wait ends the request at once.
func TestClaudeOverloadSleepHonorsCancel(t *testing.T) {
	server, _ := claudeFailoverServer(t)
	stub := &stubRoundTripper{responses: func(*http.Request) *http.Response { return claudeOverloaded529(nil) }}
	var waits []time.Duration
	transport := claudeOverloadTransport(&server, "s", "cooked@example.com", stub, &waits)
	transport.sleep = nil // the real timer: the first wait is 1s
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	request := claudeOverloadRequest("tok-cooked").WithContext(ctx)
	time.AfterFunc(50*time.Millisecond, cancel)
	started := time.Now()
	response, err := transport.RoundTrip(request)
	if response != nil && response.Body != nil {
		response.Body.Close()
	}
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("RoundTrip returned after %v, want it to stop promptly on cancel instead of sleeping the 1s wait", elapsed)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if stub.calls != 1 {
		t.Fatalf("upstream calls = %d, want no retry after the cancel", stub.calls)
	}
}

// The Claude hold is wall clock from the first attempt: slow upstream
// attempts count, and no retry starts whose wait would end past the cap
// (35s here, the short test ladder).
func TestClaudeOverloadHoldIsWallClock(t *testing.T) {
	server, _ := claudeFailoverServer(t)
	clock := time.Unix(1_800_000_000, 0)
	started := clock
	var retryStarts []time.Duration
	var calls int
	stub := &stubRoundTripper{responses: func(*http.Request) *http.Response {
		calls++
		if calls > 1 {
			retryStarts = append(retryStarts, clock.Sub(started))
		}
		clock = clock.Add(5 * time.Second) // each upstream attempt takes 5s
		return claudeOverloaded529(nil)
	}}
	var waits []time.Duration
	transport := claudeOverloadTransport(&server, "s", "cooked@example.com", stub, &waits)
	transport.overloadPolicy = claudeShortLadder
	transport.now = func() time.Time { return clock }
	transport.sleep = func(ctx context.Context, d time.Duration) error {
		waits = append(waits, d)
		clock = clock.Add(d)
		return ctx.Err()
	}
	response, err := transport.RoundTrip(claudeOverloadRequest("tok-cooked"))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != 529 {
		t.Fatalf("status = %d, want 529", response.StatusCode)
	}
	// Attempts end at 5s, 11s, 18s, 27s and 40s; the waits before the
	// retries are 1s, 2s, 4s, 8s (the last ends exactly at 35s). The next
	// 10s wait would end at 50s, past the cap, so the ladder stops two steps
	// early even though its backoff total is only 15s.
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second}
	if len(waits) != len(want) || calls != 1+len(want) {
		t.Fatalf("waits = %v calls = %d, want %v and %d calls", waits, calls, want, 1+len(want))
	}
	for i := range want {
		if waits[i] != want[i] {
			t.Fatalf("waits = %v, want %v", waits, want)
		}
	}
	for _, start := range retryStarts {
		if start > claudeShortLadder.maxWait {
			t.Fatalf("retry started %v after the first attempt (all: %v), past %v", start, retryStarts, claudeShortLadder.maxWait)
		}
	}
}

// Opt-in reroute: an outer replay (after a transport error on the rerouted
// account) continues the request-wide ladder on the session's account and
// cannot reroute a second time.
func TestClaudeOverloadRerouteAtMostOncePerRequest(t *testing.T) {
	server, _ := claudeFailoverServer(t)
	server.ClaudeOverloadReroute = true
	var cookedCalls, freshCalls int
	base := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if strings.Contains(req.Header.Get("Authorization"), "tok-cooked") {
			cookedCalls++
			return claudeOverloaded529(nil), nil
		}
		freshCalls++
		return nil, io.ErrUnexpectedEOF
	})
	var waits []time.Duration
	transport := claudeOverloadTransport(&server, "s", "cooked@example.com", base, &waits)
	transport.overloadPolicy = claudeShortLadder
	// First pass: two same-account retries, the reroute, a transport error.
	if _, err := transport.RoundTrip(claudeOverloadRequest("tok-cooked")); err == nil {
		t.Fatal("first pass succeeded, want the rerouted attempt's transport error")
	}
	if cookedCalls != 1+providerOverloadMaxRetries || freshCalls != 1 {
		t.Fatalf("first pass: cooked=%d fresh=%d, want %d and 1", cookedCalls, freshCalls, 1+providerOverloadMaxRetries)
	}
	// The outer replay reuses the transport, so the request's budget and its
	// overload ladder.
	response, err := transport.RoundTrip(claudeOverloadRequest("tok-cooked"))
	if err != nil {
		t.Fatalf("replay err = %v (fresh calls %d), want the 529 once the ladder is spent", err, freshCalls)
	}
	defer response.Body.Close()
	if response.StatusCode != 529 {
		t.Fatalf("replay status = %d, want the 529 once the ladder is spent", response.StatusCode)
	}
	if freshCalls != 1 {
		t.Fatalf("fresh calls = %d, want the reroute to run once per request", freshCalls)
	}
	// The replay's first attempt plus the rest of the ladder.
	if want := 1 + providerOverloadMaxRetries + 1 + (claudeShortLadderRetries - providerOverloadMaxRetries); cookedCalls != want {
		t.Fatalf("cooked calls = %d, want %d", cookedCalls, want)
	}
}
