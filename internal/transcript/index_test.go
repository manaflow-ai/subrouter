package transcript

import (
	"path/filepath"
	"testing"
)

func TestListSummariesAndReadSanitizedSession(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "transcripts")
	recorder := NewRecorder(dir)
	recorder.RecordMeta("codex", "session-1:0", map[string]any{
		"user":    "user@example.com",
		"account": "carol@example.com",
	})
	recorder.RecordPayload("codex", "session-1:0", "http_body", "client_to_upstream", []byte("secret body"), nil)
	recorder.RecordPayload("codex", "session-1:0", "http_body", "upstream_to_client", []byte(`{"response":{"model":"gpt-5.5","usage":{"input_tokens":100,"input_tokens_details":{"cached_tokens":20},"output_tokens":7,"output_tokens_details":{"reasoning_tokens":3},"total_tokens":107}}}`), nil)

	if err := recorder.Flush(); err != nil {
		t.Fatal(err)
	}
	summaries, err := ListSummaries(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(summaries) != 1 {
		t.Fatalf("len(summaries) = %d, want 1", len(summaries))
	}
	summary := summaries[0]
	if summary.AgentType != "codex" {
		t.Fatalf("AgentType = %q, want codex", summary.AgentType)
	}
	if summary.SessionID != "session-1" {
		t.Fatalf("SessionID = %q, want session-1", summary.SessionID)
	}
	if summary.User != "user@example.com" {
		t.Fatalf("User = %q", summary.User)
	}
	if summary.EventCount != 3 {
		t.Fatalf("EventCount = %d, want 3", summary.EventCount)
	}
	if !summary.HasBodies {
		t.Fatal("summary did not record body presence")
	}
	if summary.Usage.TotalTokens != 107 || summary.Usage.CachedInputTokens != 20 {
		t.Fatalf("Usage = %+v", summary.Usage)
	}

	events, err := ReadSanitizedSession(dir, "codex", "session-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 3 {
		t.Fatalf("len(events) = %d, want 3", len(events))
	}
	payload := events[1].Payload
	if _, ok := payload["body_base64"]; ok {
		t.Fatal("body_base64 was not redacted")
	}
	if payload["body_base64_redacted"] != true {
		t.Fatalf("body_base64_redacted = %v, want true", payload["body_base64_redacted"])
	}
}
