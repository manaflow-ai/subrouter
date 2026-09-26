// Package grok holds the xAI Grok subscription credential Subrouter obtains
// through the OAuth device-authorization grant, so a Grok subscription can be
// routed behind one endpoint. Unlike the CLI-backed providers there is no Grok
// CLI credential to import, so Subrouter owns the credential file at
// ~/.subrouter/grok/oauth.json.
package grok

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/manaflow-ai/subrouter/internal/credshape"
)

// CredentialInfo is a Grok subscription OAuth credential.
type CredentialInfo struct {
	AccessToken  string
	RefreshToken string
	IDToken      string
	TokenType    string
	Scope        string
	// Email is the sign-in identity, decoded from the ID token when one is
	// present. It labels the account; it is never sent upstream.
	Email string
	// ExpiresAt is when the access token stops being accepted. Zero means the
	// stored credential did not say, in which case it is treated as expired so
	// a refresh happens rather than a request failing upstream.
	ExpiresAt time.Time
}

// unreadableCredentialPhrase appears in every credential-decode error. A
// credential that will not decode cannot be refreshed, so callers classify this
// as terminal rather than transient, the same way the Claude store does.
const unreadableCredentialPhrase = "unreadable credential"

// credentialPayload is the shape Subrouter persists at
// ~/.subrouter/grok/oauth.json. expires_at is epoch seconds; expires_in is
// kept for humans inspecting the file and as a fallback when expires_at is
// absent.
type credentialPayload struct {
	AccessToken  string          `json:"access_token"`
	RefreshToken string          `json:"refresh_token"`
	IDToken      string          `json:"id_token"`
	TokenType    string          `json:"token_type"`
	Scope        string          `json:"scope"`
	Email        string          `json:"email"`
	ExpiresAt    json.RawMessage `json:"expires_at"`
	ExpiresIn    int64           `json:"expires_in"`
}

// ParseCredential decodes a stored Grok credential. source names where the
// blob came from and appears in the error along with a redacted summary of its
// shape, because a decode failure is otherwise indistinguishable between a
// partial write and a format change.
func ParseCredential(body []byte, source string, now time.Time) (CredentialInfo, error) {
	var payload credentialPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		return CredentialInfo{}, fmt.Errorf("%s from %s (%s): %w", unreadableCredentialPhrase, source, credshape.Describe(body, err), err)
	}
	if strings.TrimSpace(payload.AccessToken) == "" && strings.TrimSpace(payload.RefreshToken) == "" {
		return CredentialInfo{}, fmt.Errorf("%s from %s (bytes=%d): no access or refresh token", unreadableCredentialPhrase, source, len(body))
	}
	credential := CredentialInfo{
		AccessToken:  payload.AccessToken,
		RefreshToken: payload.RefreshToken,
		IDToken:      payload.IDToken,
		TokenType:    payload.TokenType,
		Scope:        payload.Scope,
		Email:        payload.Email,
	}
	expiry, err := payload.expiresAt(now)
	if err != nil {
		return CredentialInfo{}, fmt.Errorf("%s from %s: %w", unreadableCredentialPhrase, source, err)
	}
	credential.ExpiresAt = expiry
	return credential, nil
}

// expiresAt resolves the expiry from whichever field carries it. An
// unparseable expires_at is an error rather than a zero value: silently
// treating a live token as expired would burn a refresh on every request.
func (p credentialPayload) expiresAt(now time.Time) (time.Time, error) {
	if len(p.ExpiresAt) > 0 {
		var seconds int64
		if err := json.Unmarshal(p.ExpiresAt, &seconds); err != nil {
			return time.Time{}, fmt.Errorf("expires_at is not an epoch-second value")
		}
		if seconds > 0 {
			return time.Unix(seconds, 0).UTC(), nil
		}
	}
	if p.ExpiresIn > 0 {
		return now.Add(time.Duration(p.ExpiresIn) * time.Second).UTC(), nil
	}
	return time.Time{}, nil
}

// refreshLead is how far before expiry a credential is treated as expired,
// matching the lead the reference xAI clients use.
const refreshLead = 5 * time.Minute

// NeedsRefresh reports whether the access token should be refreshed before
// use. A credential with no expiry, or no access token, always needs one.
func (c CredentialInfo) NeedsRefresh(now time.Time) bool {
	if strings.TrimSpace(c.AccessToken) == "" {
		return true
	}
	if c.ExpiresAt.IsZero() {
		return true
	}
	return !now.Add(refreshLead).Before(c.ExpiresAt)
}

// emailFromIDToken extracts the email claim from a JWT without verifying it;
// the token came straight from the provider's token endpoint over TLS. It is
// used only to label the account, so a malformed token just means no label.
func emailFromIDToken(idToken string) string {
	parts := strings.Split(idToken, ".")
	if len(parts) < 2 {
		return ""
	}
	payload := parts[1]
	payload += strings.Repeat("=", (4-len(payload)%4)%4)
	raw, err := base64.URLEncoding.DecodeString(payload)
	if err != nil {
		return ""
	}
	var claims map[string]any
	if err := json.Unmarshal(raw, &claims); err != nil {
		return ""
	}
	email, _ := claims["email"].(string)
	return strings.TrimSpace(email)
}
