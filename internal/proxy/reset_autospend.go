package proxy

import (
	"context"
	"log/slog"
	"math/rand/v2"
	"time"
)

// resetCreditRecookGrace is how soon after its natural weekly reset an
// account could plausibly be cooked again. A credit that expires before
// then has no later use on that account: the only chance to spend it is
// while the account is cooked now.
const resetCreditRecookGrace = 48 * time.Hour

// expiringCreditWorthSpending reports that a cooked candidate's soonest
// credit is lost unless it is spent now: it expires before the account's
// natural reset plus resetCreditRecookGrace. A credit is per account and
// cannot move, so spending it costs nothing and saves the account's whole
// remaining wait. Waits under resetCreditExpiringMinWait are left alone;
// the account recovers on its own about as fast.
//
// Example: 0% weekly left, natural reset in 5 days, credit expiring in 7
// days. After the reset the account would have to cook again within two
// days to use the credit, so it is spent now and saves five days.
func expiringCreditWorthSpending(c rateLimitResetCandidate, now time.Time) bool {
	if c.wait < resetCreditExpiringMinWait || c.creditExpires.IsZero() {
		return false
	}
	naturalReset := now.Add(time.Duration(c.wait) * time.Second)
	return !c.creditExpires.After(naturalReset.Add(resetCreditRecookGrace))
}

// spendExpiringResetCredits redeems a credit on every cooked account whose
// soonest credit would otherwise lapse (expiringCreditWorthSpending).
// Redemption re-checks each account's live usage first, so an account that
// recovered or was reset by hand since the scan is skipped.
func (s Server) spendExpiringResetCredits(ctx context.Context, now time.Time) []RateLimitResetResult {
	candidates, failures, err := s.rateLimitResetCandidates(ctx, resetCreditExpiringMinWait)
	if err != nil {
		return []RateLimitResetResult{{Error: err.Error()}}
	}
	spend := candidates[:0]
	for _, c := range candidates {
		if expiringCreditWorthSpending(c, now) {
			spend = append(spend, c)
		}
	}
	sortResetCandidates(spend, now)
	return append(s.redeemRateLimitResetCandidates(ctx, spend, false), failures...)
}

// RunExpiringResetSpender spends expiring rate-limit reset credits in the
// background every interval (plus up to a tenth of jitter). Only the server
// holds the OAuth tokens that can redeem, so this runs here rather than in
// sr. It stays off in team mode for the same reason the usage refresher
// does: refreshing local OAuth tokens there rotates tokens the vault owns.
func (s Server) RunExpiringResetSpender(ctx context.Context, interval time.Duration) {
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
		for _, res := range s.spendExpiringResetCredits(ctx, time.Now()) {
			switch {
			case res.Reset:
				logger.Info("spent an expiring rate-limit reset credit",
					"account", res.Email, "weekly_wait_seconds", res.WeeklyWaitSeconds, "credit_expires_at", res.CreditExpiresAt)
			case res.Error != "":
				logger.Warn("expiring reset credit sweep could not act on account", "account", res.Email, "error", res.Error)
			}
		}
		timer.Reset(interval + jitter())
	}
}
