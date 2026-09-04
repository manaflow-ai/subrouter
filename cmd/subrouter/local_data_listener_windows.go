//go:build windows

package main

import (
	"errors"
	"net"
	"os"
)

func localDataSocketIdentity(string) (string, error) {
	return "", errors.New("local data sockets are unsupported on Windows")
}

type localDataSocketLease struct{}

func acquireLocalDataSocketLease(string) (*localDataSocketLease, error) {
	return nil, errors.New("local data sockets are unsupported on Windows")
}
func (l *localDataSocketLease) Close() error                   { return nil }
func (l *localDataSocketLease) parentMatches(os.FileInfo) bool { return false }
func (l *localDataSocketLease) removeStaleSocket() error {
	return errors.New("local data sockets are unsupported on Windows")
}
func wrapOwnedLocalDataListener(net.Listener, string, *localDataSocketLease) (net.Listener, error) {
	return nil, errors.New("local data sockets are unsupported on Windows")
}
func unixFchmodatLocalDataSocket(*localDataSocketLease, os.FileMode) error {
	return errors.New("local data sockets are unsupported on Windows")
}
