//go:build !windows

package main

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/internal/agents/claude"
)

const cliSettingsHookCommand = "echo cli-pretooluse-hook"

// cliSettingsHookBody is a caller's --settings value: a PreToolUse hook plus
// a routing override that sr must not let through.
const cliSettingsHookBody = `{
	"hooks": {"PreToolUse": [{"matcher": "Bash", "hooks": [{"type": "command", "command": "` + cliSettingsHookCommand + `"}]}]},
	"env": {"ANTHROPIC_BASE_URL": "http://cli-settings.invalid", "CLI_SETTINGS_ONLY": "kept"}
}`

// installSettingsCapturingClaude puts a fake claude first on PATH that
// copies whatever --settings file it is launched with to capturePath.
func installSettingsCapturingClaude(t *testing.T, home string) (capturePath, argsPath string) {
	t.Helper()
	binDir := filepath.Join(home, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	capturePath = filepath.Join(home, "launched-settings.json")
	argsPath = filepath.Join(home, "launched-args")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + shellQuote(argsPath) + "\nprev=''\nfor arg in \"$@\"; do\n  if [ \"$prev\" = '--settings' ]; then settings=$arg; break; fi\n  prev=$arg\ndone\ncat \"$settings\" > " + shellQuote(capturePath) + "\n"
	if err := os.WriteFile(filepath.Join(binDir, "claude"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return capturePath, argsPath
}

type launchedClaudeSettings struct {
	Env   map[string]string `json:"env"`
	Hooks map[string][]struct {
		Matcher string `json:"matcher"`
		Hooks   []struct {
			Type    string `json:"type"`
			Command string `json:"command"`
		} `json:"hooks"`
	} `json:"hooks"`
}

func readLaunchedClaudeSettings(t *testing.T, capturePath, argsPath string) launchedClaudeSettings {
	t.Helper()
	argsBody, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(argsBody), "--settings\n") != 1 {
		t.Fatalf("Claude must get exactly one --settings argument, got:\n%s", argsBody)
	}
	body, err := os.ReadFile(capturePath)
	if err != nil {
		t.Fatal(err)
	}
	var settings launchedClaudeSettings
	if err := json.Unmarshal(body, &settings); err != nil {
		t.Fatalf("launched settings = %q: %v", body, err)
	}
	return settings
}

func requireCLIPreToolUseHook(t *testing.T, settings launchedClaudeSettings) {
	t.Helper()
	for _, group := range settings.Hooks["PreToolUse"] {
		for _, hook := range group.Hooks {
			if group.Matcher == "Bash" && hook.Type == "command" && hook.Command == cliSettingsHookCommand {
				return
			}
		}
	}
	t.Fatalf("the --settings PreToolUse hook did not reach the launched claude: %+v", settings.Hooks)
}

func cliSettingsArgVariants(t *testing.T, home string) map[string][]string {
	t.Helper()
	path := filepath.Join(home, "cli-settings.json")
	if err := os.WriteFile(path, []byte(cliSettingsHookBody), 0o600); err != nil {
		t.Fatal(err)
	}
	return map[string][]string{
		"file":        {"--settings", path, "-p", "hi"},
		"file equals": {"--settings=" + path, "-p", "hi"},
		"inline json": {"--settings", cliSettingsHookBody, "-p", "hi"},
	}
}

func TestManagedClaudeLaunchKeepsCLISettingsHooks(t *testing.T) {
	for name, args := range cliSettingsArgVariants(t, t.TempDir()) {
		t.Run(name, func(t *testing.T) {
			home := t.TempDir()
			tempRoot := filepath.Join(home, "tmp")
			if err := os.MkdirAll(tempRoot, 0o700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("TMPDIR", tempRoot)
			store := claude.Store{Dir: filepath.Join(home, ".subrouter", "codex")}
			if _, err := store.CreateProfile("work"); err != nil {
				t.Fatal(err)
			}
			listener, err := net.Listen("tcp4", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			_, port, err := net.SplitHostPort(listener.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			profileSettings := `{"env":{"ANTHROPIC_BASE_URL":"http://localhost:` + port + `","ANTHROPIC_AUTH_TOKEN":"secret"}}`
			if err := os.WriteFile(filepath.Join(store.ClaudeConfigDir("work"), "settings.json"), []byte(profileSettings), 0o600); err != nil {
				t.Fatal(err)
			}
			capturePath, argsPath := installSettingsCapturingClaude(t, home)
			runner := claudeRunner{
				store: store, in: strings.NewReader(""), out: io.Discard, errOut: io.Discard,
				authStatus: func(context.Context, string, string) (*claude.AuthStatus, error) {
					return &claude.AuthStatus{LoggedIn: true}, nil
				},
			}
			if err := runner.runClaude(t.Context(), "work", args); err != nil {
				t.Fatal(err)
			}
			settings := readLaunchedClaudeSettings(t, capturePath, argsPath)
			requireCLIPreToolUseHook(t, settings)
			if settings.Env["CLI_SETTINGS_ONLY"] != "kept" {
				t.Fatalf("--settings env did not reach the launched claude: %v", settings.Env)
			}
			if settings.Env["ANTHROPIC_BASE_URL"] != "http://127.0.0.1:"+port || settings.Env["ANTHROPIC_AUTH_TOKEN"] != "secret" {
				t.Fatalf("--settings replaced the managed route: %v", settings.Env)
			}
		})
	}
}

func TestPooledClaudeLaunchKeepsCLISettingsHooks(t *testing.T) {
	for name, args := range cliSettingsArgVariants(t, t.TempDir()) {
		t.Run(name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			tempRoot := filepath.Join(home, "tmp")
			if err := os.MkdirAll(tempRoot, 0o700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("TMPDIR", tempRoot)
			capturePath, argsPath := installSettingsCapturingClaude(t, home)
			configDir := filepath.Join(home, "proxy-config")
			if err := os.MkdirAll(configDir, 0o700); err != nil {
				t.Fatal(err)
			}
			runner := srRunner{
				store: accounts.CodexStore{Dir: filepath.Join(home, "hermetic-store", "accounts")},
				in:    strings.NewReader(""), out: io.Discard, errOut: io.Discard,
			}
			if err := runner.launchProxyClaude(t.Context(), args, "http://127.0.0.1:31415", "subrouter", configDir, "", ""); err != nil {
				t.Fatal(err)
			}
			settings := readLaunchedClaudeSettings(t, capturePath, argsPath)
			requireCLIPreToolUseHook(t, settings)
			if settings.Env["CLI_SETTINGS_ONLY"] != "kept" {
				t.Fatalf("--settings env did not reach the launched claude: %v", settings.Env)
			}
			if got := settings.Env["ANTHROPIC_BASE_URL"]; got == "http://cli-settings.invalid" || got == "" {
				t.Fatalf("--settings replaced the pooled route: ANTHROPIC_BASE_URL=%q", got)
			}
		})
	}
}
