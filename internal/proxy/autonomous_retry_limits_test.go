package proxy

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func autonomousLimitsRequest(t *testing.T) *http.Request {
	t.Helper()
	request, err := http.NewRequest(http.MethodPost, "https://example.test/responses", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	return request
}

func autonomousLimitsResponse(request *http.Request, status int, retryAfter string) *http.Response {
	headers := make(http.Header)
	if retryAfter != "" {
		headers.Set("Retry-After", retryAfter)
	}
	return &http.Response{
		StatusCode: status,
		Header:     headers,
		Body:       io.NopCloser(strings.NewReader("upstream response")),
		Request:    request,
	}
}

func TestAutonomousRetryHonorsProviderRetryAfter(t *testing.T) {
	now := time.Unix(1800000000, 0)
	var waits []time.Duration
	attempts := 0
	transport := autonomousAgentRetryTransport{
		base: autonomousRoundTripperFunc(func(r *http.Request) (*http.Response, error) {
			attempts++
			if attempts == 1 {
				return autonomousLimitsResponse(r, http.StatusTooManyRequests, "90"), nil
			}
			return autonomousLimitsResponse(r, http.StatusOK, ""), nil
		}),
		now: func() time.Time { return now },
		sleep: func(_ context.Context, delay time.Duration) bool {
			waits = append(waits, delay)
			now = now.Add(delay)
			return true
		},
	}
	response, err := transport.RoundTrip(autonomousLimitsRequest(t))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK || attempts != 2 {
		t.Fatalf("status=%d attempts=%d, want 200 after two attempts", response.StatusCode, attempts)
	}
	if len(waits) != 1 || waits[0] != 90*time.Second {
		t.Fatalf("backoff=%v, want provider Retry-After of 90s", waits)
	}
}

func TestAutonomousRetryReturnsLongProviderThrottleUnmodified(t *testing.T) {
	attempts := 0
	sleeps := 0
	transport := autonomousAgentRetryTransport{
		base: autonomousRoundTripperFunc(func(r *http.Request) (*http.Response, error) {
			attempts++
			return autonomousLimitsResponse(r, http.StatusTooManyRequests, "7200"), nil
		}),
		sleep: func(_ context.Context, _ time.Duration) bool {
			sleeps++
			return true
		},
	}
	response, err := transport.RoundTrip(autonomousLimitsRequest(t))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusTooManyRequests || response.Header.Get("Retry-After") != "7200" {
		t.Fatalf("status=%d header=%q, want original 429 and Retry-After", response.StatusCode, response.Header.Get("Retry-After"))
	}
	body, err := io.ReadAll(response.Body)
	if err != nil || string(body) != "upstream response" {
		t.Fatalf("body=%q err=%v, want intact response body", body, err)
	}
	if attempts != 1 || sleeps != 0 {
		t.Fatalf("attempts=%d sleeps=%d, want one request and no retry", attempts, sleeps)
	}
}

func TestAutonomousRetryAllowsRecoveryBeyondArbitraryPassCount(t *testing.T) {
	now := time.Unix(1800000000, 0)
	attempts := 0
	sleeps := 0
	const recoverOn = 14 // More than the previous arbitrary 12-pass limit.
	transport := autonomousAgentRetryTransport{
		base: autonomousRoundTripperFunc(func(r *http.Request) (*http.Response, error) {
			attempts++
			if attempts == recoverOn {
				return autonomousLimitsResponse(r, http.StatusOK, ""), nil
			}
			return autonomousLimitsResponse(r, http.StatusServiceUnavailable, ""), nil
		}),
		now: func() time.Time { return now },
		sleep: func(_ context.Context, delay time.Duration) bool {
			sleeps++
			now = now.Add(delay)
			return true
		},
	}
	response, err := transport.RoundTrip(autonomousLimitsRequest(t))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK || attempts != recoverOn || sleeps != recoverOn-1 {
		t.Fatalf("status=%d attempts=%d sleeps=%d, want recovery at request %d",
			response.StatusCode, attempts, sleeps, recoverOn)
	}
}

func TestAutonomousRetryWaitsForReportedPoolRecovery(t *testing.T) {
	now := time.Unix(1800000000, 0)
	attempts := 0
	var waits []time.Duration
	transport := autonomousAgentRetryTransport{
		base: autonomousRoundTripperFunc(func(r *http.Request) (*http.Response, error) {
			attempts++
			if attempts == 1 {
				// The controller knows the next available subscription reset.
				return autonomousLimitsResponse(r, http.StatusServiceUnavailable, "300"), nil
			}
			return autonomousLimitsResponse(r, http.StatusOK, ""), nil
		}),
		now: func() time.Time { return now },
		sleep: func(_ context.Context, delay time.Duration) bool {
			waits = append(waits, delay)
			now = now.Add(delay)
			return true
		},
	}
	response, err := transport.RoundTrip(autonomousLimitsRequest(t))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK || attempts != 2 {
		t.Fatalf("status=%d attempts=%d, want one request at provider reset", response.StatusCode, attempts)
	}
	if len(waits) != 1 || waits[0] != 300*time.Second {
		t.Fatalf("waits=%v, want exactly one five-minute wait with no intermediate probes", waits)
	}
}

func TestAutonomousRetryRespectsElapsedCeiling(t *testing.T) {
	now := time.Unix(1800000000, 0)
	attempts := 0
	sleeps := 0
	transport := autonomousAgentRetryTransport{
		base: autonomousRoundTripperFunc(func(r *http.Request) (*http.Response, error) {
			attempts++
			return autonomousLimitsResponse(r, http.StatusServiceUnavailable, ""), nil
		}),
		now: func() time.Time { return now },
		sleep: func(_ context.Context, _ time.Duration) bool {
			sleeps++
			// Simulate a long wait or delayed upstream operation before
			// the next response: its following wait exceeds the budget.
			now = now.Add(autonomousRetryMaxElapsed - time.Second)
			return true
		},
	}
	response, err := transport.RoundTrip(autonomousLimitsRequest(t))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusServiceUnavailable || attempts != 2 || sleeps != 1 {
		t.Fatalf("status=%d attempts=%d sleeps=%d, want 503 after two tries and one wait",
			response.StatusCode, attempts, sleeps)
	}
}
