package proxy

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	agentclaude "github.com/manaflow-ai/subrouter/internal/agents/claude"
	"github.com/manaflow-ai/subrouter/internal/tailnet"
)

type stubTailnetAuth struct {
	identity tailnet.Identity
	allow    bool
	seen     string
}

func (s *stubTailnetAuth) Authorize(_ context.Context, remoteAddr string) (tailnet.Identity, bool) {
	s.seen = remoteAddr
	return s.identity, s.allow
}

func tailnetTestServer(t *testing.T, auth TailnetAuthorizer) Server {
	t.Helper()
	ref := NewAccountRef(accounts.CodexStore{Dir: t.TempDir()}, nil, nil)
	ref.claudeStore = agentclaude.Store{Dir: t.TempDir()}
	return Server{AccountRef: ref, TailnetAuth: auth}
}

func tailnetRequest(method, path string) *http.Request {
	req := httptest.NewRequest(method, path, nil)
	req.RemoteAddr = "100.82.214.112:52344"
	return req
}

// The whole point of the mode: no token anywhere, and a tailnet peer still gets
// in, on both the admin and the account-import surfaces.
func TestTailnetIdentityAuthorizesWithoutAnyToken(t *testing.T) {
	auth := &stubTailnetAuth{identity: tailnet.Identity{LoginName: "lawrence@manaflow.ai"}, allow: true}
	handler := tailnetTestServer(t, auth).Handler()

	for _, path := range []string{"/_subrouter/accounts", "/_subrouter/account-import"} {
		resp := httptest.NewRecorder()
		handler.ServeHTTP(resp, tailnetRequest(http.MethodGet, path))
		if resp.Code != http.StatusOK {
			t.Fatalf("%s status = %d, want 200: %s", path, resp.Code, resp.Body.String())
		}
	}
	if auth.seen != "100.82.214.112:52344" {
		t.Fatalf("authorizer saw %q, want the peer address", auth.seen)
	}
}

func TestNonTailnetPeerStillDeniedWithoutToken(t *testing.T) {
	handler := tailnetTestServer(t, &stubTailnetAuth{allow: false}).Handler()

	for _, path := range []string{"/_subrouter/accounts", "/_subrouter/account-import"} {
		resp := httptest.NewRecorder()
		handler.ServeHTTP(resp, tailnetRequest(http.MethodGet, path))
		if resp.Code != http.StatusUnauthorized {
			t.Fatalf("%s status = %d, want 401", path, resp.Code)
		}
	}
}

// Tailnet auth adds a way in; it must not disable the token path a mixed
// deployment may still rely on.
func TestConfiguredTokenStillWorksAlongsideTailnetAuth(t *testing.T) {
	server := tailnetTestServer(t, &stubTailnetAuth{allow: false})
	server.AdminToken = "admin-secret"
	handler := server.Handler()

	req := tailnetRequest(http.MethodGet, "/_subrouter/accounts")
	req.Header.Set("Authorization", "Bearer admin-secret")
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 for a valid admin token", resp.Code)
	}
}

// A server with no tailnet auth configured must behave exactly as before, which
// is what keeps this change out of the cloud deployment's path.
func TestDisabledTailnetAuthLeavesTokenRulesUnchanged(t *testing.T) {
	server := tailnetTestServer(t, nil)
	server.AccountImportToken = "import-secret"
	handler := server.Handler()

	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, tailnetRequest(http.MethodGet, "/_subrouter/accounts"))
	if resp.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 when only an import token is configured", resp.Code)
	}
	if server.AuthMode() != "token" {
		t.Fatalf("auth mode = %q, want token", server.AuthMode())
	}
}

func TestHealthReportsTailnetAuthMode(t *testing.T) {
	server := tailnetTestServer(t, &stubTailnetAuth{allow: true})
	resp := httptest.NewRecorder()
	server.Handler().ServeHTTP(resp, tailnetRequest(http.MethodGet, "/_subrouter/health"))

	var body struct {
		Auth          string `json:"auth"`
		AccountImport string `json:"account_import"`
	}
	if err := json.Unmarshal(resp.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode health: %v (%s)", err, resp.Body.String())
	}
	if body.Auth != "tailnet" {
		t.Fatalf("auth = %q, want tailnet", body.Auth)
	}
	// Import works in this mode, so health must not report it as disabled.
	if body.AccountImport != AccountImportEnabled {
		t.Fatalf("account_import = %q, want %q", body.AccountImport, AccountImportEnabled)
	}
}
