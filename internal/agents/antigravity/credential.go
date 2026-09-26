// Package antigravity reads and refreshes credentials issued to Google's
// Antigravity CLI (`agy`), so Subrouter can rotate several Antigravity
// subscription accounts behind one endpoint.
package antigravity

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/manaflow-ai/subrouter/internal/credshape"
)

// CredentialInfo is one Antigravity account's OAuth credential.
type CredentialInfo struct {
	AccessToken  string
	RefreshToken string
	IDToken      string
	TokenType    string
	Scope        string
	// OAuthClientID and OAuthClientSecret bind an imported refresh-token chain
	// to the public installed-app client that issued it. The AGY Keychain blob
	// omits this association, so import discovers it once and persists it with
	// the managed profile; a router need not have the same CLI binary installed.
	OAuthClientID     string `json:"oauth_client_id,omitempty"`
	OAuthClientSecret string `json:"oauth_client_secret,omitempty"`
	// ExpiresAt is when the access token stops being accepted. Zero means the
	// stored credential did not say, in which case it is treated as expired so
	// a refresh happens rather than a request failing upstream.
	ExpiresAt time.Time
}

// unreadableCredentialPhrase appears in every credential-decode error. A
// credential that will not decode cannot be refreshed, so callers classify this
// as terminal rather than transient, the same way the Claude store does.
const unreadableCredentialPhrase = "unreadable credential"

// keyringBase64Prefix marks the encoding the current CLI writes into the
// keychain: the literal prefix followed by the base64 of the JSON payload.
const keyringBase64Prefix = "go-keyring-base64:"

// credentialPayload accepts both shapes the CLI has persisted. The Go CLI
// writes golang.org/x/oauth2.Token, whose expiry is an RFC 3339 timestamp under
// "expiry"; the earlier Node CLI wrote "expiry_date" as epoch milliseconds.
// The CLI self-updates in the background, so accepting both is what keeps a
// stored credential readable across an update rather than a hedge.
type credentialPayload struct {
	AccessToken  string          `json:"access_token"`
	RefreshToken string          `json:"refresh_token"`
	IDToken      string          `json:"id_token"`
	TokenType    string          `json:"token_type"`
	Scope        string          `json:"scope"`
	Expiry       string          `json:"expiry"`
	ExpiryDate   json.RawMessage `json:"expiry_date"`
	ExpiresIn    int64           `json:"expires_in"`
}

// ParseCredential decodes a stored Antigravity credential. source names where
// the blob came from and appears in the error along with a redacted summary of
// its shape, because a decode failure is otherwise indistinguishable between a
// keychain wrapper, a partial write, and a format change after a CLI update.
func ParseCredential(body []byte, source string, _ time.Time) (CredentialInfo, error) {
	body = unwrapKeyringBlob(body)
	// The current CLI nests the oauth2.Token under "token" next to an
	// "auth_method" marker; earlier versions persisted the token flat.
	var envelope struct {
		Token json.RawMessage `json:"token"`
	}
	if err := json.Unmarshal(body, &envelope); err == nil && len(envelope.Token) > 0 {
		body = envelope.Token
	}
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
	}
	expiry, err := payload.expiresAt()
	if err != nil {
		return CredentialInfo{}, fmt.Errorf("%s from %s: %w", unreadableCredentialPhrase, source, err)
	}
	credential.ExpiresAt = expiry
	return credential, nil
}

// EncodeCredential returns the Keychain blob format written by the current
// AGY CLI. It is used only by the native profile switcher; callers must keep
// the returned bytes in memory and never place them in command arguments or
// logs.
func EncodeCredential(credential CredentialInfo) ([]byte, error) {
	if strings.TrimSpace(credential.AccessToken) == "" || strings.TrimSpace(credential.RefreshToken) == "" {
		return nil, errors.New("Antigravity OAuth credential is incomplete")
	}
	payload := credentialPayload{
		AccessToken: credential.AccessToken, RefreshToken: credential.RefreshToken,
		IDToken: credential.IDToken, TokenType: credential.TokenType, Scope: credential.Scope,
	}
	if !credential.ExpiresAt.IsZero() {
		payload.Expiry = credential.ExpiresAt.UTC().Format(time.RFC3339)
	}
	body, err := json.Marshal(struct {
		Token      credentialPayload `json:"token"`
		AuthMethod string            `json:"auth_method"`
	}{Token: payload, AuthMethod: "oauth"})
	if err != nil {
		return nil, fmt.Errorf("encode Antigravity credential: %w", err)
	}
	encoded := make([]byte, len(keyringBase64Prefix)+base64.StdEncoding.EncodedLen(len(body)))
	copy(encoded, keyringBase64Prefix)
	base64.StdEncoding.Encode(encoded[len(keyringBase64Prefix):], body)
	return encoded, nil
}

// expiresAt resolves an absolute expiry from whichever field the writer used. An
// unparseable expiry is an error rather than a zero value: silently treating a
// live token as expired would burn a refresh on every request.
func (p credentialPayload) expiresAt() (time.Time, error) {
	if raw := strings.TrimSpace(p.Expiry); raw != "" {
		parsed, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			return time.Time{}, fmt.Errorf("expiry %q is not RFC 3339: %w", raw, err)
		}
		return parsed.UTC(), nil
	}
	if len(p.ExpiryDate) > 0 {
		millis, err := parseEpochMillis(p.ExpiryDate)
		if err != nil {
			return time.Time{}, err
		}
		if millis > 0 {
			return time.UnixMilli(millis).UTC(), nil
		}
	}
	// expires_in is meaningful only when paired with a stable issuance time.
	// The keychain blob provides none, and rebasing it on every read would make
	// an expired token appear perpetually fresh. Leave it unknown so callers
	// refresh before use.
	return time.Time{}, nil
}

// parseEpochMillis accepts the epoch-millisecond expiry as either a JSON number
// or a JSON string, since both have been observed in Google CLI credential
// files.
func parseEpochMillis(raw json.RawMessage) (int64, error) {
	var millis int64
	if err := json.Unmarshal(raw, &millis); err == nil {
		return millis, nil
	}
	var text string
	if err := json.Unmarshal(raw, &text); err != nil {
		return 0, errors.New("expiry_date is neither a number nor a string")
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return 0, nil
	}
	if err := json.Unmarshal([]byte(text), &millis); err != nil {
		return 0, fmt.Errorf("expiry_date %q is not an epoch-millisecond value", text)
	}
	return millis, nil
}

// unwrapKeyringBlob decodes the encoding the current CLI writes into the
// keychain: the literal "go-keyring-base64:" prefix followed by the base64 of
// the JSON payload. A blob without the prefix is returned unchanged so both
// the current and earlier encodings stay readable. A blob that carries the
// prefix but does not base64-decode is returned unchanged as well, so the
// caller's JSON error describes the payload as stored.
func unwrapKeyringBlob(body []byte) []byte {
	text := bytes.TrimSpace(body)
	if !bytes.HasPrefix(text, []byte(keyringBase64Prefix)) {
		return body
	}
	decoded, err := base64.StdEncoding.DecodeString(string(text[len(keyringBase64Prefix):]))
	if err != nil {
		return body
	}
	return decoded
}

// refreshLead is how far before expiry a credential is treated as expired. The
// CLI itself refreshes five minutes early; matching it means a token handed to
// an upstream always has usable life left.
const refreshLead = 5 * time.Minute

// NeedsRefresh reports whether the access token should be refreshed before use.
// A credential with no expiry, or no access token, always needs one.
func (c CredentialInfo) NeedsRefresh(now time.Time) bool {
	if strings.TrimSpace(c.AccessToken) == "" {
		return true
	}
	if c.ExpiresAt.IsZero() {
		return true
	}
	return !now.Add(refreshLead).Before(c.ExpiresAt)
}
