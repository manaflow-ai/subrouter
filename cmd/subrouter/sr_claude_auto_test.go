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
	"strings"
	"testing"

	"github.com/manaflow-ai/subrouter/internal/accounts"
)

func testClaudeLaunchRunner(t *testing.T) srRunner {
	t.Helper()
	return srRunner{
		store: accounts.CodexStore{Dir: filepath.Join(t.TempDir(), "codex", "accounts")},
		out: io.Discard, errOut: io.Discard,
	}
}

func TestClaudePoolModeParsingAndImplicitLaunch(t *testing.T) {
	for _, tt := range []struct {
		raw string
		want claudePoolLaunchMode
	}{
		{"auto", claudePoolLaunchAuto},
		{" AUTO ", claudePoolLaunchAuto},
		{"choose", claudePoolLaunchChoose},
	} {
		got, err := parseClaudePoolLaunchMode(tt.raw)
		if err != nil || got != tt.want {
			t.Errorf("parseClaudePoolLaunchMode(%q) = %q, %v", tt.raw, got, err)
		}
	}
	for _, raw := range []string{"", "native", "api"} {
		if _, err := parseClaudePoolLaunchMode(raw); err == nil {
			t.Errorf("unexpectedly accepted mode %q", raw)
		}
	}
	for _, tt := range []struct {
		args []string
		implicit bool
	}{
		{nil, true},
		{[]string{"-p", "hello"}, true},
		{[]string{"--resume", "abc"}, true},
		{[]string{"--output-format", "stream-json"}, true},
		{[]string{"proxy"}, false},
		{[]string{"choose"}, false},
		{[]string{"mode", "auto"}, false},
		{[]string{"run", "personal"}, false},
		{[]string{"--help"}, false},
		{[]string{"-h"}, false},
	} {
		if got := claudeImplicitPoolLaunch(tt.args); got != tt.implicit {
			t.Errorf("claudeImplicitPoolLaunch(%q) = %t, want %t", tt.args, got, tt.implicit)
		}
	}
}

func TestClaudePoolModePersistenceAndEnvironmentOverride(t *testing.T) {
	t.Setenv(claudePoolLaunchModeEnv, "")
	r := testClaudeLaunchRunner(t)
	if got, err := r.claudePoolLaunchMode(); err != nil || got != claudePoolLaunchChoose {
		t.Fatalf("default mode = %q, %v", got, err)
	}
	if err := r.saveClaudePoolLaunchMode(claudePoolLaunchAuto); err != nil {
		t.Fatal(err)
	}
	if got, err := r.claudePoolLaunchMode(); err != nil || got != claudePoolLaunchAuto {
		t.Fatalf("saved mode = %q, %v", got, err)
	}
	info, err := os.Stat(r.claudePoolLaunchPreferencePath())
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("preference mode = %o; want 600", info.Mode().Perm())
	}
	t.Setenv(claudePoolLaunchModeEnv, "choose")
	if got, err := r.claudePoolLaunchMode(); err != nil || got != claudePoolLaunchChoose {
		t.Fatalf("environment mode = %q, %v", got, err)
	}
	t.Setenv(claudePoolLaunchModeEnv, "bogus")
	if _, err := r.claudePoolLaunchMode(); err == nil || !strings.Contains(err.Error(), claudePoolLaunchModeEnv) {
		t.Fatalf("invalid environment error = %v", err)
	}
}

func TestSRClaudeAutoStartsPooledWithoutInteractivePicker(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv(claudePoolLaunchModeEnv, "auto")
	// An empty input stream proves the auto path never asks for an account.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		_, _ = w.Write([]byte(`[]`))
	}))
	defer server.Close()
	store := accounts.CodexStore{Dir: filepath.Join(home, "state", "codex", "accounts")}
	if err := defaultSRServerStore(store).save(srServerFile{Default: "team", Servers: []srServerConfig{{Name: "team", URL: server.URL}}}); err != nil {
		t.Fatal(err)
	}
	binDir := filepath.Join(home, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	settingsPath := filepath.Join(home, "settings.json")
	script := "#!/bin/sh\ncp \"$2\" " + shellQuote(settingsPath) + "\n"
	if err := os.WriteFile(filepath.Join(binDir, "claude"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	var out bytes.Buffer
	r := srRunner{store: store, in: strings.NewReader(""), out: &out, errOut: &out, client: http.DefaultClient}
	if err := r.claude(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "Choose") || strings.Contains(out.String(), "POOLED Claude process") {
		t.Fatalf("auto launch invoked interactive account picker: %q", out.String())
	}
	body, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatal(err)
	}
	var overlay struct { Env map[string]string `json:"env"` }
	if err := json.Unmarshal(body, &overlay); err != nil {
		t.Fatal(err)
	}
	if got := overlay.Env["ANTHROPIC_CUSTOM_HEADERS"]; got != "X-Subrouter-Agent: claude" {
		t.Fatalf("auto launch must leave account unpinned, got headers %q", got)
	}
	if overlay.Env["ANTHROPIC_BASE_URL"] == "" {
		t.Fatal("auto launch did not configure Subrouter pooled proxy")
	}
	// Non-interactive client integrations use the same pooled path.
	if err := r.claude(context.Background(), []string{"-p", "hello", "--output-format", "stream-json"}); err != nil {
		t.Fatal(err)
	}
}
