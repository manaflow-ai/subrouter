package proxy

import (
	"strings"

	"github.com/manaflow-ai/subrouter/internal/accounts"
)

// invalidateReplacedAccountUsage evicts evidence owned by a replaced or
// removed login. Independent accounts keep their cached windows and active
// status fetches. A normal in-place token refresh uses replace instead.
func (r *AccountRef) invalidateReplacedAccountUsage(previous, current []accounts.Account) {
	if r == nil || len(previous) == 0 {
		return
	}
	type accountKey struct {
		provider accounts.Provider
		id       string
	}
	next := make(map[accountKey]string, len(current))
	for _, account := range current {
		next[accountKey{provider: account.Provider, id: account.ID}] = account.CredentialIdentity()
	}
	var replaced []accounts.Account
	for _, account := range previous {
		identity, exists := next[accountKey{provider: account.Provider, id: account.ID}]
		if !exists || identity != account.CredentialIdentity() {
			replaced = append(replaced, account)
		}
	}
	if len(replaced) == 0 {
		return
	}

	r.usageWindowsMu.Lock()
	for _, account := range replaced {
		key := account.ID + "\x00" + string(account.Provider)
		oldKey := usageWindowsCredentialKey(account)
		// The shared windows slot contains only one credential version.
		// Protect a newly fetched replacement from a late reload cleanup.
		if entry, ok := r.usageWindows[key]; ok &&
			(entry.credentialKey == "" || entry.credentialKey == oldKey) {
			delete(r.usageWindows, key)
		}
		// Each inflight fetch may complete for its own waiting caller. Clear
		// ownership only for the superseded grant, so it cannot republish.
		if flightKey := r.usageWindowsLatest[key]; strings.HasPrefix(flightKey, key+"\x00"+oldKey+"\x00") {
			delete(r.usageWindowsLatest, key)
		}
		// Cached telemetry failures and supplementary quota are already
		// credential-scoped. Keep newer grants' evidence intact.
		delete(r.usageWindowsFailures, usageWindowsFailureKey(key, account.CredentialIdentity()))
		delete(r.claudeSupplemental, claudeSupplementalCacheKey(account))
	}
	r.usageWindowsMu.Unlock()

	// A failed status refresh must not resurrect the previous login's
	// last-known-good quota. The caller separately invalidates the status
	// view and its persisted snapshot after the reload completes.
	r.usageStatusMu.Lock()
	for _, account := range replaced {
		delete(r.lastGoodUsage, account.ID+"\x00"+string(account.Provider))
	}
	r.usageStatusMu.Unlock()
}
