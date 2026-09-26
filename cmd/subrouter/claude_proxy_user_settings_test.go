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
	"github.com/manaflow-ai/subrouter/internal/agents/claude"
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
	body, err := withClaudeUserSettings(launch, userSettings, "")
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
	body, err := withClaudeUserSettings(launch, userSettings, "")
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
		body, err := withClaudeUserSettings(launch, path, "")
		if err != nil {
			t.Fatal(err)
		}
		if string(body) != string(launch) {
			t.Fatalf("%q changed launch settings: %s", path, body)
		}
	}
}

func TestClaudeProxyUserSettingsPathIgnoresHermeticStoresAndOptOut(t *testing.T) {
	if got := claudeProxyUserSettingsPath(t.TempDir()); got != "" {
		t.Fatalf("hermetic store read user settings at %q", got)
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	storeDir := claude.DefaultStore().Dir
	if got, want := claudeProxyUserSettingsPath(storeDir), filepath.Join(home, ".claude", "settings.json"); got != want {
		t.Fatalf("user settings path = %q, want %q", got, want)
	}
	t.Setenv(claudeProxyUserSettingsEnv, "off")
	if got := claudeProxyUserSettingsPath(storeDir); got != "" {
		t.Fatalf("opted-out launch read user settings at %q", got)
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
	if statuses, notice := runner.accountPickerUsage(context.Background(), config); statuses != nil || notice != "" {
		t.Fatalf("statuses = %v, notice = %q, want none without a cache", statuses, notice)
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
	statuses, notice := runner.accountPickerUsage(context.Background(), config)
	if len(statuses) != 1 || statuses[0].ID != "cached-account" || !strings.Contains(notice, "1m0s ago") {
		t.Fatalf("statuses = %+v, notice = %q, want the recent cached copy", statuses, notice)
	}

	if err := ledger.writeJSON(sessionUsageCachePath(ledger, config), sessionUsageCache{FetchedAt: time.Now().Add(-time.Hour), Statuses: cached}); err != nil {
		t.Fatal(err)
	}
	if statuses, _ := runner.accountPickerUsage(context.Background(), config); statuses != nil {
		t.Fatalf("statuses = %+v, want none from an hour-old cache", statuses)
	}
}

func TestWithClaudeUserSettingsKeepsProxyConfigChoicesAndRoutingCase(t *testing.T) {
	dir := t.TempDir()
	userSettings := filepath.Join(dir, "user.json")
	if err := os.WriteFile(userSettings, []byte(`{
		"theme": "dark",
		"model": "user-model",
		"hooks": {"SessionStart": []},
		"env": {"anthropic_base_url": "https://elsewhere.example", "USER_ONLY": "kept"}
	}`), 0o600); err != nil {
		t.Fatal(err)
	}
	proxySettings := filepath.Join(dir, "proxy.json")
	if err := os.WriteFile(proxySettings, []byte(`{"theme": "light", "hooks": {"Stop": []}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	launch := []byte(`{"env":{"ANTHROPIC_BASE_URL":"http://127.0.0.1:1"}}`)
	body, err := withClaudeUserSettings(launch, userSettings, proxySettings)
	if err != nil {
		t.Fatal(err)
	}
	var merged map[string]any
	if err := json.Unmarshal(body, &merged); err != nil {
		t.Fatal(err)
	}
	if _, ok := merged["theme"]; ok {
		t.Fatalf("user theme hid the proxy's own choice: %s", body)
	}
	if hooks, _ := merged["hooks"].(map[string]any); merged["model"] != "user-model" || hooks["SessionStart"] == nil {
		t.Fatalf("user settings missing: %s", body)
	}
	env, _ := merged["env"].(map[string]any)
	if _, ok := env["anthropic_base_url"]; ok || env["ANTHROPIC_BASE_URL"] != "http://127.0.0.1:1" || env["USER_ONLY"] != "kept" {
		t.Fatalf("env = %v, want routing keys owned by sr in any case", env)
	}
}
