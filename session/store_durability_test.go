package session

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNewStoreQuarantinesCorruptFileAndKeepsWorking(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sessions.json")
	if err := os.WriteFile(path, []byte("{\"codex:abc\": {\"agent_ty"), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := NewStore(path)
	if err != nil {
		t.Fatalf("NewStore on corrupt file: %v", err)
	}
	if _, err := store.Put("codex", "s1", "acct-1", ""); err != nil {
		t.Fatalf("Put after corrupt file: %v", err)
	}
	reopened, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := reopened.Get("codex", "s1"); !ok || got.AccountID != "acct-1" {
		t.Fatalf("reloaded assignment = %+v, %v", got, ok)
	}
	matches, err := filepath.Glob(filepath.Join(dir, "sessions.json.corrupt-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 1 {
		t.Fatalf("corrupt copies = %v, want exactly one", matches)
	}
	body, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "agent_ty") {
		t.Fatalf("quarantined file lost original bytes: %q", body)
	}
}

func TestMutationsRecoverWhenFileIsCorruptedAfterStart(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sessions.json")
	store, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put("codex", "s1", "acct-1", ""); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put("codex", "s2", "acct-2", ""); err != nil {
		t.Fatalf("Put after truncation: %v", err)
	}
	if _, swapped, err := store.CompareAndPut("codex", "s2", "acct-2", "acct-3", ""); err != nil || !swapped {
		t.Fatalf("CompareAndPut after truncation = %v, %v", swapped, err)
	}
}

func TestSaveIgnoresStaleFixedTempPath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sessions.json")
	if err := os.MkdirAll(path+".tmp", 0o700); err != nil {
		t.Fatal(err)
	}
	store, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put("codex", "s1", "acct-1", ""); err != nil {
		t.Fatalf("Put with stale temp path: %v", err)
	}
}
