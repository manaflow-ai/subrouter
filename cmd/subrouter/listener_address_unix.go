//go:build aix || android || darwin || dragonfly || freebsd || illumos || ios || linux || netbsd || openbsd || solaris

package main

import (
	"net"

	"golang.org/x/sys/unix"
)

func listenerIPv6WildcardAcceptsIPv4(listener net.Listener) bool {
	tcp, ok := listener.(*net.TCPListener)
	if !ok {
		return false
	}
	raw, err := tcp.SyscallConn()
	if err != nil {
		return false
	}
	v6Only := 1
	var optionErr error
	if err := raw.Control(func(descriptor uintptr) {
		v6Only, optionErr = unix.GetsockoptInt(int(descriptor), unix.IPPROTO_IPV6, unix.IPV6_V6ONLY)
	}); err != nil || optionErr != nil {
		return false
	}
	return v6Only == 0
}
