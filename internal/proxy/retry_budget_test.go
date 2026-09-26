package proxy

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
)

func TestAggregateRetryBudgetIncludesSameAccountOverloadRepairs(t *testing.T) {
	t.Parallel()
	budget := newAttemptBudget(replayablePostMaxAttempts - 1)
	calls := 0
	base := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		calls++
		status := http.StatusInternalServerError
		if calls%3 == 0 {
			status = http.StatusRequestTimeout
		}
		return &http.Response{
			StatusCode: status,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{"error":{"type":"overloaded_error"}}`)),
			Request:    request,
		}, nil
	})
	inner := usageLimitRetryTransport{
		base: base, provider: accounts.ProviderClaude, maxAttempts: replayablePostMaxAttempts,
		budget: budget,
		sleep:  func(context.Context, time.Duration) error { return nil },
	}
	outer := replayablePostRetryTransport{
		base: inner, maxAttempts: replayablePostMaxAttempts, budget: budget,
	}
	request := retryBudgetRequest(t, []byte(`{"model":"claude","messages":[]}`))
	response, err := outer.RoundTrip(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if calls != replayablePostMaxAttempts {
		t.Fatalf("base calls = %d, want aggregate cap %d", calls, replayablePostMaxAttempts)
	}
}

func TestAggregateRetryBudgetIncludesSealedReasoningRepair(t *testing.T) {
	t.Parallel()
	budget := newAttemptBudget(replayablePostMaxAttempts - 1)
	calls := 0
	base := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		calls++
		status := http.StatusBadRequest
		body := `{"error":{"code":"invalid_encrypted_content","message":"The encrypted content could not be verified."}}`
		if calls%2 == 0 {
			status = http.StatusRequestTimeout
			body = `{"error":{"message":"timeout"}}`
		}
		return &http.Response{
			StatusCode: status,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(body)),
			Request:    request,
		}, nil
	})
	inner := usageLimitRetryTransport{
		base: base, provider: accounts.ProviderCodex, maxAttempts: replayablePostMaxAttempts,
		budget: budget,
	}
	outer := replayablePostRetryTransport{
		base: inner, maxAttempts: replayablePostMaxAttempts, budget: budget,
	}
	body := []byte(`{"model":"gpt-5.6-codex","input":[{"type":"reasoning","id":"rs_1","encrypted_content":"sealed","summary":[]}]}`)
	request := retryBudgetRequest(t, body)
	response, err := outer.RoundTrip(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if calls != replayablePostMaxAttempts {
		t.Fatalf("base calls = %d, want aggregate cap %d", calls, replayablePostMaxAttempts)
	}
}

func retryBudgetRequest(t *testing.T, body []byte) *http.Request {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "https://upstream.invalid/responses", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(body)), nil
	}
	request.ContentLength = int64(len(body))
	return request
}
