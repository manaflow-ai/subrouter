package proxy

import (
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/selectacct"
)

// Quota can recover before its old reset time after a subscription upgrade.
// Keep admission aligned with that evidence, not just the scheduler's scores.
func (s Server) reconcileClaudeAdmissionQuota() {
	if s.AccountRef == nil || s.admission == nil {
		return
	}
	loaded, _, _ := s.AccountRef.CredentialSnapshot()
	for _, account := range loaded {
		if account.Provider != accounts.ProviderClaude || account.AuthMode != accounts.AuthModeOAuth {
			continue
		}
		s.AccountRef.usageWindowsMu.Lock()
		entry, found := s.AccountRef.usageWindows[account.ID+"\x00"+string(account.Provider)]
		windows := append([]accounts.UsageWindow(nil), entry.windows...)
		supplemental := s.AccountRef.claudeSupplemental[claudeSupplementalCacheKey(account)]
		supplemental.windows = append([]accounts.UsageWindow(nil), supplemental.windows...)
		s.AccountRef.usageWindowsMu.Unlock()
		if !found || entry.credentialKey != usageWindowsCredentialKey(account) || entry.at.IsZero() || time.Since(entry.at) >= usageWindowsTTL {
			continue
		}
		observedAt := entry.at
		if !entry.orderAt.IsZero() && entry.orderAt.Before(observedAt) {
			observedAt = entry.orderAt
		}
		modelObservedAt := supplemental.at
		var modelWindows []accounts.UsageWindow
		if !supplemental.requestStarted.IsZero() && supplemental.requestStarted.Before(modelObservedAt) {
			modelObservedAt = supplemental.requestStarted
		}
		if time.Since(supplemental.at) >= usageWindowsTTL {
			modelObservedAt = time.Time{}
		} else {
			modelWindows = supplemental.windows
			windows = mergeUsageWindows(windows, modelWindows)
		}
		s.admission.recoverClaudeQuota(account.ID, windows, modelWindows, observedAt, modelObservedAt)
	}
}

func (a *admissionControl) recoverClaudeQuota(accountID string, windows, modelWindows []accounts.UsageWindow, observedAt, modelObservedAt time.Time) {
	// A healthy model bucket alone cannot prove recovery of account-wide quota.
	sessionKnown, weeklyKnown := false, false
	for _, window := range windows {
		if window.Feature != "" {
			continue
		}
		sessionKnown = sessionKnown || window.Name == "5h"
		weeklyKnown = weeklyKnown || window.Name == "7d"
	}
	if !sessionKnown || !weeklyKnown || observedAt.IsZero() {
		return
	}
	scheduler := selectacct.NewScheduler([]selectacct.Score{scoreFromUsageWindows(accounts.ProviderClaude, accountID, windows)})
	a.mu.Lock()
	defer a.mu.Unlock()
	now := time.Now()
	for key, bucket := range a.buckets {
		if key.provider != accounts.ProviderClaude || key.account != accountID || !bucket.pausedTil.After(now) || !observedAt.After(bucket.pausedAt) {
			continue
		}
		if !bucket.accountQuotaUntil.After(now) && !bucket.modelQuotaUntil.After(now) {
			continue
		}
		modelUntil := bucket.modelQuotaUntil
		if modelUntil.After(now) {
			modelKnown := false
			for _, window := range modelWindows {
				if window.Feature != "" && selectacct.ModelKey(window.Feature) == selectacct.ModelKey(key.model) {
					modelKnown = true
				}
			}
			if modelKnown && modelObservedAt.After(bucket.pausedAt) {
				modelUntil = time.Time{}
			}
		} else {
			modelUntil = time.Time{}
		}
		if scheduler.ForModel(key.model).Exhausted(accounts.ProviderClaude, accountID) {
			continue
		}
		bucket.accountQuotaUntil = time.Time{}
		bucket.modelQuotaUntil = modelUntil
		bucket.pausedTil = bucket.throttleUntil
		bucket.recoverSlowly = !bucket.throttleUntil.IsZero()
		if modelUntil.After(bucket.pausedTil) {
			bucket.pausedTil = modelUntil
			bucket.recoverSlowly = false
		}
		close(bucket.changed)
		bucket.changed = make(chan struct{})
		a.wakeLocked(key, bucket, now)
	}
}
