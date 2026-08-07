//go:build !linux

package main

import "net"

func listenerIPv6WildcardAcceptsIPv4(net.Listener) bool {
	return false
}
