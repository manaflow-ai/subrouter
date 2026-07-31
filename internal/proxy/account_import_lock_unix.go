//go:build !windows

package proxy

import (
	"os"
	"path/filepath"
	"syscall"
)

type accountImportTransactionLock struct {
	file *os.File
}

func lockAccountImportTransaction(storeDir string) (*accountImportTransactionLock, error) {
	if err := os.MkdirAll(storeDir, 0o700); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(filepath.Join(storeDir, ".account-import.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX); err != nil {
		_ = file.Close()
		return nil, err
	}
	return &accountImportTransactionLock{file: file}, nil
}

func (l *accountImportTransactionLock) Close() error {
	unlockErr := syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN)
	closeErr := l.file.Close()
	if unlockErr != nil {
		return unlockErr
	}
	return closeErr
}
