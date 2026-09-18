package selectacct

import (
	"fmt"
	"testing"

	"github.com/manaflow-ai/subrouter/account"
)

func accounts(n int) []account.Account {
	out := make([]account.Account, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, account.Account{ID: fmt.Sprintf("acct-%d", i), Provider: account.ProviderCodex})
	}
	return out
}

// evenScheduler gives every account identical headroom, so ordering is decided
// purely by the tiebreaks under test.
func evenScheduler(n int, headroom float64) Scheduler {
	scores := make([]Score, 0, n)
	for i := 0; i < n; i++ {
		scores = append(scores, Score{
			AccountID: fmt.Sprintf("acct-%d", i),
			Provider:  account.ProviderCodex,
			Headroom:  headroom,
			Fresh:     true,
		})
	}
	return NewScheduler(scores)
}

func TestArgmaxHerdsWithoutAffinity(t *testing.T) {
	// The behaviour this change exists to fix: with no affinity key, every
	// client picks the same account.
	s := evenScheduler(5, 0.9)
	// Give one account a hair more headroom, as a real usage fetch would.
	s = s.WithScore(Score{AccountID: "acct-3", Provider: account.ProviderCodex, Headroom: 0.91, Fresh: true})

	for i := 0; i < 50; i++ {
		got, err := s.Pick(accounts(5))
		if err != nil {
			t.Fatal(err)
		}
		if got.ID != "acct-3" {
			t.Fatalf("client %d picked %s, expected every client to herd onto acct-3", i, got.ID)
		}
	}
}

func TestAffinitySpreadsClientsAcrossAccounts(t *testing.T) {
	const clients, numAccounts = 600, 6
	s := evenScheduler(numAccounts, 0.9)

	counts := map[string]int{}
	for i := 0; i < clients; i++ {
		picked, err := s.WithAffinity(fmt.Sprintf("client-%d", i)).Pick(accounts(numAccounts))
		if err != nil {
			t.Fatal(err)
		}
		counts[picked.ID]++
	}

	if len(counts) != numAccounts {
		t.Fatalf("expected all %d accounts used, got %d: %v", numAccounts, len(counts), counts)
	}
	// Rendezvous hashing is uniform in expectation; allow a generous band so
	// this never flakes while still catching a collapse onto one account.
	expected := clients / numAccounts
	for id, n := range counts {
		if n < expected/2 || n > expected*2 {
			t.Errorf("account %s got %d picks, expected near %d (%v)", id, n, expected, counts)
		}
	}
}

func TestAffinityIsStableForOneClient(t *testing.T) {
	s := evenScheduler(6, 0.9).WithAffinity("client-stable")
	first, err := s.Pick(accounts(6))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		got, err := s.Pick(accounts(6))
		if err != nil {
			t.Fatal(err)
		}
		if got.ID != first.ID {
			t.Fatalf("affinity unstable: %s then %s", first.ID, got.ID)
		}
	}
}

func TestAffinityReassignsOnlyDisplacedClientsWhenAccountRemoved(t *testing.T) {
	const clients = 600
	before := evenScheduler(6, 0.9)
	after := evenScheduler(6, 0.9)

	full := accounts(6)
	reduced := accounts(6)[:5] // drop acct-5

	moved, boundToDropped := 0, 0
	for i := 0; i < clients; i++ {
		key := fmt.Sprintf("client-%d", i)
		a, err := before.WithAffinity(key).Pick(full)
		if err != nil {
			t.Fatal(err)
		}
		b, err := after.WithAffinity(key).Pick(reduced)
		if err != nil {
			t.Fatal(err)
		}
		if a.ID == "acct-5" {
			boundToDropped++
		}
		if a.ID != b.ID {
			moved++
		}
	}
	// Only clients bound to the removed account should move. That is the
	// property that makes adding capacity cheap.
	if moved != boundToDropped {
		t.Fatalf("removing one account moved %d clients but only %d were bound to it", moved, boundToDropped)
	}
}

func TestAffinityNeverPrefersAMateriallyEmptierAccount(t *testing.T) {
	// acct-0 is nearly full, the rest are nearly empty. Affinity must not pull
	// a client onto an empty account just because the hash likes it.
	scores := []Score{{AccountID: "acct-0", Provider: account.ProviderCodex, Headroom: 0.95, Fresh: true}}
	for i := 1; i < 6; i++ {
		scores = append(scores, Score{
			AccountID: fmt.Sprintf("acct-%d", i), Provider: account.ProviderCodex,
			Headroom: 0.45, Fresh: true,
		})
	}
	s := NewScheduler(scores)
	for i := 0; i < 200; i++ {
		got, err := s.WithAffinity(fmt.Sprintf("client-%d", i)).Pick(accounts(6))
		if err != nil {
			t.Fatal(err)
		}
		if got.ID != "acct-0" {
			t.Fatalf("client %d picked %s over the much fuller acct-0", i, got.ID)
		}
	}
}

func TestHeadroomBandBucketsContinuousValues(t *testing.T) {
	if headroomBand(0.91) != headroomBand(0.95) {
		t.Fatal("0.91 and 0.95 should share a band")
	}
	if headroomBand(0.95) == headroomBand(0.45) {
		t.Fatal("0.95 and 0.45 must not share a band")
	}
	if headroomBand(-1) != 0 {
		t.Fatal("negative headroom should clamp to the lowest band")
	}
}

func TestEmptyAffinityKeyPreservesArgmax(t *testing.T) {
	s := evenScheduler(4, 0.5).WithScore(Score{
		AccountID: "acct-2", Provider: account.ProviderCodex, Headroom: 0.99, Fresh: true,
	})
	got, err := s.WithAffinity("").Pick(accounts(4))
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "acct-2" {
		t.Fatalf("empty affinity key should keep plain argmax, got %s", got.ID)
	}
}
