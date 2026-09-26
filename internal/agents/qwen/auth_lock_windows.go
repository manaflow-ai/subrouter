//go:build windows

package qwen

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

type consoleCredentialFileLock struct {
	file       *os.File
	overlapped syscall.Overlapped
}

const consoleCredentialExclusiveLock = 0x00000002

var (
	consoleCredentialKernel32 = windows.NewLazySystemDLL("kernel32.dll")
	consoleCredentialLockFile = consoleCredentialKernel32.NewProc("LockFileEx")
	consoleCredentialUnlock   = consoleCredentialKernel32.NewProc("UnlockFileEx")
)

func lockConsoleCredentialFile(root, accountID string) (*consoleCredentialFileLock, error) {
	lockDir := filepath.Join(root, ".locks")
	if err := os.MkdirAll(lockDir, 0o700); err != nil {
		return nil, err
	}
	digest := sha256.Sum256([]byte(accountID))
	path := filepath.Join(lockDir, hex.EncodeToString(digest[:])+".lock")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	lock := &consoleCredentialFileLock{file: file}
	result, _, callErr := consoleCredentialLockFile.Call(
		file.Fd(), consoleCredentialExclusiveLock, 0,
		uintptr(^uint32(0)), uintptr(^uint32(0)), uintptr(unsafe.Pointer(&lock.overlapped)),
	)
	if result == 0 {
		_ = file.Close()
		return nil, fmt.Errorf("lock Qwen console credential: %w", callErr)
	}
	return lock, nil
}

func (l *consoleCredentialFileLock) Close() error {
	result, _, callErr := consoleCredentialUnlock.Call(
		l.file.Fd(), 0, uintptr(^uint32(0)), uintptr(^uint32(0)), uintptr(unsafe.Pointer(&l.overlapped)),
	)
	closeErr := l.file.Close()
	if result == 0 {
		return fmt.Errorf("unlock Qwen console credential: %w", callErr)
	}
	return closeErr
}
