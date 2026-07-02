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

func TestSchedulerRefPreservesExhaustedUntilAcrossRefresh(t *testing.T) {
	ref := NewSchedulerRef(NewScheduler([]Score{{
		AccountID:     "claude@example.com",
		Provider:      accounts.ProviderClaude,
		Headroom:      1,
		ShortHeadroom: 1,
	}}))
	ref.MarkExhaustedUntil(accounts.ProviderClaude, "claude@example.com", time.Now().Add(time.Hour))

	ref.FinishRefresh(NewScheduler([]Score{{
		AccountID:     "claude@example.com",
		Provider:      accounts.ProviderClaude,
		Headroom:      1,
		ShortHeadroom: 1,
	}}), true)

	if !ref.Get().Exhausted(accounts.ProviderClaude, "claude@example.com") {
		t.Fatal("fresh usage refresh clobbered live exhausted-until state")
	}
}

func TestSchedulerRefExpiresExhaustedUntilOnRefresh(t *testing.T) {
	ref := NewSchedulerRef(NewScheduler([]Score{{
		AccountID:     "claude@example.com",
		Provider:      accounts.ProviderClaude,
		Headroom:      1,
		ShortHeadroom: 1,
	}}))
	ref.MarkExhaustedUntil(accounts.ProviderClaude, "claude@example.com", time.Now().Add(-time.Second))

	ref.FinishRefresh(NewScheduler([]Score{{
		AccountID:     "claude@example.com",
		Provider:      accounts.ProviderClaude,
		Headroom:      1,
		ShortHeadroom: 1,
	}}), true)

	if ref.Get().Exhausted(accounts.ProviderClaude, "claude@example.com") {
		t.Fatal("expired exhausted-until state should not override fresh usage")
	}
}
