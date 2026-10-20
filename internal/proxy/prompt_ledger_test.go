package proxy

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/attribution"
	"github.com/manaflow-ai/subrouter/internal/tailnet"
	"github.com/manaflow-ai/subrouter/session"
)

func promptLedgerServer(t *testing.T, auth TailnetAuthorizer, readers ...string) (Server, *attribution.Ledger) {
	t.Helper()
	server := tailnetTestServer(t, auth)
	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	server.Sessions = store
	ledger, err := attribution.Open(filepath.Join(t.TempDir(), "prompts"), attribution.Options{
		Identify: TailnetIdentify(auth),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ledger.Close() })
	server.PromptLedger = ledger
	server.PromptLedgerReaders = readers
	return server, ledger
}

// The person comes from the tailnet peer behind the connection, never from a
// header the client chose.
func TestClaudePromptIsAttributedToTailnetPeer(t *testing.T) {
	auth := &stubTailnetAuth{identity: tailnet.Identity{LoginName: "aziz@manaflow.ai", NodeName: "aziz-mbp"}, allow: true}
	server, ledger := promptLedgerServer(t, auth, "lawrence@manaflow.ai")
	handler := server.Handler()

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-opus-4","messages":[{"role":"user","content":"profile the tab switch"}]}`))
	req.RemoteAddr = "100.70.1.2:5555"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Subrouter-Agent", "claude")
	req.Header.Set("X-Subrouter-Session", "sess-1")
	req.Header.Set("X-Subrouter-User-Email", "lawrence@manaflow.ai")
	handler.ServeHTTP(httptest.NewRecorder(), req)
	ledger.Flush()

	entries, err := ledger.Query(attribution.Filter{Since: time.Now().Add(-time.Hour), Until: time.Now().Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("entries = %+v, want 1", entries)
	}
	if entries[0].Person != "aziz@manaflow.ai" || entries[0].Node != "aziz-mbp" || entries[0].Prompt != "profile the tab switch" {
		t.Fatalf("entry = %+v", entries[0])
	}
}

func TestPromptsEndpointIsReaderOnly(t *testing.T) {
	now := time.Now()
	reader := &stubTailnetAuth{identity: tailnet.Identity{LoginName: "lawrence@manaflow.ai"}, allow: true}
	server, ledger := promptLedgerServer(t, reader, "lawrence@manaflow.ai")
	ledger.Submit(attribution.Request{SessionID: "s", Path: "/v1/messages"}, []byte(`{"messages":[{"role":"user","content":"hello"}]}`))
	ledger.Flush()

	resp := httptest.NewRecorder()
	server.Handler().ServeHTTP(resp, tailnetRequest(http.MethodGet, "/_subrouter/prompts?since=1h&person=lawrence"))
	if resp.Code != http.StatusOK {
		t.Fatalf("reader status = %d: %s", resp.Code, resp.Body.String())
	}
	var body struct {
		Entries  []attribution.Entry `json:"entries"`
		ByPerson map[string]int      `json:"by_person"`
	}
	if err := json.Unmarshal(resp.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Entries) != 1 || body.ByPerson["lawrence@manaflow.ai"] != 1 {
		t.Fatalf("body = %+v (now %v)", body, now)
	}

	// Any other tailnet member is an admin on this gateway, but must not read prompts.
	teammate := &stubTailnetAuth{identity: tailnet.Identity{LoginName: "leo@manaflow.ai"}, allow: true}
	other, _ := promptLedgerServer(t, teammate, "lawrence@manaflow.ai")
	resp = httptest.NewRecorder()
	other.Handler().ServeHTTP(resp, tailnetRequest(http.MethodGet, "/_subrouter/prompts"))
	if resp.Code != http.StatusForbidden {
		t.Fatalf("non-reader status = %d, want 403", resp.Code)
	}
}

var _ = context.Background
