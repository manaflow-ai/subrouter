package proxy

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	agentclaude "github.com/manaflow-ai/subrouter/internal/agents/claude"
	"github.com/manaflow-ai/subrouter/internal/tailnet"
	"github.com/manaflow-ai/subrouter/session"
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

// A server whose cloud config carries a local proxy secret still serves its
// tailnet peers: clients send that secret only to a loopback base URL, so a
// remote peer can never present it. A loopback caller and an unverified remote
// caller must still present it.
func TestLocalProxyTokenAdmitsVerifiedTailnetPeerOnly(t *testing.T) {
	for _, tc := range []struct {
		name       string
		remoteAddr string
		allow      bool
		bearer     string
		wantAuth   bool
	}{
		{name: "verified tailnet peer", remoteAddr: "100.120.161.125:51000", allow: true, bearer: "subrouter", wantAuth: true},
		{name: "verified tailnet peer without authorization", remoteAddr: "100.120.161.125:51000", allow: true, wantAuth: true},
		{name: "unverified remote peer", remoteAddr: "192.168.86.60:51000", allow: false, bearer: "subrouter", wantAuth: false},
		{name: "loopback without secret", remoteAddr: "127.0.0.1:51000", allow: true, bearer: "subrouter", wantAuth: false},
		{name: "loopback with secret", remoteAddr: "127.0.0.1:51000", allow: false, bearer: "local-secret", wantAuth: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			auth := &stubTailnetAuth{identity: tailnet.Identity{LoginName: "daniel@example.com"}, allow: tc.allow}
			server := Server{LocalProxyToken: "local-secret", TailnetAuth: auth}
			req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
			req.RemoteAddr = tc.remoteAddr
			if tc.bearer != "" {
				req.Header.Set("Authorization", "Bearer "+tc.bearer)
			}
			if got := server.localProxyAuthorized(req); got != tc.wantAuth {
				t.Fatalf("localProxyAuthorized = %v, want %v", got, tc.wantAuth)
			}
		})
	}
}

func TestLocalProxyTokenStillRequiredWithoutTailnetAuth(t *testing.T) {
	server := Server{LocalProxyToken: "local-secret"}
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	req.RemoteAddr = "100.120.161.125:51000"
	req.Header.Set("Authorization", "Bearer subrouter")
	if server.localProxyAuthorized(req) {
		t.Fatal("a remote caller without the secret was admitted with tailnet auth off")
	}
}

// A session that started before the machine had a local proxy secret keeps
// working without a restart, but only for a session this server already routes
// and only with the placeholder sr sent before the secret existed.
func TestKnownSessionSkipsLocalProxyToken(t *testing.T) {
	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	const known = "08fb81d8-38a3-4b20-8bcc-c34ba2a66ffb"
	if _, err := store.Put("claude", known, "acct", ""); err != nil {
		t.Fatal(err)
	}
	server := Server{LocalProxyToken: "local-secret", Sessions: store}
	for _, tc := range []struct {
		name       string
		remoteAddr string
		sessionID  string
		bearer     string
		want       bool
	}{
		{name: "known session with placeholder", remoteAddr: "127.0.0.1:5000", sessionID: known, bearer: "subrouter", want: true},
		{name: "unknown session", remoteAddr: "127.0.0.1:5000", sessionID: "11111111-2222-3333-4444-555555555555", bearer: "subrouter", want: false},
		{name: "no session id", remoteAddr: "127.0.0.1:5000", bearer: "subrouter", want: false},
		{name: "known session with other token", remoteAddr: "127.0.0.1:5000", sessionID: known, bearer: "guess", want: false},
		{name: "known session from remote", remoteAddr: "192.168.86.60:5000", sessionID: known, bearer: "subrouter", want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
			req.RemoteAddr = tc.remoteAddr
			req.Header.Set("User-Agent", "claude-cli/2.1.283 (external, cli)")
			req.Header.Set("Authorization", "Bearer "+tc.bearer)
			if tc.sessionID != "" {
				req.Header.Set("X-Claude-Code-Session-Id", tc.sessionID)
			}
			if got := server.localProxyAuthorized(req); got != tc.want {
				t.Fatalf("localProxyAuthorized = %v, want %v", got, tc.want)
			}
		})
	}
}
