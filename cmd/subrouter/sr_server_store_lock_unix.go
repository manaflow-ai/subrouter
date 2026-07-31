//go:build !windows

package main

import (
	"os"
	"path/filepath"
	"sync"
	"syscall"
)

var srServerStoreProcessMu sync.Mutex

type srServerStoreLock struct {
	file *os.File
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
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX); err != nil {
		_ = file.Close()
		srServerStoreProcessMu.Unlock()
		return nil, err
	}
	return &srServerStoreLock{file: file}, nil
}

func (l *srServerStoreLock) Close() error {
	unlockErr := syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN)
	closeErr := l.file.Close()
	srServerStoreProcessMu.Unlock()
	if unlockErr != nil {
		return unlockErr
	}
	return closeErr
}
