package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/manaflow-ai/subrouter/internal/accounts"
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

func TestClaudeCodexLaunchesClaudeWithBridgeEnvironment(t *testing.T) {
	home := t.TempDir()
	binDir := filepath.Join(home, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	recordPath := filepath.Join(home, "claude-codex-run.txt")
	claudePath := filepath.Join(binDir, "claude")
	script := "#!/bin/sh\nprintf 'base=%s\\nauth=%s\\ncustom=%s\\noauth=%s\\nmodel=%s\\nfable=%s\\nfable_name=%s\\nopus=%s\\nsonnet=%s\\nhaiku=%s\\nhaiku_name=%s\\neffort=%s\\nargs=%s\\n' \"$ANTHROPIC_BASE_URL\" \"$ANTHROPIC_AUTH_TOKEN\" \"$ANTHROPIC_CUSTOM_HEADERS\" \"$CLAUDE_CODE_OAUTH_TOKEN\" \"$ANTHROPIC_MODEL\" \"$ANTHROPIC_DEFAULT_FABLE_MODEL\" \"$ANTHROPIC_DEFAULT_FABLE_MODEL_NAME\" \"$ANTHROPIC_DEFAULT_OPUS_MODEL\" \"$ANTHROPIC_DEFAULT_SONNET_MODEL\" \"$ANTHROPIC_DEFAULT_HAIKU_MODEL\" \"$ANTHROPIC_DEFAULT_HAIKU_MODEL_NAME\" \"$CLAUDE_CODE_EFFORT_LEVEL\" \"$*\" > " + shellQuote(recordPath) + "\n"
	if err := os.WriteFile(claudePath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("ANTHROPIC_CUSTOM_HEADERS", "X-Leaked: value")
	t.Setenv("ANTHROPIC_MODEL", "claude-fable-5")
	t.Setenv("ANTHROPIC_DEFAULT_FABLE_MODEL", "claude-fable-5")
	t.Setenv("ANTHROPIC_CUSTOM_MODEL_OPTION", "claude-fable-5")
	t.Setenv("ANTHROPIC_CUSTOM_MODEL_OPTION_NAME", "Fable 5")
	t.Setenv("CLAUDE_CODE_EFFORT_LEVEL", "high")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "must-not-leak")

	store := accounts.CodexStore{Dir: filepath.Join(home, "codex", "accounts")}
	serverStore := defaultSRServerStore(store)
	if err := serverStore.save(srServerFile{
		Default: "team",
		Servers: []srServerConfig{{Name: "team", URL: "http://subrouter-team:31415"}},
	}); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	runner := srRunner{store: store, in: strings.NewReader(""), out: &out, errOut: &out}
	if err := runner.claudeCodex(context.Background(), []string{"-p", "hello"}); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(recordPath)
	if err != nil {
		t.Fatal(err)
	}
	got := string(body)
	for _, want := range []string{
		"base=http://subrouter-team:31415/claude-codex",
		"auth=subrouter",
		"custom=",
		"oauth=",
		"model=",
		"fable=claude-codex-sol",
		"fable_name=Codex Sol",
		"opus=claude-codex-sol",
		"sonnet=claude-codex-terra",
		"haiku=claude-codex-luna",
		"haiku_name=Codex Luna",
		"effort=",
		"claude-codex-hook 'http://subrouter-team:31415/claude-codex/hooks/pre-compact'",
		"args=--settings {\"hooks\":{\"PreCompact\":[{\"hooks\":[{\"command\":",
		"\"timeout\":10,\"type\":\"command\"}],\"matcher\":\"\"}]}} --model claude-codex-sol -p hello",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in launch environment:\n%s", want, got)
		}
	}
	if !strings.Contains(out.String(), "Claude Code -> Codex Sol/Terra/Luna via Subrouter http://subrouter-team:31415 (default Sol; subagents: Luna=haiku, Terra=sonnet, Sol=opus/inherit)") {
		t.Fatalf("missing route banner: %q", out.String())
	}
}

func TestClaudeCodexArgsPreservesExplicitModel(t *testing.T) {
	got := claudeCodexArgs([]string{"--model", "claude-codex-terra", "-p", "hello"})
	want := []string{"--model", "claude-codex-terra", "-p", "hello"}
	if !slices.Equal(got, want) {
		t.Fatalf("args = %q, want %q", got, want)
	}
	got = claudeCodexArgs([]string{"--model=claude-codex-luna", "-p", "hello"})
	want = []string{"--model=claude-codex-luna", "-p", "hello"}
	if !slices.Equal(got, want) {
		t.Fatalf("args = %q, want %q", got, want)
	}
}

func TestClaudeCodexHookArgsPreservesExplicitSettings(t *testing.T) {
	args := []string{"--settings", "/tmp/custom.json", "-p", "hello"}
	if got := claudeCodexHookArgs(args, "sr claude-codex-hook http://subrouter"); !slices.Equal(got, args) {
		t.Fatalf("args = %q, want %q", got, args)
	}
}
