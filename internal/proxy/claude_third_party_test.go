package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/selectacct"
)

const anthropicThirdPartyExtraUsageBody = `{"type":"error","error":{"type":"invalid_request_error","message":"Third-party apps now draw from your extra usage, not your plan limits. Turn on extra usage to keep going."},"request_id":"req_test"}`

func TestResponseClaudeThirdPartyExtraUsage(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		want   bool
	}{
		{name: "observed 400", status: http.StatusBadRequest, body: anthropicThirdPartyExtraUsageBody, want: true},
		{name: "reworded", status: http.StatusBadRequest, body: `{"type":"error","error":{"message":"Requests from third party applications are now billed to extra usage."}}`, want: true},
		{name: "top-level message 403", status: http.StatusForbidden, body: `{"message":"Third-party apps draw from your Extra Usage"}`, want: true},
		{name: "credit balance is not third-party", status: http.StatusBadRequest, body: anthropicCreditBalanceBody},
		{name: "extra usage alone", status: http.StatusBadRequest, body: `{"error":{"message":"Your extra usage limit is reached."}}`},
		{name: "other 400", status: http.StatusBadRequest, body: `{"type":"error","error":{"type":"invalid_request_error","message":"messages: field required"}}`},
		{name: "rate limit 429", status: http.StatusTooManyRequests, body: anthropicThirdPartyExtraUsageBody},
		{name: "success is never read", status: http.StatusOK, body: anthropicThirdPartyExtraUsageBody},
	} {
		t.Run(tc.name, func(t *testing.T) {
			response := &http.Response{StatusCode: tc.status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(tc.body))}
			got, err := responseClaudeThirdPartyExtraUsage(response)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("third-party extra usage = %v, want %v", got, tc.want)
			}
			body, err := io.ReadAll(response.Body)
			if err != nil {
				t.Fatal(err)
			}
			if string(body) != tc.body {
				t.Fatalf("body after inspection = %q, want it preserved", body)
			}
		})
	}
	if claudeCreditExhaustedMessage([]byte(anthropicThirdPartyExtraUsageBody)) {
		t.Fatal("the third-party refusal must not read as a spent API credit balance")
	}
}

func claudeThirdPartyOAuthRequest(t *testing.T, token string) *http.Request {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader(`{}`)), nil }
	return req
}

func assertClaudeAccountUnmarked(t *testing.T, server Server, accountID string) {
	t.Helper()
	if server.scheduler().Exhausted(accounts.ProviderClaude, accountID) {
		t.Fatalf("%s scores as exhausted after a third-party refusal", accountID)
	}
	if until, blocked := server.SchedulerRef.ExplicitBlockedUntilFor(accounts.ProviderClaude, accountID, "", time.Now()); blocked {
		t.Fatalf("%s is held out until %s after a third-party refusal", accountID, until)
	}
}

// Every subscription account refuses a non-Claude-Code client the same way,
// so the request must not fan out across them, no account may be marked, and
// with no API key the client gets subrouter's explanation, not a retry loop.
func TestClaudeThirdPartyExtraUsageNeverRetriesOAuth(t *testing.T) {
	sessions := newSessionStore(t)
	if _, err := sessions.Put("claude", "session-third-party", "oauth-a", ""); err != nil {
		t.Fatal(err)
	}
	server := Server{
		Accounts: []accounts.Account{
			{ID: "oauth-a", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "tok-a"},
			{ID: "oauth-b", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "tok-b"},
		},
		Sessions:     sessions,
		SchedulerRef: selectacct.NewSchedulerRef(selectacct.NewScheduler(nil)),
	}
	stub := &stubRoundTripper{responses: func(*http.Request) *http.Response {
		return &http.Response{StatusCode: http.StatusBadRequest, Header: http.Header{"Content-Type": []string{"application/json"}},
			Body: io.NopCloser(strings.NewReader(anthropicThirdPartyExtraUsageBody))}
	}}
	transport := usageLimitRetryTransport{
		base: stub, server: &server, provider: accounts.ProviderClaude,
		agent: "claude", session: "session-third-party", account: "oauth-a",
		method: http.MethodPost, path: "/v1/messages", maxAttempts: 3,
		budget: newAttemptBudget(2),
	}
	response, err := transport.RoundTrip(claudeThirdPartyOAuthRequest(t, "tok-a"))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if stub.calls != 1 {
		t.Fatalf("upstream calls = %d, want 1 (no OAuth retry)", stub.calls)
	}
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want the upstream 400", response.StatusCode)
	}
	if response.Header.Get("X-Should-Retry") != "false" {
		t.Fatalf("X-Should-Retry = %q, want false", response.Header.Get("X-Should-Retry"))
	}
	var payload struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("body %q is not Anthropic error JSON: %v", body, err)
	}
	if payload.Type != "error" || payload.Error.Type != "invalid_request_error" ||
		!strings.Contains(payload.Error.Message, "isn't Claude Code") || !strings.Contains(payload.Error.Message, "API-key account") {
		t.Fatalf("error body = %+v, want subrouter's third-party explanation", payload)
	}
	assertClaudeAccountUnmarked(t, server, "oauth-a")
	assertClaudeAccountUnmarked(t, server, "oauth-b")
	if assignment, ok := sessions.Get("claude", "session-third-party"); !ok || assignment.AccountID != "oauth-a" {
		t.Fatalf("sticky assignment = %+v, ok=%v; want it left on oauth-a", assignment, ok)
	}
}

// An API key is billed per token whatever the client, so a third-party
// refusal moves the request (and the session) to an untried key, skipping the
// other subscription accounts, and leaves the refusing account unmarked.
func TestClaudeThirdPartyExtraUsageRoutesToAPIKey(t *testing.T) {
	sessions := newSessionStore(t)
	if _, err := sessions.Put("claude", "session-third-party-key", "oauth-a", ""); err != nil {
		t.Fatal(err)
	}
	server := Server{
		Accounts: []accounts.Account{
			{ID: "oauth-a", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "tok-a"},
			{ID: "oauth-b", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "tok-b"},
			{ID: "claude:key", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeAPIKey, Token: "tok-key"},
		},
		Sessions:     sessions,
		SchedulerRef: selectacct.NewSchedulerRef(selectacct.NewScheduler(nil)),
	}
	var oauthHits, keyHits int
	stub := &stubRoundTripper{responses: func(req *http.Request) *http.Response {
		if req.Header.Get("X-Api-Key") == "tok-key" {
			keyHits++
			if req.Header.Get("Authorization") != "" {
				t.Errorf("API-key replay still carries an OAuth Authorization header")
			}
			return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}},
				Body: io.NopCloser(strings.NewReader(`{"id":"ok"}`))}
		}
		oauthHits++
		return &http.Response{StatusCode: http.StatusBadRequest, Header: http.Header{"Content-Type": []string{"application/json"}},
			Body: io.NopCloser(strings.NewReader(anthropicThirdPartyExtraUsageBody))}
	}}
	transport := usageLimitRetryTransport{
		base: stub, server: &server, provider: accounts.ProviderClaude,
		agent: "claude", session: "session-third-party-key", account: "oauth-a",
		method: http.MethodPost, path: "/v1/messages", maxAttempts: 3,
		budget: newAttemptBudget(2),
	}
	response, err := transport.RoundTrip(claudeThirdPartyOAuthRequest(t, "tok-a"))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK || string(body) != `{"id":"ok"}` || oauthHits != 1 || keyHits != 1 {
		t.Fatalf("status %d body %q oauth=%d key=%d; want the key's 200 after one OAuth refusal", response.StatusCode, body, oauthHits, keyHits)
	}
	assertClaudeAccountUnmarked(t, server, "oauth-a")
	if assignment, ok := sessions.Get("claude", "session-third-party-key"); !ok || assignment.AccountID != "claude:key" {
		t.Fatalf("sticky assignment = %+v, ok=%v; want the API key", assignment, ok)
	}
}

// A spent or held-out key cannot take the request; the client then gets the
// explanation rather than an OAuth fan-out.
func TestClaudeThirdPartyExtraUsageSkipsHeldAPIKey(t *testing.T) {
	server := Server{
		Accounts: []accounts.Account{
			{ID: "oauth-a", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "tok-a"},
			{ID: "oauth-b", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "tok-b"},
			{ID: "claude:spent", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeAPIKey, Token: "tok-spent"},
		},
		SchedulerRef: selectacct.NewSchedulerRef(selectacct.NewScheduler(nil)),
	}
	server.markClaudeCreditExhausted("claude:spent")
	stub := &stubRoundTripper{responses: func(*http.Request) *http.Response {
		return &http.Response{StatusCode: http.StatusBadRequest, Header: http.Header{},
			Body: io.NopCloser(strings.NewReader(anthropicThirdPartyExtraUsageBody))}
	}}
	transport := usageLimitRetryTransport{
		base: stub, server: &server, provider: accounts.ProviderClaude,
		agent: "claude", session: "session-third-party-held", account: "oauth-a",
		method: http.MethodPost, path: "/v1/messages", maxAttempts: 3,
		budget: newAttemptBudget(2),
	}
	response, err := transport.RoundTrip(claudeThirdPartyOAuthRequest(t, "tok-a"))
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if stub.calls != 1 || response.StatusCode != http.StatusBadRequest || response.Header.Get("X-Subrouter-Error") != "claude_third_party_extra_usage" {
		t.Fatalf("calls %d status %d error %q; want one call and subrouter's 400", stub.calls, response.StatusCode, response.Header.Get("X-Subrouter-Error"))
	}
}
