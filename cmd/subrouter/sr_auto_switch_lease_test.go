package main

import (
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/selectacct"
)

// Several workers are alive at once whenever an upgraded-away generation is
// still draining. Only one of them may run the sweep per interval, because each
// sweep rewrites the shared active-account file.
func TestAutoSwitchLeaseAdmitsOneWorkerPerInterval(t *testing.T) {
	lease := newSRAutoSwitchLease(t.TempDir())
	const interval = 10 * time.Minute

	admitted := 0
	for worker := 0; worker < 6; worker++ {
		ok, err := lease.acquire(interval)
		if err != nil {
			t.Fatalf("worker %d: %v", worker, err)
		}
		if ok {
			admitted++
		}
	}
	if admitted != 1 {
		t.Fatalf("%d of 6 concurrent workers ran the sweep, want exactly 1", admitted)
	}
}

// Once the interval has genuinely elapsed the sweep must run again, otherwise
// account selection freezes.
func TestAutoSwitchLeaseAdmitsAgainAfterInterval(t *testing.T) {
	now := time.Unix(1770000000, 0)
	lease := newSRAutoSwitchLease(t.TempDir())
	lease.now = func() time.Time { return now }
	const interval = 10 * time.Minute

	if ok, err := lease.acquire(interval); err != nil || !ok {
		t.Fatalf("first acquire ok=%v err=%v, want true/nil", ok, err)
	}
	if ok, _ := lease.acquire(interval); ok {
		t.Fatal("second acquire in the same interval was admitted, want it to stand down")
	}

	now = now.Add(interval)
	if ok, err := lease.acquire(interval); err != nil || !ok {
		t.Fatalf("acquire after the interval ok=%v err=%v, want true/nil", ok, err)
	}
}

func TestAutoSwitchLeaseIsRaceFree(t *testing.T) {
	lease := newSRAutoSwitchLease(t.TempDir())
	const interval = time.Hour

	var mu sync.Mutex
	admitted := 0
	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if ok, _ := lease.acquire(interval); ok {
				mu.Lock()
				admitted++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if admitted == 0 {
		t.Fatal("no worker was admitted, the sweep would never run")
	}
	if admitted > 2 {
		t.Fatalf("%d workers admitted concurrently, want at most a small race window", admitted)
	}
}

// A zero lease (unset, as in tests and one-shot CLI paths) must not block the
// sweep.
func TestZeroLeaseAlwaysAdmits(t *testing.T) {
	var lease srAutoSwitchLease
	for range 3 {
		if ok, err := lease.acquire(time.Minute); err != nil || !ok {
			t.Fatalf("zero lease ok=%v err=%v, want true/nil", ok, err)
		}
	}
}

// End to end through the loop: with a shared lease, a second worker firing in
// the same interval must not perform a sweep.
func TestRunSRAutoSwitchHonoursLease(t *testing.T) {
	lease := newSRAutoSwitchLease(t.TempDir())
	var mu sync.Mutex
	sweeps := 0

	cfg := srAutoSwitchConfig{
		Interval: 50 * time.Millisecond,
		Accounts: []accounts.Account{{ID: "a@example.com", AuthMode: accounts.AuthModeOAuth}},
		Logger:   slog.New(slog.NewTextHandler(discardWriter{}, nil)),
		Lease:    lease,
		FetchScores: func(context.Context, []accounts.Account) ([]selectacct.Score, int) {
			return []selectacct.Score{{AccountID: "a@example.com", Headroom: 1, ShortHeadroom: 1, Fresh: true}}, 1
		},
		SwitchActive: func(context.Context, string) error {
			mu.Lock()
			sweeps++
			mu.Unlock()
			return nil
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 220*time.Millisecond)
	defer cancel()

	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			runSRAutoSwitch(ctx, cfg)
		}()
	}
	wg.Wait()

	mu.Lock()
	got := sweeps
	mu.Unlock()

	// Four workers over ~4 intervals would be ~16 sweeps unleased. With the
	// lease it tracks the interval, not the worker count.
	if got == 0 {
		t.Fatal("no sweeps ran at all")
	}
	if got > 8 {
		t.Fatalf("%d sweeps across 4 workers, want the lease to hold it near the per-interval count", got)
	}
}

type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }
