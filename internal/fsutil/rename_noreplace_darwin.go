//go:build darwin

package fsutil

import "golang.org/x/sys/unix"

// RenameNoReplace atomically moves source to destination only when the
// destination does not exist.
func RenameNoReplace(source, destination string) error {
	return unix.RenamexNp(source, destination, unix.RENAME_EXCL)
}
