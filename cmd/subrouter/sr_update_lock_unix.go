//go:build !windows

package main

import (
	"errors"
	"os"
	"syscall"
)

// errUpdateLockBusy reports that another update or deployment holds the lease.
var errUpdateLockBusy = errors.New("lock is held")

// tryLockUpdateLease takes a non-blocking BSD flock on path, the same kernel
// lease the macOS deploy scripts take through mutation-lease-lib.sh, so an
// update never interleaves with a migration, a rollback or another update.
func tryLockUpdateLease(path string) (func(), error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = file.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, errUpdateLockBusy
		}
		return nil, err
	}
	return func() {
		_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
		_ = file.Close()
	}, nil
}
