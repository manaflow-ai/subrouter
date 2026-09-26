package proxy

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
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
	stored := proxyStoredOAuthAccount("oauth-identity@example.com", "fresh", time.Now().Add(time.Hour))
	stored.Email = "stable-routing-id"
	if err := store.SaveStored(stored); err != nil {
		t.Fatal(err)
	}

	var consume int
	client := &http.Client{Transport: whamResponder(t, &consume, true)}
	handler := Server{AccountRef: NewAccountRef(store, nil, client), MaxBodyBytes: 1024}.Handler()

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/_subrouter/rate-limit-reset?email=stable-routing-id", nil))
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
	if res.Email != "stable-routing-id" {
		t.Fatalf("result account = %q, want stable routing ID", res.Email)
	}
	if !res.Reset || res.Credit == nil || res.Credit.Status != "redeemed" {
		t.Errorf("unexpected result: %+v", res)
	}
	if len(res.WindowsBefore) == 0 || len(res.WindowsAfter) == 0 {
		t.Errorf("expected before+after windows: %+v", res)
	}
}

func TestResetCreditsListsStableRoutingIdentifier(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	store := accounts.CodexStore{Dir: t.TempDir()}
	stored := proxyStoredOAuthAccount("oauth-identity@example.com", "fresh", time.Now().Add(time.Hour))
	stored.Email = "stable-routing-id"
	if err := store.SaveStored(stored); err != nil {
		t.Fatal(err)
	}
	var consume int
	client := &http.Client{Transport: whamResponder(t, &consume, true)}
	handler := Server{AccountRef: NewAccountRef(store, nil, client), MaxBodyBytes: 1024}.Handler()

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/_subrouter/reset-credits", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var payload struct {
		Accounts []ResetCreditsAccount `json:"accounts"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Accounts) != 1 || payload.Accounts[0].Email != "stable-routing-id" {
		t.Fatalf("reset credit accounts = %+v, want stable routing ID", payload.Accounts)
	}
}

// TestRateLimitResetEndpointRejectsMissingTarget asserts the handler requires
// an email or all=true rather than silently no-op'ing.
func TestRateLimitResetEndpointRejectsMissingTarget(t *testing.T) {
	handler := Server{AccountRef: NewAccountRef(accounts.CodexStore{Dir: t.TempDir()}, nil, nil), MaxBodyBytes: 1024}.Handler()
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/_subrouter/rate-limit-reset", nil))
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
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/_subrouter/rate-limit-reset?email=cooked@example.com&dry_run=true", nil))
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
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/_subrouter/rate-limit-reset?all=true", nil))
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

// TestRateLimitResetRedeemsWeeklyPrimaryWindow covers upstream reporting the
// weekly limit as primary_window with no secondary window and limit_reached
// false. The account is cooked and must be reset.
func TestRateLimitResetRedeemsWeeklyPrimaryWindow(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	store := accounts.CodexStore{Dir: t.TempDir()}
	stored := proxyStoredOAuthAccount("weekly-primary@example.com", "fresh", time.Now().Add(time.Hour))
	if err := store.SaveStored(stored); err != nil {
		t.Fatal(err)
	}

	var consume int
	client := &http.Client{Transport: proxyRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		header := make(http.Header)
		header.Set("Content-Type", "application/json")
		var body []byte
		switch req.URL.Path {
		case "/backend-api/wham/usage":
			used := 100
			if consume > 0 {
				used = 0
			}
			body, _ = json.Marshal(map[string]any{
				"plan_type": "pro",
				"rate_limit": map[string]any{
					"allowed":       true,
					"limit_reached": false,
					"primary_window": map[string]any{
						"used_percent":         used,
						"limit_window_seconds": 604800,
						"reset_after_seconds":  440286,
					},
				},
				"additional_rate_limits": []map[string]any{{
					"limit_name": "gpt-reserve",
					"rate_limit": map[string]any{
						"primary_window": map[string]any{"used_percent": 0, "limit_window_seconds": 604800, "reset_after_seconds": 604800},
					},
				}},
				"rate_limit_reset_credits": map[string]any{"available_count": 3 - consume},
			})
		case "/backend-api/wham/rate-limit-reset-credits":
			body = []byte(`{"credits":[{"id":"RateLimitResetCredit_test","reset_type":"codex_rate_limits","status":"available"}]}`)
		case "/backend-api/wham/rate-limit-reset-credits/consume":
			consume++
			body = []byte(`{"code":"reset","credit":{"id":"RateLimitResetCredit_test","status":"redeemed"}}`)
		default:
			return &http.Response{StatusCode: http.StatusNotFound, Header: header, Body: io.NopCloser(nil), Request: req}, nil
		}
		return &http.Response{StatusCode: http.StatusOK, Header: header, Body: io.NopCloser(bytes.NewReader(body)), Request: req}, nil
	})}
	handler := Server{AccountRef: NewAccountRef(store, nil, client), MaxBodyBytes: 1024}.Handler()

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/_subrouter/rate-limit-reset?email=weekly-primary@example.com", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if consume != 1 {
		t.Fatalf("expected 1 consume call, got %d; body = %s", consume, recorder.Body.String())
	}
}

// TestRateLimitResetBestPicksLongestWeeklyWait asserts best=true redeems one
// credit on the cooked account with the longest natural weekly wait, whether
// upstream reports the weekly window as primary or secondary, and ignores an
// account whose limit_reached comes only from its 5h window.
func TestRateLimitResetBestPicksLongestWeeklyWait(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	store := accounts.CodexStore{Dir: t.TempDir()}
	for _, email := range []string{"short@example.com", "long@example.com", "fivehour@example.com"} {
		if err := store.SaveStored(proxyStoredOAuthAccount(email, email, time.Now().Add(time.Hour))); err != nil {
			t.Fatal(err)
		}
	}
	usage := map[string]map[string]any{
		"short@example.com": {"limit_reached": false, "primary_window": map[string]any{
			"used_percent": 100, "limit_window_seconds": 604800, "reset_after_seconds": 30000}},
		"long@example.com": {"limit_reached": true, "secondary_window": map[string]any{
			"used_percent": 100, "limit_window_seconds": 604800, "reset_after_seconds": 400000}},
		"fivehour@example.com": {"limit_reached": true,
			"primary_window":   map[string]any{"used_percent": 100, "limit_window_seconds": 18000, "reset_after_seconds": 9000},
			"secondary_window": map[string]any{"used_percent": 40, "limit_window_seconds": 604800, "reset_after_seconds": 500000}},
	}
	var mu sync.Mutex
	consumed := map[string]int{}
	client := &http.Client{Transport: proxyRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		header := make(http.Header)
		header.Set("Content-Type", "application/json")
		email := emailFromTestJWT(req.Header.Get("Authorization"))
		var body []byte
		switch req.URL.Path {
		case "/backend-api/wham/usage":
			body, _ = json.Marshal(map[string]any{
				"plan_type":                "pro",
				"rate_limit":               usage[email],
				"rate_limit_reset_credits": map[string]any{"available_count": 3},
			})
		case "/backend-api/wham/rate-limit-reset-credits":
			body = []byte(`{"credits":[{"id":"x","status":"available"}]}`)
		case "/backend-api/wham/rate-limit-reset-credits/consume":
			mu.Lock()
			consumed[email]++
			mu.Unlock()
			body = []byte(`{"code":"reset","credit":{"id":"x","status":"redeemed"}}`)
		default:
			return &http.Response{StatusCode: http.StatusNotFound, Header: header, Body: io.NopCloser(nil), Request: req}, nil
		}
		return &http.Response{StatusCode: http.StatusOK, Header: header, Body: io.NopCloser(bytes.NewReader(body)), Request: req}, nil
	})}
	handler := Server{AccountRef: NewAccountRef(store, nil, client), MaxBodyBytes: 1024}.Handler()

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/_subrouter/rate-limit-reset?best=true", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if len(consumed) != 1 || consumed["long@example.com"] != 1 {
		t.Fatalf("consumed = %v, want exactly one credit on long@example.com; body = %s", consumed, recorder.Body.String())
	}
}

// TestRateLimitResetNotCookedSaysWhy asserts a refused reset names the
// windows it judged, so a disagreement with sr status is visible at once.
func TestRateLimitResetNotCookedSaysWhy(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	store := accounts.CodexStore{Dir: t.TempDir()}
	if err := store.SaveStored(proxyStoredOAuthAccount("healthy@example.com", "fresh", time.Now().Add(time.Hour))); err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: proxyRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		body, _ := json.Marshal(map[string]any{
			"plan_type": "pro",
			"rate_limit": map[string]any{
				"primary_window":   map[string]any{"used_percent": 100, "limit_window_seconds": 18000},
				"secondary_window": map[string]any{"used_percent": 40, "limit_window_seconds": 604800},
			},
		})
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(bytes.NewReader(body)), Request: req}, nil
	})}
	handler := Server{AccountRef: NewAccountRef(store, nil, client), MaxBodyBytes: 1024}.Handler()
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/_subrouter/rate-limit-reset?email=healthy@example.com&dry_run=1", nil))
	if !strings.Contains(recorder.Body.String(), "primary 5h 100%, secondary 7d 40%") {
		t.Fatalf("refusal does not name the windows: %s", recorder.Body.String())
	}
}

// TestRateLimitResetAllReportsUsageFetchFailures asserts the sweep reports an
// account it could not read instead of answering reset:0 as if all is well.
func TestRateLimitResetAllReportsUsageFetchFailures(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	store := accounts.CodexStore{Dir: t.TempDir()}
	if err := store.SaveStored(proxyStoredOAuthAccount("broken@example.com", "fresh", time.Now().Add(time.Hour))); err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: proxyRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusUnauthorized, Header: http.Header{}, Body: io.NopCloser(bytes.NewReader(nil)), Request: req}, nil
	})}
	handler := Server{AccountRef: NewAccountRef(store, nil, client), MaxBodyBytes: 1024}.Handler()
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/_subrouter/rate-limit-reset?all=true&dry_run=1", nil))
	var payload struct {
		Results []RateLimitResetResult `json:"results"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Results) != 1 || payload.Results[0].Email != "broken@example.com" || !strings.Contains(payload.Results[0].Error, "usage fetch failed") {
		t.Fatalf("results = %+v, want one usage-fetch failure for broken@example.com", payload.Results)
	}
}

func TestWithWeeklyCookedDoesNotMutateInput(t *testing.T) {
	in := []AccountUsageStatus{{Windows: []accounts.UsageWindow{{Name: "primary", UsedPercent: 100, LimitWindowSeconds: 604800}}}}
	out := withWeeklyCooked(in)
	if !out[0].WeeklyCooked || out[0].WeeklyCookedWindow != "primary" {
		t.Fatalf("out = %+v", out[0])
	}
	if in[0].WeeklyCooked {
		t.Fatal("withWeeklyCooked mutated its input")
	}
}

// TestRateLimitResetSweepHonorsMinWaitAndExpiry asserts min_wait_seconds
// drops accounts that recover soon on their own, and that a credit about to
// expire is spent ahead of an account with a longer wait.
func TestRateLimitResetSweepHonorsMinWaitAndExpiry(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	store := accounts.CodexStore{Dir: t.TempDir()}
	waits := map[string]int{"soon@example.com": 20000, "long@example.com": 400000, "expiring@example.com": 200000}
	for email := range waits {
		if err := store.SaveStored(proxyStoredOAuthAccount(email, email, time.Now().Add(time.Hour))); err != nil {
			t.Fatal(err)
		}
	}
	expiry := func(email string) string {
		d := 20 * 24 * time.Hour
		if email == "expiring@example.com" {
			d = 6 * time.Hour
		}
		return time.Now().Add(d).UTC().Format(time.RFC3339)
	}
	var mu sync.Mutex
	consumed := map[string]int{}
	client := &http.Client{Transport: proxyRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		email := emailFromTestJWT(req.Header.Get("Authorization"))
		var body []byte
		switch req.URL.Path {
		case "/backend-api/wham/usage":
			body, _ = json.Marshal(map[string]any{
				"plan_type": "pro",
				"rate_limit": map[string]any{"primary_window": map[string]any{
					"used_percent": 100, "limit_window_seconds": 604800, "reset_after_seconds": waits[email]}},
				"rate_limit_reset_credits": map[string]any{"available_count": 3},
			})
		case "/backend-api/wham/rate-limit-reset-credits":
			body, _ = json.Marshal(map[string]any{"credits": []map[string]any{{"id": "c-" + email, "status": "available", "expires_at": expiry(email)}}})
		case "/backend-api/wham/rate-limit-reset-credits/consume":
			mu.Lock()
			consumed[email]++
			mu.Unlock()
			body = []byte(`{"code":"reset","credit":{"id":"x","status":"redeemed"}}`)
		default:
			return &http.Response{StatusCode: http.StatusNotFound, Header: http.Header{}, Body: io.NopCloser(nil), Request: req}, nil
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(bytes.NewReader(body)), Request: req}, nil
	})}
	handler := Server{AccountRef: NewAccountRef(store, nil, client), MaxBodyBytes: 1024}.Handler()

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/_subrouter/rate-limit-reset?all=true&dry_run=1&min_wait_seconds=86400", nil))
	var payload struct {
		Results []RateLimitResetResult `json:"results"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	var order []string
	for _, res := range payload.Results {
		order = append(order, res.Email)
	}
	if strings.Join(order, ",") != "expiring@example.com,long@example.com" {
		t.Fatalf("dry-run order = %v, want expiring then long, soon filtered out", order)
	}
	if payload.Results[1].WeeklyWaitSeconds != 400000 || payload.Results[0].CreditExpiresAt == "" {
		t.Fatalf("missing wait/expiry detail: %+v", payload.Results)
	}

	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/_subrouter/rate-limit-reset?best=true", nil))
	if len(consumed) != 1 || consumed["expiring@example.com"] != 1 {
		t.Fatalf("best consumed %v, want the expiring credit", consumed)
	}

	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/_subrouter/rate-limit-reset?all=true&min_wait_seconds=-1", nil))
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("negative min_wait status = %d, want 400", recorder.Code)
	}
}

func TestSortResetCandidatesExpiringCreditNeedsARealWait(t *testing.T) {
	now := time.Now()
	candidates := []rateLimitResetCandidate{
		{account: accounts.Account{ID: "short-expiring"}, wait: 600, creditExpires: now.Add(time.Hour)},
		{account: accounts.Account{ID: "long"}, wait: 500000, creditExpires: now.Add(20 * 24 * time.Hour)},
		{account: accounts.Account{ID: "mid-expiring"}, wait: 100000, creditExpires: now.Add(time.Hour)},
	}
	sortResetCandidates(candidates, now)
	var order []string
	for _, c := range candidates {
		order = append(order, c.account.ID)
	}
	if got := strings.Join(order, ","); got != "mid-expiring,long,short-expiring" {
		t.Fatalf("order = %s", got)
	}
}
