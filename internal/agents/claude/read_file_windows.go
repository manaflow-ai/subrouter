//go:build windows

package claude

import (
	"io"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

// Windows rename-based atomic replacement needs readers to share deletion.
// os.ReadFile does not request FILE_SHARE_DELETE, so a concurrent reader can
// otherwise make ReplaceFile fail after a credential mutation has completed.
func readFileForAtomicReplace(path string) ([]byte, error) {
	pathPointer, err := windows.UTF16PtrFromString(windowsExtendedLengthPath(path))
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

// windowsExtendedLengthPath mirrors the standard library's long-path handling
// for the direct CreateFile call above. UNC paths need the distinct UNC form;
// blindly prepending \\?\ would reinterpret the server name as a drive path.
func windowsExtendedLengthPath(path string) string {
	if strings.HasPrefix(path, `\\?\`) ||
		strings.HasPrefix(path, `\??\`) ||
		strings.HasPrefix(path, `\\.\`) {
		return path
	}
	absolute := path
	if !filepath.IsAbs(absolute) {
		resolved, err := filepath.Abs(absolute)
		if err != nil {
			return path
		}
		absolute = resolved
	}
	if len(absolute) < 248 {
		return path
	}
	absolute = filepath.Clean(absolute)
	if strings.HasPrefix(absolute, `\\`) {
		return `\\?\UNC\` + strings.TrimLeft(absolute, `\`)
	}
	return `\\?\` + absolute
}
