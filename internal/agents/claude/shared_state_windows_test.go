//go:build windows

package claude

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestSharedStateMigrationRejectsJunctionOutsideSourceRoot(t *testing.T) {
	root := t.TempDir()
	instance := filepath.Join(root, "profile")
	shared := filepath.Join(root, "shared")
	external := filepath.Join(root, "external")
	if err := os.MkdirAll(filepath.Join(instance, "projects"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(shared, "projects"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(external, 0o700); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(external, "secret.jsonl")
	if err := os.WriteFile(secret, []byte("must stay outside migration root"), 0o600); err != nil {
		t.Fatal(err)
	}
	junction := filepath.Join(instance, "projects", "outside")
	output, err := exec.Command("cmd.exe", "/d", "/c", "mklink", "/J", junction, external).CombinedOutput()
	if err != nil {
		t.Fatalf("create test junction: %v: %s", err, output)
	}
	t.Cleanup(func() { _ = os.RemoveAll(junction) })

	store := Store{Dir: root, SharedStateDir: shared}
	err = store.prepareSharedState(instance)
	if err == nil {
		t.Fatal("migration accepted a junction that leaves the source root")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "outside") &&
		!strings.Contains(strings.ToLower(err.Error()), "root") {
		t.Fatalf("migration failed for an unrelated reason: %v", err)
	}
	if body, readErr := os.ReadFile(secret); readErr != nil || string(body) != "must stay outside migration root" {
		t.Fatalf("external file changed after rejected migration: %q, %v", body, readErr)
	}
	if _, statErr := os.Lstat(filepath.Join(instance, "projects")); statErr != nil {
		t.Fatalf("source state was removed after rejected migration: %v", statErr)
	}
}
