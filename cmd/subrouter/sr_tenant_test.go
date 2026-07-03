package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/internal/tenant"
)

const testTenantKey = "srt_0123456789abcdef0123456789abcdef"

func TestSRServerAddStoresTenantKey(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	store := accounts.DefaultCodexStore()

	var out bytes.Buffer
	runner := srRunner{store: store, out: &out, errOut: &out}
	err := runner.run(context.Background(), []string{
		"server", "add", "hosted",
		"--url", "http://100.64.0.1:31415",
		"--tenant-key", testTenantKey,
	})
	if err != nil {
		t.Fatal(err)
	}
	server, ok, err := defaultSRServerStore(store).find("hosted")
	if err != nil || !ok {
		t.Fatalf("server missing: %v", err)
	}
	if server.TenantKey != testTenantKey {
		t.Fatalf("tenant key = %q", server.TenantKey)
	}

	// Updating other metadata without --tenant-key preserves the stored key.
	err = runner.run(context.Background(), []string{
		"server", "add", "hosted",
		"--url", "http://100.64.0.1:31415",
	})
	if err != nil {
		t.Fatal(err)
	}
	server, _, _ = defaultSRServerStore(store).find("hosted")
	if server.TenantKey != testTenantKey {
		t.Fatalf("tenant key not preserved: %q", server.TenantKey)
	}
}

func TestSRServerAddRejectsMalformedTenantKey(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	var out bytes.Buffer
	runner := srRunner{store: accounts.DefaultCodexStore(), out: &out, errOut: &out}
	err := runner.run(context.Background(), []string{
		"server", "add", "hosted",
		"--url", "http://100.64.0.1:31415",
		"--tenant-key", "srt_nothex",
	})
	if err == nil || !strings.Contains(err.Error(), "srt_") {
		t.Fatalf("err = %v", err)
	}
}

func TestTenantScopedBaseURLs(t *testing.T) {
	server := srServerConfig{Name: "hosted", URL: "http://100.64.0.1:31415", TenantKey: testTenantKey}
	if got := codexBaseURLForServer(server); got != "http://100.64.0.1:31415/t/"+testTenantKey+"/v1" {
		t.Fatalf("codex base URL = %q", got)
	}
	if got := serverControlBaseURL(server); got != "http://100.64.0.1:31415/t/"+testTenantKey {
		t.Fatalf("control base URL = %q", got)
	}
	// A stored URL that already ends in /v1 keeps the tenant prefix at the root.
	server.URL = "http://100.64.0.1:31415/v1"
	if got := codexBaseURLForServer(server); got != "http://100.64.0.1:31415/t/"+testTenantKey+"/v1" {
		t.Fatalf("codex base URL with /v1 suffix = %q", got)
	}
	if got := serverControlBaseURL(server); got != "http://100.64.0.1:31415/t/"+testTenantKey {
		t.Fatalf("control base URL with /v1 suffix = %q", got)
	}
	server.URL = "http://100.64.0.1:31415/backend-api"
	if got := serverControlBaseURL(server); got != "http://100.64.0.1:31415/t/"+testTenantKey {
		t.Fatalf("control base URL with /backend-api suffix = %q", got)
	}
	// Without a tenant key nothing changes.
	legacy := srServerConfig{Name: "team", URL: "http://100.64.0.1:31415"}
	if got := codexBaseURLForServer(legacy); got != "http://100.64.0.1:31415/v1" {
		t.Fatalf("legacy codex base URL = %q", got)
	}
	if got := serverControlBaseURL(legacy); got != "http://100.64.0.1:31415" {
		t.Fatalf("legacy control base URL = %q", got)
	}
}

func TestWriteClaudeProxyEnvTenantKeySetsAuthToken(t *testing.T) {
	dir := t.TempDir()
	if err := writeClaudeProxyEnv(dir, "http://host:31415/t/"+testTenantKey, testTenantKey); err != nil {
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
	if env["ANTHROPIC_BASE_URL"] != "http://host:31415/t/"+testTenantKey {
		t.Fatalf("base url = %v", env["ANTHROPIC_BASE_URL"])
	}
	if env["ANTHROPIC_AUTH_TOKEN"] != testTenantKey {
		t.Fatalf("auth token = %v", env["ANTHROPIC_AUTH_TOKEN"])
	}
}

func TestSRTenantLocalCreateListRevoke(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	stateDir := filepath.Join(home, ".subrouter")
	t.Setenv("SUBROUTER_STATE_DIR", stateDir)
	store := accounts.CodexStore{Dir: filepath.Join(stateDir, "codex", "accounts")}

	var out bytes.Buffer
	runner := srRunner{store: store, out: &out, errOut: &out}
	if err := runner.run(context.Background(), []string{"tenant", "create", "acme"}); err != nil {
		t.Fatal(err)
	}
	created := out.String()
	if !strings.Contains(created, "Created tenant acme") || !strings.Contains(created, "srt_") {
		t.Fatalf("create output = %q", created)
	}

	registry := tenant.NewRegistry(stateDir)
	tenants, err := registry.List()
	if err != nil || len(tenants) != 1 {
		t.Fatalf("tenants = %+v, err %v", tenants, err)
	}

	out.Reset()
	if err := runner.run(context.Background(), []string{"tenant", "list"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "acme") {
		t.Fatalf("list output = %q", out.String())
	}

	out.Reset()
	if err := runner.run(context.Background(), []string{"tenant", "key", "revoke", "acme", tenants[0].Keys[0].Prefix}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Revoked 1 key(s)") {
		t.Fatalf("revoke output = %q", out.String())
	}
}
