package proxy

import (
	"context"
	"log/slog"
	"math/rand/v2"
	"time"
)

// A redeemed credit restarts the account's weekly clock, so it is worth
// W/7 of a window, where W is the natural wait it skips: every later reset
// arrives W earlier. Spending at a small W burns a credit for almost
// nothing; spending right after the account drains a fresh window is worth
// nearly a whole one.
//
// If the account keeps its pace, its next cook moment comes a full week
// from now with the same W (it resets after W, then drains in 7d - W). So
// there is rarely a better moment to wait for, and two reasons to spend now:
//
//   - resetCreditGoodWait: the account cooked early in its window, so this
//     W is as good as a credit gets; holding only risks the expiry.
//   - resetCreditNextCookHorizon: the credit expires before the account's
//     next cook moment, so any W beats letting it lapse.
//
// Waits under resetCreditExpiringMinWait are never worth a credit.
const (
	resetCreditGoodWait        = 4 * 24 * time.Hour
	resetCreditNextCookHorizon = 7 * 24 * time.Hour
)

// creditWorthSpending applies the rule above to a cooked candidate and its
// soonest-expiring credit.
func creditWorthSpending(c rateLimitResetCandidate, now time.Time) bool {
	wait := time.Duration(c.wait) * time.Second
	if c.wait < resetCreditExpiringMinWait {
		return false
	}
	if wait >= resetCreditGoodWait {
		return true
	}
	return !c.creditExpires.IsZero() && !c.creditExpires.After(now.Add(resetCreditNextCookHorizon))
}

// spendResetCredits redeems a credit on every cooked account where
// creditWorthSpending says no later moment beats now.
// Redemption re-checks each account's live usage first, so an account that
// recovered or was reset by hand since the scan is skipped.
func (s Server) spendResetCredits(ctx context.Context, now time.Time) []RateLimitResetResult {
	candidates, failures, err := s.rateLimitResetCandidates(ctx, resetCreditExpiringMinWait)
	if err != nil {
		return []RateLimitResetResult{{Error: err.Error()}}
	}
	spend := candidates[:0]
	for _, c := range candidates {
		if creditWorthSpending(c, now) {
			spend = append(spend, c)
		}
	}
	sortResetCandidates(spend, now)
	return append(s.redeemRateLimitResetCandidates(ctx, spend, false), failures...)
}

// RunResetCreditSpender spends rate-limit reset credits in the background
// when creditWorthSpending says now is the best moment, checking every
// interval (plus up to a tenth of jitter). Only the server holds the OAuth
// tokens that can redeem, so this runs here rather than in sr. It stays off in team mode for the same reason the usage refresher
// does: refreshing local OAuth tokens there rotates tokens the vault owns.
func (s Server) RunResetCreditSpender(ctx context.Context, interval time.Duration) {
	if interval <= 0 || s.AccountRef == nil || normalizedCredentialBroker(s.CredentialBroker) != nil {
		return
	}
	logger := s.Logger
	if logger == nil {
		logger = slog.Default()
	}
	jitter := func() time.Duration { return rand.N(interval/10 + 1) }
	timer := time.NewTimer(interval/10 + jitter())
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		if s.Lifecycle.Draining() || ctx.Err() != nil {
			return
		}
		for _, res := range s.spendResetCredits(ctx, time.Now()) {
			switch {
			case res.Reset:
				logger.Info("spent a rate-limit reset credit",
					"account", res.Email, "weekly_wait_seconds", res.WeeklyWaitSeconds, "credit_expires_at", res.CreditExpiresAt)
			case res.Error != "":
				logger.Warn("reset credit sweep could not act on account", "account", res.Email, "error", res.Error)
			}
		}
		timer.Reset(interval + jitter())
	}
}
