package qwen

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"unicode"

	"github.com/manaflow-ai/subrouter/internal/fsutil"
)

const previousAccessTokenKey = "subrouter_previous_access_token"
const consoleRemovalStageSuffix = ".delete-staged"
const consoleRemovalJournalSuffix = ".delete-journal.json"
const consoleRemovalJournalVersion = 2

const (
	consoleCredentialSnapshotMaxDepth   = 64
	consoleCredentialSnapshotMaxEntries = 4096
	consoleCredentialSnapshotMaxBytes   = 64 << 20
)

var consoleCredentialLocks sync.Map

type consoleCredentialLock struct {
	mu   *sync.Mutex
	file *consoleCredentialFileLock
}

func lockConsoleCredential(root, accountID string) (*consoleCredentialLock, error) {
	key, err := filepath.Abs(ConsoleConfigDirIn(root, accountID))
	if err != nil {
		key = filepath.Clean(ConsoleConfigDirIn(root, accountID))
	}
	value, _ := consoleCredentialLocks.LoadOrStore(key, &sync.Mutex{})
	mu := value.(*sync.Mutex)
	mu.Lock()
	fileLock, err := lockConsoleCredentialFile(root, accountID)
	if err != nil {
		mu.Unlock()
		return nil, err
	}
	return &consoleCredentialLock{mu: mu, file: fileLock}, nil
}

func (l *consoleCredentialLock) Close() error {
	err := l.file.Close()
	l.mu.Unlock()
	return err
}

// PrepareConsoleLogin temporarily gives Bailian CLI the selected account's
// model key. Alibaba uses it to associate the browser login with the purchased
// Token Plan; FinishConsoleLogin removes it again after OAuth completes.
func PrepareConsoleLogin(accountID, apiKey, baseURL string) error {
	return PrepareConsoleLoginIn(DefaultConsoleRoot(), accountID, apiKey, baseURL)
}

func PrepareConsoleLoginIn(root, accountID, apiKey, baseURL string) error {
	lock, err := lockConsoleCredential(root, accountID)
	if err != nil {
		return err
	}
	defer lock.Close()
	if strings.TrimSpace(apiKey) == "" {
		return fmt.Errorf("Qwen Token Plan API key is missing")
	}
	config, err := readRawConsoleConfigIn(root, accountID)
	if err != nil {
		return err
	}
	// A successful CLI exit is not proof of a new browser authorization. Stage
	// the previous token separately so Finish cannot mistake it for a completed
	// callback, while a cancelled/failed flow can still restore working auth.
	if token, _ := config["access_token"].(string); strings.TrimSpace(token) != "" {
		config[previousAccessTokenKey] = token
	}
	delete(config, "access_token")
	config["api_key"] = strings.TrimSpace(apiKey)
	if strings.TrimSpace(baseURL) != "" {
		config["base_url"] = strings.TrimSpace(baseURL)
	}
	return writeRawConsoleConfigIn(root, accountID, config)
}

// FinishConsoleLogin removes the temporary model credential and verifies that
// the browser flow left a reusable console access token behind.
func FinishConsoleLogin(accountID string) error {
	return FinishConsoleLoginIn(DefaultConsoleRoot(), accountID)
}

func FinishConsoleLoginIn(root, accountID string) error {
	lock, err := lockConsoleCredential(root, accountID)
	if err != nil {
		return err
	}
	defer lock.Close()
	config, err := readRawConsoleConfigIn(root, accountID)
	if err != nil {
		return err
	}
	token, _ := config["access_token"].(string)
	if strings.TrimSpace(token) == "" {
		if err := stripTemporaryLoginKeyUnlockedIn(root, accountID); err != nil {
			return err
		}
		return fmt.Errorf("Alibaba browser authorization did not save a console access token")
	}
	delete(config, "api_key")
	delete(config, "base_url")
	delete(config, previousAccessTokenKey)
	if err := writeRawConsoleConfigIn(root, accountID, config); err != nil {
		return err
	}
	return nil
}

// StripTemporaryLoginKey is safe to call after a failed or interrupted login.
func StripTemporaryLoginKey(accountID string) error {
	return StripTemporaryLoginKeyIn(DefaultConsoleRoot(), accountID)
}

func StripTemporaryLoginKeyIn(root, accountID string) error {
	lock, err := lockConsoleCredential(root, accountID)
	if err != nil {
		return err
	}
	defer lock.Close()
	return stripTemporaryLoginKeyUnlockedIn(root, accountID)
}

func stripTemporaryLoginKeyUnlockedIn(root, accountID string) error {
	config, err := readExistingRawConsoleConfigIn(root, accountID)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	delete(config, "api_key")
	delete(config, "base_url")
	if previous, _ := config[previousAccessTokenKey].(string); strings.TrimSpace(previous) != "" {
		config["access_token"] = previous
	}
	delete(config, previousAccessTokenKey)
	return writeRawConsoleConfigIn(root, accountID, config)
}

type consoleMetadata struct {
	Account string `json:"account,omitempty"`
}

// ConsoleCredential is the minimal account-isolated state that may be copied
// to a selected Subrouter server. It intentionally excludes the model API key.
type ConsoleCredential struct {
	AccessToken        string `json:"access_token"`
	ConsoleRegion      string `json:"console_region,omitempty"`
	ConsoleSite        string `json:"console_site,omitempty"`
	ConsoleSwitchAgent *int64 `json:"console_switch_agent,omitempty"`
	Account            string `json:"account,omitempty"`
}

func ExportConsoleCredential(accountID string) (ConsoleCredential, error) {
	return ExportConsoleCredentialIn(DefaultConsoleRoot(), accountID)
}

func ExportConsoleCredentialIn(root, accountID string) (ConsoleCredential, error) {
	lock, err := lockConsoleCredential(root, accountID)
	if err != nil {
		return ConsoleCredential{}, err
	}
	defer lock.Close()
	return exportConsoleCredentialUnlockedIn(root, accountID)
}

func exportConsoleCredentialUnlockedIn(root, accountID string) (ConsoleCredential, error) {
	config, err := readConsoleConfigIn(root, accountID)
	if err != nil {
		return ConsoleCredential{}, err
	}
	if strings.TrimSpace(config.AccessToken) == "" {
		return ConsoleCredential{}, fmt.Errorf("Qwen Token Plan console credential is missing")
	}
	return ConsoleCredential{
		AccessToken:        config.AccessToken,
		ConsoleRegion:      config.ConsoleRegion,
		ConsoleSite:        config.ConsoleSite,
		ConsoleSwitchAgent: config.ConsoleSwitchAgent,
		Account:            ConsoleAccountIn(root, accountID),
	}, nil
}

func SaveConsoleCredential(accountID string, credential ConsoleCredential) error {
	return SaveConsoleCredentialIn(DefaultConsoleRoot(), accountID, credential)
}

func SaveConsoleCredentialIn(root, accountID string, credential ConsoleCredential) error {
	lock, err := lockConsoleCredential(root, accountID)
	if err != nil {
		return err
	}
	defer lock.Close()
	return saveConsoleCredentialUnlockedIn(root, accountID, credential)
}

func saveConsoleCredentialUnlockedIn(root, accountID string, credential ConsoleCredential) error {
	if strings.TrimSpace(credential.AccessToken) == "" {
		return fmt.Errorf("Qwen Token Plan console credential is missing")
	}
	config := map[string]any{
		"access_token": credential.AccessToken,
	}
	if credential.ConsoleRegion != "" {
		config["console_region"] = credential.ConsoleRegion
	}
	if credential.ConsoleSite != "" {
		config["console_site"] = credential.ConsoleSite
	}
	if credential.ConsoleSwitchAgent != nil {
		config["console_switch_agent"] = *credential.ConsoleSwitchAgent
	}
	if err := writeRawConsoleConfigIn(root, accountID, config); err != nil {
		return err
	}
	if err := setConsoleAccountUnlockedIn(root, accountID, credential.Account); err != nil {
		return err
	}
	return syncConsoleCredentialDurably(root, accountID)
}

// SetConsoleAccount stores a user-facing sign-in label separately from the
// Bailian-managed credential file, which does not expose the login email.
func SetConsoleAccount(accountID, label string) error {
	return SetConsoleAccountIn(DefaultConsoleRoot(), accountID, label)
}

func SetConsoleAccountIn(root, accountID, label string) error {
	lock, err := lockConsoleCredential(root, accountID)
	if err != nil {
		return err
	}
	defer lock.Close()
	return setConsoleAccountUnlockedIn(root, accountID, label)
}

func setConsoleAccountUnlockedIn(root, accountID, label string) error {
	label = strings.TrimSpace(label)
	if label == "" {
		err := os.Remove(filepath.Join(ConsoleConfigDirIn(root, accountID), "metadata.json"))
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if len(label) > 320 || strings.IndexFunc(label, unicode.IsControl) >= 0 {
		return fmt.Errorf("Qwen console account label contains invalid terminal text")
	}
	dir := ConsoleConfigDirIn(root, accountID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	body, err := json.MarshalIndent(consoleMetadata{Account: label}, "", "  ")
	if err != nil {
		return err
	}
	body = append(body, '\n')
	path := filepath.Join(dir, "metadata.json")
	if err := os.WriteFile(path, body, 0o600); err != nil {
		return err
	}
	return os.Chmod(path, 0o600)
}

func ConsoleAccount(accountID string) string {
	return ConsoleAccountIn(DefaultConsoleRoot(), accountID)
}

func ConsoleAccountIn(root, accountID string) string {
	body, err := os.ReadFile(filepath.Join(ConsoleConfigDirIn(root, accountID), "metadata.json"))
	if err != nil {
		return ""
	}
	var metadata consoleMetadata
	if json.Unmarshal(body, &metadata) != nil {
		return ""
	}
	return strings.TrimSpace(metadata.Account)
}

func readRawConsoleConfig(accountID string) (map[string]any, error) {
	return readRawConsoleConfigIn(DefaultConsoleRoot(), accountID)
}

func readRawConsoleConfigIn(root, accountID string) (map[string]any, error) {
	config, err := readExistingRawConsoleConfigIn(root, accountID)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]any{}, nil
	}
	return config, err
}

func readExistingRawConsoleConfigIn(root, accountID string) (map[string]any, error) {
	body, err := os.ReadFile(ConsoleConfigPathIn(root, accountID))
	if err != nil {
		return nil, err
	}
	var config map[string]any
	if err := json.Unmarshal(body, &config); err != nil {
		return nil, fmt.Errorf("parse Qwen console credential: %w", err)
	}
	return config, nil
}

func writeRawConsoleConfig(accountID string, config map[string]any) error {
	return writeRawConsoleConfigIn(DefaultConsoleRoot(), accountID, config)
}

func writeRawConsoleConfigIn(root, accountID string, config map[string]any) error {
	dir := ConsoleConfigDirIn(root, accountID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return err
	}
	body, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return err
	}
	body = append(body, '\n')
	tmp, err := os.CreateTemp(dir, ".config-*.json")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()
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
	return os.Rename(tmpPath, filepath.Join(dir, "config.json"))
}

func RemoveConsoleCredential(accountID string) error {
	return RemoveConsoleCredentialIn(DefaultConsoleRoot(), accountID)
}

func RemoveConsoleCredentialIn(root, accountID string) error {
	lock, err := lockConsoleCredential(root, accountID)
	if err != nil {
		return err
	}
	defer lock.Close()
	return removeConsoleCredentialUnlockedIn(root, accountID)
}

func removeConsoleCredentialUnlockedIn(root, accountID string) error {
	dir := ConsoleConfigDirIn(root, accountID)
	info, err := os.Lstat(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("Qwen console profile is not a safe directory")
	}
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	return syncConsoleDirectory(filepath.Dir(dir))
}

func syncConsoleCredentialDurably(root, accountID string) error {
	dir := ConsoleConfigDirIn(root, accountID)
	for _, name := range []string{"config.json", "metadata.json"} {
		file, err := os.Open(filepath.Join(dir, name))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if err := file.Sync(); err != nil {
			_ = file.Close()
			return err
		}
		if err := file.Close(); err != nil {
			return err
		}
	}
	return errors.Join(syncConsoleDirectory(dir), syncConsoleDirectory(filepath.Dir(dir)))
}

var syncConsoleDirectory = defaultSyncConsoleDirectory

func defaultSyncConsoleDirectory(path string) error {
	return syncConsoleDirectoryForOS(runtime.GOOS, path, os.Open)
}

func syncConsoleDirectoryForOS(goos, path string, openDir func(string) (*os.File, error)) error {
	if goos == "windows" {
		return nil
	}
	dir, err := openDir(path)
	if err != nil {
		return err
	}
	return errors.Join(dir.Sync(), dir.Close())
}

// ConsoleCredentialVersionIn returns a non-secret version of the complete
// console config under the same per-account lock used by save and removal.
func ConsoleCredentialVersionIn(root, accountID string) (found bool, version string, err error) {
	lock, err := lockConsoleCredential(root, accountID)
	if err != nil {
		return false, "", err
	}
	defer lock.Close()
	return consoleCredentialVersionUnlockedIn(root, accountID)
}

func consoleCredentialVersionUnlockedIn(root, accountID string) (bool, string, error) {
	liveDir := ConsoleConfigDirIn(root, accountID)
	stagedDir := liveDir + consoleRemovalStageSuffix
	liveFound, liveVersion, err := consoleCredentialVersionInDir(liveDir)
	if err != nil {
		return false, "", err
	}
	stagedFound, stagedVersion, err := consoleCredentialVersionInDir(stagedDir)
	if err != nil {
		return false, "", err
	}
	if liveFound && stagedFound {
		return false, "", fmt.Errorf("Qwen console credential has conflicting live and staged profiles")
	}
	if liveFound {
		return true, liveVersion, nil
	}
	return stagedFound, stagedVersion, nil
}

func consoleCredentialVersionInDir(dir string) (bool, string, error) {
	info, statErr := os.Lstat(dir)
	if errors.Is(statErr, os.ErrNotExist) {
		return false, "", nil
	}
	if statErr != nil {
		return false, "", statErr
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return false, "", fmt.Errorf("Qwen console profile is not a safe directory")
	}

	// Exact removal renames and ultimately deletes this whole directory, not
	// merely the two files Subrouter currently writes. Fingerprint every entry
	// so an unrecognized present or replacement file cannot be deleted under a
	// version that never observed it. The journal stores only this digest.
	hasher := sha256.New()
	_, _ = hasher.Write([]byte("subrouter-qwen-console-directory-v1\x00"))
	snapshot := consoleCredentialSnapshot{hasher: hasher}
	if err := snapshot.walk(dir, "", 0); err != nil {
		return false, "", err
	}
	return true, hex.EncodeToString(hasher.Sum(nil)), nil
}

type consoleCredentialSnapshot struct {
	hasher  hash.Hash
	entries int
	bytes   int64
}

func (s *consoleCredentialSnapshot) walk(dir, relativeDir string, depth int) error {
	if depth > consoleCredentialSnapshotMaxDepth {
		return fmt.Errorf("Qwen console profile exceeds the maximum directory depth")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		s.entries++
		if s.entries > consoleCredentialSnapshotMaxEntries {
			return fmt.Errorf("Qwen console profile has too many entries")
		}
		relativePath := entry.Name()
		if relativeDir != "" {
			relativePath = filepath.Join(relativeDir, entry.Name())
		}
		fullPath := filepath.Join(dir, entry.Name())
		info, err := os.Lstat(fullPath)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("Qwen console profile contains an unsafe symbolic link: %s", filepath.ToSlash(relativePath))
		}
		switch {
		case info.IsDir():
			s.writeFrame('d', filepath.ToSlash(relativePath), nil)
			if err := s.walk(fullPath, relativePath, depth+1); err != nil {
				return err
			}
		case info.Mode().IsRegular():
			if info.Size() < 0 || info.Size() > consoleCredentialSnapshotMaxBytes-s.bytes {
				return fmt.Errorf("Qwen console profile is too large to snapshot safely")
			}
			body, err := os.ReadFile(fullPath)
			if err != nil {
				return err
			}
			after, err := os.Lstat(fullPath)
			if err != nil {
				return err
			}
			if after.Mode()&os.ModeSymlink != 0 || !after.Mode().IsRegular() || !os.SameFile(info, after) || int64(len(body)) != after.Size() {
				return fmt.Errorf("Qwen console profile changed while it was being snapshotted: %s", filepath.ToSlash(relativePath))
			}
			s.bytes += int64(len(body))
			s.writeFrame('f', filepath.ToSlash(relativePath), body)
		default:
			return fmt.Errorf("Qwen console profile contains an unsafe special file: %s", filepath.ToSlash(relativePath))
		}
	}
	return nil
}

func (s *consoleCredentialSnapshot) writeFrame(kind byte, path string, body []byte) {
	var length [8]byte
	_, _ = s.hasher.Write([]byte{kind})
	binary.BigEndian.PutUint64(length[:], uint64(len(path)))
	_, _ = s.hasher.Write(length[:])
	_, _ = s.hasher.Write([]byte(path))
	binary.BigEndian.PutUint64(length[:], uint64(len(body)))
	_, _ = s.hasher.Write(length[:])
	_, _ = s.hasher.Write(body)
}

type consoleRemovalJournal struct {
	Version           int    `json:"version"`
	AccountID         string `json:"account_id"`
	CredentialFound   bool   `json:"credential_found"`
	CredentialVersion string `json:"credential_version"`
}

func consoleRemovalJournalPathIn(root, accountID string) string {
	return filepath.Join(root, safeFilename(accountID)+consoleRemovalJournalSuffix)
}

func writeConsoleRemovalJournalUnlockedIn(root, accountID string, credentialFound bool, credentialVersion string) (err error) {
	if strings.TrimSpace(accountID) == "" || credentialFound != (strings.TrimSpace(credentialVersion) != "") {
		return errors.New("Qwen console removal journal identity is incomplete")
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return err
	}
	body, err := json.Marshal(consoleRemovalJournal{
		Version: consoleRemovalJournalVersion, AccountID: accountID,
		CredentialFound: credentialFound, CredentialVersion: credentialVersion,
	})
	if err != nil {
		return err
	}
	body = append(body, '\n')
	file, err := os.CreateTemp(root, ".qwen-console-delete-*")
	if err != nil {
		return err
	}
	tempPath := file.Name()
	defer func() {
		if file != nil {
			err = errors.Join(err, file.Close())
		}
		if err != nil {
			_ = os.Remove(tempPath)
		}
	}()
	if err := file.Chmod(0o600); err != nil {
		return err
	}
	if _, err := file.Write(body); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	file = nil
	if err := os.Rename(tempPath, consoleRemovalJournalPathIn(root, accountID)); err != nil {
		return err
	}
	return syncConsoleDirectory(root)
}

func readConsoleRemovalJournal(path string) (consoleRemovalJournal, error) {
	var journal consoleRemovalJournal
	body, err := os.ReadFile(path)
	if err != nil {
		return journal, err
	}
	if err := json.Unmarshal(body, &journal); err != nil {
		return journal, fmt.Errorf("decode Qwen console removal journal: %w", err)
	}
	if strings.TrimSpace(journal.AccountID) == "" {
		return journal, errors.New("Qwen console removal journal is invalid")
	}
	switch journal.Version {
	case 1:
		// Version 1 journals predate exact-absence tracking and were only
		// written for a present credential.
		journal.CredentialFound = true
		if strings.TrimSpace(journal.CredentialVersion) == "" {
			return journal, errors.New("Qwen console removal journal is invalid")
		}
	case consoleRemovalJournalVersion:
		if journal.CredentialFound != (strings.TrimSpace(journal.CredentialVersion) != "") {
			return journal, errors.New("Qwen console removal journal is invalid")
		}
	default:
		return journal, errors.New("Qwen console removal journal is invalid")
	}
	if journal.CredentialFound {
		decoded, decodeErr := hex.DecodeString(journal.CredentialVersion)
		if decodeErr != nil || len(decoded) != sha256.Size || strings.ToLower(journal.CredentialVersion) != journal.CredentialVersion {
			return journal, errors.New("Qwen console removal journal credential version is invalid")
		}
	}
	if filepath.Base(path) != safeFilename(journal.AccountID)+consoleRemovalJournalSuffix {
		return journal, errors.New("Qwen console removal journal identity does not match its path")
	}
	return journal, nil
}

func clearConsoleRemovalJournalUnlockedIn(root, accountID string) error {
	err := os.Remove(consoleRemovalJournalPathIn(root, accountID))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return syncConsoleDirectory(root)
}

// ReconcileConsoleCredentialRemovalsIn resolves an interrupted exact removal
// using the model-account store as the authority: a still-live model account
// restores its exact staged console profile, while an absent model account
// completes deletion. Journals contain identifiers and hashes, never secrets.
func ReconcileConsoleCredentialRemovalsIn(root string, modelAccountExists func(string) (bool, error)) error {
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), consoleRemovalJournalSuffix) {
			continue
		}
		if entry.Type()&os.ModeSymlink != 0 || !entry.Type().IsRegular() {
			return errors.New("Qwen console removal journal is not a safe regular file")
		}
		journalPath := filepath.Join(root, entry.Name())
		journal, err := readConsoleRemovalJournal(journalPath)
		if err != nil {
			return err
		}
		lock, err := lockConsoleCredential(root, journal.AccountID)
		if err != nil {
			return err
		}
		reconcileErr := func() error {
			journal, err = readConsoleRemovalJournal(journalPath)
			if err != nil {
				return err
			}
			liveDir := ConsoleConfigDirIn(root, journal.AccountID)
			stagedDir := liveDir + consoleRemovalStageSuffix
			liveFound, _, err := consoleCredentialVersionInDir(liveDir)
			if err != nil {
				return err
			}
			stagedFound, stagedVersion, err := consoleCredentialVersionInDir(stagedDir)
			if err != nil {
				return err
			}
			modelFound, err := modelAccountExists(journal.AccountID)
			if err != nil {
				return err
			}
			if !journal.CredentialFound {
				if stagedFound {
					return errors.New("Qwen absence removal journal has an unexpected staged console profile")
				}
				if modelFound {
					// Model deletion did not commit. Any live profile is a repair
					// created after the absent snapshot and must be preserved.
					return clearConsoleRemovalJournalUnlockedIn(root, journal.AccountID)
				}
				if liveFound {
					return errors.New("Qwen absence removal journal has a live console replacement after model removal")
				}
				return clearConsoleRemovalJournalUnlockedIn(root, journal.AccountID)
			}
			if stagedFound && stagedVersion != journal.CredentialVersion {
				return errors.New("Qwen staged console credential does not match its removal journal")
			}
			if liveFound && stagedFound {
				if !modelFound {
					// Model deletion committed, but a non-cooperating writer
					// installed a live replacement before cleanup. Preserve both
					// profiles and the journal so the split state remains visible
					// and fail closed instead of orphaning the live credential.
					return errors.New("Qwen removal journal has a live console replacement after model removal")
				}
				// A repair installed a replacement after the crash. Preserve it and
				// clean only the exact old staged target named by the journal. The
				// model account still exists, so this cannot hide committed deletion.
				if err := os.RemoveAll(stagedDir); err != nil {
					return err
				}
				if err := syncConsoleDirectory(root); err != nil {
					return err
				}
				return clearConsoleRemovalJournalUnlockedIn(root, journal.AccountID)
			}
			if liveFound {
				// A crash before staging, or a completed rollback. A changed live
				// version is a replacement and is preserved by abandoning the journal.
				if !modelFound {
					return errors.New("Qwen removal journal has a live console profile but no model account")
				}
				return clearConsoleRemovalJournalUnlockedIn(root, journal.AccountID)
			}
			if stagedFound {
				if modelFound {
					if err := fsutil.RenameNoReplace(stagedDir, liveDir); err != nil {
						return err
					}
				} else if err := os.RemoveAll(stagedDir); err != nil {
					return err
				}
				if err := syncConsoleDirectory(root); err != nil {
					return err
				}
				return clearConsoleRemovalJournalUnlockedIn(root, journal.AccountID)
			}
			if modelFound {
				return errors.New("Qwen removal journal lost its console credential while the model account remains")
			}
			return clearConsoleRemovalJournalUnlockedIn(root, journal.AccountID)
		}()
		closeErr := lock.Close()
		if reconcileErr != nil || closeErr != nil {
			return errors.Join(reconcileErr, closeErr)
		}
	}
	return nil
}

// RemoveConsoleCredentialExactIn holds the console repair lock across final
// version validation, console staging, and the caller's exact model-account
// deletion. The whole console directory remains as a durable staged record
// until model deletion commits. A retry after a process crash resumes that
// exact stage; a failed model deletion renames it back into the live namespace.
func RemoveConsoleCredentialExactIn(
	root, accountID string,
	expectedFound bool,
	expectedVersion string,
	removeAccount func() (bool, error),
) (removed bool, err error) {
	lock, err := lockConsoleCredential(root, accountID)
	if err != nil {
		return false, err
	}
	defer func() { err = errors.Join(err, lock.Close()) }()
	liveDir := ConsoleConfigDirIn(root, accountID)
	stagedDir := liveDir + consoleRemovalStageSuffix
	liveFound, liveVersion, err := consoleCredentialVersionInDir(liveDir)
	if err != nil {
		return false, err
	}
	stagedFound, stagedVersion, err := consoleCredentialVersionInDir(stagedDir)
	if err != nil {
		return false, err
	}
	if liveFound && stagedFound {
		return false, fmt.Errorf("Qwen console credential has conflicting live and staged profiles")
	}
	if liveFound {
		if !expectedFound || liveVersion != expectedVersion {
			return false, fmt.Errorf("Qwen console credential changed during removal")
		}
		if err := writeConsoleRemovalJournalUnlockedIn(root, accountID, true, expectedVersion); err != nil {
			return false, err
		}
		if err := os.Rename(liveDir, stagedDir); err != nil {
			return false, errors.Join(err, clearConsoleRemovalJournalUnlockedIn(root, accountID))
		}
		if syncErr := syncConsoleDirectory(filepath.Dir(liveDir)); syncErr != nil {
			renameErr := fsutil.RenameNoReplace(stagedDir, liveDir)
			if renameErr != nil {
				// The model callback has not run. Keep the journal so startup
				// restores the staged console profile while the model account lives.
				return false, errors.Join(syncErr, renameErr)
			}
			restoreSyncErr := syncConsoleDirectory(filepath.Dir(liveDir))
			if restoreSyncErr != nil {
				return false, errors.Join(syncErr, restoreSyncErr)
			}
			return false, errors.Join(syncErr, clearConsoleRemovalJournalUnlockedIn(root, accountID))
		}
		// The console CLI does not participate in Subrouter's per-account lock.
		// Revalidate after the durable rename so a concurrent writer cannot add
		// or change staged content and then have it deleted after model removal.
		stagedFound, stagedVersion, err = consoleCredentialVersionInDir(stagedDir)
		if err != nil || !stagedFound || stagedVersion != expectedVersion {
			changedErr := errors.New("Qwen console credential changed during removal")
			if err != nil {
				changedErr = errors.Join(changedErr, err)
			}
			if _, statErr := os.Lstat(liveDir); statErr == nil {
				// Preserve both the newly appeared live profile and the staged
				// contents. The journal remains as a fail-closed recovery marker.
				return false, errors.Join(changedErr, errors.New("Qwen console credential replacement appeared during staging"))
			} else if !errors.Is(statErr, os.ErrNotExist) {
				return false, errors.Join(changedErr, statErr)
			}
			if renameErr := fsutil.RenameNoReplace(stagedDir, liveDir); renameErr != nil {
				// A missing or concurrently replaced stage cannot be restored
				// safely. Keep the journal so startup fails closed.
				return false, errors.Join(changedErr, renameErr)
			}
			if syncErr := syncConsoleDirectory(filepath.Dir(liveDir)); syncErr != nil {
				return false, errors.Join(changedErr, syncErr)
			}
			return false, errors.Join(changedErr, clearConsoleRemovalJournalUnlockedIn(root, accountID))
		}
	} else if stagedFound {
		if !expectedFound || stagedVersion != expectedVersion {
			return false, fmt.Errorf("Qwen console credential changed during removal")
		}
		journal, journalErr := readConsoleRemovalJournal(consoleRemovalJournalPathIn(root, accountID))
		if journalErr != nil || !journal.CredentialFound || journal.CredentialVersion != expectedVersion {
			return false, errors.Join(errors.New("Qwen console removal stage has no matching journal"), journalErr)
		}
	} else if expectedFound {
		return false, fmt.Errorf("Qwen console credential changed during removal")
	} else {
		// Exact absence is state too. Publish it before model deletion so a
		// crash cannot orphan a console profile created by an external CLI
		// writer during the callback.
		if err := writeConsoleRemovalJournalUnlockedIn(root, accountID, false, ""); err != nil {
			return false, err
		}
		liveFound, _, liveErr := consoleCredentialVersionInDir(liveDir)
		stagedFound, _, stagedErr := consoleCredentialVersionInDir(stagedDir)
		if liveErr != nil || stagedErr != nil || liveFound || stagedFound {
			changedErr := errors.New("Qwen console credential changed during removal")
			return false, errors.Join(changedErr, liveErr, stagedErr, clearConsoleRemovalJournalUnlockedIn(root, accountID))
		}
	}
	removed, removeErr := removeAccount()
	if removed {
		if _, statErr := os.Lstat(liveDir); statErr == nil {
			return true, errors.Join(removeErr, errors.New("Qwen console credential replacement appeared after model removal"))
		} else if !errors.Is(statErr, os.ErrNotExist) {
			return true, errors.Join(removeErr, statErr)
		}
		if stagedFound {
			cleanupFound, cleanupVersion, snapshotErr := consoleCredentialVersionInDir(stagedDir)
			if snapshotErr != nil || !cleanupFound || cleanupVersion != expectedVersion {
				exactnessErr := errors.New("Qwen staged console credential changed after model removal")
				// Model deletion has committed, so restoring this profile into the
				// live namespace would resurrect a split account. Preserve the stage
				// and journal for fail-closed reconciliation instead.
				return true, errors.Join(removeErr, exactnessErr, snapshotErr)
			}
			cleanupErr := os.RemoveAll(stagedDir)
			if cleanupErr == nil {
				cleanupErr = syncConsoleDirectory(filepath.Dir(stagedDir))
			}
			removeErr = errors.Join(removeErr, cleanupErr)
		}
		if removeErr == nil {
			removeErr = clearConsoleRemovalJournalUnlockedIn(root, accountID)
		}
		return true, removeErr
	}
	if removeErr == nil {
		removeErr = errors.New("Qwen account changed during removal")
	}
	restoredDurably := !stagedFound
	if stagedFound {
		if _, statErr := os.Lstat(liveDir); statErr == nil {
			removeErr = errors.Join(removeErr, errors.New("Qwen console credential replacement appeared during rollback"))
		} else if !errors.Is(statErr, os.ErrNotExist) {
			removeErr = errors.Join(removeErr, statErr)
		} else if renameErr := fsutil.RenameNoReplace(stagedDir, liveDir); renameErr != nil {
			removeErr = errors.Join(removeErr, renameErr)
		} else {
			syncErr := syncConsoleDirectory(filepath.Dir(liveDir))
			removeErr = errors.Join(removeErr, syncErr)
			restoredDurably = syncErr == nil
		}
	}
	if restoredDurably {
		removeErr = errors.Join(removeErr, clearConsoleRemovalJournalUnlockedIn(root, accountID))
	}
	return false, removeErr
}
