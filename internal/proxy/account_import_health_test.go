package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	agentclaude "github.com/manaflow-ai/subrouter/internal/agents/claude"
)

func TestHealthReportsAccountImportState(t *testing.T) {
	for _, tc := range []struct {
		name        string
		adminToken  string
		importToken string
		want        string
	}{
		{name: "no credential configured", want: AccountImportDisabled},
		{name: "admin token", adminToken: "secret", want: AccountImportEnabled},
		{name: "import token", importToken: "secret", want: AccountImportEnabled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ref := NewAccountRef(accounts.CodexStore{Dir: t.TempDir()}, nil, nil)
			ref.claudeStore = agentclaude.Store{Dir: t.TempDir()}
			handler := Server{
				AccountRef:         ref,
				AdminToken:         tc.adminToken,
				AccountImportToken: tc.importToken,
			}.Handler()

			req := httptest.NewRequest(http.MethodGet, "/_subrouter/health", nil)
			req.RemoteAddr = "100.64.0.20:4321"
			resp := httptest.NewRecorder()
			handler.ServeHTTP(resp, req)

			if resp.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", resp.Code)
			}
			var body struct {
				OK            bool   `json:"ok"`
				AccountImport string `json:"account_import"`
			}
			if err := json.Unmarshal(resp.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode health: %v (%s)", err, resp.Body.String())
			}
			if !body.OK {
				t.Fatalf("health ok = false, body = %s", resp.Body.String())
			}
			if body.AccountImport != tc.want {
				t.Fatalf("account_import = %q, want %q", body.AccountImport, tc.want)
			}
		})
	}
}

// A disabled report must mean the endpoint actually rejects imports, so the
// health field and the authorization rule cannot drift apart.
func TestHealthAccountImportStateMatchesAuthorization(t *testing.T) {
	ref := NewAccountRef(accounts.CodexStore{Dir: t.TempDir()}, nil, nil)
	ref.claudeStore = agentclaude.Store{Dir: t.TempDir()}
	server := Server{AccountRef: ref}
	if server.AccountImportState() != AccountImportDisabled {
		t.Fatalf("state = %q, want %q", server.AccountImportState(), AccountImportDisabled)
	}

	req := httptest.NewRequest(http.MethodGet, "/_subrouter/account-import", nil)
	req.RemoteAddr = "100.64.0.20:4321"
	resp := httptest.NewRecorder()
	server.Handler().ServeHTTP(resp, req)
	if resp.Code != http.StatusUnauthorized {
		t.Fatalf("import status = %d, want 401 while state reports disabled", resp.Code)
	}
}
