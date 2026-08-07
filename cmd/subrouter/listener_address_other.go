//go:build !(aix || android || darwin || dragonfly || freebsd || illumos || ios || linux || netbsd || openbsd || solaris)

package main

import "net"

func listenerIPv6WildcardAcceptsIPv4(net.Listener) bool {
	return false
}
