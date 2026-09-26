//go:build darwin

package antigravity

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

const keychainWriteTimeout = 10 * time.Second

const nativeProfileLockPoll = 50 * time.Millisecond

type nativeProfileJournal struct {
	Account      string `json:"account,omitempty"`
	OriginalBlob []byte `json:"original_blob,omitempty"`
	HadOriginal  bool   `json:"had_original"`
}

func acquireNativeProfile(ctx context.Context, lockPath string, credential CredentialInfo) (*NativeProfileLease, error) {
	return acquireNativeProfileWith(ctx, lockPath, credential, readLocalKeychainEntry, writeLocalKeychainEntry, deleteLocalKeychainEntry)
}

type keychainReader func(context.Context) (KeychainEntry, bool, error)
type keychainWriter func(context.Context, KeychainEntry) error
type keychainDeleter func(context.Context, string) error

func acquireNativeProfileWith(ctx context.Context, lockPath string, credential CredentialInfo, read keychainReader, write keychainWriter, deleteEntry keychainDeleter) (*NativeProfileLease, error) {
	if strings.TrimSpace(lockPath) == "" {
		return nil, errors.New("Antigravity native profile lock path is required")
	}
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o700); err != nil {
		return nil, fmt.Errorf("create Antigravity profile lock directory: %w", err)
	}
	lock, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open Antigravity profile lock: %w", err)
	}
	for {
		err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			_ = lock.Close()
			return nil, fmt.Errorf("lock Antigravity profile slot: %w", err)
		}
		select {
		case <-ctx.Done():
			_ = lock.Close()
			return nil, ctx.Err()
		case <-time.After(nativeProfileLockPoll):
		}
	}
	if err := recoverNativeProfileLocked(ctx, lockPath, write, deleteEntry); err != nil {
		_ = unlockAndClose(lock)
		return nil, err
	}
	original, hadOriginal, err := read(ctx)
	if err != nil {
		_ = unlockAndClose(lock)
		return nil, err
	}
	blob, err := EncodeCredential(credential)
	if err != nil {
		_ = unlockAndClose(lock)
		return nil, err
	}
	target := original
	if !hadOriginal || strings.TrimSpace(target.Account) == "" {
		target.Account = keychainAccount
	}
	target.Blob = blob
	if err := writeNativeProfileJournal(lockPath, nativeProfileJournal{Account: original.Account, OriginalBlob: append([]byte(nil), original.Blob...), HadOriginal: hadOriginal}); err != nil {
		_ = unlockAndClose(lock)
		return nil, err
	}
	if err := write(ctx, target); err != nil {
		_ = unlockAndClose(lock)
		return nil, err
	}
	return &NativeProfileLease{restore: func(restoreCtx context.Context) error {
		defer func() { _ = unlockAndClose(lock) }()
		var restoreErr error
		if hadOriginal {
			restoreErr = write(restoreCtx, original)
		} else {
			restoreErr = deleteEntry(restoreCtx, target.Account)
		}
		if restoreErr != nil {
			return restoreErr
		}
		return removeNativeProfileJournal(lockPath)
	}}, nil
}

func recoverNativeProfile(ctx context.Context, lockPath string) error {
	if strings.TrimSpace(lockPath) == "" {
		return errors.New("Antigravity native profile lock path is required")
	}
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o700); err != nil {
		return fmt.Errorf("create Antigravity profile lock directory: %w", err)
	}
	lock, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("open Antigravity profile lock: %w", err)
	}
	defer func() { _ = unlockAndClose(lock) }()
	for {
		err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			return fmt.Errorf("lock Antigravity profile slot: %w", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(nativeProfileLockPoll):
		}
	}
	return recoverNativeProfileLocked(ctx, lockPath, writeLocalKeychainEntry, deleteLocalKeychainEntry)
}

func recoverNativeProfileLocked(ctx context.Context, lockPath string, write keychainWriter, deleteEntry keychainDeleter) error {
	journal, ok, err := readNativeProfileJournal(lockPath)
	if err != nil || !ok {
		return err
	}
	if journal.HadOriginal {
		if err := write(ctx, KeychainEntry{Account: journal.Account, Blob: journal.OriginalBlob}); err != nil {
			return fmt.Errorf("recover native AGY Keychain profile: %w", err)
		}
	} else if err := deleteEntry(ctx, journal.Account); err != nil {
		return fmt.Errorf("recover native AGY Keychain profile: %w", err)
	}
	return removeNativeProfileJournal(lockPath)
}

func nativeProfileJournalPath(lockPath string) string { return lockPath + ".journal" }

func writeNativeProfileJournal(lockPath string, journal nativeProfileJournal) error {
	body, err := json.Marshal(journal)
	if err != nil {
		return fmt.Errorf("encode native AGY profile journal: %w", err)
	}
	path := nativeProfileJournalPath(lockPath)
	tmp, err := os.CreateTemp(filepath.Dir(path), ".native-profile-journal-*")
	if err != nil {
		return fmt.Errorf("create native AGY profile journal: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(body); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write native AGY profile journal: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("publish native AGY profile journal: %w", err)
	}
	return nil
}

func readNativeProfileJournal(lockPath string) (nativeProfileJournal, bool, error) {
	body, err := os.ReadFile(nativeProfileJournalPath(lockPath))
	if os.IsNotExist(err) {
		return nativeProfileJournal{}, false, nil
	}
	if err != nil {
		return nativeProfileJournal{}, false, err
	}
	var journal nativeProfileJournal
	if err := json.Unmarshal(body, &journal); err != nil {
		return nativeProfileJournal{}, false, fmt.Errorf("decode native AGY profile journal: %w", err)
	}
	if journal.HadOriginal && (journal.Account == "" || len(journal.OriginalBlob) == 0) {
		return nativeProfileJournal{}, false, errors.New("native AGY profile journal is incomplete")
	}
	if !journal.HadOriginal && strings.TrimSpace(journal.Account) == "" {
		journal.Account = keychainAccount
	}
	return journal, true, nil
}

func removeNativeProfileJournal(lockPath string) error {
	if err := os.Remove(nativeProfileJournalPath(lockPath)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove native AGY profile journal: %w", err)
	}
	return nil
}

func unlockAndClose(lock *os.File) error {
	if lock == nil {
		return nil
	}
	unlockErr := syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	closeErr := lock.Close()
	if unlockErr != nil {
		return unlockErr
	}
	return closeErr
}

func readLocalKeychainEntry(ctx context.Context) (KeychainEntry, bool, error) {
	current, err := user.Current()
	if err != nil {
		return KeychainEntry{}, false, nil
	}
	ctx, cancel := context.WithTimeout(ctx, keychainReadTimeout)
	defer cancel()
	// AGY normally uses its fixed `antigravity` account. Prefer it when both
	// slots exist: a stale username-scoped item may belong to another client
	// and swapping that slot would verify successfully while native AGY keeps
	// reading the untouched credential. The username fallback supports older
	// CLI builds that wrote there exclusively.
	accounts := []string{keychainAccount}
	if current.Username != keychainAccount {
		accounts = append(accounts, current.Username)
	}
	for _, account := range accounts {
		cmd := exec.CommandContext(ctx, "security", "find-generic-password", "-s", keychainService, "-a", account, "-w")
		body, runErr := cmd.Output()
		if runErr == nil && len(bytes.TrimSpace(body)) > 0 {
			return KeychainEntry{Account: account, Blob: bytes.TrimSpace(body)}, true, nil
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return KeychainEntry{}, false, ctxErr
		}
		var exitErr *exec.ExitError
		if runErr != nil && (!errors.As(runErr, &exitErr) || exitErr.ExitCode() != 44) {
			return KeychainEntry{}, false, fmt.Errorf("read Antigravity keychain item: %w", runErr)
		}
	}
	return KeychainEntry{}, false, nil
}

func writeLocalKeychainEntry(ctx context.Context, entry KeychainEntry) error {
	account := strings.TrimSpace(entry.Account)
	if account == "" {
		account = keychainAccount
	}
	if len(entry.Blob) == 0 {
		return errors.New("Antigravity Keychain blob is empty")
	}
	ctx, cancel := context.WithTimeout(ctx, keychainWriteTimeout)
	defer cancel()
	// Omitting the -w argument makes `security` read the secret from stdin;
	// this avoids exposing the OAuth blob in process arguments.
	cmd := exec.CommandContext(ctx, "security", "add-generic-password", "-U", "-s", keychainService, "-a", account, "-w")
	cmd.Stdin = bytes.NewReader(entry.Blob)
	if output, err := cmd.CombinedOutput(); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		return fmt.Errorf("write Antigravity keychain item: %w (%s)", err, strings.TrimSpace(string(output)))
	}
	return nil
}

func deleteLocalKeychainEntry(ctx context.Context, account string) error {
	account = strings.TrimSpace(account)
	if account == "" {
		account = keychainAccount
	}
	ctx, cancel := context.WithTimeout(ctx, keychainWriteTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "security", "delete-generic-password", "-s", keychainService, "-a", account)
	if output, err := cmd.CombinedOutput(); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 44 {
			return nil
		}
		return fmt.Errorf("delete Antigravity keychain item: %w (%s)", err, strings.TrimSpace(string(output)))
	}
	return nil
}
