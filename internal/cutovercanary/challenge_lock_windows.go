//go:build windows

package cutovercanary

import (
	"errors"
	"fmt"
	"os"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	challengeLockFailImmediately = 0x00000001
	challengeLockExclusive       = 0x00000002
	challengeLockViolation       = syscall.Errno(33)
)

var (
	challengeKernel32  = windows.NewLazySystemDLL("kernel32.dll")
	challengeLockFile  = challengeKernel32.NewProc("LockFileEx")
	challengeUnlock    = challengeKernel32.NewProc("UnlockFileEx")
	challengeOverlapMu sync.Mutex
	challengeOverlaps  = map[uintptr]*syscall.Overlapped{}
)

func openArtifactLock(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
}

func tryLockChallengeFile(file *os.File) error {
	overlapped := &syscall.Overlapped{}
	result, _, callErr := challengeLockFile.Call(file.Fd(), challengeLockExclusive|challengeLockFailImmediately, 0, uintptr(^uint32(0)), uintptr(^uint32(0)), uintptr(unsafe.Pointer(overlapped)))
	if result == 0 {
		if errors.Is(callErr, challengeLockViolation) {
			return errChallengeActive
		}
		return fmt.Errorf("LockFileEx: %w", callErr)
	}
	challengeOverlapMu.Lock()
	challengeOverlaps[file.Fd()] = overlapped
	challengeOverlapMu.Unlock()
	return nil
}

func unlockChallengeFile(file *os.File) error {
	challengeOverlapMu.Lock()
	overlapped := challengeOverlaps[file.Fd()]
	delete(challengeOverlaps, file.Fd())
	challengeOverlapMu.Unlock()
	if overlapped == nil {
		return nil
	}
	result, _, callErr := challengeUnlock.Call(file.Fd(), 0, uintptr(^uint32(0)), uintptr(^uint32(0)), uintptr(unsafe.Pointer(overlapped)))
	if result == 0 {
		return fmt.Errorf("UnlockFileEx: %w", callErr)
	}
	return nil
}
