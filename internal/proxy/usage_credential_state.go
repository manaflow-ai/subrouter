package proxy

import "github.com/manaflow-ai/subrouter/internal/accounts"

// migrateUsageCredentialState carries quota evidence and an active telemetry
// cooldown across an in-place token refresh. A full account reload calls
// invalidateReplacedAccountUsage instead, so a replacement login does not
// inherit the previous credential's state.
func (r *AccountRef) migrateUsageCredentialState(previous, current accounts.Account) {
	if r == nil || previous.ID != current.ID || previous.Provider != current.Provider ||
		previous.AuthMode != accounts.AuthModeOAuth || current.AuthMode != accounts.AuthModeOAuth {
		return
	}
	oldIdentity := previous.CredentialIdentity()
	newIdentity := current.CredentialIdentity()
	if oldIdentity == newIdentity || oldIdentity == "" || newIdentity == "" {
		return
	}
	cacheKey := current.ID + "\x00" + string(current.Provider)
	oldCredentialKey := usageWindowsCredentialKey(previous)
	newCredentialKey := usageWindowsCredentialKey(current)
	oldFailureKey := usageWindowsFailureKey(cacheKey, oldIdentity)
	newFailureKey := usageWindowsFailureKey(cacheKey, newIdentity)

	r.usageWindowsMu.Lock()
	if flightKey := r.usageWindowsLatest[cacheKey]; len(flightKey) > 0 {
		oldPrefix := cacheKey + "\x00" + oldCredentialKey + "\x00"
		if len(flightKey) >= len(oldPrefix) && flightKey[:len(oldPrefix)] == oldPrefix {
			// A usage request started under the old token must not publish
			// after the in-place refresh installs the new token.
			delete(r.usageWindowsLatest, cacheKey)
		}
	}
	// A changed refresh grant with the same access token is a repaired or
	// replaced credential, not a routine access-token renewal. Fence its old
	// in-flight request, but force a fresh observation for the new grant.
	if previous.Token == current.Token {
		r.usageWindowsMu.Unlock()
		return
	}
	if entry, ok := r.usageWindows[cacheKey]; ok &&
		(entry.credentialKey == "" || entry.credentialKey == oldCredentialKey) {
		entry.credentialKey = newCredentialKey
		r.usageWindows[cacheKey] = entry
	}
	if failure, ok := r.usageWindowsFailures[oldFailureKey]; ok {
		if currentFailure, exists := r.usageWindowsFailures[newFailureKey]; !exists ||
			failure.retryAt.After(currentFailure.retryAt) {
			r.usageWindowsFailures[newFailureKey] = failure
		}
		delete(r.usageWindowsFailures, oldFailureKey)
	}
	r.usageWindowsMu.Unlock()
}
