package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/internal/broker"
	"github.com/manaflow-ai/subrouter/internal/stackauth"
)

func TestMatchNativeStackTeamRequiresSelectorForMultipleTeams(t *testing.T) {
	teams := []stackauth.Team{
		{ID: "team-a", DisplayName: "Alpha"},
		{ID: "team-b", DisplayName: "Beta"},
	}
	_, err := matchNativeStackTeam(teams, "")
	if err == nil ||
		!strings.Contains(err.Error(), "Alpha") ||
		!strings.Contains(err.Error(), "Beta") ||
		!strings.Contains(err.Error(), "--team") {
		t.Fatalf("error = %v, want available teams and --team guidance", err)
	}
	only, err := matchNativeStackTeam(teams[:1], "")
	if err != nil || only.ID != "team-a" {
		t.Fatalf("single team = %#v, %v", only, err)
	}
}

func TestHostedStorageRejectsIncompleteConfigBeforeSaving(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "cloud.json")
	t.Setenv("SUBROUTER_CLOUD_CONFIG", configPath)
	var out bytes.Buffer
	runner := srRunner{
		program: "sr",
		store:   accounts.CodexStore{Dir: t.TempDir()},
		out:     &out,
		errOut:  &out,
	}
	err := runner.cloudStorage([]string{"hosted"})
	if err == nil || !strings.Contains(err.Error(), "run 'sr login'") {
		t.Fatalf("error = %v, want login guidance", err)
	}
	if _, statErr := os.Stat(configPath); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("incomplete hosted config was persisted: %v", statErr)
	}
}

func TestTeamCurrentUsesPersistedSelectionOffline(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "cloud.json")
	t.Setenv("SUBROUTER_CLOUD_CONFIG", configPath)
	saveHostedTestConfig(t, configPath, broker.Config{
		TeamID: "team-a", TeamName: "Alpha",
		StackAPIURL: "https://stack.invalid",
	})
	var out bytes.Buffer
	runner := srRunner{
		program: "sr", store: accounts.CodexStore{Dir: t.TempDir()},
		out: &out, errOut: &out,
		client: &http.Client{Transport: srRoundTripFunc(func(*http.Request) (*http.Response, error) {
			return nil, errors.New("offline")
		})},
	}
	if err := runner.cloudTeam(context.Background(), []string{"current"}); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); !strings.Contains(got, "Alpha (team-a)") {
		t.Fatalf("output = %q", got)
	}
}

func TestTeamListPersistsRotatedRefreshTokenWhenListingFails(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/auth/oauth/token":
			_, _ = io.WriteString(w, `{"access_token":"rotated-access","refresh_token":"rotated-refresh"}`)
		case "/teams":
			http.Error(w, "temporarily unavailable", http.StatusServiceUnavailable)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	configPath := filepath.Join(t.TempDir(), "cloud.json")
	t.Setenv("SUBROUTER_CLOUD_CONFIG", configPath)
	saveHostedTestConfig(t, configPath, broker.Config{
		TeamID: "team-a", TeamName: "Alpha", StackAPIURL: server.URL,
	})
	runner := srRunner{
		program: "sr", store: accounts.CodexStore{Dir: t.TempDir()},
		out: io.Discard, errOut: io.Discard, client: server.Client(),
	}
	if err := runner.cloudTeam(context.Background(), []string{"list"}); err == nil {
		t.Fatal("team list unexpectedly succeeded")
	}
	config, err := broker.LoadConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if config.RefreshToken != "rotated-refresh" ||
		config.AccessToken != "rotated-access" {
		t.Fatalf("rotated tokens were not persisted: %#v", config)
	}
}

func TestServerUseCMUXLeavesSelectionUntouchedWhenBrokerLoadFails(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("CODEX_HOME", filepath.Join(root, "codex"))
	configPath := filepath.Join(root, "cloud.json")
	t.Setenv("SUBROUTER_CLOUD_CONFIG", configPath)
	if err := os.WriteFile(configPath, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	store := accounts.CodexStore{Dir: filepath.Join(root, "state", "codex", "accounts")}
	serverStore := defaultSRServerStore(store)
	saveCMUXTestRemote(t, serverStore)
	codexPath := filepath.Join(root, "codex", "config.toml")
	if err := os.MkdirAll(filepath.Dir(codexPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(codexPath, []byte("sentinel = true\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	runner := srRunner{program: "sr", store: store, out: io.Discard, errOut: io.Discard}
	if err := runner.serverUse(serverStore, []string{"cmux"}); err == nil {
		t.Fatal("server use unexpectedly succeeded")
	}
	assertCMUXSelectionUnchanged(t, serverStore, codexPath)
}

func TestServerUseCMUXLeavesSelectionUntouchedWhenBrokerSaveFails(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX directory permissions required")
	}
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory write permissions")
	}
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("CODEX_HOME", filepath.Join(root, "codex"))
	configDir := filepath.Join(root, "readonly")
	configPath := filepath.Join(configDir, "cloud.json")
	t.Setenv("SUBROUTER_CLOUD_CONFIG", configPath)
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	saveHostedTestConfig(t, configPath, broker.Config{
		CredentialSource: broker.CredentialSourceLocal,
	})
	if err := os.Chmod(configDir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(configDir, 0o700) })

	store := accounts.CodexStore{Dir: filepath.Join(root, "state", "codex", "accounts")}
	serverStore := defaultSRServerStore(store)
	saveCMUXTestRemote(t, serverStore)
	codexPath := filepath.Join(root, "codex", "config.toml")
	if err := os.MkdirAll(filepath.Dir(codexPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(codexPath, []byte("sentinel = true\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	runner := srRunner{program: "sr", store: store, out: io.Discard, errOut: io.Discard}
	if err := runner.serverUse(serverStore, []string{"cmux"}); err == nil {
		t.Fatal("server use unexpectedly succeeded")
	}
	assertCMUXSelectionUnchanged(t, serverStore, codexPath)
}

func TestLogoutDoesNotReportSuccessWhenHostedRemoteCleanupFails(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	configPath := filepath.Join(root, "cloud.json")
	t.Setenv("SUBROUTER_CLOUD_CONFIG", configPath)
	if err := broker.SaveConfig(configPath, broker.Config{
		CredentialSource: broker.CredentialSourceLocal,
	}); err != nil {
		t.Fatal(err)
	}
	store := accounts.CodexStore{Dir: filepath.Join(root, "state", "codex", "accounts")}
	serverStore := defaultSRServerStore(store)
	if err := os.MkdirAll(filepath.Dir(serverStore.Path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(serverStore.Path, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	runner := srRunner{program: "sr", store: store, out: &out, errOut: &out}
	err := runner.cloudLogout(context.Background())
	if err == nil || !strings.Contains(err.Error(), "incomplete") {
		t.Fatalf("error = %v, want incomplete logout", err)
	}
	if strings.Contains(out.String(), "Logged out of cmux.com") {
		t.Fatalf("logout reported success before cleanup: %q", out.String())
	}
}

func TestLogoutRefreshesExpiredAccessTokenBeforeRevokingSession(t *testing.T) {
	var revoked bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/auth/oauth/token":
			_, _ = io.WriteString(w, `{"access_token":"fresh-access","refresh_token":"fresh-refresh"}`)
		case "/auth/sessions/current":
			if r.Header.Get("X-Stack-Access-Token") != "fresh-access" ||
				r.Header.Get("X-Stack-Refresh-Token") != "fresh-refresh" {
				http.Error(w, "stale session tokens", http.StatusUnauthorized)
				return
			}
			revoked = true
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	runner, configPath, output := newLogoutTestRunner(t, server)
	if err := runner.cloudLogout(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !revoked {
		t.Fatal("fresh Stack session was not revoked")
	}
	assertLoggedOutConfig(t, configPath)
	if !strings.Contains(output.String(), "Logged out of cmux.com") {
		t.Fatalf("output = %q", output.String())
	}
}

func TestLogoutClearsLocallyWhenRefreshTokenIsAlreadyInvalid(t *testing.T) {
	var signOutCalls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/auth/oauth/token":
			http.Error(w, `{"code":"REFRESH_TOKEN_EXPIRED"}`, http.StatusBadRequest)
		case "/auth/sessions/current":
			signOutCalls++
			http.Error(w, "unexpected", http.StatusInternalServerError)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	runner, configPath, output := newLogoutTestRunner(t, server)
	if err := runner.cloudLogout(context.Background()); err != nil {
		t.Fatal(err)
	}
	if signOutCalls != 0 {
		t.Fatalf("sign-out calls = %d, want 0", signOutCalls)
	}
	assertLoggedOutConfig(t, configPath)
	if !strings.Contains(output.String(), "already expired or revoked") {
		t.Fatalf("output = %q", output.String())
	}
}

func TestLogoutKeepsCredentialsForNonTerminalRefreshRejection(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"code":"INVALID_CLIENT"}`, http.StatusBadRequest)
	}))
	defer server.Close()

	runner, configPath, _ := newLogoutTestRunner(t, server)
	err := runner.cloudLogout(context.Background())
	if err == nil || !strings.Contains(err.Error(), "kept so logout can be retried") {
		t.Fatalf("error = %v", err)
	}
	config, loadErr := broker.LoadConfig(configPath)
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if config.AccessToken != "access" || config.RefreshToken != "refresh" ||
		config.TenantKey == "" {
		t.Fatalf("credentials changed after nonterminal rejection: %#v", config)
	}
}

func TestLogoutKeepsCredentialsWhenStackRefreshIsTransientlyUnavailable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "temporarily unavailable", http.StatusServiceUnavailable)
	}))
	defer server.Close()

	runner, configPath, output := newLogoutTestRunner(t, server)
	err := runner.cloudLogout(context.Background())
	if err == nil || !strings.Contains(err.Error(), "kept so logout can be retried") {
		t.Fatalf("error = %v", err)
	}
	config, loadErr := broker.LoadConfig(configPath)
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if config.AccessToken != "access" || config.RefreshToken != "refresh" ||
		config.TenantKey == "" {
		t.Fatalf("credentials changed after transient refresh failure: %#v", config)
	}
	if strings.Contains(output.String(), "Logged out of cmux.com") {
		t.Fatalf("logout incorrectly reported success: %q", output.String())
	}
}

func TestLogoutPersistsRotatedTokensWhenSignOutIsTransientlyUnavailable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/auth/oauth/token":
			_, _ = io.WriteString(w, `{"access_token":"fresh-access","refresh_token":"fresh-refresh"}`)
		case "/auth/sessions/current":
			http.Error(w, "temporarily unavailable", http.StatusServiceUnavailable)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	runner, configPath, output := newLogoutTestRunner(t, server)
	err := runner.cloudLogout(context.Background())
	if err == nil || !strings.Contains(err.Error(), "refreshed local credentials were kept") {
		t.Fatalf("error = %v", err)
	}
	config, loadErr := broker.LoadConfig(configPath)
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if config.AccessToken != "fresh-access" ||
		config.RefreshToken != "fresh-refresh" || config.TenantKey == "" {
		t.Fatalf("rotated credentials were not retained: %#v", config)
	}
	if strings.Contains(output.String(), "Logged out of cmux.com") {
		t.Fatalf("logout incorrectly reported success: %q", output.String())
	}
}

func TestLogoutClearsLocallyWhenFreshSessionIsAlreadyGone(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/auth/oauth/token":
			_, _ = io.WriteString(w, `{"access_token":"fresh-access","refresh_token":"fresh-refresh"}`)
		case "/auth/sessions/current":
			http.Error(w, `{"code":"SESSION_NOT_FOUND"}`, http.StatusNotFound)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	runner, configPath, output := newLogoutTestRunner(t, server)
	if err := runner.cloudLogout(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertLoggedOutConfig(t, configPath)
	if !strings.Contains(output.String(), "already expired or revoked") {
		t.Fatalf("output = %q", output.String())
	}
}

func newLogoutTestRunner(
	t *testing.T,
	server *httptest.Server,
) (srRunner, string, *bytes.Buffer) {
	t.Helper()
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("CODEX_HOME", filepath.Join(root, "codex"))
	configPath := filepath.Join(root, "cloud.json")
	t.Setenv("SUBROUTER_CLOUD_CONFIG", configPath)
	saveHostedTestConfig(t, configPath, broker.Config{StackAPIURL: server.URL})
	output := &bytes.Buffer{}
	return srRunner{
		program: "sr",
		store: accounts.CodexStore{
			Dir: filepath.Join(root, "state", "codex", "accounts"),
		},
		out: output, errOut: output, client: server.Client(),
	}, configPath, output
}

func assertLoggedOutConfig(t *testing.T, path string) {
	t.Helper()
	config, err := broker.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if config.AccessToken != "" || config.RefreshToken != "" ||
		config.TeamID != "" || config.TeamName != "" ||
		config.HostedURL != "" || config.HostedExchangeURL != "" ||
		config.TenantKey != "" || len(config.TenantCapabilities) != 0 ||
		config.CredentialSource != broker.CredentialSourceLocal {
		t.Fatalf("config still contains hosted credentials: %#v", config)
	}
}

func TestHostedClaudeUploadGuidanceNamesHostedCMUX(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "cloud.json")
	t.Setenv("SUBROUTER_CLOUD_CONFIG", configPath)
	saveHostedTestConfig(t, configPath, broker.Config{})
	runner := srRunner{
		program: "sr", store: accounts.CodexStore{Dir: t.TempDir()},
		out: io.Discard, errOut: io.Discard,
	}
	err := runner.pushClaudeProfile(context.Background(), "work", true)
	if err == nil || !strings.Contains(err.Error(), "hosted cmux uses") {
		t.Fatalf("error = %v", err)
	}
}

func TestRemoteHelpDocumentsRenameAndLegacyServerForm(t *testing.T) {
	help := srRemoteHelp("sr remote")
	if !strings.Contains(help, "rename <old> <new>") {
		t.Fatalf("remote help missing rename:\n%s", help)
	}
	if !strings.Contains(srHelp, "legacy") {
		t.Fatalf("sr help does not identify the legacy server form:\n%s", srHelp)
	}
}

func TestBuiltInRemoteNamesCannotBeAddedOrRenamed(t *testing.T) {
	store := srServerStore{Path: filepath.Join(t.TempDir(), "servers.json")}
	runner := srRunner{program: "sr", out: io.Discard, errOut: io.Discard}
	for _, name := range []string{"local", "cmux", "cmux-local", "local-egress"} {
		if err := runner.serverAdd(store, []string{name, "--url", "https://example.com"}); err == nil {
			t.Fatalf("built-in remote %q was added", name)
		}
	}
	if err := runner.serverAdd(store, []string{"team", "--url", "https://example.com"}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"local", "cmux", "cmux-local", "local-egress"} {
		if err := runner.serverRename(store, "team", name); err == nil {
			t.Fatalf("server was renamed to built-in remote %q", name)
		}
	}
}

func saveHostedTestConfig(t *testing.T, path string, override broker.Config) {
	t.Helper()
	config := broker.Config{
		BaseURL: "https://cmux.com", AccessToken: "access",
		RefreshToken: "refresh", TeamID: "team-a", TeamName: "Alpha",
		CredentialSource: broker.CredentialSourceHosted,
		HostedURL:        "https://sr.cmux.dev",
		TenantKey:        "srt_0123456789abcdef0123456789abcdef",
		StackAPIURL:      "https://api.stack-auth.com/api/v1",
		StackProjectID:   "project",
		StackPublishable: "pck_test",
	}
	if override.TeamID != "" {
		config.TeamID = override.TeamID
	}
	if override.TeamName != "" {
		config.TeamName = override.TeamName
	}
	if override.StackAPIURL != "" {
		config.StackAPIURL = override.StackAPIURL
	}
	if override.CredentialSource != "" {
		config.CredentialSource = override.CredentialSource
	}
	if err := broker.SaveConfig(path, config); err != nil {
		t.Fatal(err)
	}
}

func saveCMUXTestRemote(t *testing.T, store srServerStore) {
	t.Helper()
	if err := store.save(srServerFile{Servers: []srServerConfig{{
		Name: "cmux", URL: "https://sr.cmux.dev",
		TenantKey: "srt_0123456789abcdef0123456789abcdef",
	}}}); err != nil {
		t.Fatal(err)
	}
}

func assertCMUXSelectionUnchanged(t *testing.T, store srServerStore, codexPath string) {
	t.Helper()
	file, err := store.load()
	if err != nil {
		t.Fatal(err)
	}
	if file.Default != "" {
		t.Fatalf("default changed to %q", file.Default)
	}
	body, err := os.ReadFile(codexPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "sentinel = true\n" {
		t.Fatalf("Codex config changed: %s", body)
	}
}
