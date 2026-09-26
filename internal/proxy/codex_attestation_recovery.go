package proxy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/manaflow-ai/subrouter/internal/accounts"
)

const pendingCodexAttestationVersion = 1

type pendingCodexAttestation struct {
	Version       int                         `json:"version"`
	AccountID     string                      `json:"account_id"`
	OAuthIdentity string                      `json:"oauth_identity"`
	Account       accounts.StoredCodexAccount `json:"account"`
}

func pendingCodexAttestationDir(storeDir string) string {
	return filepath.Join(storeDir, ".pending-codex-attestations")
}

func pendingCodexAttestationPath(storeDir, accountID string) string {
	digest := sha256.Sum256([]byte(accountID))
	return filepath.Join(pendingCodexAttestationDir(storeDir), hex.EncodeToString(digest[:])+".json")
}

// attestAndSaveTenantCodexOAuth durably stages the provider-rotated credential
// before canonical account publication. A retry after a failed save or process
// restart promotes the already server-attested credential instead of reusing
// the now-invalid submitted refresh token.
func attestAndSaveTenantCodexOAuth(
	ctx context.Context,
	client *http.Client,
	store accounts.CodexStore,
	submitted accounts.StoredCodexAccount,
	validate func(*accounts.StoredCodexAccount) error,
) error {
	if submitted.Auth.Tokens == nil {
		return invalidAccountImport("OAuth account payload is incomplete")
	}
	submittedIdentity, err := accounts.ExtractEmailFromJWT(submitted.Auth.Tokens.IDToken)
	if err != nil || strings.TrimSpace(submittedIdentity) == "" {
		return invalidAccountImport("OAuth credential identity is invalid")
	}

	pending, found, err := readPendingCodexAttestation(store.StoreDir(), submitted.Email)
	if err != nil {
		return fmt.Errorf("read pending OAuth credential transfer: %w", err)
	}
	if found {
		if pending.Version != pendingCodexAttestationVersion ||
			pending.AccountID != submitted.Email ||
			!accounts.SameCodexOAuthIdentity(pending.Account.Auth, submitted.Auth) {
			return invalidAccountImport("pending OAuth credential transfer does not match this account")
		}
		return promotePendingCodexAttestation(store, pending, validate)
	}

	attested, err := attestTenantCodexOAuth(ctx, client, submitted)
	if err != nil {
		return err
	}
	pending = pendingCodexAttestation{
		Version: pendingCodexAttestationVersion, AccountID: submitted.Email,
		OAuthIdentity: attested.LoginEmail(), Account: attested,
	}
	// This is the first local operation after the provider returns its rotated
	// refresh token. Once it succeeds, all later failures are restart-recoverable.
	if err := writePendingCodexAttestation(store.StoreDir(), pending); err != nil {
		return fmt.Errorf("stage server-attested OAuth credential: %w", err)
	}
	if err := promotePendingCodexAttestation(store, pending, validate); err != nil {
		return err
	}
	return nil
}

func promotePendingCodexAttestation(
	store accounts.CodexStore,
	pending pendingCodexAttestation,
	validate func(*accounts.StoredCodexAccount) error,
) error {
	attested := pending.Account
	if attested.OAuthCredentialOrigin != accounts.CodexOAuthOriginServerAttested || attested.Auth.Tokens == nil {
		return invalidAccountImport("pending OAuth credential transfer is not server-attested")
	}
	identity, err := accounts.ExtractEmailFromJWT(attested.Auth.Tokens.IDToken)
	if err != nil || strings.TrimSpace(identity) == "" {
		return invalidAccountImport("pending OAuth credential transfer identity is invalid")
	}
	if validate != nil {
		if err := validate(&attested); err != nil {
			// The provider returned an unusable or wrong-identity credential. It
			// must remain neither routable nor recoverable.
			return errors.Join(err, clearPendingCodexAttestation(store.StoreDir(), pending.AccountID))
		}
	}
	if !strings.EqualFold(strings.TrimSpace(identity), strings.TrimSpace(pending.OAuthIdentity)) {
		return errors.Join(
			invalidAccountImport("pending OAuth credential transfer identity is invalid"),
			clearPendingCodexAttestation(store.StoreDir(), pending.AccountID),
		)
	}
	if err := store.SaveStored(attested); err != nil {
		// Retain the staged credential. The same authenticated tenant can retry
		// and promote it without presenting a still-valid old refresh token.
		return fmt.Errorf("save server-attested OAuth credential: %w", err)
	}
	if err := clearPendingCodexAttestation(store.StoreDir(), pending.AccountID); err != nil {
		return fmt.Errorf("clear pending OAuth credential transfer: %w", err)
	}
	return nil
}

func readPendingCodexAttestation(storeDir, accountID string) (pendingCodexAttestation, bool, error) {
	var pending pendingCodexAttestation
	body, err := os.ReadFile(pendingCodexAttestationPath(storeDir, accountID))
	if errors.Is(err, os.ErrNotExist) {
		return pending, false, nil
	}
	if err != nil {
		return pending, false, err
	}
	if err := json.Unmarshal(body, &pending); err != nil {
		return pending, false, err
	}
	return pending, true, nil
}

func writePendingCodexAttestation(storeDir string, pending pendingCodexAttestation) (err error) {
	dir := pendingCodexAttestationDir(storeDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return err
	}
	body, err := json.Marshal(pending)
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(dir, ".attestation-*.json")
	if err != nil {
		return err
	}
	tempPath := file.Name()
	defer func() {
		if file != nil {
			err = errors.Join(err, file.Close())
		}
		if err != nil {
			_ = os.Remove(tempPath)
		}
	}()
	if err := file.Chmod(0o600); err != nil {
		return err
	}
	if _, err := file.Write(body); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	file = nil
	if err := os.Rename(tempPath, pendingCodexAttestationPath(storeDir, pending.AccountID)); err != nil {
		return err
	}
	return syncAccountStateDir(dir)
}

func clearPendingCodexAttestation(storeDir, accountID string) error {
	path := pendingCodexAttestationPath(storeDir, accountID)
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return syncAccountStateDir(filepath.Dir(path))
}

func recoverPendingCodexAttestations(store accounts.CodexStore) error {
	dir := pendingCodexAttestationDir(store.StoreDir())
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		body, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			return err
		}
		var pending pendingCodexAttestation
		if err := json.Unmarshal(body, &pending); err != nil {
			return fmt.Errorf("decode pending OAuth credential transfer: %w", err)
		}
		if filepath.Base(pendingCodexAttestationPath(store.StoreDir(), pending.AccountID)) != entry.Name() {
			return errors.New("pending OAuth credential transfer filename does not match its account")
		}
		if err := advanceAccountDiskGeneration(store.StoreDir()); err != nil {
			return fmt.Errorf("publish recovered OAuth credential transfer: %w", err)
		}
		if err := promotePendingCodexAttestation(store, pending, func(attested *accounts.StoredCodexAccount) error {
			if pending.Version != pendingCodexAttestationVersion ||
				attested.Email != pending.AccountID ||
				attested.ProviderOrDefault() != accounts.ProviderCodex ||
				attested.Auth.Tokens == nil {
				return invalidAccountImport("pending OAuth credential transfer is invalid")
			}
			return nil
		}); err != nil {
			return fmt.Errorf("recover pending OAuth credential transfer: %w", err)
		}
	}
	return nil
}
