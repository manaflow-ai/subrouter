//go:build windows

package main

import (
	"errors"
	"fmt"
	"os"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	startupScoreLockFailImmediately = 0x00000001
	startupScoreLockExclusive       = 0x00000002
	startupScoreLockViolation       = syscall.Errno(33)
)

var (
	startupScoreKernel32 = windows.NewLazySystemDLL("kernel32.dll")
	startupScoreLockFile = startupScoreKernel32.NewProc("LockFileEx")
	startupScoreUnlock   = startupScoreKernel32.NewProc("UnlockFileEx")
)

type startupScoreFileLock struct {
	file       *os.File
	overlapped syscall.Overlapped
}

func tryLockStartupScoreFile(path string) (*startupScoreFileLock, bool, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, false, err
	}
	lock := &startupScoreFileLock{file: file}
	result, _, callErr := startupScoreLockFile.Call(
		file.Fd(),
		startupScoreLockExclusive|startupScoreLockFailImmediately,
		0,
		uintptr(^uint32(0)),
		uintptr(^uint32(0)),
		uintptr(unsafe.Pointer(&lock.overlapped)),
	)
	if result == 0 {
		_ = file.Close()
		if errors.Is(callErr, startupScoreLockViolation) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("lock startup score file: %w", callErr)
	}
	return lock, true, nil
}

func (l *startupScoreFileLock) Close() error {
	if l == nil || l.file == nil {
		return nil
	}
	file := l.file
	l.file = nil
	result, _, callErr := startupScoreUnlock.Call(
		file.Fd(),
		0,
		uintptr(^uint32(0)),
		uintptr(^uint32(0)),
		uintptr(unsafe.Pointer(&l.overlapped)),
	)
	closeErr := file.Close()
	if result == 0 {
		return fmt.Errorf("unlock startup score file: %w", callErr)
	}
	return closeErr
}
