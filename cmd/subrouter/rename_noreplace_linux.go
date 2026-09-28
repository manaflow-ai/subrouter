package main

import "golang.org/x/sys/unix"

// renameNoReplace renames from to to, failing if to already exists.
func renameNoReplace(from, to string) error {
	return unix.Renameat2(unix.AT_FDCWD, from, unix.AT_FDCWD, to, unix.RENAME_NOREPLACE)
}
