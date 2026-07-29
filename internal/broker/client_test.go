package broker

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/account"
)

func TestSaveConfigUsesOwnerOnlyPermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "cloud.json")
	config := Config{
		AccessToken:  "stack-access",
		RefreshToken: "stack-refresh",
		TeamID:       "team-a",
	}
	if err := SaveConfig(path, config); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("config mode = %o, want 600", got)
	}
	loaded, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if !loaded.Ready() {
		t.Fatalf("loaded config is not ready: %+v", loaded)
	}
}

func TestCredentialSourceMigratesTeamConfigAndAllowsExplicitLocal(t *testing.T) {
	config := Config{
		AccessToken:  "stack-access",
		RefreshToken: "stack-refresh",
		TeamID:       "team-a",
	}
	if got := config.EffectiveCredentialSource(); got != CredentialSourceTeam {
		t.Fatalf("pre-source team config = %q, want team", got)
	}
	if !config.TeamModeReady() {
		t.Fatal("pre-source team config should remain team-ready")
	}

	config.CredentialSource = CredentialSourceLocal
	if got := config.EffectiveCredentialSource(); got != CredentialSourceLocal {
		t.Fatalf("explicit source = %q, want local", got)
	}
	if config.TeamModeReady() {
		t.Fatal("explicit local source unexpectedly enabled team leases")
	}

	config.CredentialSource = CredentialSourceLegacy
	if !config.UsesLegacyServer() {
		t.Fatal("explicit legacy source was not preserved")
	}
}

func TestConfigRejectsUnknownCredentialSource(t *testing.T) {
	config := Config{
		BaseURL:          "https://cmux.com",
		CredentialSource: "surprise",
	}
	if err := config.Validate(); err == nil {
		t.Fatal("unknown credential source unexpectedly validated")
	}
}

func TestLeaseIsAccessOnlyCachedAndInvalidatedOnUnauthorized(t *testing.T) {
	var leaseRequests atomic.Int32
	var eventRequests atomic.Int32
	now := time.Now().UTC().Truncate(time.Second)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer stack-access" ||
			r.Header.Get("X-Stack-Refresh-Token") != "stack-refresh" ||
			r.Header.Get("X-Cmux-Team-ID") != "team-a" {
			http.Error(w, "bad auth", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/api/subrouter/leases":
			leaseRequests.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"teamId": "team-a",
				"lease": map[string]any{
					"leaseId":              "lease-a",
					"accountId":            "codex-a",
					"provider":             "codex",
					"authMode":             "oauth",
					"token":                "access-only",
					"providerAccountId":    "provider-a",
					"label":                "Alice",
					"credentialGeneration": 3,
					"issuedAt":             now.Format(time.RFC3339),
					"expiresAt":            now.Add(5 * time.Minute).Format(time.RFC3339),
					"credentialExpiresAt":  now.Add(time.Hour).Format(time.RFC3339),
				},
			})
		case strings.HasSuffix(r.URL.Path, "/events"):
			eventRequests.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := NewClient(Config{
		BaseURL:      server.URL,
		AccessToken:  "stack-access",
		RefreshToken: "stack-refresh",
		TeamID:       "team-a",
	})
	request := LeaseRequest{
		Provider:  account.ProviderCodex,
		AgentType: "codex",
		SessionID: "session-a",
	}
	first, err := client.Lease(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Lease(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if leaseRequests.Load() != 1 {
		t.Fatalf("lease requests = %d, want 1 cached request", leaseRequests.Load())
	}
	if first.Account.Token != "access-only" {
		t.Fatalf("leased token = %q", first.Account.Token)
	}

	if err := client.Report(
		context.Background(),
		first.ID,
		LeaseUnauthorized,
		http.StatusUnauthorized,
	); err != nil {
		t.Fatal(err)
	}
	if eventRequests.Load() != 1 {
		t.Fatalf("event requests = %d, want 1", eventRequests.Load())
	}
	if _, err := client.Lease(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if leaseRequests.Load() != 2 {
		t.Fatalf("lease requests after 401 = %d, want 2", leaseRequests.Load())
	}
}

func TestLeaseReplacementRetainsInFlightReverseCacheEntry(t *testing.T) {
	var requests atomic.Int32
	now := time.Now().UTC()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestNumber := requests.Add(1)
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body["requiredAuthMode"] != "oauth" {
			t.Fatalf("required auth mode = %v, want oauth", body["requiredAuthMode"])
		}
		expiresAt := now.Add(10 * time.Second)
		if requestNumber > 1 {
			expiresAt = now.Add(5 * time.Minute)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"teamId": "team-a",
			"lease": map[string]any{
				"leaseId":              fmt.Sprintf("lease-%d", requestNumber),
				"accountId":            "codex-a",
				"provider":             "codex",
				"authMode":             "oauth",
				"token":                "access-only",
				"label":                "Alice",
				"credentialGeneration": 1,
				"issuedAt":             now.Format(time.RFC3339),
				"expiresAt":            expiresAt.Format(time.RFC3339),
			},
		})
	}))
	defer server.Close()

	client := NewClient(Config{
		BaseURL:      server.URL,
		AccessToken:  "stack-access",
		RefreshToken: "stack-refresh",
		TeamID:       "team-a",
	})
	request := LeaseRequest{
		Provider:         account.ProviderCodex,
		RequiredAuthMode: account.AuthModeOAuth,
		AgentType:        "codex",
		SessionID:        "session-a",
	}
	first, err := client.Lease(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	second, err := client.Lease(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if first.ID == second.ID {
		t.Fatalf("stale lease %q was reused", first.ID)
	}
	if _, ok := client.leaseToKey[first.ID]; !ok {
		t.Fatalf("replaced lease %q lost its in-flight reverse cache entry", first.ID)
	}
	if got := len(client.leaseToKey); got != 2 {
		t.Fatalf("reverse cache entries = %d, want 2", got)
	}
}

func TestInvalidatingOneLeaseEvictsEveryCacheEntryForItsGeneration(
	t *testing.T,
) {
	var requests atomic.Int32
	now := time.Now().UTC()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requestNumber := requests.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"teamId": "team-a",
			"lease": map[string]any{
				"leaseId":              fmt.Sprintf("lease-%d", requestNumber),
				"accountId":            "codex-a",
				"provider":             "codex",
				"authMode":             "oauth",
				"token":                fmt.Sprintf("access-%d", requestNumber),
				"label":                "Alice",
				"credentialGeneration": 7,
				"issuedAt":             now.Format(time.RFC3339),
				"expiresAt":            now.Add(5 * time.Minute).Format(time.RFC3339),
			},
		})
	}))
	defer server.Close()

	client := NewClient(Config{
		BaseURL:      server.URL,
		AccessToken:  "stack-access",
		RefreshToken: "stack-refresh",
		TeamID:       "team-a",
	})
	requestA := LeaseRequest{
		Provider:  account.ProviderCodex,
		AgentType: "codex",
		SessionID: "session-a",
	}
	requestB := requestA
	requestB.SessionID = "session-b"
	first, err := client.Lease(context.Background(), requestA)
	if err != nil {
		t.Fatal(err)
	}
	second, err := client.Lease(context.Background(), requestB)
	if err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 2 {
		t.Fatalf("lease requests = %d, want 2", requests.Load())
	}

	client.InvalidateLease(first.ID)
	replacement, err := client.Lease(context.Background(), requestB)
	if err != nil {
		t.Fatal(err)
	}
	if replacement.ID == second.ID {
		t.Fatalf(
			"lease %q from rejected account generation remained cached",
			second.ID,
		)
	}
	if requests.Load() != 3 {
		t.Fatalf("lease requests after invalidation = %d, want 3", requests.Load())
	}
}

func TestLeaseRejectsRefreshTokensAtTheClientBoundary(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"teamId": "team-a",
			"lease": map[string]any{
				"leaseId":              "lease-a",
				"accountId":            "codex-a",
				"provider":             "codex",
				"authMode":             "oauth",
				"token":                "access-only",
				"label":                "Alice",
				"credentialGeneration": 1,
				"issuedAt":             now.Format(time.RFC3339),
				"expiresAt":            now.Add(5 * time.Minute).Format(time.RFC3339),
				"refreshToken":         "must-never-cross",
			},
		})
	}))
	defer server.Close()

	client := NewClient(Config{
		BaseURL:      server.URL,
		AccessToken:  "stack-access",
		RefreshToken: "stack-refresh",
		TeamID:       "team-a",
	})
	_, err := client.Lease(context.Background(), LeaseRequest{
		Provider:  account.ProviderCodex,
		AgentType: "codex",
		SessionID: "session-a",
	})
	if err == nil || !strings.Contains(err.Error(), "invalid response") {
		t.Fatalf("lease error = %v, want invalid response rejection", err)
	}
	if strings.Contains(err.Error(), "must-never-cross") {
		t.Fatalf("lease error leaked the refresh token: %v", err)
	}
}

func TestPollAuthPreservesAuthoritativeClientBinding(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/vault/cli/auth/poll" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status":       "approved",
			"client":       "subrouter",
			"accessToken":  "stack-access",
			"refreshToken": "stack-refresh",
		})
	}))
	defer server.Close()

	client := NewClient(Config{BaseURL: server.URL})
	poll, err := client.PollAuth(context.Background(), strings.Repeat("a", 64))
	if err != nil {
		t.Fatal(err)
	}
	if poll.Client != "subrouter" {
		t.Fatalf("poll client = %q, want subrouter", poll.Client)
	}
}

func TestClientRefusesRedirectsWithStackCredentials(t *testing.T) {
	var redirectedRequests atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		redirectedRequests.Add(1)
	}))
	defer target.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/collect", http.StatusTemporaryRedirect)
	}))
	defer source.Close()

	client := NewClient(Config{
		BaseURL:      source.URL,
		AccessToken:  "stack-access",
		RefreshToken: "stack-refresh",
		TeamID:       "team-a",
	})
	if _, err := client.ListAccounts(context.Background()); err == nil {
		t.Fatal("redirect unexpectedly succeeded")
	}
	if redirectedRequests.Load() != 0 {
		t.Fatalf("credentialed redirect reached target %d times", redirectedRequests.Load())
	}
}

func TestLogoutRevokesTheStoredStackSession(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Method != http.MethodPost || r.URL.Path != "/api/subrouter/logout" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer stack-access" ||
			r.Header.Get("X-Stack-Refresh-Token") != "stack-refresh" {
			http.Error(w, "bad auth", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()

	client := NewClient(Config{
		BaseURL:      server.URL,
		AccessToken:  "stack-access",
		RefreshToken: "stack-refresh",
	})
	if err := client.Logout(context.Background()); err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 1 {
		t.Fatalf("logout requests = %d, want 1", requests.Load())
	}
}

func TestConfigRequiresHTTPSOutsideLoopback(t *testing.T) {
	config := Config{BaseURL: "http://cmux.example"}
	if err := config.Validate(); err == nil {
		t.Fatal("insecure remote base URL unexpectedly accepted")
	}
	config.BaseURL = "http://127.0.0.1:3000"
	if err := config.Validate(); err != nil {
		t.Fatalf("loopback development URL rejected: %v", err)
	}
}

func TestAPIErrorNeverCopiesResponseSecrets(t *testing.T) {
	const secret = "sk-secret-that-must-not-be-logged"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"error":"`+secret+`"}`, http.StatusBadGateway)
	}))
	defer server.Close()

	client := NewClient(Config{
		BaseURL:      server.URL,
		AccessToken:  "stack-access",
		RefreshToken: "stack-refresh",
		TeamID:       "team-a",
	})
	_, err := client.ListAccounts(context.Background())
	if err == nil {
		t.Fatal("expected request failure")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("error leaked response secret: %v", err)
	}
}
