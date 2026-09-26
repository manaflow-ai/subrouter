//go:build windows

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

const srServerStoreExclusiveLock = 0x00000002

var (
	srServerStoreProcessMu sync.Mutex
	srServerStoreKernel32  = windows.NewLazySystemDLL("kernel32.dll")
	srServerStoreLockFile  = srServerStoreKernel32.NewProc("LockFileEx")
	srServerStoreUnlock    = srServerStoreKernel32.NewProc("UnlockFileEx")
)

type srServerStoreLock struct {
	file       *os.File
	overlapped syscall.Overlapped
}

func lockSRServerStore(path string) (*srServerStoreLock, error) {
	srServerStoreProcessMu.Lock()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		srServerStoreProcessMu.Unlock()
		return nil, err
	}
	file, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		srServerStoreProcessMu.Unlock()
		return nil, err
	}
	lock := &srServerStoreLock{file: file}
	result, _, callErr := srServerStoreLockFile.Call(
		file.Fd(),
		srServerStoreExclusiveLock,
		0,
		uintptr(^uint32(0)),
		uintptr(^uint32(0)),
		uintptr(unsafe.Pointer(&lock.overlapped)),
	)
	if result == 0 {
		_ = file.Close()
		srServerStoreProcessMu.Unlock()
		return nil, fmt.Errorf("lock server registry: %w", callErr)
	}
	return lock, nil
}

func (l *srServerStoreLock) Close() error {
	result, _, callErr := srServerStoreUnlock.Call(
		l.file.Fd(),
		0,
		uintptr(^uint32(0)),
		uintptr(^uint32(0)),
		uintptr(unsafe.Pointer(&l.overlapped)),
	)
	closeErr := l.file.Close()
	srServerStoreProcessMu.Unlock()
	if result == 0 {
		return fmt.Errorf("unlock server registry: %w", callErr)
	}
	return closeErr
}
