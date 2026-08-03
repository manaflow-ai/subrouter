//go:build windows

package tenant

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"unsafe"
)

const (
	tenantUseFailImmediately = 0x00000001
	tenantUseExclusiveLock   = 0x00000002
	tenantUseLockViolation   = syscall.Errno(33)
)

var (
	tenantUseKernel32 = syscall.NewLazyDLL("kernel32.dll")
	tenantUseLockFile = tenantUseKernel32.NewProc("LockFileEx")
	tenantUseUnlock   = tenantUseKernel32.NewProc("UnlockFileEx")
)

// UseLock coordinates active tenant requests with permanent deletion across
// Windows worker processes using a shared/exclusive whole-file lock.
type UseLock struct {
	file       *os.File
	overlapped syscall.Overlapped
}

func (r *Registry) openUseLock(id string) (*os.File, error) {
	if !ValidExternalID(id) {
		return nil, errors.New("invalid tenant ID")
	}
	dir := filepath.Join(r.stateDir, "tenant-use-locks")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return os.OpenFile(filepath.Join(dir, id+".lock"), os.O_CREATE|os.O_RDWR, 0o600)
}

func (r *Registry) AcquireUse(id string) (*UseLock, error) {
	file, err := r.openUseLock(id)
	if err != nil {
		return nil, err
	}
	lock := &UseLock{file: file}
	result, _, callErr := tenantUseLockFile.Call(
		file.Fd(),
		0,
		0,
		uintptr(^uint32(0)),
		uintptr(^uint32(0)),
		uintptr(unsafe.Pointer(&lock.overlapped)),
	)
	if result == 0 {
		_ = file.Close()
		return nil, fmt.Errorf("lock tenant use: %w", callErr)
	}
	return lock, nil
}

func (r *Registry) TryAcquireExclusiveUse(id string) (*UseLock, bool, error) {
	file, err := r.openUseLock(id)
	if err != nil {
		return nil, false, err
	}
	lock := &UseLock{file: file}
	result, _, callErr := tenantUseLockFile.Call(
		file.Fd(),
		tenantUseExclusiveLock|tenantUseFailImmediately,
		0,
		uintptr(^uint32(0)),
		uintptr(^uint32(0)),
		uintptr(unsafe.Pointer(&lock.overlapped)),
	)
	if result == 0 {
		_ = file.Close()
		if errors.Is(callErr, tenantUseLockViolation) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("lock tenant deletion: %w", callErr)
	}
	return lock, true, nil
}

func (l *UseLock) Close() error {
	result, _, callErr := tenantUseUnlock.Call(
		l.file.Fd(),
		0,
		uintptr(^uint32(0)),
		uintptr(^uint32(0)),
		uintptr(unsafe.Pointer(&l.overlapped)),
	)
	closeErr := l.file.Close()
	if result == 0 {
		return fmt.Errorf("unlock tenant use: %w", callErr)
	}
	return closeErr
}
