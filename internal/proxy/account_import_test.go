package proxy

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
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
			handler := Server{AdminToken: tc.adminToken}.Handler()
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
