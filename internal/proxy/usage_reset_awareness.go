package proxy

import (
	"context"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/selectacct"
)

// knownAccountWideQuotaReset returns a provider-measured recovery deadline
// only when an account-wide quota score is exhausted and every binding
// exhausted window has a known reset. Model-specific exhaustion is not enough
// to skip an account's usage fetch: another model family may still be usable.
func knownAccountWideQuotaReset(score selectacct.Score, now time.Time) (time.Time, bool) {
	until, known := score.ExhaustionClearsAt()
	return until, known && !until.IsZero() && until.After(now)
}

// allOAuthAccountsWaitingForReset is true only when every locally managed
// OAuth account is measured exhausted, with a future authoritative reset.
// A missing/unknown score or any healthy account keeps the normal background
// cadence; do not guess a global recovery deadline from partial telemetry.
func (s Server) allOAuthAccountsWaitingForReset(now time.Time) (time.Time, bool) {
	if s.AccountRef == nil || s.SchedulerRef == nil {
		// Account-import/re-login advances the account generation and
		// invalidates the measured score timestamp. Do not apply an old
		// account's deadline to its newly installed credential.
		return time.Time{}, false
	}
	// Refresh the local account view before deciding that an old quota clock
	// still applies. This only reads the published account-generation marker;
	// it never contacts a provider. A replacement login advances the
	// generation and clears UpdatedAt, so the next score pass can verify it.
	allAccounts, _ := s.accountListSnapshotContext(context.Background())
	if s.SchedulerRef.UpdatedAt().IsZero() {
		return time.Time{}, false
	}
	// accountListSnapshotContext includes the server's startup list for
	// compatibility and appends the AccountRef's reloaded list. Prefer the
	// latter when a generation changed so a repaired credential does not get
	// shadowed by its pre-login copy.
	loadedByKey := make(map[string]accounts.Account, len(allAccounts))
	for _, account := range allAccounts {
		if account.AuthMode != accounts.AuthModeOAuth {
			continue
		}
		key := string(accountProviderOrCodex(account)) + "\x00" + account.ID
		loadedByKey[key] = account
	}
	loaded := make([]accounts.Account, 0, len(loadedByKey))
	for _, account := range loadedByKey {
		loaded = append(loaded, account)
	}
	if len(loaded) == 0 {
		return time.Time{}, false
	}
	current := s.SchedulerRef.RefreshSeed()
	var earliest time.Time
	for _, account := range loaded {
		if account.AuthMode != accounts.AuthModeOAuth {
			continue
		}
		score := current.ScoreFor(schedulerAccountProvider(account.Provider), account.ID)
		until, known := knownAccountWideQuotaReset(score, now)
		if !known {
			return time.Time{}, false
		}
		if earliest.IsZero() || until.Before(earliest) {
			earliest = until
		}
	}
	return earliest, !earliest.IsZero()
}
