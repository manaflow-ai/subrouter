package claude

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
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

func TestLocalProfilesShareClaudeSessionsOnFreshSetup(t *testing.T) {
	root := t.TempDir()
	instance := filepath.Join(root, "profile")
	shared := filepath.Join(root, ".claude")
	if err := os.MkdirAll(instance, 0o700); err != nil {
		t.Fatal(err)
	}

	store := Store{Dir: root, SharedStateDir: shared}
	if err := store.prepareSharedState(instance); err != nil {
		t.Fatal(err)
	}

	info, err := os.Lstat(filepath.Join(instance, "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("sessions mode = %v, want symlink", info.Mode())
	}
	if got, err := os.Readlink(filepath.Join(instance, "sessions")); err != nil || got != filepath.Join(shared, "sessions") {
		t.Fatalf("sessions link = %q, %v", got, err)
	}
	sharedInfo, err := os.Stat(filepath.Join(shared, "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	if got := sharedInfo.Mode().Perm(); got != 0o700 {
		t.Fatalf("shared sessions mode = %o, want 700", got)
	}
}

func TestExistingProfileSessionsMigrateWithoutOverwritingAndStayIdempotent(t *testing.T) {
	root := t.TempDir()
	instance := filepath.Join(root, "profile")
	shared := filepath.Join(root, ".claude")
	sessions := filepath.Join(instance, "sessions")
	sharedSessions := filepath.Join(shared, "sessions")
	if err := os.MkdirAll(sessions, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(sharedSessions, 0o700); err != nil {
		t.Fatal(err)
	}
	keyName := "12345.abc.key"
	if err := os.WriteFile(filepath.Join(sessions, keyName), []byte("profile-key"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sessions, "12345.json"), []byte("profile-session"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sharedSessions, keyName), []byte("shared-key"), 0o600); err != nil {
		t.Fatal(err)
	}

	store := Store{Dir: root, SharedStateDir: shared}
	if err := store.prepareSharedState(instance); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(sessions)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("sessions mode = %v, want symlink", info.Mode())
	}
	if body, err := os.ReadFile(filepath.Join(sharedSessions, "12345.json")); err != nil || string(body) != "profile-session" {
		t.Fatalf("migrated session = %q, %v", body, err)
	}
	if body, err := os.ReadFile(filepath.Join(sharedSessions, keyName)); err != nil || string(body) != "shared-key" {
		t.Fatalf("shared key was overwritten: %q, %v", body, err)
	}
	legacyKey, err := os.ReadFile(filepath.Join(sharedSessions, keyName+".subrouter-legacy-1"))
	if err != nil || string(legacyKey) != "profile-key" {
		t.Fatalf("profile key collision was not preserved: %q, %v", legacyKey, err)
	}
	keyInfo, err := os.Stat(filepath.Join(sharedSessions, keyName+".subrouter-legacy-1"))
	if err != nil {
		t.Fatal(err)
	}
	if got := keyInfo.Mode().Perm(); got != 0o600 {
		t.Fatalf("migrated key mode = %o, want 600", got)
	}

	entriesBefore, err := os.ReadDir(sharedSessions)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.prepareSharedState(instance); err != nil {
		t.Fatalf("second migration failed: %v", err)
	}
	entriesAfter, err := os.ReadDir(sharedSessions)
	if err != nil {
		t.Fatal(err)
	}
	if len(entriesAfter) != len(entriesBefore) {
		t.Fatalf("second migration changed shared sessions: %d entries, want %d", len(entriesAfter), len(entriesBefore))
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

func TestExistingProfileHistoryMigratesDirectoryConflicts(t *testing.T) {
	root := t.TempDir()
	instance := filepath.Join(root, "profile")
	shared := filepath.Join(root, ".claude")
	if err := os.MkdirAll(filepath.Join(instance, "projects", "conflict"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(shared, "projects"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(instance, "projects", "conflict", "session.jsonl"), []byte("profile"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(shared, "projects", "conflict"), []byte("shared"), 0o600); err != nil {
		t.Fatal(err)
	}

	store := Store{Dir: root, SharedStateDir: shared}
	if err := store.prepareSharedState(instance); err != nil {
		t.Fatal(err)
	}
	sharedBody, err := os.ReadFile(filepath.Join(shared, "projects", "conflict"))
	if err != nil || string(sharedBody) != "shared" {
		t.Fatalf("shared conflict was changed: %q, %v", sharedBody, err)
	}
	body, err := os.ReadFile(filepath.Join(shared, "projects", "conflict.subrouter-legacy-1", "session.jsonl"))
	if err != nil || string(body) != "profile" {
		t.Fatalf("directory conflict was not preserved: %q, %v", body, err)
	}
}

func TestConcurrentSharedStatePreparationIsIdempotent(t *testing.T) {
	root := t.TempDir()
	instance := filepath.Join(root, "profile")
	shared := filepath.Join(root, ".claude")
	projects := filepath.Join(instance, "projects")
	if err := os.MkdirAll(projects, 0o700); err != nil {
		t.Fatal(err)
	}
	for index := 0; index < 64; index++ {
		name := filepath.Join(projects, fmt.Sprintf("session-%d.jsonl", index))
		if err := os.WriteFile(name, []byte("history"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	store := Store{Dir: root, SharedStateDir: shared}
	const workers = 32
	start := make(chan struct{})
	errorsSeen := make(chan error, workers)
	var ready sync.WaitGroup
	ready.Add(workers)
	for range workers {
		go func() {
			ready.Done()
			<-start
			errorsSeen <- store.PrepareSharedStateDir(instance)
		}()
	}
	ready.Wait()
	close(start)
	for range workers {
		if err := <-errorsSeen; err != nil {
			t.Fatalf("concurrent shared-state preparation failed: %v", err)
		}
	}

	target, err := os.Readlink(filepath.Join(instance, "projects"))
	if err != nil {
		t.Fatal(err)
	}
	if target != filepath.Join(shared, "projects") {
		t.Fatalf("projects link = %q, want %q", target, filepath.Join(shared, "projects"))
	}
	for index := 0; index < 64; index++ {
		name := filepath.Join(shared, "projects", fmt.Sprintf("session-%d.jsonl", index))
		if body, err := os.ReadFile(name); err != nil || string(body) != "history" {
			t.Fatalf("shared history %d = %q, %v", index, body, err)
		}
	}
}

func TestConcurrentProfilesPreserveConflictingSharedHistory(t *testing.T) {
	root := t.TempDir()
	shared := filepath.Join(root, ".claude")
	store := Store{Dir: root, SharedStateDir: shared}
	const profiles = 16
	instances := make([]string, profiles)
	for index := range profiles {
		instance := filepath.Join(root, fmt.Sprintf("profile-%d", index))
		projects := filepath.Join(instance, "projects")
		if err := os.MkdirAll(projects, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(
			filepath.Join(projects, "session.jsonl"), []byte(fmt.Sprintf("profile-%d", index)), 0o600,
		); err != nil {
			t.Fatal(err)
		}
		instances[index] = instance
	}

	start := make(chan struct{})
	errorsSeen := make(chan error, profiles)
	var ready sync.WaitGroup
	ready.Add(profiles)
	for _, instance := range instances {
		go func() {
			ready.Done()
			<-start
			errorsSeen <- store.PrepareSharedStateDir(instance)
		}()
	}
	ready.Wait()
	close(start)
	for range profiles {
		if err := <-errorsSeen; err != nil {
			t.Fatalf("concurrent shared-state preparation failed: %v", err)
		}
	}

	bodies := make(map[string]bool, profiles)
	entries, err := os.ReadDir(filepath.Join(shared, "projects"))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), "session.jsonl") {
			continue
		}
		body, readErr := os.ReadFile(filepath.Join(shared, "projects", entry.Name()))
		if readErr != nil {
			t.Fatal(readErr)
		}
		bodies[string(body)] = true
	}
	for index := range profiles {
		want := fmt.Sprintf("profile-%d", index)
		if !bodies[want] {
			t.Fatalf("shared history lost %q; retained %d of %d bodies", want, len(bodies), profiles)
		}
	}
}

func TestSharedHistorySupportsExistingExternalDirectorySymlink(t *testing.T) {
	root := t.TempDir()
	instance := filepath.Join(root, "profile")
	shared := filepath.Join(root, ".claude")
	legacy := filepath.Join(root, "legacy-projects")
	for _, path := range []string{filepath.Join(instance, "projects"), shared, legacy} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(legacy, filepath.Join(shared, "projects")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacy, "old.jsonl"), []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(instance, "projects", "new.jsonl"), []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	store := Store{Dir: root, SharedStateDir: shared}
	for i := 0; i < 2; i++ {
		if err := store.PrepareSharedStateDir(instance); err != nil {
			t.Fatalf("launch %d: %v", i, err)
		}
	}
	for name, want := range map[string]string{"old.jsonl": "old", "new.jsonl": "new"} {
		body, err := os.ReadFile(filepath.Join(instance, "projects", name))
		if err != nil || string(body) != want {
			t.Fatalf("history %s = %q, %v", name, body, err)
		}
	}
	if got, err := os.Readlink(filepath.Join(shared, "projects")); err != nil || got != legacy {
		t.Fatalf("shared link changed: %q, %v", got, err)
	}
}
