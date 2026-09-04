//go:build windows

package claude

import (
	"os"
	"time"

	"golang.org/x/sys/windows"
)

func setRootFileModTime(file *os.File, modTime time.Time) error {
	value := uint64(modTime.UnixNano()/100) + 116444736000000000
	fileTime := windows.Filetime{LowDateTime: uint32(value), HighDateTime: uint32(value >> 32)}
	return windows.SetFileTime(windows.Handle(file.Fd()), nil, &fileTime, &fileTime)
}
