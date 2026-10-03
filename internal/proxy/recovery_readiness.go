package proxy

import (
	"net/http"
	"strings"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/selectacct"
)

// recoveryReadyFromFreshUsage requires a fresh, authenticated subscription
// measurement for the exact provider and quota pool. Stale last-good status,
// paid extra usage, and another agent's restored quota cannot wake an alarm.
func recoveryReadyFromFreshUsage(agent, pool string, statuses []AccountUsageStatus) bool {
	provider := accounts.Provider(agent)
	if provider != accounts.ProviderCodex && provider != accounts.ProviderClaude {
		return false
	}
	pool = selectacct.ModelKey(pool)
	for _, status := range statuses {
		if status.Provider != provider || status.AuthMode != accounts.AuthModeOAuth ||
			!status.AuthValid || status.Error != "" || !status.UsageFresh || len(status.Windows) == 0 {
			continue
		}
		baseLimit := false
		for _, window := range status.Windows {
			if window.ExtraUsage == nil && window.Feature == "" && window.LimitWindowSeconds > 0 {
				baseLimit = true
				break
			}
		}
		if !baseLimit {
			continue
		}
		score := scoreFromUsageWindows(provider, status.ID, status.Windows)
		if pool != "" {
			modelScore, ok := score.ModelScores[pool]
			if !ok {
				// Opus and Sonnet omit a per-model bucket until first use;
				// Fable's absent bucket is never proof of capacity.
				if provider != accounts.ProviderClaude ||
					(pool != selectacct.ModelKey("claude-opus") && pool != selectacct.ModelKey("claude-sonnet")) {
					continue
				}
			} else {
				score = modelScore
				score.Provider = provider
			}
		}
		if selectacct.NewScheduler([]selectacct.Score{score}).UsableForStickySession(provider, status.ID) {
			return true
		}
	}
	return false
}

// The normal status and per-account windows caches can both predate a quota
// failure while still being marked fresh. Only a successful measurement made
// after the alarm's observed failure can advance that alarm.
func (r *AccountRef) usageEvidenceAfter(status AccountUsageStatus, failureAt time.Time) bool {
	if r == nil || failureAt.IsZero() {
		return false
	}
	r.usageStatusMu.Lock()
	statusAt := r.usageStatusAt
	r.usageStatusMu.Unlock()
	if !statusAt.After(failureAt) {
		return false
	}
	return status.UsageMeasuredAt.After(failureAt)
}

func (s Server) handleRecoveryReadiness(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	agent := strings.TrimSpace(r.URL.Query().Get("agent"))
	if agent != "codex" && agent != "claude" {
		http.Error(w, "agent must be codex or claude", http.StatusBadRequest)
		return
	}
	if s.AccountRef == nil {
		writeJSON(w, map[string]bool{"ready": false})
		return
	}
	failureAt, err := time.Parse(time.RFC3339Nano, r.URL.Query().Get("observed_at"))
	if err != nil || failureAt.IsZero() {
		http.Error(w, "observed_at must be an RFC3339 timestamp", http.StatusBadRequest)
		return
	}
	scoreRevision := uint64(0)
	if s.SchedulerRef != nil {
		scoreRevision = s.SchedulerRef.ScoreRevision()
	}
	statuses := s.AccountRef.UsageStatuses(r.Context())
	for i := range statuses {
		if !s.AccountRef.usageEvidenceAfter(statuses[i], failureAt) {
			statuses[i].UsageFresh = false
		}
	}
	if s.SchedulerRef != nil {
		loaded, generation, credentialRevision := s.AccountRef.CredentialSnapshot()
		s.SchedulerRef.SyncAccountCredentials(generation, credentialRevision, SchedulerAccounts(loaded))
		s.updateSchedulerFromUsageStatusesAtScoreRevision(r.Context(), statuses, scoreRevision)
	}
	ready := recoveryReadyFromFreshUsage(agent, r.URL.Query().Get("pool"), statuses)
	writeJSON(w, map[string]bool{"ready": ready})
}
