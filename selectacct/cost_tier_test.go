package selectacct

import (
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/account"
)

// A key held out for spent credits must rank behind exhausted subscriptions:
// the Claude proxy reaches paid extra usage only from an exhausted
// subscription pick, so a dead key ahead of them would shadow that tier.
func TestPickRanksExhaustedAPIKeyBehindExhaustedSubscription(t *testing.T) {
	ref := NewSchedulerRef(NewScheduler([]Score{
		{AccountID: "plan", Provider: account.ProviderClaude, Headroom: 0, ShortHeadroom: 0},
		{AccountID: "claude:spent", Provider: account.ProviderClaude, Headroom: 0.01, ShortHeadroom: 0.01, MissingModelSupport: ModelSupportUnknown},
	}))
	candidates := []account.Account{
		{ID: "claude:spent", Provider: account.ProviderClaude, AuthMode: account.AuthModeAPIKey},
		{ID: "plan", Provider: account.ProviderClaude, AuthMode: account.AuthModeOAuth},
	}
	got, err := ref.Get().Pick(candidates)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "claude:spent" {
		t.Fatalf("got %q, want the unspent key ahead of an exhausted subscription", got.ID)
	}

	ref.MarkCreditExhaustedUntil(account.ProviderClaude, "claude:spent", time.Now().Add(time.Hour))
	scheduler := ref.Get()
	if tier := scheduler.SelectionTier(candidates[0]); tier != SelectionTierExhaustedAPIKey {
		t.Fatalf("spent key tier = %d, want %d", tier, SelectionTierExhaustedAPIKey)
	}
	got, err = scheduler.Pick(candidates)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "plan" {
		t.Fatalf("got %q, want the exhausted subscription ahead of the spent key", got.ID)
	}
	if _, held := ref.CreditExhaustedUntil(account.ProviderClaude, "claude:spent", time.Now()); !held {
		t.Fatal("credit hold not reported")
	}
	if _, held := ref.CreditExhaustedUntil(account.ProviderClaude, "claude:spent", time.Now().Add(2*time.Hour)); held {
		t.Fatal("credit hold reported after its expiry")
	}
}

// An API key has no subscription model pools. Declaring missing buckets
// unknown keeps it routable for Opus/Sonnet requests instead of scoring it
// zero (and therefore exhausted) in every pool another account reports.
func TestAPIKeyWithUnknownModelSupportStaysRoutableInModelPool(t *testing.T) {
	scheduler := NewScheduler([]Score{
		{AccountID: "plan", Provider: account.ProviderClaude, Headroom: 0.9, ShortHeadroom: 0.9,
			ModelScores: map[string]Score{"opus": {AccountID: "plan", Provider: account.ProviderClaude, Headroom: 0, ShortHeadroom: 0}}},
		{AccountID: "claude:key", Provider: account.ProviderClaude, Headroom: 0.01, ShortHeadroom: 0.01, MissingModelSupport: ModelSupportUnknown},
	}).ForModel("opus")
	key := account.Account{ID: "claude:key", Provider: account.ProviderClaude, AuthMode: account.AuthModeAPIKey}
	if scheduler.Exhausted(account.ProviderClaude, key.ID) {
		t.Fatal("API key scored exhausted in a model pool it has no bucket for")
	}
	got, err := scheduler.Pick([]account.Account{
		{ID: "plan", Provider: account.ProviderClaude, AuthMode: account.AuthModeOAuth},
		key,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != key.ID {
		t.Fatalf("got %q, want the API key while the plan's opus pool is exhausted", got.ID)
	}
}
