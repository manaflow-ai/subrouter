//go:build windows

package claude

import (
	"io"
	"os"

	"golang.org/x/sys/windows"
)

// Windows rename-based atomic replacement needs readers to share deletion.
// os.ReadFile does not request FILE_SHARE_DELETE, so a concurrent reader can
// otherwise make ReplaceFile fail after a credential mutation has completed.
func readFileForAtomicReplace(path string) ([]byte, error) {
	pathPointer, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	handle, err := windows.CreateFile(
		pathPointer,
		windows.GENERIC_READ,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL,
		0,
	)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(handle), path)
	if file == nil {
		_ = windows.CloseHandle(handle)
		return nil, os.ErrInvalid
	}
	defer file.Close()
	return io.ReadAll(file)
}
