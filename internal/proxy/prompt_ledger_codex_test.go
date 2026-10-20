package proxy

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/attribution"
	"github.com/manaflow-ai/subrouter/internal/tailnet"
)

// A Codex WebSocket response.create frame from the client is attributed to
// the tailnet peer of the socket.
func TestCodexWebSocketCreateIsAttributedToTailnetPeer(t *testing.T) {
	auth := &stubTailnetAuth{identity: tailnet.Identity{LoginName: "leo@manaflow.ai", NodeName: "leo-mbp"}, allow: true}
	server, ledger := promptLedgerServer(t, auth, "lawrence@manaflow.ai")
	state := &webSocketModelState{peerAddr: "100.70.1.3:7000"}
	server.submitWebSocketPromptAttribution(state, "codex", "sess-ws", "gpt-6", "", []byte(`{"type":"response.create","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"profile codex"}]}]}`))
	ledger.Flush()
	entries, _ := ledger.Query(attribution.Filter{Since: time.Now().Add(-time.Hour), Until: time.Now().Add(time.Hour)})
	if len(entries) != 1 || entries[0].Person != "leo@manaflow.ai" || entries[0].Prompt != "profile codex" || entries[0].Path != "websocket" {
		t.Fatalf("entries = %+v", entries)
	}
}

// The Claude-to-Codex bridge (agent claude-codex) returns before the main
// hook; its prompts must still be recorded.
func TestClaudeCodexBridgePromptIsAttributed(t *testing.T) {
	auth := &stubTailnetAuth{identity: tailnet.Identity{LoginName: "aziz@manaflow.ai", NodeName: "aziz-mbp"}, allow: true}
	server, ledger := promptLedgerServer(t, auth, "lawrence@manaflow.ai")
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"gpt-6","messages":[{"role":"user","content":"bridge ask"}]}`))
	req.RemoteAddr = "100.70.1.4:1"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Subrouter-Agent", "claude-codex")
	req.Header.Set("X-Subrouter-Session", "bridge-1")
	server.Handler().ServeHTTP(httptest.NewRecorder(), req)
	ledger.Flush()
	entries, _ := ledger.Query(attribution.Filter{Since: time.Now().Add(-time.Hour), Until: time.Now().Add(time.Hour)})
	if len(entries) != 1 || entries[0].Prompt != "bridge ask" || entries[0].Person != "aziz@manaflow.ai" {
		t.Fatalf("entries = %+v", entries)
	}
}
