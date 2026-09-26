package transcript

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestAnalyzeAggregatesUsageByUserAccountModelAndTimeline(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "transcripts")
	recorder := NewRecorder(dir)
	recorder.RecordMeta("codex", "session-1:0", map[string]any{
		"user":    "user@example.com",
		"account": "dave@example.com",
	})
	recorder.RecordPayload("codex", "session-1:0", "websocket_message", "upstream_to_client", []byte(`{"type":"response.completed","response":{"model":"gpt-5.5","usage":{"input_tokens":100,"input_tokens_details":{"cached_tokens":30},"output_tokens":8,"output_tokens_details":{"reasoning_tokens":5},"total_tokens":108}}}`), nil)
	recorder.RecordPayload("codex", "session-1:0", "websocket_message", "upstream_to_client", []byte("data: {\"response\":{\"model\":\"gpt-5.5\",\"usage\":{\"input_tokens\":50,\"output_tokens\":2,\"total_tokens\":52}}}\n\ndata: [DONE]\n"), nil)

	if err := recorder.Flush(); err != nil {
		t.Fatal(err)
	}
	analytics, err := Analyze(dir)
	if err != nil {
		t.Fatal(err)
	}
	if analytics.Totals.Requests != 2 {
		t.Fatalf("Requests = %d, want 2", analytics.Totals.Requests)
	}
	if analytics.Totals.TotalTokens != 160 {
		t.Fatalf("TotalTokens = %d, want 160", analytics.Totals.TotalTokens)
	}
	if analytics.Totals.CachedInputTokens != 30 {
		t.Fatalf("CachedInputTokens = %d, want 30", analytics.Totals.CachedInputTokens)
	}
	if len(analytics.ByUser) != 1 || analytics.ByUser[0].Key != "user@example.com" {
		t.Fatalf("ByUser = %+v", analytics.ByUser)
	}
	if len(analytics.ByAccount) != 1 || analytics.ByAccount[0].Key != "dave@example.com" {
		t.Fatalf("ByAccount = %+v", analytics.ByAccount)
	}
	if len(analytics.ByModel) != 1 || analytics.ByModel[0].Key != "gpt-5.5" {
		t.Fatalf("ByModel = %+v", analytics.ByModel)
	}
	if len(analytics.Timeline) == 0 {
		t.Fatal("expected timeline buckets")
	}
}

func TestReadRawSessionDecodesTextBodies(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "transcripts")
	recorder := NewRecorder(dir)
	recorder.RecordPayload("codex", "session-1:0", "http_body", "upstream_to_client", []byte("secret body"), nil)

	if err := recorder.Flush(); err != nil {
		t.Fatal(err)
	}
	events, err := ReadRawSession(dir, "codex", "session-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("len(events) = %d, want 1", len(events))
	}
	if events[0].BodyText != "secret body" {
		t.Fatalf("BodyText = %q", events[0].BodyText)
	}
	if _, ok := events[0].Payload["body_base64"]; ok {
		t.Fatal("body_base64 should move out of payload")
	}
	if strings.TrimSpace(events[0].BodyBase64) != "" {
		t.Fatalf("BodyBase64 = %q", events[0].BodyBase64)
	}
}

func TestChunkedPayloadsReassembleForRawSessionAndAnalytics(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "transcripts")
	recorder := NewRecorder(dir)
	body := []byte("data: {\"response\":{\"model\":\"gpt-5.5\",\"usage\":{\"input_tokens\":10,\"output_tokens\":2,\"total_tokens\":12}}}\n\ndata: [DONE]\n")

	recorder.RecordMeta("codex", "session-1:0", map[string]any{
		"user":    "user@example.com",
		"account": "account@example.com",
	})
	recorder.RecordPayloadChunk("codex", "session-1:0", "http_body", "upstream_to_client", "stream-1", 0, 0, body[:40], map[string]any{"status": 200})
	recorder.RecordPayloadChunk("codex", "session-1:0", "http_body", "upstream_to_client", "stream-1", 1, 40, body[40:], map[string]any{"status": 200})
	recorder.RecordPayloadSummary("codex", "session-1:0", "http_body", "upstream_to_client", "stream-1", int64(len(body)), "sha", 2, map[string]any{"status": 200})

	if err := recorder.Flush(); err != nil {
		t.Fatal(err)
	}
	events, err := ReadRawSession(dir, "codex", "session-1")
	if err != nil {
		t.Fatal(err)
	}
	if got := events[len(events)-1].BodyText; got != string(body) {
		t.Fatalf("BodyText = %q, want %q", got, string(body))
	}

	summaries, err := ListSummaries(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(summaries) != 1 || summaries[0].Usage.TotalTokens != 12 || !summaries[0].HasBodies {
		t.Fatalf("Summaries = %+v, want chunked 12-token summary", summaries)
	}

	if err := recorder.Flush(); err != nil {
		t.Fatal(err)
	}
	analytics, err := Analyze(dir)
	if err != nil {
		t.Fatal(err)
	}
	if analytics.Totals.TotalTokens != 12 || analytics.Totals.Requests != 1 {
		t.Fatalf("Totals = %+v, want one 12-token request", analytics.Totals)
	}
}
