package proxy

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
)

func TestAdmissionRecoveryLimitsAndDeadlines(t *testing.T) {
	control := testAdmissionControl(6, 64)
	start := time.Now().Add(time.Minute)
	bucket := &admissionBucket{pausedTil: start, recoverSlowly: true}
	for _, tc := range []struct {
		elapsed time.Duration
		limit   int
		next    time.Duration
	}{
		{-time.Second, 0, 0},
		{0, 1, time.Second},
		{time.Second - time.Nanosecond, 1, time.Second},
		{time.Second, 2, 2 * time.Second},
		{2 * time.Second, 4, 3 * time.Second},
		{3 * time.Second, 6, -1},
		{24 * time.Hour, 6, -1},
	} {
		limit, next := control.admissionLimitLocked(bucket, start.Add(tc.elapsed))
		if limit != tc.limit {
			t.Errorf("at %v: limit=%d, want %d", tc.elapsed, limit, tc.limit)
		}
		if tc.next < 0 {
			if !next.IsZero() {
				t.Errorf("at %v: unexpected timer %v", tc.elapsed, next)
			}
		} else if !next.Equal(start.Add(tc.next)) {
			t.Errorf("at %v: next=%v, want %v", tc.elapsed, next, start.Add(tc.next))
		}
	}
	// Small configured limits must never be raised by the recovery ramp.
	for _, configured := range []int{1, 2, 3, 4, 6} {
		control.limit = configured
		limit, _ := control.admissionLimitLocked(bucket, start.Add(3*time.Second))
		if limit != configured {
			t.Errorf("configured=%d: recovered limit=%d", configured, limit)
		}
	}
}

func TestAdmissionRecoveryConcurrentReservationsAndIsolation(t *testing.T) {
	control := testAdmissionControl(6, 64)
	key := admissionKeyFor(accounts.ProviderClaude, "recovering", "claude-opus")
	now := time.Now()
	control.observe(key, http.StatusTooManyRequests, http.Header{}, now.Add(-2*time.Second))
	// The one-second pause ended one second ago: only two spill slots may
	// be reserved, even if every caller saw the account as available.
	var wg sync.WaitGroup
	leases := make(chan *admissionLease, 24)
	for range 24 {
		wg.Go(func() {
			if lease, ok := control.tryAcquire(key, admissionSessionKey{}); ok {
				leases <- lease
			}
		})
	}
	wg.Wait()
	close(leases)
	if len(leases) != 2 {
		t.Fatalf("reserved %d slots in recovery, want 2", len(leases))
	}
	if free, load := control.freeLoad(key); free || load != 2 {
		t.Fatalf("free=%v load=%d, want false/2 during recovery", free, load)
	}
	for _, other := range []admissionKey{
		admissionKeyFor(accounts.ProviderClaude, "healthy", "claude-opus"),
		admissionKeyFor(accounts.ProviderClaude, "recovering", "claude-sonnet"),
		admissionKeyFor(accounts.ProviderCodex, "recovering", "claude-opus"),
	} {
		for range control.limit {
			lease, ok := control.tryAcquire(other, admissionSessionKey{})
			if !ok {
				t.Fatalf("recovery leaked to independent bucket %+v", other)
			}
			defer lease.release()
		}
	}
	for lease := range leases {
		lease.release()
	}
}

func TestAdmissionRecoveryQueuedRequestsWakeWithoutResponseRelease(t *testing.T) {
	control := testAdmissionControl(4, 64)
	key := admissionKeyFor(accounts.ProviderClaude, "account", "claude-opus")
	// A nearly expired pause keeps this an actual timer-driven recovery.
	start := time.Now().Add(30 * time.Millisecond)
	control.pauseWithRecovery(key, start, true)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	type result struct {
		lease *admissionLease
		at    time.Time
		err   error
	}
	ready := make(chan result, 4)
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			lease, err := control.acquire(ctx, key)
			ready <- result{lease, time.Now(), err}
		})
	}
	t.Cleanup(func() {
		cancel()
		wg.Wait()
	})
	// Hold every lease: growing admission capacity must wake queued callers
	// even if the first request is still streaming and never releases a slot.
	for i := range 4 {
		select {
		case got := <-ready:
			if got.err != nil {
				t.Fatal(got.err)
			}
			defer got.lease.release()
			earliest := start
			if i >= 1 {
				earliest = start.Add(time.Second)
			}
			if i >= 2 {
				earliest = start.Add(2 * time.Second)
			}
			if got.at.Before(earliest) {
				t.Fatalf("request %d admitted %v before its ramp boundary", i, earliest.Sub(got.at))
			}
		case <-ctx.Done():
			t.Fatalf("queued recovery stalled without a response release: %v", control.snapshot())
		}
	}
}

func TestAdmissionRecoveryReThrottlesAndKeepsQuotaPolicy(t *testing.T) {
	control := testAdmissionControl(6, 64)
	key := admissionKeyFor(accounts.ProviderClaude, "account", "claude-opus")
	now := time.Now()
	control.observe(key, http.StatusTooManyRequests, http.Header{}, now.Add(-2*time.Second))
	control.observe(key, http.StatusTooManyRequests, http.Header{"Retry-After": {"3"}}, now)
	control.mu.Lock()
	bucket := control.buckets[key]
	limit, next := control.admissionLimitLocked(bucket, now)
	control.mu.Unlock()
	if limit != 0 || !next.Equal(now.Add(3*time.Second)) {
		t.Fatalf("new throttle did not restart pause: limit=%d next=%v", limit, next)
	}
	// A later, shorter hint cannot shorten the cooldown or its recovery ramp.
	control.observe(key, http.StatusTooManyRequests, http.Header{}, now)
	control.mu.Lock()
	limit, next = control.admissionLimitLocked(bucket, now.Add(3*time.Second))
	control.mu.Unlock()
	if limit != 1 || !next.Equal(now.Add(4*time.Second)) {
		t.Fatalf("short hint reset recovery before longer pause: limit=%d next=%v", limit, next)
	}
	header := http.Header{}
	header.Set("Anthropic-Ratelimit-Unified-Status", "rejected")
	control.observe(key, http.StatusTooManyRequests, header, now)
	control.mu.Lock()
	limit, next = control.admissionLimitLocked(bucket, bucket.pausedTil)
	control.mu.Unlock()
	if limit != control.limit || !next.IsZero() {
		t.Fatalf("quota rejection inherited transient recovery: limit=%d next=%v", limit, next)
	}
	// Concurrent in-flight requests can return a transient hint after a
	// quota rejection. The longer authoritative pause must retain its policy.
	control.observe(key, http.StatusTooManyRequests, http.Header{}, now)
	control.mu.Lock()
	limit, next = control.admissionLimitLocked(bucket, bucket.pausedTil)
	control.mu.Unlock()
	if limit != control.limit || !next.IsZero() {
		t.Fatalf("late transient hint changed quota recovery: limit=%d next=%v", limit, next)
	}
}

func TestAdmissionRecoveryCancelledWaiterDoesNotLeak(t *testing.T) {
	control := testAdmissionControl(6, 64)
	key := admissionKeyFor(accounts.ProviderClaude, "account", "claude-opus")
	control.pauseWithRecovery(key, time.Now().Add(time.Second), true)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if lease, err := control.acquire(ctx, key); lease != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled acquire: lease=%v err=%v", lease, err)
	}
	snapshot := control.snapshot()
	if len(snapshot.Accounts) != 1 || snapshot.Accounts[0].Active != 0 || snapshot.Accounts[0].Queued != 0 {
		t.Fatalf("cancelled waiter leaked: %+v", snapshot)
	}
	if snapshot.Accounts[0].EffectiveLimit != 0 {
		t.Fatalf("paused bucket reports available slots: %+v", snapshot)
	}
}

func TestAdmissionRecoveryCountsStreamsStartedBeforeThrottle(t *testing.T) {
	control := testAdmissionControl(6, 64)
	key := admissionKeyFor(accounts.ProviderClaude, "account", "claude-opus")
	var streams []*admissionLease
	for range 4 {
		lease, ok := control.tryAcquire(key, admissionSessionKey{})
		if !ok {
			t.Fatal("healthy bucket did not admit its normal concurrency")
		}
		streams = append(streams, lease)
		defer lease.release()
	}
	control.observe(key, http.StatusTooManyRequests, http.Header{}, time.Now().Add(-time.Second))
	for _, stream := range streams {
		if lease, ok := control.tryAcquire(key, admissionSessionKey{}); ok {
			lease.release()
			t.Fatal("recovery ignored streams started before the throttle")
		}
		stream.release()
	}
	lease, ok := control.tryAcquire(key, admissionSessionKey{})
	if !ok {
		t.Fatal("released old streams left recovery stuck")
	}
	lease.release()
}
