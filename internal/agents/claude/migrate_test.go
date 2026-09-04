package claude

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestResumeSessionID(t *testing.T) {
	id := "6452edd8-ba4e-4ba7-b585-4f10d03b1bb4"
	cases := []struct {
		args []string
		want string
	}{
		{[]string{"--resume", id}, id},
		{[]string{"--dangerously-skip-permissions", "--chrome", "--resume", id}, id},
		{[]string{"-r", id}, id},
		{[]string{"--resume=" + id}, id},
		{[]string{"--resume"}, ""},
		{[]string{"--resume", "not-a-uuid"}, ""},
		{[]string{"-p", "hello"}, ""},
	}
	for _, tc := range cases {
		if got := ResumeSessionID(tc.args); got != tc.want {
			t.Fatalf("ResumeSessionID(%v) = %q, want %q", tc.args, got, tc.want)
		}
	}
}

func migrateTestStore(t *testing.T) (Store, string, string) {
	t.Helper()
	dir := t.TempDir()
	store := Store{Dir: dir}
	profiles := profilesFile{
		Active: "target@example.com",
		Profiles: map[string]Profile{
			"source@example.com": {Name: "source@example.com", Dir: "_psource"},
			"target@example.com": {Name: "target@example.com", Dir: "_ptarget"},
		},
	}
	body, err := json.Marshal(profiles)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "claude.json"), body, 0o600); err != nil {
		t.Fatal(err)
	}
	sourceDir := store.ClaudeConfigDir("source@example.com")
	targetDir := store.ClaudeConfigDir("target@example.com")
	return store, sourceDir, targetDir
}

func TestMigrateSessionCopiesConversation(t *testing.T) {
	store, sourceDir, targetDir := migrateTestStore(t)
	id := "6452edd8-ba4e-4ba7-b585-4f10d03b1bb4"
	project := "-Users-someone-fun-repo"
	for _, path := range []string{
		filepath.Join(sourceDir, "projects", project, id+".jsonl"),
		filepath.Join(sourceDir, "projects", project, id, "topic.json"),
		filepath.Join(sourceDir, "file-history", id, "f0.txt"),
		filepath.Join(sourceDir, "session-env", id, "env.json"),
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("data"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	from, err := store.MigrateSession("target@example.com", id)
	if err != nil {
		t.Fatal(err)
	}
	if from != "source@example.com" {
		t.Fatalf("from = %q, want source@example.com", from)
	}
	for _, path := range []string{
		filepath.Join(targetDir, "projects", project, id+".jsonl"),
		filepath.Join(targetDir, "projects", project, id, "topic.json"),
		filepath.Join(targetDir, "file-history", id, "f0.txt"),
		filepath.Join(targetDir, "session-env", id, "env.json"),
	} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("expected migrated file %s: %v", path, err)
		}
	}

	// Second call is a no-op: the target already has the session.
	from, err = store.MigrateSession("target@example.com", id)
	if err != nil {
		t.Fatal(err)
	}
	if from != "" {
		t.Fatalf("second migrate from = %q, want no-op", from)
	}
}

func TestMigrateSessionMissingEverywhere(t *testing.T) {
	store, _, _ := migrateTestStore(t)
	from, err := store.MigrateSession("target@example.com", "11111111-2222-3333-4444-555555555555")
	if err != nil {
		t.Fatal(err)
	}
	if from != "" {
		t.Fatalf("from = %q, want empty when no profile has the session", from)
	}
}

func TestMigrateSharedStatePreservesOpenFileWrites(t *testing.T) {
	_, sourceDir, targetDir := migrateTestStore(t)
	sourcePath := filepath.Join(sourceDir, "projects", "history.jsonl")
	if err := os.MkdirAll(filepath.Dir(sourcePath), 0o700); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(sourcePath, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString("before\n"); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if err := file.Sync(); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if err := migrateDirectoryToShared(filepath.Join(sourceDir, "projects"), filepath.Join(targetDir, "projects")); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if _, err := file.WriteString("after\n"); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(filepath.Join(targetDir, "projects", "history.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "before\nafter\n" {
		t.Fatalf("migrated file = %q, want open-file writes preserved", body)
	}
}
