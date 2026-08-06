//go:build !linux

package main

import "net"

func storeFrontListener(net.Listener) error {
	return nil
}

func removeStoredFrontListener(net.Addr) error {
	return nil
}
