package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/internal/broker"
)

func saveReadyCloudConfig(t *testing.T) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cloud.json")
	t.Setenv("SUBROUTER_CLOUD_CONFIG", path)
	if err := broker.SaveConfig(path, broker.Config{
		BaseURL:      "https://cmux.test",
		AccessToken:  "stack-access",
		RefreshToken: "stack-refresh",
		TeamID:       "team-a",
		TeamName:     "Team A",
	}); err != nil {
		t.Fatal(err)
	}
}

func TestCloudCodexAlwaysUsesLocalProxy(t *testing.T) {
	saveReadyCloudConfig(t)
	local := healthServer(t, 200)
	t.Setenv("SUBROUTER_LOCAL_BASE_URL", local.URL+"/v1")

	store := srServerStore{Path: filepath.Join(t.TempDir(), "servers.json")}
	if err := store.save(srServerFile{
		Default: "remote",
		Servers: []srServerConfig{{
			Name: "remote",
			URL:  "http://remote.example:31415",
		}},
	}); err != nil {
		t.Fatal(err)
	}
	var warnings bytes.Buffer
	got, err := codexBaseURLWithFallback(store, &warnings)
	if err != nil {
		t.Fatal(err)
	}
	if got != local.URL+"/v1" {
		t.Fatalf("base URL = %q, want local proxy", got)
	}
	if strings.Contains(warnings.String(), "remote.example") {
		t.Fatalf("cloud mode should not probe or fall back through the legacy remote: %q", warnings.String())
	}
}

func TestBareSRUsesSelectedTeamInsteadOfLegacyRemote(t *testing.T) {
	cloud := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/subrouter/accounts" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"teamId":"team-a",
			"accounts":[{
				"id":"codex-cloud",
				"kind":"codex",
				"label":"shared@example.com",
				"health":{"ok":true}
			}]
		}`))
	}))
	defer cloud.Close()

	path := filepath.Join(t.TempDir(), "cloud.json")
	t.Setenv("SUBROUTER_CLOUD_CONFIG", path)
	if err := broker.SaveConfig(path, broker.Config{
		BaseURL:      cloud.URL,
		AccessToken:  "stack-access",
		RefreshToken: "stack-refresh",
		TeamID:       "team-a",
		TeamName:     "Team A",
	}); err != nil {
		t.Fatal(err)
	}

	remoteRequests := atomic.Int32{}
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		remoteRequests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/_subrouter/usage-status":
			_, _ = w.Write([]byte(`[]`))
		case "/_subrouter/bedrock-cost":
			_, _ = w.Write([]byte(`{"requests":0,"throttled":0}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer remote.Close()

	store := accounts.CodexStore{Dir: t.TempDir()}
	if err := defaultSRServerStore(store).save(srServerFile{
		Default: "team",
		Servers: []srServerConfig{{Name: "team", URL: remote.URL}},
	}); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	runner := srRunner{
		program: "sr",
		store:   store,
		out:     &out,
		errOut:  &out,
		client:  remote.Client(),
	}
	if err := runner.run(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if !strings.Contains(got, "Credential storage: team") ||
		!strings.Contains(got, "Team A") ||
		!strings.Contains(got, "shared@example.com") {
		t.Fatalf("bare sr did not show the selected team vault:\n%s", got)
	}
	if strings.Contains(got, "Server: team") || remoteRequests.Load() != 0 {
		t.Fatalf("bare sr contacted the stale legacy server (%d requests):\n%s", remoteRequests.Load(), got)
	}
}

func TestStorageLocalUsesLocalAccountsInsteadOfCloudOrLegacyRemote(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	path := filepath.Join(t.TempDir(), "cloud.json")
	t.Setenv("SUBROUTER_CLOUD_CONFIG", path)
	if err := broker.SaveConfig(path, broker.Config{
		BaseURL:      "https://cmux.test",
		AccessToken:  "stack-access",
		RefreshToken: "stack-refresh",
		TeamID:       "team-a",
		TeamName:     "Team A",
	}); err != nil {
		t.Fatal(err)
	}

	remoteRequests := atomic.Int32{}
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		remoteRequests.Add(1)
		http.Error(w, "legacy remote must not be called", http.StatusInternalServerError)
	}))
	defer remote.Close()

	store := accounts.CodexStore{Dir: t.TempDir()}
	if err := store.SaveStored(accounts.StoredCodexAccount{
		Email:   "local@example.com",
		AddedAt: time.Now().UTC().Format(time.RFC3339),
		Auth:    testCodexAuth("local@example.com", "acct_local"),
	}); err != nil {
		t.Fatal(err)
	}
	if err := defaultSRServerStore(store).save(srServerFile{
		Default: "team",
		Servers: []srServerConfig{{Name: "team", URL: remote.URL}},
	}); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	runner := srRunner{
		program: "sr",
		store:   store,
		out:     &out,
		errOut:  &out,
		client:  remote.Client(),
	}
	if err := runner.run(context.Background(), []string{"storage", "local"}); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := runner.run(context.Background(), []string{"list"}); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if !strings.Contains(got, "local@example.com") {
		t.Fatalf("local storage did not list the local account:\n%s", got)
	}
	if strings.Contains(got, "Server: team") || remoteRequests.Load() != 0 {
		t.Fatalf("local storage contacted the legacy server (%d requests):\n%s", remoteRequests.Load(), got)
	}
}

func TestCloudClaudeEnvironmentRoutesLocallyWithoutProviderSecrets(t *testing.T) {
	env := cloudClaudeEnvironment([]string{
		"PATH=/usr/bin",
		"ANTHROPIC_BASE_URL=https://remote.example",
		"ANTHROPIC_AUTH_TOKEN=old-token",
		"ANTHROPIC_API_KEY=sk-ant-secret",
		"CLAUDE_CODE_USE_BEDROCK=1",
	}, "http://127.0.0.1:31415/v1", "stack-local-token")
	joined := strings.Join(env, "\n")
	for _, banned := range []string{
		"https://remote.example",
		"old-token",
		"sk-ant-secret",
		"CLAUDE_CODE_USE_BEDROCK",
	} {
		if strings.Contains(joined, banned) {
			t.Fatalf("cloud Claude env retained %q:\n%s", banned, joined)
		}
	}
	for _, want := range []string{
		"ANTHROPIC_BASE_URL=http://127.0.0.1:31415",
		"ANTHROPIC_AUTH_TOKEN=stack-local-token",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("cloud Claude env missing %q:\n%s", want, joined)
		}
	}
}

func TestCloudCodexUsesAuthenticatedLocalProvider(t *testing.T) {
	args := codexArgsWithLocalProxyToken(
		[]string{"exec", "hello"},
		"http://127.0.0.1:31415/v1",
		"",
		"",
		"stack-local-token",
	)
	joined := strings.Join(args, "\n")
	for _, want := range []string{
		`model_provider="subrouter"`,
		`model_providers.subrouter.env_key="SUBROUTER_CODEX_DUMMY_API_KEY"`,
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("cloud Codex args missing %q:\n%s", want, joined)
		}
	}
}

func TestUserSystemdUnitIsLoopbackAndCloudConfigured(t *testing.T) {
	unit, err := renderUserSystemdUnit(
		"/home/alice",
		"/home/alice/.local/bin/subrouter",
	)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"--addr 127.0.0.1:31415",
		"--cloud-config /home/alice/.config/subrouter/cloud.json",
		"ProtectHome=read-only",
		"UMask=0077",
	} {
		if !strings.Contains(unit, want) {
			t.Fatalf("unit missing %q:\n%s", want, unit)
		}
	}
	if strings.Contains(unit, "0.0.0.0") {
		t.Fatalf("user daemon must not bind publicly:\n%s", unit)
	}
}

func TestCloudModeConfigMissingIsLegacyMode(t *testing.T) {
	t.Setenv("SUBROUTER_CLOUD_CONFIG", filepath.Join(t.TempDir(), "missing.json"))
	config, err := cloudModeConfig()
	if err != nil {
		t.Fatal(err)
	}
	if config.Ready() {
		t.Fatalf("missing config unexpectedly enabled cloud mode: %+v", config)
	}
}

func TestSelectLocalAccountUploadRequiresOneExactCanary(t *testing.T) {
	uploads := []localAccountUpload{
		{kind: "codex", label: "alice@example.com"},
		{kind: "claude", label: "alice@example.com"},
		{kind: "codex", label: "bob@example.com"},
	}

	selected, err := selectLocalAccountUpload(uploads, "codex:alice@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if len(selected) != 1 ||
		selected[0].kind != "codex" ||
		selected[0].label != "alice@example.com" {
		t.Fatalf("selected = %+v", selected)
	}

	if _, err := selectLocalAccountUpload(uploads, "alice@example.com"); err == nil {
		t.Fatal("same-label accounts across providers must be ambiguous")
	}
	if _, err := selectLocalAccountUpload(uploads, "missing"); err == nil {
		t.Fatal("missing selector unexpectedly matched an account")
	}
}

func TestBulkAccountImportRequiresExplicitConfirmation(t *testing.T) {
	var uploads atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/subrouter/accounts":
			_, _ = w.Write([]byte(`{"accounts":[]}`))
		case r.Method == http.MethodPost && r.URL.Path == "/api/subrouter/accounts":
			uploads.Add(1)
			_, _ = w.Write([]byte(
				`{"account":{"id":"acct-1","kind":"openai-apikey","label":"paid"}}`,
			))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	store := accounts.CodexStore{Dir: t.TempDir()}
	if _, _, err := store.AddAPIKey("paid", "sk-test-paid"); err != nil {
		t.Fatal(err)
	}
	client := broker.NewClient(broker.Config{
		BaseURL:      server.URL,
		AccessToken:  "stack-access",
		RefreshToken: "stack-refresh",
		TeamID:       "team-a",
	})
	var out bytes.Buffer
	runner := srRunner{store: store, out: &out, errOut: &out}

	err := runner.cloudAccountImport(
		context.Background(),
		client,
		[]string{"--all"},
	)
	if err == nil || !strings.Contains(err.Error(), "--yes") {
		t.Fatalf("bulk import error = %v, want --yes requirement", err)
	}
	if uploads.Load() != 0 {
		t.Fatalf("bulk import uploaded %d accounts without confirmation", uploads.Load())
	}
}
