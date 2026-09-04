//go:build !darwin

package antigravity

import (
	"context"
	"errors"
)

func acquireNativeProfile(context.Context, string, CredentialInfo) (*NativeProfileLease, error) {
	return nil, errors.New("native Antigravity profile switching is supported only on macOS")
}

func recoverNativeProfile(context.Context, string) error {
	return errors.New("native Antigravity profile switching is supported only on macOS")
}

func readLocalKeychainEntry(context.Context) (KeychainEntry, bool, error) {
	return KeychainEntry{}, false, nil
}

func writeLocalKeychainEntry(context.Context, KeychainEntry) error {
	return errors.New("native Antigravity profile switching is supported only on macOS")
}

func deleteLocalKeychainEntry(context.Context, string) error {
	return errors.New("native Antigravity profile switching is supported only on macOS")
}
