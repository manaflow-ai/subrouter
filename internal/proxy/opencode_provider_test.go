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
			t.Fatalf("X-Api-Key = %q, want kimi token", got)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer kimi-token" {
			t.Fatalf("Authorization = %q, want kimi bearer", got)
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

func TestHandlerRoutesZAIProviderPrefixToZAIUpstream(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/coding/paas/v4/chat/completions" {
			t.Fatalf("upstream path = %q, want /api/coding/paas/v4/chat/completions", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer zai-token" {
			t.Fatalf("Authorization = %q, want zai bearer", got)
		}
		if got := r.Header.Get("X-Api-Key"); got != "" {
			t.Fatalf("X-Api-Key = %q, want stripped", got)
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
	switch provider {
	case accounts.ProviderKimi:
		server.KimiUpstream = upstream
	case accounts.ProviderZAI:
		server.ZAIUpstream = upstream
	default:
		t.Fatalf("unsupported provider %s", provider)
	}
	return server
}
