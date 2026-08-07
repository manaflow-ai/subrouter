//go:build unix

package main

import (
	"context"
	"fmt"
	"net"
	"syscall"
)

// openFreshPublicListener binds with SO_REUSEPORT so a replacement supervisor
// can take the address before the draining process exits (see #125).
func openFreshPublicListener(address string) (net.Listener, error) {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	network := "tcp"
	if ip := net.ParseIP(host); ip != nil {
		if ip.To4() != nil {
			network = "tcp4"
		} else {
			network = "tcp6"
		}
	}
	config := net.ListenConfig{
		Control: func(network, address string, c syscall.RawConn) error {
			var setErr error
			if err := c.Control(func(fd uintptr) {
				setErr = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_REUSEPORT, 1)
			}); err != nil {
				return err
			}
			if setErr != nil {
				return fmt.Errorf("SO_REUSEPORT: %w", setErr)
			}
			return nil
		},
	}
	return config.Listen(context.Background(), network, address)
}
