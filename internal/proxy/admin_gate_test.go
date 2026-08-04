package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The open-remote-admin regression: with no AdminToken and no
// AccountImportToken configured, authorizeAdmin used to grant every REMOTE
// caller the full admin surface — accounts, transcripts, account-import,
// drain — the moment the daemon was bound beyond loopback. Tokenless remote
// admin now requires the explicit AllowUnauthenticatedAdmin opt-in.
func TestTokenlessRemoteAdminRequiresExplicitOptIn(t *testing.T) {
	cases := []struct {
		name       string
		server     Server
		remoteAddr string
		forwarded  string
		want       bool
	}{
		{
			name:       "tokenless remote is denied by default",
			server:     Server{},
			remoteAddr: "100.64.0.9:52011",
			want:       false,
		},
		{
			name:       "explicit opt-in restores the legacy open behavior",
			server:     Server{AllowUnauthenticatedAdmin: true},
			remoteAddr: "100.64.0.9:52011",
			want:       true,
		},
		{
			name: "a scoped import token is never the only barrier, even opted in",
			server: Server{
				AllowUnauthenticatedAdmin: true,
				AccountImportToken:        "import-only",
			},
			remoteAddr: "100.64.0.9:52011",
			want:       false,
		},
		{
			name:       "loopback keeps unconditional admin without any opt-in",
			server:     Server{},
			remoteAddr: "127.0.0.1:40000",
			want:       true,
		},
		{
			name:       "spoofed forwarded header cannot claim loopback",
			server:     Server{},
			remoteAddr: "100.64.0.9:52011",
			forwarded:  "127.0.0.1",
			want:       false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", "http://example/_subrouter/accounts", nil)
			req.RemoteAddr = tc.remoteAddr
			if tc.forwarded != "" {
				req.Header.Set("X-Forwarded-For", tc.forwarded)
				req.Header.Set("X-Real-IP", tc.forwarded)
			}
			if got := tc.server.authorizeAdmin(req); got != tc.want {
				t.Fatalf("authorizeAdmin = %v, want %v", got, tc.want)
			}
		})
	}
}

// loopbackAdminRequest builds a request whose peer is the daemon host itself,
// which is how the operator-facing admin endpoints are reached in the
// deployments these tests model. httptest.NewRequest's default RemoteAddr
// (192.0.2.1) is a REMOTE peer, and tokenless remote admin now fails closed —
// tests exercising admin endpoint logic must say which peer they mean.
func loopbackAdminRequest(method, target string, body io.Reader) *http.Request {
	req := httptest.NewRequest(method, target, body)
	req.RemoteAddr = "127.0.0.1:54321"
	return req
}
