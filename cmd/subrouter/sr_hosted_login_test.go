package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/internal/broker"
	"github.com/manaflow-ai/subrouter/internal/proxy"
	"github.com/manaflow-ai/subrouter/internal/tenant"
)

func TestSRLoginNativeStackConfiguresBuiltInCMUXRemote(t *testing.T) {
	tenantKey := "srt_0123456789abcdef0123456789abcdef"
	accessToken := testUnverifiedStackToken(t, map[string]any{
		"sub": "user", "project_id": "project", "selected_team_id": "team-1",
	})
	var exchangeAuthorization string
	var exchangeRefreshToken string
	var pollAttempts int
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/cli/config":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"version": 1,
				"auth": map[string]string{
					"apiUrl": server.URL, "projectId": "project",
					"publishableClientKey": "pck_test",
					"confirmUrl":           server.URL + "/handler/cli-auth-confirm",
				},
				"subrouter": map[string]string{
					"url":         "https://published.example",
					"exchangeUrl": server.URL + "/api/subrouter/exchange",
				},
			})
		case "/auth/cli":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"polling_code": "poll", "login_code": "login",
				"expires_at": time.Now().Add(time.Minute).UTC().Format(time.RFC3339Nano),
			})
		case "/auth/cli/poll":
			pollAttempts++
			if pollAttempts == 1 {
				http.Error(w, "temporarily unavailable", http.StatusServiceUnavailable)
				return
			}
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"status": "success", "refresh_token": "refresh",
			})
		case "/auth/oauth/token":
			_ = json.NewEncoder(w).Encode(map[string]string{
				"access_token": accessToken, "refresh_token": "rotated",
			})
		case "/teams":
			_ = json.NewEncoder(w).Encode(map[string]any{"items": []map[string]string{{
				"id": "team-1", "display_name": "Acme",
			}}})
		case "/api/subrouter/exchange":
			exchangeAuthorization = r.Header.Get("Authorization")
			exchangeRefreshToken = r.Header.Get("X-Stack-Refresh-Token")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"tenantId": "team-1", "tenantName": "Acme",
				"tenantKey": tenantKey, "proxyUrl": server.URL + "/t/" + tenantKey,
				"capabilities": []string{"use", "manage_accounts"},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	root := t.TempDir()
	t.Setenv("SUBROUTER_CLOUD_CONFIG", filepath.Join(root, "cloud.json"))
	t.Setenv("CODEX_HOME", filepath.Join(root, "codex-home"))
	store := accounts.CodexStore{Dir: filepath.Join(root, "state", "codex", "accounts")}
	var output bytes.Buffer
	runner := srRunner{
		program: "sr", store: store, in: strings.NewReader(""),
		out: &output, errOut: &output, client: server.Client(),
		// The first poll is a retryable 503; the retry need not wait the
		// production two seconds.
		cloudLoginPollInterval: 10 * time.Millisecond,
	}
	if err := runner.cloudLogin(context.Background(), []string{
		"--base-url", server.URL,
		"--hosted-url", server.URL,
		"--no-browser",
	}); err != nil {
		t.Fatal(err)
	}
	config, err := broker.LoadConfig("")
	if err != nil {
		t.Fatal(err)
	}
	if !config.HostedReady() || config.TeamID != "team-1" ||
		config.TenantKey != tenantKey || config.HostedURL != server.URL {
		t.Fatalf("config = %#v", config)
	}
	servers, err := defaultSRServerStore(store).load()
	if err != nil {
		t.Fatal(err)
	}
	serverConfig, ok := servers.find("cmux")
	if !ok || servers.Default != "cmux" || serverConfig.TenantKey != tenantKey {
		t.Fatalf("servers = %#v", servers)
	}
	codexConfig, err := os.ReadFile(filepath.Join(root, "codex-home", "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(codexConfig), server.URL+"/t/"+tenantKey+"/v1") {
		t.Fatalf("Codex config = %s", codexConfig)
	}
	if exchangeAuthorization != "Bearer "+accessToken {
		t.Fatalf("exchange authorization = %q", exchangeAuthorization)
	}
	if exchangeRefreshToken != "rotated" {
		t.Fatalf("exchange refresh token = %q", exchangeRefreshToken)
	}
	if pollAttempts != 2 {
		t.Fatalf("poll attempts = %d", pollAttempts)
	}
}

func TestHostedCodexAddUsesTemporaryHomeAndUploadsCredential(t *testing.T) {
	tenantKey := "srt_0123456789abcdef0123456789abcdef"
	var uploaded broker.AccountUpload
	var uploadDecodeErr error
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/t/"+tenantKey+"/_subrouter/accounts" {
			http.NotFound(w, r)
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&uploaded); err != nil {
			uploadDecodeErr = err
			http.Error(w, "invalid upload", http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"account": map[string]string{
				"id": "hosted@example.com", "kind": "codex", "label": "hosted@example.com",
			},
		})
	}))
	defer server.Close()

	root := t.TempDir()
	localCodexHome := filepath.Join(root, "codex-home")
	t.Setenv("HOME", root)
	t.Setenv("CODEX_HOME", localCodexHome)
	if err := os.MkdirAll(localCodexHome, 0o700); err != nil {
		t.Fatal(err)
	}
	localAuth := []byte(`{"sentinel":"unchanged"}`)
	if err := os.WriteFile(filepath.Join(localCodexHome, "auth.json"), localAuth, 0o600); err != nil {
		t.Fatal(err)
	}

	command := &recordingSRCommandRunner{
		loginAuth: testCodexAuth("hosted@example.com", "account-hosted"),
	}
	var output bytes.Buffer
	runner := srRunner{
		program: "sr",
		store:   accounts.CodexStore{Dir: filepath.Join(root, "state", "codex", "accounts")},
		in:      strings.NewReader(""),
		out:     &output,
		errOut:  &output,
		client:  server.Client(),
		cmd:     command,
	}
	client := &broker.Client{
		Config: broker.Config{
			BaseURL: "https://cmux.com", AccessToken: "stack-access",
			RefreshToken: "stack-refresh", TeamID: "team-1",
			CredentialSource: broker.CredentialSourceHosted,
			HostedURL:        server.URL,
			TenantKey:        tenantKey,
		},
		HTTPClient: server.Client(),
	}
	if err := runner.cloudAccountAdd(context.Background(), client, []string{"codex"}); err != nil {
		t.Fatal(err)
	}
	if uploadDecodeErr != nil {
		t.Fatal(uploadDecodeErr)
	}
	if uploaded["provider"] != "codex" || uploaded["label"] != "hosted@example.com" {
		t.Fatalf("upload = %#v", uploaded)
	}
	if uploaded["oauthCredentialOrigin"] != string(accounts.CodexOAuthOriginIsolatedServerLogin) {
		t.Fatalf("OAuth origin = %#v", uploaded["oauthCredentialOrigin"])
	}
	tokens, ok := uploaded["tokens"].(map[string]any)
	if !ok {
		t.Fatalf("tokens = %#v", uploaded["tokens"])
	}
	for key, want := range map[string]string{
		"accessToken":  command.loginAuth.Tokens.AccessToken,
		"refreshToken": command.loginAuth.Tokens.RefreshToken,
		"idToken":      command.loginAuth.Tokens.IDToken,
		"accountID":    accounts.ExtractChatGPTAccountID(command.loginAuth),
	} {
		if got, _ := tokens[key].(string); got != want {
			t.Fatalf("tokens[%q] = %q, want %q", key, got, want)
		}
	}
	body, err := os.ReadFile(filepath.Join(localCodexHome, "auth.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(body, localAuth) {
		t.Fatalf("local auth changed: %s", body)
	}
	if !command.hasEnvPrefix("CODEX_HOME=") {
		t.Fatalf("Codex login was not isolated: %#v", command.envs)
	}
	for _, env := range command.envs {
		for _, item := range env {
			if item == "CODEX_HOME="+localCodexHome {
				t.Fatalf("Codex login used the real home: %#v", command.envs)
			}
		}
	}
}

func TestCloudCodexRepairUsesFreshIsolatedLogin(t *testing.T) {
	var repaired broker.AccountUpload
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/subrouter/accounts":
			_ = json.NewEncoder(w).Encode(map[string]any{"accounts": []map[string]string{{
				"id": "shared-codex", "kind": "codex", "label": "Production Codex",
				"email": "hosted@example.com",
			}}})
		case r.Method == http.MethodPost && r.URL.Path == "/api/subrouter/accounts/shared-codex/repair":
			if r.URL.Query().Get("adopt") != "1" {
				http.Error(w, "missing adoption", http.StatusBadRequest)
				return
			}
			if err := json.NewDecoder(r.Body).Decode(&repaired); err != nil {
				http.Error(w, "invalid upload", http.StatusBadRequest)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"account": map[string]string{
				"id": "shared-codex", "kind": "codex", "label": "Production Codex",
			}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	root := t.TempDir()
	localCodexHome := filepath.Join(root, "codex-home")
	t.Setenv("HOME", root)
	t.Setenv("CODEX_HOME", localCodexHome)
	if err := os.MkdirAll(localCodexHome, 0o700); err != nil {
		t.Fatal(err)
	}
	localAuth := []byte(`{"sentinel":"unchanged"}`)
	if err := os.WriteFile(filepath.Join(localCodexHome, "auth.json"), localAuth, 0o600); err != nil {
		t.Fatal(err)
	}

	command := &recordingSRCommandRunner{
		loginAuth: testCodexAuth("hosted@example.com", "account-hosted"),
	}
	var output bytes.Buffer
	runner := srRunner{
		program: "sr",
		store:   accounts.CodexStore{Dir: filepath.Join(root, "state", "codex", "accounts")},
		in:      strings.NewReader(""),
		out:     &output,
		errOut:  &output,
		client:  server.Client(),
		cmd:     command,
	}
	client := broker.NewClient(broker.Config{
		BaseURL: server.URL, AccessToken: "stack-access", RefreshToken: "stack-refresh",
		TeamID: "team-1", CredentialSource: broker.CredentialSourceTeam,
	})
	client.HTTPClient = server.Client()
	if err := runner.cloudAccountRepair(context.Background(), client, []string{"shared-codex"}); err != nil {
		t.Fatal(err)
	}
	if repaired["oauthCredentialOrigin"] != string(accounts.CodexOAuthOriginIsolatedServerLogin) {
		t.Fatalf("OAuth origin = %#v", repaired["oauthCredentialOrigin"])
	}
	if repaired["accountId"] != "shared-codex" || repaired["label"] != "Production Codex" {
		t.Fatalf("repair = %#v", repaired)
	}
	body, err := os.ReadFile(filepath.Join(localCodexHome, "auth.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(body, localAuth) {
		t.Fatalf("local auth changed: %s", body)
	}
}

func TestCloudCodexRepairPropagatesServerOwnerRejection(t *testing.T) {
	repairCalled := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/subrouter/accounts":
			_ = json.NewEncoder(w).Encode(map[string]any{"accounts": []map[string]string{{
				"id": "shared-codex", "kind": "codex", "label": "Production Codex",
				"email": "expected@example.com",
			}}})
		case r.Method == http.MethodPost:
			repairCalled = true
			http.Error(w, "Codex owner does not match; shared account was not changed", http.StatusConflict)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("CODEX_HOME", filepath.Join(root, "codex-home"))
	command := &recordingSRCommandRunner{
		loginAuth: testCodexAuth("different@example.com", "account-different"),
	}
	runner := srRunner{
		program: "sr",
		store:   accounts.CodexStore{Dir: filepath.Join(root, "state", "codex", "accounts")},
		in:      strings.NewReader(""),
		out:     io.Discard,
		errOut:  io.Discard,
		client:  server.Client(),
		cmd:     command,
	}
	client := broker.NewClient(broker.Config{
		BaseURL: server.URL, AccessToken: "stack-access", RefreshToken: "stack-refresh",
		TeamID: "team-1", CredentialSource: broker.CredentialSourceTeam,
	})
	client.HTTPClient = server.Client()
	err := runner.cloudAccountRepair(context.Background(), client, []string{"shared-codex"})
	if err == nil || !strings.Contains(err.Error(), "409") {
		t.Fatalf("repair error = %v", err)
	}
	if !repairCalled {
		t.Fatal("server did not validate the encrypted credential owner")
	}
}

func TestHostedCodexAddPersistsServerAttestedCredentialThroughTenantEndpoint(t *testing.T) {
	registry := tenant.NewRegistry(t.TempDir())
	created, tenantKey, err := registry.Create("hosted")
	if err != nil {
		t.Fatal(err)
	}
	rotated := testCodexAuth("hosted@example.com", "account-hosted")
	rotated.Tokens.RefreshToken = "server-refresh-account-hosted"
	transport := srRoundTripFunc(func(*http.Request) (*http.Response, error) {
		body, err := json.Marshal(map[string]string{
			"access_token":  rotated.Tokens.AccessToken,
			"refresh_token": rotated.Tokens.RefreshToken,
			"id_token":      rotated.Tokens.IDToken,
		})
		if err != nil {
			t.Fatal(err)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(bytes.NewReader(body)),
		}, nil
	})
	multi := &proxy.MultiTenant{Base: proxy.Server{Transport: transport}, Registry: registry}
	server := httptest.NewServer(multi.Handler(multi.Base.Handler()))
	defer server.Close()

	root := t.TempDir()
	localCodexHome := filepath.Join(root, "codex-home")
	t.Setenv("CODEX_HOME", localCodexHome)
	if err := os.MkdirAll(localCodexHome, 0o700); err != nil {
		t.Fatal(err)
	}
	localAuth := []byte(`{"sentinel":"unchanged"}`)
	if err := os.WriteFile(filepath.Join(localCodexHome, "auth.json"), localAuth, 0o600); err != nil {
		t.Fatal(err)
	}

	command := &recordingSRCommandRunner{
		loginAuth: testCodexAuth("hosted@example.com", "account-hosted"),
	}
	var output bytes.Buffer
	runner := srRunner{
		program: "sr",
		store:   accounts.CodexStore{Dir: filepath.Join(root, "state", "codex", "accounts")},
		in:      strings.NewReader(""),
		out:     &output,
		errOut:  &output,
		client:  server.Client(),
		cmd:     command,
	}
	client := &broker.Client{
		Config: broker.Config{
			BaseURL: "https://cmux.com", AccessToken: "stack-access",
			RefreshToken: "stack-refresh", TeamID: "team-1",
			CredentialSource: broker.CredentialSourceHosted,
			HostedURL:        server.URL,
			TenantKey:        tenantKey,
		},
		HTTPClient: server.Client(),
	}
	if err := runner.cloudAccountAdd(context.Background(), client, []string{"codex"}); err != nil {
		t.Fatal(err)
	}

	stored, err := (accounts.CodexStore{
		Dir: filepath.Join(registry.Dir(created.ID), "codex", "accounts"),
	}).ListStored()
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) != 1 {
		t.Fatalf("stored accounts = %d, want 1", len(stored))
	}
	if stored[0].OAuthCredentialOrigin != accounts.CodexOAuthOriginServerAttested {
		t.Fatalf("stored OAuth origin = %q", stored[0].OAuthCredentialOrigin)
	}
	body, err := os.ReadFile(filepath.Join(localCodexHome, "auth.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(body, localAuth) {
		t.Fatalf("local auth changed: %s", body)
	}
}

func TestHostedAndLocalEgressStorageAliasesStayDistinct(t *testing.T) {
	for _, value := range []string{"hosted", "cmux", "cloud"} {
		got, err := parseCredentialSource(value, false)
		if err != nil {
			t.Fatalf("%s: %v", value, err)
		}
		if got != broker.CredentialSourceHosted {
			t.Fatalf("%s selected %q, want hosted", value, got)
		}
	}
	for _, value := range []string{"team", "shared", "cmux-local", "local-egress"} {
		got, err := parseCredentialSource(value, false)
		if err != nil {
			t.Fatalf("%s: %v", value, err)
		}
		if got != broker.CredentialSourceTeam {
			t.Fatalf("%s selected %q, want team", value, got)
		}
	}
}

func testUnverifiedStackToken(t *testing.T, claims map[string]any) string {
	t.Helper()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"ES256"}`))
	body, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	return header + "." + base64.RawURLEncoding.EncodeToString(body) + ".signature"
}

func TestHostedDefaultOutputUsesUsageDashboard(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("COLUMNS", "160")
	tenantKey := "srt_0123456789abcdef0123456789abcdef"
	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Method != http.MethodGet || r.URL.Path != "/t/"+tenantKey+"/_subrouter/usage-status" {
			http.Error(w, "unexpected request", http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode([]broker.UsageStatus{{
			ID: "hosted@example.com", Provider: accounts.ProviderCodex,
			AuthMode: accounts.AuthModeOAuth, Email: "hosted@example.com",
			AuthChecked: true, AuthValid: true, PlanType: "pro",
			Windows: []accounts.UsageWindow{{
				Name: "weekly", UsedPercent: 25,
				LimitWindowSeconds: int64((7 * 24 * time.Hour) / time.Second),
			}},
		}})
	}))
	defer server.Close()

	configPath := filepath.Join(t.TempDir(), "cloud.json")
	t.Setenv("SUBROUTER_CLOUD_CONFIG", configPath)
	if err := broker.SaveConfig(configPath, broker.Config{
		Version: 1, BaseURL: "https://cmux.com",
		AccessToken: "access", RefreshToken: "refresh",
		TeamID: "team-1", TeamName: "Hosted team",
		CredentialSource: broker.CredentialSourceHosted,
		HostedURL:        server.URL, TenantKey: tenantKey,
	}); err != nil {
		t.Fatal(err)
	}

	var output bytes.Buffer
	runner := srRunner{
		program: "sr", store: accounts.CodexStore{Dir: t.TempDir()},
		out: &output, errOut: &output,
	}
	if err := runner.run(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if requests != 1 {
		t.Fatalf("usage requests = %d, want 1", requests)
	}
	for _, want := range []string{
		"Credential storage: hosted cmux (Hosted team, team-1)",
		"Codex accounts",
		"hosted@example.com",
		"75% left",
	} {
		if !strings.Contains(output.String(), want) {
			t.Fatalf("hosted dashboard missing %q:\n%s", want, output.String())
		}
	}
}

// A hosted tenant with no accounts yet must say how to add one; bare sr is
// where a new hosted user lands.
func TestHostedDefaultOutputWithoutAccountsSaysHowToAdd(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	tenantKey := "srt_0123456789abcdef0123456789abcdef"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]broker.UsageStatus{})
	}))
	defer server.Close()
	configPath := filepath.Join(t.TempDir(), "cloud.json")
	t.Setenv("SUBROUTER_CLOUD_CONFIG", configPath)
	if err := broker.SaveConfig(configPath, broker.Config{
		Version: 1, BaseURL: "https://cmux.com",
		AccessToken: "access", RefreshToken: "refresh",
		TeamID: "team-1", TeamName: "Hosted team",
		CredentialSource: broker.CredentialSourceHosted,
		HostedURL:        server.URL, TenantKey: tenantKey,
	}); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	runner := srRunner{program: "sr", store: accounts.CodexStore{Dir: t.TempDir()}, out: &output, errOut: &output}
	if err := runner.run(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "Run 'sr add codex' to add one.") {
		t.Fatalf("empty hosted dashboard gives no next step:\n%s", output.String())
	}
}
