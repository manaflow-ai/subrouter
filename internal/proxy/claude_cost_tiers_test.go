package proxy

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/selectacct"
)

const anthropicCreditBalanceBody = `{"type":"error","error":{"type":"invalid_request_error","message":"Your credit balance is too low to access the Anthropic API. Please go to Plans & Billing to upgrade or purchase credits."},"request_id":"req_test"}`

func TestResponseClaudeCreditExhausted(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		want   bool
	}{
		{name: "billing 400", status: http.StatusBadRequest, body: anthropicCreditBalanceBody, want: true},
		{name: "other 400", status: http.StatusBadRequest, body: `{"type":"error","error":{"type":"invalid_request_error","message":"messages: field required"}}`},
		{name: "rate limit 429", status: http.StatusTooManyRequests, body: anthropicCreditBalanceBody},
		{name: "success is never read", status: http.StatusOK, body: anthropicCreditBalanceBody},
	} {
		t.Run(tc.name, func(t *testing.T) {
			response := &http.Response{StatusCode: tc.status, Body: io.NopCloser(strings.NewReader(tc.body))}
			got, err := responseClaudeCreditExhausted(response)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("credit exhausted = %v, want %v", got, tc.want)
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
}

func claudeCostTierRequest(t *testing.T, apiKey string) *http.Request {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Api-Key", apiKey)
	req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader(`{}`)), nil }
	return req
}

// A key whose free credits are spent answers 400, not 429. The request must
// move to the next key and the spent key must leave routing, or every new
// session would keep landing on it and the pool would never reach tier 3.
func TestClaudeSpentCreditKeyFailsOverAndIsHeldOut(t *testing.T) {
	server := Server{
		Accounts: []accounts.Account{
			{ID: "claude:spent", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeAPIKey, Token: "tok-spent"},
			{ID: "claude:funded", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeAPIKey, Token: "tok-funded"},
		},
		SchedulerRef: selectacct.NewSchedulerRef(selectacct.NewScheduler(nil)),
	}
	var spentHits, fundedHits int
	stub := &stubRoundTripper{responses: func(req *http.Request) *http.Response {
		if req.Header.Get("X-Api-Key") == "tok-spent" {
			spentHits++
			return &http.Response{StatusCode: http.StatusBadRequest, Header: http.Header{},
				Body: io.NopCloser(strings.NewReader(anthropicCreditBalanceBody))}
		}
		fundedHits++
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{},
			Body: io.NopCloser(strings.NewReader(`{"id":"ok"}`))}
	}}
	transport := usageLimitRetryTransport{
		base: stub, server: &server, provider: accounts.ProviderClaude,
		agent: "claude", session: "session-spent-credit", account: "claude:spent",
		method: http.MethodPost, path: "/v1/messages", maxAttempts: 3,
		budget: newAttemptBudget(2),
	}
	response, err := transport.RoundTrip(claudeCostTierRequest(t, "tok-spent"))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK || spentHits != 1 || fundedHits != 1 {
		t.Fatalf("status %d, spent hits %d, funded hits %d; want 200 after one failover", response.StatusCode, spentHits, fundedHits)
	}
	until, held := server.SchedulerRef.CreditExhaustedUntil(accounts.ProviderClaude, "claude:spent", time.Now())
	if !held || until.Before(time.Now().Add(claudeCreditExhaustionHold-time.Minute)) {
		t.Fatalf("credit hold = %v, %v; want about %s", until, held, claudeCreditExhaustionHold)
	}
	if !server.scheduler().Exhausted(accounts.ProviderClaude, "claude:spent") {
		t.Fatal("spent key still scores as routable")
	}
	statuses := server.withClaudeCostTiers([]AccountUsageStatus{
		{AccountStatus: AccountStatus{ID: "claude:spent", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeAPIKey}},
	})
	if statuses[0].CostTier != claudeCostTierAPICredits || statuses[0].CreditsExhaustedUntil.IsZero() {
		t.Fatalf("status = %+v, want api-credits tier with a credits_exhausted_until hold", statuses[0])
	}
}

// With every tier spent the client must get subrouter's 503, which `cr
// claude` turns into its Bedrock route, never Anthropic's billing 400.
func TestClaudeAllCreditKeysSpentAnswersPoolExhausted(t *testing.T) {
	server := Server{
		Accounts: []accounts.Account{
			{ID: "claude:only", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeAPIKey, Token: "tok-only"},
		},
		SchedulerRef: selectacct.NewSchedulerRef(selectacct.NewScheduler(nil)),
	}
	stub := &stubRoundTripper{responses: func(*http.Request) *http.Response {
		return &http.Response{StatusCode: http.StatusBadRequest, Header: http.Header{},
			Body: io.NopCloser(strings.NewReader(anthropicCreditBalanceBody))}
	}}
	transport := usageLimitRetryTransport{
		base: stub, server: &server, provider: accounts.ProviderClaude,
		agent: "claude", session: "session-all-spent", account: "claude:only",
		method: http.MethodPost, path: "/v1/messages", maxAttempts: 3,
		budget: newAttemptBudget(2),
	}
	response, err := transport.RoundTrip(claudeCostTierRequest(t, "tok-only"))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want the pool-exhausted 503", response.StatusCode)
	}
}

// Failover follows the cost order: a free-credit API key is spent before a
// subscription's paid extra usage, and extra usage is reached once the keys
// are spent.
func TestClaudeRetryUsesAPICreditsBeforeExtraUsage(t *testing.T) {
	server := Server{
		Accounts: []accounts.Account{
			{ID: "paid", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "tok-paid"},
			{ID: "other", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "tok-other"},
			{ID: "claude:credits", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeAPIKey, Token: "tok-credits"},
		},
		SchedulerRef: selectacct.NewSchedulerRef(selectacct.NewScheduler([]selectacct.Score{
			{AccountID: "paid", Provider: accounts.ProviderClaude, Headroom: 0, ShortHeadroom: 0, WeeklyHeadroom: 0, WeeklyHeadroomKnown: true,
				ClaudeExtraUsageEnabled: true, ClaudeExtraUsageKnown: true, ClaudeExtraUsageRemaining: 900},
			{AccountID: "other", Provider: accounts.ProviderClaude, Headroom: 0, ShortHeadroom: 0, WeeklyHeadroom: 0, WeeklyHeadroomKnown: true},
			{AccountID: "claude:credits", Provider: accounts.ProviderClaude, Headroom: 0.01, ShortHeadroom: 0.01,
				MissingModelSupport: selectacct.ModelSupportUnknown},
		})),
	}
	server.RefreshAccountFn = func(_ context.Context, account accounts.Account) (accounts.Account, error) {
		return account, nil
	}
	tried := map[string]struct{}{"other": {}}
	got, err := server.oauthRetryCandidate(t.Context(), accounts.ProviderClaude, "claude", "session-cost-order", "", "", tried, false, true)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "claude:credits" {
		t.Fatalf("retry picked %q, want the free-credit key before paid extra usage", got.ID)
	}

	server.markClaudeCreditExhausted("claude:credits")
	got, err = server.oauthRetryCandidate(t.Context(), accounts.ProviderClaude, "claude", "session-cost-order", "", "", tried, false, true)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "paid" {
		t.Fatalf("retry picked %q, want extra usage once the keys are spent", got.ID)
	}
	statuses := server.withClaudeCostTiers([]AccountUsageStatus{
		{AccountStatus: AccountStatus{ID: "paid", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth}},
		{AccountStatus: AccountStatus{ID: "other", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth}},
	})
	if statuses[0].CostTier != claudeCostTierExtraUsage || statuses[0].CostTierRank != 3 {
		t.Fatalf("funded cooked plan status = %+v, want extra-usage tier 3", statuses[0])
	}
	if statuses[1].CostTier != claudeCostTierPlan || statuses[1].CostTierRank != 1 {
		t.Fatalf("unfunded plan status = %+v, want plan tier 1", statuses[1])
	}
}
