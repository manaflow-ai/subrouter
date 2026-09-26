package selectacct

import (
	"testing"

	"github.com/manaflow-ai/subrouter/account"
)

// Live debits floor headroom at 0.01, well under every retention threshold.
// They exist to reorder picks, so they must not evict a session from the
// account holding its upstream prompt cache.
func TestUsableForStickySessionIgnoresLiveDebits(t *testing.T) {
	scheduler := NewScheduler([]Score{
		{AccountID: "busy@example.com", Provider: account.ProviderCodex, Headroom: 0.13, ShortHeadroom: 0.13},
	})
	scheduler = scheduler.WithLiveDebits(map[string]int{
		ScoreKey(account.ProviderCodex, "busy@example.com"): 20,
	})
	if scheduler.UsableForNewSession(account.ProviderCodex, "busy@example.com") {
		t.Fatal("a debited account at 13% headroom should not take a new session")
	}
	if !scheduler.UsableForStickySession(account.ProviderCodex, "busy@example.com") {
		t.Fatal("a session already on the account should stay while measured headroom is 13%")
	}
}

func TestUsableForStickySessionFollowsMeasuredHeadroom(t *testing.T) {
	scheduler := NewScheduler([]Score{
		{AccountID: "empty@example.com", Provider: account.ProviderCodex, Headroom: 0.02, ShortHeadroom: 0.02},
		{AccountID: "short@example.com", Provider: account.ProviderCodex, Headroom: 0.50, ShortHeadroom: 0.02},
	})
	if scheduler.UsableForStickySession(account.ProviderCodex, "empty@example.com") {
		t.Fatal("2% measured headroom is below the retention threshold")
	}
	if scheduler.UsableForStickySession(account.ProviderCodex, "short@example.com") {
		t.Fatal("a spent short window must end retention even when the weekly window is healthy")
	}
}
