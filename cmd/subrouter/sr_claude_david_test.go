package main

import (
	"bytes"
	"github.com/manaflow-ai/subrouter/internal/accounts"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestClaudeDavidDispatch(t *testing.T) {
	if !isDirectSRCommand("claude-david") {
		t.Fatal("shorthand not registered")
	}
	if shouldRouteSRCommand("claude-david") {
		t.Fatal("shorthand must launch locally, not become a remote account operation")
	}
}

func TestClaudeDavidFlagsPassThrough(t *testing.T) {
	args := []string{"--dangerously-skip-permissions", "--resume", "conversation-id", "--model", "fable", "-p", "fix this bug"}
	got, err := parseClaudeAWSArgs(args, "david")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"--dangerously-skip-permissions", "--resume", "conversation-id", "-p", "fix this bug"}
	if got.account != "david" || got.model != "fable" || !reflect.DeepEqual(got.passthrough, want) {
		t.Fatalf("launch = %+v", got)
	}
	// Everything after -- belongs to Claude, including account-looking text.
	got, err = parseClaudeAWSArgs([]string{"--", "--account", "friend-b"}, "david")
	if err != nil || got.account != "david" || !reflect.DeepEqual(got.passthrough, []string{"--", "--account", "friend-b"}) {
		t.Fatalf("literal arguments = %+v, %v", got, err)
	}
}

func TestClaudeDavidRejectsAccountOverride(t *testing.T) {
	for _, args := range [][]string{{"--account", "friend-b"}, {"--aws-account", "friend-b"}, {"--account=friend-b"}, {"--aws-account=friend-b"}, {"--account", ""}, {"--account", "--dangerously-skip-permissions"}} {
		if _, err := parseClaudeAWSArgs(args, "david"); err == nil {
			t.Errorf("accepted override %q", args)
		}
	}
	for _, args := range [][]string{{"--account", "david"}, {"--aws-account=david"}} {
		got, err := parseClaudeAWSArgs(args, "david")
		if err != nil || got.account != "david" {
			t.Errorf("same account: %+v %v", got, err)
		}
	}
}

func TestClaudeAWSCanonicalSelector(t *testing.T) {
	got, err := parseClaudeAWSArgs([]string{"--account=david", "--dangerously-skip-permissions"}, "")
	if err != nil || got.account != "david" || !reflect.DeepEqual(got.passthrough, []string{"--dangerously-skip-permissions"}) {
		t.Fatalf("canonical launch: %+v %v", got, err)
	}
}

func TestClaudeDavidLaunchesPinnedChild(t *testing.T) {
	isolateCloudConfig(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("SUBROUTER_SERVER", "")
	t.Setenv("SUBROUTER_CODEX_SERVER", "")
	store := accounts.CodexStore{Dir: filepath.Join(home, "accounts")}
	writeDoctorServerFile(t, store, srServerConfig{Name: "test", URL: "http://127.0.0.1:31415"})
	bin := filepath.Join(home, "bin")
	if err := os.MkdirAll(bin, 0755); err != nil {
		t.Fatal(err)
	}
	script := `#!/bin/sh
printf 'account=%s\n' "$ANTHROPIC_CUSTOM_HEADERS"
printf 'model=%s\n' "$ANTHROPIC_MODEL"
printf 'arg=%s\n' "$@"
`
	if err := os.WriteFile(filepath.Join(bin, "claude"), []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	var out bytes.Buffer
	runner := srRunner{program: "sr", store: store, in: strings.NewReader(""), out: &out, errOut: io.Discard}
	if err := runner.run(t.Context(), []string{"claude-david", "--dangerously-skip-permissions", "-p", "fix this bug"}); err != nil {
		t.Fatal(err)
	}
	expected := "account=X-Subrouter-Bedrock-Account: david\nmodel=us.anthropic.claude-fable-5-1\narg=--dangerously-skip-permissions\narg=-p\narg=fix this bug\n"
	if out.String() != expected {
		t.Fatalf("child received %q, want %q", out.String(), expected)
	}
}
