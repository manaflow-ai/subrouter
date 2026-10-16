package selectacct

import (
	"context"
	"fmt"
	"math"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/account"
)

// reservationTestScheduler returns two equal accounts so reservation tests
// can observe the scheduler's spreading behavior.
func reservationTestScheduler() Scheduler {
	return NewScheduler([]Score{
		{AccountID: "a@example.com", Provider: account.ProviderCodex, Headroom: 0.90, ShortHeadroom: 0.90, Fresh: true},
		{AccountID: "b@example.com", Provider: account.ProviderCodex, Headroom: 0.90, ShortHeadroom: 0.90, Fresh: true},
	})
}

// reservationTestAccounts returns the candidates used by reservation tests.
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

func reservationReplayScheduler() Scheduler {
	scores := make([]Score, 0, 7)
	for i := 0; i < 7; i++ {
		id := fmt.Sprintf("reset-%d@example.com", i)
		scores = append(scores, Score{
			AccountID: id, Provider: account.ProviderCodex,
			Headroom: 0.90, ShortHeadroom: 0.90, Fresh: true,
		})
	}
	return NewScheduler(scores)
}

func reservationReplayAccounts() []account.Account {
	accounts := make([]account.Account, 0, 7)
	for i := 0; i < 7; i++ {
		accounts = append(accounts, account.Account{
			ID: fmt.Sprintf("reset-%d@example.com", i), Provider: account.ProviderCodex,
			AuthMode: account.AuthModeOAuth,
		})
	}
	return accounts
}

// A reset makes all currently pinned sessions eligible at once. This replay
// keeps the 50-session burst concurrent and checks the property that matters:
// reservations spread the burst before any upstream attempt is committed, so
// a single account reset can move only its bounded share of the sessions.
func TestPickAndReserveConcurrentResetReplayBoundsConcentration(t *testing.T) {
	previous := spreadRandFloat
	var draws uint64
	spreadRandFloat = func() float64 {
		// A deterministic low-discrepancy sequence keeps this test reproducible
		// while still exercising every part of the weighted spread interval.
		const step = 0.6180339887498949
		draw := atomic.AddUint64(&draws, 1)
		return math.Mod(float64(draw)*step, 1)
	}
	defer func() { spreadRandFloat = previous }()

	ref := NewSchedulerRef(reservationReplayScheduler())
	snapshot := ref.Get()
	candidates := reservationReplayAccounts()
	const sessions = 50

	start := make(chan struct{})
	var wg sync.WaitGroup
	var mu sync.Mutex
	counts := make(map[string]int)
	reservations := make([]*PlacementReservation, 0, sessions)
	for i := 0; i < sessions; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			picked, reservation, err := ref.PickAndReserve(snapshot, candidates)
			if err != nil {
				t.Errorf("pick: %v", err)
				return
			}
			mu.Lock()
			counts[picked.ID]++
			reservations = append(reservations, reservation)
			mu.Unlock()
		}()
	}
	close(start)
	wg.Wait()

	if len(reservations) != sessions {
		t.Fatalf("reservations = %d, want %d", len(reservations), sessions)
	}
	max := 0
	for id, count := range counts {
		if count > max {
			max = count
		}
		if count == 0 {
			t.Errorf("%s received no reset replay placements", id)
		}
	}
	if max > 12 {
		t.Fatalf("reset replay concentrated %d/%d sessions on one account: %v", max, sessions, counts)
	}
	if got := totalReservationDebits(ref.LiveDebits()); got != sessions {
		t.Fatalf("pending reservation debits = %d, want %d", got, sessions)
	}

	// Half the upstream preparations fail or are canceled. Releases must not
	// become routed debits, while successful preparations commit exactly once.
	for i, reservation := range reservations {
		if i%2 == 0 {
			reservation.Release()
		} else {
			reservation.Commit()
		}
	}
	if got := totalReservationDebits(ref.LiveDebits()); got != sessions/2 {
		t.Fatalf("post-failure live debits = %d, want %d", got, sessions/2)
	}

	// If the account holding the largest share resets, this is the largest
	// simultaneous move the router can cause from this burst.
	if max > 12 {
		t.Fatalf("mass move after account reset would be %d sessions", max)
	}
}

func totalReservationDebits(debits map[string]int) int {
	total := 0
	for _, count := range debits {
		total += count
	}
	return total
}

func TestPickAndReserveContextCancellationReleases(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	ref := NewSchedulerRef(reservationTestScheduler())
	_, reservation, err := ref.PickAndReserveContext(ctx, ref.Get(), reservationTestAccounts())
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if len(ref.LiveDebits()) == 0 {
			return
		}
		time.Sleep(time.Millisecond)
	}
	reservation.Release()
	t.Fatalf("context cancellation left reservation live: %v", ref.LiveDebits())
}
