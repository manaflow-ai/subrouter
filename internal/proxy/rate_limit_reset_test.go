package proxy

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
)

// emailFromTestJWT extracts the embedded email from a test "alg:none" JWT so a
// fake transport can serve different accounts from the same bearer token set.
func emailFromTestJWT(bearer string) string {
	token := strings.TrimPrefix(bearer, "Bearer ")
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var claims struct {
		Profile struct {
			Email string `json:"email"`
		} `json:"https://api.openai.com/profile"`
	}
	_ = json.Unmarshal(payload, &claims)
	return claims.Profile.Email
}

// whamResponder returns a fake RoundTrip that serves the wham usage, credits,
// and consume endpoints based on request path. consumeCounter is incremented
// when the consume endpoint is hit, and usage responses adapt post-consume.
func whamResponder(t *testing.T, consumeCounter *int, cookedBeforeReset bool) proxyRoundTripFunc {
	t.Helper()
	return proxyRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		header := make(http.Header)
		header.Set("Content-Type", "application/json")
		var body []byte
		switch req.URL.Path {
		case "/backend-api/wham/usage":
			consumed := *consumeCounter > 0
			used := 100
			limitReached := cookedBeforeReset
			resetAfter := 250000
			if consumed {
				used = 0
				limitReached = false
				resetAfter = 604800
			}
			body, _ = json.Marshal(map[string]any{
				"plan_type": "pro",
				"rate_limit": map[string]any{
					"limit_reached": limitReached,
					"secondary_window": map[string]any{
						"used_percent":         used,
						"limit_window_seconds": 604800,
						"reset_after_seconds":  resetAfter,
					},
				},
				"rate_limit_reset_credits": map[string]any{"available_count": 2 - *consumeCounter},
			})
		case "/backend-api/wham/rate-limit-reset-credits":
			body = []byte(`{"credits":[{"id":"RateLimitResetCredit_test","reset_type":"codex_rate_limits","status":"available"}]}`)
		case "/backend-api/wham/rate-limit-reset-credits/consume":
			if req.Method != http.MethodPost {
				return &http.Response{StatusCode: http.StatusMethodNotAllowed, Header: header, Body: io.NopCloser(nil), Request: req}, nil
			}
			*consumeCounter++
			body = []byte(`{"code":"reset","credit":{"id":"RateLimitResetCredit_test","status":"redeemed"}}`)
		default:
			return &http.Response{StatusCode: http.StatusNotFound, Header: header, Body: io.NopCloser(nil), Request: req}, nil
		}
		return &http.Response{StatusCode: http.StatusOK, Header: header, Body: io.NopCloser(bytes.NewReader(body)), Request: req}, nil
	})
}

// TestRateLimitResetEndpointRedeemsCookedAccountWithCredit asserts the email
// path fetches usage, sees a cooked account with a credit, redeems it, and
// reports before/after windows.
func TestRateLimitResetEndpointRedeemsCookedAccountWithCredit(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	store := accounts.CodexStore{Dir: t.TempDir()}
	stored := proxyStoredOAuthAccount("cooked@example.com", "fresh", time.Now().Add(time.Hour))
	if err := store.SaveStored(stored); err != nil {
		t.Fatal(err)
	}

	var consume int
	client := &http.Client{Transport: whamResponder(t, &consume, true)}
	handler := Server{AccountRef: NewAccountRef(store, nil, client), MaxBodyBytes: 1024}.Handler()

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, loopbackAdminRequest(http.MethodPost, "/_subrouter/rate-limit-reset?email=cooked@example.com", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if consume != 1 {
		t.Fatalf("expected exactly 1 consume call, got %d", consume)
	}
	var payload struct {
		Reset   int                    `json:"reset"`
		Results []RateLimitResetResult `json:"results"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Reset != 1 || len(payload.Results) != 1 {
		t.Fatalf("unexpected payload: %+v", payload)
	}
	res := payload.Results[0]
	if !res.Reset || res.Credit == nil || res.Credit.Status != "redeemed" {
		t.Errorf("unexpected result: %+v", res)
	}
	if len(res.WindowsBefore) == 0 || len(res.WindowsAfter) == 0 {
		t.Errorf("expected before+after windows: %+v", res)
	}
}

// TestRateLimitResetEndpointRejectsMissingTarget asserts the handler requires
// an email or all=true rather than silently no-op'ing.
func TestRateLimitResetEndpointRejectsMissingTarget(t *testing.T) {
	handler := Server{AccountRef: NewAccountRef(accounts.CodexStore{Dir: t.TempDir()}, nil, nil), MaxBodyBytes: 1024}.Handler()
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, loopbackAdminRequest(http.MethodPost, "/_subrouter/rate-limit-reset", nil))
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", recorder.Code)
	}
}

// TestRateLimitResetEndpointDryRunDoesNotConsume asserts dry_run lists
// eligibility without hitting the consume endpoint.
func TestRateLimitResetEndpointDryRunDoesNotConsume(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	store := accounts.CodexStore{Dir: t.TempDir()}
	stored := proxyStoredOAuthAccount("cooked@example.com", "fresh", time.Now().Add(time.Hour))
	if err := store.SaveStored(stored); err != nil {
		t.Fatal(err)
	}
	var consume int
	client := &http.Client{Transport: whamResponder(t, &consume, true)}
	handler := Server{AccountRef: NewAccountRef(store, nil, client), MaxBodyBytes: 1024}.Handler()

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, loopbackAdminRequest(http.MethodPost, "/_subrouter/rate-limit-reset?email=cooked@example.com&dry_run=true", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if consume != 0 {
		t.Fatalf("dry_run must not consume, got %d", consume)
	}
	var payload struct {
		DryRun  bool                   `json:"dry_run"`
		Reset   int                    `json:"reset"`
		Results []RateLimitResetResult `json:"results"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if !payload.DryRun || payload.Reset != 0 || len(payload.Results) != 1 || !payload.Results[0].Eligible {
		t.Fatalf("unexpected dry-run payload: %+v", payload)
	}
}

// TestRateLimitResetEndpointAllSkipsHealthy asserts the all=true sweep only
// redeems accounts that are cooked on the 7d window with a credit available.
func TestRateLimitResetEndpointAllSkipsHealthy(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	store := accounts.CodexStore{Dir: t.TempDir()}
	if err := store.SaveStored(proxyStoredOAuthAccount("cooked@example.com", "cooked", time.Now().Add(time.Hour))); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveStored(proxyStoredOAuthAccount("healthy@example.com", "healthy", time.Now().Add(time.Hour))); err != nil {
		t.Fatal(err)
	}

	var consume int
	client := &http.Client{Transport: proxyRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		header := make(http.Header)
		header.Set("Content-Type", "application/json")
		// healthy token -> low usage + limit not reached; cooked token -> cooked.
		isCooked := emailFromTestJWT(req.Header.Get("Authorization")) == "cooked@example.com"
		switch req.URL.Path {
		case "/backend-api/wham/usage":
			used := 5
			limitReached := false
			if isCooked {
				used = 100
				limitReached = true
			}
			body, _ := json.Marshal(map[string]any{
				"plan_type": "pro",
				"rate_limit": map[string]any{
					"limit_reached": limitReached,
					"secondary_window": map[string]any{
						"used_percent": used, "limit_window_seconds": 604800, "reset_after_seconds": 250000,
					},
				},
				"rate_limit_reset_credits": map[string]any{"available_count": 2},
			})
			return &http.Response{StatusCode: http.StatusOK, Header: header, Body: io.NopCloser(bytes.NewReader(body)), Request: req}, nil
		case "/backend-api/wham/rate-limit-reset-credits":
			return &http.Response{StatusCode: http.StatusOK, Header: header, Body: io.NopCloser(bytes.NewReader([]byte(`{"credits":[{"id":"x","status":"available"}]}`))), Request: req}, nil
		case "/backend-api/wham/rate-limit-reset-credits/consume":
			consume++
			return &http.Response{StatusCode: http.StatusOK, Header: header, Body: io.NopCloser(bytes.NewReader([]byte(`{"code":"reset","credit":{"id":"x","status":"redeemed"}}`))), Request: req}, nil
		default:
			return &http.Response{StatusCode: http.StatusNotFound, Header: header, Body: io.NopCloser(nil), Request: req}, nil
		}
	})}
	handler := Server{AccountRef: NewAccountRef(store, nil, client), MaxBodyBytes: 1024}.Handler()

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, loopbackAdminRequest(http.MethodPost, "/_subrouter/rate-limit-reset?all=true", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if consume != 1 {
		t.Fatalf("--all should consume exactly 1 credit (cooked only), got %d", consume)
	}
	var payload struct {
		Reset   int                    `json:"reset"`
		Results []RateLimitResetResult `json:"results"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Reset != 1 || len(payload.Results) != 1 || payload.Results[0].Email != "cooked@example.com" {
		t.Fatalf("--all should only reset the cooked account, got %+v", payload)
	}
}
