//go:build linux

package main

import (
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

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
		return nil, errorsNewListenerFDUnavailable(duplicate)
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
	if _, ok := listener.(*net.TCPListener); !ok || !listenerAddressMatches(listener.Addr(), expectedAddress) {
		actual := listener.Addr().String()
		_ = listener.Close()
		return nil, fmt.Errorf("taken listener address %q does not match %q", actual, expectedAddress)
	}
	return listener, nil
}

func errorsNewListenerFDUnavailable(fd int) error {
	return fmt.Errorf("listener fd %d is unavailable", fd)
}

func listenerAddressMatches(actual net.Addr, expected string) bool {
	actualHost, actualPort, err := net.SplitHostPort(actual.String())
	if err != nil {
		return false
	}
	expectedHost, expectedPort, err := net.SplitHostPort(expected)
	if err != nil || actualPort != expectedPort {
		return false
	}
	expectedHost = strings.TrimSpace(expectedHost)
	if expectedHost == "" || expectedHost == "0.0.0.0" || expectedHost == "::" {
		return true
	}
	actualIP, expectedIP := net.ParseIP(actualHost), net.ParseIP(expectedHost)
	if actualIP != nil && expectedIP != nil {
		return actualIP.Equal(expectedIP)
	}
	actualPortNumber, actualErr := strconv.Atoi(actualPort)
	expectedPortNumber, expectedErr := strconv.Atoi(expectedPort)
	return actualErr == nil && expectedErr == nil && actualPortNumber == expectedPortNumber &&
		strings.EqualFold(actualHost, expectedHost)
}
