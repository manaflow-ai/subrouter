package session

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestExtractRoutingIDUsesHeadOnly(t *testing.T) {
	claude := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(`{"session_id":"body"}`))
	claude.Header.Set("X-Claude-Code-Session-Id", "claude-session")
	if got := ExtractRoutingID(claude); got != "claude-session" {
		t.Fatalf("claude = %q", got)
	}
	codex := httptest.NewRequest("POST", "/v1/responses", nil)
	codex.Header.Set("X-Codex-Window-ID", "thread-1:3")
	if got := ExtractRoutingID(codex); got != "thread-1" {
		t.Fatalf("codex window = %q", got)
	}
	query := httptest.NewRequest("GET", "/v1/responses?session_id=q", nil)
	if got := ExtractRoutingID(query); got != "q" {
		t.Fatalf("query = %q", got)
	}
	body := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{"session_id":"body"}`))
	if got := ExtractRoutingID(body); got != "" {
		t.Fatalf("body-only session = %q, want none", got)
	}
	idempotent := httptest.NewRequest("POST", "/v1/messages", nil)
	idempotent.Header.Set("Idempotency-Key", "per-request")
	if got := ExtractRoutingID(idempotent); got != "" {
		t.Fatalf("Idempotency-Key used as a session: %q", got)
	}
	if got := ExtractRoutingID(nil); got != "" {
		t.Fatalf("nil = %q", got)
	}
}
