package selectacct

import (
	"sync"
	"testing"

	"github.com/manaflow-ai/subrouter/account"
)

func reservationTestScheduler() Scheduler {
	return NewScheduler([]Score{
		{AccountID: "a@example.com", Provider: account.ProviderCodex, Headroom: 0.90, ShortHeadroom: 0.90, Fresh: true},
		{AccountID: "b@example.com", Provider: account.ProviderCodex, Headroom: 0.90, ShortHeadroom: 0.90, Fresh: true},
	})
}

func reservationTestAccounts() []account.Account {
	return []account.Account{
		{ID: "a@example.com", Provider: account.ProviderCodex, AuthMode: account.AuthModeOAuth},
		{ID: "b@example.com", Provider: account.ProviderCodex, AuthMode: account.AuthModeOAuth},
	}
}

// Reusing one caller snapshot is intentional: this is the race that occurs
// when concurrent request handlers call Pick and only update NoteRouted after
// selection. PickAndReserve must install each live debit before the next pick.
func TestPickAndReserveUsesLiveStateFromTheRef(t *testing.T) {
	previous := spreadRandFloat
	spreadRandFloat = func() float64 { return 0.49 }
	defer func() { spreadRandFloat = previous }()

	ref := NewSchedulerRef(reservationTestScheduler())
	snapshot := ref.Get()
	candidates := reservationTestAccounts()

	first, firstReservation, err := ref.PickAndReserve(snapshot, candidates)
	if err != nil {
		t.Fatal(err)
	}
	second, secondReservation, err := ref.PickAndReserve(snapshot, candidates)
	if err != nil {
		t.Fatal(err)
	}
	if first.ID == second.ID {
		t.Fatalf("two placements used %q; the second pick did not see the first reservation", first.ID)
	}
	firstReservation.Commit()
	secondReservation.Commit()
	debits := ref.LiveDebits()
	if got := debits[ScoreKey(account.ProviderCodex, first.ID)]; got != 1 {
		t.Fatalf("first account live debit = %d, want 1", got)
	}
	if got := debits[ScoreKey(account.ProviderCodex, second.ID)]; got != 1 {
		t.Fatalf("second account live debit = %d, want 1", got)
	}
}

// The ref lock must cover both selection and reservation. With a fixed draw,
// an atomic sequence alternates between the two equal accounts; a non-atomic
// Pick followed by NoteRouted would let every goroutine select the same stale
// headroom winner before any debit landed.
func TestPickAndReserveConcurrentPlacementsSeeEachOther(t *testing.T) {
	previous := spreadRandFloat
	spreadRandFloat = func() float64 { return 0.49 }
	defer func() { spreadRandFloat = previous }()

	ref := NewSchedulerRef(reservationTestScheduler())
	snapshot := ref.Get()
	candidates := reservationTestAccounts()

	const placements = 32
	counts := make(map[string]int)
	reservations := make([]*PlacementReservation, 0, placements)
	var countsMu sync.Mutex
	var wg sync.WaitGroup
	for i := 0; i < placements; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			picked, reservation, err := ref.PickAndReserve(snapshot, candidates)
			if err != nil {
				t.Errorf("pick: %v", err)
				return
			}
			countsMu.Lock()
			counts[picked.ID]++
			reservations = append(reservations, reservation)
			countsMu.Unlock()
		}()
	}
	wg.Wait()

	if counts["a@example.com"] == 0 || counts["b@example.com"] == 0 {
		t.Fatalf("concurrent placements did not spread: %v", counts)
	}
	for _, reservation := range reservations {
		reservation.Commit()
	}
	debits := ref.LiveDebits()
	if got := debits[ScoreKey(account.ProviderCodex, "a@example.com")] + debits[ScoreKey(account.ProviderCodex, "b@example.com")]; got != placements {
		t.Fatalf("live debit total = %d, want %d", got, placements)
	}
}

func TestPickAndReserveDoesNotReserveOnEmptyPool(t *testing.T) {
	ref := NewSchedulerRef(reservationTestScheduler())
	if _, reservation, err := ref.PickAndReserve(ref.Get(), nil); err == nil || reservation != nil {
		t.Fatal("empty pool pick unexpectedly succeeded")
	}
	if got := ref.LiveDebits(); len(got) != 0 {
		t.Fatalf("empty pool created live debits: %v", got)
	}
}

func TestPlacementReservationCommitAndReleaseAreIdempotent(t *testing.T) {
	ref := NewSchedulerRef(reservationTestScheduler())
	picked, committed, err := ref.PickAndReserve(ref.Get(), reservationTestAccounts())
	if err != nil {
		t.Fatal(err)
	}
	committed.Commit()
	committed.Commit()
	if got := ref.LiveDebits()[ScoreKey(account.ProviderCodex, picked.ID)]; got != 1 {
		t.Fatalf("committed debit = %d, want 1", got)
	}
	stats := ref.PlacementStats(nil, nil)
	if len(stats.Accounts) != 1 || stats.Accounts[0].Routed != 1 {
		t.Fatalf("committed route stats = %+v, want one routed request", stats.Accounts)
	}

	_, released, err := ref.PickAndReserve(ref.Get(), reservationTestAccounts())
	if err != nil {
		t.Fatal(err)
	}
	released.Release()
	released.Release()
	if got := ref.LiveDebits()[ScoreKey(account.ProviderCodex, picked.ID)]; got != 1 {
		t.Fatalf("released reservation changed committed debit to %d, want 1", got)
	}
}
