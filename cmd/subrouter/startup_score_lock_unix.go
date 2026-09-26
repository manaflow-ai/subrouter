//go:build !windows

package main

import (
	"errors"
	"os"
	"syscall"
)

type startupScoreFileLock struct {
	file *os.File
}

func tryLockStartupScoreFile(path string) (*startupScoreFileLock, bool, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, false, err
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = file.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, false, nil
		}
		return nil, false, err
	}
	return &startupScoreFileLock{file: file}, true, nil
}

func (l *startupScoreFileLock) Close() error {
	if l == nil || l.file == nil {
		return nil
	}
	file := l.file
	l.file = nil
	unlocked := syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
	closed := file.Close()
	if unlocked != nil {
		return unlocked
	}
	return closed
}
