package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	agentclaude "github.com/manaflow-ai/subrouter/internal/agents/claude"
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

func TestUsageRowsFromHostedStatusesPreservesQuotaWindows(t *testing.T) {
	rows := usageRowsFromHostedStatuses([]broker.UsageStatus{{
		ID:        "account-1",
		Provider:  accounts.ProviderCodex,
		AuthMode:  accounts.AuthModeOAuth,
		Email:     "user@example.com",
		AuthValid: true,
		Windows: []accounts.UsageWindow{{
			Name:        "weekly",
			UsedPercent: 25,
		}},
	}})
	if len(rows) != 1 || rows[0].email != "user@example.com" ||
		len(rows[0].windows) != 1 || rows[0].windows[0].UsedPercent != 25 {
		t.Fatalf("usage rows = %#v", rows)
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

func TestTeamServerKeepsDedicatedProxyAuthenticationForRemoteOverride(t *testing.T) {
	saveReadyCloudConfig(t)
	t.Setenv("SUBROUTER_CODEX_BASE_URL", "https://remote.example/v1")

	store := srServerStore{Path: filepath.Join(t.TempDir(), "servers.json")}
	if _, err := codexBaseURLWithFallback(store, nil); err == nil ||
		!strings.Contains(err.Error(), "team credentials") {
		t.Fatalf("remote team pin error = %v, want a local-only rejection", err)
	}
	config, err := cloudModeConfig()
	if err != nil {
		t.Fatal(err)
	}
	if token := cloudServerProxyToken(config); token != config.LocalProxyToken {
		t.Fatal("server proxy token did not use the dedicated local secret")
	}
}

func TestTeamProxyNeverSendsStackTokenToALocalURLOverride(t *testing.T) {
	config := broker.Config{
		BaseURL:         "https://cmux.test",
		AccessToken:     "stack-access",
		RefreshToken:    "stack-refresh",
		TeamID:          "team-a",
		LocalProxyToken: "dedicated-local-secret",
	}
	t.Setenv("SUBROUTER_LOCAL_BASE_URL", "https://attacker.example/v1")

	if token := cloudClientProxyToken(
		config,
		"https://attacker.example/v1",
	); token != "" {
		t.Fatal("remote override received a non-empty proxy token")
	}
}

func TestTeamProxyUsesDedicatedLocalSecret(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cloud.json")
	if err := broker.SaveConfig(path, broker.Config{
		BaseURL:      "https://cmux.test",
		AccessToken:  "stack-access",
		RefreshToken: "stack-refresh",
		TeamID:       "team-a",
	}); err != nil {
		t.Fatal(err)
	}
	config, err := broker.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	got := cloudServerProxyToken(config)
	if got == "" {
		t.Fatal("persisted config has no dedicated local proxy secret")
	}
	if got == config.AccessToken {
		t.Fatal("local proxy secret reused the Stack session token")
	}
}

func TestLocalCredentialModeUsesDedicatedProxyAuthentication(t *testing.T) {
	config := broker.Config{
		CredentialSource: broker.CredentialSourceLocal,
		LocalProxyToken:  "dedicated-local-secret",
	}
	if got := cloudServerProxyToken(config); got != "dedicated-local-secret" {
		t.Fatalf("server token = %q, want dedicated local secret", got)
	}
	if got := cloudClientProxyToken(config, localBaseURL()); got != "dedicated-local-secret" {
		t.Fatalf("client token = %q, want dedicated local secret", got)
	}
}

func TestTeamCodexIgnoresMalformedLegacyServerStore(t *testing.T) {
	saveReadyCloudConfig(t)
	local := healthServer(t, http.StatusOK)
	t.Setenv("SUBROUTER_LOCAL_BASE_URL", local.URL+"/v1")

	path := filepath.Join(t.TempDir(), "servers.json")
	if err := os.WriteFile(path, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := codexBaseURLWithFallback(srServerStore{Path: path}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got != local.URL+"/v1" {
		t.Fatalf("base URL = %q, want local proxy %q", got, local.URL+"/v1")
	}
}

func TestLocalCodexIgnoresMalformedLegacyServerStore(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "cloud.json")
	t.Setenv("SUBROUTER_CLOUD_CONFIG", configPath)
	if err := broker.SaveConfig(configPath, broker.Config{
		CredentialSource: broker.CredentialSourceLocal,
	}); err != nil {
		t.Fatal(err)
	}
	local := healthServer(t, http.StatusOK)
	t.Setenv("SUBROUTER_LOCAL_BASE_URL", local.URL+"/v1")

	path := filepath.Join(t.TempDir(), "servers.json")
	if err := os.WriteFile(path, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := codexBaseURLWithFallback(srServerStore{Path: path}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got != local.URL+"/v1" {
		t.Fatalf("base URL = %q, want local proxy %q", got, local.URL+"/v1")
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

func TestRecoveryCommandsSurviveMalformedCloudConfig(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	configPath := filepath.Join(t.TempDir(), "cloud.json")
	if err := os.WriteFile(configPath, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SUBROUTER_CLOUD_CONFIG", configPath)
	local := healthServer(t, http.StatusOK)
	t.Setenv("SUBROUTER_LOCAL_BASE_URL", local.URL+"/v1")

	store := accounts.CodexStore{Dir: t.TempDir()}
	var out bytes.Buffer
	runner := srRunner{
		program: "sr",
		store:   store,
		in:      strings.NewReader(""),
		out:     &out,
		errOut:  &out,
		client:  local.Client(),
	}
	if err := runner.run(context.Background(), []string{"help"}); err != nil {
		t.Fatalf("help: %v", err)
	}
	if !strings.Contains(out.String(), "sr login") {
		t.Fatalf("help output missing recovery command:\n%s", out.String())
	}
	out.Reset()
	if err := runner.run(context.Background(), []string{"cleanup"}); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	out.Reset()
	if err := runner.run(context.Background(), []string{"doctor"}); err == nil {
		t.Fatal("doctor unexpectedly passed a malformed config")
	}
	if !strings.Contains(out.String(), "credential storage") {
		t.Fatalf("doctor did not diagnose the malformed config:\n%s", out.String())
	}
	out.Reset()
	if err := runner.run(
		context.Background(),
		[]string{"storage", "local"},
	); err != nil {
		t.Fatalf("storage local recovery: %v", err)
	}
	recoveredConfig, err := broker.LoadConfig(configPath)
	if err != nil {
		t.Fatalf("load recovered config: %v", err)
	}
	if recoveredConfig.EffectiveCredentialSource() !=
		broker.CredentialSourceLocal {
		t.Fatal("storage local did not replace the malformed config")
	}

	if err := os.WriteFile(configPath, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	config, recovered, err := loadCloudConfigForLogin(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if !recovered || config.BaseURL != broker.DefaultBaseURL {
		t.Fatalf("login recovery = (%+v, %v), want a fresh default config", config, recovered)
	}
}

func TestTeamProxySkipsMalformedLocalCredentialStores(t *testing.T) {
	codexStore := accounts.CodexStore{Dir: t.TempDir()}
	accountsDir := filepath.Join(codexStore.StoreDir(), "accounts")
	if err := os.MkdirAll(accountsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(accountsDir, "broken.json"),
		[]byte("{"),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	claudeStore := agentclaude.Store{Dir: t.TempDir()}

	codexAccounts, claudeAccounts, err := loadProxyAccounts(
		context.Background(),
		true,
		codexStore,
		claudeStore,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(codexAccounts) != 0 || len(claudeAccounts) != 0 {
		t.Fatalf(
			"team proxy loaded local accounts: codex=%d claude=%d",
			len(codexAccounts),
			len(claudeAccounts),
		)
	}
}

func TestDirectCloudAPIKeyCanBeRepairedWithoutLocalCredential(t *testing.T) {
	target := broker.SharedAccount{
		ID:    "anthropic-key-a",
		Kind:  "anthropic-apikey",
		Label: "shared production key",
	}
	var prompts bytes.Buffer
	upload, err := replacementUploadForSharedAccount(
		&target,
		nil,
		strings.NewReader("sk-ant-replacement\n"),
		&prompts,
	)
	if err != nil {
		t.Fatal(err)
	}
	if got := upload["provider"]; got != "anthropic-apikey" {
		t.Fatalf("provider = %v", got)
	}
	if got := upload["label"]; got != target.Label {
		t.Fatalf("label = %v, want %q", got, target.Label)
	}
	if got := upload["apiKey"]; got != "sk-ant-replacement" {
		t.Fatalf("api key = %v", got)
	}
	if strings.Contains(prompts.String(), "sk-ant-replacement") {
		t.Fatal("replacement credential was echoed to output")
	}
}

func TestDirectOpenAIAPIKeyRepairRejectsAnthropicPrefix(t *testing.T) {
	target := broker.SharedAccount{
		ID:    "openai-key-a",
		Kind:  "openai-apikey",
		Label: "shared production key",
	}
	_, err := replacementUploadForSharedAccount(
		&target,
		nil,
		strings.NewReader("sk-ant-wrong-provider\n"),
		io.Discard,
	)
	if err == nil || !strings.Contains(err.Error(), "Anthropic") {
		t.Fatalf("OpenAI repair error = %v, want Anthropic-prefix rejection", err)
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
	t.Setenv(
		"SUBROUTER_CLOUD_CONFIG",
		"/home/alice/private/subrouter-team.json",
	)
	unit, err := renderUserSystemdUnit(
		"/home/alice",
		"/home/alice/.local/bin/subrouter",
	)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"--addr 127.0.0.1:31415",
		`--cloud-config "/home/alice/private/subrouter-team.json"`,
		`BindReadOnlyPaths="/home/alice/private/subrouter-team.json"`,
		"ProtectHome=read-only",
		`"-/home/alice/.codex-accounts"`,
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

func TestUserSystemdUnitQuotesPathsWithSystemdSpecialCharacters(t *testing.T) {
	home := `/home/Alice % "Ops"\station`
	configPath := home + `/private/team config.json`
	t.Setenv("SUBROUTER_CLOUD_CONFIG", configPath)

	unit, err := renderUserSystemdUnit(
		home,
		home+`/.local/bin/subrouter`,
	)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`Environment="HOME=/home/Alice %% \"Ops\"\\station"`,
		`ExecStart="/home/Alice %% \"Ops\"\\station/.local/bin/subrouter" serve`,
		`--cloud-config "/home/Alice %% \"Ops\"\\station/private/team config.json"`,
		`ReadWritePaths="/home/Alice %% \"Ops\"\\station/.subrouter"`,
		`BindReadOnlyPaths="/home/Alice %% \"Ops\"\\station/private/team config.json"`,
	} {
		if !strings.Contains(unit, want) {
			t.Fatalf("unit missing quoted path %q:\n%s", want, unit)
		}
	}
}

func TestLocalAccountUploadsPreserveSupportedAPIKeyProviders(t *testing.T) {
	stateDir := t.TempDir()
	t.Setenv("SUBROUTER_STATE_DIR", stateDir)
	store := accounts.CodexStore{Dir: filepath.Join(stateDir, "codex", "accounts")}
	for _, account := range []accounts.StoredCodexAccount{
		{
			Email:    "apikey:openai",
			Provider: accounts.ProviderCodex,
			Auth: accounts.CodexAuthFile{
				AuthMode:     "apikey",
				OpenAIAPIKey: "sk-openai",
			},
		},
		{
			Email:    "claude:anthropic",
			Provider: accounts.ProviderClaude,
			Auth: accounts.CodexAuthFile{
				AuthMode:     "apikey",
				OpenAIAPIKey: "sk-ant-anthropic",
			},
		},
		{
			Email:    "kimi:unsupported",
			Provider: accounts.ProviderKimi,
			Auth: accounts.CodexAuthFile{
				AuthMode:     "apikey",
				OpenAIAPIKey: "sk-kimi",
			},
		},
		{
			Email:    "zai:unsupported",
			Provider: accounts.ProviderZAI,
			Auth: accounts.CodexAuthFile{
				AuthMode:     "apikey",
				OpenAIAPIKey: "sk-zai",
			},
		},
	} {
		if err := store.SaveStored(account); err != nil {
			t.Fatal(err)
		}
	}

	uploads, err := localAccountUploads(context.Background(), store)
	if err != nil {
		t.Fatal(err)
	}
	if len(uploads) != 2 {
		t.Fatalf("upload count = %d, want 2 supported providers", len(uploads))
	}
	providers := map[string]string{}
	for _, upload := range uploads {
		providers[upload.label], _ = upload.body["provider"].(string)
	}
	if providers["apikey:openai"] != "openai-apikey" {
		t.Fatalf("OpenAI provider = %q", providers["apikey:openai"])
	}
	if providers["claude:anthropic"] != "anthropic-apikey" {
		t.Fatalf("Anthropic provider = %q", providers["claude:anthropic"])
	}
}

func TestMatchCloudTeamPrefersExactIDAndRejectsAmbiguousOrUnusableNames(
	t *testing.T,
) {
	teams := []broker.Team{
		{ID: "other", Name: "team-id", Use: true},
		{ID: "team-id", Name: "Exact ID", Use: true},
	}
	matched, err := matchCloudTeam(teams, "team-id")
	if err != nil {
		t.Fatal(err)
	}
	if matched.ID != "team-id" {
		t.Fatalf("matched %q, want exact ID", matched.ID)
	}

	if _, err := matchCloudTeam([]broker.Team{
		{ID: "a", Name: "Duplicate", Use: true},
		{ID: "b", Name: "Duplicate", Use: true},
	}, "Duplicate"); err == nil {
		t.Fatal("duplicate exact team names must be ambiguous")
	}
	if _, err := matchCloudTeam([]broker.Team{
		{ID: "blocked", Name: "Blocked", ManageAccounts: true},
	}, "blocked"); err == nil {
		t.Fatal("team without subrouter use permission was selected")
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
