//go:build windows

package proxy

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"unsafe"
)

const (
	accountImportFailImmediately = 0x00000001
	accountImportExclusiveLock   = 0x00000002
	accountImportLockViolation   = syscall.Errno(33)
)

var (
	accountImportKernel32 = syscall.NewLazyDLL("kernel32.dll")
	accountImportLockFile = accountImportKernel32.NewProc("LockFileEx")
	accountImportUnlock   = accountImportKernel32.NewProc("UnlockFileEx")
)

type accountImportTransactionLock struct {
	file       *os.File
	overlapped syscall.Overlapped
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
	lock := &accountImportTransactionLock{file: file}
	for {
		if err := ctx.Err(); err != nil {
			_ = file.Close()
			return nil, err
		}
		result, _, callErr := accountImportLockFile.Call(
			file.Fd(),
			accountImportExclusiveLock|accountImportFailImmediately,
			0,
			uintptr(^uint32(0)),
			uintptr(^uint32(0)),
			uintptr(unsafe.Pointer(&lock.overlapped)),
		)
		if result != 0 {
			return lock, nil
		}
		if !errors.Is(callErr, accountImportLockViolation) {
			_ = file.Close()
			return nil, fmt.Errorf("lock account import transaction: %w", callErr)
		}
		if err := waitAccountImportLockRetry(ctx); err != nil {
			_ = file.Close()
			return nil, err
		}
	}
}

func (l *accountImportTransactionLock) Close() error {
	result, _, callErr := accountImportUnlock.Call(
		l.file.Fd(),
		0,
		uintptr(^uint32(0)),
		uintptr(^uint32(0)),
		uintptr(unsafe.Pointer(&l.overlapped)),
	)
	closeErr := l.file.Close()
	if result == 0 {
		return fmt.Errorf("unlock account import transaction: %w", callErr)
	}
	return closeErr
}
