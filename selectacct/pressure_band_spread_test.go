package selectacct

import (
	"testing"

	"github.com/manaflow-ai/subrouter/account"
)

func claudeAccounts(ids ...string) []account.Account {
	accounts := make([]account.Account, 0, len(ids))
	for _, id := range ids {
		accounts = append(accounts, account.Account{ID: id, Provider: account.ProviderClaude, AuthMode: account.AuthModeOAuth})
	}
	return accounts
}

// A Claude pool with no usage data yet (every account carries the optimistic
// default score, ExpiryPressure 0) has no drain-pressure signal to order by,
// exactly like the Codex weekly-window pools that produced the 2026-08-18
// placement stampede. Placement must spread across such a pool instead of
// herding onto the sort's first account.
func TestPickSpreadsUnscoredClaudePool(t *testing.T) {
	scheduler := NewScheduler(nil)
	candidates := claudeAccounts("a@example.com", "b@example.com", "c@example.com", "d@example.com", "e@example.com", "f@example.com")

	counts := make(map[string]int)
	for i := 0; i < 200; i++ {
		picked, err := scheduler.Pick(candidates)
		if err != nil {
			t.Fatal(err)
		}
		counts[picked.ID]++
	}
	top := 0
	for _, count := range counts {
		if count > top {
			top = count
		}
	}
	if len(counts) < 4 {
		t.Fatalf("placements used %d accounts (%v), want at least 4 of 6", len(counts), counts)
	}
	if top > 120 {
		t.Fatalf("one account received %d of 200 placements (%v), want no account above 60%%", top, counts)
	}
}

// Scored Claude accounts carry distinct ExpiryPressure values, and the
// drain-soonest ordering is deliberate (GTO: prefer the account whose window
// resets soonest). Distinct pressures form singleton bands, so the ordering
// stays exactly deterministic.
func TestPickKeepsClaudeDrainOrderingForScoredAccounts(t *testing.T) {
	scheduler := NewScheduler([]Score{
		{AccountID: "soon@example.com", Provider: account.ProviderClaude, Headroom: 0.90, ShortHeadroom: 0.90, ShortResetAfterSeconds: 3 * 3600, ExpiryPressure: 0.90 / float64(3*3600)},
		{AccountID: "later@example.com", Provider: account.ProviderClaude, Headroom: 0.95, ShortHeadroom: 0.95, ShortResetAfterSeconds: 5 * 3600, ExpiryPressure: 0.95 / float64(5*3600)},
		{AccountID: "unscored@example.com", Provider: account.ProviderClaude, Headroom: 1, ShortHeadroom: 1},
	})
	candidates := claudeAccounts("soon@example.com", "later@example.com", "unscored@example.com")

	for i := 0; i < 100; i++ {
		picked, err := scheduler.Pick(candidates)
		if err != nil {
			t.Fatal(err)
		}
		if picked.ID != "soon@example.com" {
			t.Fatalf("pick %d = %q, want the highest-pressure account soon@example.com every time", i, picked.ID)
		}
	}
}

// Spreading happens only WITHIN the leading equal-pressure band: accounts
// tied at the top pressure share the placements, accounts with less pressure
// get none while the band is usable.
func TestPickSpreadsOnlyWithinEqualPressureBand(t *testing.T) {
	pressure := 0.80 / float64(4*3600)
	scheduler := NewScheduler([]Score{
		{AccountID: "band-a@example.com", Provider: account.ProviderClaude, Headroom: 0.80, ShortHeadroom: 0.80, ShortResetAfterSeconds: 4 * 3600, ExpiryPressure: pressure},
		{AccountID: "band-b@example.com", Provider: account.ProviderClaude, Headroom: 0.80, ShortHeadroom: 0.80, ShortResetAfterSeconds: 4 * 3600, ExpiryPressure: pressure},
		{AccountID: "calm-c@example.com", Provider: account.ProviderClaude, Headroom: 0.99, ShortHeadroom: 0.99},
		{AccountID: "calm-d@example.com", Provider: account.ProviderClaude, Headroom: 0.99, ShortHeadroom: 0.99},
	})
	candidates := claudeAccounts("band-a@example.com", "band-b@example.com", "calm-c@example.com", "calm-d@example.com")

	counts := make(map[string]int)
	for i := 0; i < 200; i++ {
		picked, err := scheduler.Pick(candidates)
		if err != nil {
			t.Fatal(err)
		}
		counts[picked.ID]++
	}
	if counts["calm-c@example.com"] != 0 || counts["calm-d@example.com"] != 0 {
		t.Fatalf("placements leaked outside the top-pressure band: %v", counts)
	}
	if counts["band-a@example.com"] == 0 || counts["band-b@example.com"] == 0 {
		t.Fatalf("placements did not spread within the equal-pressure band: %v", counts)
	}
}
