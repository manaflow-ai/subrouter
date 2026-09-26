//go:build windows

package fsutil

import "golang.org/x/sys/windows"

// RenameNoReplace atomically moves source to destination only when the
// destination does not exist.
func RenameNoReplace(source, destination string) error {
	sourceUTF16, err := windows.UTF16PtrFromString(source)
	if err != nil {
		return err
	}
	destinationUTF16, err := windows.UTF16PtrFromString(destination)
	if err != nil {
		return err
	}
	// MoveFileW fails when destination already exists; unlike os.Rename it does
	// not opt into replacement semantics.
	return windows.MoveFile(sourceUTF16, destinationUTF16)
}
