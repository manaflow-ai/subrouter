package claude

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// Claude Code's `claude setup-token` mints a long-lived OAuth access token
// (prefix sk-ant-oat01-, scope user:inference only) that is valid for one year
// and has no refresh token. Subrouter stores it as an ordinary credential whose
// RefreshToken is empty: every refresh path already no-ops without a refresh
// token, so the only things this kind of credential needs are an explicit
// expiry, the scope list that makes Claude Code treat the profile as logged
// in, and validation that accepts the missing refresh token.
const (
	// SetupTokenLifetime is what Claude Code advertises for a setup token.
	SetupTokenLifetime = 365 * 24 * time.Hour
	// SetupTokenScope is the only scope a setup token carries.
	SetupTokenScope = "user:inference"
	// SetupTokenPrefix is shared by every Claude OAuth access token, so it
	// rejects API keys (sk-ant-api03-) and stray text but cannot by itself
	// tell a setup token from a short-lived login token.
	SetupTokenPrefix = "sk-ant-oat"
	// SetupTokenExpiryWarning is how long before expiry status output starts
	// asking for a re-add.
	SetupTokenExpiryWarning = 30 * 24 * time.Hour
)

// ErrSetupTokenRejected means Anthropic answered the verification probe with
// an authentication failure: the token is malformed, revoked, or expired.
var ErrSetupTokenRejected = errors.New("Anthropic rejected the Claude setup token")

// ValidateSetupToken checks the shape of a pasted setup token without
// contacting Anthropic.
func ValidateSetupToken(token string) error {
	if token == "" {
		return errors.New("Claude setup token is empty")
	}
	if !strings.HasPrefix(token, SetupTokenPrefix) {
		if strings.HasPrefix(token, "sk-ant-api") {
			return errors.New("that is an Anthropic API key, not a Claude setup token; run 'claude setup-token' to mint one")
		}
		return fmt.Errorf("Claude setup token must start with %s-", SetupTokenPrefix)
	}
	if len(token) < 40 || len(token) > 512 {
		return errors.New("Claude setup token has an unexpected length")
	}
	if strings.IndexFunc(token, func(r rune) bool {
		return r > unicode.MaxASCII || unicode.IsSpace(r) || unicode.IsControl(r)
	}) >= 0 {
		return errors.New("Claude setup token contains whitespace or control characters")
	}
	return nil
}

// SetupTokenCredential builds the stored credential for a setup token minted
// at issuedAt. The expiry is recorded so status output can say when the token
// stops working and so an expired token fails closed instead of producing
// upstream 401s.
func SetupTokenCredential(token string, issuedAt time.Time) CredentialInfo {
	return CredentialInfo{
		AccessToken: token,
		ExpiresAt:   issuedAt.Add(SetupTokenLifetime).UnixMilli(),
		Scopes:      []string{SetupTokenScope},
	}
}

// LongLived reports whether the credential is a refresh-less access token such
// as a setup token. Such a credential is used until its recorded expiry and
// can only be replaced by a new add.
func (credential *CredentialInfo) LongLived() bool {
	return credential != nil && credential.AccessToken != "" && credential.RefreshToken == ""
}

// usageEndpointScope is the scope Anthropic's OAuth usage endpoint requires.
const usageEndpointScope = "user:profile"

// LacksUsageScope reports whether the recorded scopes rule out the usage
// endpoint, as a setup token's do. A credential with no recorded scopes is
// unknown, not denied.
func (credential *CredentialInfo) LacksUsageScope() bool {
	if credential == nil || len(credential.Scopes) == 0 {
		return false
	}
	for _, scope := range credential.Scopes {
		if scope == usageEndpointScope {
			return false
		}
	}
	return true
}

// ExpiresAtTime returns the recorded expiry, if any.
func (credential *CredentialInfo) ExpiresAtTime() (time.Time, bool) {
	if credential == nil || credential.ExpiresAt <= 0 {
		return time.Time{}, false
	}
	return time.UnixMilli(credential.ExpiresAt), true
}

// Equal compares two credentials field by field. CredentialInfo carries a
// slice, so it is not comparable with ==.
func (credential CredentialInfo) Equal(other CredentialInfo) bool {
	if credential.AccessToken != other.AccessToken ||
		credential.RefreshToken != other.RefreshToken ||
		credential.SubscriptionType != other.SubscriptionType ||
		credential.RateLimitTier != other.RateLimitTier ||
		credential.ExpiresAt != other.ExpiresAt ||
		len(credential.Scopes) != len(other.Scopes) {
		return false
	}
	for i := range credential.Scopes {
		if credential.Scopes[i] != other.Scopes[i] {
			return false
		}
	}
	return true
}

// Validate reports whether the credential can be stored: a refreshable OAuth
// pair, or a long-lived token with a recorded, still-future expiry. Every
// import path (local add, server account-import, tenant upload) shares it so a
// setup token is accepted everywhere or nowhere.
func (credential CredentialInfo) Validate() error {
	return credential.validateAt(time.Now())
}

func (credential CredentialInfo) validateAt(now time.Time) error {
	if strings.TrimSpace(credential.AccessToken) == "" {
		return errors.New("Claude OAuth access token is required")
	}
	if strings.TrimSpace(credential.RefreshToken) != "" {
		return nil
	}
	expiresAt, ok := credential.ExpiresAtTime()
	if !ok {
		return errors.New("Claude OAuth refresh token is required unless the credential is a long-lived setup token with an expiry")
	}
	if !expiresAt.After(now) {
		return fmt.Errorf("Claude setup token expired %s", expiresAt.UTC().Format("2006-01-02"))
	}
	return nil
}

// longLivedCredentialError explains why a refresh-less credential cannot be
// used. The wording contains "no usable credential" because the proxy
// classifies that phrase as a terminal credential error (the account leaves
// routing until it is re-added) rather than a transient one.
func longLivedCredentialError(profileName string, credential *CredentialInfo, now time.Time) error {
	if !credential.LongLived() {
		return nil
	}
	expiresAt, ok := credential.ExpiresAtTime()
	if !ok || now.Before(expiresAt) {
		return nil
	}
	return fmt.Errorf(
		"Claude profile %q has no usable credential: setup token expired %s; re-add with 'sr claude add %s'",
		profileName, expiresAt.UTC().Format("2006-01-02"), profileName,
	)
}

// TokenIdentity is what Anthropic's answer to the verification probe reveals
// about the account a token bills against. A setup token carries only
// user:inference, so the profile endpoint that would name the account is
// closed to it; the response headers are the only identity available.
type TokenIdentity struct {
	// OrganizationID is the anthropic-organization-id response header.
	OrganizationID string
	// WeeklyResetAt is when the account-wide weekly limit resets, zero when
	// the response did not say.
	WeeklyResetAt time.Time
}

// VerifyAccessToken sends the one-token Messages probe Claude Code itself
// would send and reports ErrSetupTokenRejected on an authentication failure.
// Any 2xx or quota response proves the token is accepted; the probe has the
// Claude Code request shape because Anthropic answers a bare subscription
// OAuth call with a headerless 429 regardless of the token.
func VerifyAccessToken(ctx context.Context, client *http.Client, accessToken string) error {
	_, err := VerifyAccessTokenIdentity(ctx, client, accessToken)
	return err
}

// VerifyAccessTokenIdentity is VerifyAccessToken that also returns the
// identity the same probe response carries.
func VerifyAccessTokenIdentity(ctx context.Context, client *http.Client, accessToken string) (TokenIdentity, error) {
	if client == nil {
		client = &http.Client{Timeout: 20 * time.Second}
	}
	req, err := newFableProbeRequest(ctx, accessToken)
	if err != nil {
		return TokenIdentity{}, err
	}
	res, err := client.Do(req)
	if err != nil {
		return TokenIdentity{}, err
	}
	defer res.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 64<<10))
	switch res.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden:
		return TokenIdentity{}, fmt.Errorf("%w: %s", ErrSetupTokenRejected, res.Status)
	}
	if res.StatusCode >= 500 {
		return TokenIdentity{}, fmt.Errorf("Claude setup token verification failed: %s", res.Status)
	}
	identity := TokenIdentity{
		OrganizationID: strings.TrimSpace(res.Header.Get("anthropic-organization-id")),
	}
	rawReset := strings.TrimSpace(res.Header.Get("anthropic-ratelimit-unified-7d-reset"))
	if epoch, err := strconv.ParseInt(rawReset, 10, 64); err == nil && epoch > 0 {
		identity.WeeklyResetAt = time.Unix(epoch, 0)
	}
	return identity, nil
}

func newFableProbeRequest(ctx context.Context, accessToken string) (*http.Request, error) {
	body := bytes.NewBufferString(`{"model":"` + FableModel + `","max_tokens":1,"system":[{"type":"text","text":"You are Claude Code, Anthropic's official CLI for Claude."}],"messages":[{"role":"user","content":"."}]}`)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, messagesURL, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("anthropic-beta", "claude-code-20250219,"+oauthBetaHeader)
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "claude-cli/2.1.199 (external, cli)")
	req.Header.Set("x-app", "cli")
	return req, nil
}
