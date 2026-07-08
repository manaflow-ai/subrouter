package proxy

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"math"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// buildEventStreamFrame wraps an Anthropic streaming event JSON in a minimal AWS
// event-stream frame (empty headers, zeroed CRCs, which the parser ignores).
func buildEventStreamFrame(t *testing.T, eventJSON string) []byte {
	t.Helper()
	payload, err := json.Marshal(map[string]string{"bytes": base64.StdEncoding.EncodeToString([]byte(eventJSON))})
	if err != nil {
		t.Fatal(err)
	}
	total := 16 + len(payload)
	frame := make([]byte, total)
	binary.BigEndian.PutUint32(frame[0:4], uint32(total))
	binary.BigEndian.PutUint32(frame[4:8], 0) // headers length
	// bytes 8:12 prelude CRC (ignored), 12:12+payload payload, last 4 msg CRC.
	copy(frame[12:12+len(payload)], payload)
	return frame
}

func TestBedrockStreamUsageWriterExtractsUsage(t *testing.T) {
	start := buildEventStreamFrame(t, `{"type":"message_start","message":{"usage":{"input_tokens":1200,"cache_read_input_tokens":300,"cache_creation_input_tokens":50}}}`)
	delta1 := buildEventStreamFrame(t, `{"type":"message_delta","usage":{"output_tokens":40}}`)
	delta2 := buildEventStreamFrame(t, `{"type":"message_delta","usage":{"output_tokens":128}}`)

	p := newBedrockStreamUsageWriter()
	// Feed bytes split mid-frame to exercise buffering across writes.
	all := append(append(append([]byte{}, start...), delta1...), delta2...)
	p.Write(all[:7])
	p.Write(all[7:40])
	p.Write(all[40:])

	usage, ok := p.Usage()
	if !ok {
		t.Fatal("expected usage extracted")
	}
	if usage.InputTokens != 1200 || usage.CacheReadTokens != 300 || usage.CacheWriteTokens != 50 {
		t.Fatalf("input usage = %+v", usage)
	}
	if usage.OutputTokens != 128 {
		t.Fatalf("output tokens = %d, want 128 (last cumulative delta)", usage.OutputTokens)
	}
}

func TestBedrockUsageCost(t *testing.T) {
	u := bedrockUsage{InputTokens: 1_000_000, OutputTokens: 1_000_000}
	if got := u.costUSD("us.anthropic.claude-fable-5"); math.Abs(got-60.0) > 1e-9 {
		t.Fatalf("fable cost = %v, want 60.00 ($10 in + $50 out per Mtok)", got)
	}
	if got := u.costUSD("us.anthropic.claude-sonnet-5"); math.Abs(got-18.0) > 1e-9 {
		t.Fatalf("sonnet cost = %v, want 18.00", got)
	}
	cache := bedrockUsage{CacheReadTokens: 1_000_000}
	if got := cache.costUSD("fable"); math.Abs(got-1.0) > 1e-9 {
		t.Fatalf("fable cache-read cost = %v, want 1.00 (0.1x input)", got)
	}
}

func TestSummarizeBedrockCost(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bedrock-cost.jsonl")
	appendBedrockCostRecord(path, bedrockCostRecord{Timestamp: nowRFC3339(), Model: "us.anthropic.claude-fable-5", Status: 200, Usage: bedrockUsage{InputTokens: 100, OutputTokens: 200}, CostUSD: 0.011})
	appendBedrockCostRecord(path, bedrockCostRecord{Timestamp: nowRFC3339(), Model: "us.anthropic.claude-sonnet-5", Status: 200, Usage: bedrockUsage{InputTokens: 10, OutputTokens: 20}, CostUSD: 0.0003})

	s := summarizeBedrockCost(path)
	if s.Requests != 2 {
		t.Fatalf("requests = %d, want 2", s.Requests)
	}
	if math.Abs(s.TotalUSD-0.0113) > 1e-9 {
		t.Fatalf("total = %v, want 0.0113", s.TotalUSD)
	}
	if s.InputTokens != 110 || s.OutputTokens != 220 {
		t.Fatalf("tokens in/out = %d/%d, want 110/220", s.InputTokens, s.OutputTokens)
	}
	if _, ok := s.ByModel["claude-fable-5"]; !ok {
		t.Fatalf("expected fable in by-model breakdown: %+v", s.ByModel)
	}

	// Reading a missing file yields an empty summary, not an error.
	empty := summarizeBedrockCost(filepath.Join(dir, "missing.jsonl"))
	if empty.Requests != 0 {
		t.Fatalf("missing file requests = %d, want 0", empty.Requests)
	}
	_ = os.Remove(path)
}

func nowRFC3339() string {
	return "2026-07-03T10:00:00Z"
}

func TestTranscodeBedrockToSSE(t *testing.T) {
	start := buildEventStreamFrame(t, `{"type":"message_start","message":{"usage":{"input_tokens":10}}}`)
	delta := buildEventStreamFrame(t, `{"type":"message_delta","usage":{"output_tokens":7}}`)
	stop := buildEventStreamFrame(t, `{"type":"message_stop"}`)
	stream := append(append(append([]byte{}, start...), delta...), stop...)

	rec := httptest.NewRecorder()
	result := transcodeBedrockToSSE(rec, bytes.NewReader(stream), nil, "", "")
	out := rec.Body.String()
	if !strings.Contains(out, "event: message_start\ndata: {") {
		t.Fatalf("missing message_start SSE frame:\n%s", out)
	}
	if !strings.Contains(out, "event: message_stop\ndata: {") {
		t.Fatalf("missing message_stop SSE frame:\n%s", out)
	}
	if !result.HaveUsage || result.Usage.InputTokens != 10 || result.Usage.OutputTokens != 7 {
		t.Fatalf("usage = %+v ok=%v, want in=10 out=7", result.Usage, result.HaveUsage)
	}
}
