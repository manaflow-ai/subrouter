package proxy

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"sort"
	"strings"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
)

// ResetCreditAutospend is what the background reset-credit spender does with
// its decisions (--reset-credit-autospend).
type ResetCreditAutospend string

const (
	// ResetCreditAutospendOff evaluates nothing.
	ResetCreditAutospendOff ResetCreditAutospend = "off"
	// ResetCreditAutospendWarn makes the same decisions as spend but redeems
	// nothing: each one is logged as "would spend" and shown as reset_advice
	// on /_subrouter/usage-status. This is the default: a credit is a real,
	// irreversible action on a shared account, so an operator sees what the
	// rule would do before letting it act.
	ResetCreditAutospendWarn ResetCreditAutospend = "warn"
	// ResetCreditAutospendSpend redeems a credit for each decision.
	ResetCreditAutospendSpend ResetCreditAutospend = "spend"
)

// ParseResetCreditAutospend accepts off, warn or spend (case-insensitive).
func ParseResetCreditAutospend(raw string) (ResetCreditAutospend, error) {
	switch mode := ResetCreditAutospend(strings.ToLower(strings.TrimSpace(raw))); mode {
	case ResetCreditAutospendOff, ResetCreditAutospendWarn, ResetCreditAutospendSpend:
		return mode, nil
	}
	return "", fmt.Errorf("--reset-credit-autospend must be off, warn or spend, got %q", raw)
}

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
// Waits under resetCreditExpiringMinWait are never worth a credit on those
// grounds.
const (
	resetCreditGoodWait        = 4 * 24 * time.Hour
	resetCreditNextCookHorizon = 7 * 24 * time.Hour
)

// The W/7 rule values long-run throughput only. When almost the whole Codex
// pool is cooked and people are sending requests, a fresh window now is worth
// more than W/7: it is the difference between work proceeding and stopping.
// So a third, bounded reason:
//
//   - the pool is blocked: at most resetCreditBlockedFraction of its accounts
//     (at least one) can take a request, measured from live usage;
//   - there is demand: a Codex request was routed or turned away within
//     resetCreditDemandWindow;
//
// then spend on the cooked accounts with the largest W first (most value
// per credit), only where W >= resetCreditBlockedMinWait (below that the
// account recovers on its own soon), and only until
// resetCreditBlockedHeadroom accounts above the threshold can serve, so one
// busy moment cannot drain the bank.
const (
	resetCreditBlockedFraction = 0.05
	resetCreditBlockedMinWait  = int64(2 * 60 * 60)
	resetCreditBlockedHeadroom = 2
	resetCreditDemandWindow    = 15 * time.Minute
	// resetCreditBlockedCheckInterval is how often the spender looks for a
	// blocked pool between its regular sweeps. The look itself is in-process
	// (scheduler scores and the demand timestamp); only a pool that looks
	// blocked with demand pays for a live usage scan.
	resetCreditBlockedCheckInterval = 5 * time.Minute
	// resetCreditAdviceTTL drops warn-mode advice that no sweep has
	// re-confirmed, e.g. after the spender stopped.
	resetCreditAdviceTTL = 2 * time.Hour
)

// Decision reasons, as logged and shown in reset_advice.
const (
	resetReasonEarlyCook   = "early_cook"
	resetReasonExpiring    = "expiring"
	resetReasonPoolBlocked = "pool_blocked"
)

// creditSpendReason applies the W/7 rule to a cooked candidate and its
// soonest-expiring credit, returning why now beats later, or "" to hold.
func creditSpendReason(c rateLimitResetCandidate, now time.Time) string {
	if c.wait < resetCreditExpiringMinWait {
		return ""
	}
	if time.Duration(c.wait)*time.Second >= resetCreditGoodWait {
		return resetReasonEarlyCook
	}
	if !c.creditExpires.IsZero() && !c.creditExpires.After(now.Add(resetCreditNextCookHorizon)) {
		return resetReasonExpiring
	}
	return ""
}

// creditWorthSpending reports whether the W/7 rule alone spends on c.
func creditWorthSpending(c rateLimitResetCandidate, now time.Time) bool {
	return creditSpendReason(c, now) != ""
}

// resetBlockedThreshold is the most serving accounts a pool of total can
// have and still count as blocked.
func resetBlockedThreshold(total int) int {
	return max(1, int(float64(total)*resetCreditBlockedFraction))
}

// resetPoolState is the pool as the spender sees it for one decision.
type resetPoolState struct {
	total   int
	serving int
	demand  bool
}

func (p resetPoolState) blocked() bool {
	return p.total > 0 && p.serving <= resetBlockedThreshold(p.total)
}

type resetCreditDecision struct {
	candidate rateLimitResetCandidate
	reason    string
}

// planResetCreditSpends picks the candidates to spend on now: every one the
// W/7 rule accepts, then, if the pool is blocked with demand, the largest-W
// remaining ones (W >= resetCreditBlockedMinWait) until the threshold plus
// resetCreditBlockedHeadroom accounts can serve. Spends from the W/7 rule
// count toward that target.
func planResetCreditSpends(candidates []rateLimitResetCandidate, pool resetPoolState, now time.Time) []resetCreditDecision {
	ordered := append([]rateLimitResetCandidate(nil), candidates...)
	sortResetCandidates(ordered, now)
	var plan []resetCreditDecision
	var rest []rateLimitResetCandidate
	for _, c := range ordered {
		if reason := creditSpendReason(c, now); reason != "" {
			plan = append(plan, resetCreditDecision{candidate: c, reason: reason})
		} else {
			rest = append(rest, c)
		}
	}
	if !pool.blocked() || !pool.demand {
		return plan
	}
	need := resetBlockedThreshold(pool.total) + resetCreditBlockedHeadroom - pool.serving - len(plan)
	sort.SliceStable(rest, func(i, j int) bool {
		if rest[i].wait != rest[j].wait {
			return rest[i].wait > rest[j].wait
		}
		return rest[i].account.ID < rest[j].account.ID
	})
	for _, c := range rest {
		if need <= 0 || c.wait < resetCreditBlockedMinWait {
			break
		}
		plan = append(plan, resetCreditDecision{candidate: c, reason: resetReasonPoolBlocked})
		need--
	}
	return plan
}

// codexAccountServing reports whether live usage leaves the account able to
// take a request: not cooked on its weekly window and no account-wide
// window at its limit.
func codexAccountServing(details accounts.CodexUsageDetails) bool {
	if rateLimitCooked(details) {
		return false
	}
	for _, window := range details.Windows {
		if accounts.IsModelScopedWindow(window) || window.ExtraUsage != nil || window.LimitWindowSeconds <= 0 {
			continue
		}
		if window.UsedPercent >= 100 {
			return false
		}
	}
	return true
}

// recentCodexDemand reports whether this process routed or turned away a
// Codex request within resetCreditDemandWindow of now.
func (s Server) recentCodexDemand(now time.Time) bool {
	last := s.SchedulerRef.LastDemand(schedulerAccountProvider(accounts.ProviderCodex))
	return !last.IsZero() && now.Sub(last) <= resetCreditDemandWindow
}

// codexPoolLooksBlocked is the cheap, in-process check the spender runs
// between sweeps: recent demand, and the scheduler's current scores say the
// Codex OAuth pool is blocked. It only decides whether a live scan is worth
// running; the scan makes the actual decision.
func (s Server) codexPoolLooksBlocked(now time.Time) bool {
	if s.SchedulerRef == nil || s.AccountRef == nil || !s.recentCodexDemand(now) {
		return false
	}
	pool := oauthAccounts(filterAccountsForProvider(s.AccountRef.All(), accounts.ProviderCodex))
	scheduler := s.SchedulerRef.Get()
	serving := 0
	for _, account := range pool {
		if !scheduler.Exhausted(schedulerAccountProvider(account.Provider), account.ID) {
			serving++
		}
	}
	return resetPoolState{total: len(pool), serving: serving}.blocked()
}

// spendResetCredits scans the pool, plans with planResetCreditSpends and, in
// spend mode, redeems each decision. Redemption re-checks each account's live
// usage first, so an account that recovered or was reset by hand since the
// scan is skipped. In warn mode nothing is redeemed: each decision comes back
// as a dry-run result and is recorded as reset advice.
func (s Server) spendResetCredits(ctx context.Context, now time.Time, mode ResetCreditAutospend) []RateLimitResetResult {
	if mode != ResetCreditAutospendWarn && mode != ResetCreditAutospendSpend {
		return nil
	}
	scan, err := s.scanRateLimitReset(ctx, min(resetCreditExpiringMinWait, resetCreditBlockedMinWait))
	if err != nil {
		return []RateLimitResetResult{{Error: err.Error()}}
	}
	pool := resetPoolState{total: scan.total, serving: scan.serving, demand: s.recentCodexDemand(now)}
	plan := planResetCreditSpends(scan.candidates, pool, now)

	if mode == ResetCreditAutospendWarn {
		results := make([]RateLimitResetResult, 0, len(plan)+len(scan.failures))
		advice := make(map[string]ResetCreditAdvice, len(plan))
		for _, d := range plan {
			res := RateLimitResetResult{Email: d.candidate.account.ID, Eligible: true, DryRun: true, WeeklyWaitSeconds: d.candidate.wait, Reason: d.reason}
			if !d.candidate.creditExpires.IsZero() {
				res.CreditExpiresAt = d.candidate.creditExpires.UTC().Format(time.RFC3339)
			}
			results = append(results, res)
			advice[d.candidate.account.ID] = ResetCreditAdvice{Reason: d.reason, WSeconds: d.candidate.wait, CreditExpiresAt: res.CreditExpiresAt, EvaluatedAt: now.UTC()}
		}
		s.AccountRef.setResetAdvice(advice)
		return append(results, scan.failures...)
	}

	s.AccountRef.setResetAdvice(nil)
	spend := make([]rateLimitResetCandidate, 0, len(plan))
	for _, d := range plan {
		spend = append(spend, d.candidate)
	}
	results := s.redeemRateLimitResetCandidates(ctx, spend, false)
	for i := range results {
		results[i].Reason = plan[i].reason
	}
	for _, res := range results {
		if res.Reset {
			// Same as the reset endpoint: status reads must show the fresh
			// window, not the cached pre-reset picture.
			s.AccountRef.InvalidateUsageStatusCache()
			break
		}
	}
	return append(results, scan.failures...)
}

// ResetCreditAdvice is a decision the spender made but did not execute
// (warn mode): why, the weekly wait W a credit would skip, and the expiry
// of the credit it would use. It carries no credential material.
type ResetCreditAdvice struct {
	Reason          string    `json:"reason"`
	WSeconds        int64     `json:"w_seconds"`
	CreditExpiresAt string    `json:"credit_expires_at,omitempty"`
	EvaluatedAt     time.Time `json:"evaluated_at"`
}

// setResetAdvice replaces the advice set with the latest sweep's.
func (r *AccountRef) setResetAdvice(advice map[string]ResetCreditAdvice) {
	if r == nil {
		return
	}
	r.resetAdviceMu.Lock()
	defer r.resetAdviceMu.Unlock()
	r.resetAdvice = advice
}

// resetAdviceFor returns current advice for an account, if any.
func (r *AccountRef) resetAdviceFor(id string, now time.Time) (ResetCreditAdvice, bool) {
	if r == nil {
		return ResetCreditAdvice{}, false
	}
	r.resetAdviceMu.Lock()
	defer r.resetAdviceMu.Unlock()
	advice, ok := r.resetAdvice[id]
	if !ok || now.Sub(advice.EvaluatedAt) > resetCreditAdviceTTL {
		return ResetCreditAdvice{}, false
	}
	return advice, true
}

// withResetAdvice attaches warn-mode advice to Codex OAuth statuses. The
// input may be the shared cached snapshot, so it is copied, not mutated.
func (r *AccountRef) withResetAdvice(statuses []AccountUsageStatus) []AccountUsageStatus {
	out := append([]AccountUsageStatus(nil), statuses...)
	now := time.Now()
	for i := range out {
		out[i].ResetAdvice = nil
		if out[i].Provider != accounts.ProviderCodex || out[i].AuthMode != accounts.AuthModeOAuth {
			continue
		}
		if advice, ok := r.resetAdviceFor(out[i].ID, now); ok {
			out[i].ResetAdvice = &advice
		}
	}
	return out
}

// RunResetCreditSpender evaluates rate-limit reset credits in the background
// and, depending on mode, logs (warn) or redeems (spend) each decision. A
// full sweep runs every interval (plus up to a tenth of jitter). Between
// sweeps, a cheap in-process check every resetCreditBlockedCheckInterval runs
// an early sweep when the Codex pool looks blocked with demand, so a blocked
// team waits minutes, not up to an hour. Only the server holds the OAuth
// tokens that can redeem, so this runs here rather than in sr. It stays off
// in team mode for the same reason the usage refresher does: refreshing
// local OAuth tokens there rotates tokens the vault owns.
func (s Server) RunResetCreditSpender(ctx context.Context, mode ResetCreditAutospend, interval time.Duration) {
	if mode == ResetCreditAutospendOff || interval <= 0 || s.AccountRef == nil || normalizedCredentialBroker(s.CredentialBroker) != nil {
		return
	}
	logger := s.Logger
	if logger == nil {
		logger = slog.Default()
	}
	jitter := func(d time.Duration) time.Duration { return rand.N(d/10 + 1) }
	tick := min(interval, resetCreditBlockedCheckInterval)
	nextSweep := time.Now().Add(interval/10 + jitter(interval))
	timer := time.NewTimer(min(tick, time.Until(nextSweep)))
	defer timer.Stop()
	logged := map[string]string{}
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		if s.Lifecycle.Draining() || ctx.Err() != nil {
			return
		}
		now := time.Now()
		full := !now.Before(nextSweep)
		if full || s.codexPoolLooksBlocked(now) {
			logged = logResetCreditResults(logger, s.spendResetCredits(ctx, now, mode), logged)
		}
		if full {
			nextSweep = now.Add(interval + jitter(interval))
		}
		timer.Reset(min(tick+jitter(tick), max(time.Until(nextSweep), 0)))
	}
}

// logResetCreditResults logs one sweep. Warn-mode advice is logged when it
// first appears or its reason changes, so a pool that stays blocked does not
// repeat the same lines every few minutes; it returns the advice now current.
func logResetCreditResults(logger *slog.Logger, results []RateLimitResetResult, logged map[string]string) map[string]string {
	current := map[string]string{}
	for _, res := range results {
		switch {
		case res.Reset:
			logger.Info("spent a rate-limit reset credit",
				"account", res.Email, "reason", res.Reason, "weekly_wait_seconds", res.WeeklyWaitSeconds, "credit_expires_at", res.CreditExpiresAt)
		case res.DryRun && res.Eligible:
			current[res.Email] = res.Reason
			if logged[res.Email] == res.Reason {
				continue
			}
			logger.Warn("would spend a rate-limit reset credit (--reset-credit-autospend=warn)",
				"account", res.Email, "reason", res.Reason, "weekly_wait_seconds", res.WeeklyWaitSeconds, "credit_expires_at", res.CreditExpiresAt)
		case res.Error != "":
			logger.Warn("reset credit sweep could not act on account", "account", res.Email, "reason", res.Reason, "error", res.Error)
		}
	}
	return current
}
