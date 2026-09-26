//go:build !windows

package tenant

import (
	"os"
	"syscall"
)

type registryLock struct {
	file *os.File
}

// lockRegistry takes an exclusive flock on a sibling lock file so registry
// mutations are serialized across processes (the server's admin API and a
// local sr tenant run on the same host share one tenants.json). Same pattern
// as accounts.lockStoredAccount.
func (r *Registry) lockRegistry() (*registryLock, error) {
	if err := os.MkdirAll(r.stateDir, 0o700); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(r.Path()+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX); err != nil {
		_ = file.Close()
		return nil, err
	}
	return &registryLock{file: file}, nil
}

func (l *registryLock) Close() error {
	unlockErr := syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN)
	closeErr := l.file.Close()
	if unlockErr != nil {
		return unlockErr
	}
	return closeErr
}
