//go:build windows

package antigravity

import "golang.org/x/sys/windows"

// windows.Rename uses MoveFileEx with MOVEFILE_REPLACE_EXISTING. os.Rename
// cannot replace an existing credential on Windows, which would break both
// re-import and refresh-token rotation.
func replaceManagedCredentialFile(source, destination string) error {
	return windows.Rename(source, destination)
}
