package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/session"
)

// TestAnthropicProtocolRequestsReachClaudeUpstream drives Server.Handler()
// with the requests Claude Code sends that carry no Claude user agent or
// session header: its Bun-runtime connectivity check (HEAD /api/hello) and an
// API call identified only by anthropic-version. Both must reach the Claude
// upstream; before, they fell through to the Codex default and chatgpt.com
// answered with an HTML page. A plain Codex request must still go to Codex.
func TestAnthropicProtocolRequestsReachClaudeUpstream(t *testing.T) {
	var mu sync.Mutex
	hits := map[string][]string{}
	upstream := func(name string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			hits[name] = append(hits[name], r.Method+" "+r.URL.Path)
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
		}))
	}
	claude, codex := upstream("claude"), upstream("codex")
	defer claude.Close()
	defer codex.Close()
	claudeURL, err := url.Parse(claude.URL)
	if err != nil {
		t.Fatal(err)
	}
	codexURL, err := url.Parse(codex.URL)
	if err != nil {
		t.Fatal(err)
	}

	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	handler := Server{
		Sessions:       store,
		ClaudeUpstream: claudeURL,
		CodexUpstream:  codexURL,
		Accounts: []accounts.Account{
			{ID: "claude@example.com", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "tok-claude"},
			{ID: "codex@example.com", Provider: accounts.ProviderCodex, AuthMode: accounts.AuthModeOAuth, Token: "tok-codex"},
		},
		MaxBodyBytes: 1 << 20,
	}.Handler()
	subrouter := httptest.NewServer(handler)
	defer subrouter.Close()

	send := func(method, path string, header map[string]string) {
		t.Helper()
		req, err := http.NewRequest(method, subrouter.URL+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		for key, value := range header {
			req.Header.Set(key, value)
		}
		response, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(response.Body)
		response.Body.Close()
		t.Logf("%s %s -> %d %.200s", method, path, response.StatusCode, body)
	}
	send(http.MethodHead, "/api/hello", map[string]string{"User-Agent": "Bun/1.4.3"})
	send(http.MethodGet, "/v1/models", map[string]string{"User-Agent": "Bun/1.4.3", "Anthropic-Version": "2023-06-01"})
	send(http.MethodGet, "/backend-api/codex/models", map[string]string{"User-Agent": "codex-tui/0.162.1"})
	// A provider path prefix still decides the provider before any
	// Anthropic evidence does.
	send(http.MethodPost, "/kimi/v1/messages", map[string]string{"Anthropic-Version": "2023-06-01"})

	mu.Lock()
	defer mu.Unlock()
	claudeHits := strings.Join(hits["claude"], ", ")
	if !strings.Contains(claudeHits, "HEAD /api/hello") || !strings.Contains(claudeHits, "GET /v1/models") {
		t.Fatalf("claude upstream got %q, want the connectivity check and the anthropic-version request", claudeHits)
	}
	for _, hit := range hits["codex"] {
		if strings.Contains(hit, "/api/hello") || strings.Contains(hit, "/v1/models") {
			t.Fatalf("codex upstream got Anthropic traffic %q; all codex hits: %v", hit, hits["codex"])
		}
	}
	if strings.Contains(claudeHits, "/kimi/") || strings.Contains(claudeHits, "POST /v1/messages") {
		t.Fatalf("claude upstream got the Kimi-prefixed request: %q", claudeHits)
	}
	if len(hits["codex"]) != 1 {
		t.Fatalf("codex upstream hits = %v, want the one Codex request", hits["codex"])
	}
}
