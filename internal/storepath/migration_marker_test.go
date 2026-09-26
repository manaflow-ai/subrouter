package storepath

import (
	"os"
	"path/filepath"
	"testing"
)

func writeLegacyAccount(t *testing.T, legacy string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(legacy, "accounts"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacy, "accounts", "alice@example.com.json"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// removeAllAccounts simulates an operator deleting every account while
// leaving subrouter's own dotfile bookkeeping in place.
func removeAllAccounts(t *testing.T, target string) {
	t.Helper()
	if err := os.RemoveAll(filepath.Join(target, "accounts")); err != nil {
		t.Fatal(err)
	}
}

func assertNotResurrected(t *testing.T, target string) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(target, "accounts", "alice@example.com.json")); !os.IsNotExist(err) {
		t.Fatalf("deleted legacy account was re-imported, stat err = %v", err)
	}
}

func TestMigrateCodexDirDoesNotResurrectDeletedAccounts(t *testing.T) {
	root := t.TempDir()
	legacy := filepath.Join(root, "legacy")
	state := filepath.Join(root, "state")
	target := filepath.Join(state, "codex")
	writeLegacyAccount(t, legacy)

	if err := MigrateCodexDir(target, legacy); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(target, "accounts", "alice@example.com.json")); err != nil {
		t.Fatalf("first-run import missing: %v", err)
	}
	removeAllAccounts(t, target)

	if err := MigrateCodexDir(target, legacy); err != nil {
		t.Fatal(err)
	}
	assertNotResurrected(t, target)
}

func TestMigrateCodexDirIntoExistingEmptyTargetMarksCompletion(t *testing.T) {
	root := t.TempDir()
	legacy := filepath.Join(root, "legacy")
	target := filepath.Join(root, "codex")
	writeLegacyAccount(t, legacy)
	if err := os.MkdirAll(target, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := MigrateCodexDir(target, legacy); err != nil {
		t.Fatal(err)
	}
	removeAllAccounts(t, target)
	if err := MigrateCodexDir(target, legacy); err != nil {
		t.Fatal(err)
	}
	assertNotResurrected(t, target)
}

// Installs that migrated before the marker existed already have a populated
// target. The next start records completion so a later wipe stays empty.
func TestMigrateCodexDirMarksPreviouslyMigratedTarget(t *testing.T) {
	root := t.TempDir()
	legacy := filepath.Join(root, "legacy")
	target := filepath.Join(root, "codex")
	writeLegacyAccount(t, legacy)
	if err := os.MkdirAll(filepath.Join(target, "accounts"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "accounts", "bob@example.com.json"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := MigrateCodexDir(target, legacy); err != nil {
		t.Fatal(err)
	}
	removeAllAccounts(t, target)
	if err := MigrateCodexDir(target, legacy); err != nil {
		t.Fatal(err)
	}
	assertNotResurrected(t, target)
}

func TestReadOnlyInspectionHonorsMigrationMarker(t *testing.T) {
	root := t.TempDir()
	legacy := filepath.Join(root, "legacy")
	state := filepath.Join(root, "state")
	target := filepath.Join(state, "codex")
	writeLegacyAccount(t, legacy)
	if err := MigrateCodexDir(target, legacy); err != nil {
		t.Fatal(err)
	}
	removeAllAccounts(t, target)
	source, importsLegacy, err := codexMigrationSource(target, legacy)
	if err != nil {
		t.Fatal(err)
	}
	if importsLegacy || source != filepath.Clean(target) {
		t.Fatalf("migration source = %q (imports=%v), want target", source, importsLegacy)
	}
}
