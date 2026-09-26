//go:build !aix && !android && !darwin && !dragonfly && !freebsd && !illumos && !ios && !linux && !netbsd && !openbsd && !solaris

package main

import (
	"fmt"
	"net"
)

func listenForTransferredListeners(path string) (net.Listener, error) {
	return nil, fmt.Errorf("listener transfer socket %q is supported only on Unix", path)
}

func (f *stableFront) serveListenerTransfers(net.Listener) error {
	return fmt.Errorf("listener transfers are supported only on Unix")
}

func runListenerTransfer([]string) error {
	return fmt.Errorf("listener transfers are supported only on Unix")
}
