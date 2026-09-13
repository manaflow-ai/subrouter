package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	agentclaude "github.com/manaflow-ai/subrouter/internal/agents/claude"
	agentkimi "github.com/manaflow-ai/subrouter/internal/agents/kimi"
)

type failingAccountInventorySource struct {
	stubOAuthSource
	err error
}

func (s failingAccountInventorySource) AccountInventoryCount(context.Context) (int, error) {
	return 0, s.err
}

func TestAccountImportPreflightRequiresProtectedRemoteAccess(t *testing.T) {
	for _, tc := range []struct {
		name       string
		adminToken string
		auth       string
		wantStatus int
	}{
		{name: "missing server token fails closed", wantStatus: http.StatusUnauthorized},
		{name: "missing request token", adminToken: "secret", wantStatus: http.StatusUnauthorized},
		{name: "wrong request token", adminToken: "secret", auth: "Bearer wrong", wantStatus: http.StatusUnauthorized},
		{name: "matching request token", adminToken: "secret", auth: "Bearer secret", wantStatus: http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ref := NewAccountRef(accounts.CodexStore{Dir: t.TempDir()}, nil, nil)
			ref.claudeStore = agentclaude.Store{Dir: t.TempDir()}
			handler := Server{AccountRef: ref, AdminToken: tc.adminToken}.Handler()
			req := httptest.NewRequest(http.MethodGet, "/_subrouter/account-import", nil)
			req.RemoteAddr = "100.64.0.20:4321"
			req.Header.Set("Authorization", tc.auth)
			resp := httptest.NewRecorder()

			handler.ServeHTTP(resp, req)

			if resp.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d, body = %s", resp.Code, tc.wantStatus, resp.Body.String())
			}
		})
	}
}

func TestAccountImportTokenCannotAccessAdminEndpoints(t *testing.T) {
	ref := NewAccountRef(accounts.CodexStore{Dir: t.TempDir()}, nil, nil)
	ref.claudeStore = agentclaude.Store{Dir: t.TempDir()}
	handler := Server{
		AccountRef:         ref,
		AdminToken:         "admin-secret",
		AccountImportToken: "import-secret",
	}.Handler()

	importRequest := httptest.NewRequest(http.MethodGet, "/_subrouter/account-import", nil)
	importRequest.RemoteAddr = "100.64.0.20:4321"
	importRequest.Header.Set("Authorization", "Bearer import-secret")
	importResponse := httptest.NewRecorder()
	handler.ServeHTTP(importResponse, importRequest)
	if importResponse.Code != http.StatusOK {
		t.Fatalf("account import status = %d, want 200", importResponse.Code)
	}

	adminRequest := httptest.NewRequest(http.MethodGet, "/_subrouter/accounts", nil)
	adminRequest.RemoteAddr = "100.64.0.20:4321"
	adminRequest.Header.Set("Authorization", "Bearer import-secret")
	adminResponse := httptest.NewRecorder()
	handler.ServeHTTP(adminResponse, adminRequest)
	if adminResponse.Code != http.StatusUnauthorized {
		t.Fatalf("import token accessed admin endpoint with status %d", adminResponse.Code)
	}
}

func TestRemoteAdminEndpointsFailClosedWithoutAdminToken(t *testing.T) {
	ref := NewAccountRef(accounts.CodexStore{Dir: t.TempDir()}, nil, nil)
	ref.claudeStore = agentclaude.Store{Dir: t.TempDir()}
	handler := Server{
		AccountRef:         ref,
		AccountImportToken: "import-secret",
	}.Handler()

	req := httptest.NewRequest(http.MethodGet, "/_subrouter/accounts", nil)
	req.RemoteAddr = "100.64.0.20:4321"
	req.Header.Set("Authorization", "Bearer import-secret")
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, req)

	if resp.Code != http.StatusUnauthorized {
		t.Fatalf("remote admin endpoint without an admin token returned %d, want 401", resp.Code)
	}
}

func TestAccountImportPreflightRequiresAdminTokenFromLoopback(t *testing.T) {
	for _, tc := range []struct {
		name       string
		adminToken string
		auth       string
		wantStatus int
	}{
		{name: "missing server token fails closed", wantStatus: http.StatusUnauthorized},
		{name: "missing request token", adminToken: "secret", wantStatus: http.StatusUnauthorized},
		{name: "wrong request token", adminToken: "secret", auth: "Bearer wrong", wantStatus: http.StatusUnauthorized},
		{name: "matching request token", adminToken: "secret", auth: "Bearer secret", wantStatus: http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ref := NewAccountRef(accounts.CodexStore{Dir: t.TempDir()}, nil, nil)
			ref.claudeStore = agentclaude.Store{Dir: t.TempDir()}
			handler := Server{AccountRef: ref, AdminToken: tc.adminToken}.Handler()
			req := httptest.NewRequest(http.MethodGet, "/_subrouter/account-import", nil)
			req.RemoteAddr = "127.0.0.1:4321"
			req.Header.Set("Authorization", tc.auth)
			resp := httptest.NewRecorder()

			handler.ServeHTTP(resp, req)

			if resp.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d, body = %s", resp.Code, tc.wantStatus, resp.Body.String())
			}
		})
	}
}

func TestAccountImportCodexPersistsAndHotLoadsWithoutReturningSecrets(t *testing.T) {
	codexStore := accounts.CodexStore{Dir: t.TempDir()}
	claudeStore := agentclaude.Store{Dir: t.TempDir()}
	rotated := proxyStoredOAuthAccount("founders@manaflow.ai", "server", time.Now().Add(time.Hour))
	client := &http.Client{Transport: proxyRoundTripFunc(func(*http.Request) (*http.Response, error) {
		body, err := json.Marshal(map[string]string{
			"access_token":  rotated.Auth.Tokens.AccessToken,
			"refresh_token": rotated.Auth.Tokens.RefreshToken,
			"id_token":      rotated.Auth.Tokens.IDToken,
		})
		if err != nil {
			t.Fatal(err)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(bytes.NewReader(body)),
		}, nil
	})}
	ref := NewAccountRef(codexStore, nil, client)
	ref.claudeStore = claudeStore
	handler := Server{AccountRef: ref, AdminToken: "secret"}.Handler()
	account := proxyStoredOAuthAccount("founders@manaflow.ai", "fresh", time.Now().Add(time.Hour))
	payload, err := json.Marshal(map[string]any{
		"provider": "codex",
		"codex":    account,
	})
	if err != nil {
		t.Fatal(err)
	}

	resp := serveProtectedAccountImport(handler, payload)
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.Code, resp.Body.String())
	}
	if strings.Contains(resp.Body.String(), "fresh-refresh") || strings.Contains(resp.Body.String(), "fresh-access") {
		t.Fatalf("response leaked OAuth credentials: %s", resp.Body.String())
	}
	stored, ok, err := codexStore.FindStored("founders@manaflow.ai")
	if err != nil || !ok {
		t.Fatalf("stored account = found:%v err:%v", ok, err)
	}
	if stored.Auth.Tokens == nil || stored.Auth.Tokens.RefreshToken != "server-refresh" ||
		stored.OAuthCredentialOrigin != accounts.CodexOAuthOriginServerAttested {
		t.Fatal("stored account does not contain the server-attested refresh-token chain")
	}
	info, err := os.Stat(stored.SourcePath(codexStore))
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("credential mode = %o, want 600", got)
	}
	loaded := ref.All()
	if len(loaded) != 1 || loaded[0].Email != "founders@manaflow.ai" {
		t.Fatalf("hot-loaded accounts = %+v", loaded)
	}
}

func TestAccountImportSeparatesCodexWorkspacesWithOneEmail(t *testing.T) {
	store := accounts.CodexStore{Dir: t.TempDir()}
	client := &http.Client{Transport: proxyRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		var request map[string]string
		if err := json.NewDecoder(req.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		rotated := proxyStoredOAuthAccount("shared@example.com", "rotated-"+request["refresh_token"], time.Now().Add(time.Hour))
		body, _ := json.Marshal(rotated.Auth.Tokens)
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(bytes.NewReader(body))}, nil
	})}
	ref := NewAccountRef(store, nil, client)
	ref.claudeStore = agentclaude.Store{Dir: t.TempDir()}
	handler := Server{AccountRef: ref, AdminToken: "secret"}.Handler()
	for _, workspace := range []string{"personal", "team", "team"} {
		account := proxyStoredOAuthAccount("shared@example.com", workspace, time.Now().Add(time.Hour))
		account.Auth.Tokens.AccountID = workspace
		body, _ := json.Marshal(accountImportRequest{Provider: accounts.ProviderCodex, Codex: &account})
		response := serveProtectedAccountImport(handler, body)
		if response.Code != http.StatusOK {
			t.Fatalf("%s import status=%d body=%s", workspace, response.Code, response.Body.String())
		}
	}
	all := ref.All()
	if len(all) != 2 || all[0].ID == all[1].ID || all[0].AccountID == all[1].AccountID {
		t.Fatal("server did not retain two independent routable workspaces")
	}
	for _, account := range all {
		if account.Email != "shared@example.com" {
			t.Fatalf("login email changed: %q", account.Email)
		}
	}
}

func TestAccountImportCanonicalizesDeclaredProviderAlias(t *testing.T) {
	resetConfiguredProviders(t)
	if err := ConfigureOpenAICompatibleProviders([]OpenAICompatibleProvider{{
		Name: "acme-relay", Aliases: []string{"acme"}, BaseURL: "https://relay.example/v1",
	}}); err != nil {
		t.Fatal(err)
	}
	codexStore := accounts.CodexStore{Dir: t.TempDir()}
	ref := NewAccountRef(codexStore, nil, nil)
	ref.claudeStore = agentclaude.Store{Dir: t.TempDir()}
	handler := Server{AccountRef: ref, AdminToken: "secret"}.Handler()
	payload, err := json.Marshal(map[string]any{
		"provider": "acme",
		"codex": accounts.StoredCodexAccount{
			Email: "acme:work",
			Auth:  accounts.CodexAuthFile{AuthMode: "apikey", OpenAIAPIKey: "test-provider-key"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	resp := serveProtectedAccountImport(handler, payload)
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", resp.Code, resp.Body.String())
	}
	stored, ok, err := codexStore.FindStored("acme-relay:work")
	if err != nil || !ok {
		t.Fatalf("canonical account found=%t err=%v", ok, err)
	}
	if stored.Provider != accounts.Provider("acme-relay") {
		t.Fatalf("stored provider = %q, want acme-relay", stored.Provider)
	}
	if _, aliasExists, err := codexStore.FindStored("acme:work"); err != nil || aliasExists {
		t.Fatalf("alias storage entry exists=%t err=%v", aliasExists, err)
	}
}

func TestAccountImportCanonicalizesSharedSubscriptionOwner(t *testing.T) {
	input := accountImportRequest{
		Provider: accounts.ProviderQwenAnthropic,
		Codex: &accounts.StoredCodexAccount{
			Email: "qwen-anthropic:work",
			Auth:  accounts.CodexAuthFile{AuthMode: "apikey", OpenAIAPIKey: "test-provider-key"},
		},
	}
	canonicalizeAccountImportProvider(&input)
	if input.Provider != accounts.ProviderQwenToken || input.Codex.Provider != accounts.ProviderQwenToken || input.Codex.Email != "qwen-token:work" {
		t.Fatalf("canonical input = %+v, want qwen-token owner", input)
	}
}

func TestAccountImportRejectsCodexIdentityMismatchWithoutWriting(t *testing.T) {
	codexStore := accounts.CodexStore{Dir: t.TempDir()}
	ref := NewAccountRef(codexStore, nil, nil)
	ref.claudeStore = agentclaude.Store{Dir: t.TempDir()}
	handler := Server{AccountRef: ref, AdminToken: "secret"}.Handler()
	account := proxyStoredOAuthAccount("token-owner@example.com", "fresh", time.Now().Add(time.Hour))
	account.Email = "attacker@example.com"
	payload, err := json.Marshal(map[string]any{"provider": "codex", "codex": account})
	if err != nil {
		t.Fatal(err)
	}

	beforeGeneration, err := readAccountDiskGeneration(codexStore.StoreDir())
	if err != nil {
		t.Fatal(err)
	}
	resp := serveProtectedAccountImport(handler, payload)
	if resp.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body = %s", resp.Code, resp.Body.String())
	}
	stored, err := codexStore.ListStored()
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) != 0 || len(ref.All()) != 0 {
		t.Fatalf("identity mismatch mutated account state: stored=%d loaded=%d", len(stored), len(ref.All()))
	}
	afterGeneration, err := readAccountDiskGeneration(codexStore.StoreDir())
	if err != nil {
		t.Fatal(err)
	}
	if afterGeneration != beforeGeneration {
		t.Fatal("identity attestation rejection published an account generation")
	}
}

func TestAccountImportServerAttestsCodexOAuthRegardlessOfCallerOrigin(t *testing.T) {
	codexStore := accounts.CodexStore{Dir: t.TempDir()}
	rotated := proxyStoredOAuthAccount("owner@example.com", "server", time.Now().Add(time.Hour))
	client := &http.Client{Transport: proxyRoundTripFunc(func(*http.Request) (*http.Response, error) {
		body, err := json.Marshal(map[string]string{
			"access_token":  rotated.Auth.Tokens.AccessToken,
			"refresh_token": rotated.Auth.Tokens.RefreshToken,
			"id_token":      rotated.Auth.Tokens.IDToken,
		})
		if err != nil {
			t.Fatal(err)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(bytes.NewReader(body)),
		}, nil
	})}
	ref := NewAccountRef(codexStore, nil, client)
	ref.claudeStore = agentclaude.Store{Dir: t.TempDir()}
	handler := Server{AccountRef: ref, AdminToken: "secret"}.Handler()
	account := proxyStoredOAuthAccount("owner@example.com", "fresh", time.Now().Add(time.Hour))
	account.OAuthCredentialOrigin = ""
	payload, err := json.Marshal(map[string]any{"provider": "codex", "codex": account})
	if err != nil {
		t.Fatal(err)
	}

	resp := serveProtectedAccountImport(handler, payload)
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body = %s", resp.Code, resp.Body.String())
	}
	stored, err := codexStore.ListStored()
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) != 1 || stored[0].OAuthCredentialOrigin != accounts.CodexOAuthOriginServerAttested ||
		stored[0].Auth.Tokens == nil || stored[0].Auth.Tokens.RefreshToken != "server-refresh" {
		t.Fatalf("caller origin was not replaced by server attestation: %#v", stored)
	}
}

func TestRejectedAccountImportDoesNotPublishGeneration(t *testing.T) {
	codexStore := accounts.CodexStore{Dir: t.TempDir()}
	ref := NewAccountRef(codexStore, nil, nil)
	ref.claudeStore = agentclaude.Store{Dir: t.TempDir()}
	handler := Server{AccountRef: ref, AdminToken: "secret"}.Handler()

	before, err := readAccountDiskGeneration(codexStore.StoreDir())
	if err != nil {
		t.Fatal(err)
	}
	response := serveProtectedAccountImport(handler, []byte(`{"provider":"unsupported"}`))
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body = %s", response.Code, response.Body.String())
	}
	after, err := readAccountDiskGeneration(codexStore.StoreDir())
	if err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Fatalf("rejected import published generation %q, previous %q", after, before)
	}
}

func TestCanceledAccountImportDoesNotWaitForInProcessTransaction(t *testing.T) {
	codexStore := accounts.CodexStore{Dir: t.TempDir()}
	ref := NewAccountRef(codexStore, nil, nil)
	ref.claudeStore = agentclaude.Store{Dir: t.TempDir()}
	handler := Server{AccountRef: ref, AdminToken: "secret"}.Handler()
	account := accounts.StoredCodexAccount{
		Email:    "apikey:canceled",
		Provider: accounts.ProviderCodex,
		Auth:     accounts.CodexAuthFile{AuthMode: "apikey", OpenAIAPIKey: "sk-canceled"},
	}
	payload, err := json.Marshal(map[string]any{"provider": "codex", "codex": account})
	if err != nil {
		t.Fatal(err)
	}

	ref.installMu.Lock()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	request := httptest.NewRequest(http.MethodPost, "/_subrouter/account-import", bytes.NewReader(payload)).WithContext(ctx)
	request.RemoteAddr = "100.64.0.20:4321"
	request.Header.Set("Authorization", "Bearer secret")
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		handler.ServeHTTP(response, request)
		close(done)
	}()

	select {
	case <-done:
		ref.installMu.Unlock()
	case <-time.After(200 * time.Millisecond):
		ref.installMu.Unlock()
		<-done
		t.Fatal("canceled import waited for the in-process transaction lock")
	}
	if response.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500, body = %s", response.Code, response.Body.String())
	}
	stored, err := codexStore.ListStored()
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) != 0 {
		t.Fatalf("canceled import mutated account state: %+v", stored)
	}
}

func TestAccountImportRejectsClaudeTerminalControlNameWithoutWriting(t *testing.T) {
	codexStore := accounts.CodexStore{Dir: t.TempDir()}
	claudeStore := agentclaude.Store{Dir: t.TempDir()}
	ref := NewAccountRef(codexStore, nil, nil)
	ref.claudeStore = claudeStore
	handler := Server{AccountRef: ref, AdminToken: "secret"}.Handler()
	payload := []byte(`{
		"provider":"claude",
		"claude":{
			"name":"founders\u001b[2J@example.com",
			"credential":{"accessToken":"access-secret","refreshToken":"refresh-secret"}
		}
	}`)

	resp := serveProtectedAccountImport(handler, payload)
	if resp.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body = %s", resp.Code, resp.Body.String())
	}
	if profiles := claudeStore.ListProfiles(); len(profiles) != 0 {
		t.Fatalf("terminal-control profile was persisted: %+v", profiles)
	}
}

func TestAccountImportRejectsStoredAccountTerminalControlIdentifierWithoutWriting(t *testing.T) {
	codexStore := accounts.CodexStore{Dir: t.TempDir()}
	ref := NewAccountRef(codexStore, nil, nil)
	ref.claudeStore = agentclaude.Store{Dir: t.TempDir()}
	handler := Server{AccountRef: ref, AdminToken: "secret"}.Handler()
	account := accounts.StoredCodexAccount{
		Email:    "apikey:founders\x1b[2J",
		Provider: accounts.ProviderCodex,
		Auth: accounts.CodexAuthFile{
			AuthMode:     "apikey",
			OpenAIAPIKey: "sk-test-secret",
		},
	}
	payload, err := json.Marshal(map[string]any{"provider": "codex", "codex": account})
	if err != nil {
		t.Fatal(err)
	}

	resp := serveProtectedAccountImport(handler, payload)
	if resp.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body = %s", resp.Code, resp.Body.String())
	}
	stored, err := codexStore.ListStored()
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) != 0 || len(ref.All()) != 0 {
		t.Fatalf("terminal-control identifier mutated account state: stored=%d loaded=%d", len(stored), len(ref.All()))
	}
}

func TestAccountImportRejectsStoredAccountIdentifierThatWouldCreateHiddenState(t *testing.T) {
	codexStore := accounts.CodexStore{Dir: t.TempDir()}
	ref := NewAccountRef(codexStore, nil, nil)
	ref.claudeStore = agentclaude.Store{Dir: t.TempDir()}
	handler := Server{AccountRef: ref, AdminToken: "secret"}.Handler()
	account := proxyStoredOAuthAccount(".hidden@example.com", "fresh", time.Now().Add(time.Hour))
	payload, err := json.Marshal(map[string]any{"provider": "codex", "codex": account})
	if err != nil {
		t.Fatal(err)
	}

	resp := serveProtectedAccountImport(handler, payload)
	if resp.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body = %s", resp.Code, resp.Body.String())
	}
	entries, err := os.ReadDir(codexStore.Dir)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if len(entries) != 0 || len(ref.All()) != 0 {
		t.Fatalf("hidden identifier mutated account state: entries=%d loaded=%d", len(entries), len(ref.All()))
	}
}

func TestAccountImportClaudePersistsAndHotLoadsWithoutReturningSecrets(t *testing.T) {
	codexStore := accounts.CodexStore{Dir: t.TempDir()}
	claudeStore := agentclaude.Store{Dir: t.TempDir()}
	ref := NewAccountRef(codexStore, nil, nil)
	ref.claudeStore = claudeStore
	handler := Server{AccountRef: ref, AdminToken: "secret"}.Handler()
	payload := []byte(`{
		"provider":"claude",
		"claude":{
			"name":"founders@manaflow.ai",
			"credential":{
				"accessToken":"claude-access-secret",
				"refreshToken":"claude-refresh-secret",
				"subscriptionType":"max",
				"expiresAt":4102444800000
			}
		}
	}`)

	resp := serveProtectedAccountImport(handler, payload)
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.Code, resp.Body.String())
	}
	if strings.Contains(resp.Body.String(), "claude-access-secret") || strings.Contains(resp.Body.String(), "claude-refresh-secret") {
		t.Fatalf("response leaked Claude credentials: %s", resp.Body.String())
	}
	profile, ok := claudeStore.FindProfile("founders@manaflow.ai")
	if !ok {
		t.Fatal("Claude profile was not registered")
	}
	credential, err := claudeStore.ReadCredential(t.Context(), claudeStore.ClaudeConfigDir(profile.Name))
	if err != nil || credential == nil {
		t.Fatalf("Claude credential = %v, err = %v", credential, err)
	}
	if credential.RefreshToken != "claude-refresh-secret" {
		t.Fatal("stored Claude profile does not contain the imported refresh-token chain")
	}
	loaded := ref.All()
	if len(loaded) != 1 || loaded[0].Provider != accounts.ProviderClaude || loaded[0].ID != "founders@manaflow.ai" {
		t.Fatalf("hot-loaded accounts = %+v", loaded)
	}
}

func TestAccountImportClaudeCaseVariantRotatesCanonicalProfile(t *testing.T) {
	codexStore := accounts.CodexStore{Dir: t.TempDir()}
	claudeStore := agentclaude.Store{Dir: t.TempDir()}
	ref := NewAccountRef(codexStore, nil, nil)
	ref.claudeStore = claudeStore
	handler := Server{AccountRef: ref, AdminToken: "secret"}.Handler()
	importCredential := func(name, refreshToken string) *httptest.ResponseRecorder {
		t.Helper()
		payload, err := json.Marshal(map[string]any{
			"provider": "claude",
			"claude": map[string]any{
				"name": name,
				"credential": map[string]any{
					"accessToken":  "access-secret",
					"refreshToken": refreshToken,
					"expiresAt":    time.Now().Add(time.Hour).UnixMilli(),
				},
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		return serveProtectedAccountImport(handler, payload)
	}

	const canonical = "founders@manaflow.ai"
	if response := importCredential(canonical, "refresh-first"); response.Code != http.StatusOK {
		t.Fatalf("initial import status = %d, body = %s", response.Code, response.Body.String())
	}
	response := importCredential("FOUNDERS@manaflow.ai", "refresh-rotated")
	if response.Code != http.StatusOK {
		t.Fatalf("rotation status = %d, body = %s", response.Code, response.Body.String())
	}
	var result struct {
		Account string `json:"account"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Account != canonical {
		t.Fatalf("rotation account = %q, want canonical %q", result.Account, canonical)
	}
	profiles := claudeStore.ListProfiles()
	if len(profiles) != 1 || profiles[0].Name != canonical {
		t.Fatalf("case-variant rotation created distinct profiles: %+v", profiles)
	}
	credential, err := claudeStore.ReadCredential(t.Context(), claudeStore.ClaudeConfigDir(canonical))
	if err != nil || credential == nil {
		t.Fatalf("credential = %v, err = %v", credential, err)
	}
	if credential.RefreshToken != "refresh-rotated" {
		t.Fatalf("refresh token was not rotated")
	}
}

func TestAccountImportSupportsEveryAPIKeyProvider(t *testing.T) {
	codexStore := accounts.CodexStore{Dir: t.TempDir()}
	ref := NewAccountRef(codexStore, nil, nil)
	ref.claudeStore = agentclaude.Store{Dir: t.TempDir()}
	handler := Server{AccountRef: ref, AdminToken: "secret"}.Handler()

	providers := []struct {
		provider accounts.Provider
		email    string
		key      string
	}{
		{provider: accounts.ProviderCodex, email: "apikey:openai", key: "sk-openai-test"},
		{provider: accounts.ProviderClaude, email: "claude:anthropic", key: "anthropic-test"},
	}
	for _, entry := range builtinKeyedProviders {
		name := string(entry.Provider)
		providers = append(providers, struct {
			provider accounts.Provider
			email    string
			key      string
		}{provider: entry.Provider, email: name + ":" + name, key: name + "-test"})
	}

	for _, tc := range providers {
		account := accounts.StoredCodexAccount{
			Email:    tc.email,
			Provider: tc.provider,
			Auth: accounts.CodexAuthFile{
				AuthMode:     "apikey",
				OpenAIAPIKey: tc.key,
			},
		}
		payload, err := json.Marshal(map[string]any{"provider": tc.provider, "codex": account})
		if err != nil {
			t.Fatal(err)
		}
		resp := serveProtectedAccountImport(handler, payload)
		if resp.Code != http.StatusOK {
			t.Fatalf("provider %s status = %d, body = %s", tc.provider, resp.Code, resp.Body.String())
		}
		if strings.Contains(resp.Body.String(), tc.key) {
			t.Fatalf("provider %s response leaked its API key", tc.provider)
		}
	}

	loaded := ref.All()
	if len(loaded) != len(providers) {
		t.Fatalf("loaded accounts = %d, want %d: %+v", len(loaded), len(providers), loaded)
	}
	for _, account := range loaded {
		if account.AuthMode != accounts.AuthModeAPIKey || account.Token == "" {
			t.Fatalf("invalid loaded API-key account: %+v", account)
		}
	}
}

func TestAccountImportRejectsStorageKeyAliasWithoutOverwriting(t *testing.T) {
	codexStore := accounts.CodexStore{Dir: t.TempDir()}
	ref := NewAccountRef(codexStore, nil, nil)
	ref.claudeStore = agentclaude.Store{Dir: t.TempDir()}
	handler := Server{AccountRef: ref, AdminToken: "secret"}.Handler()
	importAPIKey := func(email, key string) *httptest.ResponseRecorder {
		t.Helper()
		body, err := json.Marshal(map[string]any{
			"provider": "codex",
			"codex": accounts.StoredCodexAccount{
				Email:    email,
				Provider: accounts.ProviderCodex,
				Auth: accounts.CodexAuthFile{
					AuthMode:     "apikey",
					OpenAIAPIKey: key,
				},
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		return serveProtectedAccountImport(handler, body)
	}

	if response := importAPIKey("apikey:a+b", "sk-first"); response.Code != http.StatusOK {
		t.Fatalf("first import status = %d, body = %s", response.Code, response.Body.String())
	}
	if response := importAPIKey("apikey:a_b", "sk-second"); response.Code != http.StatusConflict {
		t.Fatalf("colliding import status = %d, want 409, body = %s", response.Code, response.Body.String())
	}
	stored, ok, err := codexStore.FindStored("apikey:a+b")
	if err != nil || !ok {
		t.Fatalf("original account = found:%v err:%v", ok, err)
	}
	if stored.Auth.OpenAIAPIKey != "sk-first" {
		t.Fatal("colliding import overwrote the original account")
	}
}

func TestConcurrentClaudeAccountImportsDoNotLoseRegistryEntries(t *testing.T) {
	codexStore := accounts.CodexStore{Dir: t.TempDir()}
	claudeStore := agentclaude.Store{Dir: t.TempDir()}
	ref := NewAccountRef(codexStore, nil, nil)
	ref.claudeStore = claudeStore
	handler := Server{AccountRef: ref, AdminToken: "secret"}.Handler()

	const count = 12
	var wg sync.WaitGroup
	errs := make(chan string, count)
	for index := 0; index < count; index++ {
		index := index
		wg.Add(1)
		go func() {
			defer wg.Done()
			payload, err := json.Marshal(map[string]any{
				"provider": "claude",
				"claude": map[string]any{
					"name": "profile" + string(rune('a'+index)),
					"credential": map[string]any{
						"accessToken":  "access-secret",
						"refreshToken": "refresh-secret",
						"expiresAt":    time.Now().Add(time.Hour).UnixMilli(),
					},
				},
			})
			if err != nil {
				errs <- err.Error()
				return
			}
			resp := serveProtectedAccountImport(handler, payload)
			if resp.Code != http.StatusOK {
				errs <- resp.Body.String()
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent import failed: %s", err)
	}
	if t.Failed() {
		return
	}
	if profiles := claudeStore.ListProfiles(); len(profiles) != count {
		t.Fatalf("profiles = %d, want %d: %+v", len(profiles), count, profiles)
	}
	if loaded := ref.All(); len(loaded) != count {
		t.Fatalf("loaded accounts = %d, want %d", len(loaded), count)
	}
}

func TestAccountImportReportsUnreadableClaudeRegistry(t *testing.T) {
	codexStore := accounts.CodexStore{Dir: t.TempDir()}
	claudeStore := agentclaude.Store{Dir: t.TempDir()}
	if err := os.MkdirAll(filepath.Dir(claudeStore.ProfilesPath()), 0o700); err != nil {
		t.Fatal(err)
	}
	registry := []byte("{not-json")
	if err := os.WriteFile(claudeStore.ProfilesPath(), registry, 0o600); err != nil {
		t.Fatal(err)
	}
	ref := NewAccountRef(codexStore, nil, nil)
	ref.claudeStore = claudeStore
	handler := Server{AccountRef: ref, AdminToken: "secret"}.Handler()
	payload, err := json.Marshal(map[string]any{
		"provider": accounts.ProviderCodex,
		"codex": accounts.StoredCodexAccount{
			Email: "apikey:new", Provider: accounts.ProviderCodex,
			Auth: accounts.CodexAuthFile{AuthMode: "apikey", OpenAIAPIKey: "sk-new"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	beforeGeneration, err := readAccountDiskGeneration(codexStore.StoreDir())
	if err != nil {
		t.Fatal(err)
	}
	response := serveProtectedAccountImport(handler, payload)
	if response.Code != http.StatusServiceUnavailable || !strings.Contains(response.Body.String(), "account inventory unavailable for claude") {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if strings.Contains(response.Body.String(), claudeStore.ProfilesPath()) || strings.Contains(response.Body.String(), "profiles.json") {
		t.Fatalf("inventory error leaked registry details: %s", response.Body.String())
	}
	afterGeneration, err := readAccountDiskGeneration(codexStore.StoreDir())
	if err != nil {
		t.Fatal(err)
	}
	if afterGeneration != beforeGeneration {
		t.Fatal("unreadable Claude registry rejection published a generation")
	}
	body, err := os.ReadFile(claudeStore.ProfilesPath())
	if err != nil || !bytes.Equal(body, registry) {
		t.Fatalf("Claude registry changed: body=%q err=%v", body, err)
	}
}

func TestAccountImportReportsUnavailableOAuthInventoryWithoutDetails(t *testing.T) {
	codexStore := accounts.CodexStore{Dir: t.TempDir()}
	ref := NewAccountRef(codexStore, nil, nil)
	ref.claudeStore = agentclaude.Store{Dir: t.TempDir()}
	secretDetail := filepath.Join(t.TempDir(), "private-inventory.json")
	ref.oauthSources = []OAuthAccountSource{&failingAccountInventorySource{
		stubOAuthSource: stubOAuthSource{provider: accounts.ProviderKimi},
		err:             fmt.Errorf("read %s: denied", secretDetail),
	}}
	handler := Server{AccountRef: ref, AdminToken: "secret"}.Handler()
	payload, err := json.Marshal(map[string]any{
		"provider": accounts.ProviderCodex,
		"codex": accounts.StoredCodexAccount{
			Email: "apikey:new", Provider: accounts.ProviderCodex,
			Auth: accounts.CodexAuthFile{AuthMode: "apikey", OpenAIAPIKey: "sk-new"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	response := serveProtectedAccountImport(handler, payload)
	if response.Code != http.StatusServiceUnavailable || !strings.Contains(response.Body.String(), "account inventory unavailable for kimi") {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if strings.Contains(response.Body.String(), secretDetail) || strings.Contains(response.Body.String(), "private-inventory.json") {
		t.Fatalf("inventory error leaked source details: %s", response.Body.String())
	}
}

func TestAccountImportBoundsAndStrictlyParsesCredentialBodies(t *testing.T) {
	ref := NewAccountRef(accounts.CodexStore{Dir: t.TempDir()}, nil, nil)
	ref.claudeStore = agentclaude.Store{Dir: t.TempDir()}
	handler := Server{AccountRef: ref, AdminToken: "secret"}.Handler()

	for _, tc := range []struct {
		name       string
		body       []byte
		wantStatus int
	}{
		{name: "oversized", body: bytes.Repeat([]byte("x"), 512<<10), wantStatus: http.StatusRequestEntityTooLarge},
		{name: "unknown field", body: []byte(`{"provider":"codex","surprise":"secret"}`), wantStatus: http.StatusBadRequest},
		{name: "trailing document", body: []byte(`{"provider":"codex"}{"provider":"claude"}`), wantStatus: http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := serveProtectedAccountImport(handler, tc.body)
			if resp.Code != tc.wantStatus {
				body, _ := io.ReadAll(resp.Result().Body)
				t.Fatalf("status = %d, want %d, body = %s", resp.Code, tc.wantStatus, body)
			}
		})
	}

	t.Run("oversized streaming body", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/_subrouter/account-import", bytes.NewReader(bytes.Repeat([]byte("x"), 512<<10)))
		req.ContentLength = -1
		req.RemoteAddr = "100.64.0.20:4321"
		req.Header.Set("Authorization", "Bearer secret")
		req.Header.Set("Content-Type", "application/json")
		resp := httptest.NewRecorder()

		handler.ServeHTTP(resp, req)

		if resp.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("status = %d, want %d, body = %s", resp.Code, http.StatusRequestEntityTooLarge, resp.Body.String())
		}
	})
}

func TestTenantOAuthImportChecksCapacityBeforeRotatingCredential(t *testing.T) {
	store := accounts.CodexStore{Dir: t.TempDir()}
	for i := 0; i < maxAccountImportAccounts; i++ {
		account := accounts.StoredCodexAccount{
			Email: fmt.Sprintf("apikey:existing-%03d", i), Provider: accounts.ProviderCodex,
			Auth: accounts.CodexAuthFile{AuthMode: "apikey", OpenAIAPIKey: "sk-existing"},
		}
		if err := store.SaveStored(account); err != nil {
			t.Fatal(err)
		}
	}
	var refreshCalls atomic.Int32
	client := &http.Client{Transport: proxyRoundTripFunc(func(*http.Request) (*http.Response, error) {
		refreshCalls.Add(1)
		return nil, errors.New("refresh must not be called")
	})}
	ref := NewAccountRef(store, nil, client)
	ref.claudeStore = agentclaude.Store{Dir: t.TempDir()}
	server := Server{AccountRef: ref, tenantAccountImportAuthorized: true}
	token := proxyTestCodexJWT("new@example.com", "new", time.Now().Add(time.Hour))
	_, err := server.installImportedAccount(context.Background(), accountImportRequest{
		Provider: accounts.ProviderCodex,
		Codex: &accounts.StoredCodexAccount{
			Email: "new@example.com", Provider: accounts.ProviderCodex,
			OAuthCredentialOrigin: accounts.CodexOAuthOriginInteractiveImport,
			Auth: accounts.CodexAuthFile{AuthMode: "chatgpt", Tokens: &accounts.CodexTokens{
				AccessToken: token, RefreshToken: "caller-refresh", IDToken: token, AccountID: "workspace:new@example.com",
			}},
		},
	})
	var capacityErr *accountImportCapacityError
	if !errors.As(err, &capacityErr) {
		t.Fatalf("import error = %v, want capacity error", err)
	}
	if refreshCalls.Load() != 0 {
		t.Fatalf("refresh calls = %d, want 0 before capacity rejection", refreshCalls.Load())
	}
}

func TestAccountImportCapsDistinctAccountsButAllowsCredentialRotation(t *testing.T) {
	const accountLimit = maxAccountImportAccounts
	codexStore := accounts.CodexStore{Dir: t.TempDir()}
	for index := 0; index < accountLimit; index++ {
		account := accounts.StoredCodexAccount{
			Email:    fmt.Sprintf("apikey:seed-%03d", index),
			Provider: accounts.ProviderCodex,
			Auth: accounts.CodexAuthFile{
				AuthMode:     "apikey",
				OpenAIAPIKey: fmt.Sprintf("sk-seed-%03d", index),
			},
		}
		if err := codexStore.SaveStored(account); err != nil {
			t.Fatal(err)
		}
	}
	ref := NewAccountRef(codexStore, nil, nil)
	ref.claudeStore = agentclaude.Store{Dir: t.TempDir()}
	handler := Server{AccountRef: ref, AdminToken: "secret"}.Handler()

	newAccount := accounts.StoredCodexAccount{
		Email:    "apikey:over-limit",
		Provider: accounts.ProviderCodex,
		Auth:     accounts.CodexAuthFile{AuthMode: "apikey", OpenAIAPIKey: "sk-over-limit"},
	}
	newPayload, err := json.Marshal(map[string]any{"provider": "codex", "codex": newAccount})
	if err != nil {
		t.Fatal(err)
	}
	beforeRejectedGeneration, err := readAccountDiskGeneration(codexStore.StoreDir())
	if err != nil {
		t.Fatal(err)
	}
	if resp := serveProtectedAccountImport(handler, newPayload); resp.Code != http.StatusInsufficientStorage {
		t.Fatalf("new account status = %d, want 507, body = %s", resp.Code, resp.Body.String())
	}
	afterRejectedGeneration, err := readAccountDiskGeneration(codexStore.StoreDir())
	if err != nil {
		t.Fatal(err)
	}
	if afterRejectedGeneration != beforeRejectedGeneration {
		t.Fatal("capacity rejection published an account generation")
	}

	existing := accounts.StoredCodexAccount{
		Email:    "apikey:seed-000",
		Provider: accounts.ProviderCodex,
		Auth:     accounts.CodexAuthFile{AuthMode: "apikey", OpenAIAPIKey: "sk-rotated"},
	}
	existingPayload, err := json.Marshal(map[string]any{"provider": "codex", "codex": existing})
	if err != nil {
		t.Fatal(err)
	}
	if resp := serveProtectedAccountImport(handler, existingPayload); resp.Code != http.StatusOK {
		t.Fatalf("credential rotation status = %d, want 200, body = %s", resp.Code, resp.Body.String())
	}

	caseVariant := existing
	caseVariant.Email = "apikey:SEED-000"
	caseVariant.Auth.OpenAIAPIKey = "sk-case-rotated"
	casePayload, err := json.Marshal(map[string]any{"provider": "codex", "codex": caseVariant})
	if err != nil {
		t.Fatal(err)
	}
	caseResponse := serveProtectedAccountImport(handler, casePayload)
	if caseResponse.Code != http.StatusOK {
		resp := caseResponse
		t.Fatalf("case-variant rotation status = %d, want 200, body = %s", resp.Code, resp.Body.String())
	}
	var caseResult struct {
		Account string `json:"account"`
	}
	if err := json.Unmarshal(caseResponse.Body.Bytes(), &caseResult); err != nil {
		t.Fatal(err)
	}
	if caseResult.Account != existing.Email {
		t.Fatalf("case-variant rotation account = %q, want canonical %q", caseResult.Account, existing.Email)
	}
	stored, err := codexStore.ListStored()
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) != accountLimit {
		t.Fatalf("case-variant rotation created a distinct pool entry: got %d accounts, want %d", len(stored), accountLimit)
	}
}

func TestAccountImportCapacityCountsEveryProviderFromDisk(t *testing.T) {
	root := t.TempDir()
	codexStore := accounts.CodexStore{Dir: filepath.Join(root, "codex", "accounts")}
	for index := 0; index < maxAccountImportAccounts-2; index++ {
		account := accounts.StoredCodexAccount{
			Email:    fmt.Sprintf("apikey:seed-%03d", index),
			Provider: accounts.ProviderCodex,
			Auth: accounts.CodexAuthFile{
				AuthMode: "apikey", OpenAIAPIKey: fmt.Sprintf("sk-seed-%03d", index),
			},
		}
		if err := codexStore.SaveStored(account); err != nil {
			t.Fatal(err)
		}
	}
	claudeStore := agentclaude.Store{Dir: filepath.Join(root, "claude")}
	if _, err := claudeStore.UpsertCredentialProfile("claude-work", agentclaude.CredentialInfo{
		AccessToken: "claude-access", RefreshToken: "claude-refresh",
	}); err != nil {
		t.Fatal(err)
	}
	kimiStore := agentkimi.Store{
		Path:       filepath.Join(root, "kimi", "cli.json"),
		ManagedDir: filepath.Join(root, "kimi", "managed"),
	}
	if _, err := kimiStore.SaveManagedCredential("kimi-work", agentkimi.CredentialInfo{
		AccessToken: "kimi-access", RefreshToken: "kimi-refresh",
		OAuthDeviceID: "device-kimi-work", ExpiresAt: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	ref := NewAccountRef(codexStore, nil, nil)
	ref.claudeStore = claudeStore
	ref.oauthSources = []OAuthAccountSource{kimiStore}
	handler := Server{AccountRef: ref, AdminToken: "secret"}.Handler()
	newAccount := accounts.StoredCodexAccount{
		Email:    "apikey:over-mixed-limit",
		Provider: accounts.ProviderCodex,
		Auth:     accounts.CodexAuthFile{AuthMode: "apikey", OpenAIAPIKey: "sk-over-mixed-limit"},
	}
	payload, err := json.Marshal(map[string]any{"provider": "codex", "codex": newAccount})
	if err != nil {
		t.Fatal(err)
	}
	beforeGeneration, err := readAccountDiskGeneration(codexStore.StoreDir())
	if err != nil {
		t.Fatal(err)
	}
	response := serveProtectedAccountImport(handler, payload)
	if response.Code != http.StatusInsufficientStorage {
		t.Fatalf("status = %d, want 507, body = %s", response.Code, response.Body.String())
	}
	afterGeneration, err := readAccountDiskGeneration(codexStore.StoreDir())
	if err != nil {
		t.Fatal(err)
	}
	if afterGeneration != beforeGeneration {
		t.Fatal("mixed-provider capacity rejection published an account generation")
	}
}

func TestAccountImportCapacityCountsUnreadableClaudeProfiles(t *testing.T) {
	root := t.TempDir()
	codexStore := accounts.CodexStore{Dir: filepath.Join(root, "codex", "accounts")}
	for index := 0; index < maxAccountImportAccounts-1; index++ {
		account := accounts.StoredCodexAccount{
			Email:    fmt.Sprintf("apikey:seed-%03d", index),
			Provider: accounts.ProviderCodex,
			Auth: accounts.CodexAuthFile{
				AuthMode: "apikey", OpenAIAPIKey: fmt.Sprintf("sk-seed-%03d", index),
			},
		}
		if err := codexStore.SaveStored(account); err != nil {
			t.Fatal(err)
		}
	}
	claudeStore := agentclaude.Store{Dir: filepath.Join(root, "claude")}
	if err := claudeStore.RegisterProfile("unreadable", "unreadable"); err != nil {
		t.Fatal(err)
	}
	if accounts, err := claudeStore.ListAccounts(t.Context()); err != nil || len(accounts) != 0 {
		t.Fatalf("unreadable Claude fixture accounts = %d, err = %v", len(accounts), err)
	}
	ref := NewAccountRef(codexStore, nil, nil)
	ref.claudeStore = claudeStore
	handler := Server{AccountRef: ref, AdminToken: "secret"}.Handler()
	newAccount := accounts.StoredCodexAccount{
		Email: "apikey:over-unreadable-limit", Provider: accounts.ProviderCodex,
		Auth: accounts.CodexAuthFile{AuthMode: "apikey", OpenAIAPIKey: "sk-over-unreadable-limit"},
	}
	payload, err := json.Marshal(map[string]any{"provider": "codex", "codex": newAccount})
	if err != nil {
		t.Fatal(err)
	}
	response := serveProtectedAccountImport(handler, payload)
	if response.Code != http.StatusInsufficientStorage {
		t.Fatalf("status = %d, want 507, body = %s", response.Code, response.Body.String())
	}
}

func TestAccountImportCapacityCountsUnroutableStoredAccounts(t *testing.T) {
	store := accounts.CodexStore{Dir: filepath.Join(t.TempDir(), "accounts")}
	for index := 0; index < maxAccountImportAccounts-1; index++ {
		account := accounts.StoredCodexAccount{
			Email:    fmt.Sprintf("apikey:seed-%03d", index),
			Provider: accounts.ProviderCodex,
			Auth: accounts.CodexAuthFile{
				AuthMode: "apikey", OpenAIAPIKey: fmt.Sprintf("sk-seed-%03d", index),
			},
		}
		if err := store.SaveStored(account); err != nil {
			t.Fatal(err)
		}
	}
	unroutable := accounts.StoredCodexAccount{
		Email:    "apikey:unroutable",
		Provider: accounts.ProviderCodex,
		Auth:     accounts.CodexAuthFile{AuthMode: "apikey"},
	}
	if err := store.SaveStored(unroutable); err != nil {
		t.Fatal(err)
	}
	routable, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(routable) != maxAccountImportAccounts-1 {
		t.Fatalf("routable fixture count = %d, want %d", len(routable), maxAccountImportAccounts-1)
	}
	ref := NewAccountRef(store, nil, nil)
	ref.claudeStore = agentclaude.Store{Dir: filepath.Join(t.TempDir(), "claude")}
	handler := Server{AccountRef: ref, AdminToken: "secret"}.Handler()
	newAccount := accounts.StoredCodexAccount{
		Email: "apikey:over-unroutable-limit", Provider: accounts.ProviderCodex,
		Auth: accounts.CodexAuthFile{AuthMode: "apikey", OpenAIAPIKey: "sk-over-unroutable-limit"},
	}
	payload, err := json.Marshal(map[string]any{"provider": "codex", "codex": newAccount})
	if err != nil {
		t.Fatal(err)
	}
	response := serveProtectedAccountImport(handler, payload)
	if response.Code != http.StatusInsufficientStorage {
		t.Fatalf("status = %d, want 507, body = %s", response.Code, response.Body.String())
	}
}

func TestPartialOAuthInventoryErrorDoesNotBlockOtherProviderImport(t *testing.T) {
	root := t.TempDir()
	codexStore := accounts.CodexStore{Dir: filepath.Join(root, "codex", "accounts")}
	claudeStore := agentclaude.Store{Dir: filepath.Join(root, "claude")}
	kimiStore := agentkimi.Store{
		Path:       filepath.Join(root, "kimi", "cli.json"),
		ManagedDir: filepath.Join(root, "kimi", "managed"),
	}
	credential := agentkimi.CredentialInfo{
		AccessToken: "access", RefreshToken: "refresh",
		OAuthDeviceID: "device", ExpiresAt: time.Now().Add(time.Hour),
	}
	if _, err := kimiStore.SaveManagedCredential("broken", credential); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(kimiStore.ManagedDir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("broken Kimi fixture entries = %d, err = %v", len(entries), err)
	}
	brokenPath := filepath.Join(kimiStore.ManagedDir, entries[0].Name())
	if _, err := kimiStore.SaveManagedCredential("healthy", credential); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(brokenPath, []byte("not-json"), 0o600); err != nil {
		t.Fatal(err)
	}
	partial, listErr := kimiStore.ListAccounts(t.Context())
	if listErr == nil || len(partial) != 1 {
		t.Fatalf("partial Kimi fixture accounts = %d, err = %v", len(partial), listErr)
	}
	ref := NewAccountRef(codexStore, nil, nil)
	ref.claudeStore = claudeStore
	ref.oauthSources = []OAuthAccountSource{kimiStore}
	handler := Server{AccountRef: ref, AdminToken: "secret"}.Handler()
	newAccount := accounts.StoredCodexAccount{
		Email: "apikey:new", Provider: accounts.ProviderCodex,
		Auth: accounts.CodexAuthFile{AuthMode: "apikey", OpenAIAPIKey: "sk-new"},
	}
	payload, err := json.Marshal(map[string]any{"provider": "codex", "codex": newAccount})
	if err != nil {
		t.Fatal(err)
	}
	response := serveProtectedAccountImport(handler, payload)
	if response.Code != http.StatusOK {
		t.Fatalf("partial OAuth inventory blocked Codex import: status = %d, body = %s", response.Code, response.Body.String())
	}
}

func TestUnreadableMultiAccountOAuthInventoryFailsClosed(t *testing.T) {
	root := t.TempDir()
	codexStore := accounts.CodexStore{Dir: filepath.Join(root, "codex", "accounts")}
	claudeStore := agentclaude.Store{Dir: filepath.Join(root, "claude")}
	kimiStore := agentkimi.Store{
		Path:       filepath.Join(root, "kimi", "cli.json"),
		ManagedDir: filepath.Join(root, "kimi", "managed"),
	}
	if err := os.MkdirAll(filepath.Dir(kimiStore.ManagedDir), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(kimiStore.ManagedDir, []byte("not-a-directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	ref := NewAccountRef(codexStore, nil, nil)
	ref.claudeStore = claudeStore
	ref.oauthSources = []OAuthAccountSource{kimiStore}
	handler := Server{AccountRef: ref, AdminToken: "secret"}.Handler()
	newAccount := accounts.StoredCodexAccount{
		Email: "apikey:new", Provider: accounts.ProviderCodex,
		Auth: accounts.CodexAuthFile{AuthMode: "apikey", OpenAIAPIKey: "sk-new"},
	}
	payload, err := json.Marshal(map[string]any{"provider": "codex", "codex": newAccount})
	if err != nil {
		t.Fatal(err)
	}
	beforeGeneration, err := readAccountDiskGeneration(codexStore.StoreDir())
	if err != nil {
		t.Fatal(err)
	}
	response := serveProtectedAccountImport(handler, payload)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503, body = %s", response.Code, response.Body.String())
	}
	if strings.Contains(response.Body.String(), kimiStore.ManagedDir) {
		t.Fatalf("Kimi inventory error leaked its path: %s", response.Body.String())
	}
	afterGeneration, err := readAccountDiskGeneration(codexStore.StoreDir())
	if err != nil {
		t.Fatal(err)
	}
	if afterGeneration != beforeGeneration {
		t.Fatal("unreadable durable inventory published an account generation")
	}
}

func TestUnreadableClaudeRegistryInventoryFailsClosed(t *testing.T) {
	root := t.TempDir()
	codexStore := accounts.CodexStore{Dir: filepath.Join(root, "codex", "accounts")}
	claudeStore := agentclaude.Store{Dir: filepath.Join(root, "claude")}
	if err := os.MkdirAll(claudeStore.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(claudeStore.ProfilesPath(), []byte("not-json"), 0o600); err != nil {
		t.Fatal(err)
	}
	ref := NewAccountRef(codexStore, nil, nil)
	ref.claudeStore = claudeStore
	handler := Server{AccountRef: ref, AdminToken: "secret"}.Handler()
	newAccount := accounts.StoredCodexAccount{
		Email: "apikey:new", Provider: accounts.ProviderCodex,
		Auth: accounts.CodexAuthFile{AuthMode: "apikey", OpenAIAPIKey: "sk-new"},
	}
	payload, err := json.Marshal(map[string]any{"provider": "codex", "codex": newAccount})
	if err != nil {
		t.Fatal(err)
	}
	beforeGeneration, err := readAccountDiskGeneration(codexStore.StoreDir())
	if err != nil {
		t.Fatal(err)
	}
	response := serveProtectedAccountImport(handler, payload)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503, body = %s", response.Code, response.Body.String())
	}
	if strings.Contains(response.Body.String(), claudeStore.ProfilesPath()) || strings.Contains(response.Body.String(), "profiles.json") {
		t.Fatalf("Claude inventory error leaked registry details: %s", response.Body.String())
	}
	afterGeneration, err := readAccountDiskGeneration(codexStore.StoreDir())
	if err != nil {
		t.Fatal(err)
	}
	if afterGeneration != beforeGeneration {
		t.Fatal("unreadable Claude registry published an account generation")
	}
}

func TestCompletedImportIsObservedByAnotherWorkerGeneration(t *testing.T) {
	codexStore := accounts.CodexStore{Dir: t.TempDir()}
	seed := accounts.StoredCodexAccount{
		Email:    "apikey:seed",
		Provider: accounts.ProviderCodex,
		Auth: accounts.CodexAuthFile{
			AuthMode:     "apikey",
			OpenAIAPIKey: "sk-seed",
		},
	}
	if err := codexStore.SaveStored(seed); err != nil {
		t.Fatal(err)
	}
	initial, err := codexStore.List()
	if err != nil {
		t.Fatal(err)
	}
	newWorkerRef := NewAccountRef(codexStore, initial, nil)
	newWorkerRef.claudeStore = agentclaude.Store{Dir: t.TempDir()}
	newWorkerRef.usageStatusAt = time.Now()
	newWorker := Server{AccountRef: newWorkerRef}
	retiringWorkerRef := NewAccountRef(codexStore, initial, nil)
	retiringWorkerRef.claudeStore = newWorkerRef.claudeStore
	retiringHandler := Server{AccountRef: retiringWorkerRef, AdminToken: "secret"}.Handler()

	imported := accounts.StoredCodexAccount{
		Email:    "apikey:imported",
		Provider: accounts.ProviderCodex,
		Auth: accounts.CodexAuthFile{
			AuthMode:     "apikey",
			OpenAIAPIKey: "sk-imported",
		},
	}
	payload, err := json.Marshal(map[string]any{"provider": "codex", "codex": imported})
	if err != nil {
		t.Fatal(err)
	}
	if resp := serveProtectedAccountImport(retiringHandler, payload); resp.Code != http.StatusOK {
		t.Fatalf("import status = %d, want 200, body = %s", resp.Code, resp.Body.String())
	}

	found := false
	for _, account := range newWorker.accountList() {
		if account.ID == imported.Email {
			found = true
		}
	}
	if !found {
		t.Fatalf("active worker did not observe completed import: %+v", newWorker.accountList())
	}
	if !newWorkerRef.usageStatusAt.IsZero() {
		t.Fatal("active worker retained usage cache after observing account generation")
	}
}

func serveProtectedAccountImport(handler http.Handler, body []byte) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/_subrouter/account-import", bytes.NewReader(body))
	req.RemoteAddr = "100.64.0.20:4321"
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("Content-Type", "application/json")
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, req)
	return resp
}
