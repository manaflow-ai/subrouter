package selectacct

import (
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/account"
)

func TestSchedulerRefAllowsOnlyOneStaleRefresh(t *testing.T) {
	ref := NewSchedulerRef(NewScheduler(nil))
	ref.SetUpdatedAt(time.Now().Add(-time.Hour))

	if !ref.BeginRefreshIfStale(time.Minute) {
		t.Fatal("first stale refresh should begin")
	}
	if ref.BeginRefreshIfStale(time.Minute) {
		t.Fatal("second stale refresh should be suppressed while refresh is running")
	}

	ref.FinishRefresh(NewScheduler([]Score{{AccountID: "fresh", Headroom: 1, ShortHeadroom: 1}}), true)
	if ref.BeginRefreshIfStale(time.Minute) {
		t.Fatal("freshly completed refresh should not immediately restart")
	}
}

func TestSchedulerRefRetryAfterSkippedRefreshWaitsForTTL(t *testing.T) {
	ref := NewSchedulerRef(NewScheduler(nil))
	ref.SetUpdatedAt(time.Now().Add(-time.Hour))

	if !ref.BeginRefreshIfStale(time.Minute) {
		t.Fatal("stale refresh should begin")
	}
	ref.FinishRefresh(Scheduler{}, false)

	if ref.BeginRefreshIfStale(time.Minute) {
		t.Fatal("skipped refresh should still touch updatedAt")
	}
	ref.SetUpdatedAt(time.Now().Add(-time.Hour))
	if !ref.BeginRefreshIfStale(time.Minute) {
		t.Fatal("refresh should be allowed after TTL passes")
	}
}

func TestSchedulerRefNewerSetInvalidatesOlderRefresh(t *testing.T) {
	ref := NewSchedulerRef(NewScheduler([]Score{{
		AccountID: "old@example.com", Provider: account.ProviderCodex, Headroom: 0.9, ShortHeadroom: 0.9,
	}}))
	ref.SetUpdatedAt(time.Time{})
	if !ref.BeginRefreshIfStale(time.Minute) {
		t.Fatal("stale refresh should begin")
	}

	ref.Set(NewScheduler([]Score{{
		AccountID: "new@example.com", Provider: account.ProviderCodex, Headroom: 0.8, ShortHeadroom: 0.8,
	}}))
	ref.FinishRefresh(NewScheduler([]Score{{
		AccountID: "old@example.com", Provider: account.ProviderCodex, Headroom: 0.1, ShortHeadroom: 0.1,
	}}), true)

	if got := ref.Get().ScoreFor(account.ProviderCodex, "new@example.com").Headroom; got != 0.8 {
		t.Fatalf("older refresh overwrote newer scheduler: headroom = %v, want 0.8", got)
	}
}

func TestSchedulerRefAccountGenerationInvalidatesLegacyRefresh(t *testing.T) {
	ref := NewSchedulerRef(NewScheduler([]Score{{
		AccountID: "old@example.com", Provider: account.ProviderCodex, Headroom: 0.9, ShortHeadroom: 0.9,
	}}))
	ref.SetUpdatedAt(time.Time{})
	if !ref.BeginRefreshIfStale(time.Minute) {
		t.Fatal("legacy refresh did not begin")
	}

	ref.AdvanceAccountGeneration(2)
	if !ref.SetForAccountGeneration(NewScheduler([]Score{{
		AccountID: "new@example.com", Provider: account.ProviderCodex, Headroom: 0.8, ShortHeadroom: 0.8,
	}}), 2) {
		t.Fatal("new account generation was not published")
	}
	ref.FinishRefresh(NewScheduler([]Score{{
		AccountID: "old@example.com", Provider: account.ProviderCodex, Headroom: 0.1, ShortHeadroom: 0.1,
	}}), true)

	if got := ref.Get().ScoreFor(account.ProviderCodex, "new@example.com").Headroom; got != 0.8 {
		t.Fatalf("legacy refresh overwrote a newer account generation: headroom = %v, want 0.8", got)
	}
}

// TestMarkExhaustedUntilExpires: a mark with a reset time in the past must lapse
// on the next read, restoring the optimistic default so routing retries the
// account. A future mark holds.
func TestMarkExhaustedUntilExpires(t *testing.T) {
	ref := NewSchedulerRef(NewScheduler(nil))
	ref.MarkExhaustedUntil(account.ProviderClaude, "recovered@example.com", "", time.Now().Add(-time.Second))
	ref.MarkExhaustedUntil(account.ProviderClaude, "cooked@example.com", "", time.Now().Add(time.Hour))
	s := ref.Get()
	if s.Exhausted(account.ProviderClaude, "recovered@example.com") {
		t.Fatal("expired mark must lapse: recovered account still exhausted")
	}
	if !s.Exhausted(account.ProviderClaude, "cooked@example.com") {
		t.Fatal("unexpired mark must hold: cooked account not exhausted")
	}
}

func TestMarkExhaustedUntilPoolScoped(t *testing.T) {
	const (
		fable = "claudefable"
		opus  = "claudeopus"
	)
	ref := NewSchedulerRef(NewScheduler([]Score{{
		AccountID:     "a@example.com",
		Provider:      account.ProviderClaude,
		Headroom:      1,
		ShortHeadroom: 1,
		ModelScores: map[string]Score{
			fable: {AccountID: "a@example.com", Provider: account.ProviderClaude, Headroom: 1, ShortHeadroom: 1},
			opus:  {AccountID: "a@example.com", Provider: account.ProviderClaude, Headroom: 1, ShortHeadroom: 1},
		},
	}}))
	ref.MarkExhaustedUntil(account.ProviderClaude, "a@example.com", fable, time.Now().Add(time.Hour))

	s := ref.Get()
	if !s.ForModel(fable).Exhausted(account.ProviderClaude, "a@example.com") {
		t.Fatal("fable pool mark should exhaust fable")
	}
	if s.ForModel(opus).Exhausted(account.ProviderClaude, "a@example.com") {
		t.Fatal("fable pool mark should not exhaust opus")
	}
	if s.Exhausted(account.ProviderClaude, "a@example.com") {
		t.Fatal("fable pool mark should not exhaust the base account score")
	}
}

func TestMarkExhaustedUntilAccountWideStillExhaustsEveryPool(t *testing.T) {
	const fable = "claudefable"
	ref := NewSchedulerRef(NewScheduler([]Score{{
		AccountID:     "a@example.com",
		Provider:      account.ProviderClaude,
		Headroom:      1,
		ShortHeadroom: 1,
		ModelScores: map[string]Score{
			fable: {AccountID: "a@example.com", Provider: account.ProviderClaude, Headroom: 1, ShortHeadroom: 1},
		},
	}}))
	ref.MarkExhaustedUntil(account.ProviderClaude, "a@example.com", "", time.Now().Add(time.Hour))

	s := ref.Get()
	if !s.Exhausted(account.ProviderClaude, "a@example.com") {
		t.Fatal("account-wide mark should exhaust the base score")
	}
	if !s.ForModel(fable).Exhausted(account.ProviderClaude, "a@example.com") {
		t.Fatal("account-wide mark should exhaust model pools")
	}
}

func TestMarkExhaustedUntilPoolMarkExpires(t *testing.T) {
	const fable = "claudefable"
	ref := NewSchedulerRef(NewScheduler(nil))
	ref.MarkExhaustedUntil(account.ProviderClaude, "a@example.com", fable, time.Now().Add(-time.Second))

	if ref.Get().ForModel(fable).Exhausted(account.ProviderClaude, "a@example.com") {
		t.Fatal("expired pool mark should allow an optimistic retry")
	}
	if _, ok := ref.ExhaustedUntilFor(account.ProviderClaude, "a@example.com", fable); ok {
		t.Fatal("expired pool mark should be pruned")
	}
}

// TestSetClearsExhaustedUntil: a full refresh supersedes request-time marks; a
// later prune must not delete refreshed scores.
func TestSetClearsExhaustedUntil(t *testing.T) {
	ref := NewSchedulerRef(NewScheduler(nil))
	ref.MarkExhaustedUntil(account.ProviderClaude, "a@example.com", "", time.Now().Add(-time.Second))
	ref.Set(NewScheduler([]Score{{AccountID: "a@example.com", Provider: account.ProviderClaude, Headroom: 0.02, ShortHeadroom: 0.02}}))
	s := ref.Get()
	if got := s.ScoreFor(account.ProviderClaude, "a@example.com").Headroom; got != 0.02 {
		t.Fatalf("refreshed score clobbered by stale expiry prune: headroom=%v want 0.02", got)
	}
}

// TestPartialRefreshKeepsMarkExpiry is the mixed-refresh regression: a refresh
// that carries the exhausted account's zero score forward (its own usage fetch
// failed) must NOT strip the mark's expiry, or the mark becomes permanent again
// and the recovered account stays unroutable.
func TestPartialRefreshKeepsMarkExpiry(t *testing.T) {
	ref := NewSchedulerRef(NewScheduler(nil))
	ref.MarkExhaustedUntil(account.ProviderClaude, "recovered@example.com", "", time.Now().Add(-time.Second))
	// Partial refresh: another account got fresh data, but recovered@'s zero
	// score is seeded/carried forward unchanged.
	ref.FinishRefresh(NewScheduler([]Score{
		{AccountID: "other@example.com", Provider: account.ProviderClaude, Headroom: 0.8, ShortHeadroom: 0.8},
		{AccountID: "recovered@example.com", Provider: account.ProviderClaude, Headroom: 0, ShortHeadroom: 0},
	}), true)
	if ref.Get().Exhausted(account.ProviderClaude, "recovered@example.com") {
		t.Fatal("carried-forward zero score must keep its expiry; recovered account still exhausted after lapse")
	}
	// But a refresh that genuinely supersedes the mark (headroom) drops the expiry.
	ref.MarkExhaustedUntil(account.ProviderClaude, "busy@example.com", "", time.Now().Add(-time.Second))
	ref.FinishRefresh(NewScheduler([]Score{
		{AccountID: "busy@example.com", Provider: account.ProviderClaude, Headroom: 0.05, ShortHeadroom: 0.05},
	}), true)
	if got := ref.Get().ScoreFor(account.ProviderClaude, "busy@example.com").Headroom; got != 0.05 {
		t.Fatalf("superseded mark must not prune the refreshed score: headroom=%v want 0.05", got)
	}
}

// TestLapsedMarkRemarksOnNextReject documents the retry-once-on-lapse loop: a
// lapsed mark makes a still-cooked account optimistic for exactly one probe;
// the upstream's next reject re-marks it with the new authoritative reset, so
// the cost of guessing wrong is bounded at one attempt per expiry window.
func TestLapsedMarkRemarksOnNextReject(t *testing.T) {
	ref := NewSchedulerRef(NewScheduler(nil))
	ref.MarkExhaustedUntil(account.ProviderClaude, "cooked@example.com", "", time.Now().Add(-time.Second))
	if ref.Get().Exhausted(account.ProviderClaude, "cooked@example.com") {
		t.Fatal("lapsed mark should allow one optimistic probe")
	}
	// The probe's rejected response re-marks with the new upstream reset.
	ref.MarkExhaustedUntil(account.ProviderClaude, "cooked@example.com", "", time.Now().Add(2*time.Hour))
	if !ref.Get().Exhausted(account.ProviderClaude, "cooked@example.com") {
		t.Fatal("re-mark after the probe's reject must hold until the new reset")
	}
}

// TestFreshZeroReanchorsExpiry: a successful usage fetch that re-confirms
// exhaustion must re-anchor the mark's expiry to that newest evidence, so an
// older request-time expiry cannot lapse a freshly-observed zero back to
// optimistic. Expiries only extend, never shorten.
func TestFreshZeroReanchorsExpiry(t *testing.T) {
	ref := NewSchedulerRef(NewScheduler(nil))
	// Old request-time mark about to lapse.
	ref.MarkExhaustedUntil(account.ProviderClaude, "confirmed@example.com", "", time.Now().Add(time.Millisecond))
	// Fresh refresh re-confirms exhaustion with a 2h window reset.
	ref.FinishRefresh(NewScheduler([]Score{
		{AccountID: "confirmed@example.com", Provider: account.ProviderClaude, Headroom: 0, ShortHeadroom: 0, ShortResetAfterSeconds: 7200, Fresh: true},
	}), true)
	until, ok := ref.ExhaustedUntilFor(account.ProviderClaude, "confirmed@example.com", "")
	if !ok || time.Until(until) < 90*time.Minute {
		t.Fatalf("fresh zero should re-anchor expiry to its reset (~2h), got %v (in %v)", until, time.Until(until))
	}
	if ref.Get().Exhausted(account.ProviderClaude, "confirmed@example.com") != true {
		t.Fatal("freshly-confirmed exhausted account must stay exhausted")
	}

	// A fresh zero must never SHORTEN a longer authoritative expiry.
	ref2 := NewSchedulerRef(NewScheduler(nil))
	long := time.Now().Add(72 * time.Hour)
	ref2.MarkExhaustedUntil(account.ProviderClaude, "weekly@example.com", "", long)
	ref2.FinishRefresh(NewScheduler([]Score{
		{AccountID: "weekly@example.com", Provider: account.ProviderClaude, Headroom: 0, ShortHeadroom: 0, ShortResetAfterSeconds: 3600, Fresh: true},
	}), true)
	got, _ := ref2.ExhaustedUntilFor(account.ProviderClaude, "weekly@example.com", "")
	if !got.Equal(long) {
		t.Fatalf("fresh zero shortened authoritative expiry: got %v want %v", got, long)
	}
}

func TestPoolScopedFreshZeroReanchorsExpiry(t *testing.T) {
	const fable = "claudefable"
	ref := NewSchedulerRef(NewScheduler(nil))
	ref.MarkExhaustedUntil(account.ProviderClaude, "confirmed@example.com", fable, time.Now().Add(time.Millisecond))
	ref.FinishRefresh(NewScheduler([]Score{{
		AccountID:     "confirmed@example.com",
		Provider:      account.ProviderClaude,
		Headroom:      1,
		ShortHeadroom: 1,
		ModelScores: map[string]Score{
			fable: {
				AccountID:              "confirmed@example.com",
				Provider:               account.ProviderClaude,
				Headroom:               0,
				ShortHeadroom:          0,
				ShortResetAfterSeconds: 7200,
				Fresh:                  true,
			},
		},
	}}), true)

	until, ok := ref.ExhaustedUntilFor(account.ProviderClaude, "confirmed@example.com", fable)
	if !ok || time.Until(until) < 90*time.Minute {
		t.Fatalf("fresh pool zero should re-anchor expiry to its reset (~2h), got %v (in %v)", until, time.Until(until))
	}
	if !ref.Get().ForModel(fable).Exhausted(account.ProviderClaude, "confirmed@example.com") {
		t.Fatal("freshly-confirmed exhausted pool must stay exhausted")
	}
}

func TestPoolScopedRecoveredRefreshDropsExpiry(t *testing.T) {
	const fable = "claudefable"
	ref := NewSchedulerRef(NewScheduler(nil))
	ref.MarkExhaustedUntil(account.ProviderClaude, "recovered@example.com", fable, time.Now().Add(time.Hour))
	ref.FinishRefresh(NewScheduler([]Score{{
		AccountID:     "recovered@example.com",
		Provider:      account.ProviderClaude,
		Headroom:      1,
		ShortHeadroom: 1,
		ModelScores: map[string]Score{
			fable: {AccountID: "recovered@example.com", Provider: account.ProviderClaude, Headroom: 0.8, ShortHeadroom: 0.8, Fresh: true},
		},
	}}), true)

	if _, ok := ref.ExhaustedUntilFor(account.ProviderClaude, "recovered@example.com", fable); ok {
		t.Fatal("fresh recovered pool score should drop the pool mark")
	}
	if ref.Get().ForModel(fable).Exhausted(account.ProviderClaude, "recovered@example.com") {
		t.Fatal("fresh recovered pool score should be routable")
	}
}

func TestPoolScopedRefreshWithoutPoolEvidenceKeepsExpiry(t *testing.T) {
	const model = "gpt-5.6-sol"
	ref := NewSchedulerRef(NewScheduler(nil))
	ref.MarkExhaustedUntil(account.ProviderCodex, "incompatible@example.com", model, time.Now().Add(time.Hour))
	ref.FinishRefresh(NewScheduler([]Score{{
		AccountID:     "incompatible@example.com",
		Provider:      account.ProviderCodex,
		Headroom:      0.8,
		ShortHeadroom: 0.8,
		Fresh:         true,
	}}), true)

	if _, ok := ref.ExhaustedUntilFor(account.ProviderCodex, "incompatible@example.com", model); !ok {
		t.Fatal("account-wide refresh without model evidence must keep the model-scoped mark")
	}
	if !ref.Get().ForModel(model).Exhausted(account.ProviderCodex, "incompatible@example.com") {
		t.Fatal("model-scoped mark must survive a refresh that cannot evaluate that model")
	}
	if ref.Get().Exhausted(account.ProviderCodex, "incompatible@example.com") {
		t.Fatal("model-scoped mark must not exhaust the base account score")
	}
}

func TestModelIncompatibilitySurvivesHealthyPoolRefresh(t *testing.T) {
	const model = "gpt-5.6-sol"
	ref := NewSchedulerRef(NewScheduler(nil))
	ref.MarkModelIncompatibleUntil(account.ProviderCodex, "incompatible@example.com", model, time.Now().Add(time.Hour))
	ref.FinishRefresh(NewScheduler([]Score{{
		AccountID:     "incompatible@example.com",
		Provider:      account.ProviderCodex,
		Headroom:      0.8,
		ShortHeadroom: 0.8,
		Fresh:         true,
		ModelScores: map[string]Score{
			model: {
				AccountID:     "incompatible@example.com",
				Provider:      account.ProviderCodex,
				Headroom:      0.8,
				ShortHeadroom: 0.8,
				Fresh:         true,
			},
		},
	}}), true)

	if _, ok := ref.ModelIncompatibleUntilFor(account.ProviderCodex, "incompatible@example.com", model); !ok {
		t.Fatal("quota refresh must not clear account/model incompatibility")
	}
	if !ref.Get().ForModel(model).Exhausted(account.ProviderCodex, "incompatible@example.com") {
		t.Fatal("incompatible account must remain excluded for the rejected model")
	}
	if ref.Get().Exhausted(account.ProviderCodex, "incompatible@example.com") {
		t.Fatal("model incompatibility must not exhaust the base account score")
	}
}

func TestPoolScopedRetainLeavesOtherPoolMark(t *testing.T) {
	const (
		fable = "claudefable"
		opus  = "claudeopus"
	)
	ref := NewSchedulerRef(NewScheduler(nil))
	ref.MarkExhaustedUntil(account.ProviderClaude, "a@example.com", opus, time.Now().Add(time.Hour))
	ref.FinishRefresh(NewScheduler([]Score{{
		AccountID:     "a@example.com",
		Provider:      account.ProviderClaude,
		Headroom:      1,
		ShortHeadroom: 1,
		ModelScores: map[string]Score{
			fable: {AccountID: "a@example.com", Provider: account.ProviderClaude, Headroom: 0.8, ShortHeadroom: 0.8, Fresh: true},
			opus:  {AccountID: "a@example.com", Provider: account.ProviderClaude, Headroom: 0, ShortHeadroom: 0},
		},
	}}), true)

	if _, ok := ref.ExhaustedUntilFor(account.ProviderClaude, "a@example.com", opus); !ok {
		t.Fatal("fable refresh should not clear a carried-forward opus mark")
	}
	if !ref.Get().ForModel(opus).Exhausted(account.ProviderClaude, "a@example.com") {
		t.Fatal("opus mark should still apply after fable refresh")
	}
}
