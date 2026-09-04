package antigravity

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/account"
)

func testIdentityToken(subject, email string) string {
	body := base64.RawURLEncoding.EncodeToString([]byte(`{"iss":"https://accounts.google.com","sub":"` + subject + `","email":"` + email + `"}`))
	return "header." + body + ".signature"
}

func TestManagedStoreKeepsMultipleAccountsOutsideCLIKeychain(t *testing.T) {
	now := time.Now().UTC()
	store := &Store{ManagedDir: t.TempDir(), readCredential: func(context.Context, time.Time) (CredentialInfo, bool, error) {
		return CredentialInfo{AccessToken: "direct", RefreshToken: "direct-refresh", ExpiresAt: now.Add(time.Hour)}, true, nil
	}}
	for _, entry := range []struct{ label, access, refresh string }{
		{"Work", "work-access", "work-refresh"},
		{"Personal", "personal-access", "personal-refresh"},
	} {
		if _, err := store.SaveManagedCredential(entry.label, CredentialInfo{AccessToken: entry.access, RefreshToken: entry.refresh, ExpiresAt: now.Add(time.Hour)}); err != nil {
			t.Fatal(err)
		}
	}
	managed, err := store.ForServing().ListAccounts(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(managed) != 2 {
		t.Fatalf("managed accounts = %+v, want two", managed)
	}
	for _, acct := range managed {
		if acct.ID == accountID || acct.Token == "direct" || acct.Source != "subrouter managed Antigravity credential" {
			t.Fatalf("serving account crossed CLI boundary: %+v", acct)
		}
	}
}

func TestIsManagedAccountIDExcludesLegacyKeychainLogin(t *testing.T) {
	if IsManagedAccountID(accountID) || IsManagedAccountID("antigravity-subscription:Work") ||
		!IsManagedAccountID("antigravity-subscription:work") {
		t.Fatal("managed Antigravity ID classification crossed the CLI/managed boundary")
	}
}

func TestManagedStoreRejectsSameGrantAcrossLabelsDespiteFabricatedClaims(t *testing.T) {
	store := &Store{ManagedDir: t.TempDir()}
	first := CredentialInfo{AccessToken: "first-access", RefreshToken: "shared-refresh", IDToken: testIdentityToken("claimed-user-a", "a@example.test"), ExpiresAt: time.Now().Add(time.Hour)}
	if _, err := store.SaveManagedCredential("first", first); err != nil {
		t.Fatal(err)
	}
	duplicate := CredentialInfo{AccessToken: "second-access", RefreshToken: "shared-refresh", IDToken: testIdentityToken("fabricated-user-b", "b@example.test"), ExpiresAt: time.Now().Add(time.Hour)}
	_, err := store.SaveManagedCredential("duplicate", duplicate)
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("duplicate identity error = %v", err)
	}
	for _, secret := range []string{"first-access", "shared-refresh", "second-access"} {
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("duplicate error leaked credential material: %v", err)
		}
	}
	replacement := duplicate
	replacement.RefreshToken = "replacement-refresh"
	if _, err := store.SaveManagedCredential("first", replacement); err != nil {
		t.Fatalf("same-label re-import should replace in place: %v", err)
	}
	stored, ok, err := store.ReadManagedCredential("first")
	if err != nil || !ok || stored.RefreshToken != "replacement-refresh" {
		t.Fatalf("re-imported credential = %+v ok=%v err=%v", stored, ok, err)
	}
}

func TestManagedStoreRetainsBoundedReplacementGrantHistory(t *testing.T) {
	store := &Store{ManagedDir: t.TempDir()}
	for _, refresh := range []string{"grant-a", "grant-b", "grant-c"} {
		credential := CredentialInfo{AccessToken: "access-" + refresh, RefreshToken: refresh, ExpiresAt: time.Now().Add(time.Hour)}
		if _, err := store.SaveManagedCredential("primary", credential); err != nil {
			t.Fatalf("replace with %s: %v", refresh, err)
		}
	}
	for _, refresh := range []string{"grant-a", "grant-b", "grant-c"} {
		credential := CredentialInfo{AccessToken: "duplicate", RefreshToken: refresh, ExpiresAt: time.Now().Add(time.Hour)}
		if _, err := store.SaveManagedCredential("duplicate-"+refresh, credential); !errors.Is(err, ErrManagedIdentityExists) {
			t.Fatalf("forgot replacement grant %s: %v", refresh, err)
		}
	}
}

func TestManagedStoreDoesNotOverclaimSameUserDedupAcrossIndependentGrants(t *testing.T) {
	store := &Store{ManagedDir: t.TempDir()}
	for _, entry := range []struct{ label, refresh string }{{"first", "grant-one"}, {"second", "grant-two"}} {
		credential := CredentialInfo{AccessToken: "access-" + entry.label, RefreshToken: entry.refresh, IDToken: testIdentityToken("same-user", "owner@example.test"), ExpiresAt: time.Now().Add(time.Hour)}
		if _, err := store.SaveManagedCredential(entry.label, credential); err != nil {
			t.Fatalf("independently authorized grant %q was rejected: %v", entry.label, err)
		}
	}
}

func TestServingStoreUsesLegacyKeychainOnlyUntilManagedImport(t *testing.T) {
	now := time.Now().UTC()
	dir := t.TempDir()
	store := &Store{
		ManagedDir: dir, managedOnly: true, localFallback: true,
		readCredential: func(context.Context, time.Time) (CredentialInfo, bool, error) {
			return CredentialInfo{AccessToken: "legacy-access", RefreshToken: "legacy-refresh", ExpiresAt: now.Add(time.Hour)}, true, nil
		},
	}
	accounts, err := store.ListAccounts(t.Context())
	if err != nil || len(accounts) != 1 || accounts[0].ID != accountID {
		t.Fatalf("legacy fallback accounts = %+v err=%v", accounts, err)
	}
	if _, err := store.SaveManagedCredential("managed", CredentialInfo{AccessToken: "managed-access", RefreshToken: "managed-refresh", ExpiresAt: now.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	accounts, err = store.ListAccounts(t.Context())
	if err != nil || len(accounts) != 1 || accounts[0].ID != "antigravity-subscription:managed" || accounts[0].Token == "legacy-access" {
		t.Fatalf("post-import serving accounts = %+v err=%v", accounts, err)
	}
	if _, ok, err := store.RemoveManagedAccount("managed"); err != nil || !ok {
		t.Fatalf("remove managed account: ok=%v err=%v", ok, err)
	}
	restarted := &Store{ManagedDir: dir, managedOnly: true, localFallback: true, readCredential: store.readCredential}
	accounts, err = restarted.ListAccounts(t.Context())
	if err != nil || len(accounts) != 0 {
		t.Fatalf("legacy fallback revived after removal/restart: %+v err=%v", accounts, err)
	}
}

func TestManagedRefreshPersistsRotatedCredentialAcrossRestart(t *testing.T) {
	now := time.Now().UTC()
	dir := t.TempDir()
	store := (&Store{ManagedDir: dir, refreshCredential: func(_ context.Context, _ *http.Client, credential CredentialInfo, _ time.Time) (CredentialInfo, error) {
		credential.AccessToken = "fresh-access"
		credential.RefreshToken = "rotated-refresh"
		credential.ExpiresAt = now.Add(time.Hour)
		return credential, nil
	}}).ForServing()
	acct, err := store.SaveManagedCredential("work", CredentialInfo{AccessToken: "stale", RefreshToken: "old-refresh", ExpiresAt: now.Add(-time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	refreshed, err := store.RefreshAccount(t.Context(), http.DefaultClient, acct)
	if err != nil {
		t.Fatal(err)
	}
	if refreshed.Token != "fresh-access" {
		t.Fatalf("refreshed token = %q", refreshed.Token)
	}
	restarted := (&Store{ManagedDir: dir}).ForServing()
	credential, ok, err := restarted.ReadManagedCredential("work")
	if err != nil || !ok {
		t.Fatalf("read after restart: ok=%v err=%v", ok, err)
	}
	if credential.AccessToken != "fresh-access" || credential.RefreshToken != "rotated-refresh" {
		t.Fatalf("durable credential was not refreshed: access=%q refresh=%q", credential.AccessToken, credential.RefreshToken)
	}
	for _, duplicate := range []CredentialInfo{
		{AccessToken: "other", RefreshToken: "old-refresh", ExpiresAt: now.Add(time.Hour)},
		{AccessToken: "other", RefreshToken: "rotated-refresh", ExpiresAt: now.Add(time.Hour)},
	} {
		if _, err := restarted.SaveManagedCredential("duplicate", duplicate); !errors.Is(err, ErrManagedIdentityExists) {
			t.Fatalf("rotated grant duplicate error = %v", err)
		}
	}
	accounts, err := restarted.ListAccounts(t.Context())
	if err != nil || len(accounts) != 1 || accounts[0].ID != "antigravity-subscription:work" || accounts[0].AuthMode != account.AuthModeOAuth {
		t.Fatalf("restarted accounts = %+v err=%v", accounts, err)
	}
}

func TestManagedRefreshRetainsIntermediateRotationFingerprints(t *testing.T) {
	now := time.Now().UTC()
	dir := t.TempDir()
	store := (&Store{ManagedDir: dir, refreshCredential: func(_ context.Context, _ *http.Client, credential CredentialInfo, _ time.Time) (CredentialInfo, error) {
		switch credential.RefreshToken {
		case "grant-a":
			credential.RefreshToken = "grant-b"
		case "grant-b":
			credential.RefreshToken = "grant-c"
		default:
			t.Fatalf("unexpected refresh grant %q", credential.RefreshToken)
		}
		credential.AccessToken = "access-" + credential.RefreshToken
		credential.ExpiresAt = now.Add(-time.Minute)
		return credential, nil
	}}).ForServing()
	acct, err := store.SaveManagedCredential("work", CredentialInfo{AccessToken: "stale", RefreshToken: "grant-a", ExpiresAt: now.Add(-time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		acct, err = store.RefreshAccount(t.Context(), http.DefaultClient, acct)
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, refresh := range []string{"grant-a", "grant-b", "grant-c"} {
		duplicate := CredentialInfo{AccessToken: "duplicate", RefreshToken: refresh, ExpiresAt: now.Add(time.Hour)}
		if _, err := store.SaveManagedCredential("duplicate-"+refresh, duplicate); !errors.Is(err, ErrManagedIdentityExists) {
			t.Fatalf("forgot rotated grant %s: %v", refresh, err)
		}
	}
}
