package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/agents/claude"
)

func TestUploadServerClaudeProfileUsesProtectedHTTPOnly(t *testing.T) {
	var preflightRequests, importRequests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/_subrouter/account-import" {
			t.Fatalf("path = %q, want account import endpoint", req.URL.Path)
		}
		if got := req.Header.Get("Authorization"); got != "Bearer import-secret" {
			t.Fatalf("Authorization = %q, want protected import credential", got)
		}
		switch req.Method {
		case http.MethodGet:
			preflightRequests++
		case http.MethodPost:
			importRequests++
			var payload struct {
				Provider string `json:"provider"`
				Claude   *struct {
					Name       string                `json:"name"`
					Credential claude.CredentialInfo `json:"credential"`
				} `json:"claude"`
			}
			if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
				t.Fatal(err)
			}
			if payload.Provider != "claude" || payload.Claude == nil || payload.Claude.Name != "founders@manaflow.ai" {
				t.Fatalf("unexpected Claude import payload: provider=%q claude=%+v", payload.Provider, payload.Claude)
			}
			if payload.Claude.Credential.RefreshToken != "claude-refresh" {
				t.Fatal("server did not receive the fresh Claude refresh-token chain")
			}
		default:
			t.Fatalf("method = %s, want GET preflight or POST import", req.Method)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer server.Close()

	var out bytes.Buffer
	fake := &recordingSRCommandRunner{}
	runner := srRunner{out: &out, errOut: &out, cmd: fake, client: server.Client()}
	err := runner.uploadServerClaudeProfile(
		context.Background(),
		srServerConfig{Name: "gcp-team", URL: server.URL, AdminToken: "import-secret"},
		claude.Store{Dir: t.TempDir()},
		claude.Profile{Name: "founders@manaflow.ai", CreatedAt: time.Now().UTC().Format(time.RFC3339)},
		claude.CredentialInfo{AccessToken: "claude-access", RefreshToken: "claude-refresh", ExpiresAt: time.Now().Add(time.Hour).UnixMilli()},
	)
	if err != nil {
		t.Fatal(err)
	}
	if preflightRequests != 1 || importRequests != 1 {
		t.Fatalf("account import requests = preflight:%d post:%d, want 1 each", preflightRequests, importRequests)
	}
	for _, forbidden := range []string{"ssh", "scp", "gcloud"} {
		if fake.hasCommandPrefix(forbidden) {
			t.Fatalf("Claude upload must never execute %s: %#v", forbidden, fake.commands)
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
