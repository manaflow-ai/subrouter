package selectacct

import (
	"strings"
	"sync"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
)

type SchedulerRef struct {
	mu                        sync.RWMutex
	scheduler                 Scheduler
	updatedAt                 time.Time
	refreshing                bool
	modelIncompatibilityStore *ModelIncompatibilityStore
	modelIncompatibilities    map[string]ModelIncompatibility
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

func NewSchedulerRefWithModelStore(scheduler Scheduler, store ModelIncompatibilityStore) (*SchedulerRef, error) {
	issues, err := store.Load()
	if err != nil {
		return nil, err
	}
	ref := NewSchedulerRef(scheduler)
	ref.modelIncompatibilityStore = &store
	if len(issues) > 0 {
		ref.modelIncompatibilities = make(map[string]ModelIncompatibility, len(issues))
		for _, issue := range issues {
			ref.modelIncompatibilities[modelIncompatibilityKey(issue.Provider, issue.AccountID, issue.Model)] = issue
		}
	}
	return ref, nil
}

func (r *SchedulerRef) Get() Scheduler {
	now := time.Now()
	r.pruneExpired(now)
	r.mu.RLock()
	defer r.mu.RUnlock()
	return applyExhaustionMarks(r.scheduler, r.expiryMarksLocked(), now)
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
	base := r.scheduler
	r.scheduler = scheduler
	r.retainExhaustedExpiriesLocked()
	r.scheduler = stripCarriedForwardExhaustionOverlays(r.scheduler, base, r.expiryMarksLocked())
	r.updatedAt = time.Now()
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

func (r *SchedulerRef) MarkExhausted(provider accounts.Provider, accountID, poolKey string) {
	r.MarkExhaustedUntil(provider, accountID, poolKey, time.Now().Add(DefaultExhaustedTTL))
}

// MarkExhaustedUntil records an exhaustion overlay until the given time, after
// which routing reads the base snapshot again. An empty poolKey marks the whole
// account; a non-empty poolKey marks only that model pool. Callers pass the
// upstream's own reset time
// (anthropic-ratelimit-unified-reset / Retry-After) when available.
func (r *SchedulerRef) MarkExhaustedUntil(provider accounts.Provider, accountID, poolKey string, until time.Time) {
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
func (r *SchedulerRef) MarkModelIncompatibleUntil(provider accounts.Provider, accountID, model string, until time.Time) {
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

func (r *SchedulerRef) MarkModelIncompatible(provider accounts.Provider, accountID, model, message string) error {
	issue, err := normalizeModelIncompatibility(ModelIncompatibility{
		Provider:   provider,
		AccountID:  accountID,
		Model:      model,
		Code:       ModelIncompatibilityCode,
		Message:    message,
		ObservedAt: time.Now().UTC(),
	})
	if err != nil {
		return err
	}
	key := modelIncompatibilityKey(issue.Provider, issue.AccountID, issue.Model)
	r.mu.Lock()
	if r.modelIncompatibilities == nil {
		r.modelIncompatibilities = make(map[string]ModelIncompatibility)
	}
	r.modelIncompatibilities[key] = issue
	r.updatedAt = time.Now()
	store := r.modelIncompatibilityStore
	r.mu.Unlock()
	if store == nil {
		return nil
	}
	return store.Put(issue)
}

// ExhaustedUntilFor reports the expiry recorded for an account's exhaustion
// mark, if any. Used by tests and diagnostics to verify TTL selection.
func (r *SchedulerRef) ExhaustedUntilFor(provider accounts.Provider, accountID, poolKey string) (time.Time, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	until, ok := r.exhaustedUntil[poolScopedExhaustionKey(provider, accountID, poolKey)]
	return until, ok
}

func (r *SchedulerRef) ModelIncompatibleUntilFor(provider accounts.Provider, accountID, model string) (time.Time, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	key := modelIncompatibilityKey(provider, accountID, model)
	if _, ok := r.modelIncompatibilities[key]; ok {
		return permanentModelIncompatibilityUntil, true
	}
	until, ok := r.incompatibleUntil[key]
	return until, ok
}

func (r *SchedulerRef) ModelIncompatibilities() []ModelIncompatibility {
	r.refreshModelIncompatibilitiesFromStore()
	r.mu.RLock()
	defer r.mu.RUnlock()
	issues := make([]ModelIncompatibility, 0, len(r.modelIncompatibilities))
	for _, issue := range r.modelIncompatibilities {
		issues = append(issues, issue)
	}
	sortModelIncompatibilities(issues)
	return issues
}

func (r *SchedulerRef) ModelIncompatible(provider accounts.Provider, accountID, model string) bool {
	if ModelKey(model) == "" {
		return false
	}
	key := modelIncompatibilityKey(provider, accountID, model)
	r.mu.RLock()
	_, ok := r.modelIncompatibilities[key]
	store := r.modelIncompatibilityStore
	r.mu.RUnlock()
	if ok || store == nil {
		return ok
	}
	issue, found, err := store.Get(provider, accountID, model)
	if err != nil {
		// Compatibility evidence is a safety boundary. A corrupt or unreadable
		// record must fail closed so a storage incident cannot route traffic back
		// to an account that may be known-incompatible.
		return true
	}
	if !found {
		return false
	}
	r.mu.Lock()
	if r.modelIncompatibilities == nil {
		r.modelIncompatibilities = make(map[string]ModelIncompatibility)
	}
	r.modelIncompatibilities[key] = issue
	r.mu.Unlock()
	return true
}

func (r *SchedulerRef) refreshModelIncompatibilitiesFromStore() {
	r.mu.RLock()
	store := r.modelIncompatibilityStore
	r.mu.RUnlock()
	if store == nil {
		return
	}
	issues, err := store.Load()
	if err != nil || len(issues) == 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.modelIncompatibilities == nil {
		r.modelIncompatibilities = make(map[string]ModelIncompatibility, len(issues))
	}
	for _, issue := range issues {
		r.modelIncompatibilities[modelIncompatibilityKey(issue.Provider, issue.AccountID, issue.Model)] = issue
	}
}

var permanentModelIncompatibilityUntil = time.Date(9999, time.December, 31, 23, 59, 59, 0, time.UTC)

func (r *SchedulerRef) expiryMarksLocked() map[string]time.Time {
	marks := make(map[string]time.Time, len(r.exhaustedUntil)+len(r.incompatibleUntil)+len(r.modelIncompatibilities))
	for key, until := range r.exhaustedUntil {
		marks[key] = until
	}
	for key, until := range r.incompatibleUntil {
		if until.After(marks[key]) {
			marks[key] = until
		}
	}
	for key := range r.modelIncompatibilities {
		marks[key] = permanentModelIncompatibilityUntil
	}
	return marks
}

func poolScopedExhaustionKey(provider accounts.Provider, accountID, poolKey string) string {
	return ScoreKey(provider, accountID) + "\x00" + ModelKey(poolKey)
}

func exhaustionKeyParts(key string) (scoreKey string, provider accounts.Provider, poolKey string, ok bool) {
	parts := strings.SplitN(key, "\x00", 3)
	if len(parts) != 3 {
		return "", "", "", false
	}
	provider = accounts.Provider(parts[0])
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
	if ttl <= 0 {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.refreshing || (!r.updatedAt.IsZero() && time.Since(r.updatedAt) < ttl) {
		return false
	}
	r.refreshing = true
	return true
}

func (r *SchedulerRef) FinishRefresh(scheduler Scheduler, update bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
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
func (r *SchedulerRef) NoteRouted(provider accounts.Provider, accountID string) {
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
