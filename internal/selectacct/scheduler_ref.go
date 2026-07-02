package selectacct

import (
	"sync"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
)

type SchedulerRef struct {
	mu             sync.RWMutex
	scheduler      Scheduler
	updatedAt      time.Time
	refreshing     bool
	exhaustedUntil map[string]time.Time
}

func NewSchedulerRef(scheduler Scheduler) *SchedulerRef {
	return &SchedulerRef{
		scheduler: scheduler,
		updatedAt: time.Now(),
	}
}

func (r *SchedulerRef) Get() Scheduler {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.scheduler
}

func (r *SchedulerRef) Set(scheduler Scheduler) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.scheduler = r.withActiveExhaustionLocked(scheduler, time.Now())
	r.updatedAt = time.Now()
}

func (r *SchedulerRef) MarkExhausted(provider accounts.Provider, accountID string) {
	r.MarkExhaustedUntil(provider, accountID, time.Time{})
}

func (r *SchedulerRef) MarkExhaustedUntil(provider accounts.Provider, accountID string, until time.Time) {
	if accountID == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.scheduler = r.scheduler.WithScore(Score{
		AccountID:     accountID,
		Provider:      provider,
		Headroom:      0,
		ShortHeadroom: 0,
	})
	if until.After(time.Now()) {
		if r.exhaustedUntil == nil {
			r.exhaustedUntil = map[string]time.Time{}
		}
		r.exhaustedUntil[ScoreKey(provider, accountID)] = until
	}
	r.updatedAt = time.Now()
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
		r.scheduler = r.withActiveExhaustionLocked(scheduler, time.Now())
	}
	r.updatedAt = time.Now()
	r.refreshing = false
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

func (r *SchedulerRef) withActiveExhaustionLocked(scheduler Scheduler, now time.Time) Scheduler {
	for key, until := range r.exhaustedUntil {
		if !until.After(now) {
			delete(r.exhaustedUntil, key)
			continue
		}
		score := scheduler.ScoreForKey(key)
		score.Headroom = 0
		score.ShortHeadroom = 0
		scheduler = scheduler.WithScore(score)
	}
	if len(r.exhaustedUntil) == 0 {
		r.exhaustedUntil = nil
	}
	return scheduler
}
