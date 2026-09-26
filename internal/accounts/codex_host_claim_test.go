package accounts

import (
	"context"
	"errors"
	"testing"
)

func storedHostClaim(t *testing.T, store CodexStore, email string) *CodexHostClaim {
	t.Helper()
	account, ok, err := store.FindStored(email)
	if err != nil || !ok {
		t.Fatalf("account %s missing: ok=%v err=%v", email, ok, err)
	}
	return account.HostClaim
}

// Without SUBROUTER_HOST_ID nothing is stamped and refresh behaves as before.
func TestCodexHostClaimDisabledByDefault(t *testing.T) {
	t.Setenv(HostIDEnv, "")
	provider := newRotatingOAuthServer("refresh-gen-0")
	provider.start(t)
	store := CodexStore{Dir: t.TempDir()}
	seedChainAccount(t, store, "chain@example.com", "refresh-gen-0")

	account, _, _ := store.FindStored("chain@example.com")
	if _, _, err := store.RefreshStored(context.Background(), nil, account); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if claim := storedHostClaim(t, store, "chain@example.com"); claim != nil {
		t.Fatalf("claim stamped while host claims are disabled: %+v", claim)
	}
	if n, err := store.ClaimUnclaimedOAuth(); err != nil || n != 0 {
		t.Fatalf("ClaimUnclaimedOAuth = %d, %v; want 0 while disabled", n, err)
	}
}

// The owning host keeps its claim across saves and refreshes.
func TestCodexHostClaimStampedAndKeptByOwner(t *testing.T) {
	t.Setenv(HostIDEnv, "host-a")
	provider := newRotatingOAuthServer("refresh-gen-0")
	provider.start(t)
	store := CodexStore{Dir: t.TempDir()}
	seedChainAccount(t, store, "chain@example.com", "refresh-gen-0")
	if claim := storedHostClaim(t, store, "chain@example.com"); claim == nil || claim.Host != "host-a" {
		t.Fatalf("save did not stamp host-a: %+v", claim)
	}

	account, _, _ := store.FindStored("chain@example.com")
	if _, _, err := store.RefreshStored(context.Background(), nil, account); err != nil {
		t.Fatalf("owner refresh: %v", err)
	}
	if claim := storedHostClaim(t, store, "chain@example.com"); claim == nil || claim.Host != "host-a" {
		t.Fatalf("refresh changed the claim: %+v", claim)
	}
}

// A state copy of a claimed account must never reach the token endpoint on
// another host, whether that host has its own identity or none (#129).
func TestCodexRefreshRefusesForeignHostClaim(t *testing.T) {
	for _, local := range []string{"host-b", ""} {
		t.Run("local="+local, func(t *testing.T) {
			provider := newRotatingOAuthServer("refresh-gen-0")
			provider.start(t)
			store := CodexStore{Dir: t.TempDir()}
			t.Setenv(HostIDEnv, "host-a")
			seedChainAccount(t, store, "copied@example.com", "refresh-gen-0")

			t.Setenv(HostIDEnv, local)
			account, _, _ := store.FindStored("copied@example.com")
			_, refreshed, err := store.RefreshStored(context.Background(), nil, account)
			var foreign *CodexForeignHostClaimError
			if !errors.As(err, &foreign) || refreshed {
				t.Fatalf("refresh = %v, %v; want CodexForeignHostClaimError", refreshed, err)
			}
			if foreign.ClaimHost != "host-a" || foreign.LocalHost != local {
				t.Fatalf("error names %+v", foreign)
			}
			if got := provider.exchanges.Load() + provider.reuseAttempts.Load(); got != 0 {
				t.Fatalf("token endpoint was contacted %d times", got)
			}
			if got := storedRefreshToken(t, store, "copied@example.com"); got != "refresh-gen-0" {
				t.Fatalf("stored refresh token changed to %s", got)
			}
			if claim := storedHostClaim(t, store, "copied@example.com"); claim == nil || claim.Host != "host-a" {
				t.Fatalf("refusal changed the claim: %+v", claim)
			}
			if _, err := store.AttestStoredLegacyOAuth(context.Background(), nil, "copied@example.com"); !errors.As(err, &foreign) {
				t.Fatalf("attest-legacy = %v; want CodexForeignHostClaimError", err)
			}
		})
	}
}

// Enabling host claims stamps existing OAuth accounts once, and leaves API keys
// and already-claimed accounts alone.
func TestClaimUnclaimedOAuthStampsLegacyAccounts(t *testing.T) {
	t.Setenv(HostIDEnv, "")
	store := CodexStore{Dir: t.TempDir()}
	seedChainAccount(t, store, "legacy@example.com", "refresh-gen-0")
	if _, _, err := store.AddAPIKey("key", "sk-test"); err != nil {
		t.Fatal(err)
	}

	t.Setenv(HostIDEnv, "host-a")
	if n, err := store.ClaimUnclaimedOAuth(); err != nil || n != 1 {
		t.Fatalf("ClaimUnclaimedOAuth = %d, %v; want 1", n, err)
	}
	if claim := storedHostClaim(t, store, "legacy@example.com"); claim == nil || claim.Host != "host-a" {
		t.Fatalf("legacy account claim = %+v", claim)
	}
	if claim := storedHostClaim(t, store, "apikey:key"); claim != nil {
		t.Fatalf("API key was claimed: %+v", claim)
	}
	t.Setenv(HostIDEnv, "host-b")
	if n, err := store.ClaimUnclaimedOAuth(); err != nil || n != 0 {
		t.Fatalf("second pass = %d, %v; must not move an existing claim", n, err)
	}
	if claim := storedHostClaim(t, store, "legacy@example.com"); claim.Host != "host-a" {
		t.Fatalf("claim moved to %s", claim.Host)
	}
}

// A fresh login on this host starts a new chain, so it replaces a foreign claim.
// sr add, migration enrollment and server import all save a record this way.
func TestFreshChainReplacesForeignHostClaim(t *testing.T) {
	for _, local := range []string{"host-b", ""} {
		t.Run("local="+local, func(t *testing.T) {
			store := CodexStore{Dir: t.TempDir()}
			t.Setenv(HostIDEnv, "host-a")
			if err := store.SaveStored(StoredCodexAccount{
				Email:    "moved@example.com",
				Provider: ProviderCodex,
				Auth:     ownerAuth("moved@example.com", "user-1", "team", "copied"),
			}); err != nil {
				t.Fatal(err)
			}

			t.Setenv(HostIDEnv, local)
			fresh := ownerAuth("moved@example.com", "user-1", "team", "relogin")
			if err := store.ReplaceStoredOAuthWithIsolated(context.Background(), "moved@example.com", fresh); err != nil {
				t.Fatalf("relogin: %v", err)
			}
			claim := storedHostClaim(t, store, "moved@example.com")
			if local == "" && claim != nil || local != "" && (claim == nil || claim.Host != local) {
				t.Fatalf("relogin claim = %+v; want host %q", claim, local)
			}
		})
	}
}

// Re-importing the same chain from ~/.codex/auth.json is not a new credential,
// so it must not move or drop the claim; a cloned home directory would
// otherwise unlock the copy (#129).
func TestActiveImportOfSameChainKeepsHostClaim(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CODEX_HOME", t.TempDir())
	store := CodexStore{Dir: t.TempDir()}
	auth := ownerAuth("cloned@example.com", "user-1", "team", "shared")
	t.Setenv(HostIDEnv, "host-a")
	if err := store.SaveStored(StoredCodexAccount{Email: "cloned@example.com", Provider: ProviderCodex, Auth: auth}); err != nil {
		t.Fatal(err)
	}
	if err := WriteActiveCodexAuth(auth); err != nil {
		t.Fatal(err)
	}

	for _, local := range []string{"host-b", ""} {
		t.Setenv(HostIDEnv, local)
		if _, _, err := store.ImportActive(); err != nil {
			t.Fatalf("import active (local=%q): %v", local, err)
		}
		if err := store.SyncActiveToStore(); err != nil {
			t.Fatalf("sync active (local=%q): %v", local, err)
		}
		if claim := storedHostClaim(t, store, "cloned@example.com"); claim == nil || claim.Host != "host-a" {
			t.Fatalf("same-chain import (local=%q) changed the claim to %+v", local, claim)
		}
	}
}

// Switching exports the chain to the Codex CLI and the tools synced from it,
// which refresh it themselves, so a foreign chain must not be exported.
func TestSwitchRefusesForeignHostClaim(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CODEX_HOME", t.TempDir())
	store := CodexStore{Dir: t.TempDir()}
	t.Setenv(HostIDEnv, "host-a")
	if err := store.SaveStored(StoredCodexAccount{
		Email:    "copied@example.com",
		Provider: ProviderCodex,
		Auth:     ownerAuth("copied@example.com", "user-1", "team", "copied"),
	}); err != nil {
		t.Fatal(err)
	}

	t.Setenv(HostIDEnv, "host-b")
	var foreign *CodexForeignHostClaimError
	if _, err := store.SwitchActiveStored("copied@example.com"); !errors.As(err, &foreign) {
		t.Fatalf("switch = %v; want CodexForeignHostClaimError", err)
	}
	if _, ok, err := ReadActiveCodexAuth(); err != nil || ok {
		t.Fatalf("active auth written: ok=%v err=%v", ok, err)
	}

	t.Setenv(HostIDEnv, "host-a")
	if _, err := store.SwitchActiveStored("copied@example.com"); err != nil {
		t.Fatalf("owner switch: %v", err)
	}
}
