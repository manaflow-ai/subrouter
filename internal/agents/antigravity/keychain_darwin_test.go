//go:build darwin

package antigravity

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestAcquireNativeProfileRestoresExactOriginalBlob(t *testing.T) {
	lockPath := filepath.Join(t.TempDir(), "agy.lock")
	original := KeychainEntry{Account: "antigravity", Blob: []byte("original-opaque-blob")}
	current := original
	read := func(context.Context) (KeychainEntry, bool, error) { return current, true, nil }
	write := func(_ context.Context, entry KeychainEntry) error {
		current = KeychainEntry{Account: entry.Account, Blob: append([]byte(nil), entry.Blob...)}
		return nil
	}
	delete := func(context.Context, string) error { current = KeychainEntry{}; return nil }
	lease, err := acquireNativeProfileWith(context.Background(), lockPath, CredentialInfo{AccessToken: "a", RefreshToken: "r", ExpiresAt: time.Now().Add(time.Hour)}, read, write, delete)
	if err != nil {
		t.Fatal(err)
	}
	if string(current.Blob) == string(original.Blob) || current.Account != original.Account {
		t.Fatalf("profile was not installed: %+v", current)
	}
	if _, err := os.Stat(nativeProfileJournalPath(lockPath)); err != nil {
		t.Fatalf("profile journal missing while lease is active: %v", err)
	}
	if err := lease.Restore(context.Background()); err != nil {
		t.Fatal(err)
	}
	if string(current.Blob) != string(original.Blob) || current.Account != original.Account {
		t.Fatalf("restored entry = %+v, want exact original", current)
	}
	if _, err := os.Stat(nativeProfileJournalPath(lockPath)); !os.IsNotExist(err) {
		t.Fatalf("profile journal still present after restore: %v", err)
	}
	if err := lease.Restore(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(lockPath); err != nil {
		t.Fatal(err)
	}
}

func TestRecoverNativeProfileRestoresStaleJournal(t *testing.T) {
	lockPath := filepath.Join(t.TempDir(), "agy.lock")
	if err := writeNativeProfileJournal(lockPath, nativeProfileJournal{
		Account: "antigravity", OriginalBlob: []byte("before-crash"), HadOriginal: true,
	}); err != nil {
		t.Fatal(err)
	}
	var restored KeychainEntry
	write := func(_ context.Context, entry KeychainEntry) error {
		restored = KeychainEntry{Account: entry.Account, Blob: append([]byte(nil), entry.Blob...)}
		return nil
	}
	delete := func(context.Context, string) error { t.Fatal("unexpected delete"); return nil }
	if err := recoverNativeProfileLocked(context.Background(), lockPath, write, delete); err != nil {
		t.Fatal(err)
	}
	if restored.Account != "antigravity" || string(restored.Blob) != "before-crash" {
		t.Fatalf("restored = %+v", restored)
	}
	if _, err := os.Stat(nativeProfileJournalPath(lockPath)); !os.IsNotExist(err) {
		t.Fatalf("stale journal was not removed: %v", err)
	}
}

func TestAcquireNativeProfileHonorsLockCancellation(t *testing.T) {
	lockPath := filepath.Join(t.TempDir(), "agy.lock")
	first, err := acquireNativeProfileWith(context.Background(), lockPath,
		CredentialInfo{AccessToken: "a", RefreshToken: "r", ExpiresAt: time.Now().Add(time.Hour)},
		func(context.Context) (KeychainEntry, bool, error) {
			return KeychainEntry{Account: "antigravity", Blob: []byte("original")}, true, nil
		},
		func(context.Context, KeychainEntry) error { return nil },
		func(context.Context, string) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	defer first.Restore(context.Background())
	ctx, cancel := context.WithTimeout(context.Background(), 75*time.Millisecond)
	defer cancel()
	_, err = acquireNativeProfileWith(ctx, lockPath,
		CredentialInfo{AccessToken: "b", RefreshToken: "s", ExpiresAt: time.Now().Add(time.Hour)},
		func(context.Context) (KeychainEntry, bool, error) {
			return KeychainEntry{Account: "antigravity", Blob: []byte("original")}, true, nil
		},
		func(context.Context, KeychainEntry) error { return nil },
		func(context.Context, string) error { return nil })
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("second launch error = %v, want context deadline", err)
	}
}
