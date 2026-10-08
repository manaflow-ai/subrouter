package proxy

import (
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
		return time.Time{}, false
	}
	loaded, _ := s.AccountRef.Snapshot()
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
