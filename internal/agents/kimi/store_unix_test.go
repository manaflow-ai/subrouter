//go:build !windows

package kimi

import (
	"os"
	"path/filepath"
	"testing"
)

func TestManagedAccountRemovalDeletesDanglingSymlink(t *testing.T) {
	store := Store{Path: filepath.Join(t.TempDir(), "unused-cli.json"), ManagedDir: t.TempDir()}
	filename, err := managedFilename("kimi-subscription:broken")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(store.ManagedDir, filename)
	target := filepath.Join(t.TempDir(), "missing.json")
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	removed, ok, err := store.RemoveManagedAccount("broken")
	if err != nil || !ok || removed.ID != "kimi-subscription:broken" {
		t.Fatalf("remove dangling credential: removed=%+v ok=%v err=%v", removed, ok, err)
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("dangling credential remains after removal: %v", err)
	}
	if _, err := os.Lstat(target); !os.IsNotExist(err) {
		t.Fatalf("dangling symlink target was created or changed: %v", err)
	}
	ids, err := store.AccountInventoryIDs(t.Context())
	if err != nil || len(ids) != 0 {
		t.Fatalf("durable inventory after removal = %v, err = %v", ids, err)
	}
}

func TestManagedAccountRemovalDoesNotRemoveDirectory(t *testing.T) {
	store := Store{Path: filepath.Join(t.TempDir(), "unused-cli.json"), ManagedDir: t.TempDir()}
	filename, err := managedFilename("kimi-subscription:broken")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(store.ManagedDir, filename)
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if exists, err := store.ManagedAccountExists("broken"); err != nil || exists {
		t.Fatalf("directory reported as managed account: exists=%v err=%v", exists, err)
	}
	if removed, ok, err := store.RemoveManagedAccount("broken"); err != nil || ok {
		t.Fatalf("directory removal: removed=%+v ok=%v err=%v", removed, ok, err)
	}
	if info, err := os.Stat(path); err != nil || !info.IsDir() {
		t.Fatalf("managed-path directory did not survive: info=%v err=%v", info, err)
	}
}

func TestManagedAccountRemovalUnlinksSymlinkWithoutChangingTarget(t *testing.T) {
	store := Store{Path: filepath.Join(t.TempDir(), "unused-cli.json"), ManagedDir: t.TempDir()}
	filename, err := managedFilename("kimi-subscription:linked")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(store.ManagedDir, filename)
	target := filepath.Join(t.TempDir(), "external.json")
	targetBody := []byte("external credential must survive")
	if err := os.WriteFile(target, targetBody, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	removed, ok, err := store.RemoveManagedAccount("linked")
	if err != nil || !ok || removed.ID != "kimi-subscription:linked" {
		t.Fatalf("remove linked credential: removed=%+v ok=%v err=%v", removed, ok, err)
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("managed symlink remains after removal: %v", err)
	}
	after, err := os.ReadFile(target)
	if err != nil || string(after) != string(targetBody) {
		t.Fatalf("external target changed: body=%q err=%v", after, err)
	}
}
