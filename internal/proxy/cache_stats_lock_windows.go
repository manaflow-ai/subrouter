//go:build windows

package proxy

import (
	"errors"
	"golang.org/x/sys/windows"
	"os"
	"time"
)

type cacheStatsLock struct {
	file       *os.File
	overlapped windows.Overlapped
}

func acquireCacheStatsLock(path string) (*cacheStatsLock, error) {
	file, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	lock := &cacheStatsLock{file: file}
	deadline := time.Now().Add(cacheStatsLockTimeout)
	for {
		err = windows.LockFileEx(windows.Handle(file.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &lock.overlapped)
		if err == nil {
			return lock, nil
		}
		if !errors.Is(err, windows.ERROR_LOCK_VIOLATION) || time.Now().After(deadline) {
			_ = file.Close()
			return nil, err
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (lock *cacheStatsLock) Close() error {
	unlockErr := windows.UnlockFileEx(windows.Handle(lock.file.Fd()), 0, 1, 0, &lock.overlapped)
	closeErr := lock.file.Close()
	if unlockErr != nil {
		return unlockErr
	}
	return closeErr
}
