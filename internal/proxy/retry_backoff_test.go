package proxy

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
)

func TestRetryBackoffGrowsAndIsCapped(t *testing.T) {
	first := retryBackoff(1)
	if first != retryBaseBackoff {
		t.Fatalf("retryBackoff(1) = %s, want %s", first, retryBaseBackoff)
	}
	if second := retryBackoff(2); second <= first {
		t.Fatalf("retryBackoff(2) = %s, want more than attempt 1's %s; retries must space out", second, first)
	}
	// Every attempt must be waited on, and the total must stay bounded.
	var total time.Duration
	for attempt := 1; attempt <= replayablePostMaxAttempts; attempt++ {
		wait := retryBackoff(attempt)
		if wait <= 0 {
			t.Fatalf("retryBackoff(%d) = %s, want a positive wait; back-to-back retries burn the budget instantly", attempt, wait)
		}
		if wait > retryMaxBackoff {
			t.Fatalf("retryBackoff(%d) = %s, exceeds the cap %s", attempt, wait, retryMaxBackoff)
		}
		total += wait
	}
	if total > 5*time.Second {
		t.Fatalf("total backoff across %d attempts = %s, too slow to sit in a request path", replayablePostMaxAttempts, total)
	}
}

func TestSleepForRetryWaitsThenReportsTrue(t *testing.T) {
	start := time.Now()
	if !sleepForRetry(context.Background(), 40*time.Millisecond) {
		t.Fatal("sleepForRetry reported the caller gave up, want true")
	}
	if elapsed := time.Since(start); elapsed < 30*time.Millisecond {
		t.Fatalf("returned after %s, want it to actually wait ~40ms", elapsed)
	}
}

// A client that has already hung up must not be kept waiting through the
// remaining backoff.
func TestSleepForRetryAbortsWhenClientGivesUp(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	start := time.Now()
	if sleepForRetry(ctx, 5*time.Second) {
		t.Fatal("sleepForRetry waited out a canceled context, want an immediate abort")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("took %s to notice cancellation, want near-immediate", elapsed)
	}
}

func TestSleepForRetryZeroIsImmediate(t *testing.T) {
	if !sleepForRetry(context.Background(), 0) {
		t.Fatal("zero backoff should return true immediately")
	}
}

type alwaysResetTransport struct{ attempts int }

func (t *alwaysResetTransport) RoundTrip(_ *http.Request) (*http.Response, error) {
	t.attempts++
	return nil, errors.New("write tcp4 10.0.0.1:1->10.0.0.2:443: use of closed network connection")
}

// The whole point of the retry budget is to span a brief upstream refusal.
// Firing every attempt in the same microsecond spends it for nothing, which is
// how six retries still produced a 502.
func TestReplayablePostRetrySpacesOutAttempts(t *testing.T) {
	t.Parallel()
	base := &alwaysResetTransport{}
	transport := replayablePostRetryTransport{
		base:        base,
		maxAttempts: 4,
		method:      http.MethodPost,
		path:        "/responses",
	}

	request, err := http.NewRequest(http.MethodPost, "https://example.invalid/responses", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	request.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader("{}")), nil
	}

	start := time.Now()
	if _, err := transport.RoundTrip(request); err == nil {
		t.Fatal("expected the retryable transport error to surface after the budget is spent")
	}
	elapsed := time.Since(start)

	if base.attempts != 4 {
		t.Fatalf("made %d attempts, want 4", base.attempts)
	}
	// Three gaps between four attempts.
	var want time.Duration
	for attempt := 1; attempt <= 3; attempt++ {
		want += retryBackoff(attempt)
	}
	if elapsed < want {
		t.Fatalf("four attempts took %s, want at least %s of backoff between them; retries are firing back to back", elapsed, want)
	}
}

func TestReplayablePostRetryStatusLifecycle(t *testing.T) {
	fixedNow := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name             string
		firstResponse    *http.Response
		firstError       error
		reason           string
		cancelDuringWait bool
		terminalError    error
	}{
		{
			name:       "connection reset clears after success",
			firstError: errors.New("read tcp: connection reset by peer"),
			reason:     "transport_connection_reset",
		},
		{
			name:          "408 clears after terminal error",
			firstResponse: &http.Response{StatusCode: http.StatusRequestTimeout, Header: make(http.Header), Body: http.NoBody},
			reason:        "http_408",
			terminalError: errors.New("permanent upstream failure"),
		},
		{
			name:             "408 clears after cancellation",
			firstResponse:    &http.Response{StatusCode: http.StatusRequestTimeout, Header: make(http.Header), Body: http.NoBody},
			reason:           "http_408",
			cancelDuringWait: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			registry := newRetryStatusRegistry()
			server := &Server{retryStatuses: registry}
			calls := 0
			base := roundTripFunc(func(request *http.Request) (*http.Response, error) {
				calls++
				if calls == 1 {
					if tt.firstResponse != nil {
						response := *tt.firstResponse
						response.Request = request
						return &response, tt.firstError
					}
					return nil, tt.firstError
				}
				if tt.terminalError != nil {
					return nil, tt.terminalError
				}
				return &http.Response{StatusCode: http.StatusNoContent, Header: make(http.Header), Body: http.NoBody, Request: request}, nil
			})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			transport := replayablePostRetryTransport{
				base: base, server: server,
				provider: accounts.ProviderCodex, model: "gpt-6-astra",
				agent: "codex", session: "thread-1:2", account: "account-a",
				maxAttempts: 2,
				now:         func() time.Time { return fixedNow },
			}
			transport.sleep = func(waitCtx context.Context, wait time.Duration) bool {
				status := registry.forSession("codex", "thread-1")
				if status == nil {
					t.Fatal("retry state was not active during backoff")
				}
				if status.Provider != accounts.ProviderCodex || status.Model != "gpt-6-astra" || status.AccountID != "account-a" || status.Attempt != 2 || status.Reason != tt.reason || !status.NextRetryAt.Equal(fixedNow.Add(wait)) {
					t.Fatalf("retry state during backoff = %+v", status)
				}
				if tt.cancelDuringWait {
					cancel()
					return sleepForRetry(waitCtx, wait)
				}
				return true
			}

			request, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://example.invalid/responses", strings.NewReader("{}"))
			if err != nil {
				t.Fatal(err)
			}
			request.GetBody = func() (io.ReadCloser, error) {
				return io.NopCloser(strings.NewReader("{}")), nil
			}
			response, gotErr := transport.RoundTrip(request)
			if response != nil && response.Body != nil {
				_ = response.Body.Close()
			}
			switch {
			case tt.cancelDuringWait:
				if gotErr != nil || calls != 1 {
					t.Fatalf("canceled retry returned calls=%d err=%v, want original 408 after one call", calls, gotErr)
				}
			case tt.terminalError != nil:
				if !errors.Is(gotErr, tt.terminalError) || calls != 2 {
					t.Fatalf("terminal retry returned calls=%d err=%v, want %v after two calls", calls, gotErr, tt.terminalError)
				}
			default:
				if gotErr != nil || response == nil || response.StatusCode != http.StatusNoContent || calls != 2 {
					t.Fatalf("successful retry returned calls=%d response=%v err=%v", calls, response, gotErr)
				}
			}
			if status := registry.forSession("codex", "thread-1"); status != nil {
				t.Fatalf("retry state remained after transport completed: %+v", status)
			}
		})
	}
}

func TestReplayablePostRetryStatusUsesNextAttemptAccount(t *testing.T) {
	tests := []struct {
		name           string
		requestAccount string
		wantAccount    string
	}{
		{
			name:        "provisional inner failover resets to transport account",
			wantAccount: "account-a",
		},
		{
			name:           "outer attempt account remains targeted",
			requestAccount: "outer-alternate",
			wantAccount:    "outer-alternate",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			registry := newRetryStatusRegistry()
			server := &Server{retryStatuses: registry}
			calls := 0
			base := roundTripFunc(func(request *http.Request) (*http.Response, error) {
				calls++
				response := &http.Response{
					StatusCode: http.StatusNoContent,
					Header:     make(http.Header),
					Body:       http.NoBody,
					Request:    request,
				}
				if calls == 1 {
					response.StatusCode = http.StatusRequestTimeout
					// An inner failover may tag the response with an account that
					// its next RoundTrip will not retain. Retry status must describe
					// the outer request target instead of this provisional route.
					response = tagRoutedResponseAccount(response, accounts.Account{ID: "inner-provisional", Provider: accounts.ProviderCodex})
				}
				return response, nil
			})
			transport := replayablePostRetryTransport{
				base: base, server: server,
				provider: accounts.ProviderCodex, model: "gpt-6-astra",
				agent: "codex", session: "thread-account:1", account: "account-a",
				maxAttempts: 2,
				now:         func() time.Time { return time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC) },
			}
			transport.sleep = func(_ context.Context, _ time.Duration) bool {
				status := registry.forSession("codex", "thread-account")
				if status == nil || status.AccountID != tt.wantAccount {
					t.Fatalf("retry account = %+v, want %q", status, tt.wantAccount)
				}
				return true
			}

			ctx := context.Background()
			if tt.requestAccount != "" {
				ctx = withAttemptAccount(ctx, accounts.Account{ID: tt.requestAccount, Provider: accounts.ProviderCodex})
			}
			request, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://example.invalid/responses", strings.NewReader("{}"))
			if err != nil {
				t.Fatal(err)
			}
			request.GetBody = func() (io.ReadCloser, error) {
				return io.NopCloser(strings.NewReader("{}")), nil
			}
			response, err := transport.RoundTrip(request)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			if calls != 2 || response.StatusCode != http.StatusNoContent {
				t.Fatalf("calls=%d status=%d, want successful second attempt", calls, response.StatusCode)
			}
		})
	}
}
