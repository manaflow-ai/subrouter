//go:build !windows

package proxy

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
)

type accountImportTransactionLock struct {
	file *os.File
}

func lockAccountImportTransaction(ctx context.Context, storeDir string) (*accountImportTransactionLock, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(storeDir, 0o700); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(filepath.Join(storeDir, ".account-import.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	for {
		if err := ctx.Err(); err != nil {
			_ = file.Close()
			return nil, err
		}
		err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return &accountImportTransactionLock{file: file}, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			_ = file.Close()
			return nil, err
		}
		if err := waitAccountImportLockRetry(ctx); err != nil {
			_ = file.Close()
			return nil, err
		}
	}
}

func (l *accountImportTransactionLock) Close() error {
	unlockErr := syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN)
	closeErr := l.file.Close()
	if unlockErr != nil {
		return unlockErr
	}
	return closeErr
}
