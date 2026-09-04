//go:build linux || android

package main

import (
	"errors"
	"fmt"
	"strconv"

	"golang.org/x/sys/unix"
)

func openPrivateProxyDirectory(parentFD int, name string, before unix.Stat_t) (int, error) {
	fd, err := unix.Openat(parentFD, name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_DIRECTORY, 0)
	if err == nil {
		return fd, nil
	}
	if !errors.Is(err, unix.EACCES) && !errors.Is(err, unix.EPERM) {
		return -1, err
	}

	// O_PATH acquires the exact directory without requiring read permission.
	// Repair that descriptor through procfs instead of using Fchmodat with
	// AT_SYMLINK_NOFOLLOW, whose race-safe Linux implementation needs the
	// fchmodat2 syscall added in Linux 6.5.
	pathFD, pathErr := unix.Openat(parentFD, name, unix.O_PATH|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_DIRECTORY, 0)
	if pathErr != nil {
		return -1, fmt.Errorf("open permission-repair descriptor: %w", pathErr)
	}
	defer unix.Close(pathFD)

	var held unix.Stat_t
	if statErr := unix.Fstat(pathFD, &held); statErr != nil {
		return -1, fmt.Errorf("inspect permission-repair descriptor: %w", statErr)
	}
	if !samePrivateProxyNode(before, held) {
		return -1, errors.New("directory changed before permission repair")
	}
	procPath := "/proc/self/fd/" + strconv.Itoa(pathFD)
	if chmodErr := unix.Chmod(procPath, 0o700); chmodErr != nil {
		return -1, fmt.Errorf("repair exact directory through procfs: %w", chmodErr)
	}

	fd, err = unix.Openat(pathFD, ".", unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_DIRECTORY, 0)
	if err != nil {
		return -1, fmt.Errorf("open repaired directory descriptor: %w", err)
	}
	return fd, nil
}
