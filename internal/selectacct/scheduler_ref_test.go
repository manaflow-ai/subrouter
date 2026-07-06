package selectacct

import (
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
