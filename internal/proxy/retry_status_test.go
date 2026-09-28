package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/session"
)

func TestRetryStatusRegistryMapsSessionAndRedactsHealth(t *testing.T) {
	registry := newRetryStatusRegistry()
	now := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	releaseOlder := registry.begin("codex", "thread-1:2", RetryStatus{
		Provider: accounts.ProviderCodex, Model: "gpt-6-astra", AccountID: "account-secret",
		Attempt: 2, Reason: "pool_status_503", NextRetryAt: now.Add(2 * time.Second),
	})
	releaseNewer := registry.begin("codex", "thread-1:3", RetryStatus{
		Provider: accounts.ProviderCodex, Model: "gpt-6-astra", AccountID: "account-secret",
		Attempt: 3, Reason: "pool_capacity_body", NextRetryAt: now.Add(8 * time.Second),
	})

	status := registry.forSession("codex", "thread-1")
	if status == nil || status.Attempt != 3 || status.AccountID != "account-secret" {
		t.Fatalf("session retry = %+v, want the latest matching Codex window", status)
	}
	healthBody, err := json.Marshal(registry.healthSnapshot())
	if err != nil {
		t.Fatal(err)
	}
	if string(healthBody) == "[]" {
		t.Fatal("health snapshot omitted active retries")
	}
	for _, forbidden := range []string{"thread-1", "account-secret", "gpt-6-astra", "session_id", "account_id", "model"} {
		if strings.Contains(string(healthBody), forbidden) {
			t.Fatalf("health snapshot leaked %q: %s", forbidden, healthBody)
		}
	}

	releaseNewer()
	if status := registry.forSession("codex", "thread-1"); status == nil || status.Attempt != 2 {
		t.Fatalf("session retry after release = %+v, want older wait", status)
	}
	releaseNewer() // release is idempotent
	releaseOlder()
	if status := registry.forSession("codex", "thread-1"); status != nil {
		t.Fatalf("released retry remains: %+v", status)
	}
}

func TestRetryStatusAppearsOnSessionAndRedactedHealthSurfaces(t *testing.T) {
	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put("claude", "session-1", "account-1", ""); err != nil {
		t.Fatal(err)
	}
	registry := newRetryStatusRegistry()
	release := registry.begin("claude", "session-1", RetryStatus{
		Provider: accounts.ProviderClaude, Model: "claude-opus", AccountID: "account-1",
		Attempt: 4, Reason: "http_529", NextRetryAt: time.Date(2026, 9, 28, 12, 0, 15, 0, time.UTC),
	})
	defer release()
	server := Server{
		Sessions:       store,
		ActiveSessions: NewActiveSessions(),
		retryStatuses:  registry,
		overloadHeld:   newOverloadHeldGauge(),
	}

	sessions := httptest.NewRecorder()
	server.handleSessions(sessions, httptest.NewRequest(http.MethodGet, "/_subrouter/sessions?agent_type=claude&session_id=session-1", nil))
	var rows []sessionAdminView
	if err := json.NewDecoder(sessions.Body).Decode(&rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Retry == nil || rows[0].Retry.Attempt != 4 || rows[0].Retry.AccountID != "account-1" || rows[0].Retry.Model != "claude-opus" {
		t.Fatalf("session rows = %+v", rows)
	}

	health := httptest.NewRecorder()
	server.handleHealth(health, httptest.NewRequest(http.MethodGet, "/_subrouter/health", nil))
	var payload map[string]json.RawMessage
	if err := json.NewDecoder(health.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	retries := string(payload["active_retries"])
	if retries == "" || retries == "null" || strings.Contains(retries, "session-1") || strings.Contains(retries, "account-1") || strings.Contains(retries, "claude-opus") || strings.Contains(retries, `"model"`) {
		t.Fatalf("health active_retries = %s", retries)
	}
}
