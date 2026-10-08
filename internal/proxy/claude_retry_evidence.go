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
// Measured exhaustion with a known future reset cannot recover by replaying
// the same request. Unknown or stale scores stay eligible as a last resort.
// The caller still applies authoritative account/model cooldown exclusions.
func claudeRetryCandidatesFromScores(
	base, model selectacct.Scheduler,
	candidates []accounts.Account,
	now time.Time,
) []accounts.Account {
	verified := make([]accounts.Account, 0, len(candidates))
	possible := make([]accounts.Account, 0, len(candidates))
	for _, candidate := range candidates {
		provider := schedulerAccountProvider(candidate.Provider)
		baseScore := base.ScoreFor(provider, candidate.ID)
		modelScore := model.ScoreFor(provider, candidate.ID)
		if baseScore.Fresh && model.Exhausted(provider, candidate.ID) {
			if resetAt, known := modelScore.ExhaustionClearsAt(); known && resetAt.After(now) {
				// A measured quota window is still exhausted. Avoid sending
				// another upstream call before its actual reset.
				continue
			}
		}
		possible = append(possible, candidate)
		if candidate.AuthMode == accounts.AuthModeOAuth && baseScore.Fresh &&
			model.UsableForNewSession(provider, candidate.ID) {
			verified = append(verified, candidate)
		}
	}
	if len(verified) > 0 {
		return verified
	}
	return possible
}
