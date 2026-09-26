package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestUpdateTopLevelTomlStringsPreservesTables(t *testing.T) {
	input := strings.Join([]string{
		`model = "gpt-5"`,
		`openai_base_url = "http://old.example/v1"`,
		``,
		`[profiles.work]`,
		`openai_base_url = "http://profile.example/v1"`,
		``,
	}, "\n")
	got := updateTopLevelTomlStrings(input, map[string]string{
		"openai_base_url":                   "http://team.example/v1",
		"chatgpt_base_url":                  "http://team.example/backend-api",
		"experimental_realtime_ws_base_url": "http://team.example/v1",
	})

	for _, want := range []string{
		`openai_base_url = "http://team.example/v1"`,
		`chatgpt_base_url = "http://team.example/backend-api"`,
		`experimental_realtime_ws_base_url = "http://team.example/v1"`,
		`[profiles.work]`,
		`openai_base_url = "http://profile.example/v1"`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in:\n%s", want, got)
		}
	}
	if strings.Index(got, `experimental_realtime_ws_base_url`) > strings.Index(got, `[profiles.work]`) {
		t.Fatalf("routing keys were inserted inside a table:\n%s", got)
	}
}

func TestUpdateTopLevelTomlStringsSeparatesMissingKeysAfterUnterminatedLine(t *testing.T) {
	got := updateTopLevelTomlStrings(`model = "gpt-5"`, map[string]string{
		"openai_base_url": "http://team.example/v1",
	})
	want := "model = \"gpt-5\"\nopenai_base_url = \"http://team.example/v1\"\n"
	if got != want {
		t.Fatalf("config = %q, want %q", got, want)
	}
}

func TestWriteCodexConfigValuesCreatesBackup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("model = \"gpt-5\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeCodexConfigValues(path, map[string]string{
		"openai_base_url": "http://team.example/v1",
	}); err != nil {
		t.Fatal(err)
	}
	backup, err := os.ReadFile(path + ".bak")
	if err != nil {
		t.Fatal(err)
	}
	if string(backup) != "model = \"gpt-5\"\n" {
		t.Fatalf("backup = %q", string(backup))
	}
}

func TestWriteCodexConfigForRemotePlaintextKeepsDurableConfigLocal(t *testing.T) {
	t.Setenv("CODEX_HOME", t.TempDir())
	path, err := writeCodexConfigForServer(srServerConfig{
		Name: "m3", URL: "http://m3.example.ts.net.:31415", TenantKey: testTenantKey, TailscaleNodeID: "node-m3",
	})
	if err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	if !strings.Contains(text, defaultCodexBaseURL) || strings.Contains(text, "m3.example") || strings.Contains(text, testTenantKey) {
		t.Fatalf("durable Codex config retained a one-time remote route:\n%s", text)
	}
}
