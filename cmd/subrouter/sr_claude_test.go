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
	script := "#!/bin/sh\nprintf 'config=%s\\npreserve=%s\\npreserve_keys=%s\\nargs=%s\\n' \"$CLAUDE_CONFIG_DIR\" \"$CMUX_PRESERVE_CLAUDE_AUTH_SELECTION_ENV\" \"$CMUX_PRESERVE_CLAUDE_AUTH_SELECTION_ENV_KEYS\" \"$*\" > " + shellQuote(recordPath) + "\n"
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
	if !strings.Contains(got, "preserve=1") {
		t.Fatalf("Claude did not receive cmux preservation opt-in:\n%s", got)
	}
	if !strings.Contains(got, "preserve_keys=CLAUDE_CONFIG_DIR") {
		t.Fatalf("Claude did not receive cmux preservation key list:\n%s", got)
	}
	if !strings.Contains(got, "args=--dangerously-skip-permissions --resume 1721c0ce-b3bd-4d73-8b33-b3d02b677074") {
		t.Fatalf("Claude did not receive flags:\n%s", got)
	}
}
