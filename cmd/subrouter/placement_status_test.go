package main

import (
	"strings"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/account"
	"github.com/manaflow-ai/subrouter/selectacct"
)

func TestPlacementStatusLinesQuietWithoutActivity(t *testing.T) {
	snapshot := selectacct.PlacementStatsSnapshot{Accounts: []selectacct.AccountPlacementStats{{Provider: account.ProviderCodex, AccountID: "idle@example.com"}}}
	if lines := placementStatusLines(snapshot, time.Now()); len(lines) != 0 {
		t.Fatalf("lines = %q, want nothing before any activity", lines)
	}
}

func TestPlacementStatusLinesShowHerdAndCounters(t *testing.T) {
	now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	snapshot := selectacct.PlacementStatsSnapshot{
		Since: now.Add(-2 * time.Hour),
		Accounts: []selectacct.AccountPlacementStats{
			{Provider: account.ProviderCodex, AccountID: "herd@example.com", Placements: 12, Routed: 340, Evictions: 1,
				Failovers: map[selectacct.FailoverReason]uint64{selectacct.FailoverUsageLimit: 2, selectacct.FailoverCapacity: 5}, CapacityMarks: 7, Sessions: 9},
			{Provider: account.ProviderCodex, AccountID: "idle@example.com"},
		},
		Pools: []selectacct.PoolPlacementStats{{Provider: account.ProviderCodex, Placements: 12, BusiestAccountID: "herd@example.com", BusiestShare: 1, Accounts: 1}},
	}
	out := strings.Join(placementStatusLines(snapshot, now), "\n")
	for _, want := range []string{"since", "(2h0m0s ago)", "codex last hour: 12 placements across 1 accounts, busiest herd@example.com 100%", "2/0/5"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "idle@example.com") {
		t.Fatalf("idle account row should be omitted:\n%s", out)
	}
}
