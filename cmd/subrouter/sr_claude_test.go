package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/manaflow-ai/subrouter/internal/agents/claude"
)

func TestClaudeEnvPrintsActiveProfile(t *testing.T) {
	store := claude.Store{Dir: t.TempDir()}
	if _, err := store.CreateProfile("work"); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	runner := claudeRunner{store: store, in: strings.NewReader(""), out: &out, errOut: &out}
	if err := runner.run(context.Background(), []string{"env"}); err != nil {
		t.Fatal(err)
	}

	got := out.String()
	if !strings.Contains(got, "export CLAUDE_CONFIG_DIR=") {
		t.Fatalf("env output = %q", got)
	}
	if !strings.Contains(got, "/claude/work") {
		t.Fatalf("env output missing profile path: %q", got)
	}
}

func TestClaudeEnvPrefersCodexAccountsAlias(t *testing.T) {
	home := t.TempDir()
	store := claude.Store{Dir: filepath.Join(home, ".subrouter", "codex")}
	if _, err := store.CreateProfile("work"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(store.Dir, filepath.Join(home, ".codex-accounts")); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	runner := claudeRunner{store: store, in: strings.NewReader(""), out: &out, errOut: &out}
	if err := runner.run(context.Background(), []string{"env"}); err != nil {
		t.Fatal(err)
	}

	want := "export CLAUDE_CONFIG_DIR=" + filepath.Join(home, ".codex-accounts", "claude", "work")
	if got := strings.TrimSpace(out.String()); got != want {
		t.Fatalf("env output = %q, want %q", got, want)
	}
}

func TestClaudeSwitchSupportsPartialProfile(t *testing.T) {
	store := claude.Store{Dir: t.TempDir()}
	if _, err := store.CreateProfile("personal"); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	runner := claudeRunner{store: store, in: strings.NewReader(""), out: &out, errOut: &out}
	if err := runner.run(context.Background(), []string{"switch", "pers"}); err != nil {
		t.Fatal(err)
	}

	if active := store.ActiveProfile(); active != "personal" {
		t.Fatalf("active = %q, want personal", active)
	}
	if !strings.Contains(out.String(), "Active Claude profile: personal") {
		t.Fatalf("switch output = %q", out.String())
	}
}

func TestClaudeFlagsRunActiveProfile(t *testing.T) {
	home := t.TempDir()
	store := claude.Store{Dir: filepath.Join(home, ".subrouter", "codex")}
	if _, err := store.CreateProfile("work"); err != nil {
		t.Fatal(err)
	}

	binDir := filepath.Join(home, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	recordPath := filepath.Join(home, "claude-run.txt")
	claudePath := filepath.Join(binDir, "claude")
	script := "#!/bin/sh\nprintf 'config=%s\\nargs=%s\\n' \"$CLAUDE_CONFIG_DIR\" \"$*\" > " + shellQuote(recordPath) + "\n"
	if err := os.WriteFile(claudePath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	var out bytes.Buffer
	runner := claudeRunner{store: store, in: strings.NewReader(""), out: &out, errOut: &out}
	err := runner.run(context.Background(), []string{"--dangerously-skip-permissions", "--resume", "1721c0ce-b3bd-4d73-8b33-b3d02b677074"})
	if err != nil {
		t.Fatal(err)
	}

	body, err := os.ReadFile(recordPath)
	if err != nil {
		t.Fatal(err)
	}
	got := string(body)
	if !strings.Contains(got, "config="+store.ClaudeConfigDir("work")) {
		t.Fatalf("Claude did not receive active config dir:\n%s", got)
	}
	if !strings.Contains(got, "args=--dangerously-skip-permissions --resume 1721c0ce-b3bd-4d73-8b33-b3d02b677074") {
		t.Fatalf("Claude did not receive flags:\n%s", got)
	}
}

func TestProxyClaudeRunKeepsNamedProfileSemantics(t *testing.T) {
	store := claude.Store{Dir: t.TempDir()}
	if _, err := store.CreateProfile("work"); err != nil {
		t.Fatal(err)
	}

	configDir, args, err := proxyClaudeInvocation(
		store,
		[]string{"run", "work", "--resume", "session-a"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if configDir != store.ClaudeConfigDir("work") {
		t.Fatalf("config dir = %q, want work profile", configDir)
	}
	if got := strings.Join(args, " "); got != "--resume session-a" {
		t.Fatalf("Claude args = %q", got)
	}
}

func TestProxyClaudeProfileShorthandKeepsNamedProfileSemantics(t *testing.T) {
	store := claude.Store{Dir: t.TempDir()}
	if _, err := store.CreateProfile("personal"); err != nil {
		t.Fatal(err)
	}

	configDir, args, err := proxyClaudeInvocation(
		store,
		[]string{"personal", "--verbose"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if configDir != store.ClaudeConfigDir("personal") {
		t.Fatalf("config dir = %q, want personal profile", configDir)
	}
	if got := strings.Join(args, " "); got != "--verbose" {
		t.Fatalf("Claude args = %q", got)
	}
}

func TestProxyClaudeAllowsProfilelessFlagAndRunInvocations(t *testing.T) {
	store := claude.Store{Dir: t.TempDir()}
	for _, input := range [][]string{
		{"--print", "hello"},
		{"run", "--print", "hello"},
	} {
		configDir, args, err := proxyClaudeInvocation(store, input)
		if err != nil {
			t.Fatalf("proxyClaudeInvocation(%v): %v", input, err)
		}
		if configDir != "" {
			t.Fatalf("config dir for %v = %q, want default", input, configDir)
		}
		want := input
		if input[0] == "run" {
			want = input[1:]
		}
		if strings.Join(args, "\x00") != strings.Join(want, "\x00") {
			t.Fatalf("args for %v = %v, want %v", input, args, want)
		}
	}
}

func TestPrepareClaudeLoginFastPathSeedsFreshDir(t *testing.T) {
	dir := t.TempDir()
	configDir := filepath.Join(dir, "profile")
	if err := prepareClaudeLoginFastPath(configDir); err != nil {
		t.Fatalf("prepareClaudeLoginFastPath: %v", err)
	}
	state, err := os.ReadFile(filepath.Join(configDir, ".claude.json"))
	if err != nil {
		t.Fatalf("read .claude.json: %v", err)
	}
	if !strings.Contains(string(state), `"hasCompletedOnboarding": true`) {
		t.Fatalf("onboarding not seeded:\n%s", state)
	}
	settings, err := os.ReadFile(filepath.Join(configDir, "settings.json"))
	if err != nil {
		t.Fatalf("read settings.json: %v", err)
	}
	if !strings.Contains(string(settings), `"forceLoginMethod": "claudeai"`) {
		t.Fatalf("login method not seeded:\n%s", settings)
	}
}

func TestPrepareClaudeLoginFastPathPreservesExistingChoices(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".claude.json"), []byte(`{"theme":"light"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(`{"forceLoginMethod":"console"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := prepareClaudeLoginFastPath(dir); err != nil {
		t.Fatalf("prepareClaudeLoginFastPath: %v", err)
	}
	state, _ := os.ReadFile(filepath.Join(dir, ".claude.json"))
	if !strings.Contains(string(state), `"theme": "light"`) || !strings.Contains(string(state), `"hasCompletedOnboarding": true`) {
		t.Fatalf("existing state not preserved:\n%s", state)
	}
	settings, _ := os.ReadFile(filepath.Join(dir, "settings.json"))
	if !strings.Contains(string(settings), `"forceLoginMethod":"console"`) {
		t.Fatalf("existing login method overwritten:\n%s", settings)
	}
}
