package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
)

func TestWithClaudeUserSettingsCarriesHooksAndKeepsRoutingAuthoritative(t *testing.T) {
	userSettings := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(userSettings, []byte(`{
		"hooks": {"PreToolUse": [{"matcher": "Bash", "hooks": [{"type": "command", "command": "guard.sh"}]}]},
		"permissions": {"allow": ["Bash(ls:*)"]},
		"theme": "dark",
		"apiKeyHelper": "print-other-key",
		"statusLine": {"type": "command", "command": "user-status"},
		"env": {"USER_ONLY": "kept", "ANTHROPIC_BASE_URL": "https://elsewhere.example"}
	}`), 0o600); err != nil {
		t.Fatal(err)
	}
	launch, err := proxyClaudeLaunchSettings("http://127.0.0.1:31415", "token", "/tmp/proxy-config")
	if err != nil {
		t.Fatal(err)
	}
	body, err := withClaudeUserSettings(launch, userSettings)
	if err != nil {
		t.Fatal(err)
	}
	body, err = withClaudeSessionStatusLine(body, "", "/tmp/store")
	if err != nil {
		t.Fatal(err)
	}
	var merged map[string]any
	if err := json.Unmarshal(body, &merged); err != nil {
		t.Fatal(err)
	}
	hooks, _ := merged["hooks"].(map[string]any)
	if _, ok := hooks["PreToolUse"]; !ok {
		t.Fatalf("user hooks missing from launch settings: %s", body)
	}
	if merged["theme"] != "dark" || merged["permissions"] == nil {
		t.Fatalf("user settings missing: %s", body)
	}
	if _, ok := merged["apiKeyHelper"]; ok {
		t.Fatalf("credential helper leaked into proxy launch: %s", body)
	}
	env, _ := merged["env"].(map[string]any)
	if env["USER_ONLY"] != "kept" {
		t.Fatalf("user env dropped: %s", body)
	}
	if env["ANTHROPIC_BASE_URL"] != "http://127.0.0.1:31415" || env["ANTHROPIC_AUTH_TOKEN"] != "token" || env["CLAUDE_CONFIG_DIR"] != "/tmp/proxy-config" {
		t.Fatalf("routing env not authoritative: %s", body)
	}
}

func TestWithClaudeUserSettingsLetsSRStatusLineWin(t *testing.T) {
	userSettings := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(userSettings, []byte(`{"statusLine": {"type": "command", "command": "user-status"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	launch, err := json.Marshal(map[string]any{"env": map[string]string{}, "statusLine": map[string]any{"type": "command", "command": "sr-status"}})
	if err != nil {
		t.Fatal(err)
	}
	body, err := withClaudeUserSettings(launch, userSettings)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "sr-status") || strings.Contains(string(body), "user-status") {
		t.Fatalf("status line = %s, want sr's", body)
	}
}

func TestWithClaudeUserSettingsIgnoresMissingOrInvalidFile(t *testing.T) {
	launch := []byte(`{"env":{"ANTHROPIC_BASE_URL":"http://127.0.0.1:1"}}`)
	dir := t.TempDir()
	invalid := filepath.Join(dir, "invalid.json")
	if err := os.WriteFile(invalid, []byte(`[1,2]`), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"", filepath.Join(dir, "missing.json"), invalid} {
		body, err := withClaudeUserSettings(launch, path)
		if err != nil {
			t.Fatal(err)
		}
		if string(body) != string(launch) {
			t.Fatalf("%q changed launch settings: %s", path, body)
		}
	}
}

func TestClaudeProxyUserSettingsPathIgnoresHermeticStores(t *testing.T) {
	if got := claudeProxyUserSettingsPath(t.TempDir()); got != "" {
		t.Fatalf("hermetic store read user settings at %q", got)
	}
}

func TestAccountPickerUsageDoesNotWaitOnSlowServer(t *testing.T) {
	previousWait := accountPickerUsageWait
	accountPickerUsageWait = 300 * time.Millisecond
	t.Cleanup(func() { accountPickerUsageWait = previousWait })
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		select {
		case <-release:
		case <-req.Context().Done():
		}
	}))
	defer server.Close()
	defer close(release)
	storeDir := t.TempDir()
	runner := srRunner{
		store:  accounts.CodexStore{Dir: filepath.Join(storeDir, "accounts")},
		out:    io.Discard,
		errOut: io.Discard,
		client: http.DefaultClient,
	}
	config := srServerConfig{Name: "team", URL: server.URL}

	started := time.Now()
	if statuses := runner.accountPickerUsage(context.Background(), config); statuses != nil {
		t.Fatalf("statuses = %v, want none without a cache", statuses)
	}
	if elapsed := time.Since(started); elapsed > accountPickerUsageWait+time.Second {
		t.Fatalf("picker waited %s on a slow server", elapsed)
	}

	ledger := newSessionLedger(runner.store.StoreDir())
	cached := []remoteServerUsageStatus{{}}
	cached[0].ID = "cached-account"
	if err := ledger.writeJSON(sessionUsageCachePath(ledger, config), sessionUsageCache{FetchedAt: time.Now().Add(-time.Minute), Statuses: cached}); err != nil {
		t.Fatal(err)
	}
	statuses := runner.accountPickerUsage(context.Background(), config)
	if len(statuses) != 1 || statuses[0].ID != "cached-account" {
		t.Fatalf("statuses = %+v, want the recent cached copy", statuses)
	}

	if err := ledger.writeJSON(sessionUsageCachePath(ledger, config), sessionUsageCache{FetchedAt: time.Now().Add(-time.Hour), Statuses: cached}); err != nil {
		t.Fatal(err)
	}
	if statuses := runner.accountPickerUsage(context.Background(), config); statuses != nil {
		t.Fatalf("statuses = %+v, want none from an hour-old cache", statuses)
	}
}
