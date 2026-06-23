package accounts

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

const (
	// codexRateLimitResetCreditsURL is the wham endpoint Codex.app uses to list
	// and consume ChatGPT Pro rate-limit reset credits. It is a var (not a
	// const) so tests can point it at an httptest server.
	codexCLIUserAgent = "codex_cli_rs/0.0.0"
)

var (
	codexRateLimitResetCreditsURL = "https://chatgpt.com/backend-api/wham/rate-limit-reset-credits"
)

// rateLimitResetConsumeURL derives the consume endpoint from the current
// credits URL so tests only need to override codexRateLimitResetCreditsURL.
func rateLimitResetConsumeURL() string {
	return codexRateLimitResetCreditsURL + "/consume"
}

// RateLimitResetCredit is one redeemable rate-limit reset grant returned by
// GET /wham/rate-limit-reset-credits. Status is "available" before redemption
// and "redeemed" after.
type RateLimitResetCredit struct {
	ID              string `json:"id"`
	ResetType       string `json:"reset_type,omitempty"`
	Status          string `json:"status,omitempty"`
	GrantedAt       string `json:"granted_at,omitempty"`
	ExpiresAt       string `json:"expires_at,omitempty"`
	RedeemStartedAt string `json:"redeem_started_at,omitempty"`
	RedeemedAt      string `json:"redeemed_at,omitempty"`
}

// RateLimitResetCreditStatus reports a single account's reset-credit balance.
// It is derived from wham/usage (rate_limit_reset_credits.available_count) so
// callers can decide whether a redeem is possible without a second request.
type RateLimitResetCreditStatus struct {
	Known            bool
	AvailableCount   int
	AvailableCredits []RateLimitResetCredit
}

type rateLimitResetCreditsResponse struct {
	Credits []RateLimitResetCredit `json:"credits"`
}

type rateLimitResetConsumeResponse struct {
	Code   string               `json:"code,omitempty"`
	Credit RateLimitResetCredit `json:"credit"`
}

// ListRateLimitResetCredits calls GET /wham/rate-limit-reset-credits for the
// given account. Only OAuth accounts with an access token are eligible.
func ListRateLimitResetCredits(ctx context.Context, client *http.Client, account Account) ([]RateLimitResetCredit, error) {
	if account.AuthMode != AuthModeOAuth {
		return nil, fmt.Errorf("rate-limit reset is only available for OAuth accounts")
	}
	if account.Token == "" {
		return nil, fmt.Errorf("account has no access token")
	}
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, codexRateLimitResetCreditsURL, nil)
	if err != nil {
		return nil, err
	}
	applyCodexWhamHeaders(req, account)

	res, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return nil, fmt.Errorf("list rate-limit reset credits failed: %s", res.Status)
	}

	var out rateLimitResetCreditsResponse
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		return nil, err
	}
	return out.Credits, nil
}

// ConsumeRateLimitResetCredit redeems a single credit by id via
// POST /wham/rate-limit-reset-credits/consume. redeemRequestID is an
// idempotency key generated client-side; pass a fresh UUID for a new redeem.
func ConsumeRateLimitResetCredit(ctx context.Context, client *http.Client, account Account, creditID, redeemRequestID string) (RateLimitResetCredit, error) {
	if creditID == "" {
		return RateLimitResetCredit{}, fmt.Errorf("credit_id is required")
	}
	if redeemRequestID == "" {
		redeemRequestID = newUUIDv4()
	}
	if account.AuthMode != AuthModeOAuth {
		return RateLimitResetCredit{}, fmt.Errorf("rate-limit reset is only available for OAuth accounts")
	}
	if account.Token == "" {
		return RateLimitResetCredit{}, fmt.Errorf("account has no access token")
	}
	if client == nil {
		client = &http.Client{Timeout: 20 * time.Second}
	}

	body, err := json.Marshal(map[string]string{
		"credit_id":         creditID,
		"redeem_request_id": redeemRequestID,
	})
	if err != nil {
		return RateLimitResetCredit{}, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rateLimitResetConsumeURL(), bytes.NewReader(body))
	if err != nil {
		return RateLimitResetCredit{}, err
	}
	applyCodexWhamHeaders(req, account)
	req.Header.Set("Content-Type", "application/json")

	res, err := client.Do(req)
	if err != nil {
		return RateLimitResetCredit{}, err
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return RateLimitResetCredit{}, fmt.Errorf("consume rate-limit reset credit failed: %s", res.Status)
	}

	var out rateLimitResetConsumeResponse
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		return RateLimitResetCredit{}, err
	}
	return out.Credit, nil
}

// FirstAvailableRateLimitResetCredit lists the account's credits and returns
// the first one whose status is "available". Returns ok=false (no error) when
// the account has credits but none are redeemable right now.
func FirstAvailableRateLimitResetCredit(ctx context.Context, client *http.Client, account Account) (RateLimitResetCredit, bool, error) {
	credits, err := ListRateLimitResetCredits(ctx, client, account)
	if err != nil {
		return RateLimitResetCredit{}, false, err
	}
	for _, credit := range credits {
		if credit.Status == "" || credit.Status == "available" {
			return credit, true, nil
		}
	}
	return RateLimitResetCredit{}, false, nil
}

// RedeemRateLimitReset finds the first available reset credit for the account
// and consumes it. It returns the redeemed credit. This is the one-call path
// for "un-cook this account now".
func RedeemRateLimitReset(ctx context.Context, client *http.Client, account Account) (RateLimitResetCredit, error) {
	credit, ok, err := FirstAvailableRateLimitResetCredit(ctx, client, account)
	if err != nil {
		return RateLimitResetCredit{}, err
	}
	if !ok {
		return RateLimitResetCredit{}, fmt.Errorf("no available rate-limit reset credits for %s", account.Email)
	}
	return ConsumeRateLimitResetCredit(ctx, client, account, credit.ID, newUUIDv4())
}

// applyCodexWhamHeaders sets the headers Codex.app sends to the wham API
// surface. The usage GET works with only Authorization, but the consume POST
// is gated by the upstream's codex-client allowlist, so every reset request
// carries the full identifying header set.
func applyCodexWhamHeaders(req *http.Request, account Account) {
	req.Header.Set("Authorization", account.AuthorizationHeader())
	req.Header.Set("Accept", "application/json")
	req.Header.Set("OAI-Client-Id", "codex")
	req.Header.Set("OAI-Language", "en-US")
	req.Header.Set("User-Agent", codexCLIUserAgent)
	if account.AccountID != "" {
		req.Header.Set("ChatGPT-Account-ID", account.AccountID)
	}
}

// newUUIDv4 returns an RFC 4122 version 4 UUID string using crypto/rand so the
// redeem idempotency key does not pull in a third-party dependency.
func newUUIDv4() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand should never fail on macOS/Linux; fall back to a
		// time-derived value so the redeem still proceeds with a unique key.
		now := time.Now().UnixNano()
		for i := range b {
			b[i] = byte(now >> (i % 8 * 8))
		}
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
