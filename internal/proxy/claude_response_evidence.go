package proxy

import (
	"net/http"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	agentclaude "github.com/manaflow-ai/subrouter/internal/agents/claude"
)

// observeClaudeQuotaHeaders learns model-specific quota from real upstream
// responses, eliminating a later synthetic model probe when possible. It also
// publishes account-wide reset headers to the scheduler so exhausted accounts
// can sleep until the provider's next reset instead of being polled on TTL.
func (r *AccountRef) observeClaudeQuotaHeaders(routed accounts.Account, header http.Header, requestStarted time.Time) ([]accounts.UsageWindow, bool, uint64, time.Time) {
	if r == nil || routed.Provider != accounts.ProviderClaude || routed.ID == "" ||
		routed.CredentialIdentity() == "" {
		return nil, false, 0, time.Time{}
	}
	now := time.Now()
	observed := agentclaude.ObservedUsageWindows(header, now)
	if len(observed) == 0 {
		return nil, false, 0, time.Time{}
	}
	// A failed-over reply belongs to the actual serving credential, not
	// necessarily the initial choice. Ignore results from a superseded login.
	current := r.credentialSnapshot(accounts.ProviderClaude, routed.ID)
	if current.AuthMode != accounts.AuthModeOAuth || current.Token == "" ||
		current.CredentialIdentity() != routed.CredentialIdentity() {
		return nil, false, 0, time.Time{}
	}
	generation := r.Generation()
	if requestStarted.IsZero() {
		requestStarted = now
	}
	windows := make([]accounts.UsageWindow, 0, len(observed))
	for _, window := range observed {
		if window.Feature != "" {
			windows = append(windows, window)
		}
	}
	key := claudeSupplementalCacheKey(current)
	r.usageWindowsMu.Lock()
	defer r.usageWindowsMu.Unlock()
	existing, hadPrevious := r.claudeSupplemental[key]
	cacheKey := current.ID + "\x00" + string(current.Provider)
	accountEntry, hadAccountPrevious := r.usageWindows[cacheKey]
	if hadPrevious || hadAccountPrevious {
		previousStart := existing.requestStarted
		if previousStart.IsZero() {
			// Ordinary quota probes do not represent a model request. A reply
			// started before that probe cannot supersede its later evidence.
			previousStart = existing.at
		}
		if hadAccountPrevious {
			accountOrder := accountEntry.orderAt
			if accountOrder.IsZero() {
				accountOrder = accountEntry.at
			}
			if accountOrder.After(previousStart) {
				previousStart = accountOrder
			}
		}
		if !requestStarted.After(previousStart) {
			// Older concurrent requests must not overwrite newer quota evidence.
			return nil, false, 0, time.Time{}
		}
	}

	for _, window := range observed {
		if window.UsedPercent >= 100 && window.ResetAt.IsZero() {
			// An exhausted window without a reset time cannot be cached
			// safely. The next regular usage check will reassess it.
			delete(r.claudeSupplemental, key)
			return observed, true, generation, requestStarted
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
	if len(windows) > 0 {
		if r.claudeSupplemental == nil {
			r.claudeSupplemental = map[string]claudeSupplementalUsage{}
		}
		r.claudeSupplemental[key] = claudeSupplementalUsage{
			windows:        append([]accounts.UsageWindow(nil), windows...),
			at:             observedAt,
			requestStarted: requestStarted,
		}
	}

	// Account-wide windows are the scheduler's reset clock. Keep them in the
	// shared usage cache as well, so a later score sweep can reuse the same
	// response evidence without a second provider request.
	var accountWindows []accounts.UsageWindow
	for _, window := range observed {
		if window.Feature == "" {
			accountWindows = append(accountWindows, window)
		}
	}
	if len(accountWindows) > 0 {
		if r.usageWindows == nil {
			r.usageWindows = map[string]usageWindowsEntry{}
		}
		entry := accountEntry
		if entry.credentialKey == "" || entry.credentialKey == usageWindowsCredentialKey(current) {
			base := make([]accounts.UsageWindow, 0, len(entry.windows))
			for _, window := range entry.windows {
				if !window.ResetAt.IsZero() && !now.Before(window.ResetAt) {
					continue
				}
				base = append(base, window)
			}
			entry.windows = mergeUsageWindows(base, accountWindows)
			entryAt := now
			replaced := make(map[string]bool, len(accountWindows))
			for _, window := range accountWindows {
				replaced[usageWindowMergeKey(window)] = true
			}
			for _, window := range base {
				if window.Feature == "" && !replaced[usageWindowMergeKey(window)] {
					entryAt = entry.at
					break
				}
			}
			entry.at = entryAt
			entry.orderAt = requestStarted
			entry.credentialKey = usageWindowsCredentialKey(current)
			r.usageWindows[cacheKey] = entry
		}
	}
	return observed, true, generation, requestStarted
}
