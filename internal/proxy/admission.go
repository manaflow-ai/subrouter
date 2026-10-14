package proxy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
)

const (
	defaultAdmissionLimit = 6
	defaultAdmissionQueue = 64
	// defaultAdmissionSpillQueue is how many requests may wait behind a full
	// account before new ones spill to an account with a free slot. A short
	// queue keeps a session's prompt cache; a long one only adds latency.
	defaultAdmissionSpillQueue = 8
	// Retry-After bounds for a full queue. The queue drains in seconds, so a
	// client told to wait longer idles needlessly, and one told 0 burns its
	// retry attempts against the same full queue.
	minAdmissionRetryAfter = 1
	maxAdmissionRetryAfter = 5
)

var errAdmissionQueueFull = errors.New("upstream admission queue is full")

// errAdmissionPaused rejects a new attempt on an account whose bucket is
// paused for longer than admissionPauseWaitMax. A Claude 429 pauses the
// bucket until the quota reset, which can be hours away: queueing behind that
// held requests until the client gave up and then answered 503 for every
// request once the queue filled (2026-10-14: 1,393 rejections in 19 minutes
// on one account while other accounts had room). Rejecting fast lets the
// caller fail over to another account instead.
var errAdmissionPaused = errors.New("upstream account is paused after a rate limit")

// admissionPauseWaitMax is the longest pause a new attempt still waits out in
// the queue. A bare 429 without a reset pauses for a second; that is cheaper
// to wait than to move a session and lose its prompt cache.
const admissionPauseWaitMax = 5 * time.Second

// admissionRejected reports a limiter rejection that another account could
// serve: the queue is full or the account is paused.
func admissionRejected(err error) bool {
	return errors.Is(err, errAdmissionQueueFull) || errors.Is(err, errAdmissionPaused)
}

// subrouterReasonHeader names why subrouter itself answered a request.
const subrouterReasonHeader = "X-Subrouter-Reason"

// admissionReasonQueueFull is the X-Subrouter-Reason for a full admission
// queue, so a client or operator can tell it from an upstream 503.
const admissionReasonQueueFull = "admission_queue_full"

// admissionQueueFullError is errAdmissionQueueFull with the wait the queue
// implies, so the 503 can carry a Retry-After.
type admissionQueueFullError struct {
	retryAfterSeconds int
}

func (e admissionQueueFullError) Error() string { return errAdmissionQueueFull.Error() }

func (e admissionQueueFullError) Is(target error) bool { return target == errAdmissionQueueFull }

// admissionRetryAfterSeconds is the Retry-After for err, or 0 when err is not
// a full admission queue.
func admissionRetryAfterSeconds(err error) int {
	var full admissionQueueFullError
	if errors.As(err, &full) {
		return full.retryAfterSeconds
	}
	if errors.Is(err, errAdmissionQueueFull) {
		return minAdmissionRetryAfter
	}
	return 0
}

// admissionReasonPaused is the X-Subrouter-Reason for an account paused by a
// rate limit when no other account could take the request.
const admissionReasonPaused = "admission_paused"

// writeAdmissionRejected answers a request the admission gate turned away
// after failover found no other account: a full queue, or a long pause.
func writeAdmissionRejected(w http.ResponseWriter, err error) {
	if errors.Is(err, errAdmissionPaused) {
		w.Header().Set("Retry-After", strconv.Itoa(minAdmissionRetryAfter))
		w.Header().Set(subrouterReasonHeader, admissionReasonPaused)
		http.Error(w, errAdmissionPaused.Error(), http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Retry-After", strconv.Itoa(admissionRetryAfterSeconds(err)))
	w.Header().Set(subrouterReasonHeader, admissionReasonQueueFull)
	http.Error(w, errAdmissionQueueFull.Error(), http.StatusServiceUnavailable)
}

type admissionKey struct {
	provider accounts.Provider
	account  string
	model    string
}

type admissionWaiter struct {
	ready chan struct{}
	// rejected is set under the control mutex before ready is closed when a
	// long pause turns the waiter away instead of granting it a slot.
	rejected bool
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
	mu         sync.Mutex
	limit      int
	queueCap   int
	spillQueue int
	buckets    map[admissionKey]*admissionBucket
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
		limit:      defaultAdmissionLimit,
		queueCap:   defaultAdmissionQueue,
		spillQueue: defaultAdmissionSpillQueue,
		buckets:    make(map[admissionKey]*admissionBucket),
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
			if bucket.pausedTil.Sub(now) > admissionPauseWaitMax {
				bucket.blocked++
				a.mu.Unlock()
				return nil, errAdmissionPaused
			}
			if len(bucket.waiters) >= a.queueCap {
				bucket.blocked++
				retryAfter := a.retryAfterLocked(bucket)
				a.mu.Unlock()
				return nil, admissionQueueFullError{retryAfterSeconds: retryAfter}
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
			if waiter.rejected {
				return nil, errAdmissionPaused
			}
			return &admissionLease{control: a, key: key}, nil
		case <-ctx.Done():
			if stop != nil {
				stop()
			}
			if !a.cancelWaiter(key, waiter) {
				// A release or a long pause already took this waiter. Return the
				// slot only if it was granted; a rejected waiter holds none.
				a.mu.Lock()
				rejected := waiter.rejected
				a.mu.Unlock()
				if !rejected {
					a.release(key)
				}
			}
			return nil, ctx.Err()
		case <-timer:
			// The waiter remains in FIFO order. Re-check the pause before the
			// release path wakes it so a long Retry-After never drains a burst.
		}
	}
}

// retryAfterLocked estimates how long a full queue takes to drain: one
// second per limit-sized batch ahead, clamped to a range clients honor.
func (a *admissionControl) retryAfterLocked(bucket *admissionBucket) int {
	limit := a.limit
	if limit < 1 {
		limit = 1
	}
	seconds := (len(bucket.waiters) + limit - 1) / limit
	if seconds < minAdmissionRetryAfter {
		seconds = minAdmissionRetryAfter
	}
	if seconds > maxAdmissionRetryAfter {
		seconds = maxAdmissionRetryAfter
	}
	return seconds
}

// shouldSpill reports whether a new request for key would wait behind at
// least spillQueue others, so it is better placed on another account.
func (a *admissionControl) shouldSpill(key admissionKey) bool {
	if a == nil || key.account == "" || a.spillQueue < 1 {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	bucket := a.buckets[key]
	return bucket != nil && len(bucket.waiters) >= a.spillQueue
}

// tryAcquire admits a request on key only if it can start now. A spill
// reserves its target this way, under the same lock that counts the slot,
// so concurrent spillers see each other and spread out instead of all
// picking the one account that looked free.
func (a *admissionControl) tryAcquire(key admissionKey) (*admissionLease, bool) {
	if a == nil || key.account == "" {
		return nil, false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	bucket := a.bucketLocked(key)
	if len(bucket.waiters) > 0 || bucket.active >= a.limit || bucket.pausedTil.After(time.Now()) {
		return nil, false
	}
	bucket.active++
	bucket.served++
	return &admissionLease{control: a, key: key}, true
}

// freeLoad reports whether key would admit a request immediately, and how
// many requests it is already serving, for picking the least-loaded account.
func (a *admissionControl) freeLoad(key admissionKey) (free bool, load int) {
	if a == nil || key.account == "" {
		return false, 0
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	bucket := a.buckets[key]
	if bucket == nil {
		return true, 0
	}
	free = len(bucket.waiters) == 0 && bucket.active < a.limit && !bucket.pausedTil.After(time.Now())
	return free, bucket.active + len(bucket.waiters)
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
	// Requests already queued would otherwise sit out the whole pause. Turn
	// them away like new attempts so their callers can fail over.
	if bucket.pausedTil.Sub(time.Now()) > admissionPauseWaitMax {
		for _, waiter := range bucket.waiters {
			waiter.rejected = true
			close(waiter.ready)
		}
		bucket.blocked += uint64(len(bucket.waiters))
		bucket.waiters = nil
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
