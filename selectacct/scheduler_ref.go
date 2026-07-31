package selectacct

import (
	"strings"
	"sync"
	"time"

	"github.com/manaflow-ai/subrouter/account"
)

type SchedulerRef struct {
	mu                sync.RWMutex
	scheduler         Scheduler
	updatedAt         time.Time
	refreshing        bool
	accountGeneration uint64
	refreshGeneration uint64
	// legacyFinishInvalidated preserves the historical tokenless FinishRefresh
	// API for callers that publish without BeginRefreshIfStale. New concurrent
	// code uses the generation-aware methods below.
	legacyFinishInvalidated bool
	// exhaustedUntil expires request-time exhaustion marks. A mark set from a
	// rejected upstream response is only true until the account's rate-limit
	// window resets; without an expiry a recovered account stayed zero-scored
	// until the next SUCCESSFUL usage refresh, which under load can fail for
	// hours, leaving real quota unroutable while clients got 429s.
	exhaustedUntil map[string]time.Time
	// incompatibleUntil records account/model exclusions learned from upstream
	// entitlement errors. Usage refreshes cannot supersede these marks because
	// quota headroom says nothing about whether an account supports a model.
	incompatibleUntil map[string]time.Time
	// routedSinceRefresh counts requests the proxy routed per account (by
	// ScoreKey) since the last successful usage refresh. Pick debits headroom
	// by LiveDebitPerRequest per routed request so concurrent traffic spreads
	// instead of herding onto the snapshot's best account until it cooks.
	routedSinceRefresh map[string]int
}

func NewSchedulerRef(scheduler Scheduler) *SchedulerRef {
	return &SchedulerRef{
		scheduler: scheduler,
		updatedAt: time.Now(),
	}
}

func (r *SchedulerRef) Get() Scheduler {
	now := time.Now()
	r.pruneExpired(now)
	r.mu.RLock()
	defer r.mu.RUnlock()
	scheduler := applyExhaustionMarks(r.scheduler, r.exhaustedUntil, now)
	return applyExhaustionMarks(scheduler, r.incompatibleUntil, now)
}

// pruneExpired drops exhaustion marks whose window has reset. The base snapshot
// is not mutated; once the overlay mark is gone, routing reads the snapshot (or
// optimistic default) normally.
//
// Deliberate tradeoff: a lapsed mark cannot distinguish "recovered" from "fresh
// usage re-confirmed exhausted" (refreshes seed zero scores forward, so the two
// are indistinguishable here). We choose optimistic retry-once: if the account
// is still cooked, the one probe request is rejected upstream and the account
// is immediately re-marked with the NEW authoritative reset time, so the cost
// is bounded at one attempt per account per expiry window. The opposite choice
// (trusting a zero score without an expiry) is the failure this fixes: a
// recovered account's real quota stayed unroutable for hours. This matches the
// scheduler-wide philosophy that scores are load-balancing hints and the
// upstream response is the source of truth.
func (r *SchedulerRef) pruneExpired(now time.Time) {
	r.mu.RLock()
	anyExpired := false
	for _, until := range r.exhaustedUntil {
		if !until.After(now) {
			anyExpired = true
			break
		}
	}
	if !anyExpired {
		for _, until := range r.incompatibleUntil {
			if !until.After(now) {
				anyExpired = true
				break
			}
		}
	}
	r.mu.RUnlock()
	if !anyExpired {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for key, until := range r.exhaustedUntil {
		if until.After(now) {
			continue
		}
		delete(r.exhaustedUntil, key)
	}
	for key, until := range r.incompatibleUntil {
		if until.After(now) {
			continue
		}
		delete(r.incompatibleUntil, key)
	}
}

func (r *SchedulerRef) Set(scheduler Scheduler) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.refreshing {
		r.legacyFinishInvalidated = true
	}
	r.setLocked(scheduler)
	r.refreshing = false
}

func (r *SchedulerRef) setLocked(scheduler Scheduler) {
	base := r.scheduler
	r.scheduler = scheduler
	r.retainExhaustedExpiriesLocked()
	r.scheduler = stripCarriedForwardExhaustionOverlays(r.scheduler, base, r.expiryMarksLocked())
	r.updatedAt = time.Now()
}

// AdvanceAccountGeneration invalidates refresh work computed from an older
// account snapshot. Callers advance immediately after publishing a new
// AccountRef snapshot, before any potentially slow usage scoring begins.
func (r *SchedulerRef) AdvanceAccountGeneration(generation uint64) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if generation == r.accountGeneration {
		return
	}
	r.accountGeneration = generation
	r.refreshing = false
	r.updatedAt = time.Time{}
}

// SetForAccountGeneration publishes a scheduler only when it was computed
// from the current account snapshot. The comparison and write share one lock,
// so a concurrent account reload cannot slip between them.
func (r *SchedulerRef) SetForAccountGeneration(scheduler Scheduler, generation uint64) bool {
	if r == nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if generation != r.accountGeneration {
		return false
	}
	r.setLocked(scheduler)
	r.refreshing = false
	return true
}

// retainExhaustedExpiriesLocked reconciles mark expiries with an incoming
// refresh, by evidence class:
//   - A pool-scoped mark has no matching refreshed pool score: keep the expiry;
//     the refresh has no evidence about that pool.
//   - A matching score shows headroom (or the account is gone): the mark is
//     superseded; drop the expiry.
//   - Carried-forward zero (the account's own usage fetch failed, seed dragged
//     along, Fresh=false): keep the existing expiry. Clearing it would make the
//     request-time mark permanent again, recreating the stranded-recovered-
//     account failure this exists to fix.
//   - Fresh zero (a successful fetch re-confirmed exhaustion, Fresh=true):
//     re-anchor the expiry to this newest evidence — extend it to at least the
//     fresh window's own reset (floored at the default TTL) — so an OLDER
//     request-time expiry can never lapse a freshly-observed zero back to the
//     optimistic default. Expiries only extend here, never shorten, so an
//     authoritative long reset from a rejected response still holds.
func (r *SchedulerRef) retainExhaustedExpiriesLocked() {
	now := time.Now()
	for key := range r.exhaustedUntil {
		scoreKey, _, poolKey, ok := exhaustionKeyParts(key)
		if !ok {
			delete(r.exhaustedUntil, key)
			continue
		}
		score, ok := r.scheduler.scores[scoreKey]
		if ok && poolKey != "" {
			modelScore, modelOK := score.ModelScores[poolKey]
			if !modelOK {
				continue
			}
			score = modelScore
		}
		switch {
		case !ok || !score.exhausted():
			delete(r.exhaustedUntil, key)
		case score.Fresh:
			until := now.Add(DefaultExhaustedTTL)
			if score.ShortResetAfterSeconds > 0 {
				if fromReset := now.Add(time.Duration(score.ShortResetAfterSeconds) * time.Second); fromReset.After(until) {
					until = fromReset
				}
			}
			if cap := now.Add(8 * 24 * time.Hour); until.After(cap) {
				until = cap
			}
			if until.After(r.exhaustedUntil[key]) {
				r.exhaustedUntil[key] = until
			}
		}
	}
}

// DefaultExhaustedTTL bounds an exhaustion mark when the upstream response gave
// no reset time. Short on purpose: re-marking a still-cooked account costs one
// failed attempt, while over-holding a recovered account starves routing of
// real quota.
const DefaultExhaustedTTL = 10 * time.Minute

func (r *SchedulerRef) MarkExhausted(provider account.Provider, accountID, poolKey string) {
	r.MarkExhaustedUntil(provider, accountID, poolKey, time.Now().Add(DefaultExhaustedTTL))
}

// MarkExhaustedUntil records an exhaustion overlay until the given time, after
// which routing reads the base snapshot again. An empty poolKey marks the whole
// account; a non-empty poolKey marks only that model pool. Callers pass the
// upstream's own reset time
// (anthropic-ratelimit-unified-reset / Retry-After) when available.
func (r *SchedulerRef) MarkExhaustedUntil(provider account.Provider, accountID, poolKey string, until time.Time) {
	if accountID == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.exhaustedUntil == nil {
		r.exhaustedUntil = make(map[string]time.Time)
	}
	r.exhaustedUntil[poolScopedExhaustionKey(provider, accountID, poolKey)] = until
	r.updatedAt = time.Now()
}

// MarkModelIncompatibleUntil excludes one account from one model until the
// supplied expiry. Unlike quota exhaustion, usage-score refreshes cannot clear
// this mark because they do not carry entitlement evidence.
func (r *SchedulerRef) MarkModelIncompatibleUntil(provider account.Provider, accountID, model string, until time.Time) {
	if accountID == "" || model == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.incompatibleUntil == nil {
		r.incompatibleUntil = make(map[string]time.Time)
	}
	r.incompatibleUntil[poolScopedExhaustionKey(provider, accountID, model)] = until
	r.updatedAt = time.Now()
}

func (r *SchedulerRef) MarkModelIncompatible(provider account.Provider, accountID, model string) {
	r.MarkModelIncompatibleUntil(provider, accountID, model, time.Now().Add(DefaultExhaustedTTL))
}

// ExhaustedUntilFor reports the expiry recorded for an account's exhaustion
// mark, if any. Used by tests and diagnostics to verify TTL selection.
func (r *SchedulerRef) ExhaustedUntilFor(provider account.Provider, accountID, poolKey string) (time.Time, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	until, ok := r.exhaustedUntil[poolScopedExhaustionKey(provider, accountID, poolKey)]
	return until, ok
}

func (r *SchedulerRef) ModelIncompatibleUntilFor(provider account.Provider, accountID, model string) (time.Time, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	until, ok := r.incompatibleUntil[poolScopedExhaustionKey(provider, accountID, model)]
	return until, ok
}

func (r *SchedulerRef) expiryMarksLocked() map[string]time.Time {
	marks := make(map[string]time.Time, len(r.exhaustedUntil)+len(r.incompatibleUntil))
	for key, until := range r.exhaustedUntil {
		marks[key] = until
	}
	for key, until := range r.incompatibleUntil {
		if until.After(marks[key]) {
			marks[key] = until
		}
	}
	return marks
}

func poolScopedExhaustionKey(provider account.Provider, accountID, poolKey string) string {
	return ScoreKey(provider, accountID) + "\x00" + ModelKey(poolKey)
}

func exhaustionKeyParts(key string) (scoreKey string, provider account.Provider, poolKey string, ok bool) {
	parts := strings.SplitN(key, "\x00", 3)
	if len(parts) != 3 {
		return "", "", "", false
	}
	provider = account.Provider(parts[0])
	return ScoreKey(provider, parts[1]), provider, parts[2], true
}

func applyExhaustionMarks(base Scheduler, exhaustedUntil map[string]time.Time, now time.Time) Scheduler {
	if len(exhaustedUntil) == 0 {
		return base
	}
	next := Scheduler{
		scores:        make(map[string]Score, len(base.scores)),
		sessionCounts: base.sessionCounts,
		liveDebits:    base.liveDebits,
	}
	for key, score := range base.scores {
		next.scores[key] = copyScore(score)
	}
	introducedPools := make(map[string]bool)
	for key, until := range exhaustedUntil {
		if !until.After(now) {
			continue
		}
		_, _, poolKey, ok := exhaustionKeyParts(key)
		if ok && poolKey != "" && !base.hasModelScore(poolKey) {
			introducedPools[poolKey] = true
		}
	}
	for poolKey := range introducedPools {
		for scoreKey, score := range next.scores {
			if _, ok := score.ModelScores[poolKey]; ok {
				continue
			}
			score.ModelScores = copyModelScores(score.ModelScores)
			if score.ModelScores == nil {
				score.ModelScores = make(map[string]Score, 1)
			}
			poolScore := score
			poolScore.ModelScores = nil
			score.ModelScores[poolKey] = poolScore
			next.scores[scoreKey] = score
		}
	}
	accountWide := make(map[string]bool)
	for key, until := range exhaustedUntil {
		if !until.After(now) {
			continue
		}
		scoreKey, provider, poolKey, ok := exhaustionKeyParts(key)
		if !ok || poolKey != "" {
			continue
		}
		score := next.scores[scoreKey]
		if score.AccountID == "" {
			_, accountID, _ := strings.Cut(scoreKey, "\x00")
			score.AccountID = accountID
		}
		score.Provider = provider
		score.Headroom = 0
		score.ShortHeadroom = 0
		score.ModelScores = nil
		next.scores[scoreKey] = score
		accountWide[scoreKey] = true
	}
	for key, until := range exhaustedUntil {
		if !until.After(now) {
			continue
		}
		scoreKey, provider, poolKey, ok := exhaustionKeyParts(key)
		if !ok || poolKey == "" || accountWide[scoreKey] {
			continue
		}
		score := next.scores[scoreKey]
		if score.AccountID == "" {
			_, accountID, _ := strings.Cut(scoreKey, "\x00")
			score = Score{AccountID: accountID, Provider: provider, Headroom: 1, ShortHeadroom: 1}
		}
		score.Provider = provider
		score.ModelScores = copyModelScores(score.ModelScores)
		if score.ModelScores == nil {
			score.ModelScores = make(map[string]Score, 1)
		}
		score.ModelScores[poolKey] = Score{AccountID: score.AccountID, Provider: provider, Headroom: 0, ShortHeadroom: 0}
		next.scores[scoreKey] = score
	}
	return next
}

func copyScore(score Score) Score {
	score.ModelScores = copyModelScores(score.ModelScores)
	return score
}

func copyModelScores(modelScores map[string]Score) map[string]Score {
	if len(modelScores) == 0 {
		return nil
	}
	out := make(map[string]Score, len(modelScores))
	for key, score := range modelScores {
		out[key] = score
	}
	return out
}

func stripCarriedForwardExhaustionOverlays(current, base Scheduler, exhaustedUntil map[string]time.Time) Scheduler {
	if len(exhaustedUntil) == 0 {
		return current
	}
	next := Scheduler{
		scores:        make(map[string]Score, len(current.scores)),
		sessionCounts: current.sessionCounts,
		liveDebits:    current.liveDebits,
	}
	for key, score := range current.scores {
		next.scores[key] = copyScore(score)
	}
	for key := range exhaustedUntil {
		scoreKey, _, poolKey, ok := exhaustionKeyParts(key)
		if !ok {
			continue
		}
		if poolKey != "" && !base.hasModelScore(poolKey) {
			for candidateKey, candidate := range next.scores {
				modelScore, modelOK := candidate.ModelScores[poolKey]
				if !modelOK || modelScore.Fresh {
					continue
				}
				candidate.ModelScores = copyModelScores(candidate.ModelScores)
				delete(candidate.ModelScores, poolKey)
				next.scores[candidateKey] = candidate
			}
		}
		score, ok := next.scores[scoreKey]
		if !ok {
			continue
		}
		if poolKey == "" {
			if !score.exhausted() || score.Fresh {
				continue
			}
			if baseScore, baseOK := base.scores[scoreKey]; baseOK {
				next.scores[scoreKey] = copyScore(baseScore)
			} else {
				delete(next.scores, scoreKey)
			}
			continue
		}
		modelScore, modelOK := score.ModelScores[poolKey]
		if !modelOK || !modelScore.exhausted() || modelScore.Fresh {
			continue
		}
		score.ModelScores = copyModelScores(score.ModelScores)
		if baseScore, baseOK := base.scores[scoreKey]; baseOK {
			if baseModelScore, baseModelOK := baseScore.ModelScores[poolKey]; baseModelOK {
				score.ModelScores[poolKey] = baseModelScore
			} else {
				delete(score.ModelScores, poolKey)
			}
		} else {
			delete(score.ModelScores, poolKey)
		}
		next.scores[scoreKey] = score
	}
	return next
}

func (r *SchedulerRef) Stale(ttl time.Duration) bool {
	if ttl <= 0 {
		return false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.updatedAt.IsZero() || time.Since(r.updatedAt) >= ttl
}

func (r *SchedulerRef) BeginRefreshIfStale(ttl time.Duration) bool {
	if r == nil {
		return false
	}
	r.mu.RLock()
	generation := r.accountGeneration
	r.mu.RUnlock()
	return r.BeginRefreshIfStaleForAccountGeneration(ttl, generation)
}

func (r *SchedulerRef) BeginRefreshIfStaleForAccountGeneration(ttl time.Duration, generation uint64) bool {
	if ttl <= 0 {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if generation != r.accountGeneration || r.refreshing || (!r.updatedAt.IsZero() && time.Since(r.updatedAt) < ttl) {
		return false
	}
	r.refreshing = true
	r.refreshGeneration = generation
	return true
}

func (r *SchedulerRef) FinishRefresh(scheduler Scheduler, update bool) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.legacyFinishInvalidated {
		r.legacyFinishInvalidated = false
		return
	}
	if r.refreshing && r.refreshGeneration != r.accountGeneration {
		return
	}
	r.finishRefreshLocked(scheduler, update)
}

func (r *SchedulerRef) FinishRefreshForAccountGeneration(scheduler Scheduler, update bool, generation uint64) bool {
	if r == nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if generation != r.accountGeneration || !r.refreshing || r.refreshGeneration != generation {
		return false
	}
	r.finishRefreshLocked(scheduler, update)
	return true
}

func (r *SchedulerRef) finishRefreshLocked(scheduler Scheduler, update bool) {
	if update {
		base := r.scheduler
		r.scheduler = scheduler
		r.retainExhaustedExpiriesLocked()
		r.scheduler = stripCarriedForwardExhaustionOverlays(r.scheduler, base, r.expiryMarksLocked())
		// Fresh scores supersede the live debits accumulated against the old
		// snapshot. A failed refresh (update=false) keeps them: the snapshot
		// is still the old one, so its debits still apply.
		r.routedSinceRefresh = nil
	}
	r.updatedAt = time.Now()
	r.refreshing = false
}

// NoteRouted records that one request was routed to the account, debiting its
// live score until the next successful usage refresh.
func (r *SchedulerRef) NoteRouted(provider account.Provider, accountID string) {
	if r == nil || accountID == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.routedSinceRefresh == nil {
		r.routedSinceRefresh = make(map[string]int)
	}
	r.routedSinceRefresh[ScoreKey(provider, accountID)]++
}

// LiveDebits returns the per-account routed-request counts since the last
// successful refresh, for Scheduler.WithLiveDebits.
func (r *SchedulerRef) LiveDebits() map[string]int {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	if len(r.routedSinceRefresh) == 0 {
		return nil
	}
	out := make(map[string]int, len(r.routedSinceRefresh))
	for key, count := range r.routedSinceRefresh {
		out[key] = count
	}
	return out
}

func (r *SchedulerRef) Touch() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.updatedAt = time.Now()
}

func (r *SchedulerRef) SetUpdatedAt(updatedAt time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.updatedAt = updatedAt
}
