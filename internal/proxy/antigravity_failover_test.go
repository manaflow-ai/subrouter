package proxy

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/manaflow-ai/subrouter/internal/accounts"
)

func TestAntigravityRequestsAreReplayable(t *testing.T) {
	for _, path := range []string{"/antigravity/v1internal:generateContent", "/antigravity/v1internal/loadCodeAssist"} {
		r := &http.Request{Method: http.MethodPost, URL: &url.URL{Path: path}}
		if !retryableUpstreamPostRequest(accounts.ProviderAntigravity, r) {
			t.Errorf("retryableUpstreamPostRequest(%q) = false", path)
		}
	}
	for _, path := range []string{"/antigravity/v1/models", "/antigravity/health"} {
		r := &http.Request{Method: http.MethodPost, URL: &url.URL{Path: path}}
		if retryableUpstreamPostRequest(accounts.ProviderAntigravity, r) {
			t.Errorf("retryableUpstreamPostRequest(%q) = true", path)
		}
	}
}

func TestAntigravity429TriggersAccountFailover(t *testing.T) {
	response := &http.Response{
		StatusCode: http.StatusTooManyRequests,
		Body:       io.NopCloser(strings.NewReader(`{"error":{"status":"RESOURCE_EXHAUSTED"}}`)),
	}
	transport := usageLimitRetryTransport{provider: accounts.ProviderAntigravity}
	limited, exhausted, credentialFailure, err := transport.responseUsageLimited(response)
	if err != nil || !limited || exhausted || credentialFailure {
		t.Fatalf("AGY 429 classification = limited %v exhausted %v credential %v err %v", limited, exhausted, credentialFailure, err)
	}
}

func TestAntigravity401TriggersCredentialFailover(t *testing.T) {
	response := &http.Response{StatusCode: http.StatusUnauthorized, Body: io.NopCloser(strings.NewReader(`{"error":"invalid_token"}`))}
	transport := usageLimitRetryTransport{provider: accounts.ProviderAntigravity}
	limited, exhausted, credentialFailure, err := transport.responseUsageLimited(response)
	if err != nil || !limited || !exhausted || !credentialFailure {
		t.Fatalf("AGY 401 classification = limited %v exhausted %v credential %v err %v", limited, exhausted, credentialFailure, err)
	}
}

func TestAntigravityUnusableResponseLogsReasonWithoutConsumingBody(t *testing.T) {
	var logs bytes.Buffer
	response := &http.Response{
		StatusCode: http.StatusTooManyRequests,
		Header:     http.Header{"Retry-After": []string{"3"}},
		Body:       io.NopCloser(strings.NewReader(`{"error":{"status":"RESOURCE_EXHAUSTED","message":"model allocation temporarily unavailable"}}`)),
	}
	transport := usageLimitRetryTransport{
		provider:  accounts.ProviderAntigravity,
		logger:    slog.New(slog.NewTextHandler(&logs, nil)),
		agent:     "antigravity",
		session:   "session-1",
		method:    http.MethodPost,
		path:      "/antigravity/v1internal:streamGenerateContent",
		upstream:  "https://cloudcode-pa.googleapis.com",
		poolModel: "antigravity-gemini",
	}
	transport.logAntigravityUnusableResponse(response, "account-a")
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "model allocation temporarily unavailable") {
		t.Fatalf("diagnostic logging consumed or changed response body: %q", body)
	}
	for _, want := range []string{"antigravity Cloud Code account unusable", "account-a", "model allocation temporarily unavailable", "retry_after=3"} {
		if !strings.Contains(logs.String(), want) {
			t.Fatalf("logs missing %q: %s", want, logs.String())
		}
	}
	if strings.Contains(logs.String(), "Authorization") || strings.Contains(logs.String(), "prompt") {
		t.Fatalf("diagnostic log appears to include sensitive content: %s", logs.String())
	}
}

func TestCodexHeaderless429FailsOverWithoutExhaustingAccount(t *testing.T) {
	response := &http.Response{StatusCode: http.StatusTooManyRequests, Body: io.NopCloser(strings.NewReader(`{"error":{"type":"rate_limit_error"}}`))}
	transport := usageLimitRetryTransport{provider: accounts.ProviderCodex}
	limited, exhausted, credentialFailure, err := transport.responseUsageLimited(response)
	if err != nil || !limited || exhausted || credentialFailure {
		t.Fatalf("Codex headerless 429 classification = limited %v exhausted %v credential %v err %v", limited, exhausted, credentialFailure, err)
	}
}
