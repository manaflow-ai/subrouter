package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
)

// codexWorkspaceRouting applies Codex 0.162's acceptance rules for a routing
// discovery reply (app-server account_processor/workspace_routing.rs): the
// selected workspace appears exactly once, with an https origin and a known
// routing override. It returns why Codex would fail account/read, or "".
func codexWorkspaceRouting(t *testing.T, body []byte, selected string) string {
	t.Helper()
	var reply struct {
		Accounts []struct {
			ID       string  `json:"id"`
			Origin   *string `json:"workspace_backend_origin"`
			Override *string `json:"account_routing_override"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal(body, &reply); err != nil {
		return "invalid accounts response"
	}
	matches := 0
	for _, account := range reply.Accounts {
		if account.ID != selected {
			continue
		}
		matches++
		if account.Origin == nil {
			return "workspace routing discovery missing backend origin"
		}
		switch {
		case account.Override == nil:
			return "invalid account routing override"
		case *account.Override != "NO_CONSTRAINT" && *account.Override != "us" && *account.Override != "us_cr":
			return "invalid account routing override"
		}
		origin, err := url.Parse(*account.Origin)
		if err != nil || origin.Scheme != "https" || origin.Host == "" || origin.User != nil || (origin.Path != "" && origin.Path != "/") {
			return "workspace backend must use an HTTPS origin without credentials"
		}
	}
	switch matches {
	case 0:
		return "selected workspace missing from routing discovery"
	case 1:
		return ""
	}
	return "duplicate workspace in routing discovery"
}

// Forwarded upstream, discovery runs on a pool account's credentials and
// lists that account's workspaces, so Codex app-server failed account/read
// for the workspace its own auth.json selected. subrouter answers with the
// selected workspace and never sends the request upstream.
func TestCodexWorkspaceDiscoveryIncludesTheSelectedWorkspace(t *testing.T) {
	var upstreamCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"accounts":[{"id":"pool-workspace","workspace_backend_origin":"https://chatgpt.com","account_routing_override":"NO_CONSTRAINT"}]}`))
	}))
	defer upstream.Close()
	handler := pluginTestServer(t, upstream)

	for _, path := range []string{"/backend-api/wham/accounts/check", "/backend-api/api/codex/accounts/check"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer caller-token")
		req.Header.Set("ChatGPT-Account-Id", "a1b2c3d4-0000-4000-8000-selected0001")
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("%s: status %d, body %s", path, rr.Code, rr.Body.String())
		}
		if reason := codexWorkspaceRouting(t, rr.Body.Bytes(), "a1b2c3d4-0000-4000-8000-selected0001"); reason != "" {
			t.Fatalf("%s: Codex would fail account/read: %s; body %s", path, reason, rr.Body.String())
		}
	}
	if got := upstreamCalls.Load(); got != 0 {
		t.Fatalf("discovery reached upstream %d times on a pool account's credentials", got)
	}
}

// Without one plausible selected workspace there is nothing to answer for;
// the request goes upstream as before.
func TestCodexWorkspaceDiscoveryWithoutASelectedWorkspaceIsForwarded(t *testing.T) {
	var upstreamCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		_, _ = w.Write([]byte(`{"accounts":[]}`))
	}))
	defer upstream.Close()
	handler := pluginTestServer(t, upstream)

	for name, header := range map[string]string{"absent": "", "malformed": `x"},{"id":"y`} {
		req := httptest.NewRequest(http.MethodGet, "/backend-api/wham/accounts/check", nil)
		if header != "" {
			req.Header.Set("ChatGPT-Account-Id", header)
		}
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)
		if rr.Header().Get("X-Subrouter-Reason") == "codex_workspace_discovery" {
			t.Fatalf("%s: answered locally without a selected workspace", name)
		}
	}
	if got := upstreamCalls.Load(); got != 2 {
		t.Fatalf("upstream calls = %d, want both requests forwarded", got)
	}
}
