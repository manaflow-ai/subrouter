//go:build !windows

package claude

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

var profileRegistryProcessMu sync.Mutex

type profileRegistryLock struct {
	file *os.File
}

type profileCredentialLock struct {
	file           *os.File
	releaseProcess func()
}

// lockProfileRegistry serializes registry mutations within one process and
// across overlapping supervisor worker generations.
func lockProfileRegistry(path string) (*profileRegistryLock, error) {
	profileRegistryProcessMu.Lock()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		profileRegistryProcessMu.Unlock()
		return nil, err
	}
	file, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		profileRegistryProcessMu.Unlock()
		return nil, err
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX); err != nil {
		_ = file.Close()
		profileRegistryProcessMu.Unlock()
		return nil, err
	}
	return &profileRegistryLock{file: file}, nil
}

func (l *profileRegistryLock) Close() error {
	unlockErr := syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN)
	closeErr := l.file.Close()
	profileRegistryProcessMu.Unlock()
	if unlockErr != nil {
		return unlockErr
	}
	return closeErr
}

// lockProfileCredential serializes one profile's rotating OAuth credential
// across goroutines and overlapping supervisor worker generations.
func lockProfileCredential(ctx context.Context, instancePath string) (*profileCredentialLock, error) {
	if resolved, err := filepath.EvalSymlinks(instancePath); err == nil {
		instancePath = resolved
	}
	path := filepath.Clean(instancePath) + ".credentials.lock"
	releaseProcess, err := lockProfileCredentialProcess(ctx, path)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		releaseProcess()
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		releaseProcess()
		return nil, err
	}
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return &profileCredentialLock{file: file, releaseProcess: releaseProcess}, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			_ = file.Close()
			releaseProcess()
			return nil, err
		}
		select {
		case <-ctx.Done():
			_ = file.Close()
			releaseProcess()
			return nil, ctx.Err()
		case <-ticker.C:
		}
	}
}

func (l *profileCredentialLock) Close() error {
	unlockErr := syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN)
	closeErr := l.file.Close()
	l.releaseProcess()
	if unlockErr != nil {
		return unlockErr
	}
	return closeErr
}
