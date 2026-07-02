package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBuildClaudeCredentialArchive(t *testing.T) {
	payload := []byte(`{"claudeAiOauth":{"accessToken":"tok"}}`)
	archive, err := buildClaudeCredentialArchive("", "_p123", payload)
	if err != nil {
		t.Fatal(err)
	}
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	header, err := tr.Next()
	if err != nil {
		t.Fatal(err)
	}
	if header.Name != filepath.Join("codex", "claude", "_p123", ".credentials.json") {
		t.Fatalf("archive path = %q", header.Name)
	}
	if header.Mode != 0o600 {
		t.Fatalf("mode = %o, want 600", header.Mode)
	}
	body, err := io.ReadAll(tr)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(body, payload) {
		t.Fatalf("archive body = %q", body)
	}
}

func TestClaudeUploadRemoteCommand(t *testing.T) {
	command := claudeUploadRemoteCommand(srServerConfig{Name: "team", URL: "http://subrouter-team:31415"}, "", "lc9@example.com", "_p999")
	for _, want := range []string{
		"sudo tar -C /var/lib/subrouter",
		"/var/lib/subrouter/codex/claude.json",
		"'lc9@example.com'",
		"'_p999'",
		"reload-accounts",
		"command -v jq",
		"chown -R subrouter:subrouter",
	} {
		if !strings.Contains(command, want) {
			t.Fatalf("remote command missing %q:\n%s", want, command)
		}
	}
}

func TestWriteClaudeProxyEnvMergesSettings(t *testing.T) {
	dir := t.TempDir()
	settingsPath := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(settingsPath, []byte(`{"theme":"dark","env":{"FOO":"bar"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeClaudeProxyEnv(dir, "http://subrouter-team:31415/", ""); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatal(err)
	}
	var settings map[string]any
	if err := json.Unmarshal(body, &settings); err != nil {
		t.Fatal(err)
	}
	if settings["theme"] != "dark" {
		t.Fatalf("theme clobbered: %v", settings["theme"])
	}
	env := settings["env"].(map[string]any)
	if env["FOO"] != "bar" {
		t.Fatalf("unrelated env clobbered: %v", env["FOO"])
	}
	if env["ANTHROPIC_BASE_URL"] != "http://subrouter-team:31415" {
		t.Fatalf("base url = %v (trailing slash should be trimmed)", env["ANTHROPIC_BASE_URL"])
	}
	if env["ANTHROPIC_AUTH_TOKEN"] != "subrouter" {
		t.Fatalf("auth token = %v", env["ANTHROPIC_AUTH_TOKEN"])
	}

	// Second write is idempotent and keeps a custom token if one exists.
	env["ANTHROPIC_AUTH_TOKEN"] = "custom"
	body, _ = json.Marshal(settings)
	if err := os.WriteFile(settingsPath, body, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeClaudeProxyEnv(dir, "http://subrouter-team:31415", ""); err != nil {
		t.Fatal(err)
	}
	body, _ = os.ReadFile(settingsPath)
	if err := json.Unmarshal(body, &settings); err != nil {
		t.Fatal(err)
	}
	if got := settings["env"].(map[string]any)["ANTHROPIC_AUTH_TOKEN"]; got != "custom" {
		t.Fatalf("custom token clobbered: %v", got)
	}
}

func TestWriteClaudeProxyEnvCreatesSettings(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "fresh")
	if err := writeClaudeProxyEnv(dir, "http://subrouter-team:31415", ""); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(filepath.Join(dir, "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	var settings map[string]any
	if err := json.Unmarshal(body, &settings); err != nil {
		t.Fatal(err)
	}
	env := settings["env"].(map[string]any)
	if env["ANTHROPIC_BASE_URL"] != "http://subrouter-team:31415" {
		t.Fatalf("base url = %v", env["ANTHROPIC_BASE_URL"])
	}
}
