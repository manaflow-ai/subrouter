package selectacct

import (
	"strings"
	"time"

	"github.com/manaflow-ai/subrouter/account"
)

// Capacity marks.
//
// A capacity failure ("Selected model is at capacity") is upstream load
// shedding for one (model, service tier), not evidence about an account's
// quota. It must not zero headroom, evict sticky sessions (their prompt cache
// lives on the account), or read as exhaustion on dashboards, so it lives
// beside the quota overlays rather than in them: a mark only lowers the
// account's rank among otherwise-equal candidates for requests to that pool.
// Only when the same account keeps failing (CapacityStickyEvictFailures in a
// row while the mark is live) does a sticky session move. The account's
// first success on the model clears its marks for every tier.

// CapacityStickyEvictFailures is how many consecutive capacity failures on
// one account+pool it takes before a sticky session leaves the account. One
// failure is usually shedding that a retry clears.
const CapacityStickyEvictFailures = 2

// CapacityAnyTier asks for marks of every service tier of a model, for a
// selection that does not know its tier yet (a websocket upgrade: the tier
// arrives with each response.create).
const CapacityAnyTier = "*"

// NormalizeServiceTier maps the request's service_tier to its pool: auto,
// default and unset share one pool.
func NormalizeServiceTier(tier string) string {
	tier = strings.ToLower(strings.TrimSpace(tier))
	switch tier {
	case "", "auto", "default":
		return "default"
	}
	return tier
}

// CapacityPoolKey is the display and grouping key for a (model, tier) pool.
func CapacityPoolKey(model, tier string) string {
	return ModelKey(model) + "|" + NormalizeServiceTier(tier)
}

type capacityMarkKey struct {
	scoreKey string
	model    string
	tier     string
}

type capacityMark struct {
	until    time.Time
	failures int
}

// CapacityMarkInfo is one live capacity mark, for diagnostics.
type CapacityMarkInfo struct {
	Provider  account.Provider
	AccountID string
	Model     string
	Tier      string
	Failures  int
	Until     time.Time
}

// MarkCapacityUntil records a capacity failure for the account in the
// (model, tier) pool until the given time and returns the consecutive
// failure count, which restarts at 1 once a previous mark expired.
func (r *SchedulerRef) MarkCapacityUntil(provider account.Provider, accountID, model, tier string, until time.Time) int {
	if r == nil || accountID == "" {
		return 0
	}
	now := time.Now()
	key := capacityMarkKey{scoreKey: ScoreKey(provider, accountID), model: ModelKey(model), tier: NormalizeServiceTier(tier)}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.capacityUntil == nil {
		r.capacityUntil = make(map[capacityMarkKey]capacityMark)
	}
	for k, mark := range r.capacityUntil {
		if !mark.until.After(now) {
			delete(r.capacityUntil, k)
		}
	}
	mark := r.capacityUntil[key]
	mark.failures++
	if until.After(mark.until) {
		mark.until = until
	}
	if !mark.until.After(now) {
		// Already expired on arrival: keep nothing, count nothing.
		delete(r.capacityUntil, key)
		return 1
	}
	r.capacityUntil[key] = mark
	return mark.failures
}

// ClearCapacity drops the account's capacity marks for the model, every
// tier, after it served the model successfully. It reports whether any mark
// was cleared.
func (r *SchedulerRef) ClearCapacity(provider account.Provider, accountID, model string) bool {
	if r == nil || accountID == "" {
		return false
	}
	scoreKey := ScoreKey(provider, accountID)
	modelKey := ModelKey(model)
	r.mu.RLock()
	found := false
	for key := range r.capacityUntil {
		if key.scoreKey == scoreKey && key.model == modelKey {
			found = true
			break
		}
	}
	r.mu.RUnlock()
	if !found {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for key := range r.capacityUntil {
		if key.scoreKey == scoreKey && key.model == modelKey {
			delete(r.capacityUntil, key)
		}
	}
	return true
}

// CapacityMarks returns the live consecutive-failure counts per ScoreKey for
// one (model, tier) pool, or for every tier of the model with
// CapacityAnyTier. Pass the result to Scheduler.WithCapacityMarks.
func (r *SchedulerRef) CapacityMarks(provider account.Provider, model, tier string) map[string]int {
	if r == nil {
		return nil
	}
	now := time.Now()
	modelKey := ModelKey(model)
	anyTier := tier == CapacityAnyTier
	tier = NormalizeServiceTier(tier)
	prefix := string(provider) + "\x00"
	if provider == "" {
		prefix = string(account.ProviderCodex) + "\x00"
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	var out map[string]int
	for key, mark := range r.capacityUntil {
		if key.model != modelKey || !mark.until.After(now) || !strings.HasPrefix(key.scoreKey, prefix) {
			continue
		}
		if !anyTier && key.tier != tier {
			continue
		}
		if out == nil {
			out = make(map[string]int)
		}
		if mark.failures > out[key.scoreKey] {
			out[key.scoreKey] = mark.failures
		}
	}
	return out
}

// HasCapacityMarks reports whether any capacity mark is live, so callers can
// skip work (such as reading the request's service tier) when none is.
func (r *SchedulerRef) HasCapacityMarks() bool {
	if r == nil {
		return false
	}
	now := time.Now()
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, mark := range r.capacityUntil {
		if mark.until.After(now) {
			return true
		}
	}
	return false
}

// CapacityMarkFor reports one live mark's expiry and failure count.
func (r *SchedulerRef) CapacityMarkFor(provider account.Provider, accountID, model, tier string) (time.Time, int, bool) {
	if r == nil {
		return time.Time{}, 0, false
	}
	key := capacityMarkKey{scoreKey: ScoreKey(provider, accountID), model: ModelKey(model), tier: NormalizeServiceTier(tier)}
	r.mu.RLock()
	defer r.mu.RUnlock()
	mark, ok := r.capacityUntil[key]
	if !ok || !mark.until.After(time.Now()) {
		return time.Time{}, 0, false
	}
	return mark.until, mark.failures, true
}

// CapacityMarkList returns every live capacity mark.
func (r *SchedulerRef) CapacityMarkList() []CapacityMarkInfo {
	if r == nil {
		return nil
	}
	now := time.Now()
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]CapacityMarkInfo, 0, len(r.capacityUntil))
	for key, mark := range r.capacityUntil {
		if !mark.until.After(now) {
			continue
		}
		provider, accountID, _ := strings.Cut(key.scoreKey, "\x00")
		out = append(out, CapacityMarkInfo{
			Provider: account.Provider(provider), AccountID: accountID,
			Model: key.model, Tier: key.tier, Failures: mark.failures, Until: mark.until,
		})
	}
	return out
}

// WithCapacityMarks attaches one pool's capacity failure counts (ScoreKey to
// consecutive failures). Marked accounts rank after unmarked ones of the
// same selection tier and leave the placement spread band; nothing else in
// the score changes.
func (s Scheduler) WithCapacityMarks(marks map[string]int) Scheduler {
	next := s
	next.capacity = marks
	return next
}

// CapacityFailures returns the account's consecutive capacity failures in
// the attached pool, zero when unmarked.
func (s Scheduler) CapacityFailures(provider account.Provider, accountID string) int {
	return s.capacity[ScoreKey(provider, accountID)]
}

// CapacityEvicting reports that the account kept failing on capacity, so a
// sticky session should move if a better account exists.
func (s Scheduler) CapacityEvicting(provider account.Provider, accountID string) bool {
	return s.CapacityFailures(provider, accountID) >= CapacityStickyEvictFailures
}
