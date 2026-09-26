package fsutil

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRenameNoReplacePreservesOccupiedEmptyDirectory(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source")
	destination := filepath.Join(root, "destination")
	if err := os.Mkdir(source, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "credential"), []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(destination, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := RenameNoReplace(source, destination); err == nil {
		t.Fatal("no-replace rename overwrote an occupied empty directory")
	}
	if _, err := os.Stat(filepath.Join(source, "credential")); err != nil {
		t.Fatalf("source was not preserved: %v", err)
	}
	entries, err := os.ReadDir(destination)
	if err != nil || len(entries) != 0 {
		t.Fatalf("destination changed: entries=%v err=%v", entries, err)
	}
}
