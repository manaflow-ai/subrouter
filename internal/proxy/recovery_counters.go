package proxy

import (
	"sync"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
)

// RecoveryCounters is the rolling one-hour view of capacity and retry recovery
// activity. It is intentionally a small, in-memory operational signal.
type RecoveryCounters struct {
	RetriesHeld          int64 `json:"retries_held"`
	PersistentRetries    int64 `json:"persistent_retries"`
	Retryable503Handoffs int64 `json:"retryable_503_handoffs"`
	Exhausted            int64 `json:"exhausted"`
}

type recoveryCounterStore struct {
	mu      sync.Mutex
	buckets map[int64]map[string]RecoveryCounters
}

func (s *recoveryCounterStore) add(provider accounts.Provider, kind string, now time.Time) {
	if s == nil || provider == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	minute := now.Unix() / 60
	s.pruneLocked(minute)
	if s.buckets == nil {
		s.buckets = make(map[int64]map[string]RecoveryCounters)
	}
	if s.buckets[minute] == nil {
		s.buckets[minute] = make(map[string]RecoveryCounters)
	}
	counters := s.buckets[minute]
	c := counters[string(provider)]
	switch kind {
	case "held":
		c.RetriesHeld++
	case "persistent":
		c.PersistentRetries++
	case "handoff_503":
		c.Retryable503Handoffs++
	case "exhausted":
		c.Exhausted++
	}
	counters[string(provider)] = c
}

func (s *recoveryCounterStore) snapshot(now time.Time) map[string]RecoveryCounters {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	minute := now.Unix() / 60
	s.pruneLocked(minute)
	out := map[string]RecoveryCounters{
		string(accounts.ProviderClaude): {},
		string(accounts.ProviderCodex):  {},
	}
	for bucketMinute, counters := range s.buckets {
		if bucketMinute < minute-59 {
			continue
		}
		for provider, c := range counters {
			out[provider] = RecoveryCounters{
				RetriesHeld:          out[provider].RetriesHeld + c.RetriesHeld,
				PersistentRetries:    out[provider].PersistentRetries + c.PersistentRetries,
				Retryable503Handoffs: out[provider].Retryable503Handoffs + c.Retryable503Handoffs,
				Exhausted:            out[provider].Exhausted + c.Exhausted,
			}
		}
	}
	return out
}

func (s *recoveryCounterStore) pruneLocked(currentMinute int64) {
	if s.buckets == nil {
		return
	}
	for minute := range s.buckets {
		if minute < currentMinute-59 {
			delete(s.buckets, minute)
		}
	}
}
