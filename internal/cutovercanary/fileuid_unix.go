//go:build !windows

package cutovercanary

import (
	"os"
	"syscall"
)

func fileUID(info os.FileInfo) int {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return -1
	}
	return int(stat.Uid)
}
