//go:build linux

package fsutil

import "golang.org/x/sys/unix"

// RenameNoReplace atomically moves source to destination only when the
// destination does not exist.
func RenameNoReplace(source, destination string) error {
	return unix.Renameat2(unix.AT_FDCWD, source, unix.AT_FDCWD, destination, unix.RENAME_NOREPLACE)
}
