package proxy

import (
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/selectacct"
)

// claudeRetryCandidatesFromScores uses the *same* quota measurements that
// drive the controller's status/placement views. No usage HTTP fetch is made.
//
// New sessions may optimistically select an unknown account. A failover has
// already spent an upstream attempt, so prefer confirmed usable quota before
// trying an optimistic default (which scores as 100% headroom).
//
// Exhaustion with a known future reset cannot recover by replaying the same
// request. Unknown scores and stale scores without a known reset remain a last
// resort.
// The caller still applies authoritative account/model cooldown exclusions.
func claudeRetryCandidatesFromScores(
	base, model selectacct.Scheduler,
	candidates []accounts.Account,
	poolModel string,
	now time.Time,
) []accounts.Account {
	modelKey := selectacct.ModelKey(poolModel)
	hasDedicatedPool := modelKey != "" && base.HasModelPool(poolModel)
	// Keep measured headroom ahead of optimistic or stale scores. The caller
	// still applies explicit controller marks before reaching this function.
	measured := make([]accounts.Account, 0, len(candidates))
	possible := make([]accounts.Account, 0, len(candidates))
	uncertain := make([]accounts.Account, 0, len(candidates))
	for _, candidate := range candidates {
		provider := schedulerAccountProvider(candidate.Provider)
		baseScore := base.ScoreFor(provider, candidate.ID)
		modelScore := model.ScoreFor(provider, candidate.ID)
		if baseScore.Fresh && hasDedicatedPool {
			_, hasModelBucket := baseScore.ModelScores[modelKey]
			support := baseScore.MissingModelSupport
			if support == selectacct.ModelSupportProviderDefault {
				support = selectacct.DefaultMissingModelSupport(provider)
			}
			if !hasModelBucket && support == selectacct.ModelSupportUnsupported {
				// A fresh reading with a known-absent model bucket is
				// stronger evidence than an optimistic retry fallback.
				continue
			}
		}
		if model.Exhausted(provider, candidate.ID) {
			if resetAt, known := modelScore.ExhaustionClearsAt(); known && resetAt.After(now) {
				// A known future reset cannot recover by replaying the same
				// request, whether the score is fresh or carried forward.
				continue
			}
			// A fresh exhausted score without a reset is not headroom, but it
			// also is not a known future reset. Keep it as a last resort after
			// accounts with measurable capacity and unknown scores. A stale
			// exhausted score follows the same last-resort path.
			uncertain = append(uncertain, candidate)
			continue
		}
		if baseScore.Fresh {
			measured = append(measured, candidate)
		} else {
			possible = append(possible, candidate)
		}
	}
	if len(measured) > 0 {
		return measured
	}
	return append(possible, uncertain...)
}

// claudeNewSessionCandidatesFromScores prefers an account with confirmed,
// model-eligible capacity when assigning a brand-new conversation. The shared
// scheduler's unknown-account default is optimistically 100% healthy, which
// otherwise places fresh sessions there ahead of accounts with measured quota.
//
// A missing measurement remains eligible as a conservative subscription-first
// choice; a fresh dedicated-pool reading without the requested bucket is
// excluded. If no subscription has capacity, return the original pool so the
// scheduler can choose the API-key last resort. Callers must only use this for
// initial placement, not existing sticky assignments.
func claudeNewSessionCandidatesFromScores(
	base, model selectacct.Scheduler,
	candidates []accounts.Account,
	poolModel string,
) []accounts.Account {
	// A Claude subscription is always the cheapest route while it has quota.
	// In particular, a behind-pace API-key credit grant is not permission to
	// spend metered traffic while an OAuth account can serve the request.
	if subscriptions := claudeSubscriptionCandidatesFromScores(base, model, candidates, poolModel); len(subscriptions) > 0 {
		return subscriptions
	}
	return candidates
}

// claudeSubscriptionCandidatesFromScores returns OAuth accounts that still
// have quota for a Claude request. It deliberately uses the measured
// exhaustion decision rather than the new-session headroom floor: a
// subscription with 10% left is still cheaper than a metered API key. Unknown
// scores remain eligible, while a fresh dedicated-pool reading with no bucket
// remains ineligible for that model.
func claudeSubscriptionCandidatesFromScores(
	base, model selectacct.Scheduler,
	candidates []accounts.Account,
	poolModel string,
) []accounts.Account {
	modelKey := selectacct.ModelKey(poolModel)
	hasDedicatedPool := modelKey != "" && base.HasModelPool(poolModel)
	verified := make([]accounts.Account, 0, len(candidates))
	constrained := make([]accounts.Account, 0, len(candidates))
	uncertain := make([]accounts.Account, 0, len(candidates))
	for _, candidate := range candidates {
		if candidate.AuthMode != accounts.AuthModeOAuth {
			continue
		}
		provider := schedulerAccountProvider(candidate.Provider)
		baseScore := base.ScoreFor(provider, candidate.ID)
		if model.Exhausted(provider, candidate.ID) {
			continue
		}
		if hasDedicatedPool && baseScore.Fresh {
			// A fresh account-wide reading is not proof that this account
			// can serve a separately measured model quota pool.
			if _, hasBucket := baseScore.ModelScores[modelKey]; !hasBucket {
				continue
			}
		}
		switch {
		case baseScore.Fresh && model.UsableForNewSession(provider, candidate.ID):
			verified = append(verified, candidate)
		case baseScore.Fresh:
			constrained = append(constrained, candidate)
		default:
			uncertain = append(uncertain, candidate)
		}
	}
	if len(verified) > 0 {
		// Preserve the measured-healthy admission preference: optimistic or
		// stale subscriptions must not dilute a confirmed quota choice.
		return verified
	}
	return append(constrained, uncertain...)
}
