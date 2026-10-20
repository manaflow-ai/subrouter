package proxy

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/manaflow-ai/subrouter/internal/tailnet"
)

// On a tailnet-auth server every peer is an admin, but transcripts hold every
// person's prompts: only prompt readers and the admin token may read them.
func TestTranscriptsAreReaderOnlyUnderTailnetAuth(t *testing.T) {
	paths := transcriptReaderPaths()
	if len(paths) == 0 {
		t.Fatal("no transcript paths")
	}
	teammate := tailnetTestServer(t, &stubTailnetAuth{identity: tailnet.Identity{LoginName: "leo@manaflow.ai"}, allow: true})
	teammate.PromptLedgerReaders = []string{"lawrence@manaflow.ai"}
	reader := tailnetTestServer(t, &stubTailnetAuth{identity: tailnet.Identity{LoginName: "lawrence@manaflow.ai"}, allow: true})
	reader.PromptLedgerReaders = []string{"lawrence@manaflow.ai"}
	token := tailnetTestServer(t, &stubTailnetAuth{allow: false})
	token.AdminToken = "admin-secret"
	for _, path := range paths {
		resp := httptest.NewRecorder()
		teammate.Handler().ServeHTTP(resp, tailnetRequest(http.MethodGet, path))
		if resp.Code != http.StatusForbidden {
			t.Fatalf("teammate %s = %d, want 403", path, resp.Code)
		}
		resp = httptest.NewRecorder()
		reader.Handler().ServeHTTP(resp, tailnetRequest(http.MethodGet, path))
		if resp.Code == http.StatusForbidden || resp.Code == http.StatusUnauthorized {
			t.Fatalf("reader %s = %d", path, resp.Code)
		}
		req := tailnetRequest(http.MethodGet, path)
		req.Header.Set("Authorization", "Bearer admin-secret")
		resp = httptest.NewRecorder()
		token.Handler().ServeHTTP(resp, req)
		if resp.Code == http.StatusForbidden || resp.Code == http.StatusUnauthorized {
			t.Fatalf("admin token %s = %d", path, resp.Code)
		}
	}
	// Other admin routes stay open to every tailnet peer.
	resp := httptest.NewRecorder()
	teammate.Handler().ServeHTTP(resp, tailnetRequest(http.MethodGet, "/_subrouter/accounts"))
	if resp.Code != http.StatusOK {
		t.Fatalf("teammate accounts = %d, want 200", resp.Code)
	}
}
