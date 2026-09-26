//go:build !windows

package qwen

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"syscall"
)

type consoleCredentialFileLock struct {
	file *os.File
}

func lockConsoleCredentialFile(root, accountID string) (*consoleCredentialFileLock, error) {
	lockDir := filepath.Join(root, ".locks")
	if err := os.MkdirAll(lockDir, 0o700); err != nil {
		return nil, err
	}
	digest := sha256.Sum256([]byte(accountID))
	path := filepath.Join(lockDir, hex.EncodeToString(digest[:])+".lock")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX); err != nil {
		_ = file.Close()
		return nil, err
	}
	return &consoleCredentialFileLock{file: file}, nil
}

func (l *consoleCredentialFileLock) Close() error {
	unlockErr := syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN)
	closeErr := l.file.Close()
	if unlockErr != nil {
		return unlockErr
	}
	return closeErr
}
