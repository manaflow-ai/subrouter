//go:build windows

package accounts

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

type accountFileLock struct {
	file       *os.File
	overlapped syscall.Overlapped
	mu         interface{ Unlock() }
}

const accountFileExclusiveLock = 0x00000002

var (
	accountFileKernel32 = windows.NewLazySystemDLL("kernel32.dll")
	accountFileLockFile = accountFileKernel32.NewProc("LockFileEx")
	accountFileUnlock   = accountFileKernel32.NewProc("UnlockFileEx")
)

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
	lock := &accountFileLock{file: file, mu: mu}
	result, _, callErr := accountFileLockFile.Call(
		file.Fd(), accountFileExclusiveLock, 0,
		uintptr(^uint32(0)), uintptr(^uint32(0)), uintptr(unsafe.Pointer(&lock.overlapped)),
	)
	if result == 0 {
		_ = file.Close()
		mu.Unlock()
		return nil, fmt.Errorf("lock stored account: %w", callErr)
	}
	return lock, nil
}

func (l *accountFileLock) Close() error {
	result, _, callErr := accountFileUnlock.Call(
		l.file.Fd(), 0, uintptr(^uint32(0)), uintptr(^uint32(0)), uintptr(unsafe.Pointer(&l.overlapped)),
	)
	closeErr := l.file.Close()
	l.mu.Unlock()
	if result == 0 {
		return fmt.Errorf("unlock stored account: %w", callErr)
	}
	return closeErr
}
