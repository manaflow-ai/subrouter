package kimi

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/manaflow-ai/subrouter/account"
	"github.com/manaflow-ai/subrouter/internal/oauthdevice"
	"github.com/manaflow-ai/subrouter/internal/storepath"
)

// OAuth endpoints for the Kimi Code CLI's installed-app client, matching what
// the CLI itself uses. There is no client secret: the device-grant client is a
// public client.
const (
	oauthClientID  = "17e5f671-d194-4dfb-9706-5516cb48c098"
	oauthDeviceURL = "https://auth.kimi.com/api/oauth/device_authorization"
	oauthTokenURL  = "https://auth.kimi.com/api/oauth/token"
	// Kimi Code access tokens live 900 seconds. If a successful refresh omits
	// expires_in, retain that documented lifetime instead of persisting zero
	// and consuming the refresh token again on every request.
	refreshFallbackLifetime = 15 * time.Minute
	// Keep managed OAuth identities disjoint from API-key account IDs, which
	// already use the provider-derived "kimi:<label>" namespace.
	managedIDPrefix      = "kimi-subscription:"
	maxManagedLabelBytes = 160
)

// oauthConfig is a variable so tests can point the refresh at a stub server.
var oauthConfig = oauthdevice.Config{
	ClientID:      oauthClientID,
	DeviceAuthURL: oauthDeviceURL,
	TokenURL:      oauthTokenURL,
}

// accountID is the stable identifier of the one account the CLI credential
// file represents.
const accountID = "kimi-code"

// Store reads and refreshes the Kimi Code CLI credential file.
type Store struct {
	// Path is the credential file. Empty means the CLI's default location.
	Path string
	// KimiHome overrides the Kimi Code home used to resolve the CLI's sibling
	// device_id file. It is primarily useful for isolated imports and tests.
	KimiHome string
	// ManagedDir holds Subrouter-owned OAuth profiles. Empty uses the portable
	// Subrouter state directory unless Path points at an isolated test fixture.
	ManagedDir string
	// RefreshTransaction serializes a refresh write with account add/remove
	// transactions. The proxy supplies its cross-process account-state lock.
	RefreshTransaction func(context.Context, func() error) error
	// managedOnly keeps the interactive Kimi CLI credential outside a serving
	// account pool. Its rotating refresh-token chain belongs to the CLI; a
	// daemon that redeems it can silently sign the interactive client out.
	managedOnly bool
	// lockLocalCLIRefreshForTest injects failures at the official CLI lock
	// boundary without weakening the production filesystem lock.
	lockLocalCLIRefreshForTest func(context.Context) (*cliRefreshLock, error)
}

var deviceIDMu sync.Mutex

func (s Store) oauthConfig() (oauthdevice.Config, error) {
	deviceID, err := s.deviceID()
	if err != nil {
		return oauthdevice.Config{}, fmt.Errorf("load Kimi OAuth device identity: %w", err)
	}
	return oauthConfigForDevice(deviceID), nil
}

func oauthConfigForDevice(deviceID string) oauthdevice.Config {
	return oauthConfigForDevicePlatform(deviceID, "kimi_cli")
}

func oauthConfigForDevicePlatform(deviceID, platform string) oauthdevice.Config {
	hostname, _ := os.Hostname()
	cfg := oauthConfig
	cfg.Headers = map[string]string{
		// This public client ID is registered for the Kimi CLI platform. Keep
		// the platform identifier aligned with that public-client contract while
		// identifying this implementation in the version field.
		"X-Msh-Platform":     platform,
		"X-Msh-Version":      "subrouter",
		"X-Msh-Device-Name":  asciiHeaderValue(hostname, "subrouter-host"),
		"X-Msh-Device-Model": runtime.GOOS + " " + runtime.GOARCH,
		"X-Msh-Os-Version":   runtime.GOOS,
		"X-Msh-Device-Id":    deviceID,
	}
	return cfg
}

func asciiHeaderValue(value, fallback string) string {
	var out strings.Builder
	for _, r := range value {
		if r >= 0x20 && r <= 0x7e {
			out.WriteRune(r)
		}
	}
	if value := strings.TrimSpace(out.String()); value != "" {
		return value
	}
	return fallback
}

func (s Store) deviceID() (string, error) {
	deviceIDMu.Lock()
	defer deviceIDMu.Unlock()
	dir := s.managedDir()
	if dir == "" {
		if s.Path != "" {
			dir = filepath.Dir(s.Path)
		} else {
			dir = filepath.Join(storepath.StateDir(), "kimi")
		}
	}
	path := filepath.Join(dir, "device-id")
	if body, err := os.ReadFile(path); err == nil {
		if raw := strings.TrimSpace(string(body)); raw != "" && len(raw) <= 128 {
			if value := asciiHeaderValue(raw, ""); value != "" {
				return value, nil
			}
		}
		return "", fmt.Errorf("%s is empty or invalid", path)
	} else if !os.IsNotExist(err) {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	random := make([]byte, 16)
	if _, err := rand.Read(random); err != nil {
		return "", err
	}
	value := hex.EncodeToString(random)
	tmp, err := os.CreateTemp(dir, ".device-id-*")
	if err != nil {
		return "", err
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return "", err
	}
	if _, err := tmp.WriteString(value); err != nil {
		_ = tmp.Close()
		return "", err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return "", err
	}
	return value, nil
}

func DefaultStore() Store {
	return Store{}
}

// ServingStore returns the durable Subrouter-owned account source used by the
// proxy and status surfaces. The official CLI's one global login is deliberately
// excluded; users enroll an independent routable profile with `sr kimi login`.
func ServingStore() Store {
	return DefaultStore().ForServing()
}

// ForServing preserves custom paths while applying the managed-only serving
// boundary. It is primarily useful for tenant stores and regression tests.
func (s Store) ForServing() Store {
	s.managedOnly = true
	return s
}

func (s Store) cliHome() (string, error) {
	if home := strings.TrimSpace(s.KimiHome); home != "" {
		return filepath.Clean(home), nil
	}
	if s.Path != "" {
		credentialDir := filepath.Dir(s.Path)
		if filepath.Base(s.Path) == "kimi-code.json" && filepath.Base(credentialDir) == "credentials" {
			return filepath.Dir(credentialDir), nil
		}
		return "", fmt.Errorf("cannot derive Kimi Code home from custom credential path; set KimiHome")
	}
	if home := strings.TrimSpace(os.Getenv("KIMI_CODE_HOME")); home != "" {
		return filepath.Clean(home), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".kimi-code"), nil
}

func (s Store) credentialPath() string {
	if s.Path != "" {
		return s.Path
	}
	home, err := s.cliHome()
	if err != nil {
		return ""
	}
	return filepath.Join(home, "credentials", "kimi-code.json")
}

// localCLIDeviceID reads the identity the official Kimi Code client stores
// beside its credentials. It never creates a replacement: an OAuth refresh
// must use the same identity as the original authorization.
func (s Store) localCLIDeviceID() (string, error) {
	home, err := s.cliHome()
	if err != nil {
		return "", fmt.Errorf("resolve Kimi Code device identity: %w; run kimi login again", err)
	}
	path := filepath.Join(home, "device_id")
	info, err := os.Lstat(path)
	if err != nil {
		return "", fmt.Errorf("read Kimi Code device identity: %w; run kimi login again", err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("Kimi Code device identity is not a regular file; run kimi login again")
	}
	file, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("read Kimi Code device identity: %w; run kimi login again", err)
	}
	defer func() { _ = file.Close() }()
	body, err := io.ReadAll(io.LimitReader(file, 130))
	if err != nil {
		return "", fmt.Errorf("read Kimi Code device identity: %w; run kimi login again", err)
	}
	value := strings.TrimSpace(string(body))
	if len(body) > 129 || ValidateOAuthDeviceID(value) != nil {
		return "", fmt.Errorf("Kimi Code device identity is empty or invalid; run kimi login again")
	}
	return value, nil
}

func (s Store) managedDir() string {
	if s.ManagedDir != "" {
		return s.ManagedDir
	}
	if s.Path != "" {
		return ""
	}
	return filepath.Join(storepath.StateDir(), "kimi")
}

// ManagedAccountID validates a user-facing profile label and returns the
// stable account identifier used by routing and the persistent session store.
func ManagedAccountID(label string) (string, error) {
	label = strings.TrimSpace(label)
	if label == "" {
		return "", fmt.Errorf("Kimi account label is required")
	}
	if len(label) > maxManagedLabelBytes {
		return "", fmt.Errorf("Kimi account label must be at most %d bytes", maxManagedLabelBytes)
	}
	if strings.HasPrefix(strings.ToLower(label), managedIDPrefix) {
		return "", fmt.Errorf("Kimi account label must not start with reserved prefix %q", managedIDPrefix)
	}
	for _, r := range label {
		if unicode.IsControl(r) || unicode.In(r, unicode.Cf, unicode.Zl, unicode.Zp) {
			return "", fmt.Errorf("Kimi account label contains a control character")
		}
	}
	return managedIDPrefix + strings.ToLower(label), nil
}

func managedFilename(accountID string) (string, error) {
	if !strings.HasPrefix(accountID, managedIDPrefix) {
		return "", fmt.Errorf("%q is not a managed Kimi account", accountID)
	}
	label := strings.TrimSpace(strings.TrimPrefix(accountID, managedIDPrefix))
	if _, err := ManagedAccountID(label); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString([]byte(label)) + ".json", nil
}

func managedAccountID(filename string) (string, bool) {
	if filepath.Ext(filename) != ".json" {
		return "", false
	}
	encoded := strings.TrimSuffix(filename, ".json")
	decoded, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return "", false
	}
	id, err := ManagedAccountID(string(decoded))
	return id, err == nil
}

// ReadLocalCredential returns the credential the Kimi Code CLI currently holds
// on this machine. It reports ok=false rather than an error when the CLI is
// simply not signed in, so callers can distinguish "nothing to import" from
// "the stored credential is broken".
func (s Store) ReadLocalCredential(now time.Time) (credential CredentialInfo, ok bool, err error) {
	path := s.credentialPath()
	if path == "" {
		return CredentialInfo{}, false, nil
	}
	return readCredential(path, now)
}

func readCredential(path string, now time.Time) (credential CredentialInfo, ok bool, err error) {
	body, readErr := os.ReadFile(path)
	if readErr != nil {
		if os.IsNotExist(readErr) {
			return CredentialInfo{}, false, nil
		}
		return CredentialInfo{}, false, readErr
	}
	issuedAt := now
	if info, statErr := os.Stat(path); statErr == nil {
		issuedAt = info.ModTime()
	}
	credential, err = ParseCredential(body, path, issuedAt)
	if err != nil {
		return CredentialInfo{}, false, err
	}
	return credential, true, nil
}

// Provider implements the proxy's OAuth account source.
func (s Store) Provider() account.Provider {
	return account.ProviderKimi
}

// ListAccounts surfaces managed profiles and, for account-manager callers,
// the CLI credential. Serving stores never inspect or return that singleton.
// An unreadable included credential is an error: silently dropping it would
// look identical to being signed out.
func (s Store) ListAccounts(_ context.Context) ([]account.Account, error) {
	result := make([]account.Account, 0, 1)
	var listErrors []error
	if !s.managedOnly {
		credential, ok, err := s.ReadLocalCredential(time.Now())
		if err != nil {
			listErrors = append(listErrors, fmt.Errorf("read Kimi CLI credential: %w", err))
		} else if ok {
			result = append(result, credentialAccount(accountID, "Kimi Code", "kimi-code credentials file", credential))
		}
	}
	dir := s.managedDir()
	if dir == "" {
		return result, errors.Join(listErrors...)
	}
	entries, readErr := os.ReadDir(dir)
	if readErr != nil {
		if os.IsNotExist(readErr) {
			return result, errors.Join(listErrors...)
		}
		listErrors = append(listErrors, readErr)
		return result, errors.Join(listErrors...)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	for _, entry := range entries {
		managedID, valid := managedAccountID(entry.Name())
		if entry.IsDir() || !valid {
			continue
		}
		canonicalName, err := managedFilename(managedID)
		if err != nil || entry.Name() != canonicalName {
			listErrors = append(listErrors, fmt.Errorf("ignore noncanonical managed Kimi credential for %s", managedID))
			continue
		}
		path := filepath.Join(dir, entry.Name())
		managedCredential, present, readErr := readCredential(path, time.Now())
		if readErr != nil {
			listErrors = append(listErrors, fmt.Errorf("read managed Kimi account %s: %w", managedID, readErr))
			continue
		}
		if !present {
			continue
		}
		label := strings.TrimSpace(managedCredential.AccountLabel)
		if label == "" {
			label = strings.TrimPrefix(managedID, managedIDPrefix)
		}
		result = append(result, credentialAccount(managedID, label, "subrouter managed Kimi credential", managedCredential))
	}
	return result, errors.Join(listErrors...)
}

// AccountInventoryIDs returns durable Kimi credential identifiers without
// parsing their contents. Import collision checks must see malformed files and
// dangling symlinks that ListAccounts cannot route.
func (s Store) AccountInventoryIDs(_ context.Context) ([]string, error) {
	var ids []string
	if !s.managedOnly {
		if path := s.credentialPath(); path != "" {
			if _, err := os.Lstat(path); err == nil {
				ids = append(ids, accountID)
			} else if !os.IsNotExist(err) {
				return nil, err
			}
		}
	}
	dir := s.managedDir()
	if dir == "" {
		return ids, nil
	}
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return ids, nil
	}
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if id, valid := managedAccountID(entry.Name()); valid {
			ids = append(ids, id)
		}
	}
	return ids, nil
}

// AccountInventoryCount counts durable Kimi credential entries without
// parsing them. Capacity checks must include malformed credentials too: they
// still occupy managed state and must remain repairable in place.
func (s Store) AccountInventoryCount(ctx context.Context) (int, error) {
	ids, err := s.AccountInventoryIDs(ctx)
	return len(ids), err
}

// refreshMu serializes refreshes process-wide. Whether Kimi's refresh token is
// single-use is unproven, but Claude's is, and a concurrent double-redemption
// of a single-use token invalidates the credential for both the proxy and the
// CLI — so refresh takes the lock rather than finding out.
var refreshMu sync.Mutex

// RefreshAccount refreshes the credential when its access token is near expiry
// and writes the result back to the CLI's credential file. Writing back is
// what keeps the CLI itself signed in: if Kimi rotates the refresh token on
// use, a proxy that keeps the new token to itself would log the CLI out.
func (s Store) RefreshAccount(ctx context.Context, client *http.Client, acct account.Account) (account.Account, error) {
	refreshed, _, err := s.RefreshAccountIfNeeded(ctx, client, acct)
	return refreshed, err
}

// AccountRefreshState re-reads acct and reports whether refreshing it could
// mutate its credential file. Returning the re-read account keeps a status
// fast path from using a token another process already rotated.
func (s Store) AccountRefreshState(acct account.Account, now time.Time) (account.Account, bool, error) {
	path, label, source, err := s.accountCredentialPath(acct.ID)
	if err != nil {
		return acct, false, err
	}
	credential, ok, err := readCredential(path, now)
	if err != nil {
		return acct, false, err
	}
	if !ok {
		return acct, false, fmt.Errorf("Kimi credential for %s was not found", acct.ID)
	}
	if storedLabel := strings.TrimSpace(credential.AccountLabel); storedLabel != "" {
		label = storedLabel
	}
	current := credentialAccount(acct.ID, label, source, credential)
	return current, credential.NeedsRefresh(now), nil
}

// RefreshAccountIfNeeded is RefreshAccount plus whether it committed a new
// credential, allowing local CLI callers to publish the account generation
// only when disk state actually changed.
func (s Store) RefreshAccountIfNeeded(ctx context.Context, client *http.Client, acct account.Account) (refreshed account.Account, didRefresh bool, err error) {
	refreshMu.Lock()
	defer refreshMu.Unlock()
	refresh := func() (refreshErr error) {
		path, label, source, pathErr := s.accountCredentialPath(acct.ID)
		if pathErr != nil {
			return pathErr
		}
		credential, ok, readErr := readCredential(path, time.Now())
		if readErr != nil {
			return readErr
		}
		if !ok {
			return fmt.Errorf("Kimi credential for %s was not found", acct.ID)
		}
		if storedLabel := strings.TrimSpace(credential.AccountLabel); storedLabel != "" {
			label = storedLabel
		}
		if !credential.NeedsRefresh(time.Now()) {
			refreshed = credentialAccount(acct.ID, label, source, credential)
			return nil
		}
		var cliLock *cliRefreshLock
		if acct.ID == accountID {
			var lockErr error
			if s.lockLocalCLIRefreshForTest != nil {
				cliLock, lockErr = s.lockLocalCLIRefreshForTest(ctx)
			} else {
				cliLock, lockErr = s.lockLocalCLIRefresh(ctx)
			}
			if lockErr != nil {
				return lockErr
			}
			defer func() {
				if releaseErr := cliLock.Release(); releaseErr != nil && refreshErr == nil {
					refreshErr = releaseErr
				}
			}()
			// A peer CLI may have rotated and stored the token while this
			// process waited. Re-read under the shared lock and avoid spending
			// the replacement refresh token when it is already fresh.
			credential, ok, readErr = readCredential(path, time.Now())
			if readErr != nil {
				return readErr
			}
			if !ok {
				return fmt.Errorf("Kimi credential for %s was removed while waiting for its OAuth refresh lock", acct.ID)
			}
			if !credential.NeedsRefresh(time.Now()) {
				refreshed = credentialAccount(acct.ID, label, source, credential)
				return nil
			}
			if lockErr = cliLock.Check(); lockErr != nil {
				return lockErr
			}
		}
		var cfg oauthdevice.Config
		if deviceID := strings.TrimSpace(credential.OAuthDeviceID); deviceID != "" {
			if validateErr := ValidateOAuthDeviceID(deviceID); validateErr != nil {
				return fmt.Errorf("Kimi credential for %s has an invalid OAuth device identity", acct.ID)
			}
			cfg = oauthConfigForDevice(deviceID)
		} else if acct.ID == accountID {
			// The official Kimi Code credential schema stores its stable device
			// identity in <KIMI_CODE_HOME>/device_id, not in the token JSON.
			// Reuse it exactly and never generate or borrow a managed identity.
			deviceID, deviceErr := s.localCLIDeviceID()
			if deviceErr != nil {
				return deviceErr
			}
			cfg = oauthConfigForDevicePlatform(deviceID, "kimi_code_cli")
		} else {
			return fmt.Errorf("Kimi managed credential for %s is missing its OAuth device identity; sign in again", acct.ID)
		}
		refreshCtx := ctx
		if cliLock != nil {
			refreshCtx = cliLock.Context()
		}
		refreshTime := time.Now()
		token, tokenErr := oauthdevice.Refresh(refreshCtx, client, cfg, credential.RefreshToken, refreshTime)
		if tokenErr != nil {
			if cliLock != nil {
				if lockErr := cliLock.Check(); lockErr != nil {
					return lockErr
				}
			}
			return tokenErr
		}
		credential.AccessToken = token.AccessToken
		if strings.TrimSpace(token.RefreshToken) != "" {
			credential.RefreshToken = token.RefreshToken
		}
		if strings.TrimSpace(token.TokenType) != "" {
			credential.TokenType = token.TokenType
		}
		if strings.TrimSpace(token.Scope) != "" {
			credential.Scope = token.Scope
		}
		credential.ExpiresAt = refreshedExpiry(credential.ExpiresAt, token.ExpiresAt, refreshTime)
		if cliLock != nil {
			if lockErr := cliLock.Check(); lockErr != nil {
				return lockErr
			}
		}
		refreshed = credentialAccount(acct.ID, label, source, credential)
		if writeErr := writeCredential(path, credential); writeErr != nil {
			if isPostRenameSyncError(writeErr) {
				didRefresh = true
			}
			return fmt.Errorf("refreshed the Kimi credential but could not write it back: %w", writeErr)
		}
		didRefresh = true
		return nil
	}
	if s.RefreshTransaction != nil {
		err = s.RefreshTransaction(ctx, refresh)
	} else {
		err = refresh()
	}
	if err != nil {
		// The CLI lock is released after writeCredential commits. A release
		// failure must remain visible because lock ownership is uncertain, but
		// it cannot turn a durable token rotation back into the stale input
		// account or claim that no mutation occurred.
		if didRefresh {
			return refreshed, true, err
		}
		return acct, false, err
	}
	return refreshed, didRefresh, nil
}

func refreshedExpiry(previous, returned, now time.Time) time.Time {
	if !returned.IsZero() {
		return returned
	}
	fallback := now.Add(refreshFallbackLifetime).UTC()
	if previous.After(fallback) {
		return previous
	}
	return fallback
}

func (s Store) accountCredentialPath(id string) (path, label, source string, err error) {
	id = strings.TrimSpace(id)
	if id == "" || id == accountID {
		if s.managedOnly {
			return "", "", "", fmt.Errorf("Kimi CLI credential is not routable; add an isolated profile with sr kimi login <label>")
		}
		return s.credentialPath(), "Kimi Code", "kimi-code credentials file", nil
	}
	filename, err := managedFilename(id)
	if err != nil {
		return "", "", "", err
	}
	if s.managedDir() == "" {
		return "", "", "", fmt.Errorf("managed Kimi account storage is disabled")
	}
	return filepath.Join(s.managedDir(), filename), strings.TrimPrefix(id, managedIDPrefix), "subrouter managed Kimi credential", nil
}

func credentialAccount(id, label, source string, credential CredentialInfo) account.Account {
	return account.Account{
		ID:       id,
		Provider: account.ProviderKimi,
		AuthMode: account.AuthModeOAuth,
		Label:    label,
		Token:    credential.AccessToken,
		Source:   source,
	}
}

func writeCredential(path string, credential CredentialInfo) error {
	payload := map[string]any{
		"access_token":  credential.AccessToken,
		"refresh_token": credential.RefreshToken,
		"token_type":    credential.TokenType,
		"scope":         credential.Scope,
	}
	if strings.TrimSpace(credential.AccountLabel) != "" {
		payload["account_label"] = strings.TrimSpace(credential.AccountLabel)
	}
	if strings.TrimSpace(credential.OAuthDeviceID) != "" {
		payload["oauth_device_id"] = strings.TrimSpace(credential.OAuthDeviceID)
	}
	if !credential.ExpiresAt.IsZero() {
		payload["expires_at"] = credential.ExpiresAt.Unix()
		payload["expires_in"] = int64(time.Until(credential.ExpiresAt).Seconds())
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".kimi-code.json.*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(body); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return err
	}
	if err := syncCredentialDirectory(filepath.Dir(path)); err != nil {
		return postRenameSyncError{err: fmt.Errorf("sync Kimi credential directory after rename: %w", err)}
	}
	return nil
}

type postRenameSyncError struct{ err error }

func (e postRenameSyncError) Error() string { return e.err.Error() }
func (e postRenameSyncError) Unwrap() error { return e.err }

func isPostRenameSyncError(err error) bool {
	var target postRenameSyncError
	return errors.As(err, &target)
}

var syncCredentialDirectory = func(path string) error {
	// Windows does not support syncing an opened directory through os.File.
	// Rename remains atomic there; Unix platforms persist the directory entry.
	if runtime.GOOS == "windows" {
		return nil
	}
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	if err := dir.Sync(); err != nil {
		_ = dir.Close()
		return err
	}
	return dir.Close()
}

// SaveManagedCredential persists one Subrouter-owned OAuth profile without
// touching the Kimi CLI's single global credential.
func (s Store) SaveManagedCredential(label string, credential CredentialInfo) (account.Account, error) {
	displayLabel := strings.TrimSpace(label)
	id, err := ManagedAccountID(label)
	if err != nil {
		return account.Account{}, err
	}
	path, _, source, err := s.accountCredentialPath(id)
	if err != nil {
		return account.Account{}, err
	}
	credential.AccountLabel = displayLabel
	if err := writeCredential(path, credential); err != nil {
		return account.Account{}, err
	}
	return credentialAccount(id, displayLabel, source, credential), nil
}

// ReadManagedCredential exports one isolated profile for an authenticated
// server upload. Callers must keep the returned value out of logs and remove
// any staging directory after the upload completes.
func (s Store) ReadManagedCredential(label string, now time.Time) (CredentialInfo, bool, error) {
	id, err := ManagedAccountID(label)
	if err != nil {
		return CredentialInfo{}, false, err
	}
	path, _, _, err := s.accountCredentialPath(id)
	if err != nil {
		return CredentialInfo{}, false, err
	}
	return readCredential(path, now)
}

// ManagedAccountExists reports whether an isolated profile is present without
// parsing its credential. Removal must remain possible for malformed profiles,
// but callers still need a read-only existence check before publishing a disk
// generation for that removal.
func (s Store) ManagedAccountExists(label string) (bool, error) {
	id, err := ManagedAccountID(label)
	if err != nil {
		return false, err
	}
	path, _, _, err := s.accountCredentialPath(id)
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

// RemoveManagedAccount removes one Subrouter-owned profile by its user-facing
// label. The Kimi CLI credential is deliberately outside this command's
// ownership.
func (s Store) RemoveManagedAccount(label string) (account.Account, bool, error) {
	id, err := ManagedAccountID(label)
	if err != nil {
		return account.Account{}, false, err
	}
	return s.removeManagedAccountID(id)
}

// RemoveManagedAccountID removes one Subrouter-owned profile by the canonical
// routing identifier returned by ListAccounts.
func (s Store) RemoveManagedAccountID(id string) (account.Account, bool, error) {
	id = strings.TrimSpace(id)
	if !strings.HasPrefix(id, managedIDPrefix) {
		return account.Account{}, false, fmt.Errorf("%q is not a managed Kimi account", id)
	}
	canonicalID, err := ManagedAccountID(strings.TrimPrefix(id, managedIDPrefix))
	if err != nil {
		return account.Account{}, false, err
	}
	return s.removeManagedAccountID(canonicalID)
}

func (s Store) removeManagedAccountID(id string) (account.Account, bool, error) {
	path, label, source, err := s.accountCredentialPath(id)
	if err != nil {
		return account.Account{}, false, err
	}
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return account.Account{}, false, nil
	}
	if err != nil {
		return account.Account{}, false, err
	}
	if info.IsDir() {
		return account.Account{}, false, nil
	}
	credential, _, readErr := readCredential(path, time.Now())
	if removeErr := os.Remove(path); removeErr != nil {
		if os.IsNotExist(removeErr) {
			return account.Account{}, false, nil
		}
		return account.Account{}, false, removeErr
	}
	// A malformed credential must remain removable through the supported CLI;
	// parsing only enriches the confirmation row and is not authority to keep a
	// broken profile installed forever.
	if readErr == nil {
		if storedLabel := strings.TrimSpace(credential.AccountLabel); storedLabel != "" {
			label = storedLabel
		}
	} else {
		credential = CredentialInfo{}
	}
	return credentialAccount(id, label, source, credential), true, nil
}
