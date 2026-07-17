package selectacct

import (
	"sync"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
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

// TestMarkExhaustedUntilExpires: a mark with a reset time in the past must lapse
// on the next read, restoring the optimistic default so routing retries the
// account. A future mark holds.
func TestMarkExhaustedUntilExpires(t *testing.T) {
	ref := NewSchedulerRef(NewScheduler(nil))
	ref.MarkExhaustedUntil(accounts.ProviderClaude, "recovered@example.com", "", time.Now().Add(-time.Second))
	ref.MarkExhaustedUntil(accounts.ProviderClaude, "cooked@example.com", "", time.Now().Add(time.Hour))
	s := ref.Get()
	if s.Exhausted(accounts.ProviderClaude, "recovered@example.com") {
		t.Fatal("expired mark must lapse: recovered account still exhausted")
	}
	if !s.Exhausted(accounts.ProviderClaude, "cooked@example.com") {
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
		Provider:      accounts.ProviderClaude,
		Headroom:      1,
		ShortHeadroom: 1,
		ModelScores: map[string]Score{
			fable: {AccountID: "a@example.com", Provider: accounts.ProviderClaude, Headroom: 1, ShortHeadroom: 1},
			opus:  {AccountID: "a@example.com", Provider: accounts.ProviderClaude, Headroom: 1, ShortHeadroom: 1},
		},
	}}))
	ref.MarkExhaustedUntil(accounts.ProviderClaude, "a@example.com", fable, time.Now().Add(time.Hour))

	s := ref.Get()
	if !s.ForModel(fable).Exhausted(accounts.ProviderClaude, "a@example.com") {
		t.Fatal("fable pool mark should exhaust fable")
	}
	if s.ForModel(opus).Exhausted(accounts.ProviderClaude, "a@example.com") {
		t.Fatal("fable pool mark should not exhaust opus")
	}
	if s.Exhausted(accounts.ProviderClaude, "a@example.com") {
		t.Fatal("fable pool mark should not exhaust the base account score")
	}
}

func TestMarkExhaustedUntilAccountWideStillExhaustsEveryPool(t *testing.T) {
	const fable = "claudefable"
	ref := NewSchedulerRef(NewScheduler([]Score{{
		AccountID:     "a@example.com",
		Provider:      accounts.ProviderClaude,
		Headroom:      1,
		ShortHeadroom: 1,
		ModelScores: map[string]Score{
			fable: {AccountID: "a@example.com", Provider: accounts.ProviderClaude, Headroom: 1, ShortHeadroom: 1},
		},
	}}))
	ref.MarkExhaustedUntil(accounts.ProviderClaude, "a@example.com", "", time.Now().Add(time.Hour))

	s := ref.Get()
	if !s.Exhausted(accounts.ProviderClaude, "a@example.com") {
		t.Fatal("account-wide mark should exhaust the base score")
	}
	if !s.ForModel(fable).Exhausted(accounts.ProviderClaude, "a@example.com") {
		t.Fatal("account-wide mark should exhaust model pools")
	}
}

func TestMarkExhaustedUntilPoolMarkExpires(t *testing.T) {
	const fable = "claudefable"
	ref := NewSchedulerRef(NewScheduler(nil))
	ref.MarkExhaustedUntil(accounts.ProviderClaude, "a@example.com", fable, time.Now().Add(-time.Second))

	if ref.Get().ForModel(fable).Exhausted(accounts.ProviderClaude, "a@example.com") {
		t.Fatal("expired pool mark should allow an optimistic retry")
	}
	if _, ok := ref.ExhaustedUntilFor(accounts.ProviderClaude, "a@example.com", fable); ok {
		t.Fatal("expired pool mark should be pruned")
	}
}

// TestSetClearsExhaustedUntil: a full refresh supersedes request-time marks; a
// later prune must not delete refreshed scores.
func TestSetClearsExhaustedUntil(t *testing.T) {
	ref := NewSchedulerRef(NewScheduler(nil))
	ref.MarkExhaustedUntil(accounts.ProviderClaude, "a@example.com", "", time.Now().Add(-time.Second))
	ref.Set(NewScheduler([]Score{{AccountID: "a@example.com", Provider: accounts.ProviderClaude, Headroom: 0.02, ShortHeadroom: 0.02}}))
	s := ref.Get()
	if got := s.ScoreFor(accounts.ProviderClaude, "a@example.com").Headroom; got != 0.02 {
		t.Fatalf("refreshed score clobbered by stale expiry prune: headroom=%v want 0.02", got)
	}
}

// TestPartialRefreshKeepsMarkExpiry is the mixed-refresh regression: a refresh
// that carries the exhausted account's zero score forward (its own usage fetch
// failed) must NOT strip the mark's expiry, or the mark becomes permanent again
// and the recovered account stays unroutable.
func TestPartialRefreshKeepsMarkExpiry(t *testing.T) {
	ref := NewSchedulerRef(NewScheduler(nil))
	ref.MarkExhaustedUntil(accounts.ProviderClaude, "recovered@example.com", "", time.Now().Add(-time.Second))
	// Partial refresh: another account got fresh data, but recovered@'s zero
	// score is seeded/carried forward unchanged.
	ref.FinishRefresh(NewScheduler([]Score{
		{AccountID: "other@example.com", Provider: accounts.ProviderClaude, Headroom: 0.8, ShortHeadroom: 0.8},
		{AccountID: "recovered@example.com", Provider: accounts.ProviderClaude, Headroom: 0, ShortHeadroom: 0},
	}), true)
	if ref.Get().Exhausted(accounts.ProviderClaude, "recovered@example.com") {
		t.Fatal("carried-forward zero score must keep its expiry; recovered account still exhausted after lapse")
	}
	// But a refresh that genuinely supersedes the mark (headroom) drops the expiry.
	ref.MarkExhaustedUntil(accounts.ProviderClaude, "busy@example.com", "", time.Now().Add(-time.Second))
	ref.FinishRefresh(NewScheduler([]Score{
		{AccountID: "busy@example.com", Provider: accounts.ProviderClaude, Headroom: 0.05, ShortHeadroom: 0.05},
	}), true)
	if got := ref.Get().ScoreFor(accounts.ProviderClaude, "busy@example.com").Headroom; got != 0.05 {
		t.Fatalf("superseded mark must not prune the refreshed score: headroom=%v want 0.05", got)
	}
}

// TestLapsedMarkRemarksOnNextReject documents the retry-once-on-lapse loop: a
// lapsed mark makes a still-cooked account optimistic for exactly one probe;
// the upstream's next reject re-marks it with the new authoritative reset, so
// the cost of guessing wrong is bounded at one attempt per expiry window.
func TestLapsedMarkRemarksOnNextReject(t *testing.T) {
	ref := NewSchedulerRef(NewScheduler(nil))
	ref.MarkExhaustedUntil(accounts.ProviderClaude, "cooked@example.com", "", time.Now().Add(-time.Second))
	if ref.Get().Exhausted(accounts.ProviderClaude, "cooked@example.com") {
		t.Fatal("lapsed mark should allow one optimistic probe")
	}
	// The probe's rejected response re-marks with the new upstream reset.
	ref.MarkExhaustedUntil(accounts.ProviderClaude, "cooked@example.com", "", time.Now().Add(2*time.Hour))
	if !ref.Get().Exhausted(accounts.ProviderClaude, "cooked@example.com") {
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
	ref.MarkExhaustedUntil(accounts.ProviderClaude, "confirmed@example.com", "", time.Now().Add(time.Millisecond))
	// Fresh refresh re-confirms exhaustion with a 2h window reset.
	ref.FinishRefresh(NewScheduler([]Score{
		{AccountID: "confirmed@example.com", Provider: accounts.ProviderClaude, Headroom: 0, ShortHeadroom: 0, ShortResetAfterSeconds: 7200, Fresh: true},
	}), true)
	until, ok := ref.ExhaustedUntilFor(accounts.ProviderClaude, "confirmed@example.com", "")
	if !ok || time.Until(until) < 90*time.Minute {
		t.Fatalf("fresh zero should re-anchor expiry to its reset (~2h), got %v (in %v)", until, time.Until(until))
	}
	if ref.Get().Exhausted(accounts.ProviderClaude, "confirmed@example.com") != true {
		t.Fatal("freshly-confirmed exhausted account must stay exhausted")
	}

	// A fresh zero must never SHORTEN a longer authoritative expiry.
	ref2 := NewSchedulerRef(NewScheduler(nil))
	long := time.Now().Add(72 * time.Hour)
	ref2.MarkExhaustedUntil(accounts.ProviderClaude, "weekly@example.com", "", long)
	ref2.FinishRefresh(NewScheduler([]Score{
		{AccountID: "weekly@example.com", Provider: accounts.ProviderClaude, Headroom: 0, ShortHeadroom: 0, ShortResetAfterSeconds: 3600, Fresh: true},
	}), true)
	got, _ := ref2.ExhaustedUntilFor(accounts.ProviderClaude, "weekly@example.com", "")
	if !got.Equal(long) {
		t.Fatalf("fresh zero shortened authoritative expiry: got %v want %v", got, long)
	}
}

func TestPoolScopedFreshZeroReanchorsExpiry(t *testing.T) {
	const fable = "claudefable"
	ref := NewSchedulerRef(NewScheduler(nil))
	ref.MarkExhaustedUntil(accounts.ProviderClaude, "confirmed@example.com", fable, time.Now().Add(time.Millisecond))
	ref.FinishRefresh(NewScheduler([]Score{{
		AccountID:     "confirmed@example.com",
		Provider:      accounts.ProviderClaude,
		Headroom:      1,
		ShortHeadroom: 1,
		ModelScores: map[string]Score{
			fable: {
				AccountID:              "confirmed@example.com",
				Provider:               accounts.ProviderClaude,
				Headroom:               0,
				ShortHeadroom:          0,
				ShortResetAfterSeconds: 7200,
				Fresh:                  true,
			},
		},
	}}), true)

	until, ok := ref.ExhaustedUntilFor(accounts.ProviderClaude, "confirmed@example.com", fable)
	if !ok || time.Until(until) < 90*time.Minute {
		t.Fatalf("fresh pool zero should re-anchor expiry to its reset (~2h), got %v (in %v)", until, time.Until(until))
	}
	if !ref.Get().ForModel(fable).Exhausted(accounts.ProviderClaude, "confirmed@example.com") {
		t.Fatal("freshly-confirmed exhausted pool must stay exhausted")
	}
}

func TestPoolScopedRecoveredRefreshDropsExpiry(t *testing.T) {
	const fable = "claudefable"
	ref := NewSchedulerRef(NewScheduler(nil))
	ref.MarkExhaustedUntil(accounts.ProviderClaude, "recovered@example.com", fable, time.Now().Add(time.Hour))
	ref.FinishRefresh(NewScheduler([]Score{{
		AccountID:     "recovered@example.com",
		Provider:      accounts.ProviderClaude,
		Headroom:      1,
		ShortHeadroom: 1,
		ModelScores: map[string]Score{
			fable: {AccountID: "recovered@example.com", Provider: accounts.ProviderClaude, Headroom: 0.8, ShortHeadroom: 0.8, Fresh: true},
		},
	}}), true)

	if _, ok := ref.ExhaustedUntilFor(accounts.ProviderClaude, "recovered@example.com", fable); ok {
		t.Fatal("fresh recovered pool score should drop the pool mark")
	}
	if ref.Get().ForModel(fable).Exhausted(accounts.ProviderClaude, "recovered@example.com") {
		t.Fatal("fresh recovered pool score should be routable")
	}
}

func TestPoolScopedRefreshWithoutPoolEvidenceKeepsExpiry(t *testing.T) {
	const model = "gpt-5.6-sol"
	ref := NewSchedulerRef(NewScheduler(nil))
	ref.MarkExhaustedUntil(accounts.ProviderCodex, "incompatible@example.com", model, time.Now().Add(time.Hour))
	ref.FinishRefresh(NewScheduler([]Score{{
		AccountID:     "incompatible@example.com",
		Provider:      accounts.ProviderCodex,
		Headroom:      0.8,
		ShortHeadroom: 0.8,
		Fresh:         true,
	}}), true)

	if _, ok := ref.ExhaustedUntilFor(accounts.ProviderCodex, "incompatible@example.com", model); !ok {
		t.Fatal("account-wide refresh without model evidence must keep the model-scoped mark")
	}
	if !ref.Get().ForModel(model).Exhausted(accounts.ProviderCodex, "incompatible@example.com") {
		t.Fatal("model-scoped mark must survive a refresh that cannot evaluate that model")
	}
	if ref.Get().Exhausted(accounts.ProviderCodex, "incompatible@example.com") {
		t.Fatal("model-scoped mark must not exhaust the base account score")
	}
}

func TestModelIncompatibilitySurvivesHealthyPoolRefresh(t *testing.T) {
	const model = "gpt-5.6-sol"
	ref := NewSchedulerRef(NewScheduler(nil))
	ref.MarkModelIncompatibleUntil(accounts.ProviderCodex, "incompatible@example.com", model, time.Now().Add(time.Hour))
	ref.FinishRefresh(NewScheduler([]Score{{
		AccountID:     "incompatible@example.com",
		Provider:      accounts.ProviderCodex,
		Headroom:      0.8,
		ShortHeadroom: 0.8,
		Fresh:         true,
		ModelScores: map[string]Score{
			model: {
				AccountID:     "incompatible@example.com",
				Provider:      accounts.ProviderCodex,
				Headroom:      0.8,
				ShortHeadroom: 0.8,
				Fresh:         true,
			},
		},
	}}), true)

	if _, ok := ref.ModelIncompatibleUntilFor(accounts.ProviderCodex, "incompatible@example.com", model); !ok {
		t.Fatal("quota refresh must not clear account/model incompatibility")
	}
	if !ref.Get().ForModel(model).Exhausted(accounts.ProviderCodex, "incompatible@example.com") {
		t.Fatal("incompatible account must remain excluded for the rejected model")
	}
	if ref.Get().Exhausted(accounts.ProviderCodex, "incompatible@example.com") {
		t.Fatal("model incompatibility must not exhaust the base account score")
	}
}

func TestPoolScopedRetainLeavesOtherPoolMark(t *testing.T) {
	const (
		fable = "claudefable"
		opus  = "claudeopus"
	)
	ref := NewSchedulerRef(NewScheduler(nil))
	ref.MarkExhaustedUntil(accounts.ProviderClaude, "a@example.com", opus, time.Now().Add(time.Hour))
	ref.FinishRefresh(NewScheduler([]Score{{
		AccountID:     "a@example.com",
		Provider:      accounts.ProviderClaude,
		Headroom:      1,
		ShortHeadroom: 1,
		ModelScores: map[string]Score{
			fable: {AccountID: "a@example.com", Provider: accounts.ProviderClaude, Headroom: 0.8, ShortHeadroom: 0.8, Fresh: true},
			opus:  {AccountID: "a@example.com", Provider: accounts.ProviderClaude, Headroom: 0, ShortHeadroom: 0},
		},
	}}), true)

	if _, ok := ref.ExhaustedUntilFor(accounts.ProviderClaude, "a@example.com", opus); !ok {
		t.Fatal("fable refresh should not clear a carried-forward opus mark")
	}
	if !ref.Get().ForModel(opus).Exhausted(accounts.ProviderClaude, "a@example.com") {
		t.Fatal("opus mark should still apply after fable refresh")
	}
}

func TestUpdateScoresPreservesOtherProviders(t *testing.T) {
	ref := NewSchedulerRef(NewScheduler([]Score{
		{AccountID: "claude@x", Provider: accounts.ProviderClaude, Headroom: 0.90, ShortHeadroom: 0.90, Fresh: true},
		{AccountID: "codex@x", Provider: accounts.ProviderCodex, Headroom: 0.50, ShortHeadroom: 0.50, Fresh: true},
	}))
	// A Codex-only refresh (mirroring the sr auto-switch) must not touch Claude.
	ref.UpdateScores([]Score{
		{AccountID: "codex@x", Provider: accounts.ProviderCodex, Headroom: 0.10, ShortHeadroom: 0.10, Fresh: true},
	})
	sched := ref.Get()
	if got := sched.ScoreFor(accounts.ProviderClaude, "claude@x").Headroom; got != 0.90 {
		t.Fatalf("Claude headroom = %v, want 0.90 (must survive a Codex-only update)", got)
	}
	if sched.Exhausted(accounts.ProviderClaude, "claude@x") {
		t.Fatal("Claude account became exhausted after a Codex-only update")
	}
	if got := sched.ScoreFor(accounts.ProviderCodex, "codex@x").Headroom; got != 0.10 {
		t.Fatalf("Codex headroom = %v, want 0.10 (the fresh update)", got)
	}
}

func TestUpdateScoresAddsNewProviderKeys(t *testing.T) {
	ref := NewSchedulerRef(NewScheduler(nil))
	ref.UpdateScores([]Score{
		{AccountID: "codex@x", Provider: accounts.ProviderCodex, Headroom: 0.30, ShortHeadroom: 0.30, Fresh: true},
	})
	if got := ref.Get().ScoreFor(accounts.ProviderClaude, "claude@x").Headroom; got != 1 {
		t.Fatalf("unscored Claude headroom = %v, want optimistic 1", got)
	}
	if got := ref.Get().ScoreFor(accounts.ProviderCodex, "codex@x").Headroom; got != 0.30 {
		t.Fatalf("Codex headroom = %v, want 0.30", got)
	}
}

func TestUpdateScoresPreservesOtherProviderExhaustionMark(t *testing.T) {
	ref := NewSchedulerRef(NewScheduler([]Score{
		{AccountID: "claude@x", Provider: accounts.ProviderClaude, Headroom: 0.90, ShortHeadroom: 0.90, Fresh: true},
		{AccountID: "codex@x", Provider: accounts.ProviderCodex, Headroom: 0.90, ShortHeadroom: 0.90, Fresh: true},
	}))
	// Both accounts just 429'd — request-time marks recorded as read-time overlays
	// (MarkExhausted writes only exhaustedUntil; the base score stays healthy).
	ref.MarkExhausted(accounts.ProviderClaude, "claude@x", "")
	ref.MarkExhausted(accounts.ProviderCodex, "codex@x", "")
	// A Codex-only refresh must not disturb Claude's mark. Codex's own mark is in
	// scope and correctly reconciled away by its fresh healthy score.
	ref.UpdateScores([]Score{
		{AccountID: "codex@x", Provider: accounts.ProviderCodex, Headroom: 0.90, ShortHeadroom: 0.90, Fresh: true},
	})
	sched := ref.Get()
	if !sched.Exhausted(accounts.ProviderClaude, "claude@x") {
		t.Fatal("Claude request-time exhaustion mark was cleared by a Codex-only UpdateScores")
	}
	if sched.Exhausted(accounts.ProviderCodex, "codex@x") {
		t.Fatal("Codex mark should have been reconciled away by its own fresh healthy score")
	}
}

func TestUpdateScoresClearsLiveDebitsForRefreshedKeysOnly(t *testing.T) {
	ref := NewSchedulerRef(NewScheduler([]Score{
		{AccountID: "claude@x", Provider: accounts.ProviderClaude, Headroom: 0.90, ShortHeadroom: 0.90, Fresh: true},
		{AccountID: "codex@x", Provider: accounts.ProviderCodex, Headroom: 0.90, ShortHeadroom: 0.90, Fresh: true},
	}))
	ref.NoteRouted(accounts.ProviderClaude, "claude@x")
	ref.NoteRouted(accounts.ProviderCodex, "codex@x")
	// Fresh Codex scores supersede Codex's live debit, but Claude's must persist.
	ref.UpdateScores([]Score{
		{AccountID: "codex@x", Provider: accounts.ProviderCodex, Headroom: 0.80, ShortHeadroom: 0.80, Fresh: true},
	})
	debits := ref.LiveDebits()
	if _, ok := debits[ScoreKey(accounts.ProviderCodex, "codex@x")]; ok {
		t.Fatal("refreshed Codex key's live debit should be cleared (fresh score supersedes it)")
	}
	if got := debits[ScoreKey(accounts.ProviderClaude, "claude@x")]; got != 1 {
		t.Fatalf("Claude live debit should survive a Codex-only update; got %v, want 1", got)
	}
}

func TestUpdateScoresConcurrentWithRefreshAndReads(t *testing.T) {
	ref := NewSchedulerRef(NewScheduler([]Score{
		{AccountID: "codex@x", Provider: accounts.ProviderCodex, Headroom: 0.5, ShortHeadroom: 0.5},
		{AccountID: "claude@x", Provider: accounts.ProviderClaude, Headroom: 0.5, ShortHeadroom: 0.5},
	}))
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(4)
		go func() {
			defer wg.Done()
			ref.UpdateScores([]Score{{AccountID: "codex@x", Provider: accounts.ProviderCodex, Headroom: 0.7, ShortHeadroom: 0.7, Fresh: true}})
		}()
		go func() {
			defer wg.Done()
			ref.FinishRefresh(NewScheduler([]Score{{AccountID: "claude@x", Provider: accounts.ProviderClaude, Headroom: 0.6, ShortHeadroom: 0.6, Fresh: true}}), true)
		}()
		go func() {
			defer wg.Done()
			ref.NoteRouted(accounts.ProviderCodex, "codex@x")
		}()
		go func() {
			defer wg.Done()
			_ = ref.Get()
		}()
	}
	wg.Wait()
}

// TestUpdateScoresNormalizesBareCodexProvider locks the load-bearing invariant:
// a bare-provider ("") Codex score must collapse to the same ScoreKey as a
// ProviderCodex-tagged mark, or scoped reconciliation would miss the mark.
func TestUpdateScoresNormalizesBareCodexProvider(t *testing.T) {
	ref := NewSchedulerRef(NewScheduler([]Score{
		{AccountID: "codex@x", Provider: accounts.ProviderCodex, Headroom: 0.9, ShortHeadroom: 0.9, Fresh: true},
	}))
	ref.MarkExhausted(accounts.ProviderCodex, "codex@x", "")
	// Refresh with a bare-provider score (Provider left "").
	ref.UpdateScores([]Score{
		{AccountID: "codex@x", Headroom: 0.9, ShortHeadroom: 0.9, Fresh: true},
	})
	if ref.Get().Exhausted(accounts.ProviderCodex, "codex@x") {
		t.Fatal("bare-provider score did not normalize to the ProviderCodex ScoreKey; fresh score failed to reconcile the mark")
	}
}
