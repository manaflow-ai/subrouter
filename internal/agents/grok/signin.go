package grok

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/manaflow-ai/subrouter/internal/oauthdevice"
)

// Authorize runs the OAuth device-authorization grant interactively without
// mutating disk. Callers can therefore wait for human approval without holding
// the account-store transaction lock, then publish the resulting credential in
// one short atomic mutation.
func (s Store) Authorize(ctx context.Context, client *http.Client, out io.Writer) (CredentialInfo, error) {
	config, err := discoverOAuthConfig(ctx, client)
	if err != nil {
		return CredentialInfo{}, err
	}
	code, err := oauthdevice.RequestCode(ctx, client, config, time.Now())
	if err != nil {
		return CredentialInfo{}, err
	}
	if code.VerificationURIComplete != "" {
		_, _ = fmt.Fprintf(out, "Open %s to sign in to your Grok subscription", code.VerificationURIComplete)
		if code.UserCode != "" {
			_, _ = fmt.Fprintf(out, " (code %s)", code.UserCode)
		}
		_, _ = fmt.Fprintln(out, ".")
	} else {
		_, _ = fmt.Fprintf(out, "Open %s and enter code %s to sign in to your Grok subscription.\n", code.VerificationURI, code.UserCode)
	}
	token, err := oauthdevice.Poll(ctx, client, config, code, nil, nil)
	if err != nil {
		return CredentialInfo{}, err
	}
	credential := CredentialInfo{
		AccessToken:  token.AccessToken,
		RefreshToken: token.RefreshToken,
		IDToken:      token.IDToken,
		TokenType:    token.TokenType,
		Scope:        token.Scope,
		ExpiresAt:    token.ExpiresAt,
	}
	credential.Email = emailFromIDToken(token.IDToken)
	return credential, nil
}

// SignIn is the direct Store API: authorize, then persist to this store. The sr
// command uses Authorize and SaveCredential separately so it can publish the
// disk generation marker under the shared account transaction.
func (s Store) SignIn(ctx context.Context, client *http.Client, out io.Writer) (CredentialInfo, error) {
	credential, err := s.Authorize(ctx, client, out)
	if err != nil {
		return CredentialInfo{}, err
	}
	if _, err := s.SaveCredential(credential); err != nil {
		return CredentialInfo{}, fmt.Errorf("signed in but could not write the credential: %w", err)
	}
	return credential, nil
}
