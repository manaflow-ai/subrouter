//go:build linux || android

package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestRemovePrivateProxyHomeRepairsModeZeroDirectoriesWithoutFchmodat2(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses mode-000 directory access and cannot exercise the repair path")
	}
	root := t.TempDir()
	home := filepath.Join(root, "private-home")
	sealed := filepath.Join(home, "child", "sealed")
	if err := os.MkdirAll(sealed, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sealed, "credential"), []byte("ephemeral"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.Chmod(home, 0o700)
		_ = os.Chmod(filepath.Dir(sealed), 0o700)
		_ = os.Chmod(sealed, 0o700)
	})
	for _, path := range []string{sealed, filepath.Dir(sealed), home} {
		if err := os.Chmod(path, 0); err != nil {
			t.Fatal(err)
		}
	}
	if err := removePrivateProxyHome(home); err != nil {
		t.Fatalf("remove mode-000 private home: %v", err)
	}
	if _, err := os.Lstat(home); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("mode-000 private home survived cleanup: %v", err)
	}
}
