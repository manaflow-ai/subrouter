package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/session"
)

func TestHandlerRoutesKimiProviderPrefixToKimiUpstream(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/coding/v1/messages" {
			t.Fatalf("upstream path = %q, want /coding/v1/messages", r.URL.Path)
		}
		if got := r.Header.Get("X-Api-Key"); got != "kimi-token" {
			t.Fatal("upstream did not receive the expected Kimi API-key credential")
		}
		if got := r.Header.Get("Authorization"); got != "Bearer kimi-token" {
			t.Fatal("upstream did not receive the expected Kimi bearer credential")
		}
		if got := r.Header.Get("Anthropic-Version"); got != "2023-06-01" {
			t.Fatalf("Anthropic-Version = %q, want 2023-06-01", got)
		}
		_, _ = io.WriteString(w, `{"id":"msg_1"}`)
	}))
	defer upstream.Close()

	handler := opencodeProviderTestServer(t, accounts.Account{
		ID:       "kimi:main",
		Provider: accounts.ProviderKimi,
		AuthMode: accounts.AuthModeAPIKey,
		Token:    "kimi-token",
	}, accounts.ProviderKimi, upstream.URL+"/coding/v1").Handler()

	req := httptest.NewRequest(http.MethodPost, "/kimi/messages", strings.NewReader(`{"model":"kimi-for-coding"}`))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
}

func TestHandlerPreservesV1ForKimiUpstreamWithoutVersionedBase(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			t.Fatalf("upstream path = %q, want /v1/messages", r.URL.Path)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()

	handler := opencodeProviderTestServer(t, accounts.Account{
		ID:       "kimi:custom",
		Provider: accounts.ProviderKimi,
		AuthMode: accounts.AuthModeAPIKey,
		Token:    "kimi-token",
	}, accounts.ProviderKimi, upstream.URL).Handler()
	req := httptest.NewRequest(http.MethodPost, "/kimi/v1/messages", strings.NewReader(`{"model":"kimi-for-coding"}`))
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("status = %d body = %s", recorder.Code, recorder.Body.String())
	}
}

func TestHandlerRoutesZAIProviderPrefixToZAIUpstream(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/coding/paas/v4/chat/completions" {
			t.Fatalf("upstream path = %q, want /api/coding/paas/v4/chat/completions", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer zai-token" {
			t.Fatal("upstream did not receive the expected ZAI bearer credential")
		}
		if got := r.Header.Get("X-Api-Key"); got != "" {
			t.Fatal("upstream received an X-Api-Key header that should have been stripped")
		}
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"ok"}}]}`)
	}))
	defer upstream.Close()

	handler := opencodeProviderTestServer(t, accounts.Account{
		ID:       "zai:main",
		Provider: accounts.ProviderZAI,
		AuthMode: accounts.AuthModeAPIKey,
		Token:    "zai-token",
	}, accounts.ProviderZAI, upstream.URL+"/api/coding/paas/v4").Handler()

	req := httptest.NewRequest(http.MethodPost, "/zai/chat/completions", strings.NewReader(`{"model":"glm-5.2"}`))
	req.Header.Set("X-Api-Key", "client-key")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
}

func opencodeProviderTestServer(t *testing.T, account accounts.Account, provider accounts.Provider, upstreamRaw string) Server {
	t.Helper()
	upstream, err := url.Parse(upstreamRaw)
	if err != nil {
		t.Fatal(err)
	}
	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	server := Server{
		Accounts:     []accounts.Account{account},
		Sessions:     store,
		MaxBodyBytes: 1024,
	}
	// Antigravity is OAuth-only and stays out of the keyed-provider registry,
	// so it bypasses the registry resolution.
	if provider == accounts.ProviderAntigravity {
		server.AntigravityUpstream = upstream
		return server
	}
	// Resolve through the registry so a newly registered provider is testable
	// without editing this helper, and so an entry whose Upstream accessor reads
	// the wrong field fails loudly instead of silently routing nowhere.
	entry, ok := keyedProviderFor(provider)
	if !ok {
		t.Fatalf("unsupported provider %s", provider)
	}
	switch entry.Provider {
	case accounts.ProviderKimi:
		server.KimiUpstream = upstream
	case accounts.ProviderZAI:
		server.ZAIUpstream = upstream
	case accounts.ProviderOpenRouter:
		server.OpenRouterUpstream = upstream
	case accounts.ProviderDeepSeek:
		server.DeepSeekUpstream = upstream
	case accounts.ProviderTogether:
		server.TogetherUpstream = upstream
	case accounts.ProviderFireworks:
		server.FireworksUpstream = upstream
	case accounts.ProviderOpenCodeZen:
		server.OpenCodeZenUpstream = upstream
	case accounts.ProviderGrok:
		server.GrokUpstream = upstream
	case accounts.ProviderQwen:
		server.QwenUpstream = upstream
	case accounts.ProviderQwenToken:
		server.QwenTokenUpstream = upstream
	case accounts.ProviderQwenAnthropic:
		server.QwenAnthropicUpstream = upstream
	default:
		t.Fatalf("no upstream field wired for provider %s", provider)
	}
	if entry.Upstream(server, account.AuthMode) != upstream {
		t.Fatalf("provider %s upstream accessor does not read the field this helper set", provider)
	}
	return server
}

func TestHandlerRoutesAdditionalOpenAICompatibleProviders(t *testing.T) {
	for _, provider := range []accounts.Provider{
		accounts.ProviderDeepSeek,
		accounts.ProviderTogether,
		accounts.ProviderFireworks,
		accounts.ProviderOpenCodeZen,
	} {
		t.Run(string(provider), func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/chat/completions" {
					t.Fatalf("upstream path = %q, want /v1/chat/completions", r.URL.Path)
				}
				if got := r.Header.Get("Authorization"); got != "Bearer provider-token" {
					t.Fatal("upstream did not receive the expected provider bearer credential")
				}
				_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"ok"}}]}`)
			}))
			defer upstream.Close()

			handler := opencodeProviderTestServer(t, accounts.Account{
				ID: string(provider) + ":main", Provider: provider,
				AuthMode: accounts.AuthModeAPIKey, Token: "provider-token",
			}, provider, upstream.URL+"/v1").Handler()
			req := httptest.NewRequest(http.MethodPost, "/"+string(provider)+"/chat/completions", strings.NewReader(`{"model":"test"}`))
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestHandlerRoutesOpenRouterProviderPrefixToOpenRouterUpstream(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/chat/completions" {
			t.Fatalf("upstream path = %q, want /api/v1/chat/completions", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer sk-or-v1-token" {
			t.Fatal("upstream did not receive the expected OpenRouter bearer credential")
		}
		if got := r.Header.Get("X-Api-Key"); got != "" {
			t.Fatal("upstream received an X-Api-Key header that should have been stripped")
		}
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"ok"}}]}`)
	}))
	defer upstream.Close()

	handler := opencodeProviderTestServer(t, accounts.Account{
		ID:       "openrouter:main",
		Provider: accounts.ProviderOpenRouter,
		AuthMode: accounts.AuthModeAPIKey,
		Token:    "sk-or-v1-token",
	}, accounts.ProviderOpenRouter, upstream.URL+"/api/v1").Handler()

	req := httptest.NewRequest(http.MethodPost, "/openrouter/chat/completions", strings.NewReader(`{"model":"anthropic/claude-opus-5"}`))
	req.Header.Set("X-Api-Key", "client-key")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
}

// An Antigravity request carries the API version in the path the CLI sends
// (v1internal:method), so routing strips only the provider prefix and swaps in
// the OAuth token.
func TestHandlerRoutesAntigravityPrefixToCloudCodeUpstream(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1internal:loadCodeAssist" {
			t.Fatalf("upstream path = %q, want /v1internal:loadCodeAssist", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer antigravity-token" {
			t.Fatal("upstream did not receive the expected Antigravity OAuth credential")
		}
		_, _ = io.WriteString(w, `{}`)
	}))
	defer upstream.Close()

	handler := opencodeProviderTestServer(t, accounts.Account{
		ID:       "antigravity",
		Provider: accounts.ProviderAntigravity,
		AuthMode: accounts.AuthModeOAuth,
		Token:    "antigravity-token",
	}, accounts.ProviderAntigravity, upstream.URL).Handler()

	req := httptest.NewRequest(http.MethodPost, "/antigravity/v1internal:loadCodeAssist", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
}

// OpenRouter's base URL already ends in /v1 and OpenAI-compatible clients send
// /v1/chat/completions, so the version segment must collapse rather than reach
// upstream as /api/v1/v1/chat/completions.
func TestHandlerCollapsesDuplicateV1ForOpenRouterUpstream(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/chat/completions" {
			t.Fatalf("upstream path = %q, want /api/v1/chat/completions", r.URL.Path)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()

	handler := opencodeProviderTestServer(t, accounts.Account{
		ID:       "openrouter:main",
		Provider: accounts.ProviderOpenRouter,
		AuthMode: accounts.AuthModeAPIKey,
		Token:    "sk-or-v1-token",
	}, accounts.ProviderOpenRouter, upstream.URL+"/api/v1").Handler()

	req := httptest.NewRequest(http.MethodPost, "/openrouter/v1/chat/completions", strings.NewReader(`{"model":"anthropic/claude-opus-5"}`))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
}

// An unversioned override base must keep the client's /v1, the same way the
// Kimi lane does — the collapse is conditional on the upstream base, not on the
// provider.
func TestHandlerPreservesV1ForOpenRouterUpstreamWithoutVersionedBase(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Fatalf("upstream path = %q, want /v1/chat/completions", r.URL.Path)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()

	handler := opencodeProviderTestServer(t, accounts.Account{
		ID:       "openrouter:custom",
		Provider: accounts.ProviderOpenRouter,
		AuthMode: accounts.AuthModeAPIKey,
		Token:    "sk-or-v1-token",
	}, accounts.ProviderOpenRouter, upstream.URL).Handler()

	req := httptest.NewRequest(http.MethodPost, "/openrouter/v1/chat/completions", strings.NewReader(`{"model":"anthropic/claude-opus-5"}`))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
}

func TestProviderForPathOpenRouter(t *testing.T) {
	provider, ok := providerForPath("/openrouter/chat/completions")
	if !ok || provider != accounts.ProviderOpenRouter {
		t.Fatalf("providerForPath = (%q, %v), want openrouter", provider, ok)
	}
	if agent := agentTypeForProviderSession("codex", accounts.ProviderOpenRouter); agent != "openrouter" {
		t.Fatalf("agentTypeForProviderSession = %q, want openrouter", agent)
	}
	if plan := apiKeyPlanType(accounts.ProviderOpenRouter); plan != "openrouter api key" {
		t.Fatalf("apiKeyPlanType = %q, want openrouter api key", plan)
	}
}

func TestHandlerRoutesGrokProviderPrefixToGrokUpstream(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Fatalf("upstream path = %q, want /v1/chat/completions", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer xai-token" {
			t.Fatal("upstream did not receive the expected xAI bearer credential")
		}
		if got := r.Header.Get("X-Api-Key"); got != "" {
			t.Fatal("upstream received an X-Api-Key header that should have been stripped")
		}
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"ok"}}]}`)
	}))
	defer upstream.Close()

	handler := opencodeProviderTestServer(t, accounts.Account{
		ID:       "grok:main",
		Provider: accounts.ProviderGrok,
		AuthMode: accounts.AuthModeAPIKey,
		Token:    "xai-token",
	}, accounts.ProviderGrok, upstream.URL+"/v1").Handler()

	req := httptest.NewRequest(http.MethodPost, "/grok/chat/completions", strings.NewReader(`{"model":"grok-4"}`))
	req.Header.Set("X-Api-Key", "client-key")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
}

// A Grok subscription (device-code OAuth) terminates at the CLI's chat
// proxy, not the API-key host — the one provider whose upstream depends on the
// account's auth mode.
func TestHandlerRoutesGrokOAuthToTheSubscriptionUpstream(t *testing.T) {
	apiKeyUpstreamHit := false
	apiKeyUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		apiKeyUpstreamHit = true
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer apiKeyUpstream.Close()
	subscriptionUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Fatalf("upstream path = %q, want /v1/chat/completions (the /v1 collapsed, not duplicated)", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer grok-oauth-token" {
			t.Fatal("upstream did not receive the expected Grok OAuth credential")
		}
		if got := r.Header.Get("X-XAI-Token-Auth"); got != "xai-grok-cli" {
			t.Fatalf("X-XAI-Token-Auth = %q, want the Grok subscription marker", got)
		}
		if got := r.Header.Get("X-Grok-Model-Override"); got != "grok-4" {
			t.Fatalf("X-Grok-Model-Override = %q, want the requested model", got)
		}
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"ok"}}]}`)
	}))
	defer subscriptionUpstream.Close()

	apiKeyURL, err := url.Parse(apiKeyUpstream.URL + "/v1")
	if err != nil {
		t.Fatal(err)
	}
	subscriptionURL, err := url.Parse(subscriptionUpstream.URL + "/v1")
	if err != nil {
		t.Fatal(err)
	}
	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	handler := Server{
		Accounts: []accounts.Account{{
			ID:       "grok:subscription",
			Provider: accounts.ProviderGrok,
			AuthMode: accounts.AuthModeOAuth,
			Token:    "grok-oauth-token",
		}},
		Sessions:                 store,
		MaxBodyBytes:             1024,
		GrokUpstream:             apiKeyURL,
		GrokSubscriptionUpstream: subscriptionURL,
	}.Handler()

	req := httptest.NewRequest(http.MethodPost, "/grok/v1/chat/completions", strings.NewReader(`{"model":"grok-4"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	if apiKeyUpstreamHit {
		t.Fatal("an OAuth grok account must not reach the API-key upstream")
	}
}

// The API-key path must keep its own upstream after the auth-mode split.
func TestHandlerKeepsGrokAPIKeyOnTheAPIKeyUpstream(t *testing.T) {
	subscriptionUpstreamHit := false
	subscriptionUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		subscriptionUpstreamHit = true
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer subscriptionUpstream.Close()
	apiKeyUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Fatalf("upstream path = %q, want /v1/chat/completions", r.URL.Path)
		}
		if got := r.Header.Get("X-XAI-Token-Auth"); got != "" {
			t.Fatalf("API-key request retained subscription header X-XAI-Token-Auth=%q", got)
		}
		if got := r.Header.Get("X-Grok-Model-Override"); got != "" {
			t.Fatalf("API-key request retained subscription model override=%q", got)
		}
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"ok"}}]}`)
	}))
	defer apiKeyUpstream.Close()

	// The server must also carry the subscription upstream so the split is
	// observable.
	server := opencodeProviderTestServer(t, accounts.Account{
		ID:       "grok:main",
		Provider: accounts.ProviderGrok,
		AuthMode: accounts.AuthModeAPIKey,
		Token:    "xai-token",
	}, accounts.ProviderGrok, apiKeyUpstream.URL+"/v1")
	server.GrokSubscriptionUpstream = mustParseURL(t, subscriptionUpstream.URL+"/v1")
	handler := server.Handler()
	req := httptest.NewRequest(http.MethodPost, "/grok/v1/chat/completions", strings.NewReader(`{"model":"grok-4"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-XAI-Token-Auth", "spoofed")
	req.Header.Set("X-Grok-Model-Override", "spoofed-model")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	if subscriptionUpstreamHit {
		t.Fatal("an API-key grok account must not reach the subscription upstream")
	}
}

// api.x.ai/v1 already ends in /v1 and OpenAI-compatible clients send
// /v1/chat/completions, so the version segment must collapse.
func TestHandlerCollapsesDuplicateV1ForGrokUpstream(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Fatalf("upstream path = %q, want /v1/chat/completions", r.URL.Path)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()

	handler := opencodeProviderTestServer(t, accounts.Account{
		ID:       "grok:main",
		Provider: accounts.ProviderGrok,
		AuthMode: accounts.AuthModeAPIKey,
		Token:    "xai-token",
	}, accounts.ProviderGrok, upstream.URL+"/v1").Handler()

	req := httptest.NewRequest(http.MethodPost, "/grok/v1/chat/completions", strings.NewReader(`{"model":"grok-4"}`))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
}

// A bare grok- model id resolves to Grok without the caller naming a provider,
// the same way glm- resolves to ZAI. Unlike OpenRouter, xAI model ids carry no
// vendor prefix, so the provider-selector rule still applies to Grok.
func TestSessionLeaseInfersGrokFromModelPrefix(t *testing.T) {
	provider, model, err := sessionLeaseProvider("", "grok-4")
	if err != nil {
		t.Fatalf("a bare grok model should resolve: %v", err)
	}
	if provider != accounts.ProviderGrok || model != "grok-4" {
		t.Fatalf("got (%q, %q), want (grok, grok-4)", provider, model)
	}
	for _, alias := range []string{"grok", "xai", "x-ai"} {
		got, _, err := sessionLeaseProvider(alias, "grok-4")
		if err != nil || got != accounts.ProviderGrok {
			t.Fatalf("alias %q resolved to (%q, %v), want grok", alias, got, err)
		}
	}
	if _, _, err := sessionLeaseProvider("grok", "openai/gpt-5"); err == nil {
		t.Fatal("grok must keep the vendor/model selector rule and reject a mismatch")
	}
}

func TestHandlerRoutesQwenProviderPrefixToCodingPlanUpstream(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Fatalf("upstream path = %q, want /v1/chat/completions", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer sk-sp-token" {
			t.Fatal("upstream did not receive the expected Qwen Coding Plan credential")
		}
		if got := r.Header.Get("X-Api-Key"); got != "" {
			t.Fatal("upstream received an X-Api-Key header that should have been stripped")
		}
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"ok"}}]}`)
	}))
	defer upstream.Close()

	handler := opencodeProviderTestServer(t, accounts.Account{
		ID:       "qwen:main",
		Provider: accounts.ProviderQwen,
		AuthMode: accounts.AuthModeAPIKey,
		Token:    "sk-sp-token",
	}, accounts.ProviderQwen, upstream.URL+"/v1").Handler()

	req := httptest.NewRequest(http.MethodPost, "/qwen/v1/chat/completions", strings.NewReader(`{"model":"qwen3-coder-plus"}`))
	req.Header.Set("X-Api-Key", "client-key")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
}

// The Coding Plan is a subscription addressed by a plan-specific key, and its
// endpoint differs from standard DashScope. A bare qwen model id resolves
// without the caller naming a provider.
func TestSessionLeaseInfersQwenFromModelPrefix(t *testing.T) {
	provider, model, err := sessionLeaseProvider("", "qwen3-coder-plus")
	if err != nil {
		t.Fatalf("a bare qwen model should resolve: %v", err)
	}
	if provider != accounts.ProviderQwen || model != "qwen3-coder-plus" {
		t.Fatalf("got (%q, %q), want (qwen, qwen3-coder-plus)", provider, model)
	}
	for _, alias := range []string{"qwen", "dashscope", "modelstudio"} {
		got, _, err := sessionLeaseProvider(alias, "qwen3-coder-plus")
		if err != nil || got != accounts.ProviderQwen {
			t.Fatalf("alias %q resolved to (%q, %v), want qwen", alias, got, err)
		}
	}
}

func TestSessionLeaseInfersBareQwenFromAvailablePlanPool(t *testing.T) {
	tokenPlan := accounts.Account{ID: "qwen-token:main", Provider: accounts.ProviderQwenToken}
	codingPlan := accounts.Account{ID: "qwen:main", Provider: accounts.ProviderQwen}
	sharedAnthropic := accounts.Account{ID: "qwen-token:anthropic", Provider: accounts.ProviderQwenAnthropic}

	for _, test := range []struct {
		name     string
		provider string
		pool     []accounts.Account
		want     accounts.Provider
	}{
		{name: "token plan only", pool: []accounts.Account{tokenPlan}, want: accounts.ProviderQwenToken},
		{name: "shared anthropic view only", pool: []accounts.Account{sharedAnthropic}, want: accounts.ProviderQwenToken},
		{name: "coding plan only", pool: []accounts.Account{codingPlan}, want: accounts.ProviderQwen},
		{name: "both prefer coding plan", pool: []accounts.Account{tokenPlan, codingPlan}, want: accounts.ProviderQwen},
		{name: "explicit coding plan is not redirected", provider: "qwen", pool: []accounts.Account{tokenPlan}, want: accounts.ProviderQwen},
		{name: "explicit token plan is preserved", provider: "qwen-token", pool: []accounts.Account{codingPlan}, want: accounts.ProviderQwenToken},
		{name: "explicit anthropic protocol is preserved", provider: "qwen-anthropic", pool: []accounts.Account{codingPlan, tokenPlan}, want: accounts.ProviderQwenAnthropic},
	} {
		t.Run(test.name, func(t *testing.T) {
			provider, model, err := sessionLeaseProviderForAccounts(test.provider, "qwen3-coder-plus", test.pool)
			if err != nil {
				t.Fatal(err)
			}
			if provider != test.want || model != "qwen3-coder-plus" {
				t.Fatalf("got (%q, %q), want (%q, qwen3-coder-plus)", provider, model, test.want)
			}
		})
	}
}

// The Token Plan is a different subscription from the Coding Plan, with its own
// key and its own host, so both must be able to serve at once rather than the
// operator choosing one. The Token Plan base ends in /compatible-mode/v1, so a
// client's own /v1 still has to collapse.
func TestHandlerRoutesQwenTokenPlanAlongsideCodingPlan(t *testing.T) {
	var codingHits, tokenHits int
	coding := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		codingHits++
		if r.URL.Path != "/v1/chat/completions" {
			t.Fatalf("coding-plan path = %q", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer sk-sp-coding" {
			t.Fatal("upstream did not receive the expected Qwen Coding Plan credential")
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer coding.Close()
	token := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tokenHits++
		if r.URL.Path != "/compatible-mode/v1/chat/completions" {
			t.Fatalf("token-plan path = %q, want /compatible-mode/v1/chat/completions", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer sk-token-plan" {
			t.Fatal("upstream did not receive the expected Qwen Token Plan credential")
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer token.Close()

	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	server := Server{
		Accounts: []accounts.Account{
			{ID: "qwen:coding", Provider: accounts.ProviderQwen, AuthMode: accounts.AuthModeAPIKey, Token: "sk-sp-coding"},
			{ID: "qwen-token:main", Provider: accounts.ProviderQwenToken, AuthMode: accounts.AuthModeAPIKey, Token: "sk-token-plan"},
		},
		Sessions:          store,
		QwenUpstream:      mustParseURL(t, coding.URL+"/v1"),
		QwenTokenUpstream: mustParseURL(t, token.URL+"/compatible-mode/v1"),
		MaxBodyBytes:      1024,
	}
	handler := server.Handler()

	for path, wantHits := range map[string]*int{
		"/qwen/v1/chat/completions":       &codingHits,
		"/qwen-token/v1/chat/completions": &tokenHits,
	} {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"model":"qwen3.8-max"}`))
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusNoContent {
			t.Fatalf("%s status = %d body = %s", path, rec.Code, rec.Body.String())
		}
		if *wantHits != 1 {
			t.Fatalf("%s did not reach its own upstream exactly once", path)
		}
	}
}

// The Token Plan also serves the Anthropic protocol, which is what lets an
// Anthropic-shaped client run on a Qwen subscription with no translation. Its
// base stops at /apps/anthropic and the client appends /v1/messages itself, so
// the version segment must survive: collapsing it is exactly the /v1/v1 404 the
// vendor documents.
func TestHandlerPreservesTheVersionSegmentForQwenAnthropic(t *testing.T) {
	var seen []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.URL.Path)
		if got := r.Header.Get("Authorization"); got != "Bearer sk-token-plan" {
			t.Fatal("upstream did not receive the expected Qwen Token Plan credential")
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()

	handler := opencodeProviderTestServer(t, accounts.Account{
		ID:       "qwen-anthropic:main",
		Provider: accounts.ProviderQwenAnthropic,
		AuthMode: accounts.AuthModeAPIKey,
		Token:    "sk-token-plan",
	}, accounts.ProviderQwenAnthropic, upstream.URL+"/apps/anthropic").Handler()

	req := httptest.NewRequest(http.MethodPost, "/qwen-anthropic/v1/messages", strings.NewReader(`{"model":"qwen3.7-plus","max_tokens":16,"messages":[]}`))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	if len(seen) != 1 || seen[0] != "/apps/anthropic/v1/messages" {
		t.Fatalf("upstream saw %v, want [/apps/anthropic/v1/messages]", seen)
	}
	for _, path := range seen {
		if strings.Contains(path, "/v1/v1") {
			t.Fatalf("path %q duplicates the version segment, which the vendor documents as a 404", path)
		}
	}
}

// A lease for the Anthropic-protocol provider must hand the sandbox the
// Anthropic environment variables, or an Anthropic client has nothing to read.
func TestQwenAnthropicLeaseUsesAnthropicEnvironment(t *testing.T) {
	entry, ok := keyedProviderFor(accounts.ProviderQwenAnthropic)
	if !ok {
		t.Fatal("qwen-anthropic should be a registered provider")
	}
	if entry.LeaseEnv != leaseEnvAnthropic {
		t.Fatal("an Anthropic-protocol provider must lease the Anthropic environment")
	}
	if entry.LeaseAPI != "anthropic-messages" {
		t.Fatalf("LeaseAPI = %q, want anthropic-messages", entry.LeaseAPI)
	}
	if entry.CollapseVersionSegment {
		t.Fatal("collapsing the version segment would produce the documented /v1/v1 404")
	}
	// The OpenAI-protocol sibling must keep its own shape.
	openaiSibling, ok := keyedProviderFor(accounts.ProviderQwenToken)
	if !ok || openaiSibling.LeaseEnv != leaseEnvOpenAI || !openaiSibling.CollapseVersionSegment {
		t.Fatal("the OpenAI-protocol Token Plan entry must be unaffected")
	}
}

// A legacy provider-less account must never serve an Antigravity request: the
// provider is OAuth-only, so falling back to a bare Codex credential would
// forward it to Google.
func TestFilterAccountsForProviderWithholdsLegacyAccountsFromAntigravity(t *testing.T) {
	legacy := accounts.Account{ID: "legacy", Token: "codex-token"}
	if got := filterAccountsForProvider([]accounts.Account{legacy}, accounts.ProviderAntigravity); len(got) != 0 {
		t.Fatalf("got %+v, want no accounts", got)
	}
	antigravity := accounts.Account{ID: "antigravity", Provider: accounts.ProviderAntigravity, AuthMode: accounts.AuthModeOAuth}
	got := filterAccountsForProvider([]accounts.Account{legacy, antigravity}, accounts.ProviderAntigravity)
	if len(got) != 1 || got[0].ID != "antigravity" {
		t.Fatalf("got %+v, want only the antigravity account", got)
	}
}
