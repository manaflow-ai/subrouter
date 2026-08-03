//go:build windows

package tenant

import (
	"errors"
	"os"
	"path/filepath"
)

// UseLock is a best-effort file lifetime guard on Windows. Hosted deployments
// run on Linux, where the implementation provides cross-process flocking.
type UseLock struct {
	file *os.File
}

func (r *Registry) openUseLock(id string) (*os.File, error) {
	if !ValidExternalID(id) {
		return nil, errors.New("invalid tenant ID")
	}
	dir := filepath.Join(r.stateDir, "tenant-use-locks")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return os.OpenFile(filepath.Join(dir, id+".lock"), os.O_CREATE|os.O_RDWR, 0o600)
}

func (r *Registry) AcquireUse(id string) (*UseLock, error) {
	file, err := r.openUseLock(id)
	if err != nil {
		return nil, err
	}
	return &UseLock{file: file}, nil
}

func (r *Registry) TryAcquireExclusiveUse(id string) (*UseLock, bool, error) {
	file, err := r.openUseLock(id)
	if err != nil {
		return nil, false, err
	}
	return &UseLock{file: file}, true, nil
}

func (l *UseLock) Close() error {
	return l.file.Close()
}
