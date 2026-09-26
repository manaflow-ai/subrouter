package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
)

// The same-account overload wait is long by default (Claude 8m, Codex 4m),
// capped in wall-clock time, unbounded with max-wait 0, and visible: a gauge
// of held requests per provider. Clocks and sleeps are injected; nothing
// here really waits.

// fakeOverloadClock is an injected clock that the fake sleep advances.
type fakeOverloadClock struct {
	now time.Time
}

func newFakeOverloadClock() *fakeOverloadClock {
	return &fakeOverloadClock{now: time.Unix(1_800_000_000, 0)}
}

func (c *fakeOverloadClock) Now() time.Time { return c.now }

// A configured MaxWait caps the Claude ladder in wall-clock time.
func TestClaudeOverloadMaxWaitCapsTheLadder(t *testing.T) {
	server, _ := claudeFailoverServer(t)
	stub := &stubRoundTripper{responses: func(*http.Request) *http.Response { return claudeOverloaded529(nil) }}
	var waits []time.Duration
	transport := claudeOverloadTransport(&server, "s", "cooked@example.com", stub, &waits)
	transport.overloadPolicy = (&ClaudeOverloadRetryConfig{MaxWait: time.Minute}).policy()
	response, err := transport.RoundTrip(claudeOverloadRequest("tok-cooked"))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	// 1+2+4+8 = 15s, then 15s steps: 30s, 45s, 60s; the next would end at 75s.
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 15 * time.Second, 15 * time.Second, 15 * time.Second}
	if len(waits) != len(want) {
		t.Fatalf("waits = %v, want %v", waits, want)
	}
	for i := range want {
		if waits[i] != want[i] {
			t.Fatalf("waits = %v, want %v", waits, want)
		}
	}
	if response.StatusCode != 529 || stub.calls != 1+len(want) {
		t.Fatalf("status=%d calls=%d, want the 529 after %d attempts", response.StatusCode, stub.calls, 1+len(want))
	}
}

// MaxWait 0 (unbounded): the Claude ladder keeps going far past any cap
// until the client cancels, then returns promptly and releases the gauge.
func TestClaudeOverloadUnboundedRunsUntilCancel(t *testing.T) {
	server, _ := claudeFailoverServer(t)
	server.overloadHeld = newOverloadHeldGauge()
	clock := newFakeOverloadClock()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stub := &stubRoundTripper{responses: func(*http.Request) *http.Response { return claudeOverloaded529(nil) }}
	var waits []time.Duration
	transport := claudeOverloadTransport(&server, "s", "cooked@example.com", stub, &waits)
	transport.overloadPolicy = (&ClaudeOverloadRetryConfig{Unbounded: true}).policy()
	transport.now = clock.Now
	heldDuringWait := int64(-1)
	transport.sleep = func(ctx context.Context, d time.Duration) error {
		waits = append(waits, d)
		clock.now = clock.now.Add(d)
		heldDuringWait = server.overloadHeld.claude.Load()
		if len(waits) == 200 { // 200 retries: ~50 minutes of simulated waiting
			cancel()
		}
		return ctx.Err()
	}
	response, err := transport.RoundTrip(claudeOverloadRequest("tok-cooked").WithContext(ctx))
	if response != nil && response.Body != nil {
		response.Body.Close()
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if len(waits) != 200 || sumDurations(waits) < 45*time.Minute {
		t.Fatalf("waits = %d (%v total), want 200 retries well past the default 8m cap", len(waits), sumDurations(waits))
	}
	if heldDuringWait != 1 {
		t.Fatalf("held gauge during the wait = %d, want 1", heldDuringWait)
	}
	if got := server.overloadHeld.claude.Load(); got != 0 {
		t.Fatalf("held gauge after cancel = %d, want 0", got)
	}
}

// The Claude gauge counts a request while it waits and releases it when the
// retry succeeds.
func TestClaudeOverloadHeldGaugeReleasesOnSuccess(t *testing.T) {
	server, _ := claudeFailoverServer(t)
	server.overloadHeld = newOverloadHeldGauge()
	calls := 0
	stub := &stubRoundTripper{responses: func(*http.Request) *http.Response {
		calls++
		if calls < 3 {
			return claudeOverloaded529(nil)
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: http.NoBody}
	}}
	var held []int64
	transport := claudeOverloadTransport(&server, "s", "cooked@example.com", stub, nil)
	transport.sleep = func(ctx context.Context, _ time.Duration) error {
		held = append(held, server.overloadHeld.claude.Load())
		return ctx.Err()
	}
	response, err := transport.RoundTrip(claudeOverloadRequest("tok-cooked"))
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if len(held) != 2 || held[0] != 1 || held[1] != 1 {
		t.Fatalf("gauge during waits = %v, want [1 1]", held)
	}
	if got := server.overloadHeld.claude.Load(); got != 0 {
		t.Fatalf("gauge after success = %d, want 0", got)
	}
}

// codexStayTransport is the capacity layer with the failover off, an
// injected clock and a sleep that advances it.
func codexStayTransport(config *CodexOverloadFailoverConfig, clock *fakeOverloadClock, onSleep func(context.Context, time.Duration) bool) (codexOverloadFailoverTransport, *Server, *int) {
	calls := 0
	base := roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls++
		return codexOverloadedResponse(), nil
	})
	server := &Server{CodexOverloadFailover: config, overloadHeld: newOverloadHeldGauge()}
	transport := codexOverloadFailoverTransport{
		base: base, server: server, account: "codex-account-0", poolModel: "gpt-6-astra",
		policy: config.codexCapacityRetryPolicyFor(nil, nil),
		now:    clock.Now,
		sleep: func(ctx context.Context, d time.Duration) bool {
			clock.now = clock.now.Add(d)
			if onSleep != nil {
				return onSleep(ctx, d)
			}
			return ctx.Err() == nil
		},
	}
	return transport, server, &calls
}

func codexStayRequest(ctx context.Context) *http.Request {
	request, _ := http.NewRequestWithContext(ctx, http.MethodPost, "https://chatgpt.example/responses", bytes.NewReader([]byte(`{}`)))
	return request
}

// Default Codex ladder: 0.5s doubling to 8s, then a steady 8-10s, until the
// next wait would end past 4m of wall-clock time. No count bound.
func TestCodexCapacityDefaultStayLadderRunsFourMinutes(t *testing.T) {
	clock := newFakeOverloadClock()
	started := clock.now
	var gaps []time.Duration
	var heldDuring []int64
	var server *Server
	transport, server, calls := codexStayTransport(&CodexOverloadFailoverConfig{}, clock, func(ctx context.Context, d time.Duration) bool {
		gaps = append(gaps, d)
		heldDuring = append(heldDuring, server.overloadHeld.codex.Load())
		return true
	})
	response, err := transport.RoundTrip(codexStayRequest(context.Background()))
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	elapsed := clock.now.Sub(started)
	if elapsed > codexCapacityDefaultStayMaxWait || elapsed < codexCapacityDefaultStayMaxWait-10*time.Second {
		t.Fatalf("ladder waited %v, want just inside the 4m cap", elapsed)
	}
	if len(gaps) < 25 || *calls != 1+len(gaps) {
		t.Fatalf("gaps = %d, calls = %d, want about 27 retries with no count bound", len(gaps), *calls)
	}
	for i, gap := range gaps {
		if i >= 5 && (gap < 8*time.Second || gap > 10*time.Second) {
			t.Fatalf("gap %d = %v (all %v), want a steady 8-10s after the ramp", i, gap, gaps)
		}
		if heldDuring[i] != 1 {
			t.Fatalf("gauge during gap %d = %d, want 1", i, heldDuring[i])
		}
	}
	if got := server.overloadHeld.codex.Load(); got != 0 {
		t.Fatalf("gauge after the ladder = %d, want 0", got)
	}
}

// StayMaxWait sets the Codex cap; StayUnbounded runs until the client
// cancels, and the cancel returns promptly and releases the gauge.
func TestCodexCapacityStayMaxWaitAndUnbounded(t *testing.T) {
	clock := newFakeOverloadClock()
	started := clock.now
	transport, _, _ := codexStayTransport(&CodexOverloadFailoverConfig{StayMaxWait: 30 * time.Second}, clock, nil)
	response, err := transport.RoundTrip(codexStayRequest(context.Background()))
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if elapsed := clock.now.Sub(started); elapsed > 30*time.Second || elapsed < 15*time.Second {
		t.Fatalf("ladder waited %v, want it capped at 30s", elapsed)
	}

	clock = newFakeOverloadClock()
	started = clock.now
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	retries := 0
	var server *Server
	transport, server, _ = codexStayTransport(&CodexOverloadFailoverConfig{StayUnbounded: true}, clock, func(ctx context.Context, _ time.Duration) bool {
		retries++
		if retries == 500 { // well over an hour of simulated waiting
			cancel()
		}
		return ctx.Err() == nil
	})
	response, _ = transport.RoundTrip(codexStayRequest(ctx))
	if response != nil && response.Body != nil {
		response.Body.Close()
	}
	if retries != 500 || clock.now.Sub(started) < time.Hour {
		t.Fatalf("retries = %d over %v, want 500 retries past an hour until the cancel", retries, clock.now.Sub(started))
	}
	if got := server.overloadHeld.codex.Load(); got != 0 {
		t.Fatalf("gauge after cancel = %d, want 0", got)
	}
}

// /_subrouter/health reports the held-in-overload count per provider.
func TestOverloadHeldGaugeInHealth(t *testing.T) {
	server, _ := claudeFailoverServer(t)
	handler := server.Handler()
	// Handler() installs the gauge; with nothing held both counts are 0.
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/_subrouter/health", nil))
	var health struct {
		Held map[string]int64 `json:"overload_retry_held"`
	}
	if err := json.NewDecoder(recorder.Body).Decode(&health); err != nil {
		t.Fatal(err)
	}
	claude, okClaude := health.Held[string(accounts.ProviderClaude)]
	codex, okCodex := health.Held[string(accounts.ProviderCodex)]
	if !okClaude || !okCodex || claude != 0 || codex != 0 {
		t.Fatalf("health overload_retry_held = %v, want claude and codex at 0", health.Held)
	}

	gauge := newOverloadHeldGauge()
	releaseA := gauge.enter(accounts.ProviderClaude)
	releaseB := gauge.enter(accounts.ProviderClaude)
	releaseC := gauge.enter(accounts.ProviderCodex)
	if got := gauge.snapshot(); got["claude"] != 2 || got["codex"] != 1 {
		t.Fatalf("snapshot = %v, want claude 2 codex 1", got)
	}
	releaseA()
	releaseA() // idempotent
	releaseB()
	releaseC()
	if got := gauge.snapshot(); got["claude"] != 0 || got["codex"] != 0 {
		t.Fatalf("snapshot after release = %v, want zeros", got)
	}
}
