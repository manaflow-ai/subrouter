package tenant

import (
	"os"
	"path/filepath"
	"testing"
)

// A crash between writing a fixed-name temp file and renaming it can leave a
// stale sibling behind. The next save must not depend on that name.
func TestSaveIgnoresStaleFixedTempPath(t *testing.T) {
	root := t.TempDir()
	registry := NewRegistry(root)
	if err := os.MkdirAll(filepath.Join(root, "tenants.json.tmp"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, _, err := registry.Create("acme"); err != nil {
		t.Fatalf("create with stale temp path: %v", err)
	}
	tenants, err := NewRegistry(root).List()
	if err != nil || len(tenants) != 1 {
		t.Fatalf("reloaded tenants = %v, %v", tenants, err)
	}
}

func TestSaveSyncsStateDirectory(t *testing.T) {
	registry := NewRegistry(t.TempDir())
	syncCalls := 0
	registry.syncStateDir = func() error {
		syncCalls++
		return nil
	}
	if _, _, err := registry.Create("acme"); err != nil {
		t.Fatal(err)
	}
	if syncCalls == 0 {
		t.Fatal("tenants.json rename was not followed by a directory sync")
	}
	info, err := os.Stat(registry.Path())
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("tenants.json mode = %o, want 600", info.Mode().Perm())
	}
}
