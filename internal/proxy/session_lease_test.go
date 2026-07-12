package proxy

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/internal/selectacct"
	"github.com/manaflow-ai/subrouter/internal/session"
)

func TestSessionLeaseRequiresConfiguredAdminTokenForNetworkCaller(t *testing.T) {
	handler := Server{AdminToken: "expected-admin-token"}.Handler()
	for _, test := range []struct {
		name  string
		token string
	}{
		{name: "missing"},
		{name: "wrong", token: "wrong-admin-token"},
	} {
		t.Run(test.name, func(t *testing.T) {
			req := newSessionLeaseRequest(t, "codex", "gpt-5.4")
			req.RemoteAddr = "100.64.0.2:12345"
			if test.token != "" {
				req.Header.Set("Authorization", "Bearer "+test.token)
			}
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, req)
			if recorder.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401; body = %s", recorder.Code, recorder.Body.String())
			}
		})
	}

	// A network listener with no configured token must stay closed rather than
	// inheriting the permissive legacy admin behavior.
	handler = Server{}.Handler()
	req := newSessionLeaseRequest(t, "codex", "gpt-5.4")
	req.RemoteAddr = "100.64.0.2:12345"
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("status without configured token = %d, want 401", recorder.Code)
	}
}

func TestSessionLeaseCreationRejectsNewWorkWhileDraining(t *testing.T) {
	lifecycle := NewLifecycle()
	lifecycle.Drain()
	sessionStore := newSessionStore(t)
	handler := Server{
		Accounts: []accounts.Account{{
			ID:        "oauth@example.com",
			Provider:  accounts.ProviderCodex,
			AuthMode:  accounts.AuthModeOAuth,
			Token:     "oauth-token",
			AccountID: "chatgpt-account",
		}},
		Sessions:      sessionStore,
		Scheduler:     selectacct.NewScheduler(nil),
		sessionLeases: newSessionLeaseStore(),
		AdminToken:    "service-admin-token",
		Lifecycle:     lifecycle,
		MaxBodyBytes:  1024,
	}.Handler()
	req := newSessionLeaseRequest(t, "codex", "gpt-5.4")
	req.RemoteAddr = "100.64.0.2:12345"
	req.Header.Set("Authorization", "Bearer service-admin-token")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("draining lease status = %d, want 503; body = %s", recorder.Code, recorder.Body.String())
	}
	if _, exists := sessionStore.Get(agentTypeForProviderSession("pi", accounts.ProviderCodex), "agent-session-1"); exists {
		t.Fatal("draining lease creation persisted a session assignment")
	}
}

func TestCodexOAuthSessionLeaseIsIdempotentAndBrokersWithoutCredentialDisclosure(t *testing.T) {
	var upstreamCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		if r.URL.Path != "/backend-api/codex/responses" {
			t.Fatalf("upstream path = %q", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer oauth-access-secret" {
			t.Fatalf("Authorization = %q, want selected OAuth access token", got)
		}
		if got := r.Header.Get("ChatGPT-Account-ID"); got != "chatgpt-account-1" {
			t.Fatalf("ChatGPT-Account-ID = %q", got)
		}
		if got := r.Header.Get("X-Subrouter-Lease"); got != "" {
			t.Fatalf("lease header reached upstream: %q", got)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()

	codexUpstream := mustParseURL(t, upstream.URL+"/backend-api/codex")
	store := newSessionStore(t)
	leaseStore := newSessionLeaseStore()
	handler := Server{
		CodexUpstream: codexUpstream,
		Accounts: []accounts.Account{{
			ID:        "oauth@example.com",
			Provider:  accounts.ProviderCodex,
			AuthMode:  accounts.AuthModeOAuth,
			Token:     "oauth-access-secret",
			AccountID: "chatgpt-account-1",
		}},
		Sessions:      store,
		Scheduler:     selectacct.NewScheduler(nil),
		sessionLeases: leaseStore,
		AdminToken:    "service-admin-token",
		MaxBodyBytes:  1024,
	}.Handler()

	first, firstBody := issueSessionLease(t, handler, "codex", "openai/gpt-5.4")
	if strings.Contains(firstBody, "oauth-access-secret") {
		t.Fatal("lease response disclosed the underlying OAuth access token")
	}
	if first.Assignment.AuthMode != string(accounts.AuthModeOAuth) {
		t.Fatalf("auth mode = %q", first.Assignment.AuthMode)
	}
	if first.Assignment.Model != "gpt-5.4" {
		t.Fatalf("model = %q", first.Assignment.Model)
	}
	if first.Pi.API != "openai-codex-responses" || first.Pi.BaseURL != "http://subrouter:31415/backend-api" {
		t.Fatalf("unexpected Pi config: %+v", first.Pi)
	}
	if first.Environment["OPENAI_BASE_URL"] != "http://subrouter:31415/backend-api/codex" {
		t.Fatalf("OpenAI compatibility base URL = %q", first.Environment["OPENAI_BASE_URL"])
	}
	leaseToken := first.Environment["CLOUDMUX_SUBROUTER_LEASE_TOKEN"]
	header, payload, tokenParts := decodeSessionLeaseToken(t, leaseToken)
	if header.Type != sessionLeaseTokenType {
		t.Fatalf("lease token typ = %q", header.Type)
	}
	if !payload.CloudmuxSessionLease {
		t.Fatal("lease token is missing its public Cloudmux marker")
	}
	if payload.OpenAIAuthentication.ChatGPTAccountID != syntheticChatGPTAccountID {
		t.Fatalf("synthetic account ID = %q", payload.OpenAIAuthentication.ChatGPTAccountID)
	}
	if payload.OpenAIAuthentication.ChatGPTAccountID == "chatgpt-account-1" || strings.Contains(leaseToken, "chatgpt-account-1") {
		t.Fatal("lease token disclosed the upstream ChatGPT account ID")
	}
	if payload.Nonce == "" || tokenParts[2] == "" {
		t.Fatal("lease token requires a random nonce and signature segment")
	}
	if !looksLikeSessionLeaseToken(leaseToken) {
		t.Fatal("server did not recognize its JWT-shaped lease token")
	}
	if first.Environment["OPENAI_API_KEY"] != leaseToken {
		t.Fatal("OPENAI_API_KEY must contain the ephemeral broker token")
	}
	forgedSignature, err := randomLeaseValue("", 32)
	if err != nil {
		t.Fatal(err)
	}
	forged := tokenParts[0] + "." + tokenParts[1] + "." + forgedSignature
	if !looksLikeSessionLeaseToken(forged) {
		t.Fatal("public marker should identify the forged token shape")
	}
	if _, err := leaseStore.resolve(forged); !errors.Is(err, errInvalidSessionLease) {
		t.Fatalf("forged lease resolved: %v", err)
	}

	second, _ := issueSessionLease(t, handler, "codex", "openai/gpt-5.4")
	if second.LeaseID != first.LeaseID || second.Environment["CLOUDMUX_SUBROUTER_LEASE_TOKEN"] != leaseToken {
		t.Fatalf("retry minted a different lease: first=%s second=%s", first.LeaseID, second.LeaseID)
	}

	adapterResolvedURL := strings.TrimRight(first.Pi.BaseURL, "/") + "/codex/responses"
	if adapterResolvedURL != "http://subrouter:31415/backend-api/codex/responses" {
		t.Fatalf("Pi adapter URL = %q", adapterResolvedURL)
	}
	proxyReq := httptest.NewRequest(http.MethodPost, adapterResolvedURL, strings.NewReader(`{"model":"gpt-5.4"}`))
	proxyReq.Header.Set("Authorization", "Bearer "+leaseToken)
	proxyReq.Header.Set("Content-Type", "application/json")
	proxyRecorder := httptest.NewRecorder()
	handler.ServeHTTP(proxyRecorder, proxyReq)
	if proxyRecorder.Code != http.StatusNoContent {
		t.Fatalf("proxy status = %d, body = %s", proxyRecorder.Code, proxyRecorder.Body.String())
	}
	if upstreamCalls.Load() != 1 {
		t.Fatalf("upstream calls = %d, want 1", upstreamCalls.Load())
	}
	wrongEndpointReq := httptest.NewRequest(http.MethodPost, "http://subrouter:31415/backend-api/accounts/check", nil)
	wrongEndpointReq.Header.Set("Authorization", "Bearer "+leaseToken)
	wrongEndpoint := httptest.NewRecorder()
	handler.ServeHTTP(wrongEndpoint, wrongEndpointReq)
	if wrongEndpoint.Code != http.StatusForbidden {
		t.Fatalf("wrong endpoint status = %d, want 403", wrongEndpoint.Code)
	}
	if upstreamCalls.Load() != 1 {
		t.Fatal("lease reached an endpoint outside its model API")
	}

	releaseSessionLease(t, handler, first.LeaseID)
	rejectedReq := httptest.NewRequest(http.MethodPost, adapterResolvedURL, strings.NewReader(`{"model":"gpt-5.4"}`))
	rejectedReq.Header.Set("Authorization", "Bearer "+leaseToken)
	rejectedReq.Header.Set("Content-Type", "application/json")
	rejected := httptest.NewRecorder()
	handler.ServeHTTP(rejected, rejectedReq)
	if rejected.Code != http.StatusUnauthorized {
		t.Fatalf("released lease status = %d, want 401", rejected.Code)
	}
	if upstreamCalls.Load() != 1 {
		t.Fatalf("released lease reached upstream")
	}
}

func TestCodexSessionLeaseSkipsTerminalRefreshFailure(t *testing.T) {
	sessionStore := newSessionStore(t)
	if _, err := sessionStore.Put("pi", "agent-session-1", "stale@example.com", ""); err != nil {
		t.Fatal(err)
	}
	var refreshed []string
	handler := Server{
		Accounts: []accounts.Account{
			{ID: "stale@example.com", Provider: accounts.ProviderCodex, AuthMode: accounts.AuthModeOAuth, Token: "stale-token"},
			{ID: "healthy@example.com", Provider: accounts.ProviderCodex, AuthMode: accounts.AuthModeOAuth, Token: "healthy-token"},
		},
		Sessions:      sessionStore,
		Scheduler:     selectacct.NewScheduler(nil),
		sessionLeases: newSessionLeaseStore(),
		AdminToken:    "service-admin-token",
		MaxBodyBytes:  1024,
		RefreshAccountFn: func(_ context.Context, account accounts.Account) (accounts.Account, error) {
			refreshed = append(refreshed, account.ID)
			if account.ID == "stale@example.com" {
				return account, &accounts.CodexStoredRefreshFailureError{Failure: accounts.CodexRefreshFailure{
					StatusCode:   http.StatusUnauthorized,
					ProviderCode: "refresh_token_reused",
				}}
			}
			return account, nil
		},
	}.Handler()

	lease, _ := issueSessionLease(t, handler, "codex", "openai/gpt-5.4")
	if lease.Assignment.AccountID != "healthy@example.com" {
		t.Fatalf("lease account = %q, want healthy@example.com", lease.Assignment.AccountID)
	}
	if got := strings.Join(refreshed, ","); got != "stale@example.com,healthy@example.com" {
		t.Fatalf("refreshed accounts = %q, want stale then healthy", got)
	}
	assignment, ok := sessionStore.Get("pi", "agent-session-1")
	if !ok || assignment.AccountID != "healthy@example.com" {
		t.Fatalf("sticky assignment = %+v, want healthy account", assignment)
	}
}

func TestCodexSessionLeaseDoesNotFailOverTransientRefreshFailure(t *testing.T) {
	sessionStore := newSessionStore(t)
	if _, err := sessionStore.Put("pi", "agent-session-1", "temporarily-unreachable@example.com", ""); err != nil {
		t.Fatal(err)
	}
	var refreshCalls atomic.Int32
	handler := Server{
		Accounts: []accounts.Account{
			{ID: "temporarily-unreachable@example.com", Provider: accounts.ProviderCodex, AuthMode: accounts.AuthModeOAuth, Token: "first-token"},
			{ID: "other@example.com", Provider: accounts.ProviderCodex, AuthMode: accounts.AuthModeOAuth, Token: "other-token"},
		},
		Sessions:      sessionStore,
		Scheduler:     selectacct.NewScheduler(nil),
		sessionLeases: newSessionLeaseStore(),
		AdminToken:    "service-admin-token",
		MaxBodyBytes:  1024,
		RefreshAccountFn: func(_ context.Context, account accounts.Account) (accounts.Account, error) {
			refreshCalls.Add(1)
			return account, errors.New("dial tcp: connection refused")
		},
	}.Handler()

	req := newSessionLeaseRequest(t, "codex", "openai/gpt-5.4")
	req.RemoteAddr = "100.64.0.2:12345"
	req.Header.Set("Authorization", "Bearer service-admin-token")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body = %s", recorder.Code, recorder.Body.String())
	}
	if refreshCalls.Load() != 1 {
		t.Fatalf("refresh calls = %d, want one attempt without failover", refreshCalls.Load())
	}
	assignment, ok := sessionStore.Get("pi", "agent-session-1")
	if !ok || assignment.AccountID != "temporarily-unreachable@example.com" {
		t.Fatalf("sticky assignment = %+v, want original account", assignment)
	}
}

func TestRequiredSessionLeaseRejectsMissingOrUnrecognizableCapabilities(t *testing.T) {
	var upstreamCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamCalls.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()

	cache := newReadCache()
	cache.set(
		"/backend-api/ps/plugins/installed",
		http.StatusOK,
		http.Header{"Content-Type": []string{"application/json"}},
		[]byte(`{"cached":true}`),
		time.Minute,
	)
	handler := Server{
		APIUpstream: mustParseURL(t, upstream.URL),
		Accounts: []accounts.Account{{
			ID:       "apikey:openai",
			Provider: accounts.ProviderCodex,
			AuthMode: accounts.AuthModeAPIKey,
			Token:    "underlying-secret",
		}},
		Sessions:            newSessionStore(t),
		Scheduler:           selectacct.NewScheduler(nil),
		MaxBodyBytes:        1024,
		ReadCache:           cache,
		RequireSessionLease: true,
	}.Handler()

	for _, authorization := range []string{"", "Bearer subrouter", "Bearer malformed.lease.token"} {
		req := httptest.NewRequest(http.MethodPost, "http://subrouter:31415/v1/responses", strings.NewReader(`{"model":"gpt-5.4"}`))
		req.Header.Set("Content-Type", "application/json")
		if authorization != "" {
			req.Header.Set("Authorization", authorization)
		}
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, req)
		if recorder.Code != http.StatusUnauthorized {
			t.Fatalf("authorization %q status = %d, want 401; body = %s", authorization, recorder.Code, recorder.Body.String())
		}
	}
	if upstreamCalls.Load() != 0 {
		t.Fatalf("unleased requests reached upstream %d times", upstreamCalls.Load())
	}
	for _, request := range []*http.Request{
		httptest.NewRequest(http.MethodHead, "/", nil),
		httptest.NewRequest(http.MethodGet, "/backend-api/ps/plugins/installed", nil),
	} {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusUnauthorized {
			t.Fatalf("unleased %s %s status = %d, want 401", request.Method, request.URL.Path, recorder.Code)
		}
	}
}

func TestRequiredSessionLeaseAlsoClosesBedrockGateway(t *testing.T) {
	handler := Server{
		RequireSessionLease: true,
		Bedrock: &BedrockConfig{
			Regions:     []string{"us-east-1"},
			Credentials: staticBedrockCreds(),
		},
	}.Handler()
	req := httptest.NewRequest(http.MethodPost, "/bedrock/model/anthropic.claude/invoke", strings.NewReader(`{}`))
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("unleased Bedrock status = %d, want 401; body = %s", recorder.Code, recorder.Body.String())
	}
}

func TestKimiAndZAILeasePiRoutesMatchProviderUpstreams(t *testing.T) {
	tests := []struct {
		name          string
		provider      string
		model         string
		account       accounts.Account
		configure     func(*Server, *url.URL)
		wantAPI       string
		adapterSuffix string
		wantPath      string
		useAPIKey     bool
	}{
		{
			name:     "kimi anthropic messages",
			provider: "kimi",
			model:    "kimi-for-coding",
			account: accounts.Account{
				ID: "kimi:main", Provider: accounts.ProviderKimi,
				AuthMode: accounts.AuthModeAPIKey, Token: "kimi-secret",
			},
			configure: func(server *Server, upstream *url.URL) {
				server.KimiUpstream = upstream
			},
			wantAPI:       "anthropic-messages",
			adapterSuffix: "/v1/messages",
			wantPath:      "/coding/v1/messages",
			useAPIKey:     true,
		},
		{
			name:     "zai OpenAI completions",
			provider: "zai",
			model:    "glm-5.2",
			account: accounts.Account{
				ID: "zai:main", Provider: accounts.ProviderZAI,
				AuthMode: accounts.AuthModeAPIKey, Token: "zai-secret",
			},
			configure: func(server *Server, upstream *url.URL) {
				server.ZAIUpstream = upstream
			},
			wantAPI:       "openai-completions",
			adapterSuffix: "/chat/completions",
			wantPath:      "/api/coding/paas/v4/chat/completions",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != test.wantPath {
					t.Fatalf("upstream path = %q, want %q", r.URL.Path, test.wantPath)
				}
				w.WriteHeader(http.StatusNoContent)
			}))
			defer upstream.Close()
			upstreamURL := mustParseURL(t, upstream.URL)
			if test.provider == "kimi" {
				upstreamURL.Path = "/coding/v1"
			} else {
				upstreamURL.Path = "/api/coding/paas/v4"
			}
			server := Server{
				Accounts:      []accounts.Account{test.account},
				Sessions:      newSessionStore(t),
				Scheduler:     selectacct.NewScheduler(nil),
				sessionLeases: newSessionLeaseStore(),
				AdminToken:    "service-admin-token",
				MaxBodyBytes:  1024,
			}
			test.configure(&server, upstreamURL)
			handler := server.Handler()
			lease, _ := issueSessionLease(t, handler, test.provider, test.model)
			if lease.Pi.API != test.wantAPI {
				t.Fatalf("Pi API = %q, want %q", lease.Pi.API, test.wantAPI)
			}
			req := httptest.NewRequest(http.MethodPost, strings.TrimRight(lease.Pi.BaseURL, "/")+test.adapterSuffix, strings.NewReader(`{"model":"`+test.model+`"}`))
			req.Header.Set("Content-Type", "application/json")
			if test.useAPIKey {
				req.Header.Set("X-Api-Key", lease.Environment["CLOUDMUX_SUBROUTER_LEASE_TOKEN"])
			} else {
				req.Header.Set("Authorization", "Bearer "+lease.Environment["CLOUDMUX_SUBROUTER_LEASE_TOKEN"])
			}
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, req)
			if recorder.Code != http.StatusNoContent {
				t.Fatalf("proxy status = %d, body = %s", recorder.Code, recorder.Body.String())
			}
		})
	}
}

func TestClaudeAPIKeySessionLeaseUsesEphemeralTokenAtSandboxBoundary(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("X-Api-Key"); got != "sk-ant-underlying-secret" {
			t.Fatalf("X-Api-Key = %q, want selected API key", got)
		}
		if got := r.Header.Get("Authorization"); got != "" {
			t.Fatalf("Authorization = %q, want empty for Anthropic API-key auth", got)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()

	handler := Server{
		ClaudeUpstream: mustParseURL(t, upstream.URL),
		Accounts: []accounts.Account{{
			ID:       "claude:team-key",
			Provider: accounts.ProviderClaude,
			AuthMode: accounts.AuthModeAPIKey,
			Token:    "sk-ant-underlying-secret",
		}},
		Sessions:      newSessionStore(t),
		Scheduler:     selectacct.NewScheduler(nil),
		sessionLeases: newSessionLeaseStore(),
		AdminToken:    "service-admin-token",
		MaxBodyBytes:  1024,
	}.Handler()

	lease, body := issueSessionLease(t, handler, "claude", "anthropic/claude-sonnet-4-5")
	if strings.Contains(body, "sk-ant-underlying-secret") {
		t.Fatal("lease response disclosed the underlying API key")
	}
	leaseToken := lease.Environment["CLOUDMUX_SUBROUTER_LEASE_TOKEN"]
	if lease.Environment["ANTHROPIC_API_KEY"] != leaseToken || lease.Environment["ANTHROPIC_AUTH_TOKEN"] != leaseToken {
		t.Fatal("Anthropic environment must use the ephemeral broker token")
	}
	if lease.Pi.API != "anthropic-messages" || lease.Pi.BaseURL != "http://subrouter:31415" {
		t.Fatalf("unexpected Pi config: %+v", lease.Pi)
	}

	proxyReq := httptest.NewRequest(http.MethodPost, "http://subrouter:31415/v1/messages", strings.NewReader(`{"model":"claude-sonnet-4-5","max_tokens":8,"messages":[]}`))
	proxyReq.Header.Set("X-Api-Key", leaseToken)
	proxyReq.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, proxyReq)
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("proxy status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
}

func TestSessionLeaseValidatesForwardedModelInsteadOfRoutingHeaders(t *testing.T) {
	var upstreamCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		if got := r.Header.Get("X-Subrouter-Model"); got != "" {
			t.Errorf("routing header reached upstream: %q", got)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()

	handler := Server{
		CodexUpstream: mustParseURL(t, upstream.URL+"/backend-api/codex"),
		Accounts: []accounts.Account{{
			ID:        "oauth@example.com",
			Provider:  accounts.ProviderCodex,
			AuthMode:  accounts.AuthModeOAuth,
			Token:     "oauth-access-secret",
			AccountID: "chatgpt-account-1",
		}},
		Sessions:      newSessionStore(t),
		Scheduler:     selectacct.NewScheduler(nil),
		sessionLeases: newSessionLeaseStore(),
		AdminToken:    "service-admin-token",
		MaxBodyBytes:  1024,
	}.Handler()
	lease, _ := issueSessionLease(t, handler, "codex", "openai/gpt-5.4")
	leaseToken := lease.Environment["CLOUDMUX_SUBROUTER_LEASE_TOKEN"]

	tests := []struct {
		name        string
		url         string
		body        string
		header      string
		contentType string
	}{
		{
			name:        "routing header hides conflicting body",
			url:         "http://subrouter:31415/backend-api/codex/responses",
			body:        `{"model":"gpt-5.4-mini","input":"attacker-selected"}`,
			header:      "gpt-5.4",
			contentType: "application/json",
		},
		{
			name:        "routing header hides missing body model",
			url:         "http://subrouter:31415/backend-api/codex/responses",
			body:        `{"input":"missing model"}`,
			header:      "gpt-5.4",
			contentType: "application/json",
		},
		{
			name:        "duplicate body model conflicts",
			url:         "http://subrouter:31415/backend-api/codex/responses",
			body:        `{"model":"gpt-5.4","model":"gpt-5.4-mini","input":"duplicate key"}`,
			header:      "gpt-5.4",
			contentType: "application/json",
		},
		{
			name:        "query model conflicts with body",
			url:         "http://subrouter:31415/backend-api/codex/responses?model=gpt-5.4-mini",
			body:        `{"model":"gpt-5.4","input":"query conflict"}`,
			header:      "gpt-5.4",
			contentType: "application/json",
		},
		{
			name:        "non JSON body cannot establish model",
			url:         "http://subrouter:31415/backend-api/codex/responses",
			body:        `{"model":"gpt-5.4","input":"wrong content type"}`,
			header:      "gpt-5.4",
			contentType: "text/plain",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			before := upstreamCalls.Load()
			req := httptest.NewRequest(http.MethodPost, test.url, strings.NewReader(test.body))
			req.Header.Set("Authorization", "Bearer "+leaseToken)
			req.Header.Set("Content-Type", test.contentType)
			req.Header.Set("X-Subrouter-Model", test.header)
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, req)
			if recorder.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403; body = %s", recorder.Code, recorder.Body.String())
			}
			if upstreamCalls.Load() != before {
				t.Fatal("model-conflicting lease request reached upstream")
			}
		})
	}

	valid := httptest.NewRequest(http.MethodPost, "http://subrouter:31415/backend-api/codex/responses", strings.NewReader(`{"model":"gpt-5.4","input":"valid"}`))
	valid.Header.Set("Authorization", "Bearer "+leaseToken)
	valid.Header.Set("Content-Type", "application/json")
	validRecorder := httptest.NewRecorder()
	handler.ServeHTTP(validRecorder, valid)
	if validRecorder.Code != http.StatusNoContent {
		t.Fatalf("valid status = %d, want 204; body = %s", validRecorder.Code, validRecorder.Body.String())
	}
	if upstreamCalls.Load() != 1 {
		t.Fatalf("valid upstream calls = %d, want 1", upstreamCalls.Load())
	}
}

func TestRequestJSONModelsValidatesZstdAndRestoresCompressedBody(t *testing.T) {
	body := []byte(`{"model":"gpt-5.4","input":"compressed"}`)
	encoder, err := zstd.NewWriter(nil)
	if err != nil {
		t.Fatal(err)
	}
	compressed := encoder.EncodeAll(body, nil)
	encoder.Close()
	req := httptest.NewRequest(http.MethodPost, "http://subrouter/backend-api/codex/responses", bytes.NewReader(compressed))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Content-Encoding", "zstd")

	models, err := requestJSONModels(req, 1024)
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 1 || models[0] != "gpt-5.4" {
		t.Fatalf("models = %v, want [gpt-5.4]", models)
	}
	restored, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(restored, compressed) {
		t.Fatal("request body was not restored in its original compressed form")
	}
}

func TestRequestJSONModelsRejectsUnsafeZstdBodies(t *testing.T) {
	encode := func(body string) []byte {
		t.Helper()
		encoder, err := zstd.NewWriter(nil)
		if err != nil {
			t.Fatal(err)
		}
		defer encoder.Close()
		return encoder.EncodeAll([]byte(body), nil)
	}
	request := func(body []byte, maxBytes int64) error {
		req := httptest.NewRequest(http.MethodPost, "http://subrouter/backend-api/codex/responses", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Content-Encoding", "zstd")
		_, err := requestJSONModels(req, maxBytes)
		return err
	}

	conflicting := encode(`{"model":"gpt-5.4","model":"gpt-5.4-mini"}`)
	conflictingRequest := httptest.NewRequest(http.MethodPost, "http://subrouter/backend-api/codex/responses", bytes.NewReader(conflicting))
	conflictingRequest.Header.Set("Content-Type", "application/json")
	conflictingRequest.Header.Set("Content-Encoding", "zstd")
	models, err := requestJSONModels(conflictingRequest, 1024)
	if err != nil || len(models) != 2 || models[0] == models[1] {
		t.Fatalf("conflicting compressed models = %v, error = %v", models, err)
	}
	if err := request([]byte("not-zstd"), 1024); err == nil || !strings.Contains(err.Error(), "invalid zstd") {
		t.Fatalf("corrupt compressed body error = %v", err)
	}
	large := `{"model":"gpt-5.4","input":"` + strings.Repeat("x", 2048) + `"}`
	if err := request(encode(large), 1024); err == nil || !strings.Contains(err.Error(), "too large") {
		t.Fatalf("decompression-limit error = %v", err)
	}
}

func TestCodexPiSessionLeaseSelectsOnlyOAuthAccounts(t *testing.T) {
	apiKey := accounts.Account{
		ID:       "apikey:openai",
		Provider: accounts.ProviderCodex,
		AuthMode: accounts.AuthModeAPIKey,
		Token:    "sk-openai-secret",
	}
	oauth := accounts.Account{
		ID:        "oauth@example.com",
		Provider:  accounts.ProviderCodex,
		AuthMode:  accounts.AuthModeOAuth,
		Token:     "oauth-access-secret",
		AccountID: "chatgpt-account-1",
	}

	t.Run("mixed pool selects OAuth", func(t *testing.T) {
		handler := Server{
			Accounts:      []accounts.Account{apiKey, oauth},
			Sessions:      newSessionStore(t),
			Scheduler:     selectacct.NewScheduler(nil),
			sessionLeases: newSessionLeaseStore(),
			AdminToken:    "service-admin-token",
			MaxBodyBytes:  1024,
		}.Handler()
		lease, _ := issueSessionLease(t, handler, "codex", "openai/gpt-5.4")
		if lease.Assignment.AccountID != oauth.ID || lease.Assignment.AuthMode != string(accounts.AuthModeOAuth) {
			t.Fatalf("assignment = %+v, want OAuth account %q", lease.Assignment, oauth.ID)
		}
	})

	t.Run("API key only pool is rejected without mutating session state", func(t *testing.T) {
		leaseStore := newSessionLeaseStore()
		sessionStore := newSessionStore(t)
		handler := Server{
			Accounts:      []accounts.Account{apiKey},
			Sessions:      sessionStore,
			Scheduler:     selectacct.NewScheduler(nil),
			sessionLeases: leaseStore,
			AdminToken:    "service-admin-token",
			MaxBodyBytes:  1024,
		}.Handler()
		req := newSessionLeaseRequest(t, "codex", "openai/gpt-5.4")
		req.RemoteAddr = "100.64.0.2:12345"
		req.Header.Set("Authorization", "Bearer service-admin-token")
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, req)
		if recorder.Code != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want 503", recorder.Code)
		}
		if len(leaseStore.byID) != 0 {
			t.Fatal("rejected API-key lease was persisted")
		}
		if _, ok := sessionStore.Get("pi", "agent-session-1"); ok {
			t.Fatal("rejected API-key lease changed the sticky session assignment")
		}
	})
}

func TestSessionLeaseCreationIgnoresCallerAccountRoutingHeaders(t *testing.T) {
	accountsList := []accounts.Account{
		{
			ID:        "scheduled@example.com",
			Provider:  accounts.ProviderCodex,
			AuthMode:  accounts.AuthModeOAuth,
			Token:     "scheduled-token",
			AccountID: "scheduled-chatgpt",
		},
		{
			ID:        "forced@example.com",
			Provider:  accounts.ProviderCodex,
			AuthMode:  accounts.AuthModeOAuth,
			Token:     "forced-token",
			AccountID: "forced-chatgpt",
		},
	}
	newHandler := func() http.Handler {
		return Server{
			Accounts:      accountsList,
			Sessions:      newSessionStore(t),
			Scheduler:     selectacct.NewScheduler(nil),
			sessionLeases: newSessionLeaseStore(),
			AdminToken:    "service-admin-token",
			MaxBodyBytes:  1024,
		}.Handler()
	}

	baseline, _ := issueSessionLease(t, newHandler(), "codex", "gpt-5.4")
	forcedAccountID := accountsList[0].ID
	if forcedAccountID == baseline.Assignment.AccountID {
		forcedAccountID = accountsList[1].ID
	}
	req := newSessionLeaseRequest(t, "codex", "gpt-5.4")
	req.RemoteAddr = "100.64.0.2:12345"
	req.Header.Set("Authorization", "Bearer service-admin-token")
	req.Header.Set("X-Subrouter-Account-ID", forcedAccountID)
	recorder := httptest.NewRecorder()
	newHandler().ServeHTTP(recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("lease status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var routed sessionLeaseResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &routed); err != nil {
		t.Fatal(err)
	}
	if routed.Assignment.AccountID != baseline.Assignment.AccountID {
		t.Fatalf("caller forced account %q, scheduler assigned %q", routed.Assignment.AccountID, baseline.Assignment.AccountID)
	}
}

func TestSessionLeaseScopeKeyHasUnambiguousTenantBoundaries(t *testing.T) {
	left := sessionLeaseRequest{
		OrganizationID: "a",
		WorkspaceID:    "b\x00c",
		ConversationID: "conversation",
		InvocationID:   "invocation",
		AgentSessionID: "agent-session",
	}
	right := left
	right.OrganizationID = "a\x00b"
	right.WorkspaceID = "c"

	leftKey := sessionLeaseScopeKey(left, accounts.ProviderCodex, "gpt-5.4")
	rightKey := sessionLeaseScopeKey(right, accounts.ProviderCodex, "gpt-5.4")
	if leftKey == rightKey {
		t.Fatal("distinct tenant scopes produced the same lease key")
	}
}

func TestSessionLeaseDoesNotFailOverToDifferentClaudeAccount(t *testing.T) {
	var assignedCalls atomic.Int32
	var otherCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Header.Get("Authorization") {
		case "Bearer assigned-token":
			assignedCalls.Add(1)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"type":"error","error":{"type":"rate_limit_error","message":"rate limited"}}`))
		case "Bearer other-token":
			otherCalls.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"different-account-response"}`))
		default:
			http.Error(w, "unexpected credential", http.StatusUnauthorized)
		}
	}))
	defer upstream.Close()

	sessionStore := newSessionStore(t)
	handler := Server{
		ClaudeUpstream: mustParseURL(t, upstream.URL),
		Accounts: []accounts.Account{
			{ID: "assigned@example.com", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "assigned-token"},
			{ID: "other@example.com", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "other-token"},
		},
		Sessions:      sessionStore,
		Scheduler:     selectacct.NewScheduler(nil),
		sessionLeases: newSessionLeaseStore(),
		AdminToken:    "service-admin-token",
		MaxBodyBytes:  1024,
	}.Handler()
	lease, _ := issueSessionLease(t, handler, "claude", "anthropic/claude-sonnet-4-5")
	if lease.Assignment.AccountID != "assigned@example.com" {
		t.Fatalf("lease account = %q, want assigned@example.com", lease.Assignment.AccountID)
	}

	req := httptest.NewRequest(http.MethodPost, "http://subrouter:31415/v1/messages", strings.NewReader(`{"model":"claude-sonnet-4-5","max_tokens":8,"messages":[]}`))
	req.Header.Set("X-Api-Key", lease.Environment["CLOUDMUX_SUBROUTER_LEASE_TOKEN"])
	req.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want assigned account's 429; body = %s", recorder.Code, recorder.Body.String())
	}
	if assignedCalls.Load() != 1 || otherCalls.Load() != 0 {
		t.Fatalf("upstream calls assigned=%d other=%d, want 1 and 0", assignedCalls.Load(), otherCalls.Load())
	}
	assignment, ok := sessionStore.Get("pi", lease.SessionKey)
	if !ok || assignment.AccountID != "assigned@example.com" {
		t.Fatalf("sticky assignment = %+v, want assigned account", assignment)
	}
}

func TestSessionLeaseMayRetryTheSameAssignedAccount(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer assigned-token" {
			http.Error(w, "unexpected credential", http.StatusUnauthorized)
			return
		}
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusRequestTimeout)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"same-account-retry"}`))
	}))
	defer upstream.Close()

	handler := Server{
		ClaudeUpstream: mustParseURL(t, upstream.URL),
		Accounts: []accounts.Account{{
			ID:       "assigned@example.com",
			Provider: accounts.ProviderClaude,
			AuthMode: accounts.AuthModeOAuth,
			Token:    "assigned-token",
		}},
		Sessions:      newSessionStore(t),
		Scheduler:     selectacct.NewScheduler(nil),
		sessionLeases: newSessionLeaseStore(),
		AdminToken:    "service-admin-token",
		MaxBodyBytes:  1024,
	}.Handler()
	lease, _ := issueSessionLease(t, handler, "claude", "anthropic/claude-sonnet-4-5")
	req := httptest.NewRequest(http.MethodPost, "http://subrouter:31415/v1/messages", strings.NewReader(`{"model":"claude-sonnet-4-5","max_tokens":8,"messages":[]}`))
	req.Header.Set("X-Api-Key", lease.Environment["CLOUDMUX_SUBROUTER_LEASE_TOKEN"])
	req.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "same-account-retry") {
		t.Fatalf("status = %d body = %s, want successful same-account retry", recorder.Code, recorder.Body.String())
	}
	if calls.Load() != 2 {
		t.Fatalf("assigned account calls = %d, want 2", calls.Load())
	}
}

func TestSessionLeaseDoesNotUseClaudeFableBedrockPrimary(t *testing.T) {
	var bedrockCalls atomic.Int32
	var assignedCalls atomic.Int32
	bedrockTransport := bedrockRoundTripFunc(func(_ *http.Request) (*http.Response, error) {
		bedrockCalls.Add(1)
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"model":"claude-fable-5","content":[{"type":"text","text":"bedrock"}]}`)),
		}, nil
	})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		assignedCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"source":"assigned-account"}`))
	}))
	defer upstream.Close()

	handler := Server{
		ClaudeUpstream: mustParseURL(t, upstream.URL),
		Accounts: []accounts.Account{{
			ID:       "assigned@example.com",
			Provider: accounts.ProviderClaude,
			AuthMode: accounts.AuthModeOAuth,
			Token:    "assigned-token",
		}},
		Sessions:            newSessionStore(t),
		Scheduler:           selectacct.NewScheduler(nil),
		sessionLeases:       newSessionLeaseStore(),
		AdminToken:          "service-admin-token",
		MaxBodyBytes:        1024,
		FableBedrockPrimary: true,
		Bedrock: &BedrockConfig{
			Regions:     []string{"us-east-1"},
			Credentials: staticBedrockCreds(),
			Transport:   bedrockTransport,
		},
	}.Handler()
	lease, _ := issueSessionLease(t, handler, "claude", "anthropic/claude-fable-5")
	req := httptest.NewRequest(http.MethodPost, "http://subrouter:31415/v1/messages", strings.NewReader(`{"model":"claude-fable-5","max_tokens":8,"messages":[]}`))
	req.Header.Set("X-Api-Key", lease.Environment["CLOUDMUX_SUBROUTER_LEASE_TOKEN"])
	req.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "assigned-account") {
		t.Fatalf("status = %d body = %s, want assigned account response", recorder.Code, recorder.Body.String())
	}
	if bedrockCalls.Load() != 0 || assignedCalls.Load() != 1 {
		t.Fatalf("calls bedrock=%d assigned=%d, want 0 and 1", bedrockCalls.Load(), assignedCalls.Load())
	}
}

func TestClaudeFableLeaseCanBindDirectlyToAPIKeyPoolAccount(t *testing.T) {
	handler := Server{
		Accounts: []accounts.Account{{
			ID:       "claude:pool-api-key",
			Provider: accounts.ProviderClaude,
			AuthMode: accounts.AuthModeAPIKey,
			Token:    "sk-ant-pool-account",
		}},
		Sessions:          newSessionStore(t),
		Scheduler:         selectacct.NewScheduler(nil),
		sessionLeases:     newSessionLeaseStore(),
		AdminToken:        "service-admin-token",
		MaxBodyBytes:      1024,
		ClaudeFableAPIKey: "sk-ant-ordinary-fallback",
	}.Handler()

	lease, _ := issueSessionLease(t, handler, "claude", "anthropic/claude-fable-5")
	if lease.Assignment.AccountID != "claude:pool-api-key" || lease.Assignment.AuthMode != string(accounts.AuthModeAPIKey) {
		t.Fatalf("assignment = %+v, want direct API-key pool account", lease.Assignment)
	}
}

func TestSessionLeaseDoesNotUseClaudeFallbackWhenAssignedAccountIsGone(t *testing.T) {
	var fallbackCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Api-Key") == "sk-ant-fable-fallback" {
			fallbackCalls.Add(1)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"source":"fallback"}`))
	}))
	defer upstream.Close()

	leaseStore := newSessionLeaseStore()
	lease, err := leaseStore.put(sessionLease{
		ScopeKey:       "gone-account-scope",
		OrganizationID: "organization-1",
		WorkspaceID:    "workspace-1",
		ConversationID: "conversation-1",
		InvocationID:   "invocation-1",
		SessionKey:     "agent-session-1",
		Agent:          "pi",
		Provider:       accounts.ProviderClaude,
		AccountID:      "removed@example.com",
		AuthMode:       accounts.AuthModeOAuth,
		Model:          "claude-fable-5",
		ProxyBaseURL:   "http://subrouter:31415",
	})
	if err != nil {
		t.Fatal(err)
	}
	handler := Server{
		ClaudeUpstream:    mustParseURL(t, upstream.URL),
		Sessions:          newSessionStore(t),
		Scheduler:         selectacct.NewScheduler(nil),
		sessionLeases:     leaseStore,
		MaxBodyBytes:      1024,
		ClaudeFableAPIKey: "sk-ant-fable-fallback",
	}.Handler()
	req := httptest.NewRequest(http.MethodPost, "http://subrouter:31415/v1/messages", strings.NewReader(`{"model":"claude-fable-5","max_tokens":8,"messages":[]}`))
	req.Header.Set("X-Api-Key", lease.Token)
	req.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 from missing assigned account; body = %s", recorder.Code, recorder.Body.String())
	}
	if fallbackCalls.Load() != 0 {
		t.Fatalf("fallback calls = %d, want 0", fallbackCalls.Load())
	}
}

func TestSessionLeaseExpiryRejectsBrokerToken(t *testing.T) {
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	store := newSessionLeaseStore()
	store.now = func() time.Time { return now }
	store.ttl = time.Minute
	lease, err := store.put(sessionLease{ScopeKey: "scope", SessionKey: "session", Provider: accounts.ProviderCodex})
	if err != nil {
		t.Fatal(err)
	}
	other, err := store.put(sessionLease{ScopeKey: "other-scope", SessionKey: "other-session", Provider: accounts.ProviderCodex})
	if err != nil {
		t.Fatal(err)
	}
	_, leasePayload, leaseParts := decodeSessionLeaseToken(t, lease.Token)
	_, otherPayload, otherParts := decodeSessionLeaseToken(t, other.Token)
	if leasePayload.Nonce == otherPayload.Nonce || leaseParts[2] == otherParts[2] {
		t.Fatal("independent leases reused a nonce or signature segment")
	}
	now = now.Add(time.Minute)
	if _, err := store.resolve(lease.Token); !errors.Is(err, errInvalidSessionLease) {
		t.Fatalf("resolve after expiry error = %v", err)
	}
}

func TestSessionLeaseRenewalRotatesSafelyAndPreservesScope(t *testing.T) {
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	store := newSessionLeaseStore()
	store.now = func() time.Time { return now }
	lease, err := store.put(sessionLease{
		ScopeKey:       "organization-1\x00workspace-1\x00conversation-1\x00invocation-1",
		OrganizationID: "organization-1",
		WorkspaceID:    "workspace-1",
		ConversationID: "conversation-1",
		InvocationID:   "invocation-1",
		SessionKey:     "session-1",
		Agent:          "pi",
		Provider:       accounts.ProviderCodex,
		AccountID:      "oauth@example.com",
		AuthMode:       accounts.AuthModeOAuth,
		Model:          "gpt-5.4",
		ProxyBaseURL:   "http://subrouter:31415",
	})
	if err != nil {
		t.Fatal(err)
	}
	originalToken := lease.Token
	originalExpiry := lease.ExpiresAt
	handler := Server{sessionLeases: store, AdminToken: "service-admin-token"}.Handler()

	now = now.Add(10 * time.Minute)
	renewed := renewSessionLease(t, handler, lease.ID, originalToken, http.StatusOK)
	renewedToken := renewed.Environment["CLOUDMUX_SUBROUTER_LEASE_TOKEN"]
	if renewedToken == "" || renewedToken == originalToken {
		t.Fatal("renewal did not rotate the broker token")
	}
	if renewed.LeaseID != lease.ID || renewed.SessionKey != lease.SessionKey {
		t.Fatalf("renewal changed lease identity: %+v", renewed)
	}
	if renewed.Assignment.AccountID != lease.AccountID || renewed.Assignment.Provider != string(lease.Provider) || renewed.Assignment.AuthMode != string(lease.AuthMode) || renewed.Assignment.Model != lease.Model {
		t.Fatalf("renewal changed assignment: %+v", renewed.Assignment)
	}
	if renewed.Pi.BaseURL != "http://subrouter:31415/backend-api" || renewed.Pi.Model != lease.Model {
		t.Fatalf("renewal changed Pi routing: %+v", renewed.Pi)
	}
	renewedExpiry, err := time.Parse(time.RFC3339Nano, renewed.ExpiresAt)
	if err != nil {
		t.Fatal(err)
	}
	if !renewedExpiry.After(originalExpiry) || !renewedExpiry.Equal(now.Add(defaultSessionLeaseTTL)) {
		t.Fatalf("renewed expiry = %v, want %v", renewedExpiry, now.Add(defaultSessionLeaseTTL))
	}
	_, payload, _ := decodeSessionLeaseToken(t, renewedToken)
	if payload.ExpiresAt != renewedExpiry.Unix() {
		t.Fatalf("token exp = %d, response expiry = %d", payload.ExpiresAt, renewedExpiry.Unix())
	}
	resolved, err := store.resolve(renewedToken)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.OrganizationID != lease.OrganizationID || resolved.WorkspaceID != lease.WorkspaceID || resolved.ConversationID != lease.ConversationID || resolved.InvocationID != lease.InvocationID || resolved.ScopeKey != lease.ScopeKey {
		t.Fatalf("renewal changed Cloudmux scope: %+v", resolved)
	}
	if _, err := store.resolve(originalToken); err != nil {
		t.Fatalf("old token should cover in-flight requests during rotation grace: %v", err)
	}

	// A concurrent call or a retry after a lost response presents the old token.
	// It must return the same current token instead of rotating again.
	retry := renewSessionLease(t, handler, lease.ID, originalToken, http.StatusOK)
	if retry.Environment["CLOUDMUX_SUBROUTER_LEASE_TOKEN"] != renewedToken || retry.ExpiresAt != renewed.ExpiresAt {
		t.Fatal("retry with the prior token was not idempotent")
	}

	now = now.Add(sessionLeaseRotationGrace + time.Second)
	if _, err := store.resolve(originalToken); !errors.Is(err, errInvalidSessionLease) {
		t.Fatalf("old token remained usable after rotation grace: %v", err)
	}
	assertLeaseModelRequestRejected(t, handler, originalToken)
	retry = renewSessionLease(t, handler, lease.ID, originalToken, http.StatusOK)
	if retry.Environment["CLOUDMUX_SUBROUTER_LEASE_TOKEN"] != renewedToken {
		t.Fatal("timed-out renewal retry did not recover the current token")
	}

	secondRenewal := renewSessionLease(t, handler, lease.ID, renewedToken, http.StatusOK)
	secondRenewedToken := secondRenewal.Environment["CLOUDMUX_SUBROUTER_LEASE_TOKEN"]
	if secondRenewedToken == renewedToken || secondRenewedToken == originalToken {
		t.Fatal("serialized renewal did not rotate to a third token generation")
	}
	secondRetry := renewSessionLease(t, handler, lease.ID, renewedToken, http.StatusOK)
	if secondRetry.Environment["CLOUDMUX_SUBROUTER_LEASE_TOKEN"] != secondRenewedToken {
		t.Fatal("serialized renewal retry did not return the third token generation")
	}

	renewSessionLease(t, handler, lease.ID, "not-a-lease-token", http.StatusUnauthorized)
	renewSessionLease(t, handler, "lease_missing", secondRenewedToken, http.StatusNotFound)
	releaseSessionLease(t, handler, lease.ID)
	for name, token := range map[string]string{"original": originalToken, "renewed": renewedToken, "second-renewed": secondRenewedToken} {
		if _, err := store.resolve(token); !errors.Is(err, errInvalidSessionLease) {
			t.Fatalf("%s token resolved after release: %v", name, err)
		}
		assertLeaseModelRequestRejected(t, handler, token)
	}
	renewSessionLease(t, handler, lease.ID, secondRenewedToken, http.StatusNotFound)
}

func TestConcurrentSessionLeaseRenewalsReturnOneCurrentToken(t *testing.T) {
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	store := newSessionLeaseStore()
	store.now = func() time.Time { return now }
	lease, err := store.put(sessionLease{
		ScopeKey:     "scope",
		SessionKey:   "session",
		Provider:     accounts.ProviderCodex,
		ProxyBaseURL: "http://subrouter:31415",
	})
	if err != nil {
		t.Fatal(err)
	}

	const callers = 16
	results := make(chan sessionLease, callers)
	errorsSeen := make(chan error, callers)
	var wait sync.WaitGroup
	for range callers {
		wait.Add(1)
		go func() {
			defer wait.Done()
			renewed, renewErr := store.renew(lease.ID, lease.Token)
			if renewErr != nil {
				errorsSeen <- renewErr
				return
			}
			results <- renewed
		}()
	}
	wait.Wait()
	close(results)
	close(errorsSeen)
	for renewErr := range errorsSeen {
		t.Fatalf("concurrent renewal failed: %v", renewErr)
	}
	var currentToken string
	for renewed := range results {
		if currentToken == "" {
			currentToken = renewed.Token
		}
		if renewed.Token != currentToken {
			t.Fatalf("concurrent renewals returned different tokens")
		}
	}
	if currentToken == "" || currentToken == lease.Token {
		t.Fatal("concurrent renewal did not produce one new current token")
	}

	store.release(lease.ID)
	if _, err := store.renew(lease.ID, currentToken); !errors.Is(err, errSessionLeaseNotFound) {
		t.Fatalf("renewal resurrected a released lease: %v", err)
	}
}

func TestSessionLeaseProviderRejectsConflictingModelPrefix(t *testing.T) {
	if _, _, err := sessionLeaseProvider("codex", "anthropic/claude-sonnet-4-5"); err == nil {
		t.Fatal("expected conflicting provider and model prefix to fail")
	}
}

func TestPresentedSessionLeaseTokenIgnoresOrdinaryJWT(t *testing.T) {
	header := base64.RawStdEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	payload := base64.RawStdEncoding.EncodeToString([]byte(`{"cloudmux_session_lease":true}`))
	ordinaryJWT := header + "." + payload + ".signature"
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	req.Header.Set("Authorization", "Bearer "+ordinaryJWT)
	if _, presented := presentedSessionLeaseToken(req); presented {
		t.Fatal("ordinary provider JWT was mistaken for a session lease")
	}
}

func issueSessionLease(t *testing.T, handler http.Handler, provider, model string) (sessionLeaseResponse, string) {
	t.Helper()
	req := newSessionLeaseRequest(t, provider, model)
	req.RemoteAddr = "100.64.0.2:12345"
	req.Header.Set("Authorization", "Bearer service-admin-token")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("lease status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if recorder.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("lease response Cache-Control = %q, want no-store", recorder.Header().Get("Cache-Control"))
	}
	var response sessionLeaseResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	return response, recorder.Body.String()
}

func assertLeaseModelRequestRejected(t *testing.T, handler http.Handler, token string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "http://subrouter:31415/backend-api/codex/responses", strings.NewReader(`{"model":"gpt-5.4"}`))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("model request with invalid lease status = %d, want 401; body = %s", recorder.Code, recorder.Body.String())
	}
}

func newSessionLeaseRequest(t *testing.T, provider, model string) *http.Request {
	t.Helper()
	body, err := json.Marshal(sessionLeaseRequest{
		OrganizationID: "organization-1",
		WorkspaceID:    "workspace-1",
		ConversationID: "conversation-1",
		InvocationID:   "invocation-1",
		AgentSessionID: "agent-session-1",
		Agent:          "pi",
		Provider:       provider,
		Model:          model,
		ProxyBaseURL:   "http://subrouter:31415",
	})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "http://subrouter:31415/internal/v1/session-leases", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	return req
}

func releaseSessionLease(t *testing.T, handler http.Handler, leaseID string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodDelete, "http://subrouter:31415/internal/v1/session-leases/"+url.PathEscape(leaseID), nil)
	req.RemoteAddr = "100.64.0.2:12345"
	req.Header.Set("Authorization", "Bearer service-admin-token")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusNoContent {
		body, _ := io.ReadAll(recorder.Result().Body)
		t.Fatalf("release status = %d, body = %s", recorder.Code, string(body))
	}
}

func renewSessionLease(t *testing.T, handler http.Handler, leaseID, leaseToken string, wantStatus int) sessionLeaseResponse {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "http://subrouter:31415/internal/v1/session-leases/"+url.PathEscape(leaseID)+"/renew", nil)
	req.RemoteAddr = "100.64.0.2:12345"
	req.Header.Set("Authorization", "Bearer service-admin-token")
	req.Header.Set("X-Subrouter-Lease", leaseToken)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)
	if recorder.Code != wantStatus {
		t.Fatalf("renew status = %d, want %d; body = %s", recorder.Code, wantStatus, recorder.Body.String())
	}
	if wantStatus != http.StatusOK {
		return sessionLeaseResponse{}
	}
	if recorder.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("renewal response Cache-Control = %q, want no-store", recorder.Header().Get("Cache-Control"))
	}
	var response sessionLeaseResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	return response
}

func newSessionStore(t *testing.T) *session.Store {
	t.Helper()
	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func mustParseURL(t *testing.T, value string) *url.URL {
	t.Helper()
	parsed, err := url.Parse(value)
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}

func decodeSessionLeaseToken(t *testing.T, token string) (sessionLeaseTokenHeader, sessionLeaseTokenPayload, []string) {
	t.Helper()
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("lease token has %d segments, want 3", len(parts))
	}
	headerBody, err := base64.RawStdEncoding.DecodeString(parts[0])
	if err != nil {
		t.Fatalf("Pi-compatible header decode: %v", err)
	}
	payloadBody, err := base64.RawStdEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("Pi-compatible payload decode: %v", err)
	}
	var header sessionLeaseTokenHeader
	if err := json.Unmarshal(headerBody, &header); err != nil {
		t.Fatal(err)
	}
	var payload sessionLeaseTokenPayload
	if err := json.Unmarshal(payloadBody, &payload); err != nil {
		t.Fatal(err)
	}
	return header, payload, parts
}
