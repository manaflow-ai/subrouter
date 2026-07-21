package main

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/manaflow-ai/subrouter/internal/accounts"
)

func TestConfigureDefaultLoggerWritesCLIToStateLog(t *testing.T) {
	previous := slog.Default()
	defer slog.SetDefault(previous)

	stateDir := t.TempDir()
	t.Setenv("SUBROUTER_STATE_DIR", stateDir)

	configureDefaultLogger("sr", []string{"status"})
	slog.Info("cli log test", "account", "test@example.com")

	data, err := os.ReadFile(filepath.Join(stateDir, "logs", "subrouter-cli.log"))
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)
	if !strings.Contains(got, "cli log test") || !strings.Contains(got, "test@example.com") {
		t.Fatalf("cli log file missing log record:\n%s", got)
	}
}

func TestConfigureDefaultLoggerLeavesServeLoggerAlone(t *testing.T) {
	previous := slog.Default()
	defer slog.SetDefault(previous)

	sentinel := slog.New(slog.NewTextHandler(io.Discard, nil))
	slog.SetDefault(sentinel)

	configureDefaultLogger("subrouter", []string{"serve"})
	if slog.Default() != sentinel {
		t.Fatal("serve should keep the process logger")
	}
}

func TestConfigureDefaultLoggerLeavesSupervisorLoggerAlone(t *testing.T) {
	previous := slog.Default()
	defer slog.SetDefault(previous)

	sentinel := slog.New(slog.NewTextHandler(io.Discard, nil))
	slog.SetDefault(sentinel)

	configureDefaultLogger("subrouter", []string{"supervise"})
	if slog.Default() != sentinel {
		t.Fatal("supervise should keep the process logger")
	}
}

func TestSystemdListenFDsParsesCurrentProcess(t *testing.T) {
	env := map[string]string{
		"LISTEN_PID": "123",
		"LISTEN_FDS": "1",
	}
	pid, fdCount, ok, err := systemdListenFDs(123, func(key string) string {
		return env[key]
	})
	if err != nil {
		t.Fatal(err)
	}
	if !ok || pid != 123 || fdCount != 1 {
		t.Fatalf("pid=%d fdCount=%d ok=%v, want pid 123 fdCount 1 ok true", pid, fdCount, ok)
	}
}

func TestSystemdListenFDsIgnoresDifferentProcess(t *testing.T) {
	env := map[string]string{
		"LISTEN_PID": "456",
		"LISTEN_FDS": "2",
	}
	pid, fdCount, ok, err := systemdListenFDs(123, func(key string) string {
		return env[key]
	})
	if err != nil {
		t.Fatal(err)
	}
	if !ok || pid != 456 || fdCount != 2 {
		t.Fatalf("pid=%d fdCount=%d ok=%v, want pid 456 fdCount 2 ok true", pid, fdCount, ok)
	}
}

func TestSystemdListenFDsRejectsInvalidEnv(t *testing.T) {
	env := map[string]string{
		"LISTEN_PID": "123",
		"LISTEN_FDS": "wat",
	}
	if _, _, _, err := systemdListenFDs(123, func(key string) string {
		return env[key]
	}); err == nil {
		t.Fatal("expected invalid LISTEN_FDS error")
	}
}

func TestParseByteSize(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  int64
	}{
		{"0", 0},
		{"512", 512},
		{"1KiB", 1024},
		{"1.5MiB", 1572864},
		{"2G", 2147483648},
		{"3GB", 3000000000},
	} {
		got, err := parseByteSize(tc.value)
		if err != nil {
			t.Fatalf("parseByteSize(%q): %v", tc.value, err)
		}
		if got != tc.want {
			t.Fatalf("parseByteSize(%q) = %d, want %d", tc.value, got, tc.want)
		}
	}
	if _, err := parseByteSize("-1"); err == nil {
		t.Fatal("negative byte size should fail")
	}
}

func TestRunAcceptsDirectSRCommands(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	if err := run([]string{"list"}); err != nil {
		t.Fatal(err)
	}
}

func TestSRKeepsSubrouterCommands(t *testing.T) {
	if err := runForProgram("sr", []string{"--help"}); err != nil {
		t.Fatal(err)
	}
}

func TestSRDefaultRunsAccountPicker(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, ".local", "share"))
	t.Setenv("PI_CODING_AGENT_DIR", filepath.Join(home, ".pi", "agent"))

	store := accounts.DefaultCodexStore()
	if err := store.SaveStored(accounts.StoredCodexAccount{
		Email:   "apikey:paid",
		AddedAt: "2026-05-04T00:00:00Z",
		Auth: accounts.CodexAuthFile{
			AuthMode:     "apikey",
			OpenAIAPIKey: "sk-test",
		},
	}); err != nil {
		t.Fatal(err)
	}

	if err := runForProgram("sr", nil); err != nil {
		t.Fatal(err)
	}
}

func TestDirectSRCommandNames(t *testing.T) {
	expected := []string{
		"add",
		"add-admin-key",
		"add-api-key",
		"add-key",
		"admin-keys",
		"attach-project",
		"breadcrumbs",
		"claude",
		"claude-aws",
		"claude-codex",
		"claude-codex-hook",
		"claude-direct",
		"cost",
		"g",
		"gemini",
		"gui",
		"gui-switch",
		"gui-use",
		"import",
		"list",
		"list-admin-keys",
		"login",
		"ls",
		"pick",
		"remove",
		"remove-admin-key",
		"reset",
		"rm",
		"server",
		"servers",
		"spend",
		"status",
		"switch",
		"trace",
		"usage",
		"use",
		"why",
	}
	sort.Strings(expected)
	actual := make([]string, 0, len(directSRCommands))
	for command := range directSRCommands {
		actual = append(actual, command)
	}
	sort.Strings(actual)
	if strings.Join(actual, "\n") != strings.Join(expected, "\n") {
		t.Fatalf("direct sr commands mismatch:\nactual:\n%s\nexpected:\n%s", strings.Join(actual, "\n"), strings.Join(expected, "\n"))
	}
	for _, command := range expected {
		if !isDirectSRCommand(command) {
			t.Fatalf("%s should be a direct sr command", command)
		}
	}
	for _, command := range []string{"serve", "codex", "install-daemon", "install-systemd"} {
		if isDirectSRCommand(command) {
			t.Fatalf("%s should stay a subrouter command", command)
		}
	}
}

func TestUsageShowsAccountCommandsAtTopLevel(t *testing.T) {
	got := usageText("sr")
	for _, want := range []string{
		"sr add",
		"sr add-key",
		"sr switch [email]",
		"sr g [email]",
		"sr gui [email]",
		"sr pick",
		"sr server",
		"sr add-admin-key",
		"sr claude",
		"sr serve",
		"sr install-systemd",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("usage missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "sr cx [cx args...]") {
		t.Fatalf("usage should not present cx as the primary nested command:\n%s", got)
	}
}
