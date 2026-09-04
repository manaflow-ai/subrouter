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
