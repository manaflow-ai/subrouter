package proxy

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
)

func TestCredentialReplacementClearsOnlyMatchingUsageCache(t *testing.T) {
	old := accounts.Account{
		ID: "claude-a", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth,
		Token: "old-token", CredentialVersion: "old-grant",
	}
	sibling := accounts.Account{
		ID: "claude-b", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth,
		Token: "sibling-token", CredentialVersion: "sibling-grant",
	}
	changed := old
	changed.Token = "new-token"
	changed.CredentialVersion = "new-grant"
	keyA := old.ID + "\x00" + string(old.Provider)
	keyB := sibling.ID + "\x00" + string(sibling.Provider)
	failureA := usageWindowsFailureKey(keyA, old.CredentialIdentity())
	failureB := usageWindowsFailureKey(keyB, sibling.CredentialIdentity())
	ref := &AccountRef{
		usageWindows: map[string]usageWindowsEntry{
			keyA: {at: time.Now(), credentialKey: usageWindowsCredentialKey(old), windows: []accounts.UsageWindow{{Name: "7d", UsedPercent: 100}}},
			keyB: {at: time.Now(), credentialKey: usageWindowsCredentialKey(sibling), windows: []accounts.UsageWindow{{Name: "7d", UsedPercent: 30}}},
		},
		usageWindowsFailures: map[string]usageWindowsFailure{
			failureA: {err: errors.New("429"), retryAt: time.Now().Add(time.Hour)},
			failureB: {err: errors.New("429"), retryAt: time.Now().Add(time.Hour)},
		},
		claudeSupplemental: map[string]claudeSupplementalUsage{
			claudeSupplementalCacheKey(old):     {at: time.Now()},
			claudeSupplementalCacheKey(sibling): {at: time.Now()},
		},
		usageWindowsLatest: map[string]string{
			keyA: keyA + "\x00" + usageWindowsCredentialKey(old) + "\x000",
			keyB: keyB + "\x00" + usageWindowsCredentialKey(sibling) + "\x000",
		},
		lastGoodUsage: map[string]usageStatusSnapshot{
			keyA: {at: time.Now()},
			keyB: {at: time.Now()},
		},
	}
	ref.invalidateReplacedAccountUsage([]accounts.Account{old, sibling}, []accounts.Account{changed, sibling})
	if _, ok := ref.usageWindows[keyA]; ok {
		t.Fatal("re-login inherited the previous account's fresh 7d utilization")
	}
	if _, ok := ref.usageWindows[keyB]; !ok {
		t.Fatal("unrelated account usage was needlessly invalidated")
	}
	if _, ok := ref.usageWindowsFailures[failureA]; ok {
		t.Fatal("old credential's throttle cache survived re-login")
	}
	if _, ok := ref.usageWindowsFailures[failureB]; !ok {
		t.Fatal("sibling's throttle deadline was lost")
	}
	if _, ok := ref.claudeSupplemental[claudeSupplementalCacheKey(old)]; ok {
		t.Fatal("old credential's Fable quota probe survived re-login")
	}
	if _, ok := ref.claudeSupplemental[claudeSupplementalCacheKey(sibling)]; !ok {
		t.Fatal("sibling's model quota evidence was discarded")
	}
	if ref.usageWindowsEpoch != 0 {
		t.Fatal("targeted login replacement invalidated all accounts' usage flights")
	}
	if _, exists := ref.usageWindowsLatest[keyA]; exists {
		t.Fatal("old credential's inflight fetch retained cache ownership")
	}
	if _, exists := ref.usageWindowsLatest[keyB]; !exists {
		t.Fatal("unrelated account's inflight cache ownership was lost")
	}
	if _, exists := ref.lastGoodUsage[keyA]; exists {
		t.Fatal("old login's last-good usage could be restored")
	}
	if _, exists := ref.lastGoodUsage[keyB]; !exists {
		t.Fatal("unrelated login's last-good usage was lost")
	}
}

func TestUnchangedCredentialAndOrdinaryRefreshKeepUsageCache(t *testing.T) {
	original := accounts.Account{
		ID: "claude-a", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth,
		Token: "access-v1", CredentialVersion: "chain-v1",
	}
	key := original.ID + "\x00" + string(original.Provider)
	oldCredentialKey := usageWindowsCredentialKey(original)
	retryAt := time.Now().Add(time.Hour)
	oldFailureKey := usageWindowsFailureKey(key, original.CredentialIdentity())
	ref := &AccountRef{
		accounts: []accounts.Account{original},
		usageWindows: map[string]usageWindowsEntry{
			key: {at: time.Now(), windows: []accounts.UsageWindow{{Name: "5h", UsedPercent: 30}}},
		},
		usageWindowsFailures: map[string]usageWindowsFailure{
			oldFailureKey: {err: errors.New("429 Too Many Requests"), retryAt: retryAt},
		},
		usageWindowsLatest: map[string]string{
			key: key + "\x00" + oldCredentialKey + "\x000",
		},
	}
	ref.invalidateReplacedAccountUsage([]accounts.Account{original}, []accounts.Account{original})
	if ref.usageWindowsEpoch != 0 {
		t.Fatal("unchanged reload invalidated quota cache")
	}
	refreshed := original
	refreshed.Token = "access-v2"
	refreshed.CredentialVersion = "chain-v2"
	ref.replace(refreshed)
	if _, ok := ref.usageWindows[key]; !ok || ref.usageWindowsEpoch != 0 {
		t.Fatal("ordinary token refresh needlessly invalidated successful quota readings")
	}
	newFailureKey := usageWindowsFailureKey(key, refreshed.CredentialIdentity())
	if _, ok := ref.usageWindowsFailures[oldFailureKey]; ok {
		t.Fatal("ordinary token refresh retained cooldown under the old credential key")
	}
	if failure, ok := ref.usageWindowsFailures[newFailureKey]; !ok || !failure.retryAt.Equal(retryAt) {
		t.Fatalf("ordinary token refresh lost active cooldown: %+v", ref.usageWindowsFailures)
	}
	if _, ok := ref.usageWindowsLatest[key]; ok {
		t.Fatal("ordinary token refresh left an old-credential fetch owning the cache")
	}
}

func TestReloadSnapshotDropsPreviousLoginQuota(t *testing.T) {
	ref, previous := multiProfileAccountRef(t, nil, 1)
	ref.accounts = append([]accounts.Account(nil), previous...)
	old := previous[0]
	cacheKey := old.ID + "\x00" + string(old.Provider)
	ref.usageWindows = map[string]usageWindowsEntry{
		cacheKey: {at: time.Now(), credentialKey: usageWindowsCredentialKey(old), windows: []accounts.UsageWindow{{Name: "7d", UsedPercent: 5}}},
	}
	path := filepath.Join(ref.claudeStore.Dir, "claude", "_p0", ".credentials.json")
	nextCredential := fmt.Sprintf(`{"claudeAiOauth":{"accessToken":"replacement-login","refreshToken":"replacement-refresh","expiresAt":%d,"subscriptionType":"max"}}`, time.Now().Add(time.Hour).UnixMilli())
	if err := os.WriteFile(path, []byte(nextCredential), 0o600); err != nil {
		t.Fatal(err)
	}
	reloaded, _, err := ref.ReloadSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if len(reloaded) != 1 || reloaded[0].Token != "replacement-login" {
		t.Fatalf("reloaded credential did not change as expected: %+v", reloaded)
	}
	if _, ok := ref.usageWindows[cacheKey]; ok {
		t.Fatal("ReloadSnapshot left old profile's successful quota in cache")
	}
	if ref.usageWindowsEpoch != 0 {
		t.Fatal("credential reload unnecessarily invalidated unrelated usage fetches")
	}
	if !ref.usageStatusAt.IsZero() {
		t.Fatal("credential reload kept the previous status view")
	}
}

func TestRepairedGrantReplaceDoesNotCarryThrottle(t *testing.T) {
	old := accounts.Account{
		ID: "claude-a", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth,
		Token: "same-access", CredentialVersion: "old-grant",
	}
	key := old.ID + "\x00" + string(old.Provider)
	oldFailureKey := usageWindowsFailureKey(key, old.CredentialIdentity())
	ref := &AccountRef{
		accounts: []accounts.Account{old},
		usageWindowsFailures: map[string]usageWindowsFailure{
			oldFailureKey: {err: errors.New("429 Too Many Requests"), retryAt: time.Now().Add(time.Hour)},
		},
	}
	repaired := old
	repaired.CredentialVersion = "repaired-grant"
	ref.replace(repaired)
	newFailureKey := usageWindowsFailureKey(key, repaired.CredentialIdentity())
	if _, ok := ref.usageWindowsFailures[newFailureKey]; ok {
		t.Fatal("repaired refresh grant inherited the old credential throttle")
	}
}
