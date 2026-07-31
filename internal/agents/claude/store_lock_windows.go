//go:build windows

package claude

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"unsafe"
)

const profileRegistryExclusiveLock = 0x00000002

var (
	profileRegistryProcessMu sync.Mutex
	profileRegistryKernel32  = syscall.NewLazyDLL("kernel32.dll")
	profileRegistryLockFile  = profileRegistryKernel32.NewProc("LockFileEx")
	profileRegistryUnlock    = profileRegistryKernel32.NewProc("UnlockFileEx")
)

type profileRegistryLock struct {
	file       *os.File
	overlapped syscall.Overlapped
}

func lockProfileRegistry(path string) (*profileRegistryLock, error) {
	profileRegistryProcessMu.Lock()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		profileRegistryProcessMu.Unlock()
		return nil, err
	}
	file, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		profileRegistryProcessMu.Unlock()
		return nil, err
	}
	lock := &profileRegistryLock{file: file}
	result, _, callErr := profileRegistryLockFile.Call(
		file.Fd(),
		profileRegistryExclusiveLock,
		0,
		uintptr(^uint32(0)),
		uintptr(^uint32(0)),
		uintptr(unsafe.Pointer(&lock.overlapped)),
	)
	if result == 0 {
		_ = file.Close()
		profileRegistryProcessMu.Unlock()
		return nil, fmt.Errorf("lock Claude profile registry: %w", callErr)
	}
	return lock, nil
}

func (l *profileRegistryLock) Close() error {
	result, _, callErr := profileRegistryUnlock.Call(
		l.file.Fd(),
		0,
		uintptr(^uint32(0)),
		uintptr(^uint32(0)),
		uintptr(unsafe.Pointer(&l.overlapped)),
	)
	closeErr := l.file.Close()
	profileRegistryProcessMu.Unlock()
	if result == 0 {
		return fmt.Errorf("unlock Claude profile registry: %w", callErr)
	}
	return closeErr
}
