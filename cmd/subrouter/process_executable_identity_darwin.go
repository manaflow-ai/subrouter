//go:build darwin

package main

import (
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"syscall"
	"unsafe"
)

const supervisorCSOpsCDHash = 5

func executableIdentityForProcess(pid int) (processExecutableIdentity, error) {
	digest := make([]byte, 20)
	_, _, errno := syscall.Syscall6(
		syscall.SYS_CSOPS,
		uintptr(pid),
		supervisorCSOpsCDHash,
		uintptr(unsafe.Pointer(&digest[0])),
		uintptr(len(digest)),
		0,
		0,
	)
	if errno != 0 {
		return processExecutableIdentity{}, fmt.Errorf("csops(CS_OPS_CDHASH) for pid %d: %w", pid, errno)
	}
	start, err := darwinProcessStartIdentity(pid)
	if err != nil {
		return processExecutableIdentity{}, err
	}
	return processExecutableIdentity{Kind: "darwin-cdhash-sha256", Value: hex.EncodeToString(digest), StartIdentity: start}, nil
}

func darwinProcessStartIdentity(pid int) (string, error) {
	// PROC_PIDTBSDINFO's start-time fields are at byte offsets 120 and 128 in
	// proc_bsdinfo on macOS. Querying the kernel binds a reused numeric PID to
	// the exact process generation whose executable identity we attest.
	const (
		procPIDTBSDInfo = 3
		bsdInfoSize     = 136
	)
	buffer := make([]byte, bsdInfoSize)
	result, _, errno := syscall.Syscall6(
		syscall.SYS_PROC_INFO,
		2, // PROC_INFO_CALL_PIDINFO
		uintptr(pid),
		procPIDTBSDInfo,
		0,
		uintptr(unsafe.Pointer(&buffer[0])),
		uintptr(len(buffer)),
	)
	if errno != 0 {
		return "", fmt.Errorf("proc_pidinfo(PROC_PIDTBSDINFO) for pid %d: %w", pid, errno)
	}
	if result != uintptr(len(buffer)) {
		return "", fmt.Errorf("proc_pidinfo(PROC_PIDTBSDINFO) for pid %d returned %d bytes", pid, result)
	}
	seconds := binary.LittleEndian.Uint64(buffer[120:128])
	microseconds := binary.LittleEndian.Uint64(buffer[128:136])
	if seconds == 0 {
		return "", fmt.Errorf("process start identity unavailable for pid %d", pid)
	}
	return fmt.Sprintf("darwin:%d:%d", seconds, microseconds), nil
}
