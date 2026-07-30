package proxy

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
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
