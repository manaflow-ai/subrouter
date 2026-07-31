package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	agentclaude "github.com/manaflow-ai/subrouter/internal/agents/claude"
)

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
	ref := NewAccountRef(codexStore, nil, nil)
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
	if stored.Auth.Tokens == nil || stored.Auth.Tokens.RefreshToken != "fresh-refresh" {
		t.Fatal("stored account does not contain the imported refresh-token chain")
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

func TestAccountImportSupportsEveryAPIKeyProvider(t *testing.T) {
	codexStore := accounts.CodexStore{Dir: t.TempDir()}
	ref := NewAccountRef(codexStore, nil, nil)
	ref.claudeStore = agentclaude.Store{Dir: t.TempDir()}
	handler := Server{AccountRef: ref, AdminToken: "secret"}.Handler()

	for _, tc := range []struct {
		provider accounts.Provider
		email    string
		key      string
	}{
		{provider: accounts.ProviderCodex, email: "apikey:openai", key: "sk-openai-test"},
		{provider: accounts.ProviderClaude, email: "claude:anthropic", key: "anthropic-test"},
		{provider: accounts.ProviderKimi, email: "kimi:kimi", key: "kimi-test"},
		{provider: accounts.ProviderZAI, email: "zai:zai", key: "zai-test"},
	} {
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
	if len(loaded) != 4 {
		t.Fatalf("loaded accounts = %d, want 4: %+v", len(loaded), loaded)
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
	if resp := serveProtectedAccountImport(handler, newPayload); resp.Code != http.StatusInsufficientStorage {
		t.Fatalf("new account status = %d, want 507, body = %s", resp.Code, resp.Body.String())
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

	for _, account := range newWorker.accountList() {
		if account.ID == imported.Email {
			return
		}
	}
	t.Fatalf("active worker did not observe completed import: %+v", newWorker.accountList())
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
