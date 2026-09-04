package antigravity

import "context"

// KeychainEntry is the opaque credential blob and account slot used by the
// native AGY client. Blob contents must never be logged or placed in argv.
type KeychainEntry struct {
	Account string
	Blob    []byte
}

// ReadLocalKeychainEntry reads the AGY login slot without decoding or
// modifying it. It is separate from ReadLocalCredential so a native launcher
// can restore the exact original bytes after a profile run.
func ReadLocalKeychainEntry(ctx context.Context) (KeychainEntry, bool, error) {
	return readLocalKeychainEntry(ctx)
}

// WriteLocalKeychainEntry atomically replaces the AGY login slot. The
// implementation must keep the blob out of command-line arguments.
func WriteLocalKeychainEntry(ctx context.Context, entry KeychainEntry) error {
	return writeLocalKeychainEntry(ctx, entry)
}

// DeleteLocalKeychainEntry removes the AGY login slot when no credential was
// present before a managed launch.
func DeleteLocalKeychainEntry(ctx context.Context, account string) error {
	return deleteLocalKeychainEntry(ctx, account)
}

// NativeProfileLease owns the process-wide AGY Keychain slot until Restore is
// called. The lease is deliberately held by the launcher for the whole child
// lifetime so another native AGY process cannot swap credentials underneath it.
type NativeProfileLease struct {
	restore func(context.Context) error
}

// AcquireNativeProfile switches the native AGY Keychain slot to credential and
// returns a crash-safe, idempotent restoration lease. lockPath must be a
// Subrouter-owned 0600 path shared by all native AGY launchers on the machine.
func AcquireNativeProfile(ctx context.Context, lockPath string, credential CredentialInfo) (*NativeProfileLease, error) {
	return acquireNativeProfile(ctx, lockPath, credential)
}

// RecoverNativeProfile restores a swap left behind by a hard kill or host
// crash. It is safe to call before every native launch and is a no-op when no
// recovery journal exists.
func RecoverNativeProfile(ctx context.Context, lockPath string) error {
	return recoverNativeProfile(ctx, lockPath)
}

// Restore puts the exact previous Keychain state back and releases the global
// profile lease. Calling Restore more than once is safe.
func (l *NativeProfileLease) Restore(ctx context.Context) error {
	if l == nil || l.restore == nil {
		return nil
	}
	restore := l.restore
	l.restore = nil
	return restore(ctx)
}
