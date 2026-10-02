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
	if err := ledger.writeJSON(sessionUsageCachePath(ledger, config.Name), sessionUsageCache{FetchedAt: time.Now().Add(-time.Minute), Statuses: cached}); err != nil {
		t.Fatal(err)
	}
	statuses, notice := runner.accountPickerUsage(context.Background(), config)
	if len(statuses) != 1 || statuses[0].ID != "cached-account" || !strings.Contains(notice, "from 1m") {
		t.Fatalf("statuses = %+v, notice = %q, want the recent cached copy", statuses, notice)
	}

	if err := ledger.writeJSON(sessionUsageCachePath(ledger, config.Name), sessionUsageCache{FetchedAt: time.Now().Add(-time.Hour), Statuses: cached}); err != nil {
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
	if merged["theme"] != "dark" {
		t.Fatalf("shared theme did not replace stale home choice: %s", body)
	}
	if hooks, _ := merged["hooks"].(map[string]any); merged["model"] != "user-model" || hooks["SessionStart"] == nil {
		t.Fatalf("user settings missing: %s", body)
	}
	env, _ := merged["env"].(map[string]any)
	if _, ok := env["anthropic_base_url"]; ok || env["ANTHROPIC_BASE_URL"] != "http://127.0.0.1:1" || env["USER_ONLY"] != "kept" {
		t.Fatalf("env = %v, want routing keys owned by sr in any case", env)
	}
}

func TestWithClaudeUserSettingsAllowsSharedProjectMemory(t *testing.T) {
	root := t.TempDir()
	projects := filepath.Join(root, "projects")
	if err := os.MkdirAll(projects, 0o700); err != nil {
		t.Fatal(err)
	}
	settings := filepath.Join(root, "settings.json")
	if err := os.WriteFile(settings, []byte(`{"permissions":{"additionalDirectories":["/tmp/other"]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	body, err := withClaudeProxyMemoryDirectory([]byte(`{"permissions":{"additionalDirectories":["/tmp/other"]}}`), root)
	if err != nil {
		t.Fatal(err)
	}
	var merged map[string]any
	if err := json.Unmarshal(body, &merged); err != nil {
		t.Fatal(err)
	}
	permissions, _ := merged["permissions"].(map[string]any)
	directories, _ := permissions["additionalDirectories"].([]any)
	if len(directories) != 2 || directories[0] != "/tmp/other" || directories[1] != projects {
		t.Fatalf("additionalDirectories = %v, want user and shared projects", directories)
	}
}

func TestClaudeProxySettingsRefreshAndMergeHomeValues(t *testing.T) {
	root := t.TempDir()
	user := filepath.Join(root, "user.json")
	home := filepath.Join(root, "home.json")
	userBody := `{"model":"new","env":{"USER":"new"},"hooks":{"Stop":[{"hooks":[{"type":"command","command":"shared"}]}]},"permissions":{"allow":["Read(shared)"],"deny":["Read(secret)"]}}`
	homeBody := `{"model":"old","effortLevel":"high","env":{"HOME_ONLY":"kept","USER":"old"},"hooks":{"Stop":[{"hooks":[{"type":"command","command":"home"}]}]},"permissions":{"allow":["Read(home)"]}}`
	for path, body := range map[string]string{user: userBody, home: homeBody} {
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	launch := []byte(`{"env":{"ANTHROPIC_BASE_URL":"http://localhost"},"hooks":{"Stop":[{"hooks":[{"type":"command","command":"sr"}]}]}}`)
	for _, model := range []string{"new", "newer"} {
		if err := os.WriteFile(user, []byte(strings.ReplaceAll(userBody, `"new"`, `"`+model+`"`)), 0o600); err != nil {
			t.Fatal(err)
		}
		body, err := withClaudeUserSettings(launch, user, home)
		if err != nil {
			t.Fatal(err)
		}
		var got map[string]any
		if err := json.Unmarshal(body, &got); err != nil {
			t.Fatal(err)
		}
		if got["model"] != model || got["effortLevel"] != "high" {
			t.Fatalf("settings = %s", body)
		}
		hooks := got["hooks"].(map[string]any)["Stop"].([]any)
		if len(hooks) != 3 {
			t.Fatalf("lost hooks: %s", body)
		}
		env := got["env"].(map[string]any)
		if env["USER"] != model || env["HOME_ONLY"] != "kept" || env["ANTHROPIC_BASE_URL"] != "http://localhost" {
			t.Fatalf("env: %s", body)
		}
		permissions := got["permissions"].(map[string]any)
		if len(permissions["allow"].([]any)) != 2 || len(permissions["deny"].([]any)) != 1 {
			t.Fatalf("permissions: %s", body)
		}
	}
	if got, err := os.ReadFile(home); err != nil || string(got) != homeBody {
		t.Fatal("overwrote Claude's settings")
	}
}

func TestClaudeProxyMemoryDirectoryResolvesSymlinksAndDeduplicates(t *testing.T) {
	root := t.TempDir()
	real := t.TempDir()
	if err := os.Symlink(real, filepath.Join(root, "projects")); err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"permissions":{"allow":["Read(example)"]}}`)
	for i := 0; i < 2; i++ {
		var err error
		body, err = withClaudeProxyMemoryDirectory(body, root)
		if err != nil {
			t.Fatal(err)
		}
	}
	var got struct {
		Permissions struct {
			AdditionalDirectories []string
			Allow                 []string
		}
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	resolved, err := filepath.EvalSymlinks(real)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Permissions.AdditionalDirectories) != 1 || got.Permissions.AdditionalDirectories[0] != resolved || len(got.Permissions.Allow) != 1 {
		t.Fatalf("settings: %s", body)
	}
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatal(err)
	}
	if raw["env"].(map[string]any)["CLAUDE_CODE_REMOTE_MEMORY_DIR"] != root {
		t.Fatalf("memory root env missing: %s", body)
	}
}

func TestManagedClaudeLaunchCarriesUserSettings(t *testing.T) {
	userPath := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(userPath, []byte(`{
		"env": {
			"CLAUDE_CODE_MAX_CONCURRENT_SUBAGENTS": "200",
			"CLAUDE_CODE_MAX_SUBAGENT_SPAWN_DEPTH": "5",
			"ANTHROPIC_BASE_URL": "https://not-the-router.example"
		},
		"forceLoginMethod": "console",
		"includeCoAuthoredBy": false
	}`), 0o600); err != nil {
		t.Fatal(err)
	}
	launch, err := managedClaudeLaunchSettings("https://router.example", "/tmp/profile")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name    string
		body    []byte
		baseURL string
	}{
		{name: "routed", body: launch, baseURL: "https://router.example"},
		{name: "unrouted", body: nil, baseURL: "https://not-the-router.example"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body, err := withManagedClaudeUserSettings(tc.body, userPath, nil)
			if err != nil {
				t.Fatal(err)
			}
			var settings map[string]any
			if err := json.Unmarshal(body, &settings); err != nil {
				t.Fatal(err)
			}
			env, _ := settings["env"].(map[string]any)
			if env["CLAUDE_CODE_MAX_CONCURRENT_SUBAGENTS"] != "200" || env["CLAUDE_CODE_MAX_SUBAGENT_SPAWN_DEPTH"] != "5" {
				t.Fatalf("user env missing: %v", env)
			}
			if env["ANTHROPIC_BASE_URL"] != tc.baseURL {
				t.Fatalf("ANTHROPIC_BASE_URL = %v, want %s", env["ANTHROPIC_BASE_URL"], tc.baseURL)
			}
			if _, ok := settings["forceLoginMethod"]; ok {
				t.Fatal("credential-source setting must be dropped")
			}
			if settings["includeCoAuthoredBy"] != false {
				t.Fatalf("non-env user setting missing: %v", settings)
			}
		})
	}
	if body, err := withManagedClaudeUserSettings(nil, "", nil); err != nil || body != nil {
		t.Fatalf("disabled merge must leave an unrouted launch without settings, got %s, %v", body, err)
	}
}
