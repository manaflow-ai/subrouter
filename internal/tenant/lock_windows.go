//go:build windows

package tenant

import (
	"fmt"
	"os"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

type registryLock struct {
	file       *os.File
	overlapped syscall.Overlapped
}

const registryExclusiveLock = 0x00000002

var (
	registryKernel32 = windows.NewLazySystemDLL("kernel32.dll")
	registryLockFile = registryKernel32.NewProc("LockFileEx")
	registryUnlock   = registryKernel32.NewProc("UnlockFileEx")
)

// lockRegistry serializes tenant registry mutations across Windows processes.
func (r *Registry) lockRegistry() (*registryLock, error) {
	if err := os.MkdirAll(r.stateDir, 0o700); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(r.Path()+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	lock := &registryLock{file: file}
	result, _, callErr := registryLockFile.Call(
		file.Fd(),
		registryExclusiveLock,
		0,
		uintptr(^uint32(0)),
		uintptr(^uint32(0)),
		uintptr(unsafe.Pointer(&lock.overlapped)),
	)
	if result == 0 {
		_ = file.Close()
		return nil, fmt.Errorf("lock tenant registry: %w", callErr)
	}
	return lock, nil
}

func (l *registryLock) Close() error {
	result, _, callErr := registryUnlock.Call(
		l.file.Fd(),
		0,
		uintptr(^uint32(0)),
		uintptr(^uint32(0)),
		uintptr(unsafe.Pointer(&l.overlapped)),
	)
	closeErr := l.file.Close()
	if result == 0 {
		return fmt.Errorf("unlock tenant registry: %w", callErr)
	}
	return closeErr
}
