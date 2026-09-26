package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func writeClaudeConfig(t *testing.T, path string, value any) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
}

func readClaudeConfig(t *testing.T, path string) map[string]any {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func projectFlag(config map[string]any, project, flag string) any {
	projects, _ := config["projects"].(map[string]any)
	fields, _ := projects[project].(map[string]any)
	return fields[flag]
}

func TestSyncClaudeProjectTrustSharesAcceptedTrustBothWays(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	userPath := claudeUserConfigPath()
	if userPath != filepath.Join(home, ".claude.json") {
		t.Fatalf("user config path = %q", userPath)
	}
	writeClaudeConfig(t, userPath, map[string]any{
		"numStartups": 42,
		"oauthAccount": map[string]any{"emailAddress": "leo@example.com"},
		"projects": map[string]any{
			"/work/trusted":   map[string]any{"hasTrustDialogAccepted": true, "hasCompletedProjectOnboarding": true, "allowedTools": []string{"Bash"}},
			"/work/untrusted": map[string]any{"hasTrustDialogAccepted": false},
		},
	})
	proxyDir := filepath.Join(home, ".subrouter", "codex", "claude-proxy", "scope")
	writeClaudeConfig(t, filepath.Join(proxyDir, ".claude.json"), map[string]any{
		"userID": "proxy-user",
		"projects": map[string]any{
			"/work/untrusted":   map[string]any{"history": []string{"x"}},
			"/work/proxy-only":  map[string]any{"hasTrustDialogAccepted": true},
			"/work/proxy-false": map[string]any{"hasTrustDialogAccepted": false},
		},
	})

	if err := syncClaudeProjectTrust(userPath, proxyDir); err != nil {
		t.Fatal(err)
	}

	proxy := readClaudeConfig(t, filepath.Join(proxyDir, ".claude.json"))
	if projectFlag(proxy, "/work/trusted", "hasTrustDialogAccepted") != true || projectFlag(proxy, "/work/trusted", "hasCompletedProjectOnboarding") != true {
		t.Fatalf("user trust did not reach the proxy config: %v", proxy["projects"])
	}
	if projectFlag(proxy, "/work/trusted", "allowedTools") != nil {
		t.Fatal("only trust flags may be copied, not tool permissions")
	}
	if got := projectFlag(proxy, "/work/untrusted", "hasTrustDialogAccepted"); got != nil {
		t.Fatalf("untrusted folder gained trust in the proxy: %v", got)
	}
	if proxy["userID"] != "proxy-user" || projectFlag(proxy, "/work/untrusted", "history") == nil {
		t.Fatalf("proxy config lost its own fields: %v", proxy)
	}

	user := readClaudeConfig(t, userPath)
	if projectFlag(user, "/work/proxy-only", "hasTrustDialogAccepted") != true {
		t.Fatalf("trust accepted in a proxy session was not written back: %v", user["projects"])
	}
	if projectFlag(user, "/work/untrusted", "hasTrustDialogAccepted") != false {
		t.Fatal("an explicit false in the user config must stay false")
	}
	if projectFlag(user, "/work/proxy-false", "hasTrustDialogAccepted") != nil {
		t.Fatal("a proxy false must not be written back")
	}
	if user["numStartups"] != float64(42) || user["oauthAccount"] == nil {
		t.Fatalf("user config lost its own fields: %v", user)
	}
	info, err := os.Stat(userPath)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("user config mode = %v, %v", info.Mode().Perm(), err)
	}

	// A second sync is a no-op.
	before, _ := os.ReadFile(userPath)
	if err := syncClaudeProjectTrust(userPath, proxyDir); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(userPath)
	if string(before) != string(after) {
		t.Fatal("an idempotent sync rewrote the user config")
	}
}

func TestSyncClaudeProjectTrustCreatesProxyConfigButNeverUserConfig(t *testing.T) {
	home := t.TempDir()
	configDir := filepath.Join(home, "custom-claude")
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", configDir)
	userPath := claudeUserConfigPath()
	if userPath != filepath.Join(configDir, ".claude.json") {
		t.Fatalf("CLAUDE_CONFIG_DIR not respected: %q", userPath)
	}
	writeClaudeConfig(t, userPath, map[string]any{"projects": map[string]any{"/a": map[string]any{"hasTrustDialogAccepted": true}}})
	proxyDir := filepath.Join(home, "proxy")
	if err := syncClaudeProjectTrust(userPath, proxyDir); err != nil {
		t.Fatal(err)
	}
	if projectFlag(readClaudeConfig(t, filepath.Join(proxyDir, ".claude.json")), "/a", "hasTrustDialogAccepted") != true {
		t.Fatal("a fresh proxy config should start with the user's trust")
	}

	missingUser := filepath.Join(home, "nobody", ".claude.json")
	writeClaudeConfig(t, filepath.Join(proxyDir, ".claude.json"), map[string]any{"projects": map[string]any{"/b": map[string]any{"hasTrustDialogAccepted": true}}})
	if err := syncClaudeProjectTrust(missingUser, proxyDir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(missingUser); !os.IsNotExist(err) {
		t.Fatal("sr must never create the user's Claude config")
	}
}

func TestSyncClaudeProjectTrustLeavesLockedConfigAlone(t *testing.T) {
	home := t.TempDir()
	userPath := filepath.Join(home, ".claude.json")
	writeClaudeConfig(t, userPath, map[string]any{"projects": map[string]any{"/a": map[string]any{}}})
	proxyDir := filepath.Join(home, "proxy")
	writeClaudeConfig(t, filepath.Join(proxyDir, ".claude.json"), map[string]any{"projects": map[string]any{"/a": map[string]any{"hasTrustDialogAccepted": true}}})
	if err := os.Mkdir(userPath+".lock", 0o700); err != nil {
		t.Fatal(err)
	}
	if err := syncClaudeProjectTrust(userPath, proxyDir); err != nil {
		t.Fatal(err)
	}
	if projectFlag(readClaudeConfig(t, userPath), "/a", "hasTrustDialogAccepted") != nil {
		t.Fatal("sr wrote a config while Claude held its lock")
	}
}
