//go:build aix || darwin || dragonfly || freebsd || illumos || ios || netbsd || openbsd || solaris

package main

import (
	"errors"
	"fmt"

	"golang.org/x/sys/unix"
)

func openPrivateProxyDirectory(parentFD int, name string, _ unix.Stat_t) (int, error) {
	fd, err := unix.Openat(parentFD, name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_DIRECTORY, 0)
	if err == nil {
		return fd, nil
	}
	if !errors.Is(err, unix.EACCES) && !errors.Is(err, unix.EPERM) {
		return -1, err
	}
	if chmodErr := unix.Fchmodat(parentFD, name, 0o700, unix.AT_SYMLINK_NOFOLLOW); chmodErr != nil {
		return -1, fmt.Errorf("restore permissions: %w", chmodErr)
	}
	fd, err = unix.Openat(parentFD, name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_DIRECTORY, 0)
	if err != nil {
		return -1, fmt.Errorf("open repaired directory: %w", err)
	}
	return fd, nil
}
