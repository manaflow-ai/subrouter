package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	agentclaude "github.com/manaflow-ai/subrouter/internal/agents/claude"
	"github.com/manaflow-ai/subrouter/internal/buildversion"
)

func TestHealthReportsBuildVersion(t *testing.T) {
	ref := NewAccountRef(accounts.CodexStore{Dir: t.TempDir()}, nil, nil)
	ref.claudeStore = agentclaude.Store{Dir: t.TempDir()}
	handler := Server{AccountRef: ref}.Handler()

	req := httptest.NewRequest(http.MethodGet, "/_subrouter/health", nil)
	req.RemoteAddr = "127.0.0.1:4321"
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.Code, resp.Body.String())
	}
	var body struct {
		OK      bool   `json:"ok"`
		Version string `json:"version"`
	}
	if err := json.Unmarshal(resp.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode health: %v (%s)", err, resp.Body.String())
	}
	if !body.OK {
		t.Fatalf("health ok = false: %s", resp.Body.String())
	}
	if want := buildversion.Version(); body.Version == "" || body.Version != want {
		t.Fatalf("health version = %q, want %q", body.Version, want)
	}
}
