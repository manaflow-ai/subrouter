package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	agentqwen "github.com/manaflow-ai/subrouter/internal/agents/qwen"
)

type qwenConsoleRoundTripFunc func(*http.Request) (*http.Response, error)

func (f qwenConsoleRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestQwenConsoleCredentialCanBeSyncedToSelectedServer(t *testing.T) {
	t.Setenv("SUBROUTER_STATE_DIR", t.TempDir())
	server := Server{
		AdminToken: "admin-secret",
		Accounts: []accounts.Account{{
			ID:       "qwen-token:work",
			Provider: accounts.ProviderQwenToken,
			AuthMode: accounts.AuthModeAPIKey,
			Token:    "model-secret",
		}},
	}
	body, err := json.Marshal(map[string]any{
		"account_id": "qwen-token:work",
		"credential": agentqwen.ConsoleCredential{
			AccessToken:   "console-secret",
			ConsoleRegion: "ap-southeast-1",
			ConsoleSite:   "international",
			Account:       "person@example.com",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/_subrouter/qwen-console", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer admin-secret")
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	credential, err := agentqwen.ExportConsoleCredential("qwen-token:work")
	if err != nil {
		t.Fatal(err)
	}
	if credential.AccessToken != "console-secret" || credential.Account != "person@example.com" {
		t.Fatalf("saved credential = %+v", credential)
	}
}

func TestQwenConsoleCredentialSyncUsesAccountStoreScope(t *testing.T) {
	accountID := "qwen-token:shared-label"
	newServer := func(storeRoot string) (Server, string) {
		store := accounts.CodexStore{Dir: filepath.Join(storeRoot, "codex", "accounts")}
		stored := accounts.StoredCodexAccount{
			Email:    accountID,
			Provider: accounts.ProviderQwenToken,
			Auth: accounts.CodexAuthFile{
				AuthMode:     "apikey",
				OpenAIAPIKey: "model-secret",
			},
		}
		if err := store.SaveStored(stored); err != nil {
			t.Fatal(err)
		}
		account, ok := stored.Account(stored.SourcePath(store))
		if !ok {
			t.Fatal("stored Qwen account was not loadable")
		}
		ref := NewAccountRef(store, []accounts.Account{account}, nil)
		return Server{AdminToken: "admin-secret", AccountRef: ref}, ref.qwenRoot()
	}

	root := t.TempDir()
	serverA, consoleRootA := newServer(filepath.Join(root, "tenant-a"))
	serverB, consoleRootB := newServer(filepath.Join(root, "tenant-b"))
	for _, tc := range []struct {
		server Server
		token  string
		label  string
	}{
		{server: serverA, token: "tenant-a-token", label: "a@example.com"},
		{server: serverB, token: "tenant-b-token", label: "b@example.com"},
	} {
		body, err := json.Marshal(map[string]any{
			"account_id": accountID,
			"credential": agentqwen.ConsoleCredential{AccessToken: tc.token, Account: tc.label},
		})
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(http.MethodPost, "/_subrouter/qwen-console", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer admin-secret")
		rec := httptest.NewRecorder()
		tc.server.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusNoContent {
			t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
		}
	}
	credentialA, err := agentqwen.ExportConsoleCredentialIn(consoleRootA, accountID)
	if err != nil {
		t.Fatal(err)
	}
	credentialB, err := agentqwen.ExportConsoleCredentialIn(consoleRootB, accountID)
	if err != nil {
		t.Fatal(err)
	}
	if credentialA.AccessToken != "tenant-a-token" || credentialA.Account != "a@example.com" {
		t.Fatalf("tenant A credential = %+v", credentialA)
	}
	if credentialB.AccessToken != "tenant-b-token" || credentialB.Account != "b@example.com" {
		t.Fatalf("tenant B credential = %+v", credentialB)
	}
}

func TestQwenConsoleCredentialSyncRejectsUnknownAccount(t *testing.T) {
	t.Setenv("SUBROUTER_STATE_DIR", t.TempDir())
	server := Server{AdminToken: "admin-secret"}
	body := []byte(`{"account_id":"qwen-token:missing","credential":{"access_token":"console-secret"}}`)
	req := httptest.NewRequest(http.MethodPost, "/_subrouter/qwen-console", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer admin-secret")
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
}

func TestQwenConsoleCredentialSyncRejectsTerminalControlInLabel(t *testing.T) {
	t.Setenv("SUBROUTER_STATE_DIR", t.TempDir())
	server := Server{
		AdminToken: "admin-secret",
		Accounts: []accounts.Account{{
			ID:       "qwen-token:work",
			Provider: accounts.ProviderQwenToken,
			AuthMode: accounts.AuthModeAPIKey,
			Token:    "model-secret",
		}},
	}
	body := []byte("{\"account_id\":\"qwen-token:work\",\"credential\":{\"access_token\":\"console-secret\",\"account\":\"unsafe\\u001b[31m\"}}")
	req := httptest.NewRequest(http.MethodPost, "/_subrouter/qwen-console", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer admin-secret")
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	if _, err := agentqwen.ExportConsoleCredential("qwen-token:work"); err == nil {
		t.Fatal("unsafe Qwen console credential was persisted")
	}
}

func TestQwenUsageStatusPreservesQuotaWhenPlanLookupFails(t *testing.T) {
	store := accounts.CodexStore{Dir: filepath.Join(t.TempDir(), "codex", "accounts")}
	stored := accounts.StoredCodexAccount{
		Email:    "qwen-token:work",
		Provider: accounts.ProviderQwenToken,
		Auth: accounts.CodexAuthFile{
			AuthMode:     "apikey",
			OpenAIAPIKey: "model-secret",
		},
	}
	if err := store.SaveStored(stored); err != nil {
		t.Fatal(err)
	}
	account, ok := stored.Account(stored.SourcePath(store))
	if !ok {
		t.Fatal("stored Qwen account was not loadable")
	}
	reset := time.Now().Add(time.Hour).UnixMilli()
	includeWindow := true
	client := &http.Client{Transport: qwenConsoleRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		if strings.Contains(req.URL.Query().Get("api"), "/usage") {
			body := `{"data":{"data":{"per1WeekResetTime":` + fmt.Sprint(reset) + `}}}`
			if includeWindow {
				body = `{"data":{"data":{"per1WeekPercentage":0.25,"per1WeekResetTime":` + fmt.Sprint(reset) + `}}}`
			}
			return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
		}
		return &http.Response{StatusCode: http.StatusBadGateway, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("unavailable"))}, nil
	})}
	ref := NewAccountRef(store, []accounts.Account{account}, client)
	if err := agentqwen.SaveConsoleCredentialIn(ref.qwenRoot(), stored.Email, agentqwen.ConsoleCredential{
		AccessToken: "console-secret",
		Account:     "saved-account@example.test",
	}); err != nil {
		t.Fatal(err)
	}
	statuses := ref.UsageStatuses(t.Context())
	var status *AccountUsageStatus
	for i := range statuses {
		if statuses[i].ID == stored.Email {
			status = &statuses[i]
			break
		}
	}
	if status == nil || status.QuotaStatus != "partial" || len(status.Windows) != 1 || status.Error == "" ||
		status.AccountIdentity != "saved-account@example.test" {
		t.Fatalf("Qwen status = %+v; all statuses = %+v", status, statuses)
	}
	includeWindow = false
	ref.InvalidateUsageStatusCache()
	statuses = ref.UsageStatuses(t.Context())
	for i := range statuses {
		if statuses[i].ID == stored.Email {
			status = &statuses[i]
			break
		}
	}
	if status == nil || !status.QuotaUsageKnown || len(status.Windows) != 0 {
		t.Fatalf("successful empty quota response restored stale windows: %+v", status)
	}
}

func TestQwenUsageStatusExplainsExpiredConsoleLoginOnce(t *testing.T) {
	store := accounts.CodexStore{Dir: filepath.Join(t.TempDir(), "codex", "accounts")}
	stored := accounts.StoredCodexAccount{
		Email: "qwen-token:work", Provider: accounts.ProviderQwenToken,
		Auth: accounts.CodexAuthFile{AuthMode: "apikey", OpenAIAPIKey: "model-secret"},
	}
	if err := store.SaveStored(stored); err != nil {
		t.Fatal(err)
	}
	account, ok := stored.Account(stored.SourcePath(store))
	if !ok {
		t.Fatal("stored Qwen account was not loadable")
	}
	client := &http.Client{Transport: qwenConsoleRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		body := `{"code":"BailianGateway.Login.NotLogined","message":"login expired"}`
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
	})}
	ref := NewAccountRef(store, []accounts.Account{account}, client)
	if err := agentqwen.SaveConsoleCredentialIn(ref.qwenRoot(), stored.Email, agentqwen.ConsoleCredential{
		AccessToken: "expired-console-token", Account: "saved-account@example.test",
	}); err != nil {
		t.Fatal(err)
	}
	statuses := ref.UsageStatuses(t.Context())
	for _, status := range statuses {
		if status.ID != stored.Email {
			continue
		}
		if status.QuotaStatus != "login needed" || status.AccountIdentity != "saved-account@example.test" ||
			!strings.Contains(status.Error, "sr qwen login 'qwen-token:work'") || strings.Count(status.Error, "login needed") != 1 {
			t.Fatalf("expired Qwen status = %+v", status)
		}
		return
	}
	t.Fatal("Qwen status row was missing")
}
