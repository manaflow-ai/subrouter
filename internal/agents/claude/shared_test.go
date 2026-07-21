package claude

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func sharedTestStore(t *testing.T) Store {
	t.Helper()
	dir := t.TempDir()
	store := Store{Dir: dir}
	profiles := profilesFile{
		Active: "b@example.com",
		Profiles: map[string]Profile{
			"a@example.com": {Name: "a@example.com", Dir: "_pa"},
			"b@example.com": {Name: "b@example.com", Dir: "_pb"},
		},
	}
	body, err := json.Marshal(profiles)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "claude.json"), body, 0o600); err != nil {
		t.Fatal(err)
	}
	return store
}

func writeFileAt(t *testing.T, path, content string, mtime time.Time) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if !mtime.IsZero() {
		if err := os.Chtimes(path, mtime, mtime); err != nil {
			t.Fatal(err)
		}
	}
}

func assertSymlinkTo(t *testing.T, link, target string) {
	t.Helper()
	info, err := os.Lstat(link)
	if err != nil {
		t.Fatalf("lstat %s: %v", link, err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("%s is not a symlink", link)
	}
	got, err := os.Readlink(link)
	if err != nil {
		t.Fatal(err)
	}
	if got != target {
		t.Fatalf("%s -> %s, want %s", link, got, target)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(body)
}

func TestConsolidateSharedStateMergesAndSymlinks(t *testing.T) {
	store := sharedTestStore(t)
	dirA := store.ClaudeConfigDir("a@example.com")
	dirB := store.ClaudeConfigDir("b@example.com")
	project := "-Users-someone-fun-repo"
	idA := "11111111-1111-1111-1111-111111111111"
	idB := "22222222-2222-2222-2222-222222222222"

	old := time.Now().Add(-time.Hour)
	writeFileAt(t, filepath.Join(dirA, "projects", project, idA+".jsonl"), "a-transcript", time.Time{})
	writeFileAt(t, filepath.Join(dirA, "session-env", idA, "env.json"), "a-env", time.Time{})
	writeFileAt(t, filepath.Join(dirA, "file-history", idA, "f0@v2"), "a-hist", time.Time{})
	writeFileAt(t, filepath.Join(dirB, "projects", project, idB+".jsonl"), "b-transcript", time.Time{})
	// Same session in both profiles (e.g. from an old copy-based migration):
	// the newer copy must win and the older one must be quarantined.
	writeFileAt(t, filepath.Join(dirA, "projects", project, idB+".jsonl"), "stale-copy", old)

	report, err := store.ConsolidateSharedState()
	if err != nil {
		t.Fatal(err)
	}
	if report.Profiles != 2 {
		t.Fatalf("profiles = %d, want 2", report.Profiles)
	}
	if len(report.Conflicts) != 1 {
		t.Fatalf("conflicts = %v, want exactly 1", report.Conflicts)
	}

	shared := store.SharedDir()
	for _, name := range sharedStateDirNames {
		assertSymlinkTo(t, filepath.Join(dirA, name), filepath.Join(shared, name))
		assertSymlinkTo(t, filepath.Join(dirB, name), filepath.Join(shared, name))
	}
	if got := readFile(t, filepath.Join(shared, "projects", project, idA+".jsonl")); got != "a-transcript" {
		t.Fatalf("shared transcript A = %q", got)
	}
	if got := readFile(t, filepath.Join(shared, "projects", project, idB+".jsonl")); got != "b-transcript" {
		t.Fatalf("shared transcript B = %q, want winner b-transcript", got)
	}
	if got := readFile(t, report.Conflicts[0]); got != "stale-copy" {
		t.Fatalf("quarantined conflict = %q, want stale-copy", got)
	}
	// Both profiles now see both sessions through the symlinks.
	for _, dir := range []string{dirA, dirB} {
		for _, id := range []string{idA, idB} {
			if _, err := os.Stat(filepath.Join(dir, "projects", project, id+".jsonl")); err != nil {
				t.Fatalf("session %s not visible from %s: %v", id, dir, err)
			}
		}
	}

	// Idempotent re-run: nothing moves, no conflicts.
	report, err = store.ConsolidateSharedState()
	if err != nil {
		t.Fatal(err)
	}
	if report.Moved != 0 || len(report.Conflicts) != 0 {
		t.Fatalf("re-run moved=%d conflicts=%v, want 0 and none", report.Moved, report.Conflicts)
	}
}

func TestEnsureSharedLayoutRepairsRecreatedRealDir(t *testing.T) {
	store := sharedTestStore(t)
	if _, err := store.ConsolidateSharedState(); err != nil {
		t.Fatal(err)
	}
	dirB := store.ClaudeConfigDir("b@example.com")
	project := "-Users-someone-fun-repo"
	id := "33333333-3333-3333-3333-333333333333"

	// Simulate Claude recreating a real projects dir after the symlink was
	// deleted, then writing a new session into it.
	if err := os.Remove(filepath.Join(dirB, "projects")); err != nil {
		t.Fatal(err)
	}
	writeFileAt(t, filepath.Join(dirB, "projects", project, id+".jsonl"), "fresh", time.Time{})

	if _, err := store.EnsureSharedLayout(dirB); err != nil {
		t.Fatal(err)
	}
	assertSymlinkTo(t, filepath.Join(dirB, "projects"), filepath.Join(store.SharedDir(), "projects"))
	if got := readFile(t, filepath.Join(store.SharedDir(), "projects", project, id+".jsonl")); got != "fresh" {
		t.Fatalf("merged transcript = %q", got)
	}
}

func TestCreateProfileAfterConsolidationGetsSymlinks(t *testing.T) {
	store := sharedTestStore(t)
	if _, err := store.ConsolidateSharedState(); err != nil {
		t.Fatal(err)
	}
	instancePath, err := store.CreateProfile("fresh-profile")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range sharedStateDirNames {
		assertSymlinkTo(t, filepath.Join(instancePath, name), filepath.Join(store.SharedDir(), name))
	}
}

func TestCreateProfileWithoutSharedStoreKeepsRealDirs(t *testing.T) {
	store := sharedTestStore(t)
	instancePath, err := store.CreateProfile("isolated-profile")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"session-env", "todos", "file-history"} {
		info, err := os.Lstat(filepath.Join(instancePath, name))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			t.Fatalf("%s should be a real dir before consolidation", name)
		}
	}
	if store.SharedStoreEnabled() {
		t.Fatal("shared store should not exist before consolidation")
	}
}

func TestMigrateSessionNoopWhenShared(t *testing.T) {
	store := sharedTestStore(t)
	dirA := store.ClaudeConfigDir("a@example.com")
	id := "44444444-4444-4444-4444-444444444444"
	writeFileAt(t, filepath.Join(dirA, "projects", "-proj", id+".jsonl"), "x", time.Time{})
	if _, err := store.ConsolidateSharedState(); err != nil {
		t.Fatal(err)
	}
	from, err := store.MigrateSession("b@example.com", id)
	if err != nil {
		t.Fatal(err)
	}
	if from != "" {
		t.Fatalf("MigrateSession copied from %q, want shared-store no-op", from)
	}
}

func TestEnsureSharedLayoutRefusesSharedDir(t *testing.T) {
	store := sharedTestStore(t)
	if _, err := store.ConsolidateSharedState(); err != nil {
		t.Fatal(err)
	}
	if _, err := store.EnsureSharedLayout(store.SharedDir()); err == nil {
		t.Fatal("expected error converging the shared store onto itself")
	}
}

func TestConsolidateSweepsUnregisteredInstanceDirs(t *testing.T) {
	store := sharedTestStore(t)
	id := "55555555-5555-5555-5555-555555555555"
	tempInstance := filepath.Join(store.InstancesDir(), "_p1234567890")
	writeFileAt(t, filepath.Join(tempInstance, "projects", "-proj", id+".jsonl"), "temp-session", time.Time{})

	if _, err := store.ConsolidateSharedState(); err != nil {
		t.Fatal(err)
	}
	assertSymlinkTo(t, filepath.Join(tempInstance, "projects"), filepath.Join(store.SharedDir(), "projects"))
	if got := readFile(t, filepath.Join(store.SharedDir(), "projects", "-proj", id+".jsonl")); got != "temp-session" {
		t.Fatalf("temp instance session = %q", got)
	}
}
