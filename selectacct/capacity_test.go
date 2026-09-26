package selectacct

import (
	"fmt"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/account"
)

func capacityTestAccounts(n int) []account.Account {
	out := make([]account.Account, n)
	for i := range out {
		out[i] = account.Account{ID: fmt.Sprintf("acct-%d", i), Provider: account.ProviderCodex, AuthMode: account.AuthModeOAuth}
	}
	return out
}

// A capacity mark is not quota evidence: headroom, exhaustion and sticky
// retention read exactly as before, and quota marks are untouched.
func TestCapacityMarkLeavesQuotaViewUnchanged(t *testing.T) {
	ref := NewSchedulerRef(NewScheduler([]Score{
		{AccountID: "acct-0", Provider: account.ProviderCodex, Headroom: 0.8, ShortHeadroom: 0.8},
	}))
	ref.MarkCapacityUntil(account.ProviderCodex, "acct-0", "gpt-6-astra", "priority", time.Now().Add(time.Minute))
	scheduler := ref.Get().ForModel("gpt-6-astra")
	score := scheduler.ScoreFor(account.ProviderCodex, "acct-0")
	if score.Headroom != 0.8 || score.ShortHeadroom != 0.8 {
		t.Fatalf("capacity mark changed headroom: %+v", score)
	}
	if scheduler.Exhausted(account.ProviderCodex, "acct-0") || !scheduler.UsableForStickySession(account.ProviderCodex, "acct-0") {
		t.Fatal("capacity mark made the account exhausted or evicted its sticky sessions")
	}
	if _, ok := ref.ExhaustedUntilFor(account.ProviderCodex, "acct-0", "gpt-6-astra"); ok {
		t.Fatal("capacity mark recorded as a quota exhaustion mark")
	}
	marked := scheduler.WithCapacityMarks(ref.CapacityMarks(account.ProviderCodex, "gpt-6-astra", "priority"))
	if marked.CapacityEvicting(account.ProviderCodex, "acct-0") {
		t.Fatal("one capacity failure must not evict sticky sessions")
	}
	if !marked.UsableForStickySession(account.ProviderCodex, "acct-0") || marked.Exhausted(account.ProviderCodex, "acct-0") {
		t.Fatal("capacity-marked scheduler changed the quota view")
	}
}

// Marks are scoped to (model, service tier): the priority tier shedding
// says nothing about the default tier or another model. The wildcard tier
// (a websocket upgrade, whose tier arrives later) sees every tier.
func TestCapacityMarksAreScopedByModelAndTier(t *testing.T) {
	ref := NewSchedulerRef(NewScheduler(nil))
	until := time.Now().Add(time.Minute)
	ref.MarkCapacityUntil(account.ProviderCodex, "acct-0", "gpt-6-astra", "priority", until)
	if got := ref.CapacityMarks(account.ProviderCodex, "gpt-6-astra", "priority"); got[ScoreKey(account.ProviderCodex, "acct-0")] != 1 {
		t.Fatalf("priority marks = %v, want acct-0 once", got)
	}
	if got := ref.CapacityMarks(account.ProviderCodex, "gpt-6-astra", ""); len(got) != 0 {
		t.Fatalf("default-tier marks = %v, want none", got)
	}
	if got := ref.CapacityMarks(account.ProviderCodex, "gpt-6-other", "priority"); len(got) != 0 {
		t.Fatalf("other-model marks = %v, want none", got)
	}
	if got := ref.CapacityMarks(account.ProviderCodex, "gpt-6-astra", CapacityAnyTier); got[ScoreKey(account.ProviderCodex, "acct-0")] != 1 {
		t.Fatalf("any-tier marks = %v, want acct-0", got)
	}
	if CapacityPoolKey("GPT-6-Astra", "auto") != CapacityPoolKey("gpt-6-astra", "") {
		t.Fatal("auto and empty tiers must share the default pool key")
	}
}

// Consecutive failures accumulate while the mark is live; the first success
// on that account+model clears every tier's mark; an expired mark starts
// over.
func TestCapacityMarkCountsFailuresAndClearsOnSuccess(t *testing.T) {
	ref := NewSchedulerRef(NewScheduler(nil))
	until := time.Now().Add(time.Minute)
	if n := ref.MarkCapacityUntil(account.ProviderCodex, "acct-0", "m", "", until); n != 1 {
		t.Fatalf("first failure count = %d", n)
	}
	if n := ref.MarkCapacityUntil(account.ProviderCodex, "acct-0", "m", "", until); n != 2 {
		t.Fatalf("second failure count = %d", n)
	}
	ref.MarkCapacityUntil(account.ProviderCodex, "acct-0", "m", "priority", until)
	ref.MarkCapacityUntil(account.ProviderCodex, "acct-0", "other", "", until)
	if !ref.ClearCapacity(account.ProviderCodex, "acct-0", "m") {
		t.Fatal("clear reported nothing cleared")
	}
	for _, tier := range []string{"", "priority"} {
		if _, _, ok := ref.CapacityMarkFor(account.ProviderCodex, "acct-0", "m", tier); ok {
			t.Fatalf("tier %q mark survived a success", tier)
		}
	}
	if _, _, ok := ref.CapacityMarkFor(account.ProviderCodex, "acct-0", "other", ""); !ok {
		t.Fatal("success on one model cleared another model's mark")
	}
	ref.MarkCapacityUntil(account.ProviderCodex, "acct-1", "m", "", time.Now().Add(-time.Second))
	if n := ref.MarkCapacityUntil(account.ProviderCodex, "acct-1", "m", "", until); n != 1 {
		t.Fatalf("failure after an expired mark counted %d, want a fresh 1", n)
	}
}

// A capacity-marked account ranks below every unmarked account of the same
// selection tier, without being excluded: with the whole pool marked,
// placement still spreads across it instead of herding.
func TestCapacityMarkLowersRankWithoutExcluding(t *testing.T) {
	candidates := capacityTestAccounts(4)
	scores := make([]Score, len(candidates))
	for i, candidate := range candidates {
		scores[i] = Score{AccountID: candidate.ID, Provider: account.ProviderCodex, Headroom: 0.9, ShortHeadroom: 0.9}
	}
	// The marked account is also the roomiest: rank must still put it last.
	scores[0].Headroom, scores[0].ShortHeadroom = 1, 1
	base := NewScheduler(scores)
	marked := base.WithCapacityMarks(map[string]int{ScoreKey(account.ProviderCodex, "acct-0"): 1})
	picked, err := marked.PickBest(candidates)
	if err != nil || picked.ID == "acct-0" {
		t.Fatalf("PickBest = %v (%v), want an unmarked account", picked.ID, err)
	}
	for range 500 {
		picked, err := marked.Pick(candidates)
		if err != nil {
			t.Fatal(err)
		}
		if picked.ID == "acct-0" {
			t.Fatal("placement spread onto a capacity-marked account while unmarked ones were usable")
		}
	}
	only, err := marked.Pick(candidates[:1])
	if err != nil || only.ID != "acct-0" {
		t.Fatalf("sole candidate = %v (%v), want the marked account still pickable", only.ID, err)
	}
	all := map[string]int{}
	for _, candidate := range candidates {
		all[ScoreKey(account.ProviderCodex, candidate.ID)] = 1
	}
	everyMarked := base.WithCapacityMarks(all)
	seen := map[string]int{}
	for range 2000 {
		picked, err := everyMarked.Pick(candidates)
		if err != nil {
			t.Fatal(err)
		}
		seen[picked.ID]++
	}
	if len(seen) != len(candidates) {
		t.Fatalf("with every account marked, placement reached %v, want the whole pool", seen)
	}
}

// Stampede: a burst of new sessions arrives while the model sheds load on
// part of the pool. Marked accounts take none of the burst, the rest share
// it evenly rather than one of them absorbing everything.
func TestCapacityMarksSpreadStampedeAcrossUnmarkedAccounts(t *testing.T) {
	candidates := capacityTestAccounts(7)
	ref := NewSchedulerRef(NewScheduler(nil))
	until := time.Now().Add(time.Minute)
	for _, id := range []string{"acct-0", "acct-1", "acct-2"} {
		ref.MarkCapacityUntil(account.ProviderCodex, id, "gpt-6-astra", "", until)
	}
	placements := map[string]int{}
	sessions := map[string]int{}
	for range 400 {
		scheduler := ref.Get().ForModel("gpt-6-astra").
			WithSessionCounts(sessions).
			WithCapacityMarks(ref.CapacityMarks(account.ProviderCodex, "gpt-6-astra", ""))
		picked, err := scheduler.Pick(candidates)
		if err != nil {
			t.Fatal(err)
		}
		placements[picked.ID]++
		sessions[ScoreKey(account.ProviderCodex, picked.ID)]++
	}
	for _, id := range []string{"acct-0", "acct-1", "acct-2"} {
		if placements[id] != 0 {
			t.Fatalf("marked %s took %d placements: %v", id, placements[id], placements)
		}
	}
	for _, id := range []string{"acct-3", "acct-4", "acct-5", "acct-6"} {
		if share := float64(placements[id]) / 400; share < 0.15 || share > 0.35 {
			t.Fatalf("%s share %.2f, want the burst spread over the 4 unmarked accounts: %v", id, share, placements)
		}
	}
}

// Sticky retention: one capacity failure keeps the session on its account
// (its prompt cache lives there); the same account failing again evicts.
func TestCapacityStickyEvictionNeedsRepeatedFailures(t *testing.T) {
	ref := NewSchedulerRef(NewScheduler(nil))
	until := time.Now().Add(time.Minute)
	ref.MarkCapacityUntil(account.ProviderCodex, "acct-0", "m", "", until)
	scheduler := ref.Get().WithCapacityMarks(ref.CapacityMarks(account.ProviderCodex, "m", ""))
	if scheduler.CapacityEvicting(account.ProviderCodex, "acct-0") {
		t.Fatal("one failure evicted the sticky session")
	}
	ref.MarkCapacityUntil(account.ProviderCodex, "acct-0", "m", "", until)
	scheduler = ref.Get().WithCapacityMarks(ref.CapacityMarks(account.ProviderCodex, "m", ""))
	if !scheduler.CapacityEvicting(account.ProviderCodex, "acct-0") {
		t.Fatalf("%d consecutive failures did not evict", CapacityStickyEvictFailures)
	}
	// Capacity marks survive score publication (usage refresh) because they
	// live beside the snapshot, not in it.
	ref.Set(NewScheduler([]Score{{AccountID: "acct-0", Provider: account.ProviderCodex, Headroom: 1, ShortHeadroom: 1}}))
	if _, failures, ok := ref.CapacityMarkFor(account.ProviderCodex, "acct-0", "m", ""); !ok || failures != 2 {
		t.Fatalf("capacity mark after a refresh: failures=%d ok=%v", failures, ok)
	}
}
