package proxy

import (
	"bytes"
	"encoding/json"
	"io"
	"maps"
	"net/http"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/selectacct"
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

// resetFixture is one account behind fakeResetUpstream: its weekly usage,
// the wait W until its natural reset, and when its one credit expires.
type resetFixture struct {
	used    int
	wait    int
	expires time.Duration
}

// fakeResetUpstream serves wham usage, credit listing and consume for the
// fixtures, keyed by the account email in the bearer JWT. An account whose
// credit was consumed reads as a fresh window afterwards. It returns the
// server and the per-account consume counts.
func fakeResetUpstream(t *testing.T, fixtures map[string]resetFixture) (Server, func() map[string]int) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	store := accounts.CodexStore{Dir: t.TempDir()}
	day := 24 * 3600
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
	return server, func() map[string]int {
		mu.Lock()
		defer mu.Unlock()
		return maps.Clone(consumed)
	}
}

// W/7 fixtures: two accounts where no later moment beats now, and three
// that keep their credits.
func w7Fixtures() map[string]resetFixture {
	day := 24 * 3600
	return map[string]resetFixture{
		"lapsing@example.com":    {used: 100, wait: 2 * day, expires: 6 * 24 * time.Hour},
		"early-cook@example.com": {used: 100, wait: 6 * day, expires: 25 * 24 * time.Hour},
		"mid-window@example.com": {used: 100, wait: 2 * day, expires: 25 * 24 * time.Hour},
		"recovering@example.com": {used: 100, wait: 2 * 3600, expires: 12 * time.Hour},
		"healthy@example.com":    {used: 30, wait: 5 * day, expires: 12 * time.Hour},
	}
}

// The sweep spends where no later moment beats now: an account that cooked
// early in its window, and one whose credit lapses before its next cook.
// A mid-window cook with a long-lived credit, an account recovering within
// hours, and a healthy account keep theirs. (One of five accounts serving
// counts as blocked, but nobody is asking, so the blocked rule stays out.)
func TestSpendResetCreditsSpendsOnlyWhenNowIsBest(t *testing.T) {
	server, consumed := fakeResetUpstream(t, w7Fixtures())

	results := server.spendResetCredits(t.Context(), time.Now(), ResetCreditAutospendSpend)
	want := map[string]int{"lapsing@example.com": 1, "early-cook@example.com": 1}
	if got := consumed(); !maps.Equal(got, want) {
		t.Fatalf("consumed %v, want %v", got, want)
	}
	if len(results) != 2 || !results[0].Reset || !results[1].Reset {
		t.Fatalf("results = %+v", results)
	}
	reasons := map[string]string{results[0].Email: results[0].Reason, results[1].Email: results[1].Reason}
	if reasons["lapsing@example.com"] != resetReasonExpiring || reasons["early-cook@example.com"] != resetReasonEarlyCook {
		t.Fatalf("reasons = %v", reasons)
	}

	// A second sweep finds both accounts recovered and spends nothing more.
	server.spendResetCredits(t.Context(), time.Now(), ResetCreditAutospendSpend)
	if got := consumed(); !maps.Equal(got, want) {
		t.Fatalf("second sweep consumed %v, want no further spend", got)
	}
}

// Warn mode makes the same decisions, redeems nothing, and publishes them as
// reset_advice on the usage-status rows.
func TestSpendResetCreditsWarnModeNeverRedeems(t *testing.T) {
	server, consumed := fakeResetUpstream(t, w7Fixtures())

	for range 2 {
		results := server.spendResetCredits(t.Context(), time.Now(), ResetCreditAutospendWarn)
		if got := consumed(); len(got) != 0 {
			t.Fatalf("warn mode consumed %v", got)
		}
		if len(results) != 2 {
			t.Fatalf("results = %+v", results)
		}
		for _, res := range results {
			if res.Reset || !res.DryRun || !res.Eligible || res.Reason == "" || res.CreditExpiresAt == "" {
				t.Fatalf("warn result = %+v", res)
			}
		}
	}
	statuses := server.AccountRef.withResetAdvice([]AccountUsageStatus{
		{AccountStatus: AccountStatus{ID: "early-cook@example.com", Provider: accounts.ProviderCodex, AuthMode: accounts.AuthModeOAuth}},
		{AccountStatus: AccountStatus{ID: "mid-window@example.com", Provider: accounts.ProviderCodex, AuthMode: accounts.AuthModeOAuth}},
	})
	if advice := statuses[0].ResetAdvice; advice == nil || advice.Reason != resetReasonEarlyCook || advice.WSeconds != 6*24*3600 {
		t.Fatalf("early-cook advice = %+v", advice)
	}
	if statuses[1].ResetAdvice != nil {
		t.Fatalf("mid-window advice = %+v, want none", statuses[1].ResetAdvice)
	}

	// Off evaluates nothing.
	if results := server.spendResetCredits(t.Context(), time.Now(), ResetCreditAutospendOff); results != nil || len(consumed()) != 0 {
		t.Fatalf("off mode results = %+v, consumed %v", results, consumed())
	}
}

// A blocked pool with demand spends on the largest waits first, stops at
// the cap, and never touches an account about to recover on its own. The
// same pool with nobody asking keeps every credit.
func TestSpendResetCreditsPoolBlocked(t *testing.T) {
	hour := 3600
	fixtures := map[string]resetFixture{
		"serving@example.com": {used: 40, wait: 3 * 24 * hour, expires: 25 * 24 * time.Hour},
		"w20h@example.com":    {used: 100, wait: 20 * hour, expires: 25 * 24 * time.Hour},
		"w10h@example.com":    {used: 100, wait: 10 * hour, expires: 25 * 24 * time.Hour},
		"w5h@example.com":     {used: 100, wait: 5 * hour, expires: 25 * 24 * time.Hour},
		"w3h@example.com":     {used: 100, wait: 3 * hour, expires: 25 * 24 * time.Hour},
		"w1h@example.com":     {used: 100, wait: hour, expires: 25 * 24 * time.Hour},
	}

	t.Run("no demand holds", func(t *testing.T) {
		server, consumed := fakeResetUpstream(t, fixtures)
		server.SchedulerRef = selectacct.NewSchedulerRef(selectacct.NewScheduler(nil))
		server.spendResetCredits(t.Context(), time.Now(), ResetCreditAutospendSpend)
		if got := consumed(); len(got) != 0 {
			t.Fatalf("consumed %v with no demand", got)
		}
	})

	t.Run("demand spends largest W first, capped", func(t *testing.T) {
		server, consumed := fakeResetUpstream(t, fixtures)
		server.SchedulerRef = selectacct.NewSchedulerRef(selectacct.NewScheduler(nil))
		server.SchedulerRef.NoteUnserved(accounts.ProviderCodex)

		results := server.spendResetCredits(t.Context(), time.Now(), ResetCreditAutospendSpend)
		// Six accounts: threshold 1, one serving, so two more restore
		// threshold + resetCreditBlockedHeadroom.
		want := map[string]int{"w20h@example.com": 1, "w10h@example.com": 1}
		if got := consumed(); !maps.Equal(got, want) {
			t.Fatalf("consumed %v, want %v", got, want)
		}
		for _, res := range results {
			if res.Reason != resetReasonPoolBlocked || !res.Reset {
				t.Fatalf("result = %+v", res)
			}
		}

		// Three accounts serve now: no longer blocked, nothing more.
		server.spendResetCredits(t.Context(), time.Now(), ResetCreditAutospendSpend)
		if got := consumed(); !maps.Equal(got, want) {
			t.Fatalf("second sweep consumed %v, want %v", got, want)
		}
	})
}

func TestPlanResetCreditSpends(t *testing.T) {
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	hour := int64(3600)
	farExpiry := now.Add(25 * 24 * time.Hour)
	cand := func(id string, wait int64, expires time.Time) rateLimitResetCandidate {
		return rateLimitResetCandidate{account: accounts.Account{ID: id}, wait: wait, creditExpires: expires}
	}
	// The real shape: most of a 90-account pool cooked with W under a day.
	cooked := []rateLimitResetCandidate{
		cand("w1h", hour, farExpiry),
		cand("w20h", 20*hour, farExpiry),
		cand("w3h", 3*hour, farExpiry),
		cand("w12h", 12*hour, farExpiry),
		cand("w8h", 8*hour, farExpiry),
		cand("w15h", 15*hour, farExpiry),
		cand("w6h", 6*hour, farExpiry),
		cand("w4h", 4*hour, farExpiry),
	}
	for _, tc := range []struct {
		name       string
		candidates []rateLimitResetCandidate
		pool       resetPoolState
		want       []string
	}{
		{"blocked with demand: largest W first, capped at threshold+2",
			cooked, resetPoolState{total: 90, serving: 1, demand: true},
			[]string{"w20h", "w15h", "w12h", "w8h", "w6h"}}, // threshold 4: 4+2-1 = 5
		{"blocked with no demand: hold",
			cooked, resetPoolState{total: 90, serving: 1}, nil},
		{"not blocked: W/7 rule only",
			append([]rateLimitResetCandidate{cand("early", 5*24*hour, farExpiry)}, cooked...),
			resetPoolState{total: 90, serving: 30, demand: true}, []string{"early"}},
		{"blocked, every W below the floor: hold",
			[]rateLimitResetCandidate{cand("a", hour, farExpiry), cand("b", 90*60, farExpiry)},
			resetPoolState{total: 90, serving: 0, demand: true}, nil},
		{"blocked: W/7 spends count toward the cap",
			append([]rateLimitResetCandidate{cand("early", 5*24*hour, farExpiry), cand("lapsing", 10*hour, now.Add(3*24*time.Hour))}, cooked...),
			resetPoolState{total: 90, serving: 2, demand: true},
			[]string{"early", "lapsing", "w20h", "w15h"}}, // 4+2-2 = 4, two of them W/7
		{"small pool: threshold is at least one",
			[]rateLimitResetCandidate{cand("a", 5*hour, farExpiry), cand("b", 9*hour, farExpiry)},
			resetPoolState{total: 3, serving: 1, demand: true}, []string{"b", "a"}},
		{"blocked at exactly the threshold still counts",
			cooked, resetPoolState{total: 90, serving: 4, demand: true}, []string{"w20h", "w15h"}},
		{"one above the threshold is not blocked",
			cooked, resetPoolState{total: 90, serving: 5, demand: true}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got []string
			for _, d := range planResetCreditSpends(tc.candidates, tc.pool, now) {
				got = append(got, d.candidate.account.ID)
			}
			if !slices.Equal(got, tc.want) {
				t.Fatalf("plan = %v, want %v", got, tc.want)
			}
		})
	}
}

// The between-sweeps check needs both recent demand and a scheduler view of
// the pool at or under the threshold.
func TestCodexPoolLooksBlocked(t *testing.T) {
	now := time.Now()
	pool := []accounts.Account{
		{ID: "a", Provider: accounts.ProviderCodex, AuthMode: accounts.AuthModeOAuth},
		{ID: "b", Provider: accounts.ProviderCodex, AuthMode: accounts.AuthModeOAuth},
		{ID: "c", Provider: accounts.ProviderCodex, AuthMode: accounts.AuthModeOAuth},
	}
	score := func(id string, headroom float64) selectacct.Score {
		return selectacct.Score{AccountID: id, Provider: accounts.ProviderCodex, Headroom: headroom, ShortHeadroom: headroom, WeeklyHeadroom: headroom}
	}
	for _, tc := range []struct {
		name     string
		scores   []selectacct.Score
		demand   bool
		expected bool
	}{
		{"two cooked, demand", []selectacct.Score{score("a", 0), score("b", 0), score("c", 0.5)}, true, true},
		{"two cooked, no demand", []selectacct.Score{score("a", 0), score("b", 0), score("c", 0.5)}, false, false},
		{"one cooked, demand", []selectacct.Score{score("a", 0), score("b", 0.5), score("c", 0.5)}, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			server := Server{
				AccountRef:   NewAccountRef(accounts.CodexStore{Dir: t.TempDir()}, pool, nil),
				SchedulerRef: selectacct.NewSchedulerRef(selectacct.NewScheduler(tc.scores)),
			}
			if tc.demand {
				server.SchedulerRef.NoteRouted(accounts.ProviderCodex, "c")
			}
			if got := server.codexPoolLooksBlocked(now); got != tc.expected {
				t.Fatalf("codexPoolLooksBlocked = %v, want %v", got, tc.expected)
			}
		})
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
