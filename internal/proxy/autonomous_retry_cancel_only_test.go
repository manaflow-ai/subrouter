package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type autonomousRoundTripperFunc func(*http.Request) (*http.Response, error)

func (f autonomousRoundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func TestAutonomousRetryContinuesPastOneInternalPass(t *testing.T) {
	var attempts atomic.Int32
	transport := autonomousAgentRetryTransport{
		base: autonomousRoundTripperFunc(func(req *http.Request) (*http.Response, error) {
			attempt := attempts.Add(1)
			status := http.StatusServiceUnavailable
			if attempt == replayablePostMaxAttempts+2 {
				status = http.StatusOK
			}
			return &http.Response{
				StatusCode: status,
				Body:       io.NopCloser(strings.NewReader("")),
				Header:     make(http.Header),
				Request:    req,
			}, nil
		}),
		budget:         newAttemptBudget(replayablePostMaxAttempts - 1),
		retriesPerPass: replayablePostMaxAttempts - 1,
		sleep:          func(context.Context, time.Duration) bool { return true },
	}

	request := &http.Request{
		Method: "POST",
		URL:    mustAutonomousParseURL("http://upstream.test/responses"),
		Header: make(http.Header),
		Body:   io.NopCloser(strings.NewReader("{}")),
	}
	request.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader("{}")), nil
	}
	response, err := transport.RoundTrip(request)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.StatusCode)
	}
	if got := attempts.Load(); got != replayablePostMaxAttempts+2 {
		t.Fatalf("attempts = %d, want %d", got, replayablePostMaxAttempts+2)
	}
}

func mustAutonomousParseURL(raw string) *url.URL {
	parsed, err := url.Parse(raw)
	if err != nil {
		panic(err)
	}
	return parsed
}

func TestLegacyCapacityRetryableHeaderGetsAutonomousPolicy(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	if agentRetryPolicyFor(request).autonomous() {
		t.Fatal("plain request should keep the bounded policy")
	}
	request.Header.Set(CodexCapacityRetryableHeader, "1")
	if !agentRetryPolicyFor(request).autonomous() {
		t.Fatal("legacy capacity-retryable launch should retry silently")
	}
}
