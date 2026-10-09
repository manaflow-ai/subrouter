package proxy

import (
	"bytes"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/selectacct"
)

// Claude cost order. Claude traffic spends the cheapest money first:
//
//  1. plan: Claude Max/Pro subscription quota (OAuth and setup-token
//     profiles). Already paid for.
//  2. api-credits: Anthropic API keys. Each is expected to belong to a
//     Console organization funded by free monthly credits, so it costs no
//     money until its balance is spent.
//  3. extra-usage: a subscription's paid overage, bounded by the account's
//     own monthly spend limit and never used on an unknown balance. Used
//     only when every plan is unusable for the request (5h, weekly, or model
//     pool exhausted) and no untried, unheld API key can take it.
//  4. not in subrouter: with tiers 1-3 spent the pool answers its own 503
//     with Retry-After, which `cr claude` turns into its Bedrock route.
//
// The order is enforced in three places: selectacct's selection tiers rank
// plan before API keys before exhausted plans; the placement and failover
// paths reach the extra-usage fallback only from an exhausted plan pick and
// only when no API key is available; and a spent credit balance holds its key
// out of routing (MarkCreditExhaustedUntil) so the pool can reach tier 3.
const (
	claudeCostTierPlan       = "plan"
	claudeCostTierAPICredits = "api-credits"
	claudeCostTierExtraUsage = "extra-usage"
)

// ClaudeCostOrder is the human form of the order above, for status output.
const ClaudeCostOrder = "plan > API credits > extra usage > 503 (client falls back to Bedrock)"

// claudeCreditExhaustionHold is how long a key whose credit balance is spent
// stays out of routing before one request probes it again. Anthropic does not
// expose a key's balance or its grant's reset date, so the hold is a re-probe
// interval rather than a reset time: a probe costs one rejected request
// (Anthropic answers it before any tokens are billed), and a monthly grant or
// a manual top-up is picked up within the hour.
const claudeCreditExhaustionHold = time.Hour

// claudeCreditExhaustedMessage matches Anthropic's billing rejection, e.g.
// "Your credit balance is too low to access the Anthropic API. Please go to
// Plans & Billing to upgrade or purchase credits."
func claudeCreditExhaustedMessage(body []byte) bool {
	return strings.Contains(normalizedProviderErrorMessage(body), "credit balance is too low")
}

func claudeCreditExhaustedStatus(code int) bool {
	return code == http.StatusBadRequest || code == http.StatusPaymentRequired || code == http.StatusForbidden
}

// responseClaudeCreditExhausted reports whether an Anthropic error response
// says the organization's prepaid credit balance is spent. It reads at most
// usageLimitInspectMaxBytes and always leaves the body readable for the
// caller. Successful responses (including streams) are never read.
func responseClaudeCreditExhausted(response *http.Response) (bool, error) {
	if response == nil || response.Body == nil || !claudeCreditExhaustedStatus(response.StatusCode) {
		return false, nil
	}
	body := response.Body
	prefix, err := io.ReadAll(io.LimitReader(body, usageLimitInspectMaxBytes+1))
	if err != nil || int64(len(prefix)) > usageLimitInspectMaxBytes {
		response.Body = prefixReadCloser{Reader: io.MultiReader(bytes.NewReader(prefix), body), Closer: body}
		return false, err
	}
	closeErr := body.Close()
	response.Body = io.NopCloser(bytes.NewReader(prefix))
	if closeErr != nil {
		return false, closeErr
	}
	return claudeCreditExhaustedMessage(prefix), nil
}

// markClaudeCreditExhausted holds a Claude API key out of routing until the
// next re-probe. The key itself is never logged, only its account ID.
func (s Server) markClaudeCreditExhausted(accountID string) {
	if s.SchedulerRef == nil || accountID == "" {
		return
	}
	until := time.Now().Add(claudeCreditExhaustionHold)
	s.SchedulerRef.MarkCreditExhaustedUntil(schedulerAccountProvider(accounts.ProviderClaude), accountID, until)
	if s.Logger != nil {
		s.Logger.Warn("claude API key credit balance spent; holding it out until re-probe",
			"account", accountID, "until", until.UTC().Format(time.RFC3339))
	}
}

// claudeAPIKeyCandidateAvailable reports whether any Claude API key among
// candidates can still take a request: not exhausted and not held out by a
// live controller mark. Callers pass only untried candidates.
func (s Server) claudeAPIKeyCandidateAvailable(scheduler selectacct.Scheduler, candidates []accounts.Account, poolModel string) bool {
	now := time.Now()
	for _, candidate := range candidates {
		if accountProviderOrCodex(candidate) != accounts.ProviderClaude || candidate.AuthMode != accounts.AuthModeAPIKey {
			continue
		}
		provider := schedulerAccountProvider(candidate.Provider)
		if scheduler.Exhausted(provider, candidate.ID) {
			continue
		}
		if s.SchedulerRef != nil {
			if _, blocked := s.SchedulerRef.ExplicitBlockedUntilFor(provider, candidate.ID, poolModel, now); blocked {
				continue
			}
		}
		return true
	}
	return false
}

// withClaudeCostTiers labels every Claude status row with the cost tier the
// router would spend it in, and when a key's credits are held out.
func (s Server) withClaudeCostTiers(statuses []AccountUsageStatus) []AccountUsageStatus {
	out := append([]AccountUsageStatus(nil), statuses...)
	var scheduler selectacct.Scheduler
	if s.SchedulerRef != nil {
		scheduler = s.scheduler()
	}
	now := time.Now()
	for i := range out {
		if accountProviderFor(out[i].Provider) != accounts.ProviderClaude {
			continue
		}
		switch out[i].AuthMode {
		case accounts.AuthModeAPIKey:
			out[i].CostTier = claudeCostTierAPICredits
			out[i].CostTierRank = 2
			if until, held := s.SchedulerRef.CreditExhaustedUntil(accounts.ProviderClaude, out[i].ID, now); held {
				out[i].CreditsExhaustedUntil = until.UTC()
			}
		case accounts.AuthModeOAuth:
			out[i].CostTier = claudeCostTierPlan
			out[i].CostTierRank = 1
			if s.SchedulerRef == nil {
				continue
			}
			score := scheduler.ScoreFor(accounts.ProviderClaude, out[i].ID)
			if scheduler.Exhausted(accounts.ProviderClaude, out[i].ID) && claudeExtraUsageEligible(score) {
				out[i].CostTier = claudeCostTierExtraUsage
				out[i].CostTierRank = 3
			}
		}
	}
	return out
}
