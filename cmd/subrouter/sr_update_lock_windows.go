//go:build windows

package main

import (
	"errors"
	"os"
	"time"
)

var errUpdateLockBusy = errors.New("lock is held")

// tryLockUpdateLease uses an exclusively created marker on Windows, where the
// deploy scripts' flock lease does not exist.
func tryLockUpdateLease(path string) (func(), error) {
	// A marker left by a crashed update must not block the next one forever.
	if info, err := os.Stat(path); err == nil && time.Since(info.ModTime()) > 30*time.Minute {
		_ = os.Remove(path)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return nil, errUpdateLockBusy
		}
		return nil, err
	}
	return func() {
		_ = file.Close()
		_ = os.Remove(path)
	}, nil
}
