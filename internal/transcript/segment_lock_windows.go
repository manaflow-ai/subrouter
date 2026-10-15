//go:build windows

package transcript

import (
	"os"
	"time"
)

func lockLiveDir(string) error { return nil }

// tryLockLiveDir treats a live directory untouched for two hours as
// abandoned; Windows hosts do not share a spool between generations.
func tryLockLiveDir(dir string) (func(), bool) {
	info, err := os.Stat(dir)
	if err != nil || time.Since(info.ModTime()) < 2*time.Hour {
		return nil, false
	}
	return func() {}, true
}

func processAlive(pid int) bool {
	process, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	_ = process.Release()
	return true
}
