package proxy

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/selectacct"
)

// In team mode the vault owns the credentials and refreshes them. The provider
// rotates the refresh token on every use, so a second refresher invalidates the
// vault's chain and the vault invalidates this machine's: both sides end up
// dead. Observed in production across a laptop and a mac mini sharing one
// account store, where 11 of 12 accounts failed with refresh_token_reused.
//
// So a server with a credential broker must never refresh a local OAuth
// account, on any path.
func TestTeamModeNeverRefreshesLocalAccounts(t *testing.T) {
	local := []accounts.Account{
		{ID: "a@example.com", Provider: accounts.ProviderCodex, AuthMode: accounts.AuthModeOAuth, Token: "a"},
		{ID: "b@example.com", Provider: accounts.ProviderCodex, AuthMode: accounts.AuthModeOAuth, Token: "b"},
	}

	newServer := func(refreshes *int32, credBroker CredentialBroker) Server {
		return Server{
			Accounts:     local,
			SchedulerRef: selectacct.NewSchedulerRef(selectacct.NewScheduler(nil)),
			AccountRef:   NewAccountRef(accounts.CodexStore{Dir: t.TempDir()}, local, nil),
			RefreshAccountFn: func(_ context.Context, a accounts.Account) (accounts.Account, error) {
				atomic.AddInt32(refreshes, 1)
				return a, nil
			},
			CredentialBroker: credBroker,
			UsageScoreTTL:    0,
		}
	}

	t.Run("reloadAccounts", func(t *testing.T) {
		var teamRefreshes int32
		team := newServer(&teamRefreshes, &fakeCredentialBroker{})
		if _, _, err := team.reloadAccounts(context.Background()); err != nil {
			t.Fatalf("reload in team mode: %v", err)
		}
		if got := atomic.LoadInt32(&teamRefreshes); got != 0 {
			t.Fatalf("team mode refreshed %d local accounts, want 0", got)
		}
	})

	t.Run("refreshUsageScoresIfStale", func(t *testing.T) {
		var teamRefreshes int32
		team := newServer(&teamRefreshes, &fakeCredentialBroker{})
		team.refreshUsageScoresIfStale(context.Background())
		if got := atomic.LoadInt32(&teamRefreshes); got != 0 {
			t.Fatalf("team mode refreshed %d local accounts while scoring, want 0", got)
		}
	})

	// Local mode is the other half of the contract: with no broker this machine
	// is the only refresher, so it must still refresh or scores go stale.
	t.Run("local mode still refreshes", func(t *testing.T) {
		var localRefreshes int32
		solo := newServer(&localRefreshes, nil)
		if _, _, err := solo.reloadAccounts(context.Background()); err != nil {
			t.Fatalf("reload in local mode: %v", err)
		}
		if got := atomic.LoadInt32(&localRefreshes); got == 0 {
			t.Fatal("local mode refreshed nothing; the gate is too wide")
		}
	})
}
