package session

import (
	"io"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestExtractIDFromHeader(t *testing.T) {
	req := httptest.NewRequest("POST", "/v1/responses", nil)
	req.Header.Set("X-Subrouter-Session", "conv-123")

	if got := ExtractID(req, 1024); got != "conv-123" {
		t.Fatalf("got %q, want conv-123", got)
	}
}

func TestExtractIDFromClaudeCodeHeader(t *testing.T) {
	req := httptest.NewRequest("POST", "/v1/messages", nil)
	req.Header.Set("X-Claude-Code-Session-Id", "claude-session")

	if got := ExtractID(req, 1024); got != "claude-session" {
		t.Fatalf("got %q, want claude-session", got)
	}
}

func TestExtractIDFromJSONAndRestoresBody(t *testing.T) {
	req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{"metadata":{"session_id":"s1"},"input":"hello"}`))
	req.Header.Set("Content-Type", "application/json")

	if got := ExtractID(req, 1024); got != "s1" {
		t.Fatalf("got %q, want s1", got)
	}

	body, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != `{"metadata":{"session_id":"s1"},"input":"hello"}` {
		t.Fatalf("body was not restored: %s", body)
	}
}

func TestExtractUserEmailFromSubrouterHeader(t *testing.T) {
	req := httptest.NewRequest("POST", "/v1/responses", nil)
	req.Header.Set("X-Subrouter-User-Email", "Alice <Alice@Example.COM>")

	if got := ExtractUserEmail(req); got != "alice@example.com" {
		t.Fatalf("got %q, want alice@example.com", got)
	}
}

func TestExtractUserEmailIgnoresInvalidHeader(t *testing.T) {
	req := httptest.NewRequest("POST", "/v1/responses", nil)
	req.Header.Set("X-Subrouter-User-Email", "not an email")

	if got := ExtractUserEmail(req); got != "" {
		t.Fatalf("got %q, want empty", got)
	}
}

func TestExtractAccountIDFromSubrouterHeader(t *testing.T) {
	req := httptest.NewRequest("POST", "/v1/responses", nil)
	req.Header.Set("X-Subrouter-Account-ID", " team-codex-1 ")

	if got := ExtractAccountID(req); got != "team-codex-1" {
		t.Fatalf("got %q, want team-codex-1", got)
	}
}

func TestExtractModelFromHeader(t *testing.T) {
	req := httptest.NewRequest("POST", "/v1/responses", nil)
	req.Header.Set("X-Subrouter-Model", " GPT-5.3-Codex-Spark ")

	if got := ExtractModel(req, 1024); got != "GPT-5.3-Codex-Spark" {
		t.Fatalf("got %q, want GPT-5.3-Codex-Spark", got)
	}
}

func TestExtractModelFromJSONAndRestoresBody(t *testing.T) {
	body := `{"model":"GPT-5.3-Codex-Spark","input":"hello"}`
	req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	if got := ExtractModel(req, 1024); got != "GPT-5.3-Codex-Spark" {
		t.Fatalf("got %q, want GPT-5.3-Codex-Spark", got)
	}
	restored, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatal(err)
	}
	if string(restored) != body {
		t.Fatalf("body was not restored: %s", restored)
	}
}

func TestExtractModelFromLargeJSONScanAndRestoresBody(t *testing.T) {
	body := `{"tools":[{"input_schema":"` + strings.Repeat("x", 2048) + `"}],"model":"claude-fable-5","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	if got := ExtractModel(req, 128); got != "claude-fable-5" {
		t.Fatalf("got %q, want claude-fable-5", got)
	}
	restored, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatal(err)
	}
	if string(restored) != body {
		t.Fatalf("body was not restored")
	}
}

func TestExtractAgentTypeFromSubrouterHeader(t *testing.T) {
	req := httptest.NewRequest("POST", "/v1/responses", nil)
	req.Header.Set("X-Subrouter-Agent", "Claude")
	req.Header.Set("X-Codex-Session-ID", "codex-session")

	if got := ExtractAgentType(req); got != "claude" {
		t.Fatalf("got %q, want claude", got)
	}
}

func TestExtractAgentTypeFromProviderHeaders(t *testing.T) {
	tests := []struct {
		name   string
		header string
		want   string
	}{
		{name: "codex", header: "X-Codex-Window-ID", want: "codex"},
		{name: "claude", header: "Anthropic-Conversation-ID", want: "claude"},
		{name: "claude-code", header: "X-Claude-Code-Session-Id", want: "claude"},
		{name: "gemini", header: "X-Gemini-Session-ID", want: "gemini"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			req := httptest.NewRequest("POST", "/v1/responses", nil)
			req.Header.Set(test.header, "session-1")

			if got := ExtractAgentType(req); got != test.want {
				t.Fatalf("got %q, want %q", got, test.want)
			}
		})
	}
}

func TestExtractAgentTypeFromClaudeCodeUserAgent(t *testing.T) {
	req := httptest.NewRequest("HEAD", "/", nil)
	req.Header.Set("User-Agent", "claude-cli/2.1.141 (external, sdk-cli)")

	if got := ExtractAgentType(req); got != "claude" {
		t.Fatalf("got %q, want claude", got)
	}
}

func TestStripSubrouterHeaders(t *testing.T) {
	req := httptest.NewRequest("POST", "/v1/responses", nil)
	req.Header.Set("X-Subrouter-Session", "session-1")
	req.Header.Set("X-Subrouter-Agent", "claude")
	req.Header.Set("X-Subrouter-User-Email", "alice@example.com")
	req.Header.Set("X-Subrouter-User", "alice@example.com")
	req.Header.Set("X-User-Email", "alice@example.com")
	req.Header.Set("X-Subrouter-Account-ID", "apikey:paid")
	req.Header.Set("X-Subrouter-Account", "paid")
	req.Header.Set("X-Subrouter-Model", "GPT-5.3-Codex-Spark")
	req.Header.Set("X-Subrouter-Admin-Token", "admin-secret")
	req.Header.Set("X-Model", "GPT-5.3-Codex-Spark")
	req.Header.Set("X-Other", "keep")

	StripSubrouterHeaders(req.Header)

	if got := req.Header.Get("X-Subrouter-Session"); got != "" {
		t.Fatalf("X-Subrouter-Session = %q, want empty", got)
	}
	if got := req.Header.Get("X-Subrouter-Agent"); got != "" {
		t.Fatalf("X-Subrouter-Agent = %q, want empty", got)
	}
	if got := req.Header.Get("X-Subrouter-User-Email"); got != "" {
		t.Fatalf("X-Subrouter-User-Email = %q, want empty", got)
	}
	if got := req.Header.Get("X-Subrouter-User"); got != "" {
		t.Fatalf("X-Subrouter-User = %q, want empty", got)
	}
	if got := req.Header.Get("X-User-Email"); got != "" {
		t.Fatalf("X-User-Email = %q, want empty", got)
	}
	if got := req.Header.Get("X-Subrouter-Account-ID"); got != "" {
		t.Fatalf("X-Subrouter-Account-ID = %q, want empty", got)
	}
	if got := req.Header.Get("X-Subrouter-Account"); got != "" {
		t.Fatalf("X-Subrouter-Account = %q, want empty", got)
	}
	if got := req.Header.Get("X-Subrouter-Model"); got != "" {
		t.Fatalf("X-Subrouter-Model = %q, want empty", got)
	}
	if got := req.Header.Get("X-Subrouter-Admin-Token"); got != "" {
		t.Fatalf("X-Subrouter-Admin-Token = %q, want empty", got)
	}
	if got := req.Header.Get("X-Model"); got != "" {
		t.Fatalf("X-Model = %q, want empty", got)
	}
	if got := req.Header.Get("X-Other"); got != "keep" {
		t.Fatalf("X-Other = %q, want keep", got)
	}
}
