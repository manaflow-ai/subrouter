package proxy

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	agentclaude "github.com/manaflow-ai/subrouter/internal/agents/claude"
)

// A client built after this server sends fields the server does not know.
// The 400 must name that field so either side can see the version drift.
func TestAccountImportNamesUnknownFieldInRejection(t *testing.T) {
	codexStore := accounts.CodexStore{Dir: t.TempDir()}
	ref := NewAccountRef(codexStore, nil, nil)
	ref.claudeStore = agentclaude.Store{Dir: t.TempDir()}
	handler := Server{AccountRef: ref, AdminToken: "secret"}.Handler()
	account := proxyStoredOAuthAccount("owner@example.com", "fresh", time.Now().Add(time.Hour))
	raw, err := json.Marshal(account)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	fields["futureField"] = "from-a-newer-client"
	payload, err := json.Marshal(map[string]any{"provider": "codex", "codex": fields})
	if err != nil {
		t.Fatal(err)
	}

	resp := serveProtectedAccountImport(handler, payload)
	if resp.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body = %s", resp.Code, resp.Body.String())
	}
	body := resp.Body.String()
	if !strings.Contains(body, `unknown field "futureField"`) {
		t.Fatalf("rejection body = %q, want the unknown field named", body)
	}
	if strings.Contains(body, "from-a-newer-client") {
		t.Fatalf("rejection body echoed payload content: %q", body)
	}
	if len(ref.All()) != 0 {
		t.Fatal("rejected import loaded an account")
	}
}

func TestAccountImportDecodeReasonDoesNotEchoPayload(t *testing.T) {
	handler := Server{
		AccountRef: NewAccountRef(accounts.CodexStore{Dir: t.TempDir()}, nil, nil),
		AdminToken: "secret",
	}.Handler()
	resp := serveProtectedAccountImport(handler, []byte(`{"provider":"codex","codex":{"email":"secret-token-value"`))
	if resp.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body = %s", resp.Code, resp.Body.String())
	}
	body := resp.Body.String()
	if !strings.HasPrefix(body, "invalid account import body: malformed JSON") {
		t.Fatalf("rejection body = %q, want a malformed-JSON reason", body)
	}
	if strings.Contains(body, "secret-token-value") {
		t.Fatalf("rejection body echoed payload content: %q", body)
	}
}
