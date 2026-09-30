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

type recoveryEvent struct {
	at       time.Time
	provider string
	kind     string
}

type recoveryCounterStore struct {
	mu     sync.Mutex
	events []recoveryEvent
}

func (s *recoveryCounterStore) add(provider accounts.Provider, kind string, now time.Time) {
	if s == nil || provider == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked(now)
	s.events = append(s.events, recoveryEvent{at: now, provider: string(provider), kind: kind})
}

func (s *recoveryCounterStore) snapshot(now time.Time) map[string]RecoveryCounters {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked(now)
	out := map[string]RecoveryCounters{string(accounts.ProviderClaude): {}, string(accounts.ProviderCodex): {}}
	for _, e := range s.events {
		c := out[e.provider]
		switch e.kind {
		case "held":
			c.RetriesHeld++
		case "persistent":
			c.PersistentRetries++
		case "handoff_503":
			c.Retryable503Handoffs++
		case "exhausted":
			c.Exhausted++
		}
		out[e.provider] = c
	}
	return out
}

func (s *recoveryCounterStore) pruneLocked(now time.Time) {
	cutoff := now.Add(-time.Hour)
	kept := s.events[:0]
	for _, event := range s.events {
		if !event.at.Before(cutoff) {
			kept = append(kept, event)
		}
	}
	s.events = kept
}
