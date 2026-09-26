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

func TestExpiringCreditWorthSpending(t *testing.T) {
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	day := int64(24 * 3600)
	for _, tc := range []struct {
		name    string
		wait    int64
		expires time.Duration
		want    bool
	}{
		{"cooked five days, credit expires in a week", 5*day + 3600, 7 * 24 * time.Hour, true},
		{"credit expires before the natural reset", 3 * day, 2 * 24 * time.Hour, true},
		{"credit outlives reset plus re-cook grace", 6 * day, 20 * 24 * time.Hour, false},
		{"natural reset imminent", 3600, time.Hour, false},
		{"no expiry reported", 6 * day, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := rateLimitResetCandidate{wait: tc.wait}
			if tc.expires > 0 {
				c.creditExpires = now.Add(tc.expires)
			}
			if got := expiringCreditWorthSpending(c, now); got != tc.want {
				t.Fatalf("expiringCreditWorthSpending(wait=%ds, expires in %s) = %v, want %v", tc.wait, tc.expires, got, tc.want)
			}
		})
	}
}

// The sweep spends only credits that would lapse: a cooked account whose
// credit expires before it could cook again. Cooked accounts with a
// long-lived credit, accounts that recover within hours, and healthy
// accounts keep theirs.
func TestSpendExpiringResetCreditsSpendsOnlyLapsingCredits(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	store := accounts.CodexStore{Dir: t.TempDir()}
	day := 24 * 3600
	type fixture struct {
		used    int
		wait    int
		expires time.Duration
	}
	fixtures := map[string]fixture{
		"lapsing@example.com":    {used: 100, wait: 5*day + 3600, expires: 6 * 24 * time.Hour},
		"long-lived@example.com": {used: 100, wait: 6 * day, expires: 25 * 24 * time.Hour},
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

	results := server.spendExpiringResetCredits(t.Context(), time.Now())
	if len(consumed) != 1 || consumed["lapsing@example.com"] != 1 {
		t.Fatalf("consumed %v, want exactly one credit on lapsing@example.com", consumed)
	}
	if len(results) != 1 || !results[0].Reset || results[0].Email != "lapsing@example.com" {
		t.Fatalf("results = %+v", results)
	}

	// A second sweep finds the account recovered and spends nothing more.
	server.spendExpiringResetCredits(t.Context(), time.Now())
	if consumed["lapsing@example.com"] != 1 || len(consumed) != 1 {
		t.Fatalf("second sweep consumed %v, want no further spend", consumed)
	}
}
