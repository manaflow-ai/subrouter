package proxy

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

// Native Claude clients treat 409 as a bounded, replay-safe transient. Keep
// that compatibility in the generic replay layer without moving 429/5xx into
// it: those statuses belong to the provider-specific account and overload
// policies above this layer.
func TestReplayablePostRetriesConflict(t *testing.T) {
	var attempts int
	var bodies []string
	base := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		attempts++
		body, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		bodies = append(bodies, string(body))
		status := http.StatusConflict
		responseBody := `{"error":"conflict"}`
		if attempts == 2 {
			status = http.StatusOK
			responseBody = `{"id":"msg-recovered"}`
		}
		return &http.Response{
			StatusCode: status,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(responseBody)),
			Request:    req,
		}, nil
	})

	req, err := http.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages", strings.NewReader(`{"model":"claude-sonnet-4-5"}`))
	if err != nil {
		t.Fatal(err)
	}
	body := `{"model":"claude-sonnet-4-5"}`
	req.Body = io.NopCloser(strings.NewReader(body))
	req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader(body)), nil }

	transport := replayablePostRetryTransport{
		base:        base,
		maxAttempts: 2,
		method:      http.MethodPost,
		path:        "/v1/messages",
	}
	response, err := transport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 after a 409 retry", response.StatusCode)
	}
	if attempts != 2 {
		t.Fatalf("upstream attempts = %d, want 2", attempts)
	}
	if len(bodies) != 2 || bodies[0] != body || bodies[1] != body {
		t.Fatalf("replayed bodies = %#v, want the same request body twice", bodies)
	}
}

func TestReplayablePostRetryStatusKeepsProviderPoliciesOutOfGenericLayer(t *testing.T) {
	for _, test := range []struct {
		status int
		want   bool
	}{
		{status: http.StatusRequestTimeout, want: true},
		{status: http.StatusConflict, want: true},
		{status: http.StatusTooManyRequests, want: false},
		{status: http.StatusInternalServerError, want: false},
	} {
		response := &http.Response{StatusCode: test.status}
		if got := retryablePostUpstreamStatus(response); got != test.want {
			t.Errorf("retryablePostUpstreamStatus(%d) = %t, want %t", test.status, got, test.want)
		}
	}
}
