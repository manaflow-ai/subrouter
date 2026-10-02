package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func hookCommands(t *testing.T, settings map[string]any, event string) []string {
	t.Helper()
	hooks, _ := settings["hooks"].(map[string]any)
	groups, _ := hooks[event].([]any)
	var commands []string
	for _, group := range groups {
		entries, _ := group.(map[string]any)["hooks"].([]any)
		for _, entry := range entries {
			commands = append(commands, entry.(map[string]any)["command"].(string))
		}
	}
	slices.Sort(commands)
	return commands
}

// TestClaudeCLISettingsPrecedence checks the overlay order: the user's
// settings file, then --settings, then sr's routing values. Hooks from every
// layer survive.
func TestClaudeCLISettingsPrecedence(t *testing.T) {
	root := t.TempDir()
	userPath := filepath.Join(root, "user.json")
	if err := os.WriteFile(userPath, []byte(`{
		"model": "user", "effortLevel": "user",
		"env": {"FROM": "user", "USER_ONLY": "1"},
		"hooks": {"PreToolUse": [{"matcher": "Bash", "hooks": [{"type": "command", "command": "user-hook"}]}]}
	}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cliPath := filepath.Join(root, "cli.json")
	if err := os.WriteFile(cliPath, []byte(`{
		"model": "cli-file",
		"apiKeyHelper": "/bin/steal",
		"env": {"FROM": "cli", "ANTHROPIC_BASE_URL": "http://cli.invalid"},
		"hooks": {"PreToolUse": [{"matcher": "Bash", "hooks": [{"type": "command", "command": "cli-hook"}]}]}
	}`), 0o600); err != nil {
		t.Fatal(err)
	}
	routing := []byte(`{"env": {"ANTHROPIC_BASE_URL": "http://router", "ANTHROPIC_AUTH_TOKEN": ""}, "hooks": {"Stop": [{"hooks": [{"type": "command", "command": "sr-hook"}]}]}}`)
	args := []string{"--settings", cliPath, "-p", "x", "--settings={\"model\":\"cli-inline\"}", "--", "--settings", "/not/an/option"}

	body, err := withClaudeCLISettings(routing, args)
	if err != nil {
		t.Fatal(err)
	}
	body, err = withClaudeUserSettings(body, userPath, "")
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	env := got["env"].(map[string]any)
	if env["FROM"] != "cli" || env["USER_ONLY"] != "1" {
		t.Fatalf("--settings env must sit above user env: %v", env)
	}
	if env["ANTHROPIC_BASE_URL"] != "http://router" || env["ANTHROPIC_AUTH_TOKEN"] != "" {
		t.Fatalf("routing env must win over --settings: %v", env)
	}
	if got["model"] != "cli-inline" || got["effortLevel"] != "user" {
		t.Fatalf("later --settings must win, user fills the rest: model=%v effort=%v", got["model"], got["effortLevel"])
	}
	if _, ok := got["apiKeyHelper"]; ok {
		t.Fatal("credential-source setting from --settings must be dropped")
	}
	if pre := hookCommands(t, got, "PreToolUse"); !slices.Equal(pre, []string{"cli-hook", "user-hook"}) {
		t.Fatalf("PreToolUse hooks = %v", pre)
	}
	if stop := hookCommands(t, got, "Stop"); !slices.Equal(stop, []string{"sr-hook"}) {
		t.Fatalf("Stop hooks = %v", stop)
	}
}

func TestClaudeCLISettingsLeavesBodyWithoutSettingsArgs(t *testing.T) {
	body := []byte(`{"env":{"A":"1"}}`)
	got, err := withClaudeCLISettings(body, []string{"-p", "x", "--", "--settings", "literal"})
	if err != nil || string(got) != string(body) {
		t.Fatalf("body = %s, err = %v", got, err)
	}
}

func TestClaudeCLISettingsRejectsUnreadableValues(t *testing.T) {
	for _, args := range [][]string{
		{"--settings", filepath.Join(t.TempDir(), "missing.json")},
		{"--settings", "{not json"},
		{"--settings"},
		{"--settings="},
	} {
		if _, err := withClaudeCLISettings([]byte(`{}`), args); err == nil {
			t.Fatalf("args %q were accepted", args)
		}
	}
}
