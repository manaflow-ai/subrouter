package accounts

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// ErrCodexCredentialAlreadyIsolated reports a stored account whose provenance
// already satisfies the serving isolation gate, so attestation is a no-op.
var ErrCodexCredentialAlreadyIsolated = errors.New("stored Codex credential is already isolated")

// ErrCodexCredentialSharesActiveAuth reports a stored account whose refresh
// token is also the interactive Codex login on this machine. Rotating it here
// would log that user out, so it needs an isolated re-login instead.
var ErrCodexCredentialSharesActiveAuth = errors.New("stored Codex credential shares the interactive Codex refresh token; use an isolated login")

// AttestStoredLegacyOAuth proves that a legacy stored Codex OAuth chain is
// owned by this store, without an interactive login. It refreshes the stored
// credential once through the provider, which rotates the refresh token, and
// records the rotated credential as server-attested. A chain that the provider
// refuses to refresh, or refuses to rotate, is left untouched.
//
// This is the same attestation the account import path performs for a
// submitted credential; it exists so records written before provenance was
// tracked can be repaired in place instead of re-enrolled one login at a time.
func (s CodexStore) AttestStoredLegacyOAuth(ctx context.Context, client *http.Client, identifier string) (StoredCodexAccount, error) {
	identifier = strings.TrimSpace(identifier)
	if identifier == "" {
		return StoredCodexAccount{}, errors.New("account identifier is required")
	}
	lock, err := s.lockStoredAccount(identifier)
	if err != nil {
		return StoredCodexAccount{}, err
	}
	defer lock.Close()

	account, found, err := s.findStoredExact(identifier)
	if err != nil {
		return StoredCodexAccount{}, err
	}
	if !found {
		return StoredCodexAccount{}, fmt.Errorf("account %q was not found", identifier)
	}
	if account.IsAPIKey() || account.ProviderOrDefault() != ProviderCodex || account.Auth.Tokens == nil {
		return account, fmt.Errorf("account %q is not a Codex OAuth account", identifier)
	}
	if account.OAuthCredentialOrigin == CodexOAuthOriginIsolatedServerLogin ||
		account.OAuthCredentialOrigin == CodexOAuthOriginServerAttested {
		return account, ErrCodexCredentialAlreadyIsolated
	}
	if err := checkCodexHostClaim(account); err != nil {
		return account, err
	}
	shared, err := storedCodexRefreshTokenSharesActive(account.Auth.Tokens.RefreshToken)
	if err != nil {
		// Fail closed: without a readable interactive file the chain cannot be
		// proven private, and rotating it would log a real user out.
		return account, fmt.Errorf("read interactive Codex auth before attesting %q: %w", identifier, err)
	}
	if shared {
		return account, ErrCodexCredentialSharesActiveAuth
	}
	if strings.TrimSpace(account.Auth.Tokens.RefreshToken) == "" {
		return account, fmt.Errorf("account %q has no refresh token", identifier)
	}

	previous := account
	submittedRefreshToken := account.Auth.Tokens.RefreshToken
	refreshed, err := RefreshCodexAuth(ctx, client, account.Auth)
	if err != nil {
		// A rejected chain is reported, never recorded: the stored record stays
		// byte-identical so a retry or an isolated re-login starts clean.
		return account, err
	}
	if refreshed.Tokens == nil || subtle.ConstantTimeCompare(
		[]byte(refreshed.Tokens.RefreshToken), []byte(submittedRefreshToken),
	) == 1 {
		return account, errors.New("OAuth provider did not rotate the stored refresh token")
	}
	account.Auth = refreshed
	account.Auth.RefreshFailure = nil
	account.OAuthCredentialOrigin = CodexOAuthOriginServerAttested
	appendCodexAuthBreadcrumb(ctx, s, &account, "legacy_credential_attested", "account_manager", true, &previous, &account, nil, nil)
	if err := s.saveStoredUnlocked(account); err != nil {
		return account, err
	}
	return account, nil
}

func storedCodexRefreshTokenSharesActive(refreshToken string) (bool, error) {
	if refreshToken == "" {
		return false, nil
	}
	active, ok, err := ReadActiveCodexAuth()
	if err != nil {
		return false, err
	}
	if !ok || active.Tokens == nil {
		return false, nil
	}
	return active.Tokens.RefreshToken != "" && active.Tokens.RefreshToken == refreshToken, nil
}
