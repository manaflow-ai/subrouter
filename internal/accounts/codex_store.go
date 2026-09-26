package accounts

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	accountpkg "github.com/manaflow-ai/subrouter/account"
	"github.com/manaflow-ai/subrouter/internal/storepath"
)

type CodexStore struct {
	Dir                   string
	DisableActiveAuthSync bool
	RequireIsolatedOAuth  bool
}

const migrationBatchControlLockID = ".subrouter-migration-batch-control"

var storedAccountProcessLocks sync.Map

func storedAccountProcessMutex(storeDir, identifier string) *sync.Mutex {
	key, err := filepath.Abs(filepath.Join(storeDir, "."+accountLockFilename(identifier)+".lock"))
	if err != nil {
		key = filepath.Clean(filepath.Join(storeDir, "."+accountLockFilename(identifier)+".lock"))
	}
	value, _ := storedAccountProcessLocks.LoadOrStore(key, &sync.Mutex{})
	return value.(*sync.Mutex)
}

// StoredAccountLease holds the same process and cross-process account lock used
// by saves, refreshes, and exact removals. Callers coordinating a stored
// credential with another provider store can keep one exact stored identity
// stable across that larger transaction without re-entering the account lock.
//
// A lease is bound to one exact account identifier. Close must be called once.
type StoredAccountLease struct {
	store      CodexStore
	identifier string
	lock       *accountFileLock
	closed     bool
}

// AcquireStoredAccountLease locks one exact stored-account identity. The
// caller must already hold any broader transaction locks; the canonical order
// is broader transaction -> stored-account lease -> provider-specific locks.
func (s CodexStore) AcquireStoredAccountLease(identifier string) (*StoredAccountLease, error) {
	identifier = strings.TrimSpace(identifier)
	if err := validateStoredAccountIdentifier(identifier); err != nil {
		return nil, err
	}
	lock, err := s.lockStoredAccount(identifier)
	if err != nil {
		return nil, err
	}
	return &StoredAccountLease{store: s, identifier: identifier, lock: lock}, nil
}

// Close releases the stored-account lease.
func (l *StoredAccountLease) Close() error {
	if l == nil || l.lock == nil || l.closed {
		return nil
	}
	l.closed = true
	return l.lock.Close()
}

func (l *StoredAccountLease) validFor(identifier string) error {
	if l == nil || l.lock == nil || l.closed {
		return errors.New("stored account lease is not active")
	}
	if !strings.EqualFold(strings.TrimSpace(identifier), l.identifier) {
		return fmt.Errorf("stored account lease for %q cannot access %q", l.identifier, identifier)
	}
	return nil
}

// FindExact returns the exact account protected by this lease.
func (l *StoredAccountLease) FindExact() (StoredCodexAccount, bool, error) {
	if err := l.validFor(l.identifier); err != nil {
		return StoredCodexAccount{}, false, err
	}
	return l.store.findStoredExact(l.identifier)
}

type StorageKeyCollisionError struct {
	Identifier         string
	ExistingIdentifier string
}

func (e *StorageKeyCollisionError) Error() string {
	return fmt.Sprintf("account identifier %q collides with stored account %q", e.Identifier, e.ExistingIdentifier)
}

type StoredCodexAccount struct {
	// Email is the durable account key, including legacy emails and provider
	// aliases. For Codex OAuth, LoginEmail returns the actual sign-in email.
	Email                 string                     `json:"email"`
	Label                 string                     `json:"label,omitempty"`
	Provider              Provider                   `json:"provider,omitempty"`
	OAuthCredentialOrigin CodexOAuthCredentialOrigin `json:"oauthCredentialOrigin,omitempty"`
	MigrationBatchID      string                     `json:"migrationBatchId,omitempty"`
	AddedAt               string                     `json:"addedAt"`
	Auth                  CodexAuthFile              `json:"auth"`
	ProjectID             string                     `json:"projectId,omitempty"`
	ProjectName           string                     `json:"projectName,omitempty"`
	AdminKeyLabel         string                     `json:"adminKeyLabel,omitempty"`
	Breadcrumbs           []CodexAuthBreadcrumb      `json:"breadcrumbs,omitempty"`
	HostClaim             *CodexHostClaim            `json:"hostClaim,omitempty"`
}

type CodexOAuthCredentialOrigin string

const (
	CodexOAuthOriginInteractiveImport   CodexOAuthCredentialOrigin = "interactive-import"
	CodexOAuthOriginIsolatedServerLogin CodexOAuthCredentialOrigin = "isolated-server-login"
	CodexOAuthOriginServerAttested      CodexOAuthCredentialOrigin = "server-attested-transfer"
)

type CodexAuthFile struct {
	Tokens         *CodexTokens         `json:"tokens,omitempty"`
	LastRefresh    string               `json:"last_refresh,omitempty"`
	AuthMode       string               `json:"auth_mode,omitempty"`
	OpenAIAPIKey   string               `json:"OPENAI_API_KEY,omitempty"`
	RefreshFailure *CodexRefreshFailure `json:"refresh_failure,omitempty"`
}

type CodexTokens struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	IDToken      string `json:"id_token"`
	AccountID    string `json:"account_id,omitempty"`
}

type CodexRefreshFailure struct {
	At              string `json:"at"`
	StatusCode      int    `json:"status_code,omitempty"`
	ProviderType    string `json:"provider_type,omitempty"`
	ProviderCode    string `json:"provider_code,omitempty"`
	ProviderMessage string `json:"provider_message,omitempty"`
}

type CodexAuthBreadcrumb struct {
	At                 string `json:"at"`
	Event              string `json:"event"`
	Source             string `json:"source,omitempty"`
	Reason             string `json:"reason,omitempty"`
	Host               string `json:"host,omitempty"`
	PID                int    `json:"pid,omitempty"`
	PPID               int    `json:"ppid,omitempty"`
	Executable         string `json:"executable,omitempty"`
	WorkingDir         string `json:"working_dir,omitempty"`
	StoreDir           string `json:"store_dir,omitempty"`
	SourcePath         string `json:"source_path,omitempty"`
	Force              bool   `json:"force"`
	LastRefresh        string `json:"last_refresh,omitempty"`
	AccessExp          string `json:"access_exp,omitempty"`
	AccessExpired      bool   `json:"access_expired"`
	AccessFP           string `json:"access_fp,omitempty"`
	RefreshFP          string `json:"refresh_fp,omitempty"`
	AccountID          string `json:"account_id,omitempty"`
	OldAccessExp       string `json:"old_access_exp,omitempty"`
	OldAccessFP        string `json:"old_access_fp,omitempty"`
	OldRefreshFP       string `json:"old_refresh_fp,omitempty"`
	OldAccountID       string `json:"old_account_id,omitempty"`
	NewAccessExp       string `json:"new_access_exp,omitempty"`
	NewAccessFP        string `json:"new_access_fp,omitempty"`
	NewRefreshFP       string `json:"new_refresh_fp,omitempty"`
	NewAccountID       string `json:"new_account_id,omitempty"`
	RecoveredAccessExp string `json:"recovered_access_exp,omitempty"`
	RecoveredAccessFP  string `json:"recovered_access_fp,omitempty"`
	RecoveredRefreshFP string `json:"recovered_refresh_fp,omitempty"`
	RecoveredAccountID string `json:"recovered_account_id,omitempty"`
	StatusCode         int    `json:"status_code,omitempty"`
	ProviderType       string `json:"provider_type,omitempty"`
	ProviderCode       string `json:"provider_code,omitempty"`
	ProviderMessage    string `json:"provider_message,omitempty"`
}

func DefaultCodexStore() CodexStore {
	return CodexStore{Dir: filepath.Join(storepath.CodexDir(), "accounts")}
}

// DefaultCodexStoreForReadOnlyInspection resolves the same effective source as
// DefaultCodexStore's best-effort legacy migration without copying any state.
func DefaultCodexStoreForReadOnlyInspection() CodexStore {
	return CodexStoreForStateRootReadOnlyInspection(storepath.StateDir())
}

// CodexStoreForStateRootReadOnlyInspection resolves the effective account
// source for an explicit state root without performing legacy migration.
func CodexStoreForStateRootReadOnlyInspection(stateRoot string) CodexStore {
	return CodexStore{Dir: filepath.Join(storepath.CodexDirForStateRootReadOnlyInspection(stateRoot), "accounts")}
}

func (s CodexStore) StoreDir() string {
	return filepath.Dir(s.Dir)
}

func (s CodexStore) List() ([]Account, error) {
	stored, err := s.ListStored()
	if err != nil {
		return nil, err
	}
	accounts := make([]Account, 0, len(stored))
	for _, item := range stored {
		account, ok := item.toAccount(item.SourcePath(s))
		if ok {
			accounts = append(accounts, account)
		}
	}
	sort.Slice(accounts, func(i, j int) bool {
		return accounts[i].ID < accounts[j].ID
	})
	return accounts, nil
}

func (s CodexStore) ListStored() ([]StoredCodexAccount, error) {
	lock, err := s.lockStoredAccount(migrationBatchControlLockID)
	if err != nil {
		return nil, err
	}
	defer lock.Close()
	return s.listStored(false)
}

// ListStoredReadOnly reads the durable snapshot without creating a lock file.
// Writers publish account files atomically, so diagnostics can safely inspect
// individual files; unlike ListStored, this does not promise one transactionally
// locked view across a concurrent migration batch.
func (s CodexStore) ListStoredReadOnly() ([]StoredCodexAccount, error) {
	return s.listStored(false)
}

func (s CodexStore) listStored(includeInactiveMigrations bool) ([]StoredCodexAccount, error) {
	var activeMigrationBatches map[string]struct{}
	if !includeInactiveMigrations {
		var err error
		activeMigrationBatches, err = s.activeMigrationBatchSnapshot()
		if err != nil {
			return nil, err
		}
	}
	files, err := os.ReadDir(s.Dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	accounts := make([]StoredCodexAccount, 0, len(files))
	for _, file := range files {
		if file.IsDir() || strings.HasPrefix(file.Name(), ".") || !strings.HasSuffix(file.Name(), ".json") {
			continue
		}

		path := filepath.Join(s.Dir, file.Name())
		body, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", path, err)
		}

		var stored StoredCodexAccount
		if err := json.Unmarshal(body, &stored); err != nil {
			return nil, fmt.Errorf("parse %s: %w", path, err)
		}
		if !includeInactiveMigrations && stored.MigrationBatchID != "" {
			if err := validateMigrationBatchID(stored.MigrationBatchID); err != nil {
				return nil, err
			}
			if _, active := activeMigrationBatches[stored.MigrationBatchID]; !active {
				continue
			}
		}
		if strings.TrimSpace(stored.Email) != "" {
			accounts = append(accounts, stored)
		}
	}

	sort.Slice(accounts, func(i, j int) bool {
		return accounts[i].Email < accounts[j].Email
	})
	return accounts, nil
}

func (a StoredCodexAccount) SourcePath(s CodexStore) string {
	// Before workspace identifiers, punctuation was replaced with underscores.
	// Keep an existing record at that path, but do not adopt another alias.
	if emailToFilename(a.Email) != legacyEmailToFilename(a.Email) {
		legacyPath := filepath.Join(s.Dir, legacyEmailToFilename(a.Email))
		if body, err := os.ReadFile(legacyPath); err == nil {
			var legacy StoredCodexAccount
			if json.Unmarshal(body, &legacy) == nil && strings.EqualFold(strings.TrimSpace(legacy.Email), strings.TrimSpace(a.Email)) {
				return legacyPath
			}
		}
	}
	return filepath.Join(s.Dir, emailToFilename(a.Email))
}

func (a StoredCodexAccount) IsAPIKey() bool {
	return a.Auth.AuthMode == "apikey" || a.Auth.OpenAIAPIKey != ""
}

func (a StoredCodexAccount) APIKeyLabel() string {
	return strings.TrimPrefix(a.Email, "apikey:")
}

func (a StoredCodexAccount) ProviderOrDefault() Provider {
	if a.Provider != "" {
		return a.Provider
	}
	return ProviderCodex
}

func (a StoredCodexAccount) LoginEmail() string {
	if a.Auth.Tokens != nil {
		if email, err := ExtractEmailFromJWT(a.Auth.Tokens.IDToken); err == nil && strings.TrimSpace(email) != "" {
			return strings.TrimSpace(email)
		}
	}
	return a.Email
}

// DisplayName is what a person reads for this record. A Codex OAuth record
// shows its login email and plan, "lawrence@example.com [team]" for an
// organization workspace and "lawrence@example.com [pro]" for the personal
// plan, so two records under one email tell apart at a glance. The stored
// key of an owner-identified record is an opaque "codex-owner-<hash>" and
// never appears; when such a record has no plan claim, a workspace prefix
// stands in. An explicit label always wins; API keys keep their identifier.
func (a StoredCodexAccount) DisplayName() string {
	if label := strings.TrimSpace(a.Label); label != "" {
		return label
	}
	if a.IsAPIKey() || a.Auth.Tokens == nil {
		return a.LoginEmail()
	}
	email := a.LoginEmail()
	if plan := ExtractChatGPTPlanType(a.Auth); plan != "" {
		return email + " [" + plan + "]"
	}
	if strings.HasPrefix(a.Email, codexOwnerKeyPrefix) {
		if workspace := ExtractChatGPTAccountID(a.Auth); len(workspace) >= 8 {
			return email + " [workspace " + workspace[:8] + "]"
		}
	}
	return email
}

func (a StoredCodexAccount) toAccount(source string) (Account, bool) {
	id := strings.TrimSpace(a.Email)
	if id == "" {
		return Account{}, false
	}

	addedAt, _ := time.Parse(time.RFC3339, a.AddedAt)
	label := a.DisplayName()
	out := Account{
		ID:       id,
		Provider: a.ProviderOrDefault(),
		Label:    label,
		Email:    id,
		AddedAt:  addedAt,
		Source:   source,
	}

	if a.IsAPIKey() {
		out.AuthMode = AuthModeAPIKey
		out.Token = a.Auth.OpenAIAPIKey
		return out, out.Token != ""
	}

	if a.Auth.Tokens == nil || a.Auth.Tokens.AccessToken == "" {
		return Account{}, false
	}

	out.AuthMode = AuthModeOAuth
	out.Token = a.Auth.Tokens.AccessToken
	out.CredentialVersion = accountpkg.OAuthCredentialVersion(
		a.Auth.Tokens.AccessToken, a.Auth.Tokens.RefreshToken,
	)
	out.AccountID = ExtractChatGPTAccountID(a.Auth)
	out.Email = a.LoginEmail()
	return out, true
}

func (a StoredCodexAccount) Account(source string) (Account, bool) {
	return a.toAccount(source)
}

func (s CodexStore) SaveStored(account StoredCodexAccount) error {
	if err := validateStoredAccountIdentifier(account.Email); err != nil {
		return err
	}
	if account.MigrationBatchID != "" {
		batchLock, err := s.lockStoredAccount(migrationBatchControlLockID)
		if err != nil {
			return err
		}
		defer batchLock.Close()
	}
	lock, err := s.lockStoredAccount(account.Email)
	if err != nil {
		return err
	}
	defer lock.Close()
	return s.saveStoredUnlocked(account)
}

// ReplaceStoredOAuthWithIsolated replaces one existing Codex OAuth credential
// after an account-manager login whose identity has already been checked. The
// read and write share the account lock so a concurrent repair, removal, or
// refresh cannot be overwritten by a stale pre-login snapshot.
func (s CodexStore) ReplaceStoredOAuthWithIsolated(ctx context.Context, identifier string, auth CodexAuthFile) error {
	identifier = strings.TrimSpace(identifier)
	if identifier == "" {
		return errors.New("account identifier is required")
	}
	if auth.Tokens == nil || strings.TrimSpace(auth.Tokens.AccessToken) == "" ||
		strings.TrimSpace(auth.Tokens.RefreshToken) == "" || strings.TrimSpace(auth.Tokens.IDToken) == "" {
		return errors.New("isolated Codex login did not produce complete OAuth auth")
	}
	lock, err := s.lockStoredAccount(identifier)
	if err != nil {
		return err
	}
	defer lock.Close()
	account, found, err := s.findStoredExact(identifier)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("account %q changed or was removed during migration", identifier)
	}
	if account.IsAPIKey() || account.ProviderOrDefault() != ProviderCodex {
		return fmt.Errorf("account %q is not a Codex OAuth account", identifier)
	}
	if !CanReplaceCodexOAuthIdentity(account.Auth, auth) {
		return errors.New("isolated Codex login identity does not match the stored account")
	}
	previous := account
	account.Auth = auth
	account.Auth.RefreshFailure = nil
	account.OAuthCredentialOrigin = CodexOAuthOriginIsolatedServerLogin
	appendCodexAuthBreadcrumb(
		ctx, s, &account, "credential_reenrolled_isolated", "account_manager", false,
		&previous, &account, nil, nil,
	)
	return s.saveStoredUnlocked(account)
}

func (s CodexStore) saveStoredUnlocked(account StoredCodexAccount) error {
	if err := validateStoredAccountIdentifier(account.Email); err != nil {
		return err
	}
	stored, err := s.listStored(true)
	if err != nil {
		return err
	}
	var canonical string
	for _, existing := range stored {
		if !strings.EqualFold(strings.TrimSpace(existing.Email), strings.TrimSpace(account.Email)) {
			continue
		}
		existingOwner, ownerErr := ParseCodexOwner(existing.Auth)
		incomingOwner, incomingErr := ParseCodexOwner(account.Auth)
		if existing.ProviderOrDefault() == ProviderCodex && account.ProviderOrDefault() == ProviderCodex &&
			!existing.IsAPIKey() && !account.IsAPIKey() &&
			(ownerErr != nil || incomingErr != nil || !codexOwnerTransitionAllowed(existingOwner, incomingOwner)) {
			return fmt.Errorf("Codex workspace does not match stored account %q", account.Email)
		}
		if canonical != "" && canonical != existing.Email {
			return fmt.Errorf("multiple stored accounts differ only by case: %q and %q", canonical, existing.Email)
		}
		canonical = existing.Email
	}
	if canonical != "" {
		account.Email = canonical
	}
	path := account.SourcePath(s)
	newChain := true
	if body, err := os.ReadFile(path); err == nil {
		var existing StoredCodexAccount
		if err := json.Unmarshal(body, &existing); err != nil {
			return fmt.Errorf("parse %s: %w", path, err)
		}
		newChain = codexRefreshToken(existing) != codexRefreshToken(account)
		if !strings.EqualFold(strings.TrimSpace(existing.Email), strings.TrimSpace(account.Email)) {
			return &StorageKeyCollisionError{
				Identifier:         account.Email,
				ExistingIdentifier: existing.Email,
			}
		}
		if existing.MigrationBatchID != account.MigrationBatchID {
			if account.MigrationBatchID != "" {
				return fmt.Errorf("account %q belongs to a different migration state", account.Email)
			}
			active, activeErr := s.migrationBatchActive(existing.MigrationBatchID)
			if activeErr != nil {
				return activeErr
			}
			if existing.MigrationBatchID == "" || !active {
				return fmt.Errorf("account %q belongs to a different migration state", account.Email)
			}
			account.MigrationBatchID = existing.MigrationBatchID
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if account.AddedAt == "" {
		account.AddedAt = time.Now().UTC().Format(time.RFC3339)
	}
	settleCodexHostClaim(&account, newChain)
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return err
	}
	body, err := json.MarshalIndent(account, "", "  ")
	if err != nil {
		return err
	}
	body = append(body, '\n')
	return writeFileAtomic(path, body, 0o600)
}

func validateStoredAccountIdentifier(identifier string) error {
	trimmed := strings.TrimSpace(identifier)
	if trimmed == "" {
		return errors.New("account email is required")
	}
	if strings.HasPrefix(emailToFilename(trimmed), ".") {
		return errors.New("account identifier cannot create a hidden store entry")
	}
	// Save, lock, and atomic-temp paths all decorate this component. Bounding
	// the undecorated filename leaves room below common 255-byte filesystem
	// limits for every decoration.
	if len(emailToFilename(trimmed)) > 220 {
		return errors.New("account identifier is too long for the credential store")
	}
	return nil
}

func (s CodexStore) FindStored(identifier string) (StoredCodexAccount, bool, error) {
	needle := strings.TrimSpace(identifier)
	if needle == "" {
		return StoredCodexAccount{}, false, nil
	}
	if account, ok, err := s.findStoredExact(needle); err != nil || ok {
		return account, ok, err
	}

	if !strings.HasPrefix(needle, "apikey:") && !strings.Contains(needle, "@") {
		if account, ok, err := s.findStoredExact("apikey:" + needle); err != nil || ok {
			return account, ok, err
		}
	}

	all, err := s.ListStored()
	if err != nil {
		return StoredCodexAccount{}, false, err
	}
	// sr list shows DisplayName ("email [plan]" or a label), so the exact
	// text it prints must select the account it names.
	var named []StoredCodexAccount
	for _, account := range all {
		if strings.EqualFold(strings.TrimSpace(account.DisplayName()), needle) {
			named = append(named, account)
		}
	}
	if len(named) == 1 {
		return named[0], true, nil
	}
	lower := strings.ToLower(needle)
	var matches []StoredCodexAccount
	for _, account := range all {
		if strings.Contains(strings.ToLower(account.Email), lower) || strings.Contains(strings.ToLower(account.LoginEmail()), lower) {
			matches = append(matches, account)
		}
	}
	if len(matches) == 0 {
		return StoredCodexAccount{}, false, nil
	}
	if len(matches) > 1 {
		names := make([]string, 0, len(matches))
		for _, match := range matches {
			names = append(names, match.Email)
		}
		return StoredCodexAccount{}, false, fmt.Errorf("multiple accounts match %q: %s", identifier, strings.Join(names, ", "))
	}
	return matches[0], true, nil
}

func (s CodexStore) findStoredExact(identifier string) (StoredCodexAccount, bool, error) {
	needle := strings.TrimSpace(identifier)
	if needle == "" {
		return StoredCodexAccount{}, false, nil
	}
	directPath := (StoredCodexAccount{Email: needle}).SourcePath(s)
	if body, err := os.ReadFile(directPath); err == nil {
		var account StoredCodexAccount
		if err := json.Unmarshal(body, &account); err != nil {
			return StoredCodexAccount{}, false, err
		}
		if strings.EqualFold(strings.TrimSpace(account.Email), needle) {
			if account.MigrationBatchID != "" {
				active, activeErr := s.migrationBatchActive(account.MigrationBatchID)
				if activeErr != nil {
					return StoredCodexAccount{}, false, activeErr
				}
				if !active {
					return StoredCodexAccount{}, false, nil
				}
			}
			return account, true, nil
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return StoredCodexAccount{}, false, err
	}

	return StoredCodexAccount{}, false, nil
}

// StageMigrationBatch persists credentials outside every routing and refresh
// path. A single activation marker later makes the complete batch visible.
func (s CodexStore) StageMigrationBatch(batchID string, staged []StoredCodexAccount) error {
	if err := validateMigrationBatchID(batchID); err != nil {
		return err
	}
	lock, err := s.lockStoredAccount(migrationBatchControlLockID)
	if err != nil {
		return err
	}
	defer lock.Close()
	if active, err := s.migrationBatchActive(batchID); err != nil {
		return err
	} else if active {
		return errors.New("migration batch is already active")
	}
	desired := make(map[string]bool, len(staged))
	accountIDs := make([]string, 0, len(staged))
	for i := range staged {
		if err := validateStoredAccountIdentifier(staged[i].Email); err != nil {
			return err
		}
		key := strings.ToLower(strings.TrimSpace(staged[i].Email))
		if desired[key] {
			return fmt.Errorf("duplicate migration account %q", staged[i].Email)
		}
		desired[key] = true
		staged[i].MigrationBatchID = batchID
		accountIDs = append(accountIDs, staged[i].Email)
	}
	all, err := s.listStored(true)
	if err != nil {
		return err
	}
	for _, account := range all {
		if account.MigrationBatchID == batchID {
			accountIDs = append(accountIDs, account.Email)
		}
	}
	accountLocks, err := s.lockStoredAccounts(accountIDs)
	if err != nil {
		return err
	}
	defer closeAccountFileLocks(accountLocks)
	for i := range staged {
		if err := s.saveStoredUnlocked(staged[i]); err != nil {
			return err
		}
	}
	all, err = s.listStored(true)
	if err != nil {
		return err
	}
	for _, account := range all {
		if account.MigrationBatchID != batchID || desired[strings.ToLower(strings.TrimSpace(account.Email))] {
			continue
		}
		if err := os.Remove(account.SourcePath(s)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

// ActivateMigrationBatch atomically publishes a previously staged batch.
func (s CodexStore) ActivateMigrationBatch(batchID string, expectedIDs []string) error {
	if err := validateMigrationBatchID(batchID); err != nil {
		return err
	}
	lock, err := s.lockStoredAccount(migrationBatchControlLockID)
	if err != nil {
		return err
	}
	defer lock.Close()
	all, err := s.listStored(true)
	if err != nil {
		return err
	}
	actual := make([]string, 0, len(expectedIDs))
	for _, account := range all {
		if account.MigrationBatchID == batchID {
			actual = append(actual, account.Email)
		}
	}
	sort.Strings(actual)
	expected := append([]string(nil), expectedIDs...)
	for i := range expected {
		expected[i] = strings.TrimSpace(expected[i])
		if err := validateStoredAccountIdentifier(expected[i]); err != nil {
			return err
		}
	}
	sort.Strings(expected)
	accountLocks, err := s.lockStoredAccounts(append(append([]string(nil), actual...), expected...))
	if err != nil {
		return err
	}
	defer closeAccountFileLocks(accountLocks)
	all, err = s.listStored(true)
	if err != nil {
		return err
	}
	actual = actual[:0]
	for _, account := range all {
		if account.MigrationBatchID == batchID {
			actual = append(actual, account.Email)
		}
	}
	sort.Strings(actual)
	if !slices.Equal(actual, expected) {
		return errors.New("migration batch account set does not match staged credentials")
	}
	body, err := json.Marshal(map[string]any{"accountIds": expected})
	if err != nil {
		return err
	}
	return writeFileAtomic(s.migrationBatchMarker(batchID), append(body, '\n'), 0o600)
}

// RollbackMigrationBatch deactivates and removes only credentials owned by the
// named batch. It is idempotent so callers can resolve an ambiguous activation.
func (s CodexStore) RollbackMigrationBatch(batchID string) error {
	if err := validateMigrationBatchID(batchID); err != nil {
		return err
	}
	lock, err := s.lockStoredAccount(migrationBatchControlLockID)
	if err != nil {
		return err
	}
	defer lock.Close()
	all, err := s.listStored(true)
	if err != nil {
		return err
	}
	accountIDs := make([]string, 0, len(all))
	for _, account := range all {
		if account.MigrationBatchID == batchID {
			accountIDs = append(accountIDs, account.Email)
		}
	}
	accountLocks, err := s.lockStoredAccounts(accountIDs)
	if err != nil {
		return err
	}
	defer closeAccountFileLocks(accountLocks)
	if err := os.Remove(s.migrationBatchMarker(batchID)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	all, err = s.listStored(true)
	if err != nil {
		return err
	}
	for _, account := range all {
		if account.MigrationBatchID != batchID {
			continue
		}
		if err := os.Remove(account.SourcePath(s)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

func validateMigrationBatchID(batchID string) error {
	if batchID == "" || len(batchID) > 160 {
		return errors.New("migration id is invalid")
	}
	for _, character := range batchID {
		if (character >= 'a' && character <= 'z') ||
			(character >= '0' && character <= '9') ||
			character == '-' || character == '_' || character == '.' {
			continue
		}
		return errors.New("migration id is invalid")
	}
	return nil
}

func (s CodexStore) lockStoredAccounts(identifiers []string) ([]*accountFileLock, error) {
	unique := make(map[string]struct{}, len(identifiers))
	canonical := make([]string, 0, len(identifiers))
	for _, identifier := range identifiers {
		key := strings.ToLower(strings.TrimSpace(identifier))
		if key == "" {
			continue
		}
		if _, exists := unique[key]; exists {
			continue
		}
		unique[key] = struct{}{}
		canonical = append(canonical, key)
	}
	sort.Strings(canonical)
	locks := make([]*accountFileLock, 0, len(canonical))
	for _, identifier := range canonical {
		lock, err := s.lockStoredAccount(identifier)
		if err != nil {
			closeAccountFileLocks(locks)
			return nil, err
		}
		locks = append(locks, lock)
	}
	return locks, nil
}

func closeAccountFileLocks(locks []*accountFileLock) {
	for i := len(locks) - 1; i >= 0; i-- {
		_ = locks[i].Close()
	}
}

func (s CodexStore) migrationBatchMarker(batchID string) string {
	return filepath.Join(s.Dir, ".migration-batches", batchID+".active.json")
}

func (s CodexStore) activeMigrationBatchSnapshot() (map[string]struct{}, error) {
	active := make(map[string]struct{})
	entries, err := os.ReadDir(filepath.Join(s.Dir, ".migration-batches"))
	if errors.Is(err, os.ErrNotExist) {
		return active, nil
	}
	if err != nil {
		return nil, err
	}
	const suffix = ".active.json"
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), suffix) {
			continue
		}
		batchID := strings.TrimSuffix(entry.Name(), suffix)
		if err := validateMigrationBatchID(batchID); err != nil {
			continue
		}
		active[batchID] = struct{}{}
	}
	return active, nil
}

func (s CodexStore) migrationBatchActive(batchID string) (bool, error) {
	if batchID == "" {
		return false, nil
	}
	if err := validateMigrationBatchID(batchID); err != nil {
		return false, err
	}
	_, err := os.Stat(s.migrationBatchMarker(batchID))
	if err == nil {
		return true, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return false, err
}

func (s CodexStore) RemoveStored(identifier string) (StoredCodexAccount, bool, error) {
	account, ok, err := s.FindStored(identifier)
	if err != nil || !ok {
		return account, ok, err
	}
	lock, err := s.lockStoredAccount(account.Email)
	if err != nil {
		return account, false, err
	}
	defer lock.Close()
	account, ok, err = s.findStoredExact(account.Email)
	if err != nil || !ok {
		return account, ok, err
	}
	if err := os.Remove(account.SourcePath(s)); err != nil {
		return account, false, err
	}
	return account, true, nil
}

// RemoveStoredExact holds the account's credential lock across the final
// identity check and unlink. A same-ID repair that wins before this lock is
// rejected; one that starts afterward cannot be unlinked by this deletion.
func (s CodexStore) RemoveStoredExact(expected StoredCodexAccount) (StoredCodexAccount, bool, error) {
	lock, err := s.lockStoredAccount(expected.Email)
	if err != nil {
		return StoredCodexAccount{}, false, err
	}
	defer lock.Close()
	current, found, err := s.findStoredExact(expected.Email)
	if err != nil || !found {
		return current, found, err
	}
	if !reflect.DeepEqual(current, expected) {
		return current, false, fmt.Errorf("stored account %q changed during removal", expected.Email)
	}
	if err := os.Remove(current.SourcePath(s)); err != nil {
		return current, false, err
	}
	return current, true, nil
}

const storedRemovalStageSuffix = ".delete-staged"

// RemoveStoredExactDurable stages the exact record out of the live namespace,
// syncs that rename, and then removes the staged secret. A crash after the
// rename is recoverable by ReconcileStoredRemovalStages and cannot resurrect a
// stale live account.
func (s CodexStore) RemoveStoredExactDurable(expected StoredCodexAccount, syncDir func(string) error) (StoredCodexAccount, bool, error) {
	lease, err := s.AcquireStoredAccountLease(expected.Email)
	if err != nil {
		return StoredCodexAccount{}, false, err
	}
	defer lease.Close()
	return lease.RemoveExactDurable(expected, syncDir)
}

// RemoveExactDurable performs an exact durable removal while retaining an
// already-held lease. This avoids releasing or recursively acquiring the
// account lock inside a composite provider transaction.
func (l *StoredAccountLease) RemoveExactDurable(expected StoredCodexAccount, syncDir func(string) error) (StoredCodexAccount, bool, error) {
	if err := l.validFor(expected.Email); err != nil {
		return StoredCodexAccount{}, false, err
	}
	current, found, err := l.store.findStoredExact(expected.Email)
	if err != nil || !found {
		return current, found, err
	}
	if !reflect.DeepEqual(current, expected) {
		return current, false, fmt.Errorf("stored account %q changed during removal", expected.Email)
	}
	path := current.SourcePath(l.store)
	staged := path + storedRemovalStageSuffix
	if err := os.Remove(staged); err != nil && !errors.Is(err, os.ErrNotExist) {
		return current, false, err
	}
	if err := os.Rename(path, staged); err != nil {
		return current, false, err
	}
	if err := syncDir(l.store.Dir); err != nil {
		renameErr := os.Rename(staged, path)
		if renameErr != nil {
			// The live record could not be restored, so the deletion remains
			// logically committed even though its directory publication failed.
			return current, true, errors.Join(err, renameErr)
		}
		// Once the reverse rename succeeds the record is live again. Report
		// removed=false even if publishing that restoration fails so a caller
		// coordinating another credential store restores its side as well.
		return current, false, errors.Join(err, syncDir(l.store.Dir))
	}
	if err := os.Remove(staged); err != nil {
		return current, true, err
	}
	// The first parent sync made the live-to-staged rename durable. Therefore a
	// failure syncing the final staged unlink cannot resurrect the live record:
	// either the unlink persists, or the staged record reappears and startup
	// reconciliation removes it. The deletion is committed in both cases.
	return current, true, syncDir(l.store.Dir)
}

// ReconcileStoredRemovalStages rolls forward exact deletions whose live record
// was already staged away before a process crash.
func (s CodexStore) ReconcileStoredRemovalStages(syncDir func(string) error) error {
	return s.reconcileStoredRemovalStages(syncDir, nil)
}

func (s CodexStore) reconcileStoredRemovalStages(syncDir func(string) error, beforeLease func(string)) error {
	entries, err := os.ReadDir(s.Dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json"+storedRemovalStageSuffix) {
			continue
		}
		path := filepath.Join(s.Dir, entry.Name())
		staged, found, err := readStoredRemovalStage(path, entry.Name())
		if err != nil {
			return err
		}
		if !found {
			continue
		}
		if beforeLease != nil {
			beforeLease(staged.Email)
		}
		lease, err := s.AcquireStoredAccountLease(staged.Email)
		if err != nil {
			return err
		}
		err = lease.reconcileRemovalStage(path, entry.Name(), staged, syncDir)
		closeErr := lease.Close()
		if err != nil || closeErr != nil {
			return errors.Join(err, closeErr)
		}
	}
	return nil
}

func readStoredRemovalStage(path, name string) (StoredCodexAccount, bool, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return StoredCodexAccount{}, false, nil
	}
	if err != nil {
		return StoredCodexAccount{}, false, err
	}
	if !info.Mode().IsRegular() {
		return StoredCodexAccount{}, false, fmt.Errorf("stored removal stage %q is not a regular file", name)
	}
	body, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return StoredCodexAccount{}, false, nil
	}
	if err != nil {
		return StoredCodexAccount{}, false, err
	}
	var staged StoredCodexAccount
	if err := json.Unmarshal(body, &staged); err != nil {
		return StoredCodexAccount{}, false, fmt.Errorf("parse stored removal stage %q: %w", name, err)
	}
	if err := validateStoredAccountIdentifier(staged.Email); err != nil {
		return StoredCodexAccount{}, false, fmt.Errorf("stored removal stage %q has ambiguous identity: %w", name, err)
	}
	if name != emailToFilename(staged.Email)+storedRemovalStageSuffix && name != legacyEmailToFilename(staged.Email)+storedRemovalStageSuffix {
		return StoredCodexAccount{}, false, fmt.Errorf("stored removal stage %q does not match its account identity", name)
	}
	return staged, true, nil
}

func (l *StoredAccountLease) reconcileRemovalStage(path, name string, observed StoredCodexAccount, syncDir func(string) error) error {
	if err := l.validFor(observed.Email); err != nil {
		return err
	}
	staged, found, err := readStoredRemovalStage(path, name)
	if err != nil || !found {
		return err
	}
	if !reflect.DeepEqual(staged, observed) {
		return fmt.Errorf("stored removal stage %q changed while awaiting its account lease", name)
	}

	livePath := strings.TrimSuffix(path, storedRemovalStageSuffix)
	liveAccount, live, err := readStoredRemovalLive(livePath, strings.TrimSuffix(name, storedRemovalStageSuffix))
	if err != nil {
		return err
	}
	if live {
		if !strings.EqualFold(strings.TrimSpace(liveAccount.Email), strings.TrimSpace(staged.Email)) {
			return fmt.Errorf("stored removal stage %q collides with live account identity", name)
		}
		// A live record is an authoritative replacement (or a restored failed
		// deletion). The stage is stale, but only this identity's lease may
		// remove it.
	}
	if err := os.Remove(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	return syncDir(l.store.Dir)
}

func readStoredRemovalLive(path, name string) (StoredCodexAccount, bool, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return StoredCodexAccount{}, false, nil
	}
	if err != nil {
		return StoredCodexAccount{}, false, err
	}
	if !info.Mode().IsRegular() {
		return StoredCodexAccount{}, false, fmt.Errorf("stored account %q is not a regular file", name)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return StoredCodexAccount{}, false, err
	}
	var live StoredCodexAccount
	if err := json.Unmarshal(body, &live); err != nil {
		return StoredCodexAccount{}, false, fmt.Errorf("parse stored account %q: %w", name, err)
	}
	if err := validateStoredAccountIdentifier(live.Email); err != nil {
		return StoredCodexAccount{}, false, fmt.Errorf("stored account %q has ambiguous identity: %w", name, err)
	}
	if name != emailToFilename(live.Email) && name != legacyEmailToFilename(live.Email) {
		return StoredCodexAccount{}, false, fmt.Errorf("stored account %q does not match its account identity", name)
	}
	return live, true, nil
}

// MigratedDirName holds credentials this machine has handed to the team vault.
// The store does not scan it, so the local daemon stops refreshing them: the
// provider rotates refresh tokens on use, so a credential must have exactly one
// refresher or the two invalidate each other.
const MigratedDirName = "migrated"

// MigrateStoredAway moves a stored account out of the active store and into the
// migrated directory, keeping it as a rollback record the daemon will not touch.
// It returns the path the record now lives at.
func (s CodexStore) MigrateStoredAway(identifier string) (string, bool, error) {
	account, ok, err := s.FindStored(identifier)
	if err != nil || !ok {
		return "", ok, err
	}
	lock, err := s.lockStoredAccount(account.Email)
	if err != nil {
		return "", false, err
	}
	defer lock.Close()
	account, ok, err = s.findStoredExact(account.Email)
	if err != nil || !ok {
		return "", ok, err
	}
	name := filepath.Base(account.SourcePath(s))
	dest := filepath.Join(s.Dir, MigratedDirName)
	if err := os.MkdirAll(dest, 0o700); err != nil {
		return "", false, err
	}
	target := filepath.Join(dest, name)
	if err := os.Rename(filepath.Join(s.Dir, name), target); err != nil {
		return "", false, err
	}
	return target, true, nil
}

func emailToFilename(email string) string {
	if strings.Contains(email, "#") {
		// '%' never appeared in legacy filenames. Escaping '#' and '%' reserves
		// a namespace that cannot collide with underscore-normalized aliases.
		return url.PathEscape(email) + ".json"
	}
	return legacyEmailToFilename(email)
}

func legacyEmailToFilename(email string) string {
	var b strings.Builder
	for _, r := range email {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '.' || r == '_' || r == '@' || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	return b.String() + ".json"
}

func accountLockFilename(identifier string) string {
	// Keep locks compatible with an older worker while its connections drain.
	return legacyEmailToFilename(strings.ToLower(strings.TrimSpace(identifier)))
}

func writeFileAtomic(path string, body []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpPath)
		}
	}()

	if err := tmp.Chmod(perm); err != nil {
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
	if err := os.Rename(tmpPath, path); err != nil {
		return err
	}
	cleanup = false
	if dirFile, err := os.Open(dir); err == nil {
		_ = dirFile.Sync()
		_ = dirFile.Close()
	}
	return nil
}
