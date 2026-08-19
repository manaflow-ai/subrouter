package selectacct

import (
	"sync"
	"testing"

	"github.com/manaflow-ai/subrouter/account"
)

// The live Codex pool shape as reported by the usage endpoint on
// cmux-lawrence: one account-wide 7-day window per account and nothing at or
// under 6 hours, so ShortHeadroom mirrors Headroom, ShortResetAfterSeconds is
// 0, and ExpiryPressure is 0 for every account. used_percent is integer-only
// and lags consumption, so consecutive placements see identical scores.
func codexPoolScheduler(headrooms map[string]float64) Scheduler {
	scores := make([]Score, 0, len(headrooms))
	for id, headroom := range headrooms {
		scores = append(scores, Score{
			AccountID:     id,
			Provider:      account.ProviderCodex,
			Headroom:      headroom,
			ShortHeadroom: headroom,
			Fresh:         true,
		})
	}
	return NewScheduler(scores)
}

func codexPoolAccounts(headrooms map[string]float64) []account.Account {
	accounts := make([]account.Account, 0, len(headrooms))
	for id := range headrooms {
		accounts = append(accounts, account.Account{
			ID:       id,
			Provider: account.ProviderCodex,
			AuthMode: account.AuthModeOAuth,
		})
	}
	return accounts
}

var livePoolHeadrooms = map[string]float64{
	"aziz+8@example.com":  0.96,
	"aziz+9@example.com":  0.85,
	"aziz+10@example.com": 0.88,
	"aziz+11@example.com": 0.88,
	"aziz+12@example.com": 0.81,
	"aziz+13@example.com": 0.80,
	"aziz+14@example.com": 0.87,
}

// Placements are rare (61 new Codex sessions in 19 hours on cmux-lawrence)
// while usage scores refresh every 30 seconds, so in production nearly every
// placement decision runs against a freshly refreshed snapshot: live debits
// from earlier placements have been wiped and the lagging integer
// used_percent has not moved. A deterministic argmax therefore sends every
// new session to the same account until that account is drained — on
// 2026-08-18 a single account served 100% of Codex traffic for five hours
// while six healthy accounts sat idle. Placement must spread across the
// usable pool instead.
func TestPickSpreadsPlacementsAcrossHealthyCodexPool(t *testing.T) {
	scheduler := codexPoolScheduler(livePoolHeadrooms)
	candidates := codexPoolAccounts(livePoolHeadrooms)

	const placements = 200
	counts := make(map[string]int)
	for i := 0; i < placements; i++ {
		picked, err := scheduler.Pick(candidates)
		if err != nil {
			t.Fatal(err)
		}
		counts[picked.ID]++
	}

	// With seven healthy accounts, the odds of a spreading policy putting 60%
	// of 200 placements on one account, or using at most three accounts, are
	// negligible. A deterministic argmax fails both by construction.
	top := 0
	for _, count := range counts {
		if count > top {
			top = count
		}
	}
	if len(counts) < 4 {
		t.Fatalf("placements used %d accounts (%v), want at least 4 of the 7 healthy accounts", len(counts), counts)
	}
	if top > placements*6/10 {
		t.Fatalf("one account received %d of %d placements (%v), want no account above 60%%", top, placements, counts)
	}
}

// The live debit (NoteRouted) is wiped by every successful usage refresh
// (FinishRefresh with update=true), and the refreshed snapshot lags actual
// consumption. This is the exact production loop: a placement, then a refresh
// that restores the same stale scores, then the next placement. The debit
// cannot protect placements that straddle refreshes, so Pick itself must
// spread.
func TestPickSpreadSurvivesRefreshWipedDebits(t *testing.T) {
	base := codexPoolScheduler(livePoolHeadrooms)
	candidates := codexPoolAccounts(livePoolHeadrooms)
	ref := NewSchedulerRef(base)

	const placements = 120
	counts := make(map[string]int)
	for i := 0; i < placements; i++ {
		scheduler := ref.Get().WithLiveDebits(ref.LiveDebits())
		picked, err := scheduler.Pick(candidates)
		if err != nil {
			t.Fatal(err)
		}
		counts[picked.ID]++
		ref.NoteRouted(picked.Provider, picked.ID)
		// The 30-second usage refresh lands between rare placements and
		// clears routedSinceRefresh; the lagging usage endpoint still reports
		// the same integer used_percent, so the snapshot is unchanged.
		ref.FinishRefresh(base, true)
	}

	top := 0
	for _, count := range counts {
		if count > top {
			top = count
		}
	}
	if len(counts) < 4 {
		t.Fatalf("placements used %d accounts (%v), want at least 4 of the 7 healthy accounts", len(counts), counts)
	}
	if top > placements*6/10 {
		t.Fatalf("one account received %d of %d placements (%v), want no account above 60%%", top, placements, counts)
	}
}

// A mass reroute (an account hitting its weekly cap moves every session on it
// at once) issues many concurrent Picks. They must spread and must not race.
func TestPickConcurrentPlacementsSpread(t *testing.T) {
	scheduler := codexPoolScheduler(livePoolHeadrooms)
	candidates := codexPoolAccounts(livePoolHeadrooms)

	const placements = 64
	var mu sync.Mutex
	counts := make(map[string]int)
	var wg sync.WaitGroup
	for i := 0; i < placements; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			picked, err := scheduler.Pick(candidates)
			if err != nil {
				t.Error(err)
				return
			}
			mu.Lock()
			counts[picked.ID]++
			mu.Unlock()
		}()
	}
	wg.Wait()

	if len(counts) < 3 {
		t.Fatalf("concurrent placements used %d accounts (%v), want at least 3", len(counts), counts)
	}
}

// Spreading must not leak outside the healthy pool: accounts under the
// new-session threshold and exhausted accounts stay unpickable while a
// usable account exists.
func TestPickSpreadNeverSelectsUnusableAccount(t *testing.T) {
	headrooms := map[string]float64{
		"healthy-a@example.com": 0.90,
		"healthy-b@example.com": 0.70,
		"low@example.com":       0.20,
		"empty@example.com":     0.00,
	}
	scheduler := codexPoolScheduler(headrooms)
	candidates := codexPoolAccounts(headrooms)

	for i := 0; i < 200; i++ {
		picked, err := scheduler.Pick(candidates)
		if err != nil {
			t.Fatal(err)
		}
		if picked.ID == "low@example.com" || picked.ID == "empty@example.com" {
			t.Fatalf("picked unusable account %q while healthy accounts exist", picked.ID)
		}
	}
}
