package main

import "golang.org/x/sys/unix"

// renameNoReplace renames from to to, failing if to already exists.
func renameNoReplace(from, to string) error {
	return unix.RenamexNp(from, to, unix.RENAME_EXCL)
}
