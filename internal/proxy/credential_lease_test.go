package proxy

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/internal/tenant"
	"github.com/manaflow-ai/subrouter/selectacct"
	"github.com/manaflow-ai/subrouter/session"
)

func TestTenantCredentialLeaseReturnsAccessOnlyRefreshedCredential(t *testing.T) {
	t.Parallel()

	stateDir := t.TempDir()
	registry := tenant.NewRegistry(stateDir)
	created, key, err := registry.Create("team")
	if err != nil {
		t.Fatal(err)
	}
	accountDir := filepath.Join(registry.Dir(created.ID), "codex", "accounts")
	if err := os.MkdirAll(accountDir, 0o700); err != nil {
		t.Fatal(err)
	}
	body := `{
		"email":"user@example.com",
		"provider":"codex",
		"addedAt":"2026-01-01T00:00:00Z",
		"auth":{
			"auth_mode":"chatgpt",
			"tokens":{
				"access_token":"stored-access",
				"refresh_token":"refresh-secret",
				"id_token":"id-secret",
				"account_id":"provider-account"
			}
		}
	}`
	if err := os.WriteFile(
		filepath.Join(accountDir, "user_at_example.com.json"),
		[]byte(body),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	legacySessions, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	var refreshes atomic.Int32
	base := Server{
		Sessions: legacySessions, Scheduler: selectacct.NewScheduler(nil),
		MaxBodyBytes: 64 << 10,
		RefreshAccountFn: func(_ context.Context, account accounts.Account) (accounts.Account, error) {
			refreshes.Add(1)
			account.Token = "refreshed-access"
			return account, nil
		},
	}
	multi := &MultiTenant{Base: base, Registry: registry, Enabled: true}
	handler := multi.Handler(base.Handler())

	request := httptest.NewRequest(
		http.MethodPost,
		"/t/"+key+"/_subrouter/leases",
		strings.NewReader(`{
			"provider":"codex",
			"agentType":"codex",
			"sessionId":"session-1",
			"requiredAuthMode":"oauth"
		}`),
	)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if refreshes.Load() != 1 {
		t.Fatalf("central refreshes = %d, want 1", refreshes.Load())
	}
	raw := response.Body.String()
	for _, forbidden := range []string{
		"refresh-secret", "id-secret", "refreshToken", "idToken", "apiKey",
	} {
		if strings.Contains(raw, forbidden) {
			t.Fatalf("credential lease leaked %q: %s", forbidden, raw)
		}
	}
	var envelope struct {
		TeamID string `json:"teamId"`
		Lease  struct {
			LeaseID              string `json:"leaseId"`
			AccountID            string `json:"accountId"`
			Provider             string `json:"provider"`
			AuthMode             string `json:"authMode"`
			Token                string `json:"token"`
			CredentialGeneration int    `json:"credentialGeneration"`
			IssuedAt             string `json:"issuedAt"`
			ExpiresAt            string `json:"expiresAt"`
		} `json:"lease"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.TeamID != created.ID ||
		envelope.Lease.AccountID != "user@example.com" ||
		envelope.Lease.Provider != "codex" ||
		envelope.Lease.AuthMode != "oauth" ||
		envelope.Lease.Token != "refreshed-access" ||
		envelope.Lease.LeaseID == "" ||
		envelope.Lease.CredentialGeneration == 0 {
		t.Fatalf("lease = %#v", envelope)
	}
	issuedAt, err := time.Parse(time.RFC3339Nano, envelope.Lease.IssuedAt)
	if err != nil {
		t.Fatal(err)
	}
	expiresAt, err := time.Parse(time.RFC3339Nano, envelope.Lease.ExpiresAt)
	if err != nil {
		t.Fatal(err)
	}
	if ttl := expiresAt.Sub(issuedAt); ttl <= 0 || ttl > 5*time.Minute {
		t.Fatalf("lease TTL = %s", ttl)
	}

	report := httptest.NewRequest(
		http.MethodPost,
		"/t/"+key+"/_subrouter/leases/"+
			envelope.Lease.LeaseID+"/events",
		strings.NewReader(`{"outcome":"success","statusCode":200}`),
	)
	reportResponse := httptest.NewRecorder()
	handler.ServeHTTP(reportResponse, report)
	if reportResponse.Code != http.StatusNoContent {
		t.Fatalf(
			"report status = %d, body = %s",
			reportResponse.Code,
			reportResponse.Body.String(),
		)
	}
}

func TestTenantCredentialLeaseRejectsUnknownOrMalformedRequests(t *testing.T) {
	t.Parallel()

	registry, handler, _ := newMultiTenantFixture(t)
	_, key, err := registry.Create("team")
	if err != nil {
		t.Fatal(err)
	}
	for name, testCase := range map[string]struct {
		path string
		body string
		want int
	}{
		"unknown tenant": {
			path: "/t/srt_00000000000000000000000000000000/_subrouter/leases",
			body: `{"provider":"codex","sessionId":"s"}`,
			want: http.StatusUnauthorized,
		},
		"unknown field": {
			path: "/t/" + key + "/_subrouter/leases",
			body: `{"provider":"codex","sessionId":"s","refreshToken":"steal"}`,
			want: http.StatusBadRequest,
		},
		"unknown lease report": {
			path: "/t/" + key + "/_subrouter/leases/lease_missing/events",
			body: `{"outcome":"success","statusCode":200}`,
			want: http.StatusNotFound,
		},
	} {
		t.Run(name, func(t *testing.T) {
			request := httptest.NewRequest(
				http.MethodPost,
				testCase.path,
				strings.NewReader(testCase.body),
			)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != testCase.want {
				t.Fatalf(
					"status = %d, want %d, body = %s",
					response.Code,
					testCase.want,
					response.Body.String(),
				)
			}
		})
	}
}

func TestTenantCredentialLeaseHidesSessionPersistenceFailures(t *testing.T) {
	root := t.TempDir()
	sessionDir := filepath.Join(root, "session-state")
	sessions, err := session.NewStore(filepath.Join(sessionDir, "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(sessionDir, "sessions.json.lock")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(sessionDir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sessionDir, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}

	server := Server{
		Accounts: []accounts.Account{{
			ID: "codex-a", Provider: accounts.ProviderCodex,
			AuthMode: accounts.AuthModeOAuth, Token: "access-only",
		}},
		Sessions:  sessions,
		Scheduler: selectacct.NewScheduler(nil),
	}
	request := httptest.NewRequest(
		http.MethodPost,
		"/_subrouter/leases",
		strings.NewReader(`{"provider":"codex","agentType":"codex","sessionId":"session-a"}`),
	)
	response := httptest.NewRecorder()
	newTenantCredentialLeaseStore().handleIssue(
		&server,
		tenant.Tenant{ID: "team"},
		response,
		request,
	)

	if response.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500, body = %s", response.Code, response.Body.String())
	}
	if got := strings.TrimSpace(response.Body.String()); got != "credential lease unavailable" {
		t.Fatalf("response exposed persistence details: %q", got)
	}
}
