//go:build !windows

package claude

import "os"

func readFileForAtomicReplace(path string) ([]byte, error) {
	return os.ReadFile(path)
}
