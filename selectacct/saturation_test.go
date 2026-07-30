package selectacct

import (
	"math"
	"testing"

	"github.com/manaflow-ai/subrouter/account"
)

type simulatedAccount struct {
	id       string
	used5h   float64
	used7d   float64
	sessions int
}

func TestScoreFromLimitWindowsUsesMostConstrainedWindow(t *testing.T) {
	score := ScoreFromLimitWindows("a", 3, []LimitWindow{
		{Name: "5h", UsedPercent: 20},
		{Name: "7d", UsedPercent: 80},
	})

	if math.Abs(score.Headroom-0.20) > 0.0001 {
		t.Fatalf("headroom = %.2f, want 0.20", score.Headroom)
	}
	if score.Sessions != 3 {
		t.Fatalf("sessions = %d, want 3", score.Sessions)
	}
}

func TestScoreFromLimitWindowsComputesExpiryPressure(t *testing.T) {
	score := ScoreFromLimitWindows("a", 0, []LimitWindow{
		{Name: "5h", UsedPercent: 10, LimitWindowSeconds: 5 * 60 * 60, ResetAfterSeconds: 3 * 60 * 60},
		{Name: "7d", UsedPercent: 20, LimitWindowSeconds: 7 * 24 * 60 * 60, ResetAfterSeconds: 6 * 24 * 60 * 60},
	})

	if math.Abs(score.Headroom-0.80) > 0.0001 {
		t.Fatalf("headroom = %.2f, want 0.80", score.Headroom)
	}
	if math.Abs(score.ShortHeadroom-0.90) > 0.0001 {
		t.Fatalf("short headroom = %.2f, want 0.90", score.ShortHeadroom)
	}
	wantPressure := 0.80 / float64(3*60*60)
	if math.Abs(score.ExpiryPressure-wantPressure) > 0.000001 {
		t.Fatalf("expiry pressure = %.8f, want %.8f", score.ExpiryPressure, wantPressure)
	}
}

func TestSchedulerForSparkModelUsesSparkWindows(t *testing.T) {
	// spark-healthy has a cooked account-wide quota but an open Spark pool;
	// spark-cooked is the reverse. A Spark request must pick spark-healthy.
	scheduler := NewScheduler([]Score{
		ScoreFromLimitWindows("spark-healthy", 0, []LimitWindow{
			{Name: "primary", UsedPercent: 100, LimitWindowSeconds: 5 * 60 * 60},
			{Name: "secondary", UsedPercent: 100, LimitWindowSeconds: 7 * 24 * 60 * 60},
			{Name: "GPT-5.3-Codex-Spark/primary", Feature: "GPT-5.3-Codex-Spark", UsedPercent: 1, LimitWindowSeconds: 5 * 60 * 60},
			{Name: "GPT-5.3-Codex-Spark/secondary", Feature: "GPT-5.3-Codex-Spark", UsedPercent: 2, LimitWindowSeconds: 7 * 24 * 60 * 60},
		}),
		ScoreFromLimitWindows("spark-cooked", 0, []LimitWindow{
			{Name: "primary", UsedPercent: 0, LimitWindowSeconds: 5 * 60 * 60},
			{Name: "secondary", UsedPercent: 0, LimitWindowSeconds: 7 * 24 * 60 * 60},
			{Name: "GPT-5.3-Codex-Spark/primary", Feature: "GPT-5.3-Codex-Spark", UsedPercent: 100, LimitWindowSeconds: 5 * 60 * 60},
			{Name: "GPT-5.3-Codex-Spark/secondary", Feature: "GPT-5.3-Codex-Spark", UsedPercent: 100, LimitWindowSeconds: 7 * 24 * 60 * 60},
		}),
	}).ForModel("gpt-5.3-codex-spark")

	got, err := scheduler.Pick([]account.Account{
		{ID: "spark-cooked", AuthMode: account.AuthModeOAuth},
		{ID: "spark-healthy", AuthMode: account.AuthModeOAuth},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "spark-healthy" {
		t.Fatalf("got %q, want spark-healthy", got.ID)
	}
	if !scheduler.UsableForNewSession("", "spark-healthy") {
		t.Fatal("spark-healthy should be usable for a Spark request")
	}
	if !scheduler.Exhausted("", "spark-cooked") {
		t.Fatal("spark-cooked should be exhausted for a Spark request")
	}
}

func TestRegularModelDoesNotMatchSparkPool(t *testing.T) {
	// "gpt-5.3-codex" normalizes to "gpt53codex", a prefix of the Spark feature
	// key "gpt53codexspark". Matching is strict equality, so a regular request
	// must use base quota, never the (here cooked) Spark pool. Any loosening to
	// substring matching would regress this.
	scheduler := NewScheduler([]Score{
		ScoreFromLimitWindows("acct", 0, []LimitWindow{
			{Name: "primary", UsedPercent: 10, LimitWindowSeconds: 5 * 60 * 60},
			{Name: "secondary", UsedPercent: 20, LimitWindowSeconds: 7 * 24 * 60 * 60},
			{Name: "GPT-5.3-Codex-Spark/primary", Feature: "GPT-5.3-Codex-Spark", UsedPercent: 100, LimitWindowSeconds: 5 * 60 * 60},
		}),
	}).ForModel("gpt-5.3-codex")

	if scheduler.Exhausted("", "acct") {
		t.Fatal("regular model must use base quota, not the cooked Spark pool")
	}
	if math.Abs(scheduler.score("", "acct").Headroom-0.80) > 0.0001 {
		t.Fatalf("regular headroom = %.2f, want 0.80 (base)", scheduler.score("", "acct").Headroom)
	}
}

func TestSchedulerGeneralizesToAnyMeteredFeature(t *testing.T) {
	// A metered model the code has never heard of must route to its own pool,
	// keyed purely from the upstream limit name. No per-model special case.
	scheduler := NewScheduler([]Score{
		ScoreFromLimitWindows("turbo-open", 0, []LimitWindow{
			{Name: "primary", UsedPercent: 100, LimitWindowSeconds: 5 * 60 * 60},
			{Name: "GPT-6-Codex-Turbo/primary", Feature: "GPT-6-Codex-Turbo", UsedPercent: 5, LimitWindowSeconds: 5 * 60 * 60},
		}),
		ScoreFromLimitWindows("turbo-cooked", 0, []LimitWindow{
			{Name: "primary", UsedPercent: 0, LimitWindowSeconds: 5 * 60 * 60},
			{Name: "GPT-6-Codex-Turbo/primary", Feature: "GPT-6-Codex-Turbo", UsedPercent: 100, LimitWindowSeconds: 5 * 60 * 60},
		}),
	}).ForModel("gpt-6-codex-turbo")

	got, err := scheduler.Pick([]account.Account{
		{ID: "turbo-cooked", AuthMode: account.AuthModeOAuth},
		{ID: "turbo-open", AuthMode: account.AuthModeOAuth},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "turbo-open" {
		t.Fatalf("got %q, want turbo-open (routed by its own metered pool)", got.ID)
	}
}

func TestScoreExcludesFeatureWindowsFromBase(t *testing.T) {
	// An exhausted Spark pool must not drag down the base score used for regular
	// models. This is an intended behavior change to the regular selection path.
	score := ScoreFromLimitWindows("acct", 0, []LimitWindow{
		{Name: "5h", UsedPercent: 10},
		{Name: "7d", UsedPercent: 20},
		{Name: "GPT-5.3-Codex-Spark/primary", Feature: "GPT-5.3-Codex-Spark", UsedPercent: 100},
	})

	if math.Abs(score.Headroom-0.80) > 0.0001 {
		t.Fatalf("base headroom = %.2f, want 0.80 (Spark exhaustion excluded)", score.Headroom)
	}
	spark, ok := score.ModelScores[ModelKey("gpt-5.3-codex-spark")]
	if !ok {
		t.Fatal("expected a Spark model pool score")
	}
	if !spark.exhausted() {
		t.Fatal("Spark pool should be exhausted")
	}
}

func TestForModelZeroesAccountsLackingTheFeature(t *testing.T) {
	// One account advertises the Spark pool, another does not. A Spark request
	// must not land on the account that cannot serve it.
	scheduler := NewScheduler([]Score{
		ScoreFromLimitWindows("has-spark", 0, []LimitWindow{
			{Name: "primary", UsedPercent: 50, LimitWindowSeconds: 5 * 60 * 60},
			{Name: "GPT-5.3-Codex-Spark/primary", Feature: "GPT-5.3-Codex-Spark", UsedPercent: 0, LimitWindowSeconds: 5 * 60 * 60},
		}),
		ScoreFromLimitWindows("no-spark", 0, []LimitWindow{
			{Name: "primary", UsedPercent: 0, LimitWindowSeconds: 5 * 60 * 60},
		}),
	}).ForModel("gpt-5.3-codex-spark")

	if !scheduler.Exhausted("", "no-spark") {
		t.Fatal("account without the Spark pool must score zero for a Spark request")
	}
	if scheduler.Exhausted("", "has-spark") {
		t.Fatal("account with an open Spark pool must be usable")
	}
}

func TestForModelWithoutAnyPoolsFallsBackToBase(t *testing.T) {
	// If no account advertises a matching pool (e.g. the upstream renamed the
	// feature), a model request falls back to base quota rather than zeroing
	// every account. This documents the version-skew fallback behavior.
	scheduler := NewScheduler([]Score{
		ScoreFromLimitWindows("a", 0, []LimitWindow{{Name: "5h", UsedPercent: 30}}),
	}).ForModel("gpt-5.3-codex-spark")

	if math.Abs(scheduler.score("", "a").Headroom-0.70) > 0.0001 {
		t.Fatalf("headroom = %.2f, want 0.70 (base, no matching pool)", scheduler.score("", "a").Headroom)
	}
}

func TestExpiryAwareRoutingMatchesSnapshotOrder(t *testing.T) {
	state := []simulatedAccount{
		{id: "alpha@example.com", used5h: 73, used7d: 16},
		{id: "founders@example.com", used5h: 0, used7d: 0},
		{id: "dave@example.com", used5h: 7, used7d: 1},
		{id: "erin@example.com", used5h: 0, used7d: 0},
	}
	reset5h := map[string]int64{
		"alpha@example.com":    157 * 60,
		"founders@example.com": 164 * 60,
		"dave@example.com":     175 * 60,
		"erin@example.com":     300 * 60,
	}
	scheduler := NewScheduler(scoresForWithReset(state, reset5h))
	candidates := []account.Account{
		{ID: "alpha@example.com", AuthMode: account.AuthModeOAuth},
		{ID: "founders@example.com", AuthMode: account.AuthModeOAuth},
		{ID: "dave@example.com", AuthMode: account.AuthModeOAuth},
		{ID: "erin@example.com", AuthMode: account.AuthModeOAuth},
	}

	got, err := scheduler.Pick(candidates)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "founders@example.com" {
		t.Fatalf("got %q, want founders@example.com", got.ID)
	}
}

func TestBottleneckHeadroomBeatsRoundRobinWhenWeeklyLimitsDiffer(t *testing.T) {
	accounts := []simulatedAccount{
		{id: "near-weekly-cap", used5h: 5, used7d: 90},
		{id: "healthy", used5h: 5, used7d: 10},
	}

	roundRobinAccepted := simulateRoundRobin(accounts, 20, 3, 3)
	bottleneckAccepted := simulateBottleneck(accounts, 20, 3, 3)

	if roundRobinAccepted != 13 {
		t.Fatalf("round-robin accepted %d, want 13", roundRobinAccepted)
	}
	if bottleneckAccepted != 20 {
		t.Fatalf("bottleneck accepted %d, want 20", bottleneckAccepted)
	}
}

func TestBottleneckHeadroomAvoidsShortWindowExhaustion(t *testing.T) {
	accounts := []simulatedAccount{
		{id: "near-5h-cap", used5h: 92, used7d: 5},
		{id: "healthy", used5h: 10, used7d: 5},
	}

	roundRobinAccepted := simulateRoundRobin(accounts, 20, 4, 1)
	bottleneckAccepted := simulateBottleneck(accounts, 20, 4, 1)

	if roundRobinAccepted != 12 {
		t.Fatalf("round-robin accepted %d, want 12", roundRobinAccepted)
	}
	if bottleneckAccepted != 20 {
		t.Fatalf("bottleneck accepted %d, want 20", bottleneckAccepted)
	}
}

func simulateRoundRobin(initial []simulatedAccount, sessions int, cost5h float64, cost7d float64) int {
	state := cloneSimulated(initial)
	accepted := 0
	for i := 0; i < sessions; i++ {
		idx := i % len(state)
		if canAssign(state[idx], cost5h, cost7d) {
			assign(&state[idx], cost5h, cost7d)
			accepted++
		}
	}
	return accepted
}

func simulateBottleneck(initial []simulatedAccount, sessions int, cost5h float64, cost7d float64) int {
	state := cloneSimulated(initial)
	accepted := 0
	for i := 0; i < sessions; i++ {
		scheduler := NewScheduler(scoresFor(state))
		candidates := make([]account.Account, 0, len(state))
		for _, acct := range state {
			candidates = append(candidates, account.Account{ID: acct.id, AuthMode: account.AuthModeOAuth})
		}

		pick, err := scheduler.Pick(candidates)
		if err != nil {
			return accepted
		}
		idx := findSimulated(state, pick.ID)
		if idx < 0 || !canAssign(state[idx], cost5h, cost7d) {
			return accepted
		}
		assign(&state[idx], cost5h, cost7d)
		accepted++
	}
	return accepted
}

func scoresFor(state []simulatedAccount) []Score {
	scores := make([]Score, 0, len(state))
	for _, acct := range state {
		scores = append(scores, ScoreFromLimitWindows(acct.id, acct.sessions, []LimitWindow{
			{Name: "5h", UsedPercent: acct.used5h},
			{Name: "7d", UsedPercent: acct.used7d},
		}))
	}
	return scores
}

func scoresForWithReset(state []simulatedAccount, reset5h map[string]int64) []Score {
	scores := make([]Score, 0, len(state))
	for _, acct := range state {
		scores = append(scores, ScoreFromLimitWindows(acct.id, acct.sessions, []LimitWindow{
			{Name: "5h", UsedPercent: acct.used5h, LimitWindowSeconds: 5 * 60 * 60, ResetAfterSeconds: reset5h[acct.id]},
			{Name: "7d", UsedPercent: acct.used7d, LimitWindowSeconds: 7 * 24 * 60 * 60, ResetAfterSeconds: 7 * 24 * 60 * 60},
		}))
	}
	return scores
}

func canAssign(acct simulatedAccount, cost5h float64, cost7d float64) bool {
	return acct.used5h+cost5h <= 100 && acct.used7d+cost7d <= 100
}

func assign(acct *simulatedAccount, cost5h float64, cost7d float64) {
	acct.used5h += cost5h
	acct.used7d += cost7d
	acct.sessions++
}

func cloneSimulated(initial []simulatedAccount) []simulatedAccount {
	return append([]simulatedAccount(nil), initial...)
}

func findSimulated(state []simulatedAccount, id string) int {
	for i, acct := range state {
		if acct.id == id {
			return i
		}
	}
	return -1
}
