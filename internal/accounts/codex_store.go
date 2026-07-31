package accounts

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/manaflow-ai/subrouter/internal/storepath"
)

type CodexStore struct {
	Dir string
}

type StoredCodexAccount struct {
	Email         string                `json:"email"`
	Provider      Provider              `json:"provider,omitempty"`
	AddedAt       string                `json:"addedAt"`
	Auth          CodexAuthFile         `json:"auth"`
	ProjectID     string                `json:"projectId,omitempty"`
	ProjectName   string                `json:"projectName,omitempty"`
	AdminKeyLabel string                `json:"adminKeyLabel,omitempty"`
	Breadcrumbs   []CodexAuthBreadcrumb `json:"breadcrumbs,omitempty"`
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
	out := Account{
		ID:       id,
		Provider: a.ProviderOrDefault(),
		Label:    id,
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
	if account.AddedAt == "" {
		account.AddedAt = time.Now().UTC().Format(time.RFC3339)
	}
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return err
	}
	body, err := json.MarshalIndent(account, "", "  ")
	if err != nil {
		return err
	}
	body = append(body, '\n')
	return writeFileAtomic(filepath.Join(s.Dir, emailToFilename(account.Email)), body, 0o600)
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
	directPath := filepath.Join(s.Dir, emailToFilename(needle))
	if body, err := os.ReadFile(directPath); err == nil {
		var account StoredCodexAccount
		if err := json.Unmarshal(body, &account); err != nil {
			return StoredCodexAccount{}, false, err
		}
		return account, true, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return StoredCodexAccount{}, false, err
	}

	if !strings.HasPrefix(needle, "apikey:") && !strings.Contains(needle, "@") {
		if account, ok, err := s.FindStored("apikey:" + needle); err != nil || ok {
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

func (s CodexStore) RemoveStored(identifier string) (StoredCodexAccount, bool, error) {
	account, ok, err := s.FindStored(identifier)
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
