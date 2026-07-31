//go:build windows

package proxy

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"unsafe"
)

const accountImportExclusiveLock = 0x00000002

var (
	accountImportKernel32 = syscall.NewLazyDLL("kernel32.dll")
	accountImportLockFile = accountImportKernel32.NewProc("LockFileEx")
	accountImportUnlock   = accountImportKernel32.NewProc("UnlockFileEx")
)

type accountImportTransactionLock struct {
	file       *os.File
	overlapped syscall.Overlapped
}

func lockAccountImportTransaction(storeDir string) (*accountImportTransactionLock, error) {
	if err := os.MkdirAll(storeDir, 0o700); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(filepath.Join(storeDir, ".account-import.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	lock := &accountImportTransactionLock{file: file}
	result, _, callErr := accountImportLockFile.Call(
		file.Fd(),
		accountImportExclusiveLock,
		0,
		uintptr(^uint32(0)),
		uintptr(^uint32(0)),
		uintptr(unsafe.Pointer(&lock.overlapped)),
	)
	if result == 0 {
		_ = file.Close()
		return nil, fmt.Errorf("lock account import transaction: %w", callErr)
	}
	return lock, nil
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
