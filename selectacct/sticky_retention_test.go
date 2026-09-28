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

// Live debits steer picks between subscription accounts but must never tip
// a measured-healthy one behind a paid API key.
func TestLiveDebitsNeverMoveSubscriptionBehindAPIKey(t *testing.T) {
	sub := account.Account{ID: "sub@example.com", Provider: account.ProviderCodex, AuthMode: account.AuthModeOAuth}
	key := account.Account{ID: "api-key", Provider: account.ProviderCodex, AuthMode: account.AuthModeAPIKey}
	scheduler := NewScheduler([]Score{{AccountID: sub.ID, Provider: sub.Provider, Headroom: 0.50, ShortHeadroom: 0.50}}).
		WithLiveDebits(map[string]int{ScoreKey(sub.Provider, sub.ID): 6})
	picked, err := scheduler.Pick([]account.Account{key, sub})
	if err != nil || picked.ID != sub.ID {
		t.Fatalf("picked %q (err %v): six routed requests sent a 50%% subscription account's traffic to the API key", picked.ID, err)
	}

	// Between subscription accounts the debit still steers new work away.
	other := account.Account{ID: "other@example.com", Provider: account.ProviderCodex, AuthMode: account.AuthModeOAuth}
	scheduler = NewScheduler([]Score{
		{AccountID: sub.ID, Provider: sub.Provider, Headroom: 0.50, ShortHeadroom: 0.50},
		{AccountID: other.ID, Provider: other.Provider, Headroom: 0.45, ShortHeadroom: 0.45},
	}).WithLiveDebits(map[string]int{ScoreKey(sub.Provider, sub.ID): 6})
	if picked, _ := scheduler.PickBest([]account.Account{key, sub, other}); picked.ID != other.ID {
		t.Fatalf("PickBest = %q, want the undebited subscription account", picked.ID)
	}
}
