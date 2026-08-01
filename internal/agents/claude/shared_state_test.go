package claude

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLocalProfilesShareClaudeHistoryWithoutLosingExistingFiles(t *testing.T) {
	root := t.TempDir()
	store := Store{
		Dir:            filepath.Join(root, "subrouter"),
		SharedStateDir: filepath.Join(root, ".claude"),
	}
	first, err := store.CreateProfile("work")
	if err != nil {
		t.Fatal(err)
	}
	projects := filepath.Join(first, "projects")
	info, err := os.Lstat(projects)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("%s is not a symlink", projects)
	}
	if err := os.WriteFile(filepath.Join(projects, "session.jsonl"), []byte("shared"), 0o600); err != nil {
		t.Fatal(err)
	}
	second, err := store.CreateProfile("personal")
	if err != nil {
		t.Fatal(err)
	}
	if body, err := os.ReadFile(filepath.Join(second, "projects", "session.jsonl")); err != nil || string(body) != "shared" {
		t.Fatalf("second profile did not share history: %q, %v", body, err)
	}
}

func TestExistingProfileHistoryMigratesAndPreservesConflicts(t *testing.T) {
	root := t.TempDir()
	instance := filepath.Join(root, "profile")
	shared := filepath.Join(root, ".claude")
	if err := os.MkdirAll(filepath.Join(instance, "projects"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(shared, "projects"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(instance, "projects", "same.jsonl"), []byte("profile"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(shared, "projects", "same.jsonl"), []byte("shared"), 0o600); err != nil {
		t.Fatal(err)
	}
	store := Store{Dir: root, SharedStateDir: shared}
	if err := store.prepareSharedState(instance); err != nil {
		t.Fatal(err)
	}
	if body, _ := os.ReadFile(filepath.Join(shared, "projects", "same.jsonl")); string(body) != "shared" {
		t.Fatalf("shared file overwritten: %q", body)
	}
	if body, _ := os.ReadFile(filepath.Join(shared, "projects", "same.jsonl.subrouter-legacy-1")); string(body) != "profile" {
		t.Fatalf("profile conflict lost: %q", body)
	}
	if err := store.prepareSharedState(instance); err != nil {
		t.Fatalf("second migration failed: %v", err)
	}
}
