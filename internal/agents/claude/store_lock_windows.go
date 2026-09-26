//go:build windows

package claude

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	profileRegistryFailImmediately = 0x00000001
	profileRegistryExclusiveLock   = 0x00000002
	profileLockViolation           = syscall.Errno(33)
)

var (
	profileRegistryProcessMu sync.Mutex
	profileRegistryKernel32  = windows.NewLazySystemDLL("kernel32.dll")
	profileRegistryLockFile  = profileRegistryKernel32.NewProc("LockFileEx")
	profileRegistryUnlock    = profileRegistryKernel32.NewProc("UnlockFileEx")
)

type profileRegistryLock struct {
	file       *os.File
	overlapped syscall.Overlapped
}

type profileCredentialLock struct {
	file           *os.File
	overlapped     syscall.Overlapped
	releaseProcess func()
}

func lockProfileRegistry(path string) (*profileRegistryLock, error) {
	return lockProfileRegistryContext(context.Background(), path)
}

func lockProfileRegistryContext(ctx context.Context, path string) (*profileRegistryLock, error) {
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for !profileRegistryProcessMu.TryLock() {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
		}
	}
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
	for {
		result, _, callErr := profileRegistryLockFile.Call(
			file.Fd(),
			profileRegistryExclusiveLock|profileRegistryFailImmediately,
			0,
			uintptr(^uint32(0)),
			uintptr(^uint32(0)),
			uintptr(unsafe.Pointer(&lock.overlapped)),
		)
		if result != 0 {
			return lock, nil
		}
		if !errors.Is(callErr, profileLockViolation) {
			_ = file.Close()
			profileRegistryProcessMu.Unlock()
			return nil, fmt.Errorf("lock Claude profile registry: %w", callErr)
		}
		select {
		case <-ctx.Done():
			_ = file.Close()
			profileRegistryProcessMu.Unlock()
			return nil, ctx.Err()
		case <-ticker.C:
		}
	}
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

func lockProfileCredential(ctx context.Context, instancePath string) (*profileCredentialLock, error) {
	if resolved, err := filepath.EvalSymlinks(instancePath); err == nil {
		instancePath = resolved
	}
	path := filepath.Clean(instancePath) + ".credentials.lock"
	releaseProcess, err := lockProfileCredentialProcess(ctx, path)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		releaseProcess()
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		releaseProcess()
		return nil, err
	}
	lock := &profileCredentialLock{file: file, releaseProcess: releaseProcess}
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		result, _, callErr := profileRegistryLockFile.Call(
			file.Fd(),
			profileRegistryExclusiveLock|profileRegistryFailImmediately,
			0,
			uintptr(^uint32(0)),
			uintptr(^uint32(0)),
			uintptr(unsafe.Pointer(&lock.overlapped)),
		)
		if result != 0 {
			return lock, nil
		}
		if !errors.Is(callErr, profileLockViolation) {
			_ = file.Close()
			releaseProcess()
			return nil, fmt.Errorf("lock Claude profile credential: %w", callErr)
		}
		select {
		case <-ctx.Done():
			_ = file.Close()
			releaseProcess()
			return nil, ctx.Err()
		case <-ticker.C:
		}
	}
}

func (l *profileCredentialLock) Close() error {
	result, _, callErr := profileRegistryUnlock.Call(
		l.file.Fd(),
		0,
		uintptr(^uint32(0)),
		uintptr(^uint32(0)),
		uintptr(unsafe.Pointer(&l.overlapped)),
	)
	closeErr := l.file.Close()
	l.releaseProcess()
	if result == 0 {
		return fmt.Errorf("unlock Claude profile credential: %w", callErr)
	}
	return closeErr
}

type profileRefreshLock struct {
	file           *os.File
	overlapped     syscall.Overlapped
	releaseProcess func()
}

// lockProfileRefresh serializes one profile's OAuth refresh network
// round-trip across goroutines and overlapping supervisor worker
// generations. It is distinct from lockProfileCredential so that a
// long-running refresh never blocks an unrelated ImportProfileCredential
// call on the same profile.
func lockProfileRefresh(ctx context.Context, instancePath string) (*profileRefreshLock, error) {
	if resolved, err := filepath.EvalSymlinks(instancePath); err == nil {
		instancePath = resolved
	}
	path := filepath.Clean(instancePath) + ".refresh.lock"
	releaseProcess, err := lockProfileCredentialProcess(ctx, path)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		releaseProcess()
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		releaseProcess()
		return nil, err
	}
	lock := &profileRefreshLock{file: file, releaseProcess: releaseProcess}
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		result, _, callErr := profileRegistryLockFile.Call(
			file.Fd(),
			profileRegistryExclusiveLock|profileRegistryFailImmediately,
			0,
			uintptr(^uint32(0)),
			uintptr(^uint32(0)),
			uintptr(unsafe.Pointer(&lock.overlapped)),
		)
		if result != 0 {
			return lock, nil
		}
		if !errors.Is(callErr, profileLockViolation) {
			_ = file.Close()
			releaseProcess()
			return nil, fmt.Errorf("lock Claude profile refresh: %w", callErr)
		}
		select {
		case <-ctx.Done():
			_ = file.Close()
			releaseProcess()
			return nil, ctx.Err()
		case <-ticker.C:
		}
	}
}

func (l *profileRefreshLock) Close() error {
	result, _, callErr := profileRegistryUnlock.Call(
		l.file.Fd(),
		0,
		uintptr(^uint32(0)),
		uintptr(^uint32(0)),
		uintptr(unsafe.Pointer(&l.overlapped)),
	)
	closeErr := l.file.Close()
	l.releaseProcess()
	if result == 0 {
		return fmt.Errorf("unlock Claude profile refresh: %w", callErr)
	}
	return closeErr
}
