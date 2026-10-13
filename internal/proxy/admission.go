package proxy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
)

const (
	defaultAdmissionLimit = 6
	defaultAdmissionQueue = 64
)

var errAdmissionQueueFull = errors.New("upstream admission queue is full")

type admissionKey struct {
	provider accounts.Provider
	account  string
	model    string
}

type admissionWaiter struct {
	ready chan struct{}
}

type admissionBucket struct {
	active    int
	waiters   []*admissionWaiter
	pausedTil time.Time
	served    uint64
	queued    uint64
	blocked   uint64
	throttled uint64
}

// admissionControl is the controller-wide physical-attempt gate. It is
// intentionally process-local: a controller owns one credential pool and all
// clients enter through this process. The lease spans response-body lifetime,
// just like SchedulerRef's in-flight count.
type admissionControl struct {
	mu       sync.Mutex
	limit    int
	queueCap int
	buckets  map[admissionKey]*admissionBucket
}

type admissionLease struct {
	control *admissionControl
	key     admissionKey
	once    sync.Once
}

type admissionKeySnapshot struct {
	Provider    accounts.Provider `json:"provider"`
	AccountHash string            `json:"account_hash"`
	Model       string            `json:"model"`
	Limit       int               `json:"limit"`
	Active      int               `json:"active"`
	Queued      int               `json:"queued"`
	Served      uint64            `json:"served"`
	Blocked     uint64            `json:"blocked"`
	Throttled   uint64            `json:"throttled"`
	PausedUntil string            `json:"paused_until,omitempty"`
}

type admissionSnapshot struct {
	Limit    int                    `json:"limit"`
	Accounts []admissionKeySnapshot `json:"accounts,omitempty"`
}

func newAdmissionControl() *admissionControl {
	return &admissionControl{
		limit:    defaultAdmissionLimit,
		queueCap: defaultAdmissionQueue,
		buckets:  make(map[admissionKey]*admissionBucket),
	}
}

func (a *admissionControl) bucketLocked(key admissionKey) *admissionBucket {
	bucket := a.buckets[key]
	if bucket == nil {
		bucket = &admissionBucket{}
		a.buckets[key] = bucket
	}
	return bucket
}

func (a *admissionControl) acquire(ctx context.Context, key admissionKey) (*admissionLease, error) {
	if a == nil || key.account == "" {
		return nil, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	var waiter *admissionWaiter
	for {
		now := time.Now()
		a.mu.Lock()
		bucket := a.bucketLocked(key)
		if waiter == nil && len(bucket.waiters) == 0 && bucket.active < a.limit && !bucket.pausedTil.After(now) {
			bucket.active++
			bucket.served++
			a.mu.Unlock()
			return &admissionLease{control: a, key: key}, nil
		}
		if waiter != nil && len(bucket.waiters) > 0 && bucket.waiters[0] == waiter &&
			bucket.active < a.limit && !bucket.pausedTil.After(now) {
			copy(bucket.waiters, bucket.waiters[1:])
			bucket.waiters = bucket.waiters[:len(bucket.waiters)-1]
			bucket.active++
			bucket.served++
			a.mu.Unlock()
			return &admissionLease{control: a, key: key}, nil
		}
		if waiter == nil {
			if len(bucket.waiters) >= a.queueCap {
				bucket.blocked++
				a.mu.Unlock()
				return nil, errAdmissionQueueFull
			}
			waiter = &admissionWaiter{ready: make(chan struct{})}
			bucket.waiters = append(bucket.waiters, waiter)
			bucket.queued++
		}
		pausedUntil := bucket.pausedTil
		a.mu.Unlock()

		var timer <-chan time.Time
		var stop func()
		if pausedUntil.After(now) {
			t := time.NewTimer(time.Until(pausedUntil))
			timer = t.C
			stop = func() { t.Stop() }
		}
		select {
		case <-waiter.ready:
			if stop != nil {
				stop()
			}
			return &admissionLease{control: a, key: key}, nil
		case <-ctx.Done():
			if stop != nil {
				stop()
			}
			if !a.cancelWaiter(key, waiter) {
				// A release won the race and already granted this waiter.
				// Return that slot before reporting cancellation.
				a.release(key)
			}
			return nil, ctx.Err()
		case <-timer:
			// The waiter remains in FIFO order. Re-check the pause before the
			// release path wakes it so a long Retry-After never drains a burst.
		}
	}
}

func (a *admissionControl) cancelWaiter(key admissionKey, waiter *admissionWaiter) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	bucket := a.buckets[key]
	if bucket == nil {
		return false
	}
	for i, candidate := range bucket.waiters {
		if candidate == waiter {
			copy(bucket.waiters[i:], bucket.waiters[i+1:])
			bucket.waiters = bucket.waiters[:len(bucket.waiters)-1]
			return true
		}
	}
	return false
}

func (a *admissionControl) release(key admissionKey) {
	a.mu.Lock()
	defer a.mu.Unlock()
	bucket := a.buckets[key]
	if bucket == nil {
		return
	}
	if bucket.active > 0 {
		bucket.active--
	}
	a.wakeLocked(key, bucket, time.Now())
}

func (a *admissionControl) wakeLocked(_ admissionKey, bucket *admissionBucket, now time.Time) {
	if bucket.pausedTil.After(now) || bucket.active >= a.limit || len(bucket.waiters) == 0 {
		return
	}
	waiter := bucket.waiters[0]
	copy(bucket.waiters, bucket.waiters[1:])
	bucket.waiters = bucket.waiters[:len(bucket.waiters)-1]
	bucket.active++
	bucket.served++
	close(waiter.ready)
}

func (a *admissionControl) pause(key admissionKey, until time.Time) {
	if a == nil || until.IsZero() {
		return
	}
	a.mu.Lock()
	bucket := a.bucketLocked(key)
	bucket.throttled++
	if until.After(bucket.pausedTil) {
		bucket.pausedTil = until
	}
	a.mu.Unlock()
}

func (a *admissionControl) observe(key admissionKey, status int, header http.Header, now time.Time) {
	if a == nil || status != http.StatusTooManyRequests {
		return
	}
	until := retryAfterExpiry(header, now)
	if key.provider == accounts.ProviderClaude {
		until = claudeExhaustionExpiryForPool(header, now, key.model)
	}
	if until.IsZero() {
		until = now.Add(time.Second)
	}
	a.pause(key, until)
}

func (l *admissionLease) release() {
	if l == nil || l.control == nil {
		return
	}
	l.once.Do(func() { l.control.release(l.key) })
}

func (a *admissionControl) snapshot() admissionSnapshot {
	if a == nil {
		return admissionSnapshot{}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	out := admissionSnapshot{Limit: a.limit}
	for key, bucket := range a.buckets {
		digest := sha256.Sum256([]byte(key.account))
		entry := admissionKeySnapshot{
			Provider:    key.provider,
			AccountHash: "sha256:" + hex.EncodeToString(digest[:8]),
			Model:       key.model,
			Limit:       a.limit,
			Active:      bucket.active,
			Queued:      len(bucket.waiters),
			Served:      bucket.served,
			Blocked:     bucket.blocked,
			Throttled:   bucket.throttled,
		}
		if bucket.pausedTil.After(time.Now()) {
			entry.PausedUntil = bucket.pausedTil.UTC().Format(time.RFC3339)
		}
		out.Accounts = append(out.Accounts, entry)
	}
	return out
}

func admissionKeyFor(provider accounts.Provider, accountID, model string) admissionKey {
	if model == "" {
		model = "default"
	}
	return admissionKey{provider: provider, account: accountID, model: model}
}
