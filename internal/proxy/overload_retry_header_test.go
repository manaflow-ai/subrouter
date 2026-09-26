package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/session"
)

// X-Subrouter-Retry lets a client shape its own same-account overload wait,
// only where the operator allows it, within clamps, and never upstream.

func TestParseOverloadRetryHeader(t *testing.T) {
	cases := []struct {
		raw      string
		want     overloadRetryOverride
		problems int
	}{
		{"interval=2s,max-wait=20m", overloadRetryOverride{interval: 2 * time.Second, maxWait: 20 * time.Minute, maxWaitSet: true}, 0},
		{"interval=2s", overloadRetryOverride{interval: 2 * time.Second}, 0},
		{" max-wait = 90s ", overloadRetryOverride{maxWait: 90 * time.Second, maxWaitSet: true}, 0},
		// A client cannot ask for an unbounded wait: max-wait=0 is the 60m cap.
		{"max-wait=0", overloadRetryOverride{maxWait: OverloadRetryMaxWaitCap, maxWaitSet: true}, 0},
		{"max-wait=0s", overloadRetryOverride{maxWait: OverloadRetryMaxWaitCap, maxWaitSet: true}, 0},
		// Clamps: interval to [500ms, 60m], max-wait down to 60m.
		{"interval=100ms,max-wait=3h", overloadRetryOverride{interval: OverloadRetryMinInterval, maxWait: OverloadRetryMaxWaitCap, maxWaitSet: true}, 0},
		{"interval=2h", overloadRetryOverride{interval: OverloadRetryMaxWaitCap}, 0},
		// Malformed entries are ignored, the rest still applies.
		{"interval=soon,max-wait=5m", overloadRetryOverride{maxWait: 5 * time.Minute, maxWaitSet: true}, 1},
		{"interval=0,max-wait=-1m,color=blue,junk", overloadRetryOverride{}, 4},
	}
	for _, test := range cases {
		got, problems := parseOverloadRetryHeader(test.raw)
		if got != test.want || len(problems) != test.problems {
			t.Fatalf("parse(%q) = %+v problems %v, want %+v with %d problems", test.raw, got, problems, test.want, test.problems)
		}
	}
	if got := FormatOverloadRetryHeader(2*time.Second, 20*time.Minute); got != "interval=2s,max-wait=20m0s" {
		t.Fatalf("format = %q", got)
	}
	if got := FormatOverloadRetryHeader(0, 0); got != "" {
		t.Fatalf("format of nothing = %q, want empty", got)
	}
	if got, _ := parseOverloadRetryHeader(FormatOverloadRetryHeader(2*time.Second, 20*time.Minute)); got.interval != 2*time.Second || got.maxWait != 20*time.Minute {
		t.Fatalf("round trip = %+v", got)
	}
}

func overloadRetryRequest(value string) *http.Request {
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	request.Header.Set(OverloadRetryHeader, value)
	return request
}

// Claude: the header is ignored unless SUBROUTER_CLAUDE_OVERLOAD_RETRY_HEADER=1.
func TestClaudeOverloadRetryHeaderNeedsOptIn(t *testing.T) {
	request := overloadRetryRequest("interval=2s,max-wait=20m")
	operator := &ClaudeOverloadRetryConfig{Interval: 5 * time.Second, MaxWait: time.Minute}
	for _, config := range []*ClaudeOverloadRetryConfig{nil, operator} {
		if got := config.policyFor(request, nil); got != config.policy() {
			t.Fatalf("policy without opt-in = %+v, want the operator's %+v", got, config.policy())
		}
	}
	allowed := &ClaudeOverloadRetryConfig{Interval: 5 * time.Second, MaxWait: time.Minute, AllowHeader: true}
	if got := allowed.policyFor(request, nil); got.interval != 2*time.Second || got.maxWait != 20*time.Minute || got.unbounded {
		t.Fatalf("policy with opt-in = %+v, want 2s / 20m", got)
	}
	// Either key alone overrides only itself.
	// max-wait=0 is the 60m hard cap, never unbounded.
	if got := allowed.policyFor(overloadRetryRequest("max-wait=0"), nil); got.interval != 5*time.Second || got.unbounded || got.maxWait != OverloadRetryMaxWaitCap {
		t.Fatalf("max-wait=0 policy = %+v, want the operator interval and the 60m cap", got)
	}
	// Only the operator can make the wait unbounded; a requested max-wait
	// replaces it with a finite cap, an interval alone keeps it.
	unbounded := &ClaudeOverloadRetryConfig{Unbounded: true, AllowHeader: true}
	if got := unbounded.policyFor(overloadRetryRequest("interval=2s"), nil); !got.unbounded {
		t.Fatalf("interval-only policy = %+v, want the operator's unbounded wait kept", got)
	}
	if got := unbounded.policyFor(overloadRetryRequest("max-wait=20m"), nil); got.unbounded || got.maxWait != 20*time.Minute {
		t.Fatalf("max-wait policy over unbounded = %+v, want 20m", got)
	}
}

// Codex: honored with SUBROUTER_CODEX_CAPACITY_RETRY_HEADER=1 or the
// failover, ignored otherwise; a requested max-wait is explicit, so a
// configured fallback does not shorten it.
func TestCodexOverloadRetryHeaderNeedsOptIn(t *testing.T) {
	request := overloadRetryRequest("interval=2s,max-wait=20m")
	for _, config := range []*CodexOverloadFailoverConfig{nil, {}, {StayInterval: 5 * time.Second}} {
		stay, explicit := config.stayPolicy(config.codexCapacityRetryPolicyFor(request, nil))
		if stay.interval == 2*time.Second || stay.maxWait == 20*time.Minute || explicit {
			t.Fatalf("config %+v: stay = %+v explicit=%t, want the header ignored", config, stay, explicit)
		}
	}
	for _, config := range []*CodexOverloadFailoverConfig{{CapacityRetryHeader: true}, {Enabled: true}} {
		stay, explicit := config.stayPolicy(config.codexCapacityRetryPolicyFor(request, nil))
		if stay.interval != 2*time.Second || stay.maxWait != 20*time.Minute || !explicit {
			t.Fatalf("config %+v: stay = %+v explicit=%t, want 2s / 20m, explicit", config, stay, explicit)
		}
	}
	server := &Server{
		CodexOverloadFailover: &CodexOverloadFailoverConfig{CapacityRetryHeader: true},
		CodexEgress:           &CodexEgressConfig{Proxies: []*url.URL{mustParseURL(t, "http://egress.example:3128")}},
	}
	if budget, unbounded := server.codexStayBudget(overloadRetryPolicy{maxWait: 20 * time.Minute}, true); budget != 20*time.Minute || unbounded {
		t.Fatalf("explicit budget with a fallback = %v, want the requested 20m", budget)
	}
	if budget, _ := server.codexStayBudget(overloadRetryPolicy{interval: 2 * time.Second}, false); budget != codexCapacityFallbackStayRetryBudget {
		t.Fatalf("interval-only budget with a fallback = %v, want the 10s fallback cap", budget)
	}
}

// A configured or requested interval shapes both ladders: the ramp is capped
// at it, then it holds.
func TestOverloadRetryIntervalShapesLadders(t *testing.T) {
	for retry, want := range []time.Duration{time.Second, 2 * time.Second, 2 * time.Second, 2 * time.Second, 2 * time.Second} {
		if got := claudeOverloadGap(retry, 2*time.Second); got != want {
			t.Fatalf("claude gap %d at 2s = %v, want %v", retry, got, want)
		}
	}
	if got := claudeOverloadGap(0, 500*time.Millisecond); got != 500*time.Millisecond {
		t.Fatalf("claude first gap at 500ms = %v, want the ramp capped at 500ms", got)
	}

	server, _ := claudeFailoverServer(t)
	stub := &stubRoundTripper{responses: func(*http.Request) *http.Response { return claudeOverloaded529(nil) }}
	var waits []time.Duration
	transport := claudeOverloadTransport(&server, "s", "cooked@example.com", stub, &waits)
	transport.overloadPolicy = (&ClaudeOverloadRetryConfig{Interval: 2 * time.Second, MaxWait: 11 * time.Second}).policy()
	response, err := transport.RoundTrip(claudeOverloadRequest("tok-cooked"))
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	want := []time.Duration{time.Second, 2 * time.Second, 2 * time.Second, 2 * time.Second, 2 * time.Second, 2 * time.Second}
	if len(waits) != len(want) {
		t.Fatalf("waits = %v, want %v", waits, want)
	}
	for i := range want {
		if waits[i] != want[i] {
			t.Fatalf("waits = %v, want %v", waits, want)
		}
	}

	clock := newFakeOverloadClock()
	var gaps []time.Duration
	codex, _, _ := codexStayTransport(&CodexOverloadFailoverConfig{StayInterval: 2 * time.Second, StayMaxWait: 30 * time.Second}, clock, func(ctx context.Context, d time.Duration) bool {
		gaps = append(gaps, d)
		return true
	})
	response, err = codex.RoundTrip(codexStayRequest(context.Background()))
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if len(gaps) < 10 {
		t.Fatalf("codex gaps = %v, want many 2s gaps inside 30s", gaps)
	}
	for i, gap := range gaps[3:] {
		if gap < 1600*time.Millisecond || gap > 2400*time.Millisecond {
			t.Fatalf("codex gap %d = %v (all %v), want ~2s (with jitter) once the ramp reaches it", i+3, gap, gaps)
		}
	}
}

// The header never reaches the upstream, allowed or not.
func TestOverloadRetryHeaderStrippedUpstream(t *testing.T) {
	headers := http.Header{}
	headers.Set(OverloadRetryHeader, "interval=2s")
	session.StripSubrouterHeaders(headers)
	if headers.Get(OverloadRetryHeader) != "" {
		t.Fatal("StripSubrouterHeaders left X-Subrouter-Retry")
	}

	server, seen := codexStayServer(t, func(_ string, index int) bool { return index < 2 }, 1)
	server.CodexOverloadFailover.CapacityRetryHeader = true
	proxy := httptest.NewServer(server.Handler())
	defer proxy.Close()
	status, body, err := codexCapacityPost(context.Background(), t, proxy.URL, "session-strip", "a", map[string]string{
		OverloadRetryHeader: "interval=2s,max-wait=1m",
	})
	if err != nil || status != http.StatusOK || !strings.Contains(body, "served-from-oauth-token-0") {
		t.Fatalf("status=%d body=%s err=%v", status, body, err)
	}
	got := seen()
	if len(got) != 3 {
		t.Fatalf("pool saw %d attempts, want 3", len(got))
	}
	for i, attempt := range got {
		if value := attempt.header.Get(OverloadRetryHeader); value != "" {
			t.Fatalf("attempt %d carried %s: %q upstream", i, OverloadRetryHeader, value)
		}
	}
}
