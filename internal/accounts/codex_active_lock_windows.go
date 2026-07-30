//go:build windows

package accounts

import (
	"errors"
	"os"
	"path/filepath"
)

// ErrActiveCodexAuthBusy means another process currently holds the active-auth lock.
var ErrActiveCodexAuthBusy = errors.New("active Codex auth is busy")

type ActiveCodexAuthLock struct {
	file *os.File
}

func activeCodexAuthLockPath() string {
	return DefaultCodexAuthPath() + ".lock"
}

// TryLockActiveCodexAuth acquires an exclusive lock without blocking.
// Windows builds keep a best-effort open handle; concurrent writers are uncommon.
func TryLockActiveCodexAuth() (*ActiveCodexAuthLock, error) {
	return lockActiveCodexAuth()
}

// LockActiveCodexAuth acquires an exclusive lock, blocking until available.
func LockActiveCodexAuth() (*ActiveCodexAuthLock, error) {
	return lockActiveCodexAuth()
}

// AcquireActiveCodexAuthLock tries non-blocking first; if busy, calls onWait then blocks.
func AcquireActiveCodexAuthLock(onWait func()) (*ActiveCodexAuthLock, error) {
	lock, err := TryLockActiveCodexAuth()
	if err == nil {
		return lock, nil
	}
	if !errors.Is(err, ErrActiveCodexAuthBusy) {
		return nil, err
	}
	if onWait != nil {
		onWait()
	}
	return LockActiveCodexAuth()
}

func lockActiveCodexAuth() (*ActiveCodexAuthLock, error) {
	path := activeCodexAuthLockPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	return &ActiveCodexAuthLock{file: file}, nil
}

func (l *ActiveCodexAuthLock) Close() error {
	if l == nil || l.file == nil {
		return nil
	}
	err := l.file.Close()
	l.file = nil
	return err
}
