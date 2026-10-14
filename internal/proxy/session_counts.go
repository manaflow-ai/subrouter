package proxy

import (
	"time"

	"github.com/manaflow-ai/subrouter/account"
	"github.com/manaflow-ai/subrouter/selectacct"
	"github.com/manaflow-ai/subrouter/session"
)

// SchedulerSessionCounts returns persistent assignments keyed the same way as
// scheduler scores. Account labels are not globally unique: a Codex account
// and a Claude profile can legitimately share an email, so bare-ID counts
// would make one provider's sessions damp placement for another. Agent types
// such as pi still map to Codex because they use the Codex credential pool.
func SchedulerSessionCounts(store *session.Store) map[string]int {
	counts := map[string]int{}
	if store == nil {
		return counts
	}
	for _, assignment := range store.All() {
		provider := providerForStoredSession(assignment.AgentType)
		counts[selectacct.ScoreKey(provider, assignment.AccountID)]++
	}
	return counts
}

// placementActiveWindow is how recently a session must have sent a request
// to count as load on its account for placement. The store retains up to
// 512 assignments for stickiness, most of them sessions that ended long ago;
// counting those damped placement onto accounts by conversations that no
// longer ran (2026-10-14: 40-57 "sessions" per Claude account against 1-6
// requests in flight).
const placementActiveWindow = 30 * time.Minute

// SchedulerActiveSessionCounts counts, per scheduler ScoreKey, the sticky
// sessions that sent a request within placementActiveWindow. Routing passes
// these to Scheduler.WithSessionCounts; status views that report how many
// assignments an account holds keep SchedulerSessionCounts.
func SchedulerActiveSessionCounts(store *session.Store) map[string]int {
	counts := map[string]int{}
	if store == nil {
		return counts
	}
	for _, assignment := range store.ActiveSince(time.Now().Add(-placementActiveWindow)) {
		provider := providerForStoredSession(assignment.AgentType)
		counts[selectacct.ScoreKey(provider, assignment.AccountID)]++
	}
	return counts
}

func providerForStoredSession(agentType string) account.Provider {
	normalized := session.NormalizeAgentType(agentType)
	if entry, ok := keyedProviderForName(normalized); ok {
		return accountProviderFor(entry.Provider)
	}
	if normalized == string(account.ProviderAntigravity) {
		return account.ProviderAntigravity
	}
	return providerForAgent(normalized)
}
