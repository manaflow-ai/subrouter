//go:build !windows

package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

type localServingStoreBindingLock struct {
	file *os.File
}

func lockLocalServingStoreBinding(path string) (*localServingStoreBindingLock, error) {
	parentDescriptor, err := unix.Open(filepath.Dir(path), unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_DIRECTORY, 0)
	if err != nil {
		return nil, err
	}
	defer unix.Close(parentDescriptor)
	var parentStat unix.Stat_t
	if err := unix.Fstat(parentDescriptor, &parentStat); err != nil {
		return nil, err
	}
	if int(parentStat.Uid) != os.Geteuid() || parentStat.Mode&0o022 != 0 {
		return nil, errors.New("serving-store lock directory must be current-user-owned and not group/world writable")
	}
	descriptor, err := unix.Openat(parentDescriptor, ".local-serving-store.lock", unix.O_CREAT|unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(descriptor), filepath.Join(filepath.Dir(path), ".local-serving-store.lock"))
	var stat unix.Stat_t
	if err := unix.Fstat(descriptor, &stat); err != nil {
		_ = file.Close()
		return nil, err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Mode&0o077 != 0 || int(stat.Uid) != os.Geteuid() {
		_ = file.Close()
		return nil, errors.New("serving-store lock must be a current-user-owned private regular file")
	}
	if err := unix.Flock(descriptor, unix.LOCK_EX); err != nil {
		_ = file.Close()
		return nil, err
	}
	return &localServingStoreBindingLock{file: file}, nil
}

func (lock *localServingStoreBindingLock) Close() error {
	unlockErr := unix.Flock(int(lock.file.Fd()), unix.LOCK_UN)
	closeErr := lock.file.Close()
	if unlockErr != nil {
		return unlockErr
	}
	return closeErr
}

func openPrivateLocalServingStoreBinding(path string) (*os.File, error) {
	parentDescriptor, err := unix.Open(filepath.Dir(path), unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_DIRECTORY, 0)
	if err != nil {
		return nil, err
	}
	defer unix.Close(parentDescriptor)
	var parentStat unix.Stat_t
	if err := unix.Fstat(parentDescriptor, &parentStat); err != nil {
		return nil, err
	}
	if int(parentStat.Uid) != os.Geteuid() || parentStat.Mode&0o022 != 0 {
		return nil, errors.New("containing directory must be current-user-owned and not group/world writable")
	}
	descriptor, err := unix.Openat(parentDescriptor, filepath.Base(path), unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(descriptor), path)
	var stat unix.Stat_t
	if err := unix.Fstat(descriptor, &stat); err != nil {
		_ = file.Close()
		return nil, err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Mode&0o077 != 0 || int(stat.Uid) != os.Geteuid() {
		_ = file.Close()
		return nil, errors.New("path must be a current-user-owned private regular file")
	}
	return file, nil
}

func validatePrivateLocalServingStorePath(path string, directory bool) (string, error) {
	canonical, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	canonical, err = filepath.Abs(canonical)
	if err != nil {
		return "", err
	}
	flags := unix.O_RDONLY | unix.O_CLOEXEC | unix.O_NOFOLLOW
	if directory {
		flags |= unix.O_DIRECTORY
	}
	descriptor, err := unix.Open(canonical, flags, 0)
	if err != nil {
		return "", err
	}
	defer unix.Close(descriptor)
	var stat unix.Stat_t
	if err := unix.Fstat(descriptor, &stat); err != nil {
		return "", err
	}
	if int(stat.Uid) != os.Geteuid() || stat.Mode&0o022 != 0 {
		return "", fmt.Errorf("path must be current-user-owned and not group/world writable")
	}
	return filepath.Clean(canonical), nil
}

func syncLocalServingStoreBindingDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

func validatePrivateLocalDataSocket(path string) (string, error) {
	path = filepath.Clean(strings.TrimSpace(path))
	if !filepath.IsAbs(path) || path == string(filepath.Separator) {
		return "", errors.New("local data socket must be an absolute non-root path")
	}
	parent, err := validatePrivateLocalServingStorePath(filepath.Dir(path), true)
	if err != nil || parent != filepath.Dir(path) {
		return "", errors.New("local data socket parent must be canonical, current-user-owned, and private")
	}
	var stat unix.Stat_t
	if err := unix.Lstat(path, &stat); err != nil {
		return "", err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFSOCK || stat.Mode&0o077 != 0 || int(stat.Uid) != os.Geteuid() {
		return "", errors.New("local data socket must be a current-user-owned mode-0600 Unix socket")
	}
	return path, nil
}
