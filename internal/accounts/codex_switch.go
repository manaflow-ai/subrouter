package accounts

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

type rawCodexStoredAccount struct {
	Auth json.RawMessage `json:"auth"`
}

func DefaultCodexAuthPath() string {
	if codexHome := strings.TrimSpace(os.Getenv("CODEX_HOME")); codexHome != "" {
		return filepath.Join(codexHome, "auth.json")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".codex", "auth.json")
	}
	return filepath.Join(home, ".codex", "auth.json")
}

func (s CodexStore) SwitchActive(accountID string) error {
	_, err := s.SwitchActiveStored(accountID)
	return err
}

// SwitchActiveStored writes the latest stored credential to Codex and returns
// the exact stored account that was activated. Callers that mirror the active
// credential to compatible clients must use this returned snapshot rather than
// one read before SwitchActiveStored acquired the account lock.
func (s CodexStore) SwitchActiveStored(accountID string) (StoredCodexAccount, error) {
	stored, ok, err := s.FindStored(accountID)
	if err != nil {
		return StoredCodexAccount{}, err
	}
	if !ok {
		return StoredCodexAccount{}, fmt.Errorf("account %q not found", accountID)
	}
	lock, err := s.lockStoredAccount(stored.Email)
	if err != nil {
		return StoredCodexAccount{}, err
	}
	defer lock.Close()
	stored, ok, err = s.findStoredExact(stored.Email)
	if err != nil {
		return StoredCodexAccount{}, err
	}
	if !ok {
		return StoredCodexAccount{}, fmt.Errorf("account %q not found", accountID)
	}
	_, usable := stored.toAccount(stored.SourcePath(s))
	if !usable {
		return StoredCodexAccount{}, fmt.Errorf("account %q is not usable", accountID)
	}
	rawAuth, err := readRawAuth(stored.SourcePath(s))
	if err != nil {
		return StoredCodexAccount{}, err
	}

	previous := stored
	downgraded := !stored.IsAPIKey() &&
		(stored.OAuthCredentialOrigin == CodexOAuthOriginIsolatedServerLogin ||
			stored.OAuthCredentialOrigin == CodexOAuthOriginServerAttested)
	if downgraded {
		stored.OAuthCredentialOrigin = CodexOAuthOriginInteractiveImport
		appendCodexAuthBreadcrumb(
			context.Background(), s, &stored,
			"credential_exported_to_active", "account_manager", false,
			&previous, &stored, nil, nil,
		)
		if err := s.saveStoredUnlocked(stored); err != nil {
			return StoredCodexAccount{}, err
		}
	}
	if err := writeCodexActiveAuthLocked(DefaultCodexAuthPath(), rawAuth); err != nil {
		if downgraded {
			if rollbackErr := s.saveStoredUnlocked(previous); rollbackErr != nil {
				return StoredCodexAccount{}, errors.Join(err, fmt.Errorf("restore isolated credential provenance: %w", rollbackErr))
			}
		}
		return StoredCodexAccount{}, err
	}
	return stored, nil
}

func (s CodexStore) rawAuthFor(accountID string) (Account, json.RawMessage, error) {
	needle := strings.TrimSpace(accountID)
	if needle == "" {
		return Account{}, nil, errors.New("account id is required")
	}
	stored, ok, err := s.FindStored(needle)
	if err != nil {
		return Account{}, nil, err
	}
	if !ok {
		return Account{}, nil, fmt.Errorf("account %q not found", accountID)
	}
	source := stored.SourcePath(s)
	account, ok := stored.toAccount(source)
	if !ok {
		return Account{}, nil, fmt.Errorf("account %q is not usable", accountID)
	}
	rawAuth, err := readRawAuth(source)
	if err != nil {
		return Account{}, nil, err
	}
	return account, rawAuth, nil
}

func readRawAuth(path string) (json.RawMessage, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var stored rawCodexStoredAccount
	if err := json.Unmarshal(body, &stored); err != nil {
		return nil, err
	}
	if len(stored.Auth) == 0 {
		return nil, fmt.Errorf("%s has no auth", path)
	}
	return stored.Auth, nil
}

// activeCodexAuthWriteMu is the in-process half of the active-auth write lock.
var activeCodexAuthWriteMu sync.Mutex

func activeCodexAuthWriteLockPath() string {
	return DefaultCodexAuthPath() + ".write.lock"
}

// lockActiveCodexAuthWrite serializes every read-compare-write and write of
// ~/.codex/auth.json performed by subrouter (SwitchActiveStored,
// WriteActiveCodexAuth, and the post-refresh sync), within this process and
// across processes.
//
// It is deliberately not ActiveCodexAuthLock: that lock serializes
// interactive `sr add`/login flows and is held across a browser OAuth that can
// take minutes, which must not stall a background refresh holding an account
// lock.
//
// Lock order: a per-account lock (lockStoredAccount) may be held while taking
// this lock, never the reverse. This lock is a leaf: no other lock is acquired
// while it is held, so it cannot take part in an inversion.
func lockActiveCodexAuthWrite() (func(), error) {
	activeCodexAuthWriteMu.Lock()
	release, err := lockActiveCodexAuthWriteFile()
	if err != nil {
		activeCodexAuthWriteMu.Unlock()
		return nil, err
	}
	return func() {
		release()
		activeCodexAuthWriteMu.Unlock()
	}, nil
}

func writeCodexActiveAuthLocked(path string, rawAuth json.RawMessage) error {
	unlock, err := lockActiveCodexAuthWrite()
	if err != nil {
		return err
	}
	defer unlock()
	return writeCodexActiveAuth(path, rawAuth)
}

// writeCodexActiveAuth replaces auth.json atomically: the new content is
// written and fsynced to a unique temp file in the same directory, the current
// file is copied to auth.json.bak, and only then is the temp file renamed over
// auth.json and the directory fsynced. auth.json is never absent, and a failed
// write leaves the previous file in place. Callers hold lockActiveCodexAuthWrite.
func writeCodexActiveAuth(path string, rawAuth json.RawMessage) error {
	var auth CodexAuthFile
	if err := json.Unmarshal(rawAuth, &auth); err != nil {
		return err
	}
	payload := rawAuth
	if auth.AuthMode == "apikey" || auth.OpenAIAPIKey != "" {
		apiKeyPayload := map[string]string{
			"auth_mode":      "apikey",
			"OPENAI_API_KEY": auth.OpenAIAPIKey,
		}
		body, err := json.Marshal(apiKeyPayload)
		if err != nil {
			return err
		}
		payload = body
	}

	var formatted bytes.Buffer
	if err := json.Indent(&formatted, payload, "", "  "); err != nil {
		return err
	}
	formatted.WriteByte('\n')

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if previous, err := os.ReadFile(path); err == nil {
		if err := writeFileAtomic(path+".bak", previous, 0o600); err != nil {
			return fmt.Errorf("back up %s: %w", path, err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return writeFileAtomic(path, formatted.Bytes(), 0o600)
}
