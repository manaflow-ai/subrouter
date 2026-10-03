package proxy

import (
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
)

func TestRecoveryReadinessRequiresFreshMatchingSubscriptionQuota(t *testing.T) {
	base := AccountUsageStatus{
		AccountStatus: AccountStatus{ID: "codex-a", Provider: accounts.ProviderCodex, AuthMode: accounts.AuthModeOAuth, AuthValid: true},
		UsageFresh:    true,
		Windows:       []accounts.UsageWindow{{Name: "5h", UsedPercent: 30, LimitWindowSeconds: 18000}},
	}
	if !recoveryReadyFromFreshUsage("codex", "", []AccountUsageStatus{base}) {
		t.Fatal("fresh Codex subscription quota should advance its alarm")
	}
	if recoveryReadyFromFreshUsage("claude", "", []AccountUsageStatus{base}) {
		t.Fatal("Codex quota must not advance Claude alarms")
	}
	stale := base
	stale.UsageFresh = false
	if recoveryReadyFromFreshUsage("codex", "", []AccountUsageStatus{stale}) {
		t.Fatal("last-good usage is not fresh recovery evidence")
	}
	low := base
	low.Windows = []accounts.UsageWindow{{Name: "5h", UsedPercent: 100, LimitWindowSeconds: 18000}}
	if recoveryReadyFromFreshUsage("codex", "", []AccountUsageStatus{low}) {
		t.Fatal("exhausted quota should not advance an alarm")
	}
	invalid := base
	invalid.AuthValid = false
	if recoveryReadyFromFreshUsage("codex", "", []AccountUsageStatus{invalid}) {
		t.Fatal("invalid credential should not advance an alarm")
	}
	paidOnly := base
	paidOnly.Provider = accounts.ProviderClaude
	paidOnly.Windows = []accounts.UsageWindow{{Name: "extra", ExtraUsage: &accounts.ExtraUsageInfo{IsEnabled: true}}}
	if recoveryReadyFromFreshUsage("claude", "", []AccountUsageStatus{paidOnly}) {
		t.Fatal("paid extra usage alone cannot prove subscription recovery")
	}
}

func TestRecoveryReadinessRequiresMeasurementAfterFailure(t *testing.T) {
	now := time.Now().UTC()
	ref := &AccountRef{usageStatusAt: now}
	status := AccountUsageStatus{AccountStatus: AccountStatus{ID: "claude-a", Provider: accounts.ProviderClaude}, UsageMeasuredAt: now.Add(-time.Minute)}
	if ref.usageEvidenceAfter(status, now.Add(-time.Second)) {
		t.Fatal("old per-account measurement accepted after failure")
	}
	status.UsageMeasuredAt = now.Add(time.Second)
	if !ref.usageEvidenceAfter(status, now.Add(-time.Second)) {
		t.Fatal("new status and account measurement should be accepted")
	}
	codexStatus := AccountUsageStatus{AccountStatus: AccountStatus{ID: "codex-a", Provider: accounts.ProviderCodex}, UsageMeasuredAt: now}
	if !ref.usageEvidenceAfter(codexStatus, now.Add(-time.Second)) {
		t.Fatal("Codex direct status measurement should be accepted without window cache")
	}
	codexStatus.UsageMeasuredAt = now.Add(-time.Minute)
	if ref.usageEvidenceAfter(codexStatus, now.Add(-time.Second)) {
		t.Fatal("old Codex direct measurement accepted after failure")
	}
	ref.usageStatusAt = now.Add(-time.Minute)
	if ref.usageEvidenceAfter(status, now.Add(-time.Second)) {
		t.Fatal("old status sweep accepted after failure")
	}
}

func TestRecoveryReadinessRespectsClaudeModelPool(t *testing.T) {
	status := AccountUsageStatus{
		AccountStatus: AccountStatus{ID: "claude-a", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, AuthValid: true},
		UsageFresh:    true,
		Windows: []accounts.UsageWindow{
			{Name: "5h", UsedPercent: 20, LimitWindowSeconds: 18000},
			{Name: "7d", UsedPercent: 20, LimitWindowSeconds: 604800},
			{Name: "7d_oi", UsedPercent: 100, LimitWindowSeconds: 604800, Feature: "claude-fable"},
		},
	}
	if recoveryReadyFromFreshUsage("claude", "claude-fable", []AccountUsageStatus{status}) {
		t.Fatal("fresh base quota must not wake a Fable-exhausted pool")
	}
	if !recoveryReadyFromFreshUsage("claude", "claude-opus", []AccountUsageStatus{status}) {
		t.Fatal("fresh shared Opus quota should wake its own pool")
	}
}
