// Package qwensubscription owns isolated Qwen.ai Plus/Pro OAuth grants.
// It never reads or refreshes the Qwen CLI's interactive credential.
package qwensubscription

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/manaflow-ai/subrouter/account"
	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/internal/fsutil"
	"github.com/manaflow-ai/subrouter/internal/oauthdevice"
	"github.com/manaflow-ai/subrouter/internal/storepath"
)

const DefaultUpstream = "https://api.qwen.ai/compatible-mode/v1"
const idPrefix = "qwen-subscription:"

// The paid subscription client is distinct from Qwen's legacy PKCE client.
// Source: QwenLM/qwen-code packages/core/src/qwen/subscription-oauth-client.ts.
var oauthConfig = oauthdevice.Config{
	ClientID:      "qwen-code-cli",
	DeviceAuthURL: "https://auth.qwen.ai/api/oauth/device/code",
	TokenURL:      "https://auth.qwen.ai/api/oauth/token",
	Scope:         "model:invoke profile:read",
	// Qwen challenges generic Go/Python user agents with HTML rather than OAuth JSON.
	Headers: map[string]string{"User-Agent": "subrouter"},
}
var userInfoURL = "https://auth.qwen.ai/api/oauth/userinfo"

type CredentialInfo struct {
	AccessToken         string    `json:"access_token"`
	RefreshToken        string    `json:"refresh_token"`
	ExpiresAt           time.Time `json:"expires_at"`
	Label               string    `json:"label"`
	Subject             string    `json:"subject"`
	Email               string    `json:"email"`
	MemberType          string    `json:"member_type"`
	VerificationPending bool      `json:"verification_pending,omitempty"`
}

type Store struct {
	Dir string
	// The serving proxy supplies its cross-process account mutation lock.
	RefreshTransaction func(context.Context, func() error) error
}

func (s Store) dir() string {
	if s.Dir != "" {
		return s.Dir
	}
	return filepath.Join(storepath.StateDir(), "qwen-subscription")
}

func (s Store) Provider() account.Provider { return account.ProviderQwenOAuth }

func ManagedAccountID(label string) (string, error) {
	label = strings.TrimSpace(label)
	if label == "" || len(label) > 160 || strings.HasPrefix(strings.ToLower(label), idPrefix) {
		return "", errors.New("Qwen subscription label must contain 1–160 bytes and must not start with qwen-subscription:")
	}
	for _, r := range label {
		if unicode.IsControl(r) || unicode.In(r, unicode.Cf, unicode.Zl, unicode.Zp) {
			return "", errors.New("Qwen subscription label contains a control character")
		}
	}
	return idPrefix + strings.ToLower(label), nil
}

func (s Store) path(label string) (string, error) {
	id, err := ManagedAccountID(label)
	if err != nil {
		return "", err
	}
	return filepath.Join(s.dir(), base64.RawURLEncoding.EncodeToString([]byte(strings.TrimPrefix(id, idPrefix)))+".json"), nil
}

func (s Store) read(label string) (CredentialInfo, bool, error) {
	path, err := s.path(label)
	if err != nil {
		return CredentialInfo{}, false, err
	}
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return CredentialInfo{}, false, nil
	}
	if err != nil {
		return CredentialInfo{}, false, err
	}
	if !info.Mode().IsRegular() || info.Size() > 128<<10 {
		return CredentialInfo{}, false, errors.New("unreadable credential: invalid Qwen subscription file")
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return CredentialInfo{}, false, err
	}
	var credential CredentialInfo
	if json.Unmarshal(body, &credential) != nil || credential.AccessToken == "" || credential.RefreshToken == "" || credential.ExpiresAt.IsZero() || credential.Subject == "" {
		return CredentialInfo{}, false, errors.New("unreadable credential: incomplete Qwen subscription credential")
	}
	return credential, true, nil
}

func (s Store) labels() ([]string, error) {
	entries, err := os.ReadDir(s.dir())
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var labels []string
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		label, err := base64.RawURLEncoding.DecodeString(strings.TrimSuffix(entry.Name(), ".json"))
		if err != nil {
			return nil, errors.New("invalid Qwen subscription filename")
		}
		if _, err := ManagedAccountID(string(label)); err != nil {
			return nil, err
		}
		labels = append(labels, string(label))
	}
	return labels, nil
}

func (s Store) AccountInventoryIDs(context.Context) ([]string, error) {
	labels, err := s.labels()
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(labels))
	for _, label := range labels {
		id, _ := ManagedAccountID(label)
		ids = append(ids, id)
	}
	return ids, nil
}

func (s Store) ListAccounts(context.Context) ([]account.Account, error) {
	labels, err := s.labels()
	if err != nil {
		return nil, err
	}
	var result []account.Account
	for _, label := range labels {
		credential, ok, err := s.read(label)
		if err != nil {
			return nil, err
		}
		if ok {
			result = append(result, credentialAccount(label, credential))
		}
	}
	return result, nil
}

func credentialAccount(label string, credential CredentialInfo) account.Account {
	id, _ := ManagedAccountID(label)
	token := credential.AccessToken
	// Pending grants remain in inventory for recovery, but no consumer receives
	// their bearer token until the rotated identity is verified.
	if credential.VerificationPending {
		token = ""
	}
	return account.Account{
		ID:                id,
		Provider:          account.ProviderQwenOAuth,
		AuthMode:          account.AuthModeOAuth,
		Label:             credential.Label,
		Email:             credential.Email,
		AccountID:         credential.Subject,
		Token:             token,
		CredentialVersion: account.OAuthCredentialVersion(credential.AccessToken, credential.RefreshToken),
		Source:            "subrouter managed Qwen subscription credential",
	}
}

func (s Store) write(label string, credential CredentialInfo) (committed bool, err error) {
	path, err := s.path(label)
	if err != nil {
		return false, err
	}
	if err := os.MkdirAll(s.dir(), 0o700); err != nil {
		return false, err
	}
	body, err := json.Marshal(credential)
	if err != nil {
		return false, err
	}
	if err := fsutil.ReplaceFile(path, body, 0o600); err != nil {
		return false, err
	}
	return true, fsutil.SyncDir(s.dir())
}

// CheckImportCredential must run under the account mutation lock, before
// redeeming a grant. One membership is one quota pool even across device grants.
func (s Store) CheckImportCredential(label string, credential CredentialInfo) error {
	if _, err := ManagedAccountID(label); err != nil {
		return err
	}
	if strings.TrimSpace(credential.AccessToken) == "" || strings.TrimSpace(credential.RefreshToken) == "" || !credential.ExpiresAt.After(time.Now()) || credential.Subject == "" {
		return errors.New("Qwen subscription credential is incomplete or expired")
	}
	labels, err := s.labels()
	if err != nil {
		return err
	}
	for _, existing := range labels {
		if strings.EqualFold(existing, strings.TrimSpace(label)) {
			continue
		}
		other, ok, err := s.read(existing)
		if err != nil {
			return err
		}
		if ok && (other.RefreshToken == credential.RefreshToken || other.Subject == credential.Subject) {
			return errors.New("Qwen subscription membership is already stored under another label")
		}
	}
	return nil
}

func (s Store) SaveManagedCredential(label string, credential CredentialInfo) (account.Account, error) {
	if err := s.CheckImportCredential(label, credential); err != nil {
		return account.Account{}, err
	}
	credential.Label = strings.TrimSpace(label)
	_, err := s.write(label, credential)
	return credentialAccount(label, credential), err
}

func (s Store) ManagedAccountExists(label string) (bool, error) {
	path, err := s.path(label)
	if err != nil {
		return false, err
	}
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return !info.IsDir(), nil
}

func (s Store) RemoveManagedAccount(label string) (account.Account, bool, error) {
	path, err := s.path(label)
	if err != nil {
		return account.Account{}, false, err
	}
	exists, err := s.ManagedAccountExists(label)
	if err != nil || !exists {
		return account.Account{}, false, err
	}
	id, _ := ManagedAccountID(label)
	if err := os.Remove(path); err != nil {
		return account.Account{}, false, err
	}
	return account.Account{ID: id, Provider: account.ProviderQwenOAuth}, true, fsutil.SyncDir(s.dir())
}

func (s Store) AuthorizeManaged(ctx context.Context, client *http.Client, label string, out io.Writer) (CredentialInfo, error) {
	if _, err := ManagedAccountID(label); err != nil {
		return CredentialInfo{}, err
	}
	code, err := oauthdevice.RequestCode(ctx, client, oauthConfig, time.Now())
	if err != nil {
		return CredentialInfo{}, err
	}
	// Qwen's own CLI uses /authorize; the OAuth response's /device URL does
	// not open the subscription consent flow in the web app.
	authorizationURL := "https://chat.qwen.ai/authorize?" + url.Values{
		"user_code": {code.UserCode}, "client": {"cli"},
		"entry": {"qwen_code"}, "paywallScene": {"cli"},
	}.Encode()
	fmt.Fprintf(out, "Open %s to authorize Qwen Plus/Pro for %q (code %s).\n", authorizationURL, label, code.UserCode)
	token, err := oauthdevice.Poll(ctx, client, oauthConfig, code, nil, nil)
	if err != nil {
		return CredentialInfo{}, err
	}
	credential := CredentialInfo{AccessToken: token.AccessToken, RefreshToken: token.RefreshToken, ExpiresAt: token.ExpiresAt, Label: strings.TrimSpace(label)}
	return AttestCredential(ctx, client, credential)
}

// AttestCredential binds an imported grant to Qwen's authenticated userinfo,
// rather than trusting identity or membership fields supplied by the client.
func AttestCredential(ctx context.Context, client *http.Client, credential CredentialInfo) (CredentialInfo, error) {
	if credential.AccessToken == "" || credential.RefreshToken == "" || !credential.ExpiresAt.After(time.Now()) {
		return CredentialInfo{}, errors.New("Qwen subscription credential is incomplete or expired")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, userInfoURL, nil)
	if err != nil {
		return CredentialInfo{}, err
	}
	req.Header.Set("Authorization", "Bearer "+credential.AccessToken)
	req.Header.Set("User-Agent", "subrouter")
	if client == nil {
		client = http.DefaultClient
	}
	checkedClient := *client
	checkedClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	res, err := checkedClient.Do(req)
	if err != nil {
		return CredentialInfo{}, errors.New("Qwen subscription userinfo request failed")
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return CredentialInfo{}, fmt.Errorf("Qwen subscription userinfo returned HTTP %d", res.StatusCode)
	}
	var profile struct {
		Subject    string `json:"sub"`
		Email      string `json:"email"`
		MemberType string `json:"member_type"`
	}
	if json.NewDecoder(io.LimitReader(res.Body, 128<<10)).Decode(&profile) != nil || profile.Subject == "" || profile.MemberType == "" {
		return CredentialInfo{}, errors.New("invalid Qwen subscription userinfo response")
	}
	if credential.Subject != "" && credential.Subject != profile.Subject {
		return CredentialInfo{}, errors.New("Qwen subscription identity changed")
	}
	credential.Subject, credential.Email, credential.MemberType = profile.Subject, profile.Email, profile.MemberType
	return credential, nil
}

var refreshMu sync.Mutex

// ImportVerifiedCredential takes refresh ownership after the server has
// attested the submitted access token's Subject. The caller holds the account
// mutation lock and publishes even a partially committed import.
func (s Store) ImportVerifiedCredential(ctx context.Context, client *http.Client, label string, credential CredentialInfo) (account.Account, error) {
	if err := s.CheckImportCredential(label, credential); err != nil {
		return account.Account{}, err
	}
	credential.Label = strings.TrimSpace(label)
	credential, err := refreshCredential(ctx, client, credential)
	if err != nil {
		return account.Account{}, err
	}
	credential, _, err = s.persistAndVerify(ctx, client, label, credential)
	return credentialAccount(label, credential), err
}

func refreshCredential(ctx context.Context, client *http.Client, credential CredentialInfo) (CredentialInfo, error) {
	token, err := oauthdevice.Refresh(ctx, client, oauthConfig, credential.RefreshToken, time.Now())
	if err != nil {
		return credential, err
	}
	credential.AccessToken, credential.RefreshToken = token.AccessToken, token.RefreshToken
	credential.ExpiresAt = token.ExpiresAt
	if credential.ExpiresAt.IsZero() {
		credential.ExpiresAt = time.Now().UTC()
	}
	credential.VerificationPending = true
	return credential, nil
}

func (s Store) persistAndVerify(ctx context.Context, client *http.Client, label string, credential CredentialInfo) (CredentialInfo, bool, error) {
	// Never put another network operation (or cancellation check) between a
	// successful token exchange and this durable write.
	committed, err := s.write(label, credential)
	if err != nil {
		return credential, committed, err
	}
	verified, err := AttestCredential(ctx, client, credential)
	if err != nil {
		return credential, true, err
	}
	verified.VerificationPending = false
	_, err = s.write(label, verified)
	return verified, true, err
}

func (s Store) AccountRefreshState(acct account.Account, now time.Time) (account.Account, bool, error) {
	if acct.Provider != s.Provider() || !strings.HasPrefix(acct.ID, idPrefix) {
		return acct, false, errors.New("not a managed Qwen subscription")
	}
	label := strings.TrimPrefix(acct.ID, idPrefix)
	credential, ok, err := s.read(label)
	if err != nil {
		return acct, false, err
	}
	if !ok {
		return acct, false, errors.New("Qwen subscription credential was removed")
	}
	return credentialAccount(label, credential), credential.VerificationPending || !credential.ExpiresAt.After(now.Add(30*time.Second)), nil
}

func (s Store) RefreshAccount(ctx context.Context, client *http.Client, acct account.Account) (account.Account, error) {
	current, _, err := s.RefreshAccountIfNeeded(ctx, client, acct)
	return current, err
}

func (s Store) RefreshAccountIfNeeded(ctx context.Context, client *http.Client, acct account.Account) (account.Account, bool, error) {
	return s.refreshAccount(ctx, client, acct, "")
}

// RefreshRejectedAccount repairs only the exact credential the upstream
// rejected. A concurrent rotation is reused rather than redeemed again.
func (s Store) RefreshRejectedAccount(ctx context.Context, client *http.Client, acct account.Account) (account.Account, error) {
	current, _, err := s.refreshAccount(ctx, client, acct, acct.CredentialIdentity())
	return current, err
}

func (s Store) refreshAccount(ctx context.Context, client *http.Client, acct account.Account, rejectedIdentity string) (current account.Account, committed bool, err error) {
	refreshMu.Lock()
	defer refreshMu.Unlock()
	current = acct
	refresh := func() error {
		fresh, needed, err := s.AccountRefreshState(acct, time.Now())
		if err != nil {
			return err
		}
		current = fresh
		force := rejectedIdentity != "" && fresh.CredentialIdentity() == rejectedIdentity
		if !needed && !force {
			return nil
		}
		label := strings.TrimPrefix(acct.ID, idPrefix)
		credential, _, err := s.read(label)
		if err != nil {
			return err
		}
		// A pending access token only needs verification while still fresh. If it
		// expired during an outage, preserve another rotation before retrying it.
		if !credential.VerificationPending || !credential.ExpiresAt.After(time.Now().Add(30*time.Second)) {
			credential, err = refreshCredential(ctx, client, credential)
			if err != nil {
				return err
			}
		}
		credential, committed, err = s.persistAndVerify(ctx, client, label, credential)
		current = credentialAccount(label, credential)
		return err
	}
	if s.RefreshTransaction != nil {
		err = s.RefreshTransaction(ctx, refresh)
	} else {
		err = refresh()
	}
	return current, committed, err
}

// Qwen exposes verified membership through userinfo, but no quota windows in
// its subscription client. Return no invented usage percentages.
func (s Store) FetchUsage(ctx context.Context, client *http.Client, acct account.Account) (string, []accounts.UsageWindow, error) {
	_, plan, windows, err := s.FetchUsageIdentity(ctx, client, acct)
	return plan, windows, err
}

func (s Store) FetchUsageIdentity(ctx context.Context, client *http.Client, acct account.Account) (string, string, []accounts.UsageWindow, error) {
	credential, ok, err := s.read(strings.TrimPrefix(acct.ID, idPrefix))
	if err != nil {
		return "", "", nil, err
	}
	if !ok {
		return "", "", nil, errors.New("Qwen subscription credential was removed")
	}
	if credential.VerificationPending {
		return "", "", nil, errors.New("Qwen subscription is awaiting identity verification")
	}
	credential.AccessToken = acct.Token
	verified, err := AttestCredential(ctx, client, credential)
	if err != nil {
		return "", "", nil, err
	}
	return verified.Email, verified.MemberType, nil, nil
}
