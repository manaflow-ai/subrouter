package proxy

import (
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/manaflow-ai/subrouter/selectacct"
)

// Shedding signal: a non-blocking breaker.
//
// Every capacity outcome the Codex path sees (an upstream attempt that
// failed on capacity, or one that succeeded) is recorded per (model, tier)
// across all accounts. When most recent outcomes in the window are capacity
// failures, the model is shedding pool-wide: retrying harder only amplifies
// the overload. Nothing is blocked; the default retry policy's budget
// shrinks from ~10s to ~3s until the ratio recovers. Persist mode is the
// caller's explicit choice and keeps its budget (its gaps are jittered
// either way).
const (
	codexSheddingWindow = time.Minute
	// codexSheddingMinSamples keeps a handful of unlucky requests from
	// declaring a pool-wide event.
	codexSheddingMinSamples = 8
	// Enter at >=50% failures, leave below 30%, so the budget does not
	// flap at the boundary.
	codexSheddingEnterRatio = 0.5
	codexSheddingExitRatio  = 0.3
	// codexSheddingMaxEvents bounds memory per pool; the window is what
	// matters, and a busy pool fills it with the most recent outcomes.
	codexSheddingMaxEvents = 2048
	// codexCapacityShedRetryBudget is the default policy's budget while
	// the model is shedding.
	codexCapacityShedRetryBudget = 3 * time.Second
)

// CodexSheddingState is one (model, tier) pool's recent capacity outcomes,
// as exposed in /_subrouter/health.
type CodexSheddingState struct {
	Model        string    `json:"model"`
	Tier         string    `json:"tier"`
	FailureRatio float64   `json:"failure_ratio"`
	Samples      int       `json:"samples"`
	Failures     int       `json:"failures"`
	Shedding     bool      `json:"shedding"`
	Since        time.Time `json:"since,omitzero"`
	// RetryBudget is the default policy's current budget for this pool.
	RetryBudget string `json:"retry_budget"`
}

type codexSheddingOutcome struct {
	at     time.Time
	failed bool
}

type codexSheddingPool struct {
	model, tier string
	events      []codexSheddingOutcome
	since       time.Time
}

type codexSheddingTracker struct {
	mu    sync.Mutex
	pools map[string]*codexSheddingPool
}

func newCodexSheddingTracker() *codexSheddingTracker {
	return &codexSheddingTracker{pools: map[string]*codexSheddingPool{}}
}

// codexSheddingKey groups by the same normalized model key the capacity
// marks use, and keeps the model's own spelling for display.
func codexSheddingKey(model, tier string) (string, string, string) {
	display := strings.ToLower(strings.TrimSpace(model))
	tier = selectacct.NormalizeServiceTier(tier)
	return selectacct.ModelKey(model) + "|" + tier, display, tier
}

// record adds one outcome for the (model, tier) pool.
func (t *codexSheddingTracker) record(model, tier string, failed bool, now time.Time) {
	if t == nil {
		return
	}
	key, model, tier := codexSheddingKey(model, tier)
	t.mu.Lock()
	defer t.mu.Unlock()
	pool := t.pools[key]
	if pool == nil {
		if !failed {
			// A success in a pool with no failure history carries no
			// signal; do not grow the map for every healthy model.
			return
		}
		pool = &codexSheddingPool{model: model, tier: tier}
		t.pools[key] = pool
	}
	pool.events = append(pool.events, codexSheddingOutcome{at: now, failed: failed})
	if len(pool.events) > codexSheddingMaxEvents {
		pool.events = append(pool.events[:0:0], pool.events[len(pool.events)-codexSheddingMaxEvents:]...)
	}
	t.updateLocked(key, pool, now)
}

// updateLocked prunes the window and moves the pool's shedding state. It
// returns the pool's failures and samples in the window.
func (t *codexSheddingTracker) updateLocked(key string, pool *codexSheddingPool, now time.Time) (int, int) {
	cutoff := now.Add(-codexSheddingWindow)
	first := 0
	for first < len(pool.events) && !pool.events[first].at.After(cutoff) {
		first++
	}
	if first > 0 {
		pool.events = append(pool.events[:0:0], pool.events[first:]...)
	}
	failures := 0
	for _, event := range pool.events {
		if event.failed {
			failures++
		}
	}
	samples := len(pool.events)
	if samples == 0 {
		delete(t.pools, key)
		return 0, 0
	}
	ratio := float64(failures) / float64(samples)
	switch {
	case pool.since.IsZero() && samples >= codexSheddingMinSamples && ratio >= codexSheddingEnterRatio:
		pool.since = now
	case !pool.since.IsZero() && (ratio < codexSheddingExitRatio || samples < codexSheddingMinSamples):
		pool.since = time.Time{}
	}
	return failures, samples
}

// shedding reports whether the (model, tier) pool is currently shedding.
func (t *codexSheddingTracker) shedding(model, tier string, now time.Time) bool {
	if t == nil {
		return false
	}
	key, _, _ := codexSheddingKey(model, tier)
	t.mu.Lock()
	defer t.mu.Unlock()
	pool := t.pools[key]
	if pool == nil {
		return false
	}
	t.updateLocked(key, pool, now)
	return !pool.since.IsZero()
}

// snapshot lists every pool with capacity failures in the window, shedding
// pools first.
func (t *codexSheddingTracker) snapshot(now time.Time) []CodexSheddingState {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]CodexSheddingState, 0, len(t.pools))
	for key, pool := range t.pools {
		failures, samples := t.updateLocked(key, pool, now)
		if samples == 0 || failures == 0 {
			continue
		}
		state := CodexSheddingState{
			Model: pool.model, Tier: pool.tier, Samples: samples, Failures: failures,
			FailureRatio: float64(failures) / float64(samples),
			Shedding:     !pool.since.IsZero(), Since: pool.since,
			RetryBudget: codexCapacityDefaultRetryBudget.String(),
		}
		if state.Shedding {
			state.RetryBudget = codexCapacityShedRetryBudget.String()
		}
		out = append(out, state)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Shedding != out[j].Shedding {
			return out[i].Shedding
		}
		if out[i].Model != out[j].Model {
			return out[i].Model < out[j].Model
		}
		return out[i].Tier < out[j].Tier
	})
	return out
}

// codexDefaultRetryBudget is the default policy's budget for this request:
// the configured one, shrunk while its (model, tier) pool is shedding.
func (s *Server) codexDefaultRetryBudget(model, tier string) time.Duration {
	budget := s.CodexOverloadFailover.retryBudget()
	if s.codexShedding.shedding(model, tier, time.Now()) {
		budget = min(budget, codexCapacityShedRetryBudget)
	}
	return budget
}

// recordCodexCapacityOutcome feeds the shedding signal.
func (s *Server) recordCodexCapacityOutcome(model, tier string, failed bool) {
	if s == nil {
		return
	}
	s.codexShedding.record(model, tier, failed, time.Now())
}
