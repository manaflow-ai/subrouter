//go:build windows

package tenant

import (
	"os"
)

type registryLock struct {
	file *os.File
}

// lockRegistry mirrors the unix flock variant; like
// accounts.lockStoredAccount, Windows gets a best-effort lock file only.
func (r *Registry) lockRegistry() (*registryLock, error) {
	if err := os.MkdirAll(r.stateDir, 0o700); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(r.Path()+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	return &registryLock{file: file}, nil
}

func (l *registryLock) Close() error {
	return l.file.Close()
}
