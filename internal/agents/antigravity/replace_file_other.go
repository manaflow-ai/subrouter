//go:build !windows

package antigravity

import "os"

func replaceManagedCredentialFile(source, destination string) error {
	return os.Rename(source, destination)
}
