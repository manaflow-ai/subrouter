package proxy

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// countingCloseTransport records how often the retry path discards the shared
// idle-connection pool.
type countingCloseTransport struct {
	attempts atomic.Int64
	closes   atomic.Int64
	failFor  int64
}

func (t *countingCloseTransport) RoundTrip(_ *http.Request) (*http.Response, error) {
	n := t.attempts.Add(1)
	if n <= t.failFor {
		return nil, errors.New("write tcp4 10.0.0.1:1->10.0.0.2:443: use of closed network connection")
	}
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("ok")), Header: http.Header{}}, nil
}

func (t *countingCloseTransport) CloseIdleConnections() { t.closes.Add(1) }

func newReplayableRequest(t *testing.T) *http.Request {
	t.Helper()
	request, err := http.NewRequest(http.MethodPost, "https://example.invalid/responses", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	request.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader("{}")), nil }
	return request
}

// A retry must not discard the pool shared by every other session. Doing so
// forces unrelated in-flight work to re-dial, and under sustained failure the
// extra dials exhaust the machine's ephemeral ports, which causes more
// failures.
func TestRetryDoesNotDiscardSharedIdlePool(t *testing.T) {
	base := &countingCloseTransport{failFor: 3}
	transport := replayablePostRetryTransport{
		base:        base,
		maxAttempts: 6,
		method:      http.MethodPost,
		path:        "/responses",
	}

	response, err := transport.RoundTrip(newReplayableRequest(t))
	if err != nil {
		t.Fatalf("expected the retry to eventually succeed, got %v", err)
	}
	defer response.Body.Close()

	if got := base.attempts.Load(); got != 4 {
		t.Fatalf("made %d attempts, want 4 (3 failures then success)", got)
	}
	if got := base.closes.Load(); got != 0 {
		t.Fatalf("retry discarded the shared idle pool %d times; that forces every other session to re-dial and is how the port range was exhausted", got)
	}
}

// Retries must still actually retry, and still back off between attempts.
func TestRetryStillRetriesAndBacksOff(t *testing.T) {
	base := &countingCloseTransport{failFor: 2}
	transport := replayablePostRetryTransport{
		base:        base,
		maxAttempts: 6,
		method:      http.MethodPost,
		path:        "/responses",
	}

	start := time.Now()
	response, err := transport.RoundTrip(newReplayableRequest(t))
	if err != nil {
		t.Fatalf("retry should have recovered: %v", err)
	}
	defer response.Body.Close()

	if got := base.attempts.Load(); got != 3 {
		t.Fatalf("made %d attempts, want 3", got)
	}
	if elapsed := time.Since(start); elapsed < retryBackoff(1) {
		t.Fatalf("recovered in %s, want at least one backoff interval of %s", elapsed, retryBackoff(1))
	}
}
