//go:build windows

package cutovercanary

import "os"

func openPrivateNoFollow(path string) (*os.File, error) {
	return os.Open(path)
}
