//go:build !aix && !android && !darwin && !dragonfly && !freebsd && !illumos && !ios && !linux && !netbsd && !openbsd && !solaris

package main

import (
	"errors"
	"net"
)

func inheritedFrontProcessFromEnvironment(frontConfig) (*inheritedFrontProcess, error) {
	return nil, nil
}

func startFrontSuccessor(frontConfig, net.Listener, net.Listener, net.Listener) (frontSuccessor, error) {
	return nil, errors.New("front hot reload is supported only on Unix")
}

func promoteFrontSuccessor(int) error {
	return errors.New("front hot reload is supported only on Unix")
}
