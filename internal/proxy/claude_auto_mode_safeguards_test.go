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

// Claude Code v2.1.278+ negotiates server-side auto mode on the ordinary
// Messages request. The gateway must carry the opaque safeguards envelope and
// beta value to Anthropic, then return safeguard_results without decoding or
// rewriting it.
func TestClaudeAutoModeSafeguardsPassThrough(t *testing.T) {
	const requestBody = `{"model":"claude-sonnet-4-6","max_tokens":32,"safeguards":[{"type":"dangerous_tool_use","classifier_context":{"permission_mode":"auto"}}],"messages":[]}`
	const responseBody = `{"id":"msg_1","type":"message","safeguard_results":[{"type":"dangerous_tool_use","status":{"type":"available","tool_uses":{"toolu_1":{"type":"evaluated","outcome":"not_flagged"}}}}]}`
	var gotBody string
	var gotBeta string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read upstream body: %v", err)
		}
		gotBody = string(body)
		gotBeta = r.Header.Get("Anthropic-Beta")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, responseBody)
	}))
	defer upstream.Close()
	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}

	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	handler := Server{
		ClaudeUpstream: upstreamURL,
		Accounts: []accounts.Account{{
			ID: "claude@example.com", Provider: accounts.ProviderClaude,
			AuthMode: accounts.AuthModeOAuth, Token: "oauth-token",
		}},
		Sessions:     store,
		MaxBodyBytes: 1 << 20,
	}.Handler()
	req := httptest.NewRequest(http.MethodPost, "http://subrouter.local/v1/messages", strings.NewReader(requestBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Anthropic-Version", "2023-06-01")
	req.Header.Add("Anthropic-Beta", "dangerous-tool-use-2026-09-03")
	req.Header.Add("Anthropic-Beta", "custom-future-beta")
	req.Header.Set("X-Subrouter-Agent", "claude")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, req)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if gotBody != requestBody {
		t.Fatalf("request body changed in transit:\n got %s\nwant %s", gotBody, requestBody)
	}
	if gotBeta != "dangerous-tool-use-2026-09-03,custom-future-beta,oauth-2025-04-20" {
		t.Fatalf("Anthropic-Beta = %q, want incoming values plus OAuth beta", gotBeta)
	}
	if response.Body.String() != responseBody {
		t.Fatalf("safeguard response changed in transit:\n got %s\nwant %s", response.Body.String(), responseBody)
	}
}
