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

// Scored Claude accounts carry distinct ExpiryPressure values. Placement
// must spread across every account with room for a new session (the
// 2026-10-13 herd: eleven accounts had quota while sessions piled onto one),
// while still leaning toward the account whose 5h window resets soonest.
func TestPickSpreadsScoredClaudePoolLeaningToDrainPressure(t *testing.T) {
	scheduler := NewScheduler([]Score{
		{AccountID: "soon@example.com", Provider: account.ProviderClaude, Headroom: 0.90, ShortHeadroom: 0.90, ShortResetAfterSeconds: 1 * 3600, ExpiryPressure: 0.90 / float64(1*3600)},
		{AccountID: "later@example.com", Provider: account.ProviderClaude, Headroom: 0.90, ShortHeadroom: 0.90, ShortResetAfterSeconds: 5 * 3600, ExpiryPressure: 0.90 / float64(5*3600)},
		{AccountID: "fresh@example.com", Provider: account.ProviderClaude, Headroom: 0.90, ShortHeadroom: 0.90},
		{AccountID: "low@example.com", Provider: account.ProviderClaude, Headroom: 0.20, ShortHeadroom: 0.20, ShortResetAfterSeconds: 1 * 3600, ExpiryPressure: 0.20 / float64(1*3600)},
	})
	candidates := claudeAccounts("soon@example.com", "later@example.com", "fresh@example.com", "low@example.com")

	counts := pickCounts(t, scheduler, candidates, 2000)
	for _, id := range []string{"soon@example.com", "later@example.com", "fresh@example.com"} {
		if counts[id] < 150 {
			t.Fatalf("%s got %d of 2000 placements (%v); every account with room must share new sessions", id, counts[id], counts)
		}
	}
	if counts["soon@example.com"] <= counts["later@example.com"] || counts["later@example.com"] <= counts["fresh@example.com"] {
		t.Fatalf("placements %v, want the soonest-resetting window to lead, then later, then no window", counts)
	}
	if counts["low@example.com"] != 0 {
		t.Fatalf("an account below the new-session floor got %d placements while roomy accounts remained: %v", counts["low@example.com"], counts)
	}
}

// Among accounts with the same reset timing, the fuller account takes more
// new sessions, so the pool converges toward even headroom.
func TestPickSendsMoreNewSessionsToFullerClaudeAccounts(t *testing.T) {
	scheduler := NewScheduler([]Score{
		{AccountID: "full@example.com", Provider: account.ProviderClaude, Headroom: 1.0, ShortHeadroom: 1.0},
		{AccountID: "half@example.com", Provider: account.ProviderClaude, Headroom: 0.55, ShortHeadroom: 0.55},
	})
	counts := pickCounts(t, scheduler, claudeAccounts("full@example.com", "half@example.com"), 2000)
	if counts["full@example.com"] < 3*counts["half@example.com"] || counts["half@example.com"] == 0 {
		t.Fatalf("placements %v, want the full account to take most new sessions without starving the other", counts)
	}
}
