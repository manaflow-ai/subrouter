//go:build !linux

package main

import (
	"fmt"
	"net"
)

func openPublicListener(address string, takeoverPID, takeoverFD int) (net.Listener, error) {
	if takeoverPID != 0 {
		return takeoverTCPListener(takeoverPID, takeoverFD, address)
	}
	return openFreshPublicListener(address)
}

func takeoverTCPListener(pid, targetFD int, expectedAddress string) (net.Listener, error) {
	return nil, fmt.Errorf(
		"listener takeover from pid %d fd %d for %s is supported only on Linux",
		pid, targetFD, expectedAddress,
	)
}
