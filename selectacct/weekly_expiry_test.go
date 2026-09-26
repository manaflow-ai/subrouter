package selectacct

import (
	"math"
	"math/rand"
	"testing"

	"github.com/manaflow-ai/subrouter/account"
)

const testDay = 24 * 3600

func codexWeeklyScore(id string, usedPercent float64, resetAfterSeconds int64) Score {
	score := ScoreFromLimitWindows(id, 0, []LimitWindow{{
		Name: "primary", UsedPercent: usedPercent,
		LimitWindowSeconds: 7 * testDay, ResetAfterSeconds: resetAfterSeconds,
	}})
	score.Provider = account.ProviderCodex
	return score
}

func withSeededSpread(t *testing.T, seed int64) {
	t.Helper()
	saved := spreadRandFloat
	spreadRandFloat = rand.New(rand.NewSource(seed)).Float64
	t.Cleanup(func() { spreadRandFloat = saved })
}

func pickCounts(t *testing.T, scheduler Scheduler, candidates []account.Account, picks int) map[string]int {
	t.Helper()
	counts := make(map[string]int)
	for i := 0; i < picks; i++ {
		picked, err := scheduler.Pick(candidates)
		if err != nil {
			t.Fatal(err)
		}
		counts[picked.ID]++
	}
	return counts
}

func TestScoreFromLimitWindowsWeeklySurplus(t *testing.T) {
	soon := codexWeeklyScore("soon", 40, testDay)
	if want := 0.60 - 1.0/7; math.Abs(soon.WeeklySurplus-want) > 1e-9 {
		t.Fatalf("surplus with 60%% left and one day to reset = %.4f, want %.4f", soon.WeeklySurplus, want)
	}
	if late := codexWeeklyScore("late", 40, 6*testDay); late.WeeklySurplus != 0 {
		t.Fatalf("surplus with 60%% left and six days to reset = %.4f, want 0 (on pace)", late.WeeklySurplus)
	}
	if unknown := codexWeeklyScore("unknown", 40, 0); unknown.WeeklySurplus != 0 {
		t.Fatalf("surplus without a reset time = %.4f, want 0", unknown.WeeklySurplus)
	}

	// A short window under the new-session floor holds the account back on
	// its own, so weekly surplus must not count.
	shortSpent := ScoreFromLimitWindows("claude", 0, []LimitWindow{
		{Name: "five_hour", UsedPercent: 70, LimitWindowSeconds: 5 * 3600, ResetAfterSeconds: 3600},
		{Name: "seven_day", UsedPercent: 70, LimitWindowSeconds: 7 * testDay, ResetAfterSeconds: 6 * 3600},
	})
	if shortSpent.WeeklySurplus != 0 {
		t.Fatalf("surplus with a 30%% short window = %.4f, want 0", shortSpent.WeeklySurplus)
	}
	shortHealthy := ScoreFromLimitWindows("claude", 0, []LimitWindow{
		{Name: "five_hour", UsedPercent: 10, LimitWindowSeconds: 5 * 3600, ResetAfterSeconds: 3600},
		{Name: "seven_day", UsedPercent: 70, LimitWindowSeconds: 7 * testDay, ResetAfterSeconds: 6 * 3600},
	})
	if shortHealthy.WeeklySurplus < 0.25 {
		t.Fatalf("surplus with a healthy short window and 30%% weekly left six hours from reset = %.4f, want ~0.26", shortHealthy.WeeklySurplus)
	}
}

// Codex accounts report only a weekly window, so their sort pressure is 0 and
// they share one spread band. The account whose weekly window resets tomorrow
// with plenty left must take a larger share of new sessions than equally roomy
// accounts six days from reset, while every account still gets placements.
func TestPickLeansTowardCodexAccountWithExpiringWeeklyQuota(t *testing.T) {
	withSeededSpread(t, 1)
	scheduler := NewScheduler([]Score{
		codexWeeklyScore("expiring@example.com", 40, testDay),
		codexWeeklyScore("a@example.com", 40, 6*testDay),
		codexWeeklyScore("b@example.com", 40, 6*testDay),
		codexWeeklyScore("c@example.com", 40, 6*testDay),
	})
	candidates := codexPoolAccounts(map[string]float64{
		"expiring@example.com": 0, "a@example.com": 0, "b@example.com": 0, "c@example.com": 0,
	})

	counts := pickCounts(t, scheduler, candidates, 800)
	others := float64(counts["a@example.com"]+counts["b@example.com"]+counts["c@example.com"]) / 3
	if float64(counts["expiring@example.com"]) < 2*others {
		t.Fatalf("expiring account got %d placements vs %.0f per on-pace account (%v), want at least twice as many", counts["expiring@example.com"], others, counts)
	}
	for _, id := range []string{"a@example.com", "b@example.com", "c@example.com"} {
		if counts[id] < 40 {
			t.Fatalf("on-pace account %s got %d of 800 placements (%v): the band collapsed onto one account", id, counts[id], counts)
		}
	}
}

// Equally situated expiring accounts must still share placements evenly: the
// weekly surplus weights the spread, it does not order the pool.
func TestPickSpreadsAcrossEquallyExpiringAccounts(t *testing.T) {
	withSeededSpread(t, 2)
	ids := []string{"a@example.com", "b@example.com", "c@example.com", "d@example.com"}
	scores := make([]Score, 0, len(ids))
	headrooms := map[string]float64{}
	for _, id := range ids {
		scores = append(scores, codexWeeklyScore(id, 40, testDay))
		headrooms[id] = 0
	}
	counts := pickCounts(t, NewScheduler(scores), codexPoolAccounts(headrooms), 800)
	for _, id := range ids {
		if counts[id] < 120 || counts[id] > 280 {
			t.Fatalf("account %s got %d of 800 placements (%v), want roughly a quarter each", id, counts[id], counts)
		}
	}
}

// Below the new-session floor, weekly quota that would go unused before reset
// still takes new sessions ahead of an API key; an account that is merely low
// (on pace to use what it has left) does not.
func TestPickUsesExpiringSubFloorQuotaBeforeAPIKey(t *testing.T) {
	withSeededSpread(t, 3)
	apiKey := account.Account{ID: "api-key", Provider: account.ProviderCodex, AuthMode: account.AuthModeAPIKey}
	sub := account.Account{ID: "sub@example.com", Provider: account.ProviderCodex, AuthMode: account.AuthModeOAuth}
	candidates := []account.Account{apiKey, sub}

	expiring := NewScheduler([]Score{codexWeeklyScore(sub.ID, 70, 12*3600)})
	if picked, err := expiring.Pick(candidates); err != nil || picked.ID != sub.ID {
		t.Fatalf("30%% left twelve hours from reset: picked %q (err %v), want the subscription account", picked.ID, err)
	}
	onPace := NewScheduler([]Score{codexWeeklyScore(sub.ID, 70, 5*testDay)})
	if picked, err := onPace.Pick(candidates); err != nil || picked.ID != apiKey.ID {
		t.Fatalf("30%% left five days from reset: picked %q (err %v), want the API key", picked.ID, err)
	}
	nearlyEmpty := NewScheduler([]Score{codexWeeklyScore(sub.ID, 90, 3600)})
	if picked, err := nearlyEmpty.Pick(candidates); err != nil || picked.ID != apiKey.ID {
		t.Fatalf("10%% left one hour from reset: picked %q (err %v), want the API key (below weeklySurplusMinHeadroom)", picked.ID, err)
	}
}

// Admission below the floor reads the live-debited score, so the proxy's own
// in-flight placements use up the surplus between refreshes.
func TestExpiringAdmissionHonoursLiveDebits(t *testing.T) {
	scheduler := NewScheduler([]Score{codexWeeklyScore("sub@example.com", 70, 12*3600)})
	if !scheduler.UsableForNewSession(account.ProviderCodex, "sub@example.com") {
		t.Fatal("expiring sub-floor account should take new sessions before any debit")
	}
	debited := scheduler.WithLiveDebits(map[string]int{ScoreKey(account.ProviderCodex, "sub@example.com"): 10})
	if debited.UsableForNewSession(account.ProviderCodex, "sub@example.com") {
		t.Fatal("ten routed requests (0.20 debit) should use up the expiring surplus")
	}
	if !debited.UsableForStickySession(account.ProviderCodex, "sub@example.com") {
		t.Fatal("sticky retention must not change: measured headroom is still 30%")
	}
}

// A reduced run of TestWeeklyExpiryPolicyExperiment, kept fast enough for
// every test run: the shipped policy must waste much less weekly quota than
// the pre-change policy without adding failovers or concentrating placements.
func TestWeeklyExpirySimBeatsBaseline(t *testing.T) {
	const seeds = 2
	baseline := weeklySimPolicy{name: "baseline", admit: math.Inf(1), transform: weeklyBaseline}
	shipped := weeklySimPolicy{name: "shipped", admit: weeklySurplusMinAdmit, minHeadroom: weeklySurplusMinHeadroom, gain: weeklySurplusSpreadGain}
	for _, demand := range []float64{0.85, 1.0} {
		before := runWeeklySimSeeds(baseline, demand, 27, seeds)
		after := runWeeklySimSeeds(shipped, demand, 27, seeds)
		t.Logf("demand %.2f: wasted %.1f -> %.1f, failovers %d -> %d, apiKey %d -> %d, herd6h %.2f -> %.2f",
			demand, before.wasted, after.wasted, before.failovers, after.failovers, before.apiKey, after.apiKey, before.herdShare, after.herdShare)
		if after.wasted > 0.7*before.wasted {
			t.Fatalf("demand %.2f: wasted %.2f, want at most 70%% of baseline %.2f", demand, after.wasted, before.wasted)
		}
		if after.failovers > before.failovers {
			t.Fatalf("demand %.2f: failovers %d, baseline %d", demand, after.failovers, before.failovers)
		}
		if after.herdShare > before.herdShare {
			t.Fatalf("demand %.2f: herd share %.2f above baseline %.2f", demand, after.herdShare, before.herdShare)
		}
		if after.apiKey >= before.apiKey {
			t.Fatalf("demand %.2f: API-key placements %d, want fewer than baseline %d", demand, after.apiKey, before.apiKey)
		}
	}
}

// A model pool carries the account-wide weekly window and its own. Surplus
// on one does not admit the pool while the other is under the floor and on
// pace.
func TestWeeklySurplusRequiresEveryFloorWeeklyWindowAheadOfPace(t *testing.T) {
	score := ScoreFromLimitWindows("claude", 0, []LimitWindow{
		{Name: "seven_day", UsedPercent: 70, LimitWindowSeconds: 7 * testDay, ResetAfterSeconds: 12 * 3600},
		{Name: "seven_day_fable", UsedPercent: 65, LimitWindowSeconds: 7 * testDay, ResetAfterSeconds: 5 * testDay},
	})
	if score.WeeklySurplus != 0 || score.UsableForNewSession() {
		t.Fatalf("surplus %.3f usable=%v: the on-pace 35%% window must block admission", score.WeeklySurplus, score.UsableForNewSession())
	}
}

// Live debits can push the short window under the floor after it was
// measured; the surplus must not keep admitting the account then.
func TestLiveDebitsUnderShortFloorCancelWeeklySurplus(t *testing.T) {
	score := ScoreFromLimitWindows("claude", 0, []LimitWindow{
		{Name: "five_hour", UsedPercent: 55, LimitWindowSeconds: 5 * 3600, ResetAfterSeconds: 2 * 3600},
		{Name: "seven_day", UsedPercent: 50, LimitWindowSeconds: 7 * testDay, ResetAfterSeconds: 3 * 3600},
	})
	score.Provider = account.ProviderClaude
	scheduler := NewScheduler([]Score{score})
	if !scheduler.UsableForNewSession(account.ProviderClaude, "claude") {
		t.Fatal("45% short, 50% weekly: usable before any debit")
	}
	debited := scheduler.WithLiveDebits(map[string]int{ScoreKey(account.ProviderClaude, "claude"): 10})
	if debited.UsableForNewSession(account.ProviderClaude, "claude") {
		t.Fatal("debited short window at 25% must not be admitted through weekly surplus")
	}
}
