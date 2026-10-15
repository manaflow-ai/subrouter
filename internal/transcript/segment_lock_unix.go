//go:build unix

package transcript

import (
	"os"
	"path/filepath"
	"syscall"
)

// liveLocks keeps each live directory's lock file open for the life of the
// process, so the flock is released exactly when the process exits.
var liveLocks []*os.File

func lockLiveDir(dir string) error {
	file, err := os.OpenFile(filepath.Join(dir, ".lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = file.Close()
		return err
	}
	liveLocks = append(liveLocks, file)
	return nil
}

// tryLockLiveDir succeeds only when no live process holds dir.
func tryLockLiveDir(dir string) (func(), bool) {
	file, err := os.OpenFile(filepath.Join(dir, ".lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, false
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = file.Close()
		return nil, false
	}
	return func() { _ = file.Close() }, true
}

func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}
