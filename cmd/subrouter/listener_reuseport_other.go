//go:build !unix

package main

import (
	"context"
	"net"
)

// openFreshPublicListener falls back to a plain Listen where SO_REUSEPORT is unavailable.
func openFreshPublicListener(address string) (net.Listener, error) {
	return (&net.ListenConfig{}).Listen(context.Background(), "tcp", address)
}
