package proxy

import (
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/selectacct"
)

// restoredUsageMaxAge bounds how old a saved observation may be and still
// seed routing. The previous worker rewrites the snapshot on every usage
// sweep, so a normal swap restores minutes-old windows; a long outage
// restores nothing and keeps the optimistic default.
const restoredUsageMaxAge = 2 * time.Hour

// SeedScoresFromRestoredUsage puts the Claude usage windows the previous
// worker saved into the scheduler before the first live sweep, and returns
// how many accounts it seeded. Codex is left to its own startup scores, which
// read the shared score store and gate readiness.
//
// Only Codex scores were fetched before a new worker reported ready. Every
// Claude account read as fully healthy until the first live sweep finished,
// so on 2026-10-14 a session walked fourteen weekly-exhausted accounts in
// four seconds, one 429 each, 51 seconds after a canary started. The saved
// windows are already restored for status rendering; this gives routing the
// same view. Seeded scores are not Fresh and do not mark the scheduler
// refreshed, so the first live sweep still runs and replaces them.
func (s Server) SeedScoresFromRestoredUsage(now time.Time) int {
	if s.AccountRef == nil || s.SchedulerRef == nil {
		return 0
	}
	all, generation := s.AccountRef.Snapshot()
	scores := make([]selectacct.Score, 0, len(all))
	for _, account := range oauthAccounts(all) {
		if account.Provider != accounts.ProviderClaude {
			continue
		}
		windows, ok := s.AccountRef.restoredUsageWindows(account, now)
		if !ok {
			continue
		}
		score := scoreFromUsageWindows(account.Provider, account.ID, windows)
		score.Provider = account.Provider
		score.Fresh = false
		scores = append(scores, score)
	}
	if len(scores) == 0 {
		return 0
	}
	if _, published := s.SchedulerRef.MergeScoresForAccountGeneration(scores, generation); !published {
		if s.Logger != nil {
			s.Logger.Warn("saved usage scores were not seeded; the account generation moved",
				"accounts", len(scores), "generation", generation)
		}
		return 0
	}
	return len(scores)
}

// restoredUsageWindows returns the saved windows for account as of now: a
// window whose reset has passed reads as unused (its quota is back; keeping
// the window keeps the account in that window's model pool), and the rest
// count down from now rather than from when they were observed.
func (r *AccountRef) restoredUsageWindows(account accounts.Account, now time.Time) ([]accounts.UsageWindow, bool) {
	key := account.ID + "\x00" + string(account.Provider)
	r.usageWindowsMu.Lock()
	entry, ok := r.usageWindows[key]
	r.usageWindowsMu.Unlock()
	if !ok || len(entry.windows) == 0 || entry.at.IsZero() || now.Sub(entry.at) > restoredUsageMaxAge {
		return nil, false
	}
	if entry.credentialKey != "" && entry.credentialKey != usageWindowsCredentialKey(account) {
		return nil, false
	}
	windows := make([]accounts.UsageWindow, 0, len(entry.windows))
	for _, window := range entry.windows {
		resetAt := window.ResetAt
		if resetAt.IsZero() && window.ResetAfterSeconds > 0 {
			resetAt = entry.at.Add(time.Duration(window.ResetAfterSeconds) * time.Second)
		}
		if !resetAt.IsZero() {
			if !resetAt.After(now) {
				window.UsedPercent = 0
				window.ResetAt = time.Time{}
				window.ResetAfterSeconds = 0
				windows = append(windows, window)
				continue
			}
			window.ResetAt = resetAt
			window.ResetAfterSeconds = int64(resetAt.Sub(now) / time.Second)
		}
		windows = append(windows, window)
	}
	return windows, len(windows) > 0
}
