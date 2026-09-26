package antigravity

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"

	"github.com/manaflow-ai/subrouter/account"
	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/internal/storepath"
)

const (
	// keychainService and keychainAccount address the generic-password item the
	// Antigravity CLI writes on macOS. The service is "gemini" for historical
	// reasons: the CLI was Gemini CLI before it was renamed.
	keychainService = "gemini"
	keychainAccount = "antigravity"

	// keychainReadTimeout bounds the `security` call, which can block on a
	// locked keychain or an access-control prompt.
	keychainReadTimeout = 5 * time.Second
)

// ErrManagedIdentityExists is returned without credential material when an
// import would place one OAuth identity under multiple routing labels.
var ErrManagedIdentityExists = errors.New("this Antigravity OAuth identity already exists under another managed profile")

// oauthTokenURL is a variable so tests can point the refresh at a stub server.
var oauthTokenURL = "https://oauth2.googleapis.com/token"

// oauthClient is a public installed-app OAuth client the Antigravity CLI was
// built with. Refreshing a credential the CLI issued requires presenting the
// client it was issued to. The values are not committed to source: they are
// read from the installed CLI binary, or from the
// SUBROUTER_ANTIGRAVITY_CLIENT_ID / SUBROUTER_ANTIGRAVITY_CLIENT_SECRET
// environment variables when both are set.
type oauthClient struct {
	id     string
	secret string
}

// oauthClientsForRefresh is a variable so tests can stub the candidate list.
var oauthClientsForRefresh = defaultOAuthClients

// workingClient caches the candidate that last refreshed successfully, so a
// binary carrying several clients pays the trial cost once per process.
var workingClient atomic.Pointer[oauthClient]

func defaultOAuthClients() []oauthClient {
	if client, ok := oauthClientFromEnv(); ok {
		return []oauthClient{client}
	}
	return oauthClientsFromBinary(agyBinaryPath())
}

// oauthClientFromEnv reports the explicitly configured client, if any. A
// half-set pair is ignored rather than used, because presenting a client id
// with the wrong secret is indistinguishable from a dead account upstream.
func oauthClientFromEnv() (oauthClient, bool) {
	client := oauthClient{
		id:     strings.TrimSpace(os.Getenv("SUBROUTER_ANTIGRAVITY_CLIENT_ID")),
		secret: strings.TrimSpace(os.Getenv("SUBROUTER_ANTIGRAVITY_CLIENT_SECRET")),
	}
	return client, client.id != "" && client.secret != ""
}

// agyBinaryPath locates the installed Antigravity CLI. PATH first, then the
// install location its installer uses, because launchd runs this daemon with a
// sparse PATH.
func agyBinaryPath() string {
	if path, err := exec.LookPath("agy"); err == nil {
		return path
	}
	if home, err := os.UserHomeDir(); err == nil {
		candidate := filepath.Join(home, ".local", "bin", "agy")
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate
		}
	}
	return ""
}

var (
	clientIDPattern = regexp.MustCompile(`[0-9]+-[a-z0-9]+\.apps\.googleusercontent\.com`)
	// Google client secrets are GOCSPX- plus exactly 28 characters. The length
	// must be pinned: the binary packs its string constants with no
	// separators, so an unbounded match would swallow whatever string the
	// linker placed next.
	clientSecretPattern = regexp.MustCompile(`GOCSPX-[A-Za-z0-9_-]{28}`)
)

// oauthClientsFromBinary scans the CLI binary for the installed-app clients it
// carries. The binary packs its string constants with no separators and no
// recorded pairing between client ids and secrets — today's build holds two of
// each — so this returns the cross product and lets the refresh path discover
// the working pair by trying them.
func oauthClientsFromBinary(path string) []oauthClient {
	if path == "" {
		return nil
	}
	binary, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	ids := uniqueMatches(clientIDPattern, binary)
	secrets := uniqueMatches(clientSecretPattern, binary)
	clients := make([]oauthClient, 0, len(ids)*len(secrets))
	for _, id := range ids {
		for _, secret := range secrets {
			clients = append(clients, oauthClient{id: id, secret: secret})
		}
	}
	return clients
}

func uniqueMatches(pattern *regexp.Regexp, blob []byte) []string {
	seen := make(map[string]bool)
	var out []string
	for _, match := range pattern.FindAll(blob, -1) {
		text := string(match)
		if !seen[text] {
			seen[text] = true
			out = append(out, text)
		}
	}
	return out
}

// ReadLocalCredential returns the credential the Antigravity CLI currently
// holds on this machine. It reports ok=false rather than an error when the CLI
// is simply not signed in, so callers can distinguish "nothing to import" from
// "the stored credential is broken".
func ReadLocalCredential(ctx context.Context, now time.Time) (credential CredentialInfo, ok bool, err error) {
	if runtime.GOOS != "darwin" {
		return CredentialInfo{}, false, nil
	}
	current, err := user.Current()
	if err != nil {
		return CredentialInfo{}, false, nil
	}
	ctx, cancel := context.WithTimeout(ctx, keychainReadTimeout)
	defer cancel()
	lookup := func(account string) ([]byte, bool, error) {
		cmd := exec.CommandContext(ctx, "security", "find-generic-password", "-s", keychainService, "-a", account, "-w")
		body, runErr := cmd.Output()
		if runErr == nil {
			return body, len(bytes.TrimSpace(body)) > 0, nil
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, false, ctxErr
		}
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) && exitErr.ExitCode() == 44 {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("read Antigravity keychain item: %w", runErr)
	}
	// Prefer AGY's fixed Keychain account when present. If both the fixed slot
	// and a legacy username-scoped item exist, reading the latter can make
	// identity verification pass while native AGY continues using the former.
	body, found, lookupErr := lookup(keychainAccount)
	if lookupErr != nil {
		return CredentialInfo{}, false, lookupErr
	}
	if !found {
		// The CLI stores the item under its own account name rather than the
		// unix user on some versions; try that before concluding it is absent.
		body, found, lookupErr = lookup(current.Username)
		if lookupErr != nil {
			return CredentialInfo{}, false, lookupErr
		}
		if !found {
			return CredentialInfo{}, false, nil
		}
	}
	credential, err = ParseCredential(bytes.TrimSpace(body), "antigravity keychain", now)
	if err != nil {
		return CredentialInfo{}, false, err
	}
	return credential, true, nil
}

// tokenResponse is Google's OAuth2 token-endpoint response.
type tokenResponse struct {
	AccessToken string `json:"access_token"`
	// RefreshToken is only returned when Google rotates it. Google does not
	// rotate on every refresh, so an empty value means keep the existing one
	// rather than that the credential lost its refresh token.
	RefreshToken string `json:"refresh_token"`
	IDToken      string `json:"id_token"`
	TokenType    string `json:"token_type"`
	Scope        string `json:"scope"`
	ExpiresIn    int64  `json:"expires_in"`
}

// RefreshCredential exchanges the refresh token for a fresh access token.
// Google refresh tokens are not single-use, so unlike the Claude path a
// concurrent refresh wastes a round trip rather than invalidating the
// credential; callers may still serialize to avoid the waste.
//
// The CLI binary can carry several OAuth clients and does not record which
// one a credential was issued to, so candidates are tried in order: the pair
// that last worked in this process, then the configured or discovered pairs.
// Only a client rejection advances to the next candidate — an invalid_grant is
// about the credential, not the client, and retrying it against another client
// would just multiply a terminal failure. (Google answers invalid_client for
// an unknown id but 401 unauthorized_client for a known id with the wrong
// secret, so both mark the candidate as wrong.)
func RefreshCredential(ctx context.Context, client *http.Client, credential CredentialInfo, now time.Time) (CredentialInfo, error) {
	if strings.TrimSpace(credential.RefreshToken) == "" {
		return credential, fmt.Errorf("Antigravity credential has no refresh token")
	}
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	if strings.TrimSpace(credential.OAuthClientID) != "" && strings.TrimSpace(credential.OAuthClientSecret) != "" {
		refreshed, err := refreshWithClient(ctx, client, credential, now, oauthClient{
			id: credential.OAuthClientID, secret: credential.OAuthClientSecret,
		})
		if err == nil || !isInvalidClient(err) {
			return refreshed, err
		}
	}
	if cached := workingClient.Load(); cached != nil {
		refreshed, err := refreshWithClient(ctx, client, credential, now, *cached)
		if err == nil {
			return refreshed, nil
		}
		if !isInvalidClient(err) {
			return credential, err
		}
	}
	clients := oauthClientsForRefresh()
	if len(clients) == 0 {
		return credential, fmt.Errorf("no Antigravity OAuth client available: install the agy CLI or set SUBROUTER_ANTIGRAVITY_CLIENT_ID and SUBROUTER_ANTIGRAVITY_CLIENT_SECRET")
	}
	var lastErr error
	for _, candidate := range clients {
		refreshed, err := refreshWithClient(ctx, client, credential, now, candidate)
		if err == nil {
			workingClient.Store(&candidate)
			return refreshed, nil
		}
		if !isInvalidClient(err) {
			return credential, err
		}
		lastErr = err
	}
	return credential, lastErr
}

// PrepareManagedCredential validates an imported Keychain credential and
// discovers the installed-app client that issued it. Unlike request-path
// refresh, this explicit enrollment operation tries every bounded CLI client
// candidate: Google's token endpoint can report a wrong client/credential pair
// as invalid_grant, which is otherwise indistinguishable from a revoked token.
func PrepareManagedCredential(ctx context.Context, client *http.Client, credential CredentialInfo, now time.Time) (CredentialInfo, error) {
	if strings.TrimSpace(credential.RefreshToken) == "" {
		return credential, errors.New("Antigravity credential has no refresh token")
	}
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	if strings.TrimSpace(credential.OAuthClientID) != "" && strings.TrimSpace(credential.OAuthClientSecret) != "" {
		return refreshWithClient(ctx, client, credential, now, oauthClient{id: credential.OAuthClientID, secret: credential.OAuthClientSecret})
	}
	clients := oauthClientsForRefresh()
	if len(clients) == 0 {
		return credential, errors.New("no Antigravity OAuth client available: install the agy CLI or set SUBROUTER_ANTIGRAVITY_CLIENT_ID and SUBROUTER_ANTIGRAVITY_CLIENT_SECRET")
	}
	var failures []error
	for _, candidate := range clients {
		refreshed, err := refreshWithClient(ctx, client, credential, now, candidate)
		if err == nil {
			workingClient.Store(&candidate)
			return refreshed, nil
		}
		failures = append(failures, err)
	}
	return credential, fmt.Errorf("Antigravity OAuth credential could not be validated with the installed CLI client: %w", errors.Join(failures...))
}

// invalidClientError marks a rejection of the presented client rather than of
// the credential, so the caller tries the next candidate.
type invalidClientError struct{ err error }

func (e invalidClientError) Error() string { return e.err.Error() }
func (e invalidClientError) Unwrap() error { return e.err }

func isInvalidClient(err error) bool {
	var target invalidClientError
	return errors.As(err, &target)
}

func refreshWithClient(ctx context.Context, client *http.Client, credential CredentialInfo, now time.Time, oauth oauthClient) (CredentialInfo, error) {
	form := url.Values{
		"client_id":     {oauth.id},
		"client_secret": {oauth.secret},
		"grant_type":    {"refresh_token"},
		"refresh_token": {credential.RefreshToken},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, oauthTokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return credential, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	requestClient := *client
	requestClient.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	res, err := requestClient.Do(req)
	if err != nil {
		return credential, err
	}
	defer func() { _ = res.Body.Close() }()
	body, copyErr := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if copyErr != nil {
		return credential, copyErr
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		var rejection struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(body, &rejection)
		err := fmt.Errorf("Antigravity OAuth refresh failed: %s (error=%s)", res.Status, rejection.Error)
		// A client rejection means the presented pair is wrong, not the
		// credential, so it is the one failure worth retrying with the next
		// candidate.
		if rejection.Error != "" &&
			(rejection.Error == "invalid_client" || rejection.Error == "unauthorized_client") {
			return credential, invalidClientError{err}
		}
		return credential, err
	}
	var parsed tokenResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return credential, fmt.Errorf("Antigravity OAuth refresh returned an undecodable body: %w", err)
	}
	if strings.TrimSpace(parsed.AccessToken) == "" {
		return credential, fmt.Errorf("Antigravity OAuth refresh returned no access token")
	}
	refreshed := credential
	refreshed.OAuthClientID = oauth.id
	refreshed.OAuthClientSecret = oauth.secret
	refreshed.AccessToken = parsed.AccessToken
	if strings.TrimSpace(parsed.RefreshToken) != "" {
		refreshed.RefreshToken = parsed.RefreshToken
	}
	if strings.TrimSpace(parsed.IDToken) != "" {
		refreshed.IDToken = parsed.IDToken
	}
	if strings.TrimSpace(parsed.TokenType) != "" {
		refreshed.TokenType = parsed.TokenType
	}
	if strings.TrimSpace(parsed.Scope) != "" {
		refreshed.Scope = parsed.Scope
	}
	if parsed.ExpiresIn > 0 {
		refreshed.ExpiresAt = now.Add(time.Duration(parsed.ExpiresIn) * time.Second).UTC()
	} else {
		refreshed.ExpiresAt = time.Time{}
	}
	return refreshed, nil
}

const (
	// accountID identifies the vendor CLI's single Keychain login. It remains
	// outside serving pools; managed imports use a disjoint namespace.
	accountID            = "antigravity"
	managedIDPrefix      = "antigravity-subscription:"
	maxManagedLabelBytes = 160
	managedMarkerName    = ".managed-inventory"
	maxGrantFingerprints = 16
)

// Store adapts the CLI's keychain credential to the proxy's OAuth account
// source.
type Store struct {
	// ManagedDir holds Subrouter-owned OAuth profiles. Empty uses the portable
	// Subrouter state directory.
	ManagedDir string
	// RefreshTransaction serializes managed refresh writes with add/remove and
	// overlapping supervisor generations.
	RefreshTransaction func(context.Context, func() error) error
	// managedOnly excludes the vendor CLI's fixed Keychain slot from serving.
	managedOnly bool
	// localFallback preserves the historical router-host login until the first
	// managed profile is imported. Once managed state exists, the singleton is
	// excluded to avoid routing the same refresh chain twice.
	localFallback     bool
	mu                sync.Mutex
	cached            CredentialInfo
	sourceFingerprint string
	// cachedFromRefresh records that cached may contain a rotated refresh token
	// which the unchanged CLI keychain entry cannot replace safely.
	cachedFromRefresh bool
	readCredential    func(context.Context, time.Time) (CredentialInfo, bool, error)
	refreshCredential func(context.Context, *http.Client, CredentialInfo, time.Time) (CredentialInfo, error)
}

// ServingStore returns the durable Subrouter-owned AGY profiles used by the
// proxy. The vendor CLI's fixed Keychain slot is only an explicit import source.
func ServingStore() *Store {
	store := (&Store{}).ForServing()
	store.localFallback = true
	return store
}

// ForServing preserves custom storage while excluding the direct CLI login.
func (s *Store) ForServing() *Store {
	s.managedOnly = true
	return s
}

func (s *Store) managedDir() string {
	if strings.TrimSpace(s.ManagedDir) != "" {
		return filepath.Clean(s.ManagedDir)
	}
	return filepath.Join(storepath.StateDir(), "antigravity")
}

// ManagedAccountID validates a user-facing label and returns its routing ID.
func ManagedAccountID(label string) (string, error) {
	label = strings.TrimSpace(label)
	if label == "" {
		return "", errors.New("Antigravity account label is required")
	}
	if len(label) > maxManagedLabelBytes {
		return "", fmt.Errorf("Antigravity account label must be at most %d bytes", maxManagedLabelBytes)
	}
	if strings.HasPrefix(strings.ToLower(label), managedIDPrefix) {
		return "", fmt.Errorf("Antigravity account label must not start with reserved prefix %q", managedIDPrefix)
	}
	for _, r := range label {
		if unicode.IsControl(r) || unicode.In(r, unicode.Cf, unicode.Zl, unicode.Zp) {
			return "", errors.New("Antigravity account label contains a control character")
		}
	}
	return managedIDPrefix + strings.ToLower(label), nil
}

// IsManagedAccountID reports whether id belongs to Subrouter's isolated AGY
// profile namespace rather than the vendor CLI's singleton Keychain login.
func IsManagedAccountID(id string) bool {
	_, ok := managedAccountLabel(id)
	return ok
}

func managedAccountLabel(id string) (string, bool) {
	if !strings.HasPrefix(id, managedIDPrefix) {
		return "", false
	}
	label := strings.TrimPrefix(id, managedIDPrefix)
	canonical, err := ManagedAccountID(label)
	return label, err == nil && canonical == id
}

func managedFilename(id string) (string, error) {
	label, ok := managedAccountLabel(id)
	if !ok {
		return "", fmt.Errorf("%q is not a managed Antigravity account", id)
	}
	return base64.RawURLEncoding.EncodeToString([]byte(label)) + ".json", nil
}

func managedAccountID(filename string) (string, bool) {
	if filepath.Ext(filename) != ".json" {
		return "", false
	}
	decoded, err := base64.RawURLEncoding.DecodeString(strings.TrimSuffix(filename, ".json"))
	if err != nil {
		return "", false
	}
	id, err := ManagedAccountID(string(decoded))
	return id, err == nil
}

type managedCredentialFile struct {
	Version              int            `json:"version"`
	Label                string         `json:"label"`
	IdentityFingerprint  string         `json:"identity_fingerprint,omitempty"`
	IdentityFingerprints []string       `json:"identity_fingerprints,omitempty"`
	Credential           CredentialInfo `json:"credential"`
}

func (s *Store) managedInventoryInitialized() (bool, error) {
	info, err := os.Lstat(filepath.Join(s.managedDir(), managedMarkerName))
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !info.Mode().IsRegular() {
		return false, errors.New("managed Antigravity inventory marker is not a regular file")
	}
	return true, nil
}

func (s *Store) initializeManagedInventory() error {
	dir := s.managedDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	path := filepath.Join(dir, managedMarkerName)
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if os.IsExist(err) {
		initialized, checkErr := s.managedInventoryInitialized()
		if checkErr != nil {
			return checkErr
		}
		if initialized {
			return nil
		}
	}
	if err != nil {
		return err
	}
	if _, err := file.WriteString("managed-v1\n"); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return err
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(path)
		return err
	}
	if runtime.GOOS != "windows" {
		directory, err := os.Open(dir)
		if err != nil {
			return err
		}
		if err := directory.Sync(); err != nil {
			_ = directory.Close()
			return err
		}
		return directory.Close()
	}
	return nil
}

func (s *Store) legacyFallbackRetired(ctx context.Context) (bool, error) {
	initialized, err := s.managedInventoryInitialized()
	if err != nil || initialized {
		return initialized, err
	}
	ids, err := s.AccountInventoryIDs(ctx)
	if err != nil || len(ids) == 0 {
		return false, err
	}
	if err := s.initializeManagedInventory(); err != nil {
		return false, err
	}
	return true, nil
}

func (s *Store) managedCredentialPath(id string) (string, error) {
	filename, err := managedFilename(id)
	if err != nil {
		return "", err
	}
	return filepath.Join(s.managedDir(), filename), nil
}

// Provider implements the proxy's OAuth account source.
func (*Store) Provider() account.Provider {
	return account.ProviderAntigravity
}

// ListAccounts returns managed profiles and, outside serving mode, the CLI's
// singleton Keychain login. A malformed managed profile remains visible to
// inventory accounting through AccountInventoryIDs.
func (s *Store) ListAccounts(ctx context.Context) ([]account.Account, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var result []account.Account
	var listErrors []error
	now := time.Now()
	includeLocal := !s.managedOnly
	if s.managedOnly && s.localFallback {
		initialized, err := s.legacyFallbackRetired(ctx)
		if err != nil {
			listErrors = append(listErrors, err)
		} else {
			includeLocal = !initialized
		}
	}
	if includeLocal {
		credential, ok, err := s.read(ctx, now)
		if err != nil {
			listErrors = append(listErrors, err)
		} else if ok {
			fingerprint := credentialFingerprint(credential)
			if s.cached.AccessToken != "" && s.sourceFingerprint == fingerprint &&
				(s.cachedFromRefresh || !s.cached.NeedsRefresh(now)) {
				credential = s.cached
			} else {
				s.cached = credential
				s.sourceFingerprint = fingerprint
				s.cachedFromRefresh = false
			}
			result = append(result, credentialAccount(accountID, credentialDisplayLabel(credential), "antigravity keychain", credential))
		}
	}
	entries, err := os.ReadDir(s.managedDir())
	if err != nil {
		if !os.IsNotExist(err) {
			listErrors = append(listErrors, err)
		}
		return result, errors.Join(listErrors...)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	for _, entry := range entries {
		id, valid := managedAccountID(entry.Name())
		if entry.IsDir() || !valid {
			continue
		}
		canonical, _ := managedFilename(id)
		if entry.Name() != canonical {
			listErrors = append(listErrors, fmt.Errorf("ignore noncanonical managed Antigravity credential for %s", id))
			continue
		}
		stored, ok, readErr := readManagedCredential(filepath.Join(s.managedDir(), entry.Name()))
		if readErr != nil {
			listErrors = append(listErrors, fmt.Errorf("read managed Antigravity account %s: %w", id, readErr))
			continue
		}
		if ok {
			result = append(result, credentialAccount(id, stored.Label, "subrouter managed Antigravity credential", stored.Credential))
		}
	}
	return result, errors.Join(listErrors...)
}

// RefreshAccount refreshes near-expiry credentials. Managed profiles persist
// the complete returned chain transactionally; the direct CLI's Keychain item
// remains read-only and keeps only an in-process refreshed copy.
func (s *Store) RefreshAccount(ctx context.Context, client *http.Client, acct account.Account) (account.Account, error) {
	now := time.Now()
	if strings.HasPrefix(acct.ID, managedIDPrefix) {
		var result account.Account
		refresh := func() error {
			refreshNow := time.Now()
			path, err := s.managedCredentialPath(acct.ID)
			if err != nil {
				return err
			}
			stored, ok, err := readManagedCredential(path)
			if err != nil {
				return err
			}
			if !ok {
				return fmt.Errorf("managed Antigravity credential %q is missing", acct.ID)
			}
			if stored.Credential.NeedsRefresh(refreshNow) {
				previous := stored.Credential
				refreshed, err := s.refresh(ctx, client, stored.Credential, refreshNow)
				if err != nil {
					return err
				}
				stored.IdentityFingerprints = canonicalGrantFingerprints(append([]string{
					credentialGrantFingerprint(previous), credentialGrantFingerprint(refreshed),
				}, managedGrantFingerprints(stored)...))
				stored.IdentityFingerprint = ""
				stored.Credential = refreshed
				if err := writeManagedCredential(path, stored); err != nil {
					return fmt.Errorf("persist refreshed Antigravity credential: %w", err)
				}
			}
			result = credentialAccount(acct.ID, stored.Label, "subrouter managed Antigravity credential", stored.Credential)
			return nil
		}
		var err error
		if s.RefreshTransaction != nil {
			err = s.RefreshTransaction(ctx, refresh)
		} else {
			s.mu.Lock()
			defer s.mu.Unlock()
			err = refresh()
		}
		if err != nil {
			return acct, err
		}
		return result, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.managedOnly && !s.localFallback {
		return acct, fmt.Errorf("Antigravity CLI credential is not routable; import it with 'sr agy add <label>'")
	}
	if s.managedOnly {
		initialized, err := s.legacyFallbackRetired(ctx)
		if err != nil {
			return acct, err
		}
		if initialized {
			return acct, fmt.Errorf("legacy Antigravity Keychain fallback was retired after managed profile import")
		}
	}
	credential, ok, err := s.read(ctx, now)
	if err != nil {
		return acct, err
	}
	if !ok {
		s.cached = CredentialInfo{}
		s.sourceFingerprint = ""
		s.cachedFromRefresh = false
		return acct, fmt.Errorf("Antigravity keychain credential is missing")
	}
	fingerprint := credentialFingerprint(credential)
	if s.cached.AccessToken != "" && s.sourceFingerprint == fingerprint && !s.cached.NeedsRefresh(now) {
		return credentialAccount(accountID, credentialDisplayLabel(s.cached), "antigravity keychain", s.cached), nil
	}
	// A prior refresh may have rotated the refresh token even though the CLI's
	// keychain entry is unchanged. Continue from the in-process credential when
	// available; otherwise the stale keychain token can force a refresh on every
	// request or eventually fail after rotation.
	if s.cached.RefreshToken != "" && s.sourceFingerprint == fingerprint {
		credential = s.cached
	} else {
		s.sourceFingerprint = fingerprint
		s.cachedFromRefresh = false
	}
	if !credential.NeedsRefresh(now) {
		s.cached = credential
		return credentialAccount(accountID, credentialDisplayLabel(credential), "antigravity keychain", credential), nil
	}
	refreshed, err := s.refresh(ctx, client, credential, now)
	if err != nil {
		return acct, err
	}
	s.cached = refreshed
	s.cachedFromRefresh = true
	return credentialAccount(accountID, credentialDisplayLabel(refreshed), "antigravity keychain", refreshed), nil
}

// FetchUsageIdentity implements the proxy's identity-aware OAuth telemetry
// source. The refreshed account token selects one managed profile; telemetry
// never reads or mutates the vendor CLI's singleton login.
func (s *Store) FetchUsageIdentity(ctx context.Context, client *http.Client, acct account.Account) (string, string, []accounts.UsageWindow, error) {
	details, err := FetchUsage(ctx, client, acct.Token, time.Now())
	plan := details.Plan
	if plan == "" {
		plan = "subscription"
	}
	return details.Email, plan, details.Windows, err
}

// FetchUsage keeps Store compatible with generic OAuth usage consumers. Status
// callers use FetchUsageIdentity so a provider-verified email can be rendered.
func (s *Store) FetchUsage(ctx context.Context, client *http.Client, acct account.Account) (string, []accounts.UsageWindow, error) {
	_, plan, windows, err := s.FetchUsageIdentity(ctx, client, acct)
	return plan, windows, err
}

func credentialFingerprint(credential CredentialInfo) string {
	if credential.RefreshToken != "" {
		return "refresh:" + credential.RefreshToken
	}
	if credential.IDToken != "" {
		return "id:" + credential.IDToken
	}
	return "access:" + credential.AccessToken
}

func (s *Store) read(ctx context.Context, now time.Time) (CredentialInfo, bool, error) {
	if s.readCredential != nil {
		return s.readCredential(ctx, now)
	}
	return ReadLocalCredential(ctx, now)
}

func (s *Store) refresh(ctx context.Context, client *http.Client, credential CredentialInfo, now time.Time) (CredentialInfo, error) {
	if s.refreshCredential != nil {
		return s.refreshCredential(ctx, client, credential, now)
	}
	return RefreshCredential(ctx, client, credential, now)
}

func credentialAccount(id, label, source string, credential CredentialInfo) account.Account {
	return account.Account{
		ID:       id,
		Provider: account.ProviderAntigravity,
		AuthMode: account.AuthModeOAuth,
		Label:    label,
		Token:    credential.AccessToken,
		Source:   source,
	}
}

func readManagedCredential(path string) (managedCredentialFile, bool, error) {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return managedCredentialFile{}, false, nil
	}
	if err != nil {
		return managedCredentialFile{}, false, err
	}
	if !info.Mode().IsRegular() {
		return managedCredentialFile{}, false, errors.New("managed Antigravity credential is not a regular file")
	}
	if info.Size() > 64<<10 {
		return managedCredentialFile{}, false, errors.New("managed Antigravity credential is too large")
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return managedCredentialFile{}, false, err
	}
	var stored managedCredentialFile
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&stored); err != nil {
		return managedCredentialFile{}, false, fmt.Errorf("decode managed Antigravity credential: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return managedCredentialFile{}, false, errors.New("managed Antigravity credential has trailing data")
	}
	if stored.Version != 1 || strings.TrimSpace(stored.Label) == "" ||
		strings.TrimSpace(stored.Credential.RefreshToken) == "" {
		return managedCredentialFile{}, false, errors.New("managed Antigravity credential is incomplete")
	}
	return stored, true, nil
}

func writeManagedCredential(path string, stored managedCredentialFile) error {
	stored.Version = 1
	body, err := json.Marshal(stored)
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".antigravity-credential-*")
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
	if err := replaceManagedCredentialFile(tmp.Name(), path); err != nil {
		return err
	}
	if runtime.GOOS != "windows" {
		directory, err := os.Open(dir)
		if err != nil {
			return err
		}
		if err := directory.Sync(); err != nil {
			_ = directory.Close()
			return err
		}
		return directory.Close()
	}
	return nil
}

// SaveManagedCredential imports one independently refreshable account without
// modifying the vendor CLI's Keychain item.
func (s *Store) SaveManagedCredential(label string, credential CredentialInfo) (account.Account, error) {
	return s.SaveManagedCredentialFromGrant(label, credential, credential)
}

// SaveManagedCredentialFromGrant persists a validated credential while
// anchoring duplicate detection to the refresh grant submitted for enrollment.
// JWT claims are intentionally ignored: account import accepts bearer material,
// not an independently verified Google identity assertion.
func (s *Store) SaveManagedCredentialFromGrant(label string, credential, grant CredentialInfo) (account.Account, error) {
	display := strings.TrimSpace(label)
	id, err := ManagedAccountID(display)
	if err != nil {
		return account.Account{}, err
	}
	if strings.TrimSpace(credential.AccessToken) == "" || strings.TrimSpace(credential.RefreshToken) == "" {
		return account.Account{}, errors.New("Antigravity OAuth credential is incomplete")
	}
	path, err := s.managedCredentialPath(id)
	if err != nil {
		return account.Account{}, err
	}
	fingerprint := credentialGrantFingerprint(grant)
	fingerprints := []string{fingerprint, credentialGrantFingerprint(credential)}
	var prior managedCredentialFile
	var priorExists bool
	if existing, ok, readErr := readManagedCredential(path); readErr != nil {
		return account.Account{}, readErr
	} else if ok {
		prior, priorExists = existing, true
		fingerprints = append(fingerprints, managedGrantFingerprints(existing)...)
	}
	fingerprints = canonicalGrantFingerprints(fingerprints)
	entries, err := os.ReadDir(s.managedDir())
	if err != nil && !os.IsNotExist(err) {
		return account.Account{}, err
	}
	for _, entry := range entries {
		existingID, valid := managedAccountID(entry.Name())
		if entry.IsDir() || !valid || existingID == id {
			continue
		}
		existing, ok, readErr := readManagedCredential(filepath.Join(s.managedDir(), entry.Name()))
		if readErr != nil {
			return account.Account{}, fmt.Errorf("check existing managed Antigravity identities: %w", readErr)
		}
		if ok {
			for _, known := range managedGrantFingerprints(existing) {
				if subtleCredentialFingerprintEqual(fingerprint, known) {
					return account.Account{}, ErrManagedIdentityExists
				}
			}
		}
	}
	stored := managedCredentialFile{Version: 1, Label: display, IdentityFingerprints: fingerprints, Credential: credential}
	if err := writeManagedCredential(path, stored); err != nil {
		return account.Account{}, err
	}
	if err := s.initializeManagedInventory(); err != nil {
		if priorExists {
			_ = writeManagedCredential(path, prior)
		} else {
			_ = os.Remove(path)
		}
		return account.Account{}, fmt.Errorf("initialize managed Antigravity inventory: %w", err)
	}
	return credentialAccount(id, display, "subrouter managed Antigravity credential", credential), nil
}

func managedGrantFingerprints(stored managedCredentialFile) []string {
	fingerprints := append([]string(nil), stored.IdentityFingerprints...)
	if stored.IdentityFingerprint != "" {
		fingerprints = append(fingerprints, stored.IdentityFingerprint)
	}
	if stored.Credential.RefreshToken != "" {
		fingerprints = append(fingerprints, credentialGrantFingerprint(stored.Credential))
	}
	return canonicalGrantFingerprints(fingerprints)
}

func canonicalGrantFingerprints(input []string) []string {
	capacity := len(input)
	if capacity > maxGrantFingerprints {
		capacity = maxGrantFingerprints
	}
	result := make([]string, 0, capacity)
	for _, candidate := range input {
		if candidate == "" {
			continue
		}
		duplicate := false
		for _, existing := range result {
			if subtleCredentialFingerprintEqual(candidate, existing) {
				duplicate = true
				break
			}
		}
		if !duplicate {
			result = append(result, candidate)
			if len(result) == maxGrantFingerprints {
				break
			}
		}
	}
	return result
}

func credentialGrantFingerprint(credential CredentialInfo) string {
	digest := sha256.Sum256([]byte("subrouter-antigravity-refresh-grant-v1\x00" + credential.RefreshToken))
	return hex.EncodeToString(digest[:])
}

func subtleCredentialFingerprintEqual(left, right string) bool {
	return subtle.ConstantTimeCompare([]byte(left), []byte(right)) == 1
}

func (s *Store) ReadManagedCredential(label string) (CredentialInfo, bool, error) {
	id, err := ManagedAccountID(label)
	if err != nil {
		return CredentialInfo{}, false, err
	}
	path, err := s.managedCredentialPath(id)
	if err != nil {
		return CredentialInfo{}, false, err
	}
	stored, ok, err := readManagedCredential(path)
	return stored.Credential, ok, err
}

func (s *Store) ManagedAccountExists(label string) (bool, error) {
	id, err := ManagedAccountID(label)
	if err != nil {
		return false, err
	}
	path, err := s.managedCredentialPath(id)
	if err != nil {
		return false, err
	}
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return false, nil
	}
	return err == nil && !info.IsDir(), err
}

func (s *Store) RemoveManagedAccount(label string) (account.Account, bool, error) {
	id, err := ManagedAccountID(label)
	if err != nil {
		return account.Account{}, false, err
	}
	path, err := s.managedCredentialPath(id)
	if err != nil {
		return account.Account{}, false, err
	}
	stored, ok, readErr := readManagedCredential(path)
	if !ok && readErr == nil {
		return account.Account{}, false, nil
	}
	if err := os.Remove(path); err != nil {
		if os.IsNotExist(err) {
			return account.Account{}, false, nil
		}
		return account.Account{}, false, err
	}
	labelOut := strings.TrimSpace(label)
	credential := CredentialInfo{}
	if readErr == nil {
		labelOut, credential = stored.Label, stored.Credential
	}
	return credentialAccount(id, labelOut, "subrouter managed Antigravity credential", credential), true, nil
}

func (s *Store) AccountInventoryIDs(_ context.Context) ([]string, error) {
	entries, err := os.ReadDir(s.managedDir())
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, entry := range entries {
		if !entry.IsDir() {
			if id, ok := managedAccountID(entry.Name()); ok {
				ids = append(ids, id)
			}
		}
	}
	return ids, nil
}

func (s *Store) AccountInventoryCount(ctx context.Context) (int, error) {
	ids, err := s.AccountInventoryIDs(ctx)
	return len(ids), err
}

func credentialDisplayLabel(credential CredentialInfo) string {
	for _, token := range []string{credential.IDToken, credential.AccessToken} {
		parts := strings.Split(token, ".")
		if len(parts) != 3 {
			continue
		}
		body, err := base64.RawURLEncoding.DecodeString(parts[1])
		if err != nil {
			continue
		}
		var claims struct {
			Email string `json:"email"`
		}
		if json.Unmarshal(body, &claims) != nil {
			continue
		}
		email := strings.TrimSpace(claims.Email)
		if len(email) == 0 || len(email) > 320 || !strings.Contains(email, "@") {
			continue
		}
		valid := true
		for _, char := range email {
			if unicode.IsControl(char) || unicode.Is(unicode.Cf, char) ||
				unicode.Is(unicode.Zl, char) || unicode.Is(unicode.Zp, char) {
				valid = false
				break
			}
		}
		if valid {
			return email
		}
	}
	return "router agy login"
}
