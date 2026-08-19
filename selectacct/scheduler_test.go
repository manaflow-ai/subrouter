package selectacct

import (
	"testing"

	"github.com/manaflow-ai/subrouter/account"
)

func TestPickPrefersHeadroom(t *testing.T) {
	scheduler := NewScheduler([]Score{
		{AccountID: "a", Headroom: 0.20, Sessions: 0},
		{AccountID: "b", Headroom: 0.90, Sessions: 5},
	})

	got, err := scheduler.Pick([]account.Account{{ID: "a"}, {ID: "b"}})
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "b" {
		t.Fatalf("got %q, want b", got.ID)
	}
}

func TestPickPrefersSoonExpiringHealthyHeadroom(t *testing.T) {
	scheduler := NewScheduler([]Score{
		{AccountID: "later-full", Headroom: 1.00, ShortHeadroom: 1.00, ShortResetAfterSeconds: 5 * 60 * 60, ExpiryPressure: 1.00 / float64(5*60*60)},
		{AccountID: "soon-healthy", Headroom: 0.93, ShortHeadroom: 0.93, ShortResetAfterSeconds: 3 * 60 * 60, ExpiryPressure: 0.93 / float64(3*60*60)},
	})

	got, err := scheduler.Pick([]account.Account{{ID: "later-full"}, {ID: "soon-healthy"}})
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "soon-healthy" {
		t.Fatalf("got %q, want soon-healthy", got.ID)
	}
}

func TestPickProtectsLowShortWindowHeadroom(t *testing.T) {
	scheduler := NewScheduler([]Score{
		{AccountID: "soon-low", Headroom: 0.27, ShortHeadroom: 0.27, ShortResetAfterSeconds: 157 * 60, ExpiryPressure: 0.27 / float64(157*60)},
		{AccountID: "soon-full", Headroom: 1.00, ShortHeadroom: 1.00, ShortResetAfterSeconds: 164 * 60, ExpiryPressure: 1.00 / float64(164*60)},
	})

	got, err := scheduler.Pick([]account.Account{{ID: "soon-low"}, {ID: "soon-full"}})
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "soon-full" {
		t.Fatalf("got %q, want soon-full", got.ID)
	}
}

func TestPickBreaksTiesByFewestSessions(t *testing.T) {
	scheduler := NewScheduler([]Score{
		{AccountID: "a", Headroom: 0.75, Sessions: 4},
		{AccountID: "b", Headroom: 0.75, Sessions: 1},
	})

	got, err := scheduler.Pick([]account.Account{{ID: "a"}, {ID: "b"}})
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "b" {
		t.Fatalf("got %q, want b", got.ID)
	}
}

func TestWithSessionCountsUsesLiveAssignments(t *testing.T) {
	scheduler := NewScheduler([]Score{
		{AccountID: "a", Headroom: 0.75, Sessions: 0},
		{AccountID: "b", Headroom: 0.75, Sessions: 0},
	}).WithSessionCounts(map[string]int{"a": 2})

	got, err := scheduler.Pick([]account.Account{{ID: "a"}, {ID: "b"}})
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "b" {
		t.Fatalf("got %q, want b", got.ID)
	}
}

func TestPickPrefersOAuthBeforeAPIKey(t *testing.T) {
	scheduler := NewScheduler(nil)

	got, err := scheduler.Pick([]account.Account{
		{ID: "apikey:first", AuthMode: account.AuthModeAPIKey},
		{ID: "z@example.com", AuthMode: account.AuthModeOAuth},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "z@example.com" {
		t.Fatalf("got %q, want OAuth account", got.ID)
	}
}

// API-key accounts cost real money per token; OAuth subscription accounts are
// already paid for. Usable OAuth accounts stay ahead of API-key fallback.
func TestPickKeepsUsableOAuthBeforeAPIKey(t *testing.T) {
	scheduler := NewScheduler([]Score{
		{AccountID: "oauth-usable", Headroom: 0.50, ShortHeadroom: 0.50, Sessions: 9},
		{AccountID: "apikey:flush", Headroom: 0.99, Sessions: 0},
	})

	got, err := scheduler.Pick([]account.Account{
		{ID: "apikey:flush", AuthMode: account.AuthModeAPIKey},
		{ID: "oauth-usable", AuthMode: account.AuthModeOAuth},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "oauth-usable" {
		t.Fatalf("got %q, want usable OAuth account", got.ID)
	}
}

func TestPickHealthyOAuthBeforeExhaustedOAuthAndAPIKey(t *testing.T) {
	scheduler := NewScheduler([]Score{
		{AccountID: "alice@example.com", Headroom: 0.90, ShortHeadroom: 0.90},
		{AccountID: "frank@example.com", Headroom: 0, ShortHeadroom: 0},
		{AccountID: "apikey:team-codex-1", Headroom: 0.01, ShortHeadroom: 0.01},
	})

	got, err := scheduler.Pick([]account.Account{
		{ID: "frank@example.com", AuthMode: account.AuthModeOAuth},
		{ID: "apikey:team-codex-1", AuthMode: account.AuthModeAPIKey},
		{ID: "alice@example.com", AuthMode: account.AuthModeOAuth},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "alice@example.com" {
		t.Fatalf("got %q, want alice@example.com", got.ID)
	}
}

func TestPickFallsBackToAPIKeyBeforeExhaustedOAuth(t *testing.T) {
	scheduler := NewScheduler([]Score{
		{AccountID: "oauth-empty", Headroom: 0, ShortHeadroom: 0},
		{AccountID: "apikey:paid", Headroom: 0.01, ShortHeadroom: 0.01},
	})

	got, err := scheduler.Pick([]account.Account{
		{ID: "oauth-empty", AuthMode: account.AuthModeOAuth},
		{ID: "apikey:paid", AuthMode: account.AuthModeAPIKey},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "apikey:paid" {
		t.Fatalf("got %q, want API-key fallback", got.ID)
	}
}

func TestPickKeepsConstrainedOAuthBeforeExhaustedOAuth(t *testing.T) {
	scheduler := NewScheduler([]Score{
		{AccountID: "short-empty@example.com", Headroom: 0.79, ShortHeadroom: 0},
		{AccountID: "near-threshold@example.com", Headroom: 0.39, ShortHeadroom: 0.39},
	})

	got, err := scheduler.Pick([]account.Account{
		{ID: "short-empty@example.com", AuthMode: account.AuthModeOAuth},
		{ID: "near-threshold@example.com", AuthMode: account.AuthModeOAuth},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "near-threshold@example.com" {
		t.Fatalf("got %q, want constrained but non-exhausted OAuth account", got.ID)
	}
}

// API-key accounts are still picked when no OAuth candidate exists.
func TestPickFallsBackToAPIKeyWhenNoOAuth(t *testing.T) {
	scheduler := NewScheduler(nil)

	got, err := scheduler.Pick([]account.Account{
		{ID: "apikey:only", AuthMode: account.AuthModeAPIKey},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "apikey:only" {
		t.Fatalf("got %q, want apikey:only", got.ID)
	}
}

// A Codex account's ID is its bare email and a Claude account's ID is its
// profile name, which is often the same email, so both providers routinely
// share one ID. The scheduler must score them independently; keying by the
// bare ID alone let one provider's score clobber the other's, which made a
// healthy Codex account look exhausted (and vice versa) whenever a same-named
// Claude profile existed.
func TestScoresAreIsolatedPerProvider(t *testing.T) {
	const shared = "lawrence@cmux.com"
	scheduler := NewScheduler([]Score{
		{AccountID: shared, Provider: account.ProviderCodex, Headroom: 1, ShortHeadroom: 1},
		{AccountID: shared, Provider: account.ProviderClaude, Headroom: 0, ShortHeadroom: 0},
	})

	if scheduler.Exhausted(account.ProviderCodex, shared) {
		t.Fatal("healthy Codex account must not be exhausted by a same-named Claude profile")
	}
	if !scheduler.UsableForNewSession(account.ProviderCodex, shared) {
		t.Fatal("healthy Codex account must stay usable for a new session")
	}
	if !scheduler.Exhausted(account.ProviderClaude, shared) {
		t.Fatal("exhausted Claude profile must stay exhausted")
	}

	got, err := scheduler.Pick([]account.Account{
		{ID: shared, Provider: account.ProviderCodex, AuthMode: account.AuthModeOAuth},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != shared || got.Provider != account.ProviderCodex {
		t.Fatalf("got %q/%s, want the Codex account", got.ID, got.Provider)
	}
}

// Independent of ordering: a healthy Claude profile must survive an exhausted
// same-named Codex account (the reverse clobber direction).
func TestScoresAreIsolatedPerProviderReversed(t *testing.T) {
	const shared = "austin@manaflow.ai"
	scheduler := NewScheduler([]Score{
		{AccountID: shared, Provider: account.ProviderClaude, Headroom: 1, ShortHeadroom: 1},
		{AccountID: shared, Provider: account.ProviderCodex, Headroom: 0, ShortHeadroom: 0},
	})

	if scheduler.Exhausted(account.ProviderClaude, shared) {
		t.Fatal("healthy Claude profile must not be exhausted by a same-named Codex account")
	}
	if !scheduler.Exhausted(account.ProviderCodex, shared) {
		t.Fatal("exhausted Codex account must stay exhausted")
	}
}

func TestLiveDebitsReorderPickWithoutExhausting(t *testing.T) {
	accountA := account.Account{ID: "a", Provider: account.ProviderClaude, AuthMode: account.AuthModeOAuth, Token: "x"}
	accountB := account.Account{ID: "b", Provider: account.ProviderClaude, AuthMode: account.AuthModeOAuth, Token: "x"}
	scheduler := NewScheduler([]Score{
		{AccountID: "a", Provider: account.ProviderClaude, Headroom: 1, ShortHeadroom: 1},
		{AccountID: "b", Provider: account.ProviderClaude, Headroom: 1, ShortHeadroom: 1},
	})

	// Equal scores: the deterministic ordering (PickBest) ties to "a".
	// Pick itself spreads within equal-pressure bands, so the ordering
	// contract is asserted through PickBest.
	picked, err := scheduler.PickBest([]account.Account{accountA, accountB})
	if err != nil || picked.ID != "a" {
		t.Fatalf("baseline pick = %v %v, want a", picked.ID, err)
	}

	// Five requests routed to "a" since the snapshot: the debit flips the order.
	debited := scheduler.WithLiveDebits(map[string]int{ScoreKey(account.ProviderClaude, "a"): 5})
	picked, err = debited.PickBest([]account.Account{accountA, accountB})
	if err != nil || picked.ID != "b" {
		t.Fatalf("debited pick = %v %v, want b", picked.ID, err)
	}

	// Even a huge debit never exhausts the account or resurrects an exhausted one.
	heavy := scheduler.WithLiveDebits(map[string]int{ScoreKey(account.ProviderClaude, "a"): 1000})
	if heavy.Exhausted(account.ProviderClaude, "a") {
		t.Fatal("a live debit must never mark an account exhausted")
	}
	cooked := NewScheduler([]Score{{AccountID: "a", Provider: account.ProviderClaude, Headroom: 0, ShortHeadroom: 0}}).
		WithLiveDebits(map[string]int{ScoreKey(account.ProviderClaude, "a"): 3})
	if !cooked.Exhausted(account.ProviderClaude, "a") {
		t.Fatal("a live debit must not resurrect an exhausted account")
	}
}

func TestLiveDebitsSurviveForModelAndSessionCounts(t *testing.T) {
	scheduler := NewScheduler([]Score{
		{AccountID: "a", Provider: account.ProviderClaude, Headroom: 1, ShortHeadroom: 1,
			ModelScores: map[string]Score{"claudefable": {AccountID: "a", Provider: account.ProviderClaude, Headroom: 1, ShortHeadroom: 1}}},
		{AccountID: "b", Provider: account.ProviderClaude, Headroom: 1, ShortHeadroom: 1,
			ModelScores: map[string]Score{"claudefable": {AccountID: "b", Provider: account.ProviderClaude, Headroom: 1, ShortHeadroom: 1}}},
		// 35 debited requests push a's headroom to 0.30, below the
		// new-session threshold, so the pick is deterministic even though
		// placement spreads within equal-pressure bands.
	}).WithLiveDebits(map[string]int{ScoreKey(account.ProviderClaude, "a"): 35}).
		WithSessionCounts(map[string]int{})
	accountA := account.Account{ID: "a", Provider: account.ProviderClaude, AuthMode: account.AuthModeOAuth, Token: "x"}
	accountB := account.Account{ID: "b", Provider: account.ProviderClaude, AuthMode: account.AuthModeOAuth, Token: "x"}
	picked, err := scheduler.ForModel("claude-fable").Pick([]account.Account{accountA, accountB})
	if err != nil || picked.ID != "b" {
		t.Fatalf("pool pick = %v %v, want b (debits must survive ForModel and WithSessionCounts)", picked.ID, err)
	}
}

func TestFablePickPrefersSoonerWeeklyReset(t *testing.T) {
	const fable = "claudefable"
	soon := ScoreFromLimitWindows("soon@example.com", 0, []LimitWindow{
		{Name: "5h", Feature: fable, UsedPercent: 10, LimitWindowSeconds: 5 * 60 * 60, ResetAfterSeconds: 5 * 60 * 60},
		{Name: "oauth-apps-weekly", Feature: fable, UsedPercent: 10, LimitWindowSeconds: 7 * 24 * 60 * 60, ResetAfterSeconds: 2 * 60 * 60},
	})
	soon.Provider = account.ProviderClaude
	later := ScoreFromLimitWindows("later@example.com", 0, []LimitWindow{
		{Name: "5h", Feature: fable, UsedPercent: 10, LimitWindowSeconds: 5 * 60 * 60, ResetAfterSeconds: 5 * 60 * 60},
		{Name: "oauth-apps-weekly", Feature: fable, UsedPercent: 10, LimitWindowSeconds: 7 * 24 * 60 * 60, ResetAfterSeconds: 5 * 24 * 60 * 60},
	})
	later.Provider = account.ProviderClaude
	scheduler := NewScheduler([]Score{later, soon}).ForModel(fable)
	picked, err := scheduler.Pick([]account.Account{
		{ID: "later@example.com", Provider: account.ProviderClaude, AuthMode: account.AuthModeOAuth, Token: "x"},
		{ID: "soon@example.com", Provider: account.ProviderClaude, AuthMode: account.AuthModeOAuth, Token: "x"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if picked.ID != "soon@example.com" {
		t.Fatalf("picked %q, want sooner-resetting fable account", picked.ID)
	}
}

func TestOpusPickIgnoresWeeklyDrainPressure(t *testing.T) {
	const opus = "claudeopus"
	soon := ScoreFromLimitWindows("soon@example.com", 0, []LimitWindow{
		{Name: "5h", Feature: opus, UsedPercent: 10, LimitWindowSeconds: 5 * 60 * 60, ResetAfterSeconds: 5 * 60 * 60},
		{Name: "opus-weekly", Feature: opus, UsedPercent: 10, LimitWindowSeconds: 7 * 24 * 60 * 60, ResetAfterSeconds: 2 * 60 * 60},
	})
	soon.Provider = account.ProviderClaude
	later := ScoreFromLimitWindows("later@example.com", 0, []LimitWindow{
		{Name: "5h", Feature: opus, UsedPercent: 10, LimitWindowSeconds: 5 * 60 * 60, ResetAfterSeconds: 5 * 60 * 60},
		{Name: "opus-weekly", Feature: opus, UsedPercent: 10, LimitWindowSeconds: 7 * 24 * 60 * 60, ResetAfterSeconds: 5 * 24 * 60 * 60},
	})
	later.Provider = account.ProviderClaude
	scheduler := NewScheduler([]Score{later, soon}).ForModel(opus)
	// PickBest: the assertion is about ordering (weekly drain pressure must
	// not reorder opus picks), not about placement spreading.
	picked, err := scheduler.PickBest([]account.Account{
		{ID: "later@example.com", Provider: account.ProviderClaude, AuthMode: account.AuthModeOAuth, Token: "x"},
		{ID: "soon@example.com", Provider: account.ProviderClaude, AuthMode: account.AuthModeOAuth, Token: "x"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if picked.ID != "later@example.com" {
		t.Fatalf("picked %q, want ordinary headroom/tie ordering unchanged for opus", picked.ID)
	}
}

func TestSchedulerRefLiveDebitLifecycle(t *testing.T) {
	ref := NewSchedulerRef(NewScheduler(nil))
	ref.NoteRouted(account.ProviderClaude, "a")
	ref.NoteRouted(account.ProviderClaude, "a")
	ref.NoteRouted(account.ProviderCodex, "a")
	debits := ref.LiveDebits()
	if debits[ScoreKey(account.ProviderClaude, "a")] != 2 || debits[ScoreKey(account.ProviderCodex, "a")] != 1 {
		t.Fatalf("debits = %v", debits)
	}
	// A failed refresh keeps the debits (the stale snapshot still applies)...
	ref.FinishRefresh(Scheduler{}, false)
	if ref.LiveDebits() == nil {
		t.Fatal("failed refresh must keep live debits")
	}
	// ...a successful one supersedes them.
	ref.FinishRefresh(NewScheduler(nil), true)
	if ref.LiveDebits() != nil {
		t.Fatal("fresh refresh must clear live debits")
	}
}
