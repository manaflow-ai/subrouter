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
	return lockProfileRegistryContext(context.Background(), path)
}

func lockProfileRegistryContext(ctx context.Context, path string) (*profileRegistryLock, error) {
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for !profileRegistryProcessMu.TryLock() {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		profileRegistryProcessMu.Unlock()
		return nil, err
	}
	file, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		profileRegistryProcessMu.Unlock()
		return nil, err
	}
	for {
		err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return &profileRegistryLock{file: file}, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			_ = file.Close()
			profileRegistryProcessMu.Unlock()
			return nil, err
		}
		select {
		case <-ctx.Done():
			_ = file.Close()
			profileRegistryProcessMu.Unlock()
			return nil, ctx.Err()
		case <-ticker.C:
		}
	}
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

type profileRefreshLock struct {
	file           *os.File
	releaseProcess func()
}

// lockProfileRefresh serializes one profile's OAuth refresh network
// round-trip across goroutines and overlapping supervisor worker
// generations. It is distinct from lockProfileCredential so that a
// long-running refresh never blocks an unrelated ImportProfileCredential
// call on the same profile.
func lockProfileRefresh(ctx context.Context, instancePath string) (*profileRefreshLock, error) {
	if resolved, err := filepath.EvalSymlinks(instancePath); err == nil {
		instancePath = resolved
	}
	path := filepath.Clean(instancePath) + ".refresh.lock"
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
			return &profileRefreshLock{file: file, releaseProcess: releaseProcess}, nil
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

func (l *profileRefreshLock) Close() error {
	unlockErr := syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN)
	closeErr := l.file.Close()
	l.releaseProcess()
	if unlockErr != nil {
		return unlockErr
	}
	return closeErr
}
