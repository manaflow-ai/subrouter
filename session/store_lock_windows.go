//go:build windows

package session

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"unsafe"
)

const lockFileExclusiveLock = 0x00000002

var (
	kernel32     = syscall.NewLazyDLL("kernel32.dll")
	lockFileEx   = kernel32.NewProc("LockFileEx")
	unlockFileEx = kernel32.NewProc("UnlockFileEx")
)

type storeFileLock struct {
	file       *os.File
	overlapped syscall.Overlapped
}

func lockSessionStore(path string) (*storeFileLock, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	lock := &storeFileLock{file: file}
	result, _, callErr := lockFileEx.Call(
		file.Fd(),
		lockFileExclusiveLock,
		0,
		uintptr(^uint32(0)),
		uintptr(^uint32(0)),
		uintptr(unsafe.Pointer(&lock.overlapped)),
	)
	if result == 0 {
		_ = file.Close()
		return nil, fmt.Errorf("lock session store: %w", callErr)
	}
	return lock, nil
}

func (l *storeFileLock) Close() error {
	result, _, callErr := unlockFileEx.Call(
		l.file.Fd(),
		0,
		uintptr(^uint32(0)),
		uintptr(^uint32(0)),
		uintptr(unsafe.Pointer(&l.overlapped)),
	)
	closeErr := l.file.Close()
	if result == 0 {
		return fmt.Errorf("unlock session store: %w", callErr)
	}
	return closeErr
}
