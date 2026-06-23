package accounts

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// useResetTestServer points the reset-credit package vars at srv for the
// duration of the test and returns a restore func. Every test that exercises
// the live reset endpoints calls this so the production chatgpt.com URL is
// never hit.
func useResetTestServer(t *testing.T, srv *httptest.Server) {
	t.Helper()
	orig := codexRateLimitResetCreditsURL
	codexRateLimitResetCreditsURL = srv.URL + "/wham/rate-limit-reset-credits"
	t.Cleanup(func() { codexRateLimitResetCreditsURL = orig })
}

func oauthAccountForTest() Account {
	return Account{
		Provider:  ProviderCodex,
		AuthMode:  AuthModeOAuth,
		Email:     "user@example.com",
		Token:     "tok-123",
		AccountID: "acct-1",
	}
}

func TestListRateLimitResetCredits(t *testing.T) {
	var gotAuth, gotUA string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotUA = r.Header.Get("User-Agent")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"credits":[
			{"id":"RateLimitResetCredit_abc","reset_type":"codex_rate_limits","status":"available"},
			{"id":"RateLimitResetCredit_xyz","reset_type":"codex_rate_limits","status":"redeemed"}
		]}`))
	}))
	defer srv.Close()
	useResetTestServer(t, srv)

	credits, err := ListRateLimitResetCredits(context.Background(), srv.Client(), oauthAccountForTest())
	if err != nil {
		t.Fatalf("ListRateLimitResetCredits: %v", err)
	}
	if gotAuth != "Bearer tok-123" {
		t.Errorf("Authorization = %q, want Bearer tok-123", gotAuth)
	}
	if gotUA != codexCLIUserAgent {
		t.Errorf("User-Agent = %q, want %q", gotUA, codexCLIUserAgent)
	}
	if len(credits) != 2 {
		t.Fatalf("expected 2 credits, got %d", len(credits))
	}
	if credits[0].ID != "RateLimitResetCredit_abc" || credits[0].Status != "available" {
		t.Errorf("credit[0] = %+v", credits[0])
	}
}

func TestConsumeRateLimitResetCredit(t *testing.T) {
	var gotMethod, gotContentType string
	var gotBody map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotContentType = r.Header.Get("Content-Type")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":"reset","credit":{"id":"RateLimitResetCredit_abc","status":"redeemed"}}`))
	}))
	defer srv.Close()
	useResetTestServer(t, srv)

	credit, err := ConsumeRateLimitResetCredit(context.Background(), srv.Client(),
		oauthAccountForTest(), "RateLimitResetCredit_abc", "req-7")
	if err != nil {
		t.Fatalf("Consume: %v", err)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method = %q, want POST", gotMethod)
	}
	if gotContentType != "application/json" {
		t.Errorf("Content-Type = %q", gotContentType)
	}
	if gotBody["credit_id"] != "RateLimitResetCredit_abc" {
		t.Errorf("credit_id = %q", gotBody["credit_id"])
	}
	if gotBody["redeem_request_id"] != "req-7" {
		t.Errorf("redeem_request_id = %q", gotBody["redeem_request_id"])
	}
	if credit.Status != "redeemed" {
		t.Errorf("credit status = %q, want redeemed", credit.Status)
	}
}

func TestFirstAvailableRateLimitResetCredit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"credits":[
			{"id":"used-up","status":"redeemed"},
			{"id":"good-one","status":"available"}
		]}`))
	}))
	defer srv.Close()
	useResetTestServer(t, srv)

	credit, ok, err := FirstAvailableRateLimitResetCredit(context.Background(), srv.Client(), oauthAccountForTest())
	if err != nil {
		t.Fatalf("FirstAvailable: %v", err)
	}
	if !ok {
		t.Fatal("expected an available credit")
	}
	if credit.ID != "good-one" {
		t.Errorf("got %q, want good-one", credit.ID)
	}
}

func TestFirstAvailableRateLimitResetCreditNone(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"credits":[{"id":"x","status":"redeemed"}]}`))
	}))
	defer srv.Close()
	useResetTestServer(t, srv)
	_, ok, err := FirstAvailableRateLimitResetCredit(context.Background(), srv.Client(), oauthAccountForTest())
	if err != nil || ok {
		t.Fatalf("expected no available credit, got ok=%v err=%v", ok, err)
	}
}

func TestRedeemRateLimitResetListsThenConsumes(t *testing.T) {
	var consumed bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/consume"):
			consumed = true
			_, _ = w.Write([]byte(`{"code":"reset","credit":{"id":"RateLimitResetCredit_abc","status":"redeemed"}}`))
		default:
			_, _ = w.Write([]byte(`{"credits":[{"id":"RateLimitResetCredit_abc","status":"available"}]}`))
		}
	}))
	defer srv.Close()
	useResetTestServer(t, srv)

	credit, err := RedeemRateLimitReset(context.Background(), srv.Client(), oauthAccountForTest())
	if err != nil {
		t.Fatalf("Redeem: %v", err)
	}
	if !consumed {
		t.Error("consume endpoint was never called")
	}
	if credit.Status != "redeemed" {
		t.Errorf("status = %q", credit.Status)
	}
}

func TestRedeemRateLimitResetNoCredits(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"credits":[{"id":"x","status":"redeemed"}]}`))
	}))
	defer srv.Close()
	useResetTestServer(t, srv)
	_, err := RedeemRateLimitReset(context.Background(), srv.Client(), oauthAccountForTest())
	if err == nil {
		t.Fatal("expected error when no credits available")
	}
	if !strings.Contains(err.Error(), "no available") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestRateLimitResetRejectsAPIKeyAndEmptyToken(t *testing.T) {
	if _, err := ListRateLimitResetCredits(context.Background(), nil, Account{AuthMode: AuthModeAPIKey}); err == nil {
		t.Error("expected error for API-key account")
	}
	if _, err := ListRateLimitResetCredits(context.Background(), nil, Account{AuthMode: AuthModeOAuth, Token: ""}); err == nil {
		t.Error("expected error for empty token")
	}
}

func TestNewUUIDv4Shape(t *testing.T) {
	u := newUUIDv4()
	// 8-4-4-4-12 hex with the v4 and variant bits set.
	if len(u) != 36 {
		t.Fatalf("uuid len = %d (%q)", len(u), u)
	}
	if u[14] != '4' {
		t.Errorf("expected version 4 at index 14, got %q", u[14])
	}
	if u[19] != '8' && u[19] != '9' && u[19] != 'a' && u[19] != 'b' {
		t.Errorf("expected variant at index 19, got %q", u[19])
	}
	if newUUIDv4() == newUUIDv4() {
		t.Error("expected unique uuids")
	}
}
