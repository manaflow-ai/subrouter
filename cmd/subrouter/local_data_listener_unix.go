//go:build !windows

package main

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"

	"golang.org/x/sys/unix"
)

func localDataSocketIdentity(socket string) (string, error) {
	var stat unix.Stat_t
	if err := unix.Lstat(socket, &stat); err != nil {
		return "", err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFSOCK {
		return "", errors.New("local data socket identity requires a Unix socket")
	}
	return fmt.Sprintf("unix:%d:%d", uint64(stat.Dev), stat.Ino), nil
}

type localDataSocketLease struct {
	file   *os.File
	parent *os.File
	name   string
}

func acquireLocalDataSocketLease(socket string) (*localDataSocketLease, error) {
	parentFD, err := unix.Open(filepath.Dir(socket), unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_DIRECTORY, 0)
	if err != nil {
		return nil, err
	}
	fd, err := unix.Openat(parentFD, filepath.Base(socket)+".lock", unix.O_CREAT|unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		unix.Close(parentFD)
		return nil, err
	}
	file := os.NewFile(uintptr(fd), socket+".lock")
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil || stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Mode&0o077 != 0 || int(stat.Uid) != os.Geteuid() {
		_ = file.Close()
		unix.Close(parentFD)
		if err != nil {
			return nil, err
		}
		return nil, errors.New("local data socket lock must be a current-user-owned private regular file")
	}
	if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = file.Close()
		unix.Close(parentFD)
		return nil, fmt.Errorf("local data socket is already owned: %w", err)
	}
	return &localDataSocketLease{
		file:   file,
		parent: os.NewFile(uintptr(parentFD), filepath.Dir(socket)),
		name:   filepath.Base(socket),
	}, nil
}

func (l *localDataSocketLease) Close() error {
	if l == nil || l.file == nil {
		return nil
	}
	_ = unix.Flock(int(l.file.Fd()), unix.LOCK_UN)
	lockErr := l.file.Close()
	parentErr := l.parent.Close()
	if lockErr != nil {
		return lockErr
	}
	return parentErr
}

func (l *localDataSocketLease) parentMatches(info os.FileInfo) bool {
	held, err := l.parent.Stat()
	return err == nil && os.SameFile(info, held)
}

func (l *localDataSocketLease) removeStaleSocket() error {
	var stat unix.Stat_t
	err := unix.Fstatat(int(l.parent.Fd()), l.name, &stat, unix.AT_SYMLINK_NOFOLLOW)
	if errors.Is(err, unix.ENOENT) {
		return nil
	}
	if err != nil {
		return err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFSOCK || stat.Mode&0o077 != 0 || int(stat.Uid) != os.Geteuid() {
		return errors.New("refuse unsafe stale socket: local data socket must be a current-user-owned mode-0600 Unix socket")
	}
	if err := unix.Unlinkat(int(l.parent.Fd()), l.name, 0); err != nil && !errors.Is(err, unix.ENOENT) {
		return err
	}
	return nil
}

func unixFchmodatLocalDataSocket(lease *localDataSocketLease, mode os.FileMode) error {
	return unix.Fchmodat(int(lease.parent.Fd()), lease.name, uint32(mode.Perm()), 0)
}

type ownedLocalDataListener struct {
	net.Listener
	lease *localDataSocketLease
	name  string
	dev   uint64
	ino   uint64
	once  sync.Once
	err   error
}

func wrapOwnedLocalDataListener(listener net.Listener, socket string, lease *localDataSocketLease) (net.Listener, error) {
	var stat unix.Stat_t
	if err := unix.Fstatat(int(lease.parent.Fd()), lease.name, &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return nil, err
	}
	return &ownedLocalDataListener{Listener: listener, lease: lease, name: lease.name, dev: uint64(stat.Dev), ino: stat.Ino}, nil
}

func (l *ownedLocalDataListener) Close() error {
	l.once.Do(func() {
		closeErr := l.Listener.Close()
		var stat unix.Stat_t
		if err := unix.Fstatat(int(l.lease.parent.Fd()), l.name, &stat, unix.AT_SYMLINK_NOFOLLOW); err == nil && uint64(stat.Dev) == l.dev && stat.Ino == l.ino {
			if err := unix.Unlinkat(int(l.lease.parent.Fd()), l.name, 0); err != nil && !errors.Is(err, unix.ENOENT) {
				l.err = err
			}
		}
		_ = l.lease.Close()
		if l.err == nil && closeErr != nil && !errors.Is(closeErr, net.ErrClosed) {
			l.err = closeErr
		}
	})
	return l.err
}
