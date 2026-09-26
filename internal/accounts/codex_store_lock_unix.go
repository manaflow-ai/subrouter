//go:build !windows

package accounts

import (
	"os"
	"path/filepath"
	"syscall"
)

type accountFileLock struct {
	file *os.File
	mu   interface{ Unlock() }
}

func (s CodexStore) lockStoredAccount(email string) (*accountFileLock, error) {
	mu := storedAccountProcessMutex(s.Dir, email)
	mu.Lock()
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		mu.Unlock()
		return nil, err
	}
	path := filepath.Join(s.Dir, "."+accountLockFilename(email)+".lock")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		mu.Unlock()
		return nil, err
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX); err != nil {
		_ = file.Close()
		mu.Unlock()
		return nil, err
	}
	return &accountFileLock{file: file, mu: mu}, nil
}

func (l *accountFileLock) Close() error {
	unlockErr := syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN)
	closeErr := l.file.Close()
	l.mu.Unlock()
	if unlockErr != nil {
		return unlockErr
	}
	return closeErr
}
