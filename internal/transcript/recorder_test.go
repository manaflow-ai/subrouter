package transcript

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRecorderScopesFilesAndFieldsByAgentType(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "transcripts")
	recorder := NewRecorder(dir)

	recorder.RecordPayload("codex", "same-session:0", "http_body", "client_to_upstream", []byte("codex-body"), nil)
	recorder.RecordPayload("claude", "same-session:0", "http_body", "client_to_upstream", []byte("claude-body"), nil)

	if err := recorder.Flush(); err != nil {
		t.Fatal(err)
	}
	codexPayload := readFirstPayload(t, recorder.PathForSession("codex", "same-session:0"))
	claudePayload := readFirstPayload(t, recorder.PathForSession("claude", "same-session:0"))

	if got := codexPayload["agent_type"]; got != "codex" {
		t.Fatalf("codex agent_type = %v, want codex", got)
	}
	if got := claudePayload["agent_type"]; got != "claude" {
		t.Fatalf("claude agent_type = %v, want claude", got)
	}
	if got := codexPayload["agent_session_id"]; got != "same-session" {
		t.Fatalf("codex agent_session_id = %v, want same-session", got)
	}
	if got := claudePayload["agent_session_id"]; got != "same-session" {
		t.Fatalf("claude agent_session_id = %v, want same-session", got)
	}
	if got := codexPayload["codex_session_id"]; got != "same-session" {
		t.Fatalf("codex_session_id = %v, want same-session", got)
	}
	if _, ok := claudePayload["codex_session_id"]; ok {
		t.Fatal("claude payload unexpectedly has codex_session_id")
	}
	assertBody(t, codexPayload, "codex-body")
	assertBody(t, claudePayload, "claude-body")
}

func readFirstPayload(t *testing.T, path string) map[string]any {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	line := strings.Split(strings.TrimSpace(string(body)), "\n")[0]
	var event struct {
		Payload map[string]any `json:"payload"`
	}
	if err := json.Unmarshal([]byte(line), &event); err != nil {
		t.Fatal(err)
	}
	return event.Payload
}

func assertBody(t *testing.T, payload map[string]any, want string) {
	t.Helper()
	got, err := base64.StdEncoding.DecodeString(payload["body_base64"].(string))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Fatalf("body = %q, want %q", got, want)
	}
}
