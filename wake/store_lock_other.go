//go:build !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd

package wake

import "os"

// Other platforms still get atomic file replacement; this lock is process-local
// because the supported CLI platforms provide flock above.
func acquireFileLock(path string) (func(), error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	return func() { _ = f.Close() }, nil
}

func AcquireWorkerLock(path string) (func(), error) { return acquireFileLock(path) }
