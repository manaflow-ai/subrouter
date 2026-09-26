//go:build !windows

package tenant

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
)

// UseLock coordinates live tenant requests with permanent tenant deletion
// across supervisor worker generations.
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
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_SH); err != nil {
		_ = file.Close()
		return nil, err
	}
	return &UseLock{file: file}, nil
}

func (r *Registry) TryAcquireExclusiveUse(id string) (*UseLock, bool, error) {
	file, err := r.openUseLock(id)
	if err != nil {
		return nil, false, err
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = file.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, false, nil
		}
		return nil, false, err
	}
	return &UseLock{file: file}, true, nil
}

// AcquireExclusiveUse waits until every active request has released its shared
// tenant-use lock, then holds the deletion lock.
func (r *Registry) AcquireExclusiveUse(id string) (*UseLock, error) {
	file, err := r.openUseLock(id)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX); err != nil {
		_ = file.Close()
		return nil, err
	}
	return &UseLock{file: file}, nil
}

func (l *UseLock) Close() error {
	unlockErr := syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN)
	closeErr := l.file.Close()
	if unlockErr != nil {
		return unlockErr
	}
	return closeErr
}
