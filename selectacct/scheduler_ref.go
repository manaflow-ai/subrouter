package selectacct

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"sync"
	"time"

	"github.com/manaflow-ai/subrouter/account"
)

type SchedulerRef struct {
	mu                 sync.RWMutex
	scheduler          Scheduler
	updatedAt          time.Time
	refreshing         bool
	refreshInvalidated bool
	accountGeneration  uint64
	refreshGeneration  uint64
	scoreRevision      uint64
	// exhaustedUntil expires request-time exhaustion marks. A mark set from a
	// rejected upstream response is only true until the account's rate-limit
	// window resets; without an expiry a recovered account stayed zero-scored
	// until the next SUCCESSFUL usage refresh, which under load can fail for
	// hours, leaving real quota unroutable while clients got 429s.
	exhaustedUntil map[string]time.Time
	// weeklyExhaustedUntil is the subset of marks whose upstream response
	// proved the weekly window cooked (7d status rejected). Paid Claude
	// fallback reads it: session-only marks never authorize paid spend.
	// Entries are always paired with an exhaustedUntil mark and share its
	// expiry.
	weeklyExhaustedUntil map[string]time.Time
	// credentialExhaustedUntil is separate from quota/model evidence and is
	// scoped to the account snapshot generation that observed the bad token.
	// Replacing credentials advances the generation, immediately discarding the
	// old token's exclusion while rejecting late reports from in-flight work.
	credentialExhaustedUntil map[string]time.Time
	credentialFingerprints   map[string]string
	credentialRevision       uint64
	// accountUnavailableUntil records account-state exclusions, such as an
	// organization disabling OAuth. Neither healthy quota evidence nor token
	// rotation proves that account state recovered, so those events must not
	// clear this overlay.
	accountUnavailableUntil map[string]time.Time
	// incompatibleUntil records account/model exclusions learned from upstream
	// entitlement errors. Usage refreshes cannot supersede these marks because
	// quota headroom says nothing about whether an account supports a model.
	incompatibleUntil map[string]time.Time
	// recoveryProbeReady remembers that an authoritative exclusion expired while
	// the measured snapshot still read zero. Routing may attempt exactly one
	// optimistic probe; RunIfAccountNotBlocked consumes the allowance atomically.
	recoveryProbeReady map[string]struct{}
	// routedSinceRefresh counts requests the proxy routed per account (by
	// ScoreKey) since the last successful usage refresh. Pick debits headroom
	// by LiveDebitPerRequest per routed request so concurrent traffic spreads
	// instead of herding onto the snapshot's best account until it cooks.
	routedSinceRefresh map[string]int
	// capacityUntil holds capacity (load-shedding) marks per account and
	// (model, service tier). They are deliberately not an exhaustion overlay:
	// see capacity.go.
	capacityUntil map[capacityMarkKey]capacityMark
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
	scheduler = applyWeeklyExhaustionMarks(scheduler, r.weeklyExhaustedUntil, now)
	scheduler = applyExhaustionMarks(scheduler, r.activeCredentialExhaustionLocked(), now)
	scheduler = applyExhaustionMarks(scheduler, r.accountUnavailableUntil, now)
	return applyExhaustionMarks(scheduler, r.incompatibleUntil, now)
}

// RefreshSeed returns the measured scheduler snapshot, including live routing
// debits but excluding temporary exclusion overlays. A usage refresh carries
// stale scores forward when an account cannot be fetched; seeding that work
// from Get would bake an overlay's zero into the replacement snapshot if the
// overlay expired between scoring and FinishRefreshForAccountGeneration. Routing reads must use
// Get, while refresh construction must use this base-only view.
func (r *SchedulerRef) RefreshSeed() Scheduler {
	if r == nil {
		return Scheduler{}
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	debits := make(map[string]int, len(r.routedSinceRefresh))
	for key, count := range r.routedSinceRefresh {
		debits[key] = count
	}
	return r.scheduler.WithLiveDebits(debits)
}

// RunIfAccountNotBlocked executes publish while the current scheduler state
// remains read-locked. The caller may hold its own publication mutex, but
// publish must not call back into this SchedulerRef.
func (r *SchedulerRef) RunIfAccountNotBlocked(
	provider account.Provider,
	accountID string,
	model string,
	now time.Time,
	publish func(),
) (time.Time, bool) {
	if r == nil {
		publish()
		return time.Time{}, true
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if until, blocked := r.blockedUntilLocked(provider, accountID, model, now); blocked {
		return until, false
	}
	r.clearExpiredRouteMarksLocked(provider, accountID, model, now)
	publish()
	return time.Time{}, true
}

// BlockedUntilFor reports the union of account-wide, credential, quota-pool,
// account-state, and model-incompatibility exclusions for one route.
func (r *SchedulerRef) BlockedUntilFor(
	provider account.Provider,
	accountID string,
	model string,
	now time.Time,
) (time.Time, bool) {
	if r == nil {
		return time.Time{}, false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.blockedUntilLocked(provider, accountID, model, now)
}

// ExplicitBlockedUntilFor reports only time-bounded exclusion marks, without
// interpreting a missing model score as exhausted. API-key accounts do not
// participate in subscription model-pool scoring, but still honor explicit
// account, credential, and model-incompatibility state.
func (r *SchedulerRef) ExplicitBlockedUntilFor(
	provider account.Provider,
	accountID string,
	model string,
	now time.Time,
) (time.Time, bool) {
	if r == nil {
		return time.Time{}, false
	}
	r.pruneExpired(now)
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.explicitBlockedUntilLocked(provider, accountID, model, now)
}

func (r *SchedulerRef) explicitBlockedUntilLocked(
	provider account.Provider,
	accountID string,
	model string,
	now time.Time,
) (time.Time, bool) {
	marks := r.expiryMarksLocked()
	var until time.Time
	for _, key := range []string{
		poolScopedExhaustionKey(provider, accountID, ""),
		poolScopedExhaustionKey(provider, accountID, model),
	} {
		if candidate := marks[key]; candidate.After(until) {
			until = candidate
		}
	}
	return until, until.After(now)
}

// RunIfAccountNotExplicitlyBlocked is the API-key publication guard. It
// ignores absent subscription pool scores while retaining explicit routing
// exclusions and the same lock-order guarantee as RunIfAccountNotBlocked.
func (r *SchedulerRef) RunIfAccountNotExplicitlyBlocked(
	provider account.Provider,
	accountID string,
	model string,
	now time.Time,
	publish func(),
) (time.Time, bool) {
	if r == nil {
		publish()
		return time.Time{}, true
	}
	r.pruneExpired(now)
	r.mu.RLock()
	defer r.mu.RUnlock()
	if until, blocked := r.explicitBlockedUntilLocked(provider, accountID, model, now); blocked {
		return until, false
	}
	publish()
	return time.Time{}, true
}

func (r *SchedulerRef) blockedUntilLocked(
	provider account.Provider,
	accountID string,
	model string,
	now time.Time,
) (time.Time, bool) {
	scheduler := applyExhaustionMarks(r.scheduler, r.exhaustedUntil, now)
	scheduler = applyExhaustionMarks(scheduler, r.activeCredentialExhaustionLocked(), now)
	scheduler = applyExhaustionMarks(scheduler, r.accountUnavailableUntil, now)
	scheduler = applyExhaustionMarks(scheduler, r.incompatibleUntil, now)
	if !scheduler.ForModel(model).Exhausted(provider, accountID) {
		return time.Time{}, false
	}
	var until time.Time
	marks := r.expiryMarksLocked()
	for _, key := range []string{
		poolScopedExhaustionKey(provider, accountID, ""),
		poolScopedExhaustionKey(provider, accountID, model),
	} {
		if candidate := marks[key]; candidate.After(until) {
			until = candidate
		}
	}
	if until.IsZero() {
		if r.recoveryProbeReadyLocked(provider, accountID, model) {
			return time.Time{}, false
		}
		until = now.Add(DefaultExhaustedTTL)
	} else if !until.After(now) {
		// An expired authoritative mark grants one optimistic probe even when
		// the measured snapshot still says zero. The publication guard consumes
		// that mark atomically so concurrent lease attempts cannot all probe.
		return time.Time{}, false
	}
	return until, true
}

func (r *SchedulerRef) clearExpiredRouteMarksLocked(
	provider account.Provider,
	accountID string,
	model string,
	now time.Time,
) {
	keys := []string{
		poolScopedExhaustionKey(provider, accountID, ""),
		poolScopedExhaustionKey(provider, accountID, model),
	}
	for _, key := range keys {
		delete(r.recoveryProbeReady, key)
	}
	for _, marks := range []map[string]time.Time{
		r.exhaustedUntil,
		r.accountUnavailableUntil,
		r.incompatibleUntil,
	} {
		for _, key := range keys {
			if until := marks[key]; !until.IsZero() && !until.After(now) {
				delete(marks, key)
			}
		}
	}
	// Credential exclusions include the credential fingerprint after the score
	// key, so remove any expired entry for this route's account.
	prefix := ScoreKey(provider, accountID) + "\x00"
	for key, until := range r.credentialExhaustedUntil {
		if strings.HasPrefix(key, prefix) && !until.After(now) {
			delete(r.credentialExhaustedUntil, key)
		}
	}
}

func (r *SchedulerRef) recoveryProbeReadyLocked(provider account.Provider, accountID, model string) bool {
	for _, key := range []string{
		poolScopedExhaustionKey(provider, accountID, ""),
		poolScopedExhaustionKey(provider, accountID, model),
	} {
		if _, ok := r.recoveryProbeReady[key]; ok {
			return true
		}
	}
	return false
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
		for _, until := range r.weeklyExhaustedUntil {
			if !until.After(now) {
				anyExpired = true
				break
			}
		}
	}
	if !anyExpired {
		for _, until := range r.credentialExhaustedUntil {
			if !until.After(now) {
				anyExpired = true
				break
			}
		}
	}
	if !anyExpired {
		for _, until := range r.accountUnavailableUntil {
			if !until.After(now) {
				anyExpired = true
				break
			}
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
		probeKeys := r.recoveryProbeKeysForExpiredMarkLocked(key)
		if len(probeKeys) > 0 {
			if r.recoveryProbeReady == nil {
				r.recoveryProbeReady = make(map[string]struct{})
			}
			for _, probeKey := range probeKeys {
				r.recoveryProbeReady[probeKey] = struct{}{}
			}
		}
		delete(r.exhaustedUntil, key)
	}
	for key, until := range r.weeklyExhaustedUntil {
		if !until.After(now) {
			delete(r.weeklyExhaustedUntil, key)
		}
	}
	for key, until := range r.credentialExhaustedUntil {
		if !until.After(now) {
			delete(r.credentialExhaustedUntil, key)
		}
	}
	for key, until := range r.accountUnavailableUntil {
		if !until.After(now) {
			delete(r.accountUnavailableUntil, key)
		}
	}
	for key, until := range r.incompatibleUntil {
		if until.After(now) {
			continue
		}
		delete(r.incompatibleUntil, key)
	}
}

func (r *SchedulerRef) scoreForExhaustionKeyLocked(key string) (Score, bool) {
	scoreKey, _, poolKey, ok := exhaustionKeyParts(key)
	if !ok {
		return Score{}, false
	}
	score, ok := r.scheduler.scores[scoreKey]
	if !ok {
		return Score{}, false
	}
	if poolKey != "" {
		modelScore, exists := score.ModelScores[poolKey]
		if exists {
			return modelScore, true
		}
		if r.scheduler.hasModelScore(poolKey) {
			return Score{}, false
		}
	}
	return score, true
}

func (r *SchedulerRef) recoveryProbeKeysForExpiredMarkLocked(key string) []string {
	scoreKey, provider, poolKey, ok := exhaustionKeyParts(key)
	if !ok {
		return nil
	}
	score, ok := r.scheduler.scores[scoreKey]
	if !ok {
		return nil
	}
	if poolKey != "" {
		modelScore, exists := score.ModelScores[poolKey]
		if exists && modelScore.exhausted() {
			return []string{key}
		}
		if !r.scheduler.hasModelScore(poolKey) && score.exhausted() {
			return []string{key}
		}
		return nil
	}
	if score.exhausted() {
		return []string{key}
	}
	keys := make([]string, 0, len(score.ModelScores))
	for model, modelScore := range score.ModelScores {
		if modelScore.exhausted() {
			keys = append(keys, poolScopedExhaustionKey(provider, score.AccountID, model))
		}
	}
	return keys
}

func (r *SchedulerRef) Set(scheduler Scheduler) {
	r.mu.Lock()
	defer r.mu.Unlock()
	inflightRefresh := r.refreshing
	r.setLocked(scheduler)
	if inflightRefresh {
		r.refreshInvalidated = true
		return
	}
	r.refreshing = false
	r.refreshInvalidated = false
}

func (r *SchedulerRef) setLocked(scheduler Scheduler) {
	r.setLockedForScoreKeys(scheduler, nil, true)
}

func (r *SchedulerRef) setLockedForScoreKeys(scheduler Scheduler, scoreKeys map[string]struct{}, touchUpdatedAt bool) {
	base := r.scheduler
	r.scheduler = scheduler
	r.retainExhaustedExpiriesForScoreKeysLocked(scoreKeys)
	r.scheduler = stripCarriedForwardExhaustionOverlaysForScoreKeys(r.scheduler, base, r.expiryMarksLocked(), scoreKeys)
	now := time.Now()
	for key := range r.recoveryProbeReady {
		scoreKey, _, _, valid := exhaustionKeyParts(key)
		if scoreKeys != nil {
			if _, included := scoreKeys[scoreKey]; valid && !included {
				continue
			}
		}
		score, ok := r.scoreForExhaustionKeyLocked(key)
		if !ok || !score.exhausted() {
			delete(r.recoveryProbeReady, key)
			continue
		}
		if score.Fresh {
			delete(r.recoveryProbeReady, key)
			if r.exhaustedUntil == nil {
				r.exhaustedUntil = make(map[string]time.Time)
			}
			until := now.Add(DefaultExhaustedTTL)
			if score.ShortResetAfterSeconds > 0 {
				if fromReset := now.Add(time.Duration(score.ShortResetAfterSeconds) * time.Second); fromReset.After(until) {
					until = fromReset
				}
			}
			if cap := now.Add(8 * 24 * time.Hour); until.After(cap) {
				until = cap
			}
			r.exhaustedUntil[key] = until
		}
	}
	if touchUpdatedAt {
		r.updatedAt = time.Now()
	}
	r.scoreRevision++
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
	r.advanceAccountGenerationLocked(generation)
}

// AdvanceAccountGenerationWithAccounts advances the snapshot and publishes
// the credential identity currently attached to each routing account. Existing
// exclusions remain effective for unchanged tokens; repaired tokens do not
// inherit them.
func (r *SchedulerRef) AdvanceAccountGenerationWithAccounts(generation, credentialRevision uint64, accounts []account.Account) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if generation < r.accountGeneration {
		return
	}
	if generation == r.accountGeneration && credentialRevision <= r.credentialRevision && r.credentialFingerprints != nil {
		return
	}
	r.advanceAccountGenerationLocked(generation)
	r.credentialRevision = credentialRevision
	r.credentialFingerprints = make(map[string]string, len(accounts))
	for _, candidate := range accounts {
		r.credentialFingerprints[ScoreKey(candidate.Provider, candidate.ID)] = credentialFingerprint(candidate.CredentialIdentity())
	}
}

// SyncAccountCredentials publishes token rotations that occur within an
// account generation (for example, a successful OAuth refresh). It does not
// invalidate score work; it only changes which credential-scoped exclusions
// apply to the current snapshot.
func (r *SchedulerRef) SyncAccountCredentials(generation, credentialRevision uint64, accounts []account.Account) bool {
	if r == nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if generation != r.accountGeneration || credentialRevision < r.credentialRevision {
		return false
	}
	if credentialRevision == r.credentialRevision && r.credentialFingerprints != nil {
		return true
	}
	r.credentialRevision = credentialRevision
	r.credentialFingerprints = make(map[string]string, len(accounts))
	for _, candidate := range accounts {
		r.credentialFingerprints[ScoreKey(candidate.Provider, candidate.ID)] = credentialFingerprint(candidate.CredentialIdentity())
	}
	return true
}

func (r *SchedulerRef) advanceAccountGenerationLocked(generation uint64) {
	if generation == r.accountGeneration {
		return
	}
	r.accountGeneration = generation
	r.refreshing = false
	r.refreshInvalidated = false
	r.updatedAt = time.Time{}
	r.scoreRevision++
}

// SetForAccountGeneration publishes a scheduler only when it was computed
// from the current account snapshot. The comparison and write share one lock,
// so a concurrent account reload cannot slip between them.
func (r *SchedulerRef) SetForAccountGeneration(scheduler Scheduler, generation uint64) bool {
	return r.SetForAccountGenerationAtScoreRevision(scheduler, generation, r.ScoreRevision())
}

// ScoreRevision identifies the measured score snapshot independently of the
// account generation. Callers capture it before slow score work and present it
// when publishing so a newer same-generation measurement cannot be replaced.
func (r *SchedulerRef) ScoreRevision() uint64 {
	if r == nil {
		return 0
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.scoreRevision
}

func (r *SchedulerRef) SetForAccountGenerationAtScoreRevision(
	scheduler Scheduler,
	generation uint64,
	scoreRevision uint64,
) bool {
	if r == nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if generation != r.accountGeneration || scoreRevision != r.scoreRevision {
		return false
	}
	inflightRefresh := r.refreshing
	r.setLocked(scheduler)
	if inflightRefresh {
		r.refreshInvalidated = true
	} else {
		r.refreshing = false
		r.refreshInvalidated = false
	}
	return true
}

// MergeScoresForAccountGeneration atomically overlays measured scores onto the
// current shared scheduler when they were computed from the current account
// snapshot. Unlike SetForAccountGeneration, a partial provider refresh cannot
// replace scores published concurrently for other providers, and it does not
// cancel a full refresh already in progress for the same generation.
func (r *SchedulerRef) MergeScoresForAccountGeneration(scores []Score, generation uint64) (Scheduler, bool) {
	return r.MergeScoresForAccountGenerationAtScoreRevision(scores, generation, r.ScoreRevision())
}

func (r *SchedulerRef) MergeScoresForAccountGenerationAtScoreRevision(
	scores []Score,
	generation uint64,
	scoreRevision uint64,
) (Scheduler, bool) {
	if r == nil {
		return Scheduler{}, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if generation != r.accountGeneration || scoreRevision != r.scoreRevision {
		return Scheduler{}, false
	}
	merged := r.scheduler
	scoreKeys := make(map[string]struct{}, len(scores))
	for _, score := range scores {
		merged = merged.WithScore(score)
		scoreKeys[ScoreKey(score.Provider, score.AccountID)] = struct{}{}
	}
	// This is only a partial provider refresh, so it must not make the shared
	// full-provider snapshot look fresh and suppress its next scheduled refresh.
	r.setLockedForScoreKeys(merged, scoreKeys, false)
	if r.refreshing {
		// A full refresh that seeded before this merge is now stale for at least
		// these scores. Keep its claim only until its finish is consumed and
		// rejected; admitting a replacement sooner would give both attempts the
		// same account-generation identity and let the older one publish first.
		r.refreshInvalidated = true
	}
	debits := make(map[string]int, len(r.routedSinceRefresh))
	for key, count := range r.routedSinceRefresh {
		debits[key] = count
	}
	now := time.Now()
	published := applyExhaustionMarks(r.scheduler, r.exhaustedUntil, now)
	published = applyExhaustionMarks(published, r.activeCredentialExhaustionLocked(), now)
	published = applyExhaustionMarks(published, r.accountUnavailableUntil, now)
	published = applyExhaustionMarks(published, r.incompatibleUntil, now)
	return published.WithLiveDebits(debits), true
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
	r.retainExhaustedExpiriesForScoreKeysLocked(nil)
}

func (r *SchedulerRef) retainExhaustedExpiriesForScoreKeysLocked(scoreKeys map[string]struct{}) {
	now := time.Now()
	for key := range r.exhaustedUntil {
		scoreKey, _, poolKey, ok := exhaustionKeyParts(key)
		if !ok {
			delete(r.exhaustedUntil, key)
			continue
		}
		if scoreKeys != nil {
			if _, included := scoreKeys[scoreKey]; !included {
				continue
			}
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
	r.markExhaustedUntilLocked(provider, accountID, poolKey, until)
}

func (r *SchedulerRef) markExhaustedUntilLocked(provider account.Provider, accountID, poolKey string, until time.Time) {
	if r.exhaustedUntil == nil {
		r.exhaustedUntil = make(map[string]time.Time)
	}
	key := poolScopedExhaustionKey(provider, accountID, poolKey)
	r.exhaustedUntil[key] = until
	delete(r.recoveryProbeReady, key)
	r.updatedAt = time.Now()
}

// MarkWeeklyExhaustedUntil records an exhaustion mark whose upstream response
// proved the WEEKLY window is cooked (anthropic-ratelimit-unified-7d-status:
// rejected), not merely the 5h session window. Only weekly-cooked evidence
// authorizes paid Claude fallback; a session-level 429 is a temporary wait.
func (r *SchedulerRef) MarkWeeklyExhaustedUntil(provider account.Provider, accountID, poolKey string, until time.Time) {
	if accountID == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.markExhaustedUntilLocked(provider, accountID, poolKey, until)
	if r.weeklyExhaustedUntil == nil {
		r.weeklyExhaustedUntil = make(map[string]time.Time)
	}
	r.weeklyExhaustedUntil[poolScopedExhaustionKey(provider, accountID, poolKey)] = until
}

// MarkCredentialExhaustedUntil records a terminal credential failure only if
// it was observed against the current account snapshot. Unlike quota marks,
// credential exclusions are discarded when credentials are reloaded.
func (r *SchedulerRef) MarkCredentialExhaustedUntil(
	provider account.Provider,
	accountID string,
	credentialIdentity string,
	until time.Time,
	accountGeneration uint64,
) bool {
	if r == nil || accountID == "" {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	fingerprint := credentialFingerprint(credentialIdentity)
	scoreKey := ScoreKey(provider, accountID)
	if accountGeneration != r.accountGeneration {
		return false
	}
	if r.credentialFingerprints == nil {
		r.credentialFingerprints = make(map[string]string)
	}
	if current, tracked := r.credentialFingerprints[scoreKey]; tracked && current != fingerprint {
		return false
	}
	r.credentialFingerprints[scoreKey] = fingerprint
	if r.credentialExhaustedUntil == nil {
		r.credentialExhaustedUntil = make(map[string]time.Time)
	}
	r.credentialExhaustedUntil[scoreKey+"\x00"+fingerprint] = until
	r.updatedAt = time.Now()
	return true
}

// MarkCredentialExhaustedForSnapshot atomically publishes a coherent account
// snapshot and records a terminal failure against the credential in that same
// snapshot. This closes the reload window where AccountRef has advanced but a
// separate scheduler-generation sync has not arrived yet.
func (r *SchedulerRef) MarkCredentialExhaustedForSnapshot(
	provider account.Provider,
	accountID string,
	credentialIdentity string,
	until time.Time,
	accountGeneration uint64,
	credentialRevision uint64,
	accounts []account.Account,
) bool {
	if r == nil || accountID == "" {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if accountGeneration < r.accountGeneration ||
		(accountGeneration == r.accountGeneration && credentialRevision < r.credentialRevision) {
		return false
	}
	if accountGeneration > r.accountGeneration {
		r.advanceAccountGenerationLocked(accountGeneration)
	}
	if credentialRevision > r.credentialRevision || r.credentialFingerprints == nil {
		r.credentialRevision = credentialRevision
		r.credentialFingerprints = make(map[string]string, len(accounts))
		for _, candidate := range accounts {
			r.credentialFingerprints[ScoreKey(candidate.Provider, candidate.ID)] = credentialFingerprint(candidate.CredentialIdentity())
		}
	}

	fingerprint := credentialFingerprint(credentialIdentity)
	scoreKey := ScoreKey(provider, accountID)
	if current, tracked := r.credentialFingerprints[scoreKey]; !tracked || current != fingerprint {
		return false
	}
	if r.credentialExhaustedUntil == nil {
		r.credentialExhaustedUntil = make(map[string]time.Time)
	}
	r.credentialExhaustedUntil[scoreKey+"\x00"+fingerprint] = until
	r.updatedAt = time.Now()
	return true
}

func credentialFingerprint(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func (r *SchedulerRef) activeCredentialExhaustionLocked() map[string]time.Time {
	active := make(map[string]time.Time)
	for key, until := range r.credentialExhaustedUntil {
		parts := strings.SplitN(key, "\x00", 3)
		if len(parts) != 3 {
			continue
		}
		scoreKey := ScoreKey(account.Provider(parts[0]), parts[1])
		if current, tracked := r.credentialFingerprints[scoreKey]; tracked && current != parts[2] {
			continue
		}
		active[poolScopedExhaustionKey(account.Provider(parts[0]), parts[1], "")] = until
	}
	return active
}

// MarkAccountUnavailableUntil excludes an account because of state that is
// independent of both its quota and its current credential. Usage refreshes
// and token rotation cannot supersede this evidence; only expiry can.
func (r *SchedulerRef) MarkAccountUnavailableUntil(provider account.Provider, accountID string, until time.Time) {
	if accountID == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.accountUnavailableUntil == nil {
		r.accountUnavailableUntil = make(map[string]time.Time)
	}
	r.accountUnavailableUntil[poolScopedExhaustionKey(provider, accountID, "")] = until
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
	key := poolScopedExhaustionKey(provider, accountID, poolKey)
	until, ok := r.exhaustedUntil[key]
	if credentialUntil, credentialOK := r.activeCredentialExhaustionLocked()[key]; credentialOK && (!ok || credentialUntil.After(until)) {
		until, ok = credentialUntil, true
	}
	if accountUntil, accountOK := r.accountUnavailableUntil[key]; accountOK && (!ok || accountUntil.After(until)) {
		until, ok = accountUntil, true
	}
	return until, ok
}

func (r *SchedulerRef) ModelIncompatibleUntilFor(provider account.Provider, accountID, model string) (time.Time, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	until, ok := r.incompatibleUntil[poolScopedExhaustionKey(provider, accountID, model)]
	return until, ok
}

func (r *SchedulerRef) expiryMarksLocked() map[string]time.Time {
	marks := make(map[string]time.Time, len(r.exhaustedUntil)+len(r.accountUnavailableUntil)+len(r.incompatibleUntil))
	for key, until := range r.exhaustedUntil {
		marks[key] = until
	}
	for key, until := range r.incompatibleUntil {
		if until.After(marks[key]) {
			marks[key] = until
		}
	}
	for key, until := range r.accountUnavailableUntil {
		if until.After(marks[key]) {
			marks[key] = until
		}
	}
	for key, until := range r.activeCredentialExhaustionLocked() {
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
		capacity:      base.capacity,
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
			// A mark for an account with no measured score is session-level
			// evidence only; weekly stays fail-closed at "not cooked".
			score = Score{AccountID: accountID, Provider: provider, Headroom: 1, ShortHeadroom: 1, WeeklyHeadroom: 1}
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
			score = Score{AccountID: accountID, Provider: provider, Headroom: 1, ShortHeadroom: 1, WeeklyHeadroom: 1}
		}
		score.Provider = provider
		score.ModelScores = copyModelScores(score.ModelScores)
		if score.ModelScores == nil {
			score.ModelScores = make(map[string]Score, 1)
		}
		poolScore, exists := score.ModelScores[poolKey]
		if !exists {
			poolScore = score
			poolScore.ModelScores = nil
		}
		poolScore.AccountID = score.AccountID
		poolScore.Provider = provider
		poolScore.Headroom = 0
		poolScore.ShortHeadroom = 0
		score.ModelScores[poolKey] = poolScore
		next.scores[scoreKey] = score
	}
	return next
}

// applyWeeklyExhaustionMarks zeroes WeeklyHeadroom for accounts (or model
// pools) whose upstream response proved the weekly window cooked. Ordinary
// exhaustion marks leave WeeklyHeadroom untouched: a session-level 429 must
// never read as weekly evidence, because paid Claude fallback gates on it.
func applyWeeklyExhaustionMarks(base Scheduler, weeklyUntil map[string]time.Time, now time.Time) Scheduler {
	if len(weeklyUntil) == 0 {
		return base
	}
	next := Scheduler{
		scores:        make(map[string]Score, len(base.scores)),
		sessionCounts: base.sessionCounts,
		liveDebits:    base.liveDebits,
		capacity:      base.capacity,
	}
	for key, score := range base.scores {
		next.scores[key] = copyScore(score)
	}
	for key, until := range weeklyUntil {
		if !until.After(now) {
			continue
		}
		scoreKey, provider, poolKey, ok := exhaustionKeyParts(key)
		if !ok {
			continue
		}
		if poolKey == "" {
			score := next.scores[scoreKey]
			if score.AccountID == "" {
				_, accountID, _ := strings.Cut(scoreKey, "\x00")
				score.AccountID = accountID
			}
			score.Provider = provider
			score.WeeklyHeadroom = 0
			score.WeeklyHeadroomKnown = true
			for pool, modelScore := range score.ModelScores {
				modelScore.WeeklyHeadroom = 0
				modelScore.WeeklyHeadroomKnown = true
				score.ModelScores[pool] = modelScore
			}
			next.scores[scoreKey] = score
			continue
		}
		score := next.scores[scoreKey]
		if score.AccountID == "" {
			continue
		}
		if poolScore, exists := score.ModelScores[poolKey]; exists {
			poolScore.WeeklyHeadroom = 0
			poolScore.WeeklyHeadroomKnown = true
			score.ModelScores[poolKey] = poolScore
			next.scores[scoreKey] = score
		}
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
	return stripCarriedForwardExhaustionOverlaysForScoreKeys(current, base, exhaustedUntil, nil)
}

func stripCarriedForwardExhaustionOverlaysForScoreKeys(current, base Scheduler, exhaustedUntil map[string]time.Time, scoreKeys map[string]struct{}) Scheduler {
	if len(exhaustedUntil) == 0 {
		return current
	}
	next := Scheduler{
		scores:        make(map[string]Score, len(current.scores)),
		sessionCounts: current.sessionCounts,
		liveDebits:    current.liveDebits,
		capacity:      current.capacity,
	}
	for key, score := range current.scores {
		next.scores[key] = copyScore(score)
	}
	for key := range exhaustedUntil {
		scoreKey, _, poolKey, ok := exhaustionKeyParts(key)
		if !ok {
			continue
		}
		if scoreKeys != nil {
			if _, included := scoreKeys[scoreKey]; !included {
				continue
			}
		}
		if poolKey != "" && !base.hasModelScore(poolKey) {
			for candidateKey, candidate := range next.scores {
				if scoreKeys != nil {
					if _, included := scoreKeys[candidateKey]; !included {
						continue
					}
				}
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

// UpdatedAt reports when the scores were last stamped by a refresh (successful
// or not) or Set. The zero time means the scheduler has never been scored.
func (r *SchedulerRef) UpdatedAt() time.Time {
	if r == nil {
		return time.Time{}
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.updatedAt
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
	r.refreshInvalidated = false
	r.refreshGeneration = generation
	return true
}

// FinishRefreshForAccountGeneration publishes a refresh begun by
// BeginRefreshIfStaleForAccountGeneration. It is the only way to finish a
// refresh: the generation token rejects a result computed from an older
// account snapshot even when a newer refresh has since begun.
func (r *SchedulerRef) FinishRefreshForAccountGeneration(scheduler Scheduler, update bool, generation uint64) bool {
	if r == nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if generation != r.accountGeneration || !r.refreshing || r.refreshGeneration != generation {
		return false
	}
	if r.refreshInvalidated {
		r.refreshInvalidated = false
		r.refreshing = false
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
		r.scoreRevision++
	}
	r.updatedAt = time.Now()
	r.refreshing = false
	r.refreshInvalidated = false
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
