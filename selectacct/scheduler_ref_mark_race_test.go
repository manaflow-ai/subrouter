package selectacct

import (
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/account"
)

func codexScore(id string, headroom float64) Score {
	return Score{AccountID: id, Provider: account.ProviderCodex, Headroom: headroom, ShortHeadroom: headroom, Fresh: true}
}

func beginRefresh(t *testing.T, ref *SchedulerRef) {
	t.Helper()
	ref.SetUpdatedAt(time.Time{})
	if !ref.BeginRefreshIfStale(time.Minute) {
		t.Fatal("refresh did not begin")
	}
}

// A 429 that lands while a refresh is fetching usage is newer than every
// score in that refresh. Publishing the refresh must not read the account's
// pre-429 headroom as recovery and route straight back into the rejection.
func TestRefreshKeepsExhaustionMarkSetWhileItRan(t *testing.T) {
	ref := NewSchedulerRef(NewScheduler([]Score{codexScore("a", 0.5)}))
	beginRefresh(t, ref)
	ref.MarkExhaustedUntil(account.ProviderCodex, "a", "", time.Now().Add(5*time.Hour))
	if !finishBegunRefresh(t, ref, NewScheduler([]Score{codexScore("a", 0.3)}), true) {
		t.Fatal("refresh was not published")
	}
	if _, ok := ref.ExhaustedUntilFor(account.ProviderCodex, "a", ""); !ok {
		t.Fatal("the mark set during the refresh was dropped")
	}
	if !ref.Get().Exhausted(account.ProviderCodex, "a") {
		t.Fatal("routing reads the account as usable right after upstream rejected it")
	}
}

// A mark from before the refresh began is still superseded by the fresh
// headroom the refresh measured: the account recovered.
func TestRefreshStillClearsMarkItMeasuredPast(t *testing.T) {
	ref := NewSchedulerRef(NewScheduler([]Score{codexScore("a", 0.5)}))
	ref.MarkExhaustedUntil(account.ProviderCodex, "a", "", time.Now().Add(5*time.Hour))
	publishRefresh(t, ref, NewScheduler([]Score{codexScore("a", 0.6)}), true)
	if _, ok := ref.ExhaustedUntilFor(account.ProviderCodex, "a", ""); ok {
		t.Fatal("fresh headroom measured after the mark should clear it")
	}
}

// The partial-refresh path has the same race: scores computed at a
// revision read before the 429 must not clear it.
func TestMergeKeepsExhaustionMarkNewerThanItsScores(t *testing.T) {
	ref := NewSchedulerRef(NewScheduler([]Score{codexScore("a", 0.5)}))
	revision := ref.ScoreRevision()
	ref.MarkExhaustedUntil(account.ProviderCodex, "a", "", time.Now().Add(5*time.Hour))
	if _, ok := ref.MergeScoresForAccountGenerationAtScoreRevision([]Score{codexScore("a", 0.3)}, 0, revision); !ok {
		t.Fatal("merge was not published")
	}
	if !ref.Get().Exhausted(account.ProviderCodex, "a") {
		t.Fatal("merge cleared a mark set after its scores were measured")
	}
}

// Fresh evidence of weekly headroom overtakes a weekly-cooked mark even
// while the session window keeps the account exhausted; otherwise a
// session-only cook reads as weekly-cooked and authorizes paid fallback.
func TestFreshWeeklyHeadroomClearsWeeklyMark(t *testing.T) {
	ref := NewSchedulerRef(NewScheduler([]Score{codexScore("a", 0.5)}))
	ref.MarkWeeklyExhaustedUntil(account.ProviderCodex, "a", "", time.Now().Add(6*24*time.Hour))
	sessionCooked := Score{AccountID: "a", Provider: account.ProviderCodex, Headroom: 0, ShortHeadroom: 0,
		WeeklyHeadroom: 0.9, WeeklyHeadroomKnown: true, Fresh: true}
	publishRefresh(t, ref, NewScheduler([]Score{sessionCooked}), true)
	if ref.Get().ScoreFor(account.ProviderCodex, "a").WeeklyCooked() {
		t.Fatal("stale weekly mark overrides a fresh 90% weekly reading")
	}
	if !ref.Get().Exhausted(account.ProviderCodex, "a") {
		t.Fatal("the session window is still cooked; the account must stay exhausted")
	}
}

// A later session-level 429 must not shorten a proven weekly block, and a
// weekly mark whose account left the pool must not linger as a zero score.
func TestWeeklyMarkStaysPairedWithItsExhaustionMark(t *testing.T) {
	ref := NewSchedulerRef(NewScheduler([]Score{codexScore("a", 0.5), codexScore("ghost", 0.5)}))
	weekly := time.Now().Add(6 * 24 * time.Hour)
	ref.MarkWeeklyExhaustedUntil(account.ProviderCodex, "a", "", weekly)
	ref.MarkExhaustedUntil(account.ProviderCodex, "a", "", time.Now().Add(10*time.Minute))
	if until, ok := ref.ExhaustedUntilFor(account.ProviderCodex, "a", ""); !ok || until.Before(weekly) {
		t.Fatalf("session 429 shortened the weekly block to %v", until)
	}

	ref.MarkWeeklyExhaustedUntil(account.ProviderCodex, "ghost", "", weekly)
	publishRefresh(t, ref, NewScheduler([]Score{codexScore("a", 0)}), true)
	if ref.Get().Exhausted(account.ProviderCodex, "ghost") {
		t.Fatal("a weekly mark for an account no longer scored still zeroes it")
	}
}
