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
// A missing or stale measurement never makes an account ineligible: if no
// verified usable choice exists, return the original pool unchanged. Callers
// must only use this for initial placement, not existing sticky assignments.
func claudeNewSessionCandidatesFromScores(
	base, model selectacct.Scheduler,
	candidates []accounts.Account,
	poolModel string,
) []accounts.Account {
	modelKey := selectacct.ModelKey(poolModel)
	hasDedicatedPool := modelKey != "" && base.HasModelPool(poolModel)
	verified := make([]accounts.Account, 0, len(candidates))
	for _, candidate := range candidates {
		if candidate.AuthMode != accounts.AuthModeOAuth {
			continue
		}
		provider := schedulerAccountProvider(candidate.Provider)
		baseScore := base.ScoreFor(provider, candidate.ID)
		if !baseScore.Fresh || !model.UsableForNewSession(provider, candidate.ID) {
			continue
		}
		if hasDedicatedPool {
			// A fresh account-wide reading is not proof that this account
			// can serve a separately measured model quota pool.
			if _, hasBucket := baseScore.ModelScores[modelKey]; !hasBucket {
				continue
			}
		}
		verified = append(verified, candidate)
	}
	if len(verified) > 0 {
		return verified
	}
	return candidates
}
