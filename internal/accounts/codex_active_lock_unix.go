//go:build !windows

package accounts

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
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
func TryLockActiveCodexAuth() (*ActiveCodexAuthLock, error) {
	return lockActiveCodexAuth(true)
}

// LockActiveCodexAuth acquires an exclusive lock, blocking until available.
func LockActiveCodexAuth() (*ActiveCodexAuthLock, error) {
	return lockActiveCodexAuth(false)
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

func lockActiveCodexAuth(nonBlocking bool) (*ActiveCodexAuthLock, error) {
	path := activeCodexAuthLockPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	flags := syscall.LOCK_EX
	if nonBlocking {
		flags |= syscall.LOCK_NB
	}
	if err := syscall.Flock(int(file.Fd()), flags); err != nil {
		_ = file.Close()
		if nonBlocking && (errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN)) {
			return nil, ErrActiveCodexAuthBusy
		}
		return nil, err
	}
	return &ActiveCodexAuthLock{file: file}, nil
}

func (l *ActiveCodexAuthLock) Close() error {
	if l == nil || l.file == nil {
		return nil
	}
	unlockErr := syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN)
	closeErr := l.file.Close()
	l.file = nil
	if unlockErr != nil {
		return unlockErr
	}
	return closeErr
}
