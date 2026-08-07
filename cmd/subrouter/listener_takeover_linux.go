//go:build linux

package main

import (
	"fmt"
	"net"
	"os"

	"golang.org/x/sys/unix"
)

func openPublicListener(address string, takeoverPID, takeoverFD int) (net.Listener, error) {
	if takeoverPID != 0 {
		return takeoverTCPListener(takeoverPID, takeoverFD, address)
	}
	return openFreshPublicListener(address)
}

// takeoverTCPListener duplicates a live listener from another process without
// closing or rebinding the kernel socket. Both processes may accept during the
// handoff; retiring the previous owner later only closes its descriptor.
func takeoverTCPListener(pid, targetFD int, expectedAddress string) (net.Listener, error) {
	if pid <= 1 || targetFD < 0 {
		return nil, fmt.Errorf("invalid listener takeover source pid=%d fd=%d", pid, targetFD)
	}
	pidfd, err := unix.PidfdOpen(pid, 0)
	if err != nil {
		return nil, fmt.Errorf("open listener owner pid %d: %w", pid, err)
	}
	defer unix.Close(pidfd)
	duplicate, err := unix.PidfdGetfd(pidfd, targetFD, 0)
	if err != nil {
		return nil, fmt.Errorf("duplicate listener fd %d from pid %d: %w", targetFD, pid, err)
	}
	file := os.NewFile(uintptr(duplicate), "subrouter-public-listener-takeover")
	if file == nil {
		_ = unix.Close(duplicate)
		return nil, fmt.Errorf("listener fd %d is unavailable", duplicate)
	}
	listener, err := net.FileListener(file)
	closeErr := file.Close()
	if err != nil {
		return nil, fmt.Errorf("open duplicated listener: %w", err)
	}
	if closeErr != nil {
		_ = listener.Close()
		return nil, fmt.Errorf("close duplicated listener file: %w", closeErr)
	}
	if _, ok := listener.(*net.TCPListener); !ok || !listenerCoversConfiguredAddress(listener, expectedAddress) {
		actual := listener.Addr().String()
		_ = listener.Close()
		return nil, fmt.Errorf("taken listener address %q does not match %q", actual, expectedAddress)
	}
	return listener, nil
}
