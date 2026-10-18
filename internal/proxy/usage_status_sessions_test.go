package proxy

import (
	"path/filepath"
	"testing"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/session"
)

func TestUsageStatusAddsAssignedSessionCounts(t *testing.T) {
	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put("qwen-token", "first", "qwen-token:large-plan", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put("qwen-token", "second", "qwen-token:large-plan", ""); err != nil {
		t.Fatal(err)
	}
	server := Server{Sessions: store}
	statuses := server.withSessionCounts([]AccountUsageStatus{{AccountStatus: AccountStatus{
		ID:       "qwen-token:large-plan",
		Provider: accounts.ProviderQwenToken,
	}}})
	if len(statuses) != 1 || !statuses[0].SessionsKnown || statuses[0].AssignedSessions != 2 {
		t.Fatalf("session counts = %+v", statuses)
	}
}

func TestUsageStatusAddsSessionActivityMetadata(t *testing.T) {
	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put("claude", "active", "claude:work", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put("claude", "idle", "claude:work", ""); err != nil {
		t.Fatal(err)
	}
	active := NewActiveSessions()
	end := active.Begin("claude", "active")
	defer end()

	server := Server{Sessions: store, ActiveSessions: active}
	statuses := server.withSessionActivity(server.withSessionCounts([]AccountUsageStatus{{AccountStatus: AccountStatus{
		ID: "claude:work", Provider: accounts.ProviderClaude,
	}}}))
	status := statuses[0]
	if status.ActiveSessions != 1 || status.RecentSessions != 2 {
		t.Fatalf("session activity = %+v, want one active and two recent", status)
	}
	if status.FirstSessionAt == nil || status.LastSessionAt == nil {
		t.Fatalf("session timestamps = %+v, want both timestamps", status)
	}
}

func TestKimiUsageStatusIsActiveOnlyWithAssignedSession(t *testing.T) {
	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	server := Server{Sessions: store}
	input := []AccountUsageStatus{{AccountStatus: AccountStatus{
		ID: "kimi:work", Provider: accounts.ProviderKimi,
	}, Active: true}}
	statuses := server.withSessionCounts(input)
	if statuses[0].Active {
		t.Fatal("idle Kimi account remained active")
	}
	if _, err := store.Put("kimi", "session", "kimi:work", ""); err != nil {
		t.Fatal(err)
	}
	statuses = server.withSessionCounts(input)
	if !statuses[0].Active || statuses[0].AssignedSessions != 1 {
		t.Fatalf("assigned Kimi status = %+v", statuses[0])
	}
}

func TestGrokOAuthUsageStatusIsActiveOnlyWithAssignedSession(t *testing.T) {
	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	server := Server{Sessions: store}
	input := []AccountUsageStatus{{AccountStatus: AccountStatus{
		ID: "grok-subscription", Provider: accounts.ProviderGrok, AuthMode: accounts.AuthModeOAuth,
	}, Active: true}}
	statuses := server.withSessionCounts(input)
	if statuses[0].Active {
		t.Fatal("idle Grok subscription account remained active")
	}
	if _, err := store.Put("grok", "session", "grok-subscription", ""); err != nil {
		t.Fatal(err)
	}
	statuses = server.withSessionCounts(input)
	if !statuses[0].Active || statuses[0].AssignedSessions != 1 {
		t.Fatalf("assigned Grok status = %+v", statuses[0])
	}
}

func TestAntigravityUsageStatusIsActiveOnlyWithAssignedSession(t *testing.T) {
	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	server := Server{Sessions: store}
	input := []AccountUsageStatus{{AccountStatus: AccountStatus{
		ID: "antigravity", Provider: accounts.ProviderAntigravity, AuthMode: accounts.AuthModeOAuth,
	}, Active: true}}
	statuses := server.withSessionCounts(input)
	if statuses[0].Active {
		t.Fatal("idle Antigravity account remained active")
	}
	if _, err := store.Put("antigravity", "session", "antigravity", ""); err != nil {
		t.Fatal(err)
	}
	statuses = server.withSessionCounts(input)
	if !statuses[0].Active || statuses[0].AssignedSessions != 1 {
		t.Fatalf("assigned Antigravity status = %+v", statuses[0])
	}
}
