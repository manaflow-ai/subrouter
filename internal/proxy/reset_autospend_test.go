package proxy

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
)

func TestCreditWorthSpending(t *testing.T) {
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	day := int64(24 * 3600)
	for _, tc := range []struct {
		name    string
		wait    int64
		expires time.Duration
		want    bool
	}{
		{"cooked five days, credit expires in a week", 5*day + 3600, 7 * 24 * time.Hour, true},
		{"cooked early in the window, credit far from expiry", 6 * day, 25 * 24 * time.Hour, true},
		{"small wait, credit expires before the next cook", day, 3 * 24 * time.Hour, true},
		{"small wait, credit outlives the next cook", day, 20 * 24 * time.Hour, false},
		{"three days, no expiry reported", 3 * day, 0, false},
		{"natural reset imminent, credit expiring", 3600, time.Hour, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := rateLimitResetCandidate{wait: tc.wait}
			if tc.expires > 0 {
				c.creditExpires = now.Add(tc.expires)
			}
			if got := creditWorthSpending(c, now); got != tc.want {
				t.Fatalf("creditWorthSpending(wait=%ds, expires in %s) = %v, want %v", tc.wait, tc.expires, got, tc.want)
			}
		})
	}
}

// The sweep spends where no later moment beats now: an account that cooked
// early in its window, and one whose credit lapses before its next cook.
// A mid-window cook with a long-lived credit, an account recovering within
// hours, and a healthy account keep theirs.
func TestSpendResetCreditsSpendsOnlyWhenNowIsBest(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	store := accounts.CodexStore{Dir: t.TempDir()}
	day := 24 * 3600
	type fixture struct {
		used    int
		wait    int
		expires time.Duration
	}
	fixtures := map[string]fixture{
		"lapsing@example.com":    {used: 100, wait: 2 * day, expires: 6 * 24 * time.Hour},
		"early-cook@example.com": {used: 100, wait: 6 * day, expires: 25 * 24 * time.Hour},
		"mid-window@example.com": {used: 100, wait: 2 * day, expires: 25 * 24 * time.Hour},
		"recovering@example.com": {used: 100, wait: 2 * 3600, expires: 12 * time.Hour},
		"healthy@example.com":    {used: 30, wait: 5 * day, expires: 12 * time.Hour},
	}
	for email := range fixtures {
		if err := store.SaveStored(proxyStoredOAuthAccount(email, email, time.Now().Add(time.Hour))); err != nil {
			t.Fatal(err)
		}
	}
	var mu sync.Mutex
	consumed := map[string]int{}
	client := &http.Client{Transport: proxyRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		email := emailFromTestJWT(req.Header.Get("Authorization"))
		f := fixtures[email]
		mu.Lock()
		spent := consumed[email] > 0
		mu.Unlock()
		var body []byte
		switch req.URL.Path {
		case "/backend-api/wham/usage":
			used, wait := f.used, f.wait
			if spent {
				used, wait = 0, 7*day
			}
			body, _ = json.Marshal(map[string]any{
				"plan_type": "pro",
				"rate_limit": map[string]any{"primary_window": map[string]any{
					"used_percent": used, "limit_window_seconds": 7 * day, "reset_after_seconds": wait}},
				"rate_limit_reset_credits": map[string]any{"available_count": 1},
			})
		case "/backend-api/wham/rate-limit-reset-credits":
			body, _ = json.Marshal(map[string]any{"credits": []map[string]any{{
				"id": "c-" + email, "status": "available", "expires_at": time.Now().Add(f.expires).UTC().Format(time.RFC3339)}}})
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
	server := Server{AccountRef: NewAccountRef(store, nil, client), MaxBodyBytes: 1024}

	results := server.spendResetCredits(t.Context(), time.Now())
	want := map[string]int{"lapsing@example.com": 1, "early-cook@example.com": 1}
	if len(consumed) != len(want) || consumed["lapsing@example.com"] != 1 || consumed["early-cook@example.com"] != 1 {
		t.Fatalf("consumed %v, want %v", consumed, want)
	}
	if len(results) != 2 || !results[0].Reset || !results[1].Reset {
		t.Fatalf("results = %+v", results)
	}

	// A second sweep finds both accounts recovered and spends nothing more.
	server.spendResetCredits(t.Context(), time.Now())
	if len(consumed) != 2 || consumed["lapsing@example.com"] != 1 || consumed["early-cook@example.com"] != 1 {
		t.Fatalf("second sweep consumed %v, want no further spend", consumed)
	}
}

// Two redeems racing on one cooked account (the background spender and a
// manual sr reset) must spend one credit, not one each.
func TestConcurrentRedeemsSpendOneCredit(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	store := accounts.CodexStore{Dir: t.TempDir()}
	const email = "racing@example.com"
	stored := proxyStoredOAuthAccount(email, email, time.Now().Add(time.Hour))
	if err := store.SaveStored(stored); err != nil {
		t.Fatal(err)
	}
	account, ok := stored.Account(stored.SourcePath(store))
	if !ok {
		t.Fatal("stored account has no routing account")
	}
	var mu sync.Mutex
	consumed := 0
	client := &http.Client{Transport: proxyRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		mu.Lock()
		spent := consumed > 0
		mu.Unlock()
		var body []byte
		switch req.URL.Path {
		case "/backend-api/wham/usage":
			used, wait := 100, 5*24*3600
			if spent {
				used, wait = 0, 7*24*3600
			}
			body, _ = json.Marshal(map[string]any{
				"plan_type": "pro",
				"rate_limit": map[string]any{"primary_window": map[string]any{
					"used_percent": used, "limit_window_seconds": 7 * 24 * 3600, "reset_after_seconds": wait}},
				"rate_limit_reset_credits": map[string]any{"available_count": 2},
			})
		case "/backend-api/wham/rate-limit-reset-credits":
			body = []byte(`{"credits":[{"id":"c1","status":"available"},{"id":"c2","status":"available"}]}`)
		case "/backend-api/wham/rate-limit-reset-credits/consume":
			time.Sleep(20 * time.Millisecond)
			mu.Lock()
			consumed++
			mu.Unlock()
			body = []byte(`{"code":"reset","credit":{"id":"x","status":"redeemed"}}`)
		default:
			return &http.Response{StatusCode: http.StatusNotFound, Header: http.Header{}, Body: io.NopCloser(nil), Request: req}, nil
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(bytes.NewReader(body)), Request: req}, nil
	})}
	server := Server{AccountRef: NewAccountRef(store, nil, client), MaxBodyBytes: 1024}

	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			server.redeemAccountIfEligible(t.Context(), account, false)
		}()
	}
	wg.Wait()
	if consumed != 1 {
		t.Fatalf("consumed %d credits for one reset, want 1", consumed)
	}
}
