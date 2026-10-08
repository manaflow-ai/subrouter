package proxy

import (
	"net/http"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	agentclaude "github.com/manaflow-ai/subrouter/internal/agents/claude"
)

// observeClaudeQuotaHeaders learns model-specific quota from real upstream
// responses, eliminating a later synthetic model probe when possible.
// It never starts a network request or mutates the active scheduler score.
// Normal usage refreshes continue to supply account-wide headroom.
func (r *AccountRef) observeClaudeQuotaHeaders(routed accounts.Account, header http.Header, requestStarted time.Time) {
	if r == nil || routed.Provider != accounts.ProviderClaude || routed.ID == "" ||
		routed.CredentialIdentity() == "" {
		return
	}
	now := time.Now()
	windows := agentclaude.ObservedFeatureUsageWindows(header, now)
	if len(windows) == 0 {
		return
	}
	// A failed-over reply belongs to the actual serving credential, not
	// necessarily the initial choice. Ignore results from a superseded login.
	current := r.credentialSnapshot(accounts.ProviderClaude, routed.ID)
	if current.AuthMode != accounts.AuthModeOAuth || current.Token == "" ||
		current.CredentialIdentity() != routed.CredentialIdentity() {
		return
	}
	if requestStarted.IsZero() {
		requestStarted = now
	}
	key := claudeSupplementalCacheKey(current)
	r.usageWindowsMu.Lock()
	defer r.usageWindowsMu.Unlock()
	existing, hadPrevious := r.claudeSupplemental[key]
	if hadPrevious {
		previousStart := existing.requestStarted
		if previousStart.IsZero() {
			// Ordinary quota probes do not represent a model request. A reply
			// started before that probe cannot supersede its later evidence.
			previousStart = existing.at
		}
		if !requestStarted.After(previousStart) {
			// Older concurrent requests must not overwrite newer quota evidence.
			return
		}
	}

	for _, window := range windows {
		if window.UsedPercent >= 100 && window.ResetAt.IsZero() {
			// An exhausted window without a reset time cannot be cached
			// safely. The next regular usage check will reassess it.
			delete(r.claudeSupplemental, key)
			return
		}
	}
	observedAt := now // the headers arrived now, regardless of request duration
	if hadPrevious && claudeSupplementalFreshEnough(existing, now) {
		// Merge partial headers, but do not renew the age of older buckets
		// unless the reply contains a replacement for every prior bucket.
		replaced := make(map[string]bool, len(windows))
		for _, window := range windows {
			replaced[usageWindowMergeKey(window)] = true
		}
		for _, window := range existing.windows {
			if !replaced[usageWindowMergeKey(window)] {
				observedAt = existing.at
				break
			}
		}
		windows = mergeUsageWindows(existing.windows, windows)
	}
	if r.claudeSupplemental == nil {
		r.claudeSupplemental = map[string]claudeSupplementalUsage{}
	}
	r.claudeSupplemental[key] = claudeSupplementalUsage{
		windows:        append([]accounts.UsageWindow(nil), windows...),
		at:             observedAt,
		requestStarted: requestStarted,
	}
}
