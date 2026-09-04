//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package claude

import (
	"os"
	"time"

	"golang.org/x/sys/unix"
)

func setRootFileModTime(file *os.File, modTime time.Time) error {
	value := unix.NsecToTimeval(modTime.UnixNano())
	return unix.Futimes(int(file.Fd()), []unix.Timeval{value, value})
}
