//go:build !windows

package proxy

import (
	"errors"
	"os"
	"syscall"
	"time"
)

type cacheStatsLock struct{ file *os.File }

func acquireCacheStatsLock(path string) (*cacheStatsLock, error) {
	file, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(cacheStatsLockTimeout)
	for {
		err = syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return &cacheStatsLock{file: file}, nil
		}
		if (!errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN)) || time.Now().After(deadline) {
			_ = file.Close()
			return nil, err
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (lock *cacheStatsLock) Close() error {
	unlockErr := syscall.Flock(int(lock.file.Fd()), syscall.LOCK_UN)
	closeErr := lock.file.Close()
	if unlockErr != nil {
		return unlockErr
	}
	return closeErr
}
