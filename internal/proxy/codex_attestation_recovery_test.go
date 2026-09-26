package proxy

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	agentclaude "github.com/manaflow-ai/subrouter/internal/agents/claude"
)

func TestTenantCodexAttestationRecoversRotatedCredentialAfterCanonicalSaveFailure(t *testing.T) {
	store := accounts.CodexStore{Dir: filepath.Join(t.TempDir(), "accounts")}
	submitted := proxyStoredOAuthAccount("owner@example.com", "submitted", time.Now().Add(time.Hour))
	attested := proxyStoredOAuthAccount("owner@example.com", "server", time.Now().Add(time.Hour))
	attested.OAuthCredentialOrigin = accounts.CodexOAuthOriginServerAttested
	pending := pendingCodexAttestation{
		Version: pendingCodexAttestationVersion, AccountID: submitted.Email,
		OAuthIdentity: submitted.Email, Account: attested,
	}
	if err := writePendingCodexAttestation(store.StoreDir(), pending); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(pendingCodexAttestationPath(store.StoreDir(), submitted.Email))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("pending credential mode = %o, want 600", info.Mode().Perm())
	}

	// A directory at the canonical account path makes the atomic rename fail
	// after the rotated credential has already been durably staged.
	if err := os.MkdirAll(attested.SourcePath(store), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := promotePendingCodexAttestation(store, pending, nil); err == nil {
		t.Fatal("canonical save unexpectedly succeeded")
	}
	if _, found, err := readPendingCodexAttestation(store.StoreDir(), submitted.Email); err != nil || !found {
		t.Fatalf("failed save discarded rotated credential: found=%v err=%v", found, err)
	}
	if err := os.RemoveAll(attested.SourcePath(store)); err != nil {
		t.Fatal(err)
	}

	ref, err := OpenAccountRef(store, agentclaude.Store{Dir: t.TempDir()}, http.DefaultClient)
	if err != nil {
		t.Fatal(err)
	}
	stored, found, err := store.FindStored(submitted.Email)
	if err != nil || !found {
		t.Fatalf("recovered account = found %v, err %v", found, err)
	}
	if stored.Auth.Tokens == nil || stored.Auth.Tokens.RefreshToken != attested.Auth.Tokens.RefreshToken ||
		stored.OAuthCredentialOrigin != accounts.CodexOAuthOriginServerAttested {
		t.Fatalf("recovered credential lost server attestation: %#v", stored)
	}
	if loaded := ref.All(); len(loaded) != 1 || loaded[0].ID != submitted.Email {
		t.Fatalf("restarted worker did not load recovered credential: %+v", loaded)
	}
	if _, found, err := readPendingCodexAttestation(store.StoreDir(), submitted.Email); err != nil || found {
		t.Fatalf("pending credential remained after promotion: found=%v err=%v", found, err)
	}
}

func TestPendingCodexAttestationCannotCrossOAuthIdentity(t *testing.T) {
	store := accounts.CodexStore{Dir: filepath.Join(t.TempDir(), "accounts")}
	pendingAccount := proxyStoredOAuthAccount("owner@example.com", "server", time.Now().Add(time.Hour))
	pendingAccount.OAuthCredentialOrigin = accounts.CodexOAuthOriginServerAttested
	if err := writePendingCodexAttestation(store.StoreDir(), pendingCodexAttestation{
		Version: pendingCodexAttestationVersion, AccountID: "stable-routing-id",
		OAuthIdentity: "owner@example.com", Account: pendingAccount,
	}); err != nil {
		t.Fatal(err)
	}
	submitted := proxyStoredOAuthAccount("attacker@example.com", "submitted", time.Now().Add(time.Hour))
	submitted.Email = "stable-routing-id"
	err := attestAndSaveTenantCodexOAuth(context.Background(), http.DefaultClient, store, submitted, nil)
	var validationErr *accountImportValidationError
	if !errors.As(err, &validationErr) {
		t.Fatalf("identity mismatch error = %v, want validation rejection", err)
	}
	if _, found, findErr := store.FindStored("stable-routing-id"); findErr != nil || found {
		t.Fatalf("mismatched identity promoted pending credential: found=%v err=%v", found, findErr)
	}
}
