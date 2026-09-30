package proxy

import (
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
)

func TestRecoveryCounterStoreKeepsOneHourPerProvider(t *testing.T) {
	store := &recoveryCounterStore{}
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	store.add(accounts.ProviderClaude, "held", now.Add(-59*time.Minute))
	store.add(accounts.ProviderClaude, "persistent", now.Add(-61*time.Minute))
	store.add(accounts.ProviderCodex, "handoff_503", now)
	store.add(accounts.ProviderCodex, "exhausted", now)
	got := store.snapshot(now)
	if got["claude"].RetriesHeld != 1 || got["claude"].PersistentRetries != 0 {
		t.Fatalf("claude counters = %+v", got["claude"])
	}
	if got["codex"].Retryable503Handoffs != 1 || got["codex"].Exhausted != 1 {
		t.Fatalf("codex counters = %+v", got["codex"])
	}
}
