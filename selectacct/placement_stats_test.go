package selectacct

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/account"
)

func poolStats(t *testing.T, snapshot PlacementStatsSnapshot, provider account.Provider, pool string) PoolPlacementStats {
	t.Helper()
	for _, entry := range snapshot.Pools {
		if entry.Provider == provider && entry.Pool == pool {
			return entry
		}
	}
	t.Fatalf("pool %s/%q missing from %+v", provider, pool, snapshot.Pools)
	return PoolPlacementStats{}
}

// A herd is exactly what the pool summary exists to show: every placement on
// one account reads as a busiest share of 1.
func TestPlacementHerdReportsFullShare(t *testing.T) {
	ref := NewSchedulerRef(NewScheduler(nil))
	for i := 0; i < 50; i++ {
		ref.NotePlacement(account.ProviderCodex, "herd@example.com", "")
	}
	pool := poolStats(t, ref.PlacementStats(nil, nil), account.ProviderCodex, "")
	if pool.BusiestShare < 0.999 || pool.BusiestAccountID != "herd@example.com" || pool.Placements != 50 || pool.Accounts != 1 {
		t.Fatalf("pool = %+v, want share ~1 on herd@example.com", pool)
	}
}

func TestPlacementSpreadReportsEvenShareAndSeparatesPools(t *testing.T) {
	ref := NewSchedulerRef(NewScheduler(nil))
	for i := 0; i < 40; i++ {
		ref.NotePlacement(account.ProviderCodex, fmt.Sprintf("a%d@example.com", i%4), "")
	}
	ref.NotePlacement(account.ProviderClaude, "a0@example.com", "opus")
	snapshot := ref.PlacementStats(nil, nil)
	codex := poolStats(t, snapshot, account.ProviderCodex, "")
	if codex.BusiestShare != 0.25 || codex.Accounts != 4 {
		t.Fatalf("codex pool = %+v, want share 0.25 across 4", codex)
	}
	if claude := poolStats(t, snapshot, account.ProviderClaude, "opus"); claude.Placements != 1 {
		t.Fatalf("claude opus pool = %+v", claude)
	}
}

// The busiest share covers the last hour only, so an old herd ages out
// while the lifetime counters keep it.
func TestPlacementHerdWindowAgesOut(t *testing.T) {
	now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	ref := NewSchedulerRef(NewScheduler(nil))
	ref.placement.now = func() time.Time { return now }
	for i := 0; i < 10; i++ {
		ref.NotePlacement(account.ProviderCodex, "old@example.com", "")
	}
	now = now.Add(HerdWindow + time.Minute)
	ref.NotePlacement(account.ProviderCodex, "new@example.com", "")
	ref.NotePlacement(account.ProviderCodex, "other@example.com", "")
	snapshot := ref.PlacementStats(nil, nil)
	pool := poolStats(t, snapshot, account.ProviderCodex, "")
	if pool.Placements != 2 || pool.BusiestShare != 0.5 {
		t.Fatalf("pool = %+v, want only the last hour's 2 placements", pool)
	}
	for _, acct := range snapshot.Accounts {
		if acct.AccountID == "old@example.com" && acct.Placements != 10 {
			t.Fatalf("old lifetime placements = %d, want 10", acct.Placements)
		}
	}
}

// NoteRouted keeps feeding the live debit and also counts the request.
func TestNoteRoutedFeedsLiveDebitAndStats(t *testing.T) {
	ref := NewSchedulerRef(NewScheduler(nil))
	ref.NoteRouted(account.ProviderCodex, "a@example.com")
	ref.NoteRouted(account.ProviderCodex, "a@example.com")
	if got := ref.LiveDebits()[ScoreKey(account.ProviderCodex, "a@example.com")]; got != 2 {
		t.Fatalf("live debit = %d, want 2", got)
	}
	ref.SetUpdatedAt(time.Now().Add(-time.Hour))
	if !ref.BeginRefreshIfStaleForAccountGeneration(time.Minute, 0) || !ref.FinishRefreshForAccountGeneration(NewScheduler(nil), true, 0) {
		t.Fatal("refresh did not run")
	}
	if len(ref.LiveDebits()) != 0 {
		t.Fatal("refresh should clear live debits")
	}
	stats := ref.PlacementStats(nil, nil).Accounts
	if len(stats) != 1 || stats[0].Routed != 2 {
		t.Fatalf("routed stats = %+v, want 2 surviving the refresh", stats)
	}
}

func TestCapacityMarkCounted(t *testing.T) {
	ref := NewSchedulerRef(NewScheduler(nil))
	until := time.Now().Add(time.Minute)
	ref.MarkCapacityUntil(account.ProviderCodex, "a@example.com", "gpt-5", "", until)
	ref.MarkCapacityUntil(account.ProviderCodex, "a@example.com", "gpt-5", "", until)
	stats := ref.PlacementStats(nil, nil).Accounts
	if len(stats) != 1 || stats[0].CapacityMarks != 2 {
		t.Fatalf("stats = %+v, want 2 capacity marks", stats)
	}
}

func TestPlacementStatsIncludeKnownAccountsAndSessions(t *testing.T) {
	ref := NewSchedulerRef(NewScheduler(nil))
	known := []account.Account{{ID: "idle@example.com", Provider: account.ProviderClaude}}
	counts := map[string]int{ScoreKey(account.ProviderCodex, "busy@example.com"): 3}
	stats := ref.PlacementStats(counts, known).Accounts
	if len(stats) != 2 {
		t.Fatalf("stats = %+v, want the idle known account and the session holder", stats)
	}
	if stats[0].Provider != account.ProviderClaude || stats[0].AccountID != "idle@example.com" {
		t.Fatalf("first = %+v", stats[0])
	}
	if stats[1].AccountID != "busy@example.com" || stats[1].Sessions != 3 {
		t.Fatalf("second = %+v", stats[1])
	}
}

func TestPlacementStatsConcurrentNotes(t *testing.T) {
	ref := NewSchedulerRef(NewScheduler(nil))
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				ref.NotePlacement(account.ProviderCodex, "a@example.com", "")
				ref.NoteRouted(account.ProviderCodex, "a@example.com")
				ref.NoteFailover(account.ProviderCodex, "a@example.com", FailoverAuth)
				ref.NoteStickyEviction(account.ProviderCodex, "a@example.com")
				_ = ref.PlacementStats(nil, nil)
			}
		}()
	}
	wg.Wait()
	stats := ref.PlacementStats(nil, nil).Accounts[0]
	if stats.Placements != 1600 || stats.Routed != 1600 || stats.Evictions != 1600 || stats.Failovers[FailoverAuth] != 1600 {
		t.Fatalf("stats = %+v, want 1600 of each", stats)
	}
}

func TestNilSchedulerRefPlacementNotesAreSafe(t *testing.T) {
	var ref *SchedulerRef
	ref.NotePlacement(account.ProviderCodex, "a", "")
	ref.NoteStickyEviction(account.ProviderCodex, "a")
	ref.NoteFailover(account.ProviderCodex, "a", FailoverAuth)
	if got := ref.PlacementStats(nil, nil); len(got.Accounts) != 0 {
		t.Fatalf("nil ref stats = %+v", got)
	}
}
