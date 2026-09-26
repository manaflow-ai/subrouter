package kimi

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/manaflow-ai/subrouter/account"
	"github.com/manaflow-ai/subrouter/internal/oauthdevice"
)

// SignInManaged runs Kimi's public device-authorization flow and stores the
// result in an isolated Subrouter profile. It never reads or rewrites the Kimi
// CLI's single global credential, so adding a second account cannot log the
// currently selected CLI account out.
func (s Store) SignInManaged(ctx context.Context, client *http.Client, label string, out io.Writer) (account.Account, error) {
	credential, err := s.AuthorizeManaged(ctx, client, label, out)
	if err != nil {
		return account.Account{}, err
	}
	acct, err := s.SaveManagedCredential(label, credential)
	if err != nil {
		return account.Account{}, fmt.Errorf("signed in but could not save the managed Kimi credential: %w", err)
	}
	return acct, nil
}

// AuthorizeManaged performs Kimi's public device grant without deciding where
// the resulting credential is persisted. Callers that publish credentials to
// a running proxy can therefore hold the account-state transaction only for
// the final disk mutation, not while the user is in the browser.
func (s Store) AuthorizeManaged(ctx context.Context, client *http.Client, label string, out io.Writer) (CredentialInfo, error) {
	if _, err := ManagedAccountID(label); err != nil {
		return CredentialInfo{}, err
	}
	cfg, err := s.oauthConfig()
	if err != nil {
		return CredentialInfo{}, err
	}
	code, err := oauthdevice.RequestCode(ctx, client, cfg, time.Now())
	if err != nil {
		return CredentialInfo{}, err
	}
	if code.VerificationURIComplete != "" {
		_, _ = fmt.Fprintf(out, "Open %s to sign in to the Kimi account named %q", code.VerificationURIComplete, label)
		if code.UserCode != "" {
			_, _ = fmt.Fprintf(out, " (code %s)", code.UserCode)
		}
		_, _ = fmt.Fprintln(out, ".")
	} else {
		_, _ = fmt.Fprintf(out, "Open %s and enter code %s to sign in to the Kimi account named %q.\n", code.VerificationURI, code.UserCode, label)
	}
	token, err := oauthdevice.Poll(ctx, client, cfg, code, nil, nil)
	if err != nil {
		return CredentialInfo{}, err
	}
	credential := CredentialInfo{
		AccessToken:   token.AccessToken,
		RefreshToken:  token.RefreshToken,
		TokenType:     token.TokenType,
		Scope:         token.Scope,
		ExpiresAt:     token.ExpiresAt,
		OAuthDeviceID: cfg.Headers["X-Msh-Device-Id"],
	}
	if err := validateManagedAuthorization(credential, time.Now()); err != nil {
		return CredentialInfo{}, err
	}
	return credential, nil
}

func validateManagedAuthorization(credential CredentialInfo, now time.Time) error {
	if strings.TrimSpace(credential.AccessToken) == "" {
		return fmt.Errorf("Kimi device authorization returned an unusable credential: missing access token")
	}
	if strings.TrimSpace(credential.RefreshToken) == "" {
		return fmt.Errorf("Kimi device authorization returned an unusable credential: missing refresh token")
	}
	if credential.ExpiresAt.IsZero() || !credential.ExpiresAt.After(now) {
		return fmt.Errorf("Kimi device authorization returned an unusable credential: missing or expired access-token lifetime")
	}
	if err := ValidateOAuthDeviceID(credential.OAuthDeviceID); err != nil {
		return fmt.Errorf("Kimi device authorization returned an unusable credential: invalid device identity")
	}
	return nil
}
