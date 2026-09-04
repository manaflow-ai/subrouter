package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	agentantigravity "github.com/manaflow-ai/subrouter/internal/agents/antigravity"
	agentkimi "github.com/manaflow-ai/subrouter/internal/agents/kimi"
)

func TestServerAccountImportNeverFollowsRedirects(t *testing.T) {
	var redirected atomic.Int32
	destination := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		redirected.Add(1)
	}))
	defer destination.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", destination.URL)
		w.WriteHeader(http.StatusTemporaryRedirect)
	}))
	defer source.Close()

	runner := srRunner{client: source.Client()}
	err := runner.ensureServerAccountImportAvailable(t.Context(), srServerConfig{
		Name:       "team",
		URL:        source.URL,
		AdminToken: "secret",
	})
	if err == nil || !strings.Contains(err.Error(), "307") {
		t.Fatalf("error = %v, want rejected redirect", err)
	}
	if redirected.Load() != 0 {
		t.Fatalf("credential request followed redirect %d time(s)", redirected.Load())
	}
}

func TestUploadAndRemoveServerAntigravityAccountUseProtectedImport(t *testing.T) {
	var imports, removals atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Header.Get("Authorization") != "Bearer import-secret" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if req.Method == http.MethodGet {
			_, _ = io.WriteString(w, `{"ok":true,"providers":["antigravity"]}`)
			return
		}
		var payload serverAccountImportRequest
		if err := json.NewDecoder(req.Body).Decode(&payload); err != nil || payload.Antigravity == nil || payload.Provider != accounts.ProviderAntigravity {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if payload.Antigravity.Remove {
			removals.Add(1)
			if payload.Antigravity.Credential.AccessToken != "" || payload.Antigravity.Credential.RefreshToken != "" {
				t.Error("Antigravity removal carried a credential")
			}
		} else {
			imports.Add(1)
			if payload.Antigravity.Credential.RefreshToken != "refresh-secret" {
				t.Error("Antigravity import omitted its refresh-token chain")
			}
		}
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer server.Close()
	runner := srRunner{client: server.Client()}
	target := srServerConfig{Name: "team", URL: server.URL, AccountImportToken: "import-secret"}
	credential := agentantigravity.CredentialInfo{
		AccessToken: "access-secret", RefreshToken: "refresh-secret", ExpiresAt: time.Now().Add(time.Hour),
		OAuthClientID: "client-id", OAuthClientSecret: "client-secret",
	}
	if err := runner.uploadServerAntigravityAccount(t.Context(), target, "work", credential); err != nil {
		t.Fatal(err)
	}
	if err := runner.removeServerAntigravityAccount(t.Context(), target, "work"); err != nil {
		t.Fatal(err)
	}
	if imports.Load() != 1 || removals.Load() != 1 {
		t.Fatalf("Antigravity mutations = imports:%d removals:%d", imports.Load(), removals.Load())
	}
}

func TestAntigravityRemoteImportPreservesOriginalGrantWithDiscoveredClient(t *testing.T) {
	original := agentantigravity.CredentialInfo{RefreshToken: "original-grant"}
	prepared := agentantigravity.CredentialInfo{
		AccessToken: "prepared-access", RefreshToken: "rotated-during-preparation",
		OAuthClientID: "discovered-client", OAuthClientSecret: "discovered-secret",
	}
	upload := antigravityRemoteImportCredential(prepared, original)
	if upload.RefreshToken != "original-grant" || upload.OAuthClientID != "discovered-client" || upload.OAuthClientSecret != "discovered-secret" {
		t.Fatalf("remote import credential = %+v", upload)
	}
}

func TestServerAccountImportUsesScopedTokenInsteadOfAdminToken(t *testing.T) {
	var authorization string
	serverHTTP := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		authorization = req.Header.Get("Authorization")
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer serverHTTP.Close()
	runner := srRunner{client: serverHTTP.Client()}
	server := srServerConfig{
		Name:               "team",
		URL:                serverHTTP.URL,
		AdminToken:         "admin-secret",
		AccountImportToken: "import-secret",
	}

	response, err := runner.doServerAccountImportRequest(t.Context(), server, http.MethodGet, nil)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if authorization != "Bearer import-secret" {
		t.Fatalf("Authorization = %q, want scoped account import token", authorization)
	}
}

func TestServerAccountImportFailureDoesNotEchoResponseOrCredential(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodGet {
			_, _ = io.WriteString(w, `{"ok":true,"providers":["kimi"]}`)
			return
		}
		http.Error(w, "provider rejected sk-access-secret", http.StatusBadRequest)
	}))
	defer server.Close()
	runner := srRunner{client: server.Client()}
	account := accounts.StoredCodexAccount{
		Email: "apikey:test",
		Auth: accounts.CodexAuthFile{
			AuthMode:     "apikey",
			OpenAIAPIKey: "sk-access-secret",
		},
	}

	err := runner.uploadServerAccount(t.Context(), srServerConfig{
		Name:       "team",
		URL:        server.URL,
		AdminToken: "secret",
	}, account)
	if err == nil {
		t.Fatal("expected account import failure")
	}
	if strings.Contains(err.Error(), "access-secret") || strings.Contains(err.Error(), "provider rejected") {
		t.Fatalf("account import error leaked a credential-bearing response: %v", err)
	}
}

func TestUploadAndRemoveServerKimiAccountUseProtectedImport(t *testing.T) {
	var imports, removals atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if got := req.Header.Get("Authorization"); got != "Bearer import-secret" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if req.Method == http.MethodGet {
			_, _ = io.WriteString(w, `{"ok":true,"providers":["kimi"]}`)
			return
		}
		var payload serverAccountImportRequest
		if err := json.NewDecoder(req.Body).Decode(&payload); err != nil || payload.Kimi == nil || payload.Provider != accounts.ProviderKimi {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if payload.Kimi.Remove {
			removals.Add(1)
			if payload.Kimi.Credential.AccessToken != "" || payload.Kimi.Credential.RefreshToken != "" {
				t.Error("Kimi removal carried a credential")
			}
		} else {
			imports.Add(1)
			if payload.Kimi.Credential.RefreshToken != "refresh-secret" {
				t.Error("Kimi import omitted its refresh-token chain")
			}
			if payload.Kimi.Credential.OAuthDeviceID != "authorized-device" {
				t.Error("Kimi import omitted its OAuth device identity")
			}
		}
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer server.Close()
	runner := srRunner{client: server.Client()}
	target := srServerConfig{Name: "team", URL: server.URL, AccountImportToken: "import-secret"}
	credential := agentkimi.CredentialInfo{
		AccessToken: "access-secret", RefreshToken: "refresh-secret", ExpiresAt: time.Now().Add(time.Hour),
		OAuthDeviceID: "authorized-device",
	}
	if err := runner.uploadServerKimiAccount(t.Context(), target, "work", credential); err != nil {
		t.Fatal(err)
	}
	if err := runner.removeServerKimiAccount(t.Context(), target, "work"); err != nil {
		t.Fatal(err)
	}
	if imports.Load() != 1 || removals.Load() != 1 {
		t.Fatalf("Kimi mutations = imports:%d removals:%d", imports.Load(), removals.Load())
	}
}

func TestRemoteKimiLoginPreflightsProviderBeforeDeviceAuthorization(t *testing.T) {
	var requests atomic.Int32
	serverHTTP := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		_, _ = io.WriteString(w, `{"ok":true,"providers":["codex"]}`)
	}))
	defer serverHTTP.Close()
	runner := srRunner{
		client: serverHTTP.Client(),
		out:    io.Discard,
	}
	err := runner.kimiRemoteLogin(t.Context(), srServerConfig{Name: "old-server", URL: serverHTTP.URL}, "work")
	if err == nil || !strings.Contains(err.Error(), "does not advertise kimi account import") {
		t.Fatalf("remote Kimi preflight error = %v", err)
	}
	if requests.Load() != 1 {
		t.Fatalf("remote Kimi login made %d requests; OAuth must not start after failed preflight", requests.Load())
	}
}

func TestServerAccountImportTransportFailureRedactsTenantKey(t *testing.T) {
	const tenantKey = "srt_secret-tenant-key"
	runner := srRunner{client: &http.Client{Transport: srRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		return nil, fmt.Errorf("dial %s failed", req.URL.String())
	})}}

	err := runner.ensureServerAccountImportAvailable(t.Context(), srServerConfig{
		Name:      "team",
		URL:       "https://subrouter.example.com",
		TenantKey: tenantKey,
	})
	if err == nil {
		t.Fatal("expected account-import transport failure")
	}
	if strings.Contains(err.Error(), tenantKey) {
		t.Fatalf("transport error leaked tenant key: %v", err)
	}
}

func TestPlainHTTPAccountImportBypassesConfiguredProxy(t *testing.T) {
	var proxyRequests atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		proxyRequests.Add(1)
		http.Error(w, "proxy must not receive credentials", http.StatusBadGateway)
	}))
	defer proxy.Close()
	var targetRequests atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		targetRequests.Add(1)
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer target.Close()
	proxyURL, err := url.Parse(proxy.URL)
	if err != nil {
		t.Fatal(err)
	}
	runner := srRunner{client: &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)}}}
	account := accounts.StoredCodexAccount{
		Email: "apikey:test",
		Auth: accounts.CodexAuthFile{
			AuthMode:     "apikey",
			OpenAIAPIKey: "sk-test",
		},
	}

	if err := runner.uploadServerAccount(t.Context(), srServerConfig{
		Name:       "team",
		URL:        target.URL,
		AdminToken: "secret",
	}, account); err != nil {
		t.Fatal(err)
	}
	if proxyRequests.Load() != 0 {
		t.Fatalf("plaintext credential import used a configured HTTP proxy %d time(s)", proxyRequests.Load())
	}
	if targetRequests.Load() != 2 {
		t.Fatalf("target requests = %d, want preflight and POST", targetRequests.Load())
	}
}

func TestSecuredServerRequestClientDisablesHTTPSProxy(t *testing.T) {
	configured := &http.Transport{Proxy: http.ProxyFromEnvironment}
	secured, err := securedServerRequestClient(&http.Client{Transport: configured}, "https://subrouter.example.com/t/secret")
	if err != nil {
		t.Fatal(err)
	}
	transport, ok := secured.Transport.(*http.Transport)
	if !ok || transport.Proxy != nil {
		t.Fatalf("secured HTTPS transport retained a proxy: %#v", secured.Transport)
	}
	if configured.Proxy == nil {
		t.Fatal("secured client mutated the caller's transport")
	}
}

func TestSecuredServerRequestClientRejectsCustomTransport(t *testing.T) {
	client := &http.Client{Transport: srRoundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("must not run")
	})}
	for _, rawURL := range []string{"http://127.0.0.1:31415/t/secret", "http://100.64.0.10/t/secret", "https://subrouter.example.com/t/secret"} {
		if _, err := securedServerRequestClient(client, rawURL); err == nil || !strings.Contains(err.Error(), "pinnable") {
			t.Errorf("custom transport error for %s = %v, want fail-closed", rawURL, err)
		}
	}
}

func TestUnsafeHTTPServerFailsBeforeCodexOAuth(t *testing.T) {
	fake := &recordingSRCommandRunner{loginAuth: testCodexAuth("founders@manaflow.ai", "fresh")}
	var output bytes.Buffer
	runner := srRunner{store: accounts.CodexStore{Dir: t.TempDir()}, out: &output, errOut: &output, cmd: fake}

	err := runner.serverLoginOne(context.Background(), srServerConfig{
		Name:       "unsafe",
		URL:        "http://192.168.1.10:31415",
		AdminToken: "secret",
	}, false, "")
	if err == nil || !strings.Contains(err.Error(), "must use HTTPS") {
		t.Fatalf("error = %v, want unsafe plaintext rejection", err)
	}
	if fake.hasCommandPrefix("codex", "login") {
		t.Fatalf("Codex OAuth started for unsafe upload destination: %#v", fake.commands)
	}
}
