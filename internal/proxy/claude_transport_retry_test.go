package proxy

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
)

func TestClaudeConnectionResetsUseLongRequestWideHold(t *testing.T) {
	t.Parallel()
	clock := newFakeOverloadClock()
	registry := newRetryStatusRegistry()
	server := &Server{overloadHeld: newOverloadHeldGauge(), retryStatuses: registry}
	budget := newAttemptBudget(1)
	calls := 0
	base := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		calls++
		if calls <= 3 {
			return nil, errors.New("read tcp: connection reset by peer")
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: http.NoBody, Request: request}, nil
	})
	var waits []time.Duration
	transport := replayablePostRetryTransport{
		base: base, server: server, provider: accounts.ProviderClaude, model: "claude-opus-4-8",
		agent: "claude", session: "session", account: "account",
		method: http.MethodPost, path: "/v1/messages", maxAttempts: 2, budget: budget,
		claudeTransientRetry: true,
		now:                  clock.Now,
		sleep: func(ctx context.Context, wait time.Duration) error {
			waits = append(waits, wait)
			status := registry.forSession("claude", "session")
			wantAttempt := len(waits) + 1
			if status == nil || status.Provider != accounts.ProviderClaude || status.Model != "claude-opus-4-8" ||
				status.AccountID != "account" || status.Attempt != wantAttempt || status.Reason != "transport_connection_reset" ||
				!status.NextRetryAt.Equal(clock.Now().Add(wait)) {
				t.Fatalf("retry status during wait %d = %+v", len(waits), status)
			}
			if got := server.overloadHeld.claude.Load(); got != 1 {
				t.Fatalf("held gauge during retry = %d, want 1", got)
			}
			clock.now = clock.now.Add(wait)
			return ctx.Err()
		},
	}

	response, err := transport.RoundTrip(claudeReplayRequest(t, context.Background()))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK || calls != 4 {
		t.Fatalf("status=%d calls=%d, want success after four attempts", response.StatusCode, calls)
	}
	wantWaits := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second}
	if len(waits) != len(wantWaits) {
		t.Fatalf("waits=%v, want %v", waits, wantWaits)
	}
	for i := range wantWaits {
		if waits[i] != wantWaits[i] {
			t.Fatalf("waits=%v, want %v", waits, wantWaits)
		}
	}
	if remaining := budget.remaining.Load(); remaining != 1 {
		t.Fatalf("shared failover budget remaining=%d, want 1; transient retries use the long hold", remaining)
	}
	if got := server.overloadHeld.claude.Load(); got != 0 {
		t.Fatalf("held gauge after success = %d, want 0", got)
	}
	if status := registry.forSession("claude", "session"); status != nil {
		t.Fatalf("retry status remained after success: %+v", status)
	}
}

func TestClaudeTransportFailureUnboundedUntilClientCancels(t *testing.T) {
	t.Parallel()
	clock := newFakeOverloadClock()
	server := &Server{overloadHeld: newOverloadHeldGauge()}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	calls := 0
	transport := replayablePostRetryTransport{
		base: roundTripFunc(func(*http.Request) (*http.Response, error) {
			calls++
			return nil, errors.New("read tcp: connection reset by peer")
		}),
		server: server, agent: "claude", session: "session", account: "account",
		method: http.MethodPost, path: "/v1/messages", maxAttempts: 1,
		budget: newAttemptBudget(0), overloadPolicy: overloadRetryPolicy{unbounded: true},
		claudeTransientRetry: true, now: clock.Now,
		sleep: func(ctx context.Context, wait time.Duration) error {
			clock.now = clock.now.Add(wait)
			if calls == 100 {
				cancel()
			}
			return ctx.Err()
		},
	}

	response, err := transport.RoundTrip(claudeReplayRequest(t, ctx))
	if response != nil {
		response.Body.Close()
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v, want context.Canceled", err)
	}
	if calls != 100 || clock.now.Sub(time.Unix(1_800_000_000, 0)) < 20*time.Minute {
		t.Fatalf("calls=%d elapsed=%v, want retries well beyond the quick replay budget", calls, clock.now.Sub(time.Unix(1_800_000_000, 0)))
	}
	if got := server.overloadHeld.claude.Load(); got != 0 {
		t.Fatalf("held gauge after cancel = %d, want 0", got)
	}
}

func TestClaudeTransportRetryLogIncludesAttemptAndNextWait(t *testing.T) {
	t.Parallel()
	var log bytes.Buffer
	calls := 0
	transport := replayablePostRetryTransport{
		base: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			calls++
			if calls == 1 {
				return nil, errors.New("read tcp: connection reset by peer")
			}
			return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: http.NoBody, Request: request}, nil
		}),
		logger: slog.New(slog.NewJSONHandler(&log, nil)),
		agent:  "claude", session: "session", account: "account",
		method: http.MethodPost, path: "/v1/messages", maxAttempts: 1,
		budget: newAttemptBudget(0), claudeTransientRetry: true,
		sleep: func(context.Context, time.Duration) error { return nil },
	}

	response, err := transport.RoundTrip(claudeReplayRequest(t, context.Background()))
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	got := log.String()
	if !strings.Contains(got, `"attempt":2`) || !strings.Contains(got, `"next_in":"1s"`) {
		t.Fatalf("retry log = %s, want attempt 2 and next_in 1s", got)
	}
}

func TestClaudeConnectionResetAfterFailoverStaysOnRoutedAccount(t *testing.T) {
	server, store := claudeFailoverServer(t)
	registry := newRetryStatusRegistry()
	server.retryStatuses = registry
	const (
		initialAccount = "cooked@example.com"
		routedAccount  = "fresh@example.com"
	)
	if _, err := store.Put("claude", "session-reset", initialAccount, ""); err != nil {
		t.Fatal(err)
	}

	var accountsSeen []string
	routedCalls := 0
	base := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		account := initialAccount
		if strings.Contains(request.Header.Get("Authorization"), "tok-fresh") {
			account = routedAccount
		}
		accountsSeen = append(accountsSeen, account)
		if account == initialAccount {
			header := http.Header{}
			header.Set("Anthropic-Ratelimit-Unified-Status", "rejected")
			return &http.Response{
				StatusCode: http.StatusTooManyRequests,
				Header:     header,
				Body:       io.NopCloser(strings.NewReader(`{"type":"error","error":{"type":"rate_limit_error"}}`)),
				Request:    request,
			}, nil
		}
		routedCalls++
		if routedCalls <= 2 {
			return nil, errors.New("read tcp: connection reset by peer")
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: http.NoBody, Request: request}, nil
	})

	clock := newFakeOverloadClock()
	budget := newAttemptBudget(5)
	var waits []time.Duration
	inner := usageLimitRetryTransport{
		base: base, server: &server, provider: accounts.ProviderClaude, agent: "claude", session: "session-reset",
		account: initialAccount, method: http.MethodPost, path: "/v1/messages", maxAttempts: 3, poolModel: "claude-opus-4-8",
		budget: budget, overloadPolicy: overloadRetryPolicy{unbounded: true}, now: clock.Now,
		sleep: func(ctx context.Context, wait time.Duration) error {
			waits = append(waits, wait)
			status := registry.forSession("claude", "session-reset")
			wantAttempt := len(waits) + 1
			if status == nil || status.Provider != accounts.ProviderClaude || status.Model != "claude-opus-4-8" ||
				status.AccountID != routedAccount || status.Attempt != wantAttempt || status.Reason != "transport_connection_reset" ||
				!status.NextRetryAt.Equal(clock.Now().Add(wait)) {
				t.Fatalf("routed retry status during wait %d = %+v", len(waits), status)
			}
			clock.now = clock.now.Add(wait)
			return ctx.Err()
		},
	}
	outerSleeps := 0
	transport := replayablePostRetryTransport{
		base: inner, server: &server, agent: "claude", session: "session-reset", account: initialAccount,
		method: http.MethodPost, path: "/v1/messages", maxAttempts: 6, budget: budget,
		claudeTransientRetry: true, overloadPolicy: overloadRetryPolicy{unbounded: true}, now: clock.Now,
		sleep: func(context.Context, time.Duration) error {
			outerSleeps++
			return nil
		},
	}
	request := claudeReplayRequest(t, context.Background())
	request.Header.Set("Authorization", "Bearer tok-cooked")

	response, err := transport.RoundTrip(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	wantAccounts := []string{initialAccount, routedAccount, routedAccount, routedAccount}
	if strings.Join(accountsSeen, ",") != strings.Join(wantAccounts, ",") {
		t.Fatalf("accounts=%v, want %v", accountsSeen, wantAccounts)
	}
	if outerSleeps != 0 {
		t.Fatalf("outer retry sleeps=%d, want 0; inner routed request should absorb resets", outerSleeps)
	}
	wantWaits := []time.Duration{time.Second, 2 * time.Second}
	if len(waits) != len(wantWaits) || waits[0] != wantWaits[0] || waits[1] != wantWaits[1] {
		t.Fatalf("waits=%v, want %v", waits, wantWaits)
	}
	routed, ok := routedResponseAccount(response)
	if !ok || routed.ID != routedAccount {
		t.Fatalf("final response account=%+v, %t; want %s", routed, ok, routedAccount)
	}
	if status := registry.forSession("claude", "session-reset"); status != nil {
		t.Fatalf("routed retry status remained after success: %+v", status)
	}
}

func TestClaudePreHeaderResetRetryStatusClearsOnCancel(t *testing.T) {
	t.Parallel()
	registry := newRetryStatusRegistry()
	server := &Server{overloadHeld: newOverloadHeldGauge(), retryStatuses: registry}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	transport := usageLimitRetryTransport{
		base: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return nil, errors.New("read tcp: connection reset by peer")
		}),
		server: server, provider: accounts.ProviderClaude, agent: "claude", session: "cancel-reset",
		account: "same@example.com", method: http.MethodPost, path: "/v1/messages", maxAttempts: 1,
		poolModel: "claude-opus-4-8", budget: newAttemptBudget(0), overloadPolicy: overloadRetryPolicy{unbounded: true},
		sleep: func(waitCtx context.Context, _ time.Duration) error {
			status := registry.forSession("claude", "cancel-reset")
			if status == nil || status.AccountID != "same@example.com" || status.Attempt != 2 ||
				status.Reason != "transport_connection_reset" {
				t.Fatalf("retry status during canceled wait = %+v", status)
			}
			cancel()
			return waitCtx.Err()
		},
	}

	response, err := transport.RoundTrip(claudeReplayRequest(t, ctx))
	if response != nil {
		response.Body.Close()
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v, want context.Canceled", err)
	}
	if status := registry.forSession("claude", "cancel-reset"); status != nil {
		t.Fatalf("retry status remained after cancel: %+v", status)
	}
}

func TestClaudeMidStreamResetIsNeverReplayed(t *testing.T) {
	t.Parallel()
	calls := 0
	transport := replayablePostRetryTransport{
		base: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			calls++
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       &partialResetBody{prefix: []byte("data: partial\n\n")},
				Request:    request,
			}, nil
		}),
		claudeTransientRetry: true,
		maxAttempts:          6,
		budget:               newAttemptBudget(5),
	}

	response, err := transport.RoundTrip(claudeReplayRequest(t, context.Background()))
	if err != nil {
		t.Fatal(err)
	}
	body, readErr := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if string(body) != "data: partial\n\n" || readErr == nil || !strings.Contains(readErr.Error(), "connection reset") {
		t.Fatalf("body=%q err=%v, want the one partial stream and its reset", body, readErr)
	}
	if calls != 1 {
		t.Fatalf("upstream calls=%d, want 1; replaying after partial output would duplicate it", calls)
	}
}

func claudeReplayRequest(t *testing.T, ctx context.Context) *http.Request {
	t.Helper()
	body := []byte(`{"model":"claude-opus-4-8","messages":[]}`)
	request := httptest.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages", bytes.NewReader(body)).WithContext(ctx)
	request.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(body)), nil
	}
	request.ContentLength = int64(len(body))
	return request
}

type partialResetBody struct {
	prefix []byte
	read   bool
}

func (b *partialResetBody) Read(p []byte) (int, error) {
	if !b.read {
		b.read = true
		return copy(p, b.prefix), nil
	}
	return 0, errors.New("read tcp: connection reset by peer")
}

func (*partialResetBody) Close() error { return nil }
