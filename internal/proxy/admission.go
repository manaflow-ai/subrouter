package proxy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"sort"
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
	who      admissionSessionKey
}

// admissionSessionKey names the client session a physical attempt belongs
// to. A session fanning out (subagents, workflows) shares one id, so its
// attempts are the ones that pile onto one sticky account.
type admissionSessionKey struct {
	agent   string
	session string
}

// admissionSession is one session's share of the admission gates.
type admissionSession struct {
	active   int
	queued   int
	peak     int
	served   uint64
	blocked  uint64
	spilled  uint64
	account  string
	lastSeen time.Time
	// overflowFrom and overflowStreak count consecutive queue-full overflows
	// off one sticky account; an admitted attempt there resets them.
	overflowFrom   string
	overflowStreak int
}

// admissionOverflowMoveAfter is how many queue-full overflows in a row move a
// session off its sticky account. One overflow keeps the session, and its
// prompt cache, where it is; a sticky account that stays full would instead
// make every turn re-write the cache on a random account.
const admissionOverflowMoveAfter = 3

const (
	// admissionSessionIdle is how long an idle session's counters are kept,
	// so a fan-out that just finished is still visible in the snapshot.
	admissionSessionIdle = 15 * time.Minute
	// admissionSessionMax bounds the per-session table. Past it, idle
	// entries are dropped first.
	admissionSessionMax = 1024
	// admissionSessionTop is how many sessions the snapshot lists.
	admissionSessionTop = 20
)

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
	sessions   map[admissionSessionKey]*admissionSession
}

type admissionLease struct {
	control *admissionControl
	key     admissionKey
	who     admissionSessionKey
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

// admissionSessionSnapshot is one session's fan-out: how many physical
// attempts it holds and queues across every account right now, its peak, and
// the account it last used. The traffic endpoint is unauthenticated, so it
// carries only a session id prefix: enough to find the session (and the
// client address) in the server log, not enough to send requests as it.
type admissionSessionSnapshot struct {
	Agent         string `json:"agent"`
	SessionPrefix string `json:"session_prefix"`
	Active        int    `json:"active"`
	Queued        int    `json:"queued"`
	Peak          int    `json:"peak"`
	Served        uint64 `json:"served"`
	Blocked       uint64 `json:"blocked"`
	Spilled       uint64 `json:"spilled"`
	AccountHash   string `json:"account_hash,omitempty"`
	LastSeen      string `json:"last_seen"`
}

type admissionSnapshot struct {
	Limit    int                        `json:"limit"`
	Accounts []admissionKeySnapshot     `json:"accounts,omitempty"`
	Sessions []admissionSessionSnapshot `json:"sessions,omitempty"`
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

// sessionLocked returns who's counters, creating them, or nil for a request
// without a session.
func (a *admissionControl) sessionLocked(who admissionSessionKey, now time.Time) *admissionSession {
	if who.session == "" {
		return nil
	}
	if a.sessions == nil {
		a.sessions = make(map[admissionSessionKey]*admissionSession)
	}
	entry := a.sessions[who]
	if entry == nil {
		if len(a.sessions) >= admissionSessionMax {
			a.pruneSessionsLocked(now)
		}
		entry = &admissionSession{}
		a.sessions[who] = entry
	}
	entry.lastSeen = now
	return entry
}

// pruneSessionsLocked drops sessions idle past admissionSessionIdle, and if
// the table is still full, the longest-idle sessions holding nothing.
func (a *admissionControl) pruneSessionsLocked(now time.Time) {
	var oldest admissionSessionKey
	var oldestSeen time.Time
	for who, entry := range a.sessions {
		if entry.active > 0 || entry.queued > 0 {
			continue
		}
		if now.Sub(entry.lastSeen) > admissionSessionIdle {
			delete(a.sessions, who)
			continue
		}
		if oldestSeen.IsZero() || entry.lastSeen.Before(oldestSeen) {
			oldest, oldestSeen = who, entry.lastSeen
		}
	}
	if len(a.sessions) >= admissionSessionMax && !oldestSeen.IsZero() {
		delete(a.sessions, oldest)
	}
}

// grantSessionLocked moves one of who's attempts onto account.
func (a *admissionControl) grantSessionLocked(who admissionSessionKey, account string, fromQueue bool, now time.Time) {
	entry := a.sessionLocked(who, now)
	if entry == nil {
		return
	}
	if fromQueue && entry.queued > 0 {
		entry.queued--
	}
	entry.active++
	entry.served++
	entry.account = account
	if account == entry.overflowFrom {
		entry.overflowFrom, entry.overflowStreak = "", 0
	}
	if load := entry.active + entry.queued; load > entry.peak {
		entry.peak = load
	}
}

// noteQueueFullOverflow records one queue-full overflow off from and reports
// whether the session should now move: the streak reached
// admissionOverflowMoveAfter, which also resets it.
func (a *admissionControl) noteQueueFullOverflow(who admissionSessionKey, from string) bool {
	if a == nil {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	entry := a.sessionLocked(who, time.Now())
	if entry == nil {
		return false
	}
	entry.spilled++
	if entry.overflowFrom != from {
		entry.overflowFrom, entry.overflowStreak = from, 0
	}
	entry.overflowStreak++
	if entry.overflowStreak < admissionOverflowMoveAfter {
		return false
	}
	entry.overflowFrom, entry.overflowStreak = "", 0
	return true
}

func (a *admissionControl) noteSpill(who admissionSessionKey) {
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if entry := a.sessionLocked(who, time.Now()); entry != nil {
		entry.spilled++
	}
}

func (a *admissionControl) acquire(ctx context.Context, key admissionKey) (*admissionLease, error) {
	return a.acquireFor(ctx, key, admissionSessionKey{})
}

// acquireFor is acquire with the attempt attributed to who.
func (a *admissionControl) acquireFor(ctx context.Context, key admissionKey, who admissionSessionKey) (*admissionLease, error) {
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
			a.grantSessionLocked(who, key.account, false, now)
			a.mu.Unlock()
			return &admissionLease{control: a, key: key, who: who}, nil
		}
		if waiter != nil && len(bucket.waiters) > 0 && bucket.waiters[0] == waiter &&
			bucket.active < a.limit && !bucket.pausedTil.After(now) {
			copy(bucket.waiters, bucket.waiters[1:])
			bucket.waiters = bucket.waiters[:len(bucket.waiters)-1]
			bucket.active++
			bucket.served++
			a.grantSessionLocked(who, key.account, true, now)
			a.mu.Unlock()
			return &admissionLease{control: a, key: key, who: who}, nil
		}
		if waiter == nil {
			if bucket.pausedTil.Sub(now) > admissionPauseWaitMax {
				bucket.blocked++
				a.mu.Unlock()
				return nil, errAdmissionPaused
			}
			if len(bucket.waiters) >= a.queueCap {
				bucket.blocked++
				if entry := a.sessionLocked(who, now); entry != nil {
					entry.blocked++
				}
				retryAfter := a.retryAfterLocked(bucket)
				a.mu.Unlock()
				return nil, admissionQueueFullError{retryAfterSeconds: retryAfter}
			}
			waiter = &admissionWaiter{ready: make(chan struct{}), who: who}
			bucket.waiters = append(bucket.waiters, waiter)
			bucket.queued++
			if entry := a.sessionLocked(who, now); entry != nil {
				entry.queued++
				if load := entry.active + entry.queued; load > entry.peak {
					entry.peak = load
				}
			}
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
			return &admissionLease{control: a, key: key, who: who}, nil
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
					a.release(key, who)
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
func (a *admissionControl) tryAcquire(key admissionKey, who admissionSessionKey) (*admissionLease, bool) {
	if a == nil || key.account == "" {
		return nil, false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	now := time.Now()
	bucket := a.bucketLocked(key)
	if len(bucket.waiters) > 0 || bucket.active >= a.limit || bucket.pausedTil.After(now) {
		return nil, false
	}
	bucket.active++
	bucket.served++
	a.grantSessionLocked(who, key.account, false, now)
	return &admissionLease{control: a, key: key, who: who}, true
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
			if entry := a.sessions[waiter.who]; entry != nil && entry.queued > 0 {
				entry.queued--
			}
			return true
		}
	}
	return false
}

func (a *admissionControl) release(key admissionKey, who admissionSessionKey) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if entry := a.sessions[who]; entry != nil && entry.active > 0 {
		entry.active--
		entry.lastSeen = time.Now()
	}
	bucket := a.buckets[key]
	if bucket == nil {
		return
	}
	if bucket.active > 0 {
		bucket.active--
	}
	a.wakeLocked(key, bucket, time.Now())
}

func (a *admissionControl) wakeLocked(key admissionKey, bucket *admissionBucket, now time.Time) {
	if bucket.pausedTil.After(now) || bucket.active >= a.limit || len(bucket.waiters) == 0 {
		return
	}
	waiter := bucket.waiters[0]
	copy(bucket.waiters, bucket.waiters[1:])
	bucket.waiters = bucket.waiters[:len(bucket.waiters)-1]
	bucket.active++
	bucket.served++
	a.grantSessionLocked(waiter.who, key.account, true, now)
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
			if entry := a.sessions[waiter.who]; entry != nil {
				if entry.queued > 0 {
					entry.queued--
				}
				entry.blocked++
			}
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
	l.once.Do(func() { l.control.release(l.key, l.who) })
}

func (a *admissionControl) snapshot() admissionSnapshot {
	if a == nil {
		return admissionSnapshot{}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	out := admissionSnapshot{Limit: a.limit}
	for key, bucket := range a.buckets {
		entry := admissionKeySnapshot{
			Provider:    key.provider,
			AccountHash: admissionAccountHash(key.account),
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
	now := time.Now()
	a.pruneSessionsLocked(now)
	for who, entry := range a.sessions {
		session := admissionSessionSnapshot{
			Agent:         who.agent,
			SessionPrefix: admissionSessionPrefix(who.session),
			Active:        entry.active,
			Queued:        entry.queued,
			Peak:          entry.peak,
			Served:        entry.served,
			Blocked:       entry.blocked,
			Spilled:       entry.spilled,
			LastSeen:      entry.lastSeen.UTC().Format(time.RFC3339),
		}
		if entry.account != "" {
			session.AccountHash = admissionAccountHash(entry.account)
		}
		out.Sessions = append(out.Sessions, session)
	}
	// Busiest first: what a session holds now, then the most it ever held.
	sort.Slice(out.Sessions, func(i, j int) bool {
		left, right := out.Sessions[i], out.Sessions[j]
		if l, r := left.Active+left.Queued, right.Active+right.Queued; l != r {
			return l > r
		}
		if left.Peak != right.Peak {
			return left.Peak > right.Peak
		}
		return left.SessionPrefix < right.SessionPrefix
	})
	if len(out.Sessions) > admissionSessionTop {
		out.Sessions = out.Sessions[:admissionSessionTop]
	}
	return out
}

// admissionSessionPrefixLen is the length of the session id shown in the
// traffic snapshot.
const admissionSessionPrefixLen = 8

func admissionSessionPrefix(session string) string {
	if len(session) > admissionSessionPrefixLen {
		return session[:admissionSessionPrefixLen]
	}
	return session
}

func admissionAccountHash(account string) string {
	digest := sha256.Sum256([]byte(account))
	return "sha256:" + hex.EncodeToString(digest[:8])
}

func admissionKeyFor(provider accounts.Provider, accountID, model string) admissionKey {
	if model == "" {
		model = "default"
	}
	return admissionKey{provider: provider, account: accountID, model: model}
}
