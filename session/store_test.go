package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func TestAllBoundsPersistedHistoryToMostRecentAssignments(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.json")
	base := time.Unix(1_700_000_000, 0).UTC()
	data := make(map[string]Assignment, MaxRetainedAssignments+100)
	for index := 0; index < MaxRetainedAssignments+100; index++ {
		assignment := Assignment{
			AgentType: "codex",
			SessionID: "session-" + strconv.Itoa(index),
			AccountID: "account",
			CreatedAt: base,
			UpdatedAt: base.Add(time.Duration(index) * time.Second),
		}
		data[ScopedSessionKey(assignment.AgentType, assignment.SessionID)] = assignment
	}
	body, err := json.Marshal(data)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}

	store, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	assignments := store.All()
	if len(assignments) != MaxRetainedAssignments {
		t.Fatalf("retained assignments = %d, want %d", len(assignments), MaxRetainedAssignments)
	}
	wantNewest := "session-" + strconv.Itoa(MaxRetainedAssignments+99)
	if assignments[0].SessionID != wantNewest {
		t.Fatalf("newest assignment = %q, want %q", assignments[0].SessionID, wantNewest)
	}
	body, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var persisted map[string]Assignment
	if err := json.Unmarshal(body, &persisted); err != nil {
		t.Fatal(err)
	}
	if len(persisted) != MaxRetainedAssignments {
		t.Fatalf("persisted assignments = %d, want %d", len(persisted), MaxRetainedAssignments)
	}
}

func TestCountByAccount(t *testing.T) {
	store, err := NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put("codex", "session-1", "account-a", "alice@example.com"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put("codex", "session-2", "account-a", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put("claude", "session-3", "account-b", ""); err != nil {
		t.Fatal(err)
	}

	counts := store.CountByAccount()
	if counts["account-a"] != 2 {
		t.Fatalf("account-a count = %d, want 2", counts["account-a"])
	}
	if counts["account-b"] != 1 {
		t.Fatalf("account-b count = %d, want 1", counts["account-b"])
	}
}

func TestPutPreservesExistingUserEmailWhenMissing(t *testing.T) {
	store, err := NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put("codex", "session-1", "account-a", "Alice@Example.COM"); err != nil {
		t.Fatal(err)
	}
	assignment, err := store.Put("codex", "session-1", "account-a", "")
	if err != nil {
		t.Fatal(err)
	}

	if assignment.UserEmail != "alice@example.com" {
		t.Fatalf("user email = %q, want alice@example.com", assignment.UserEmail)
	}
}

func TestNewStoreExpiresStaleUserEmailWithoutDroppingAssignment(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.json")
	stale := time.Now().UTC().Add(-UserEmailRetention - time.Hour)
	assignment := Assignment{
		AgentType: "codex", SessionID: "session-1", AccountID: "account-a",
		UserEmail: "alice@example.com", CreatedAt: stale, UpdatedAt: stale,
	}
	body, err := json.Marshal(map[string]Assignment{ScopedSessionKey("codex", "session-1"): assignment})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}

	store, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := store.Get("codex", "session-1")
	if !ok {
		t.Fatal("stale assignment was removed")
	}
	if got.UserEmail != "" {
		t.Fatalf("stale user email = %q, want empty", got.UserEmail)
	}
}

func TestTouchRefreshesActiveAssignmentRetentionTimestamp(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.json")
	stale := time.Now().UTC().Add(-2 * sessionActivityWriteInterval)
	assignment := Assignment{
		AgentType: "codex", SessionID: "session-1", AccountID: "account-a",
		UserEmail: "alice@example.com", CreatedAt: stale, UpdatedAt: stale,
	}
	body, err := json.Marshal(map[string]Assignment{ScopedSessionKey("codex", "session-1"): assignment})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}

	store, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	touched, ok, err := store.Touch("codex", "session-1")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("existing assignment was not touched")
	}
	if !touched.UpdatedAt.After(stale) {
		t.Fatalf("updated_at = %s, want after %s", touched.UpdatedAt, stale)
	}
	if touched.UserEmail != "alice@example.com" {
		t.Fatalf("touch changed user email to %q", touched.UserEmail)
	}

	reloaded, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	persisted, ok := reloaded.Get("codex", "session-1")
	if !ok || !persisted.UpdatedAt.Equal(touched.UpdatedAt) {
		t.Fatalf("persisted touch = %+v, ok=%t; want updated_at %s", persisted, ok, touched.UpdatedAt)
	}
}

func TestTouchMissingAssignmentIsANoop(t *testing.T) {
	store, err := NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := store.Touch("codex", "missing"); err != nil || ok {
		t.Fatalf("Touch missing assignment ok=%t err=%v, want false, nil", ok, err)
	}
}

func TestDeleteRemovesScopedAssignment(t *testing.T) {
	store, err := NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put("codex", "session-1", "account-a", "alice@example.com"); err != nil {
		t.Fatal(err)
	}
	deleted, err := store.Delete("codex", "session-1")
	if err != nil {
		t.Fatal(err)
	}
	if !deleted {
		t.Fatal("existing assignment was not deleted")
	}
	if _, ok := store.Get("codex", "session-1"); ok {
		t.Fatal("deleted assignment is still present")
	}
}

func TestCodexTurnScopedIDsUseOneStickyAssignment(t *testing.T) {
	store, err := NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put("codex", "codex-session:4", "account-a", ""); err != nil {
		t.Fatal(err)
	}

	assignment, ok := store.Get("codex", "codex-session:7")
	if !ok {
		t.Fatal("missing sticky assignment for later codex turn")
	}
	if assignment.SessionID != "codex-session" {
		t.Fatalf("SessionID = %q, want codex-session", assignment.SessionID)
	}
	if assignment.AccountID != "account-a" {
		t.Fatalf("AccountID = %q, want account-a", assignment.AccountID)
	}
	counts := store.CountByAccount()
	if counts["account-a"] != 1 {
		t.Fatalf("account-a count = %d, want 1", counts["account-a"])
	}
}

func TestNonCodexColonIDsStayDistinct(t *testing.T) {
	store, err := NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put("claude", "claude-session:4", "account-a", ""); err != nil {
		t.Fatal(err)
	}
	if _, ok := store.Get("claude", "claude-session:7"); ok {
		t.Fatal("claude colon-suffixed IDs should not be collapsed")
	}
}

func TestStoreScopesAssignmentsByAgentType(t *testing.T) {
	store, err := NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put("codex", "same-session", "codex-account", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put("claude", "same-session", "claude-account", ""); err != nil {
		t.Fatal(err)
	}

	codex, ok := store.Get("codex", "same-session")
	if !ok {
		t.Fatal("missing codex assignment")
	}
	claude, ok := store.Get("claude", "same-session")
	if !ok {
		t.Fatal("missing claude assignment")
	}
	if codex.AccountID != "codex-account" {
		t.Fatalf("codex AccountID = %q, want codex-account", codex.AccountID)
	}
	if claude.AccountID != "claude-account" {
		t.Fatalf("claude AccountID = %q, want claude-account", claude.AccountID)
	}
}

func TestStoreCompareAndPutPreservesNewerAssignment(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.json")
	store, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put("codex", "session-1", "original", "user@example.com"); err != nil {
		t.Fatal(err)
	}

	assignment, swapped, err := store.CompareAndPut("codex", "session-1", "original", "alternate", "")
	if err != nil {
		t.Fatal(err)
	}
	if !swapped || assignment.AccountID != "alternate" || assignment.UserEmail != "user@example.com" {
		t.Fatalf("matching compare-and-put = (%+v, %v), want alternate with retained identity", assignment, swapped)
	}

	if _, err := store.Put("codex", "session-1", "forced", ""); err != nil {
		t.Fatal(err)
	}
	assignment, swapped, err = store.CompareAndPut("codex", "session-1", "alternate", "stale", "")
	if err != nil {
		t.Fatal(err)
	}
	if swapped || assignment.AccountID != "forced" {
		t.Fatalf("stale compare-and-put = (%+v, %v), want forced unchanged", assignment, swapped)
	}

	reloaded, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	persisted, ok := reloaded.Get("codex", "session-1")
	if !ok || persisted.AccountID != "forced" {
		t.Fatalf("persisted assignment = (%+v, %v), want forced", persisted, ok)
	}
}

func TestStoresReloadAndMergeConcurrentGenerationWrites(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.json")
	oldGeneration, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	newGeneration, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := oldGeneration.Put("codex", "old-session", "account-a", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := newGeneration.Put("codex", "new-session", "account-b", ""); err != nil {
		t.Fatal(err)
	}
	if _, ok := oldGeneration.Get("codex", "new-session"); !ok {
		t.Fatal("old generation did not reload the new generation's assignment")
	}
	reloaded, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(reloaded.All()); got != 2 {
		t.Fatalf("stored assignments = %d, want 2", got)
	}
}

func TestStoreMigratesUnscopedAssignmentsToCodex(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.json")
	if err := os.WriteFile(path, []byte(`{
  "old-session": {
    "session_id": "old-session",
    "account_id": "codex-account",
    "created_at": "2026-04-28T00:00:00Z",
    "updated_at": "2026-04-28T00:00:00Z"
  }
}`), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}

	assignment, ok := store.Get("codex", "old-session")
	if !ok {
		t.Fatal("missing migrated assignment")
	}
	if assignment.AgentType != "codex" {
		t.Fatalf("AgentType = %q, want codex", assignment.AgentType)
	}
}

func TestStoreMigratesCodexTurnAssignmentsToLatestBaseSession(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.json")
	if err := os.WriteFile(path, []byte(`{
  "codex:codex-session:4": {
    "agent_type": "codex",
    "session_id": "codex-session:4",
    "account_id": "account-a",
    "created_at": "2026-04-28T00:00:00Z",
    "updated_at": "2026-04-28T00:01:00Z"
  },
  "codex:codex-session:7": {
    "agent_type": "codex",
    "session_id": "codex-session:7",
    "account_id": "account-b",
    "created_at": "2026-04-28T00:00:00Z",
    "updated_at": "2026-04-28T00:02:00Z"
  }
}`), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}

	assignment, ok := store.Get("codex", "codex-session:8")
	if !ok {
		t.Fatal("missing migrated base assignment")
	}
	if assignment.SessionID != "codex-session" {
		t.Fatalf("SessionID = %q, want codex-session", assignment.SessionID)
	}
	if assignment.AccountID != "account-b" {
		t.Fatalf("AccountID = %q, want latest account-b", assignment.AccountID)
	}
}
