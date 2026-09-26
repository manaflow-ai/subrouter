// Package kimi reads and refreshes the credential the Kimi Code CLI holds on
// this machine, so Subrouter can route a Kimi subscription behind one
// endpoint.
package kimi

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/manaflow-ai/subrouter/internal/credshape"
)

// CredentialInfo is the Kimi Code CLI's OAuth credential.
type CredentialInfo struct {
	AccessToken   string `json:"access_token,omitempty"`
	RefreshToken  string `json:"refresh_token,omitempty"`
	TokenType     string `json:"token_type,omitempty"`
	Scope         string `json:"scope,omitempty"`
	AccountLabel  string `json:"account_label,omitempty"`
	OAuthDeviceID string `json:"oauth_device_id,omitempty"`
	// ExpiresAt is when the access token stops being accepted. Zero means the
	// stored credential did not say, in which case it is treated as expired so
	// a refresh happens rather than a request failing upstream.
	ExpiresAt time.Time `json:"expires_at,omitempty"`
}

// unreadableCredentialPhrase appears in every credential-decode error. A
// credential that will not decode cannot be refreshed, so callers classify this
// as terminal rather than transient, the same way the Claude store does.
const unreadableCredentialPhrase = "unreadable credential"

// credentialPayload is the shape the Kimi Code CLI persists at
// ~/.kimi-code/credentials/kimi-code.json. The access token lives 900 seconds,
// so expires_at is what keeps a stored credential from being refreshed on
// every request.
type credentialPayload struct {
	AccessToken   string          `json:"access_token"`
	RefreshToken  string          `json:"refresh_token"`
	TokenType     string          `json:"token_type"`
	Scope         string          `json:"scope"`
	AccountLabel  string          `json:"account_label"`
	OAuthDeviceID string          `json:"oauth_device_id"`
	ExpiresAt     json.RawMessage `json:"expires_at"`
	ExpiresIn     int64           `json:"expires_in"`
}

// ParseCredential decodes a stored Kimi credential. source names where the
// blob came from and appears in the error along with a redacted summary of its
// shape, because a decode failure is otherwise indistinguishable between a
// partial write and a format change after a CLI update.
func ParseCredential(body []byte, source string, now time.Time) (CredentialInfo, error) {
	var payload credentialPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		return CredentialInfo{}, fmt.Errorf("%s from %s (%s): %w", unreadableCredentialPhrase, source, credshape.Describe(body, err), err)
	}
	if strings.TrimSpace(payload.AccessToken) == "" && strings.TrimSpace(payload.RefreshToken) == "" {
		return CredentialInfo{}, fmt.Errorf("%s from %s (bytes=%d): no access or refresh token", unreadableCredentialPhrase, source, len(body))
	}
	credential := CredentialInfo{
		AccessToken:   payload.AccessToken,
		RefreshToken:  payload.RefreshToken,
		TokenType:     payload.TokenType,
		Scope:         payload.Scope,
		AccountLabel:  payload.AccountLabel,
		OAuthDeviceID: payload.OAuthDeviceID,
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
		seconds, err := parseEpochSeconds(p.ExpiresAt)
		if err != nil {
			return time.Time{}, err
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

// parseEpochSeconds accepts the epoch-second expiry as either a JSON number or
// a JSON string, since both are plausible across CLI versions.
func parseEpochSeconds(raw json.RawMessage) (int64, error) {
	var seconds int64
	if err := json.Unmarshal(raw, &seconds); err == nil {
		return seconds, nil
	}
	var text string
	if err := json.Unmarshal(raw, &text); err != nil {
		return 0, fmt.Errorf("expires_at is neither a number nor a string")
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return 0, nil
	}
	if err := json.Unmarshal([]byte(text), &seconds); err != nil {
		return 0, fmt.Errorf("expires_at string is not an epoch-second value (length=%d)", len(text))
	}
	return seconds, nil
}

// refreshLead is how far before expiry a credential is treated as expired. The
// access token lives 900 seconds, so the five-minute lead the reference
// clients use is a third of its life — refreshing earlier wastes single-use
// refresh attempts, refreshing later hands upstream a dead token.
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

// ValidateOAuthDeviceID checks the stable device identity Kimi binds to an
// installed-app authorization. Managed credentials retain this value so a
// refresh uses the same identity even after the profile moves to a server.
func ValidateOAuthDeviceID(value string) error {
	if value == "" || len(value) > 128 || strings.TrimSpace(value) != value {
		return fmt.Errorf("OAuth device identity is empty or invalid")
	}
	for _, char := range value {
		if char < 0x20 || char > 0x7e {
			return fmt.Errorf("OAuth device identity contains a non-ASCII character")
		}
	}
	return nil
}
