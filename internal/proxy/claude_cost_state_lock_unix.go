//go:build !windows

package proxy

import (
	"os"
	"path/filepath"
	"syscall"
)

// lockClaudeCostState takes an exclusive advisory lock beside the state file
// so concurrent workers (a canary and its incumbent) serialize their
// read-modify-write. It returns a no-op unlock when locking is unavailable.
func lockClaudeCostState(path string) func() {
	if path == "" {
		return func() {}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return func() {}
	}
	file, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return func() {}
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX); err != nil {
		_ = file.Close()
		return func() {}
	}
	return func() {
		_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
		_ = file.Close()
	}
}
