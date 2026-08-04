package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/manaflow-ai/subrouter/internal/transcript"
	"github.com/manaflow-ai/subrouter/session"
)

func TestDashboardAndTranscriptEndpoints(t *testing.T) {
	transcripts := transcript.NewRecorder(filepath.Join(t.TempDir(), "transcripts"))
	transcripts.RecordMeta("codex", "session-1:0", map[string]any{
		"user":    "user@example.com",
		"account": "carol@example.com",
	})
	transcripts.RecordPayload("codex", "session-1:0", "http_body", "client_to_upstream", []byte("secret body"), nil)
	transcripts.RecordPayload("codex", "session-1:0", "http_body", "upstream_to_client", []byte(`{"response":{"model":"gpt-5.5","usage":{"input_tokens":100,"output_tokens":7,"total_tokens":107}}}`), nil)

	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put("codex", "session-1:0", "carol@example.com", "user@example.com"); err != nil {
		t.Fatal(err)
	}

	server := Server{Sessions: store, Transcripts: transcripts}
	handler := server.Handler()

	dashboard := httptest.NewRecorder()
	handler.ServeHTTP(dashboard, loopbackAdminRequest(http.MethodGet, "/_subrouter/dashboard", nil))
	if dashboard.Code != http.StatusOK {
		t.Fatalf("dashboard status = %d", dashboard.Code)
	}
	if !strings.Contains(dashboard.Body.String(), "session-1") {
		t.Fatal("dashboard did not include transcript session")
	}
	if !strings.Contains(dashboard.Body.String(), "Total Tokens") || !strings.Contains(dashboard.Body.String(), "107") {
		t.Fatal("dashboard did not include token usage")
	}

	list := httptest.NewRecorder()
	handler.ServeHTTP(list, loopbackAdminRequest(http.MethodGet, "/_subrouter/transcripts", nil))
	if list.Code != http.StatusOK {
		t.Fatalf("list status = %d", list.Code)
	}
	var summaries []transcript.Summary
	if err := json.Unmarshal(list.Body.Bytes(), &summaries); err != nil {
		t.Fatal(err)
	}
	if len(summaries) != 1 || summaries[0].SessionID != "session-1" {
		t.Fatalf("unexpected summaries: %+v", summaries)
	}
	if summaries[0].Usage.TotalTokens != 107 {
		t.Fatalf("summary usage = %+v", summaries[0].Usage)
	}

	detail := httptest.NewRecorder()
	handler.ServeHTTP(detail, loopbackAdminRequest(http.MethodGet, "/_subrouter/transcripts/codex/session-1", nil))
	if detail.Code != http.StatusOK {
		t.Fatalf("detail status = %d", detail.Code)
	}
	if strings.Contains(detail.Body.String(), "body_base64\"") {
		t.Fatal("detail response exposed body_base64")
	}
	if !strings.Contains(detail.Body.String(), "body_base64_redacted") {
		t.Fatal("detail response did not mark redacted body")
	}

	raw := httptest.NewRecorder()
	handler.ServeHTTP(raw, loopbackAdminRequest(http.MethodGet, "/_subrouter/transcripts/codex/session-1/raw", nil))
	if raw.Code != http.StatusOK {
		t.Fatalf("raw status = %d", raw.Code)
	}
	if !strings.Contains(raw.Body.String(), "secret body") {
		t.Fatal("raw response did not include decoded body text")
	}
}
