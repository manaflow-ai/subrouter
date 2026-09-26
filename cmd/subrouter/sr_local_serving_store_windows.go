//go:build windows

package main

import (
	"errors"
	"os"
)

type localServingStoreBindingLock struct{}

func lockLocalServingStoreBinding(string) (*localServingStoreBindingLock, error) {
	return nil, errors.New("local serving-store bindings are not supported on Windows")
}

func (*localServingStoreBindingLock) Close() error { return nil }

func openPrivateLocalServingStoreBinding(string) (*os.File, error) {
	return nil, errors.New("local serving-store bindings are not supported on Windows")
}

func validatePrivateLocalServingStorePath(string, bool) (string, error) {
	return "", errors.New("local serving-store bindings are not supported on Windows")
}

func syncLocalServingStoreBindingDirectory(string) error {
	return errors.New("local serving-store bindings are not supported on Windows")
}

func validatePrivateLocalDataSocket(string) (string, error) {
	return "", errors.New("local data sockets are not supported on Windows")
}
