package accounts

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/manaflow-ai/subrouter/internal/storepath"
)

type CodexStore struct {
	Dir string
}

const migrationBatchControlLockID = ".subrouter-migration-batch-control"

type StorageKeyCollisionError struct {
	Identifier         string
	ExistingIdentifier string
}

func (e *StorageKeyCollisionError) Error() string {
	return fmt.Sprintf("account identifier %q collides with stored account %q", e.Identifier, e.ExistingIdentifier)
}

type StoredCodexAccount struct {
	Email            string                `json:"email"`
	Label            string                `json:"label,omitempty"`
	Provider         Provider              `json:"provider,omitempty"`
	MigrationBatchID string                `json:"migrationBatchId,omitempty"`
	AddedAt          string                `json:"addedAt"`
	Owner            *OwnerClaim           `json:"owner,omitempty"`
	Auth             CodexAuthFile         `json:"auth"`
	ProjectID        string                `json:"projectId,omitempty"`
	ProjectName      string                `json:"projectName,omitempty"`
	AdminKeyLabel    string                `json:"adminKeyLabel,omitempty"`
	Breadcrumbs      []CodexAuthBreadcrumb `json:"breadcrumbs,omitempty"`
}

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

func (a StoredCodexAccount) toAccount(source string) (Account, bool) {
	id := strings.TrimSpace(a.Email)
	if id == "" {
		return Account{}, false
	}

	addedAt, _ := time.Parse(time.RFC3339, a.AddedAt)
	label := strings.TrimSpace(a.Label)
	if label == "" {
		label = id
	}
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
	out.AccountID = a.Auth.Tokens.AccountID
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
		if canonical != "" && canonical != existing.Email {
			return fmt.Errorf("multiple stored accounts differ only by case: %q and %q", canonical, existing.Email)
		}
		canonical = existing.Email
	}
	if canonical != "" {
		account.Email = canonical
	}
	path := filepath.Join(s.Dir, emailToFilename(account.Email))
	if body, err := os.ReadFile(path); err == nil {
		var existing StoredCodexAccount
		if err := json.Unmarshal(body, &existing); err != nil {
			return fmt.Errorf("parse %s: %w", path, err)
		}
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
	// Stamp only unclaimed OAuth files. Never silently overwrite a foreign
	// owner claim on ordinary save — that requires an explicit takeover (#129).
	if !account.IsAPIKey() && (account.Owner == nil || strings.TrimSpace(account.Owner.Host) == "") {
		claim := StampOwnerClaim(nil, false)
		account.Owner = &claim
	}
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
	lower := strings.ToLower(needle)
	var matches []StoredCodexAccount
	for _, account := range all {
		if strings.Contains(strings.ToLower(account.Email), lower) {
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
	directPath := filepath.Join(s.Dir, emailToFilename(needle))
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
	if err := os.Remove(filepath.Join(s.Dir, emailToFilename(account.Email))); err != nil {
		return account, false, err
	}
	return account, true, nil
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
	name := emailToFilename(account.Email)
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

// TakeoverStoredOwner bumps the owner-claim epoch onto this host so a copied
// account file can refresh again after an explicit cutover (#129).
func (s CodexStore) TakeoverStoredOwner(identifier string) (StoredCodexAccount, error) {
	account, ok, err := s.FindStored(identifier)
	if err != nil {
		return StoredCodexAccount{}, err
	}
	if !ok {
		return StoredCodexAccount{}, fmt.Errorf("account %q not found", identifier)
	}
	if account.IsAPIKey() {
		return account, nil
	}
	claim := TakeoverOwnerClaim(account.Owner)
	account.Owner = &claim
	if err := s.SaveStored(account); err != nil {
		return StoredCodexAccount{}, err
	}
	return account, nil
}

func emailToFilename(email string) string {
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
	return emailToFilename(strings.ToLower(strings.TrimSpace(identifier)))
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
