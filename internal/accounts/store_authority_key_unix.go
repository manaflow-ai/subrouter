//go:build !windows

package accounts

import (
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

func openPrivateStoreAuthorityKey(path string) (*os.File, error) {
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
		return nil, fmt.Errorf("account store authority directory must be current-user-owned and not group/world writable")
	}
	descriptor, err := unix.Openat(parentDescriptor, filepath.Base(path), unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
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
		return nil, fmt.Errorf("account store authority key must be a current-user-owned private regular file")
	}
	return file, nil
}
