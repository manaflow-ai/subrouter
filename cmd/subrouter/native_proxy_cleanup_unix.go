//go:build aix || android || darwin || dragonfly || freebsd || illumos || ios || linux || netbsd || openbsd || solaris

package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"golang.org/x/sys/unix"
)

// removePrivateProxyHome removes a child-controlled directory tree without
// following links the child may have placed anywhere in it.
func removePrivateProxyHome(path string) error {
	path = filepath.Clean(path)
	parentPath, name := filepath.Dir(path), filepath.Base(path)
	if name == "." || name == string(filepath.Separator) {
		return fmt.Errorf("refuse to remove private proxy home %q", path)
	}
	parentFD, err := unix.Open(parentPath, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_DIRECTORY, 0)
	if err != nil {
		return fmt.Errorf("open private proxy home parent: %w", err)
	}
	defer unix.Close(parentFD)
	return removePrivateProxyEntryAt(parentFD, name)
}

func preparePrivateProxyHomeCleanup(path string) (func() error, error) {
	path = filepath.Clean(path)
	parentPath, name := filepath.Dir(path), filepath.Base(path)
	if name == "." || name == string(filepath.Separator) {
		return nil, fmt.Errorf("refuse to prepare private proxy home cleanup for %q", path)
	}
	parentFD, err := unix.Open(parentPath, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_DIRECTORY, 0)
	if err != nil {
		return nil, fmt.Errorf("open private proxy home parent: %w", err)
	}
	var before unix.Stat_t
	if err := unix.Fstatat(parentFD, name, &before, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		unix.Close(parentFD)
		return nil, fmt.Errorf("inspect private proxy home: %w", err)
	}
	if before.Mode&unix.S_IFMT != unix.S_IFDIR {
		unix.Close(parentFD)
		return nil, fmt.Errorf("private proxy home must be a directory")
	}
	homeFD, err := openPrivateProxyDirectory(parentFD, name, before)
	if err != nil {
		unix.Close(parentFD)
		return nil, fmt.Errorf("pin private proxy home: %w", err)
	}
	var opened unix.Stat_t
	if err := unix.Fstat(homeFD, &opened); err != nil || !samePrivateProxyNode(before, opened) {
		unix.Close(homeFD)
		unix.Close(parentFD)
		if err != nil {
			return nil, fmt.Errorf("inspect pinned private proxy home: %w", err)
		}
		return nil, fmt.Errorf("private proxy home changed while preparing cleanup")
	}
	var once sync.Once
	var cleanupErr error
	cleanup := func() error {
		once.Do(func() {
			var contentsErr error
			if err := unix.Fchmod(homeFD, 0o700); err != nil {
				contentsErr = fmt.Errorf("restore pinned private proxy home: %w", err)
				_ = unix.Close(homeFD)
			} else {
				contentsErr = removePrivateProxyDirectoryContents(homeFD)
			}
			var removeErr error
			if contentsErr == nil {
				var current unix.Stat_t
				if err := unix.Fstatat(parentFD, name, &current, unix.AT_SYMLINK_NOFOLLOW); err != nil {
					if !errors.Is(err, unix.ENOENT) {
						removeErr = fmt.Errorf("reinspect private proxy home: %w", err)
					}
				} else if !samePrivateProxyNode(opened, current) {
					removeErr = fmt.Errorf("private proxy home basename was replaced during cleanup")
				} else {
					if err := unix.Unlinkat(parentFD, name, unix.AT_REMOVEDIR); err != nil && !errors.Is(err, unix.ENOENT) {
						removeErr = fmt.Errorf("remove private proxy home basename: %w", err)
					}
				}
			}
			closeErr := unix.Close(parentFD)
			cleanupErr = errors.Join(contentsErr, removeErr, closeErr)
		})
		return cleanupErr
	}
	return cleanup, nil
}

func removePrivateProxyEntryAt(parentFD int, name string) error {
	var before unix.Stat_t
	if err := unix.Fstatat(parentFD, name, &before, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		if errors.Is(err, unix.ENOENT) {
			return nil
		}
		return fmt.Errorf("inspect %q: %w", name, err)
	}
	if before.Mode&unix.S_IFMT != unix.S_IFDIR {
		if err := unix.Unlinkat(parentFD, name, 0); err != nil && !errors.Is(err, unix.ENOENT) {
			return fmt.Errorf("unlink %q: %w", name, err)
		}
		return nil
	}

	fd, err := openPrivateProxyDirectory(parentFD, name, before)
	if err != nil {
		return fmt.Errorf("open private proxy directory %q: %w", name, err)
	}
	var opened unix.Stat_t
	if err := unix.Fstat(fd, &opened); err != nil {
		unix.Close(fd)
		return fmt.Errorf("inspect opened private proxy directory %q: %w", name, err)
	}
	if !samePrivateProxyNode(before, opened) {
		unix.Close(fd)
		return fmt.Errorf("private proxy directory %q changed during cleanup", name)
	}
	if err := unix.Fchmod(fd, 0o700); err != nil {
		unix.Close(fd)
		return fmt.Errorf("restore opened private proxy directory %q: %w", name, err)
	}
	if err := removePrivateProxyDirectoryContents(fd); err != nil {
		return fmt.Errorf("remove private proxy directory %q: %w", name, err)
	}

	var current unix.Stat_t
	if err := unix.Fstatat(parentFD, name, &current, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		if errors.Is(err, unix.ENOENT) {
			return nil
		}
		return fmt.Errorf("reinspect private proxy directory %q: %w", name, err)
	}
	if !samePrivateProxyNode(opened, current) {
		return fmt.Errorf("private proxy directory %q was replaced during cleanup", name)
	}
	if err := unix.Unlinkat(parentFD, name, unix.AT_REMOVEDIR); err != nil && !errors.Is(err, unix.ENOENT) {
		return fmt.Errorf("remove private proxy directory %q: %w", name, err)
	}
	return nil
}

func removePrivateProxyDirectoryContents(fd int) error {
	file := os.NewFile(uintptr(fd), "private-proxy-home")
	if file == nil {
		unix.Close(fd)
		return errors.New("wrap private proxy directory descriptor")
	}
	entries, err := file.ReadDir(-1)
	if err != nil {
		return errors.Join(err, file.Close())
	}
	for _, entry := range entries {
		if err := removePrivateProxyEntryAt(fd, entry.Name()); err != nil {
			return errors.Join(err, file.Close())
		}
	}
	return file.Close()
}

func samePrivateProxyNode(a, b unix.Stat_t) bool {
	return a.Dev == b.Dev && a.Ino == b.Ino
}
