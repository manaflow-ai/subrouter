package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/internal/broker"
	"github.com/manaflow-ai/subrouter/internal/proxy"
	"github.com/manaflow-ai/subrouter/selectacct"
)

func TestStandaloneUsageFetchOnlyRunsWhenAutoSwitchIsDisabled(t *testing.T) {
	tests := []struct {
		name             string
		fetchUsage       bool
		brokerConfigured bool
		interval         time.Duration
		want             bool
	}{
		{name: "interval zero", fetchUsage: true, want: true},
		{name: "negative interval", fetchUsage: true, interval: -time.Second, want: true},
		{name: "leased auto switch owns startup sweep", fetchUsage: true, interval: time.Minute},
		{name: "usage disabled", interval: 0},
		{name: "credential broker", fetchUsage: true, brokerConfigured: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := shouldStartStandaloneUsageFetch(test.fetchUsage, test.brokerConfigured, test.interval); got != test.want {
				t.Fatalf("shouldStartStandaloneUsageFetch() = %v, want %v", got, test.want)
			}
		})
	}
}

func TestCodexUsageFetchConcurrencyIsBounded(t *testing.T) {
	accountsToFetch := make([]accounts.Account, codexUsageFetchConcurrency*3)
	for i := range accountsToFetch {
		accountsToFetch[i] = accounts.Account{
			ID:       fmt.Sprintf("account-%d", i),
			Provider: accounts.ProviderCodex,
			AuthMode: accounts.AuthModeOAuth,
		}
	}
	started := make(chan struct{}, len(accountsToFetch))
	release := make(chan struct{})
	done := make(chan struct{})
	var active atomic.Int32
	var peak atomic.Int32
	go func() {
		defer close(done)
		_, _ = fetchCodexScoresWithRefresh(context.Background(), accountsToFetch, func(_ context.Context, _ *http.Client, account accounts.Account) (accounts.Account, error) {
			current := active.Add(1)
			for {
				old := peak.Load()
				if current <= old || peak.CompareAndSwap(old, current) {
					break
				}
			}
			started <- struct{}{}
			<-release
			active.Add(-1)
			return account, errors.New("test stops before the usage request")
		})
	}()
	for range codexUsageFetchConcurrency {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("bounded workers did not start")
		}
	}
	select {
	case <-started:
		t.Fatalf("more than %d usage workers started concurrently", codexUsageFetchConcurrency)
	case <-time.After(25 * time.Millisecond):
	}
	close(release)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("bounded usage fetch did not finish")
	}
	if got := peak.Load(); got != codexUsageFetchConcurrency {
		t.Fatalf("peak concurrent usage fetches = %d, want %d", got, codexUsageFetchConcurrency)
	}
}

func TestSchedulerAccountsByProviderExcludesNonCodexPools(t *testing.T) {
	all := []accounts.Account{
		{ID: "codex", Provider: accounts.ProviderCodex, AuthMode: accounts.AuthModeOAuth},
		{ID: "claude", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth},
		{ID: "kimi-oauth", Provider: accounts.ProviderKimi, AuthMode: accounts.AuthModeOAuth},
		{ID: "kimi-key", Provider: accounts.ProviderKimi, AuthMode: accounts.AuthModeAPIKey},
		{ID: "grok", Provider: accounts.ProviderGrok, AuthMode: accounts.AuthModeOAuth},
		{ID: "antigravity", Provider: accounts.ProviderAntigravity, AuthMode: accounts.AuthModeOAuth},
		{ID: "qwen", Provider: accounts.ProviderQwenToken, AuthMode: accounts.AuthModeAPIKey},
		{ID: "legacy-empty-provider", AuthMode: accounts.AuthModeOAuth},
	}
	codex, claude := schedulerAccountsByProvider(all)
	if len(codex) != 1 || codex[0].ID != "codex" {
		t.Fatalf("Codex scheduler accounts = %+v", codex)
	}
	if len(claude) != 1 || claude[0].ID != "claude" {
		t.Fatalf("Claude scheduler accounts = %+v", claude)
	}

	scores := fallbackScores(all)
	if len(scores) != 1 || scores[0].AccountID != "codex" || scores[0].Provider != accounts.ProviderCodex {
		t.Fatalf("Codex fallback scores = %+v", scores)
	}
}

func TestInitialSchedulerCredentialSnapshotIncludesNonCodexProvidersAtSameRevision(t *testing.T) {
	const (
		generation = uint64(7)
		revision   = uint64(11)
	)
	all := []accounts.Account{
		{ID: "codex", Provider: accounts.ProviderCodex, AuthMode: accounts.AuthModeOAuth, Token: "codex-token"},
		{ID: "qwen-token:work", Provider: accounts.ProviderQwenAnthropic, AuthMode: accounts.AuthModeAPIKey, Token: "qwen-key"},
	}
	ref := selectacct.NewSchedulerRef(selectacct.NewScheduler(fallbackScores(all)))
	initial := initialSchedulerCredentialAccounts(all)
	ref.AdvanceAccountGenerationWithAccounts(generation, revision, initial)

	// The first all-account refresh can legitimately carry the same revision as
	// startup. Its no-op must be safe because startup already fingerprinted the
	// non-Codex credential under its shared scheduler provider.
	if !ref.SyncAccountCredentials(generation, revision, proxy.SchedulerAccounts(all)) {
		t.Fatal("same-revision credential sync was rejected")
	}
	qwen := all[1]
	ref.MarkCredentialExhaustedForSnapshot(
		accounts.ProviderQwenToken, qwen.ID, qwen.CredentialIdentity(), time.Now().Add(time.Hour),
		generation, revision, proxy.SchedulerAccounts(all),
	)
	if _, blocked := ref.ExhaustedUntilFor(accounts.ProviderQwenToken, qwen.ID, ""); !blocked {
		t.Fatal("startup snapshot omitted same-revision Qwen credential")
	}
}

func TestSecretValueUsesFileEnvironmentWithoutOverridingFlag(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tenant-secret")
	if err := os.WriteFile(path, []byte("file-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TEST_SECRET", "")
	t.Setenv("TEST_SECRET_FILE", path)
	got, err := secretValue("", "TEST_SECRET", "TEST_SECRET_FILE")
	if err != nil || got != "file-secret" {
		t.Fatalf("secret = %q, %v", got, err)
	}
	got, err = secretValue("flag-secret", "TEST_SECRET", "TEST_SECRET_FILE")
	if err != nil || got != "flag-secret" {
		t.Fatalf("flag secret = %q, %v", got, err)
	}
}

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

func TestConfigureDefaultLoggerLeavesIsolationCheckReadOnly(t *testing.T) {
	previous := slog.Default()
	defer slog.SetDefault(previous)

	root := t.TempDir()
	stateDir := filepath.Join(root, "state")
	t.Setenv("SUBROUTER_STATE_DIR", stateDir)
	sentinel := slog.New(slog.NewTextHandler(io.Discard, nil))
	slog.SetDefault(sentinel)

	configureDefaultLogger("subrouter", []string{"codex", "isolation-check", "--json"})
	if slog.Default() != sentinel {
		t.Fatal("isolation check should keep the process logger")
	}
	if _, err := os.Stat(stateDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("isolation check logger created state: %v", err)
	}
}

func TestValidatePublicSubrouterURLRequiresAnHTTPSOrigin(t *testing.T) {
	for _, valid := range []string{
		"",
		"https://sr.example.com",
		"https://sr.example.com/",
		"http://127.0.0.1:31415",
	} {
		if err := validatePublicSubrouterURL(valid); err != nil {
			t.Fatalf("%q: %v", valid, err)
		}
	}
	for _, invalid := range []string{
		"https://sr.example.com/path",
		"https://user@sr.example.com",
		"https://sr.example.com?query=1",
		"http://sr.example.com",
	} {
		if err := validatePublicSubrouterURL(invalid); err == nil {
			t.Fatalf("%q was accepted", invalid)
		}
	}
}

func TestNormalizePublicSubrouterURLTrimsFlagValues(t *testing.T) {
	got, err := normalizePublicSubrouterURL("  https://sr.example.com/  ")
	if err != nil {
		t.Fatal(err)
	}
	if got != "https://sr.example.com" {
		t.Fatalf("normalized public URL = %q, want https://sr.example.com", got)
	}
}

func TestServeKeepsHostedLoginCompatibleWithoutTenantDeleteToken(t *testing.T) {
	t.Setenv("SUBROUTER_STATE_DIR", t.TempDir())
	// DefaultCodexStore performs one-time legacy migration. Isolate HOME so a
	// unit test never walks or copies the developer's real account archive.
	t.Setenv("HOME", t.TempDir())
	t.Setenv("SUBROUTER_STACK_PROJECT_ID", "project")
	t.Setenv("SUBROUTER_STACK_PUBLISHABLE_CLIENT_KEY", "publishable")
	t.Setenv("SUBROUTER_STACK_TENANT_KEY_SECRET", "0123456789abcdef0123456789abcdef")
	t.Setenv("SUBROUTER_STACK_TENANT_KEY_SECRET_FILE", "")
	t.Setenv("SUBROUTER_STACK_TENANT_DELETE_TOKEN", "")
	t.Setenv("SUBROUTER_STACK_TENANT_DELETE_TOKEN_FILE", "")

	err := serve([]string{"--public-url", "http://sr.example.com"})
	if err == nil || !strings.Contains(err.Error(), "must use HTTPS") {
		t.Fatalf("serve error = %v, want public URL validation after legacy hosted-login config", err)
	}
}

func TestServeDoesNotMigrateLegacyCodexOAuthOnStartup(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	stateDir := filepath.Join(root, "state")
	t.Setenv("HOME", home)
	t.Setenv("SUBROUTER_STATE_DIR", stateDir)

	legacyStore := accounts.CodexStore{Dir: filepath.Join(home, ".codex-accounts", "accounts")}
	if err := legacyStore.SaveStored(accounts.StoredCodexAccount{
		Email: "legacy@example.test",
		Auth:  testCodexAuth("legacy@example.test", "acct-legacy"),
	}); err != nil {
		t.Fatal(err)
	}

	err := serve([]string{
		"--bedrock",
		"--bedrock-region", "",
		"--fetch-usage=false",
		"--sr-switch-interval=0",
	})
	if err == nil || !strings.Contains(err.Error(), "no AWS regions configured") {
		t.Fatalf("serve error = %v, want post-store validation failure", err)
	}

	migrated := filepath.Join(stateDir, "codex", "accounts", "legacy@example.test.json")
	if _, err := os.Stat(migrated); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("serve copied legacy OAuth credential to %s: %v", migrated, err)
	}
	if _, err := os.Stat(filepath.Join(legacyStore.Dir, "legacy@example.test.json")); err != nil {
		t.Fatalf("legacy OAuth credential disappeared: %v", err)
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

func TestTeamModeRejectsBedrockCredentialFallback(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "cloud.json")
	if err := broker.SaveConfig(configPath, broker.Config{
		BaseURL:          "https://cmux.com",
		AccessToken:      "stack-access",
		RefreshToken:     "stack-refresh",
		TeamID:           "team-a",
		CredentialSource: broker.CredentialSourceTeam,
	}); err != nil {
		t.Fatal(err)
	}
	err := serve([]string{
		"--cloud-config", configPath,
		"--addr", "invalid:::",
		"--bedrock",
	})
	if err == nil || !strings.Contains(err.Error(), "team credential storage") {
		t.Fatal("team mode did not reject Bedrock credential fallback")
	}
}

func TestTeamModeRejectsPersonalFableKeyFallback(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "cloud.json")
	if err := broker.SaveConfig(configPath, broker.Config{
		BaseURL:          "https://cmux.com",
		AccessToken:      "stack-access",
		RefreshToken:     "stack-refresh",
		TeamID:           "team-a",
		CredentialSource: broker.CredentialSourceTeam,
	}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SUBROUTER_CLAUDE_FABLE_API_KEY", "sk-ant-private")
	err := serve([]string{
		"--cloud-config", configPath,
		"--addr", "invalid:::",
	})
	if err == nil || !strings.Contains(err.Error(), "team credential storage") {
		t.Fatal("team mode did not reject personal Fable credential fallback")
	}
}

// Docker team mode accepts a copied workstation config. In production that
// config can still describe legacy storage and a loopback development API, so
// the container must explicitly select team storage and the hosted API without
// rewriting the read-only credential secret.
func TestServeCloudOverridesUpgradeCopiedLegacyConfigForTeamContainer(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "cloud.json")
	if err := broker.SaveConfig(configPath, broker.Config{
		BaseURL:          "http://127.0.0.1:3928",
		AccessToken:      "stack-access",
		RefreshToken:     "stack-refresh",
		TeamID:           "team-a",
		CredentialSource: broker.CredentialSourceLegacy,
	}); err != nil {
		t.Fatal(err)
	}
	err := serve([]string{
		"--cloud-config", configPath,
		"--cloud-base-url", "https://cmux.com",
		"--cloud-credential-source", "team",
		"--addr", "invalid:::",
		"--bedrock",
	})
	if err == nil || !strings.Contains(err.Error(), "team credential storage") {
		t.Fatalf("serve error = %v, want team-mode validation after Docker overrides", err)
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
	t.Setenv("SUBROUTER_STATE_DIR", filepath.Join(home, ".subrouter"))

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
	t.Setenv("SUBROUTER_STATE_DIR", filepath.Join(home, ".subrouter"))

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
		"account",
		"accounts",
		"add",
		"add-admin-key",
		"add-api-key",
		"add-key",
		"admin-keys",
		"agy",
		"antigravity",
		"attach-project",
		"az",
		"azure",
		"breadcrumbs",
		"claude",
		"claude-aws",
		"claude-direct",
		"cleanup",
		"cost",
		"daemon",
		"doctor",
		"g",
		"gemini",
		"gui",
		"gui-switch",
		"gui-use",
		"import",
		"kimi",
		"list",
		"list-admin-keys",
		"login",
		"logout",
		"ls",
		"pick",
		"qwen",
		"remote",
		"remotes",
		"remove",
		"remove-admin-key",
		"reset",
		"rm",
		"server",
		"servers",
		"setup",
		"spend",
		"status",
		"storage",
		"switch",
		"tenant",
		"tenants",
		"team",
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

func TestKimiNamespaceDispatchesThroughExecutableEntrypoints(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for _, program := range []string{"sr", "subrouter"} {
		if err := runForProgram(program, []string{"kimi", "help"}); err != nil {
			t.Fatalf("%s kimi help: %v", program, err)
		}
	}
}

func TestAntigravityNamespaceDispatchesThroughExecutableEntrypoints(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for _, program := range []string{"sr", "subrouter"} {
		for _, command := range []string{"agy", "antigravity"} {
			if err := runForProgram(program, []string{command, "help"}); err != nil {
				t.Fatalf("%s %s help: %v", program, command, err)
			}
		}
	}
}

func TestAntigravityHelpDescribesRoutedAndDirectBoundaries(t *testing.T) {
	for name, text := range map[string]string{
		"sr help":         srHelp,
		"subrouter help":  usageText("subrouter"),
		"management help": antigravityManagementHelp,
		"routing notice":  antigravityProxyHelp,
	} {
		if name != "sr help" && name != "subrouter help" && !strings.Contains(strings.ToLower(text), "pooled") {
			t.Fatalf("%s does not explain pooled AGY routing", name)
		}
	}
}

func TestSRAccountsAliasUsesTheSelectedTeamVault(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/api/subrouter/accounts" {
				http.NotFound(w, r)
				return
			}
			requests.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"teamId":"team-a","accounts":[]}`))
		},
	))
	defer server.Close()

	t.Setenv("SUBROUTER_STATE_DIR", t.TempDir())
	// sr initializes the local Codex store before dispatching the hosted alias.
	// Keep its legacy migration away from the developer's real HOME.
	t.Setenv("HOME", t.TempDir())
	configPath := filepath.Join(t.TempDir(), "cloud.json")
	t.Setenv("SUBROUTER_CLOUD_CONFIG", configPath)
	if err := broker.SaveConfig(configPath, broker.Config{
		BaseURL:          server.URL,
		AccessToken:      "stack-access",
		RefreshToken:     "stack-refresh",
		TeamID:           "team-a",
		TeamName:         "Team A",
		CredentialSource: broker.CredentialSourceTeam,
	}); err != nil {
		t.Fatal(err)
	}

	if err := runForProgram("sr", []string{"accounts"}); err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 1 {
		t.Fatalf("team account requests = %d, want 1", requests.Load())
	}
}

func TestUsageShowsAccountCommandsAtTopLevel(t *testing.T) {
	got := usageText("sr")
	for _, want := range []string{
		"sr login",
		"sr team",
		"sr account",
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
