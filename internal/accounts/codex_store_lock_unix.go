//go:build !windows

package accounts

import (
	"os"
	"path/filepath"
	"syscall"
)

type accountFileLock struct {
	file *os.File
}

func (s CodexStore) lockStoredAccount(email string) (*accountFileLock, error) {
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return nil, err
	}
	path := filepath.Join(s.Dir, "."+accountLockFilename(email)+".lock")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX); err != nil {
		_ = file.Close()
		return nil, err
	}
	return &accountFileLock{file: file}, nil
}

func (l *accountFileLock) Close() error {
	unlockErr := syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN)
	closeErr := l.file.Close()
	if unlockErr != nil {
		return unlockErr
	}
	return closeErr
}
