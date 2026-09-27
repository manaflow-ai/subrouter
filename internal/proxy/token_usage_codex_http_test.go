package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/selectacct"
	"github.com/manaflow-ai/subrouter/session"
)

// orderedJSON marshals key/value pairs in the given order. A Go map always
// marshals with sorted keys, and key order is what these tests are about.
type orderedJSON []orderedField

type orderedField struct {
	key   string
	value any
}

func (o orderedJSON) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, field := range o {
		if i > 0 {
			b.WriteByte(',')
		}
		key, _ := json.Marshal(field.key)
		value, err := json.Marshal(field.value)
		if err != nil {
			return nil, err
		}
		b.Write(key)
		b.WriteByte(':')
		b.Write(value)
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

func (o orderedJSON) with(key string, value any) orderedJSON {
	return append(o, orderedField{key, value})
}

// realisticCodexResponse is the Responses object chatgpt.com echoes in
// response.created, response.in_progress, and response.completed: output
// items (encrypted reasoning, messages, tool calls), instructions, and tools,
// so on a long agentic turn the completed event is a single SSE line of
// hundreds of KiB. usageFirst puts usage ahead of the large fields; the
// scanner must not depend on where usage sits in the object.
func realisticCodexResponse(status string, usage any, output []any, usageFirst bool) orderedJSON {
	tools := make([]any, 0, 40)
	for i := 0; i < 40; i++ {
		tools = append(tools, map[string]any{
			"type": "function", "name": fmt.Sprintf("tool_%d", i), "strict": false,
			"description": strings.Repeat(fmt.Sprintf("Describes tool %d with \"quoted\" usage notes. ", i), 30),
			"parameters":  map[string]any{"type": "object", "properties": map[string]any{"cmd": map[string]any{"type": "string"}}},
		})
	}
	response := orderedJSON{}.with("id", "resp_0a1b2c").with("object", "response").
		with("created_at", 1790000000).with("status", status)
	if usageFirst {
		response = response.with("usage", usage)
	}
	response = response.with("background", false).with("error", nil).with("incomplete_details", nil).
		with("model", "gpt-6-astra").
		with("parallel_tool_calls", true).with("previous_response_id", nil).
		with("prompt_cache_key", "01a09d97-d4a4-7720-832f-7643b3ad6f3f").
		with("reasoning", map[string]any{"effort": "high", "summary": "auto"}).with("service_tier", "auto").
		with("store", false).with("temperature", 1.0).
		with("text", map[string]any{"format": map[string]any{"type": "text"}, "verbosity": "low"}).
		with("tool_choice", "auto").with("top_p", 0.98).with("truncation", "disabled").
		with("instructions", strings.Repeat("You are Codex, a coding agent. Follow the user's \"usage\": rules. ", 600)).
		with("tools", tools).with("output", output)
	if !usageFirst {
		response = response.with("usage", usage)
	}
	return response.with("user", nil).with("metadata", map[string]any{})
}

func codexSSEEvent(t *testing.T, eventType string, sequence int, response any, fields orderedJSON) string {
	t.Helper()
	payload := orderedJSON{}.with("type", eventType).with("sequence_number", sequence)
	if response != nil {
		payload = payload.with("response", response)
	}
	payload = append(payload, fields...)
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return "event: " + eventType + "\ndata: " + string(encoded) + "\n\n"
}

// realisticCodexStream is a long agentic codex-tui turn over HTTP: created
// and in_progress with usage null, an encrypted reasoning item, text deltas,
// a large apply_patch call, then response.completed carrying usage.
func realisticCodexStream(t *testing.T, usageFirst bool) string {
	t.Helper()
	var b strings.Builder
	seq := 0
	emit := func(eventType string, response any, fields orderedJSON) {
		b.WriteString(codexSSEEvent(t, eventType, seq, response, fields))
		seq++
	}
	emit("response.created", realisticCodexResponse("in_progress", nil, []any{}, usageFirst), nil)
	emit("response.in_progress", realisticCodexResponse("in_progress", nil, []any{}, usageFirst), nil)
	reasoning := map[string]any{"id": "rs_1", "type": "reasoning", "summary": []any{}, "encrypted_content": strings.Repeat("gAAAAB", 20000)}
	emit("response.output_item.added", nil, orderedJSON{}.with("output_index", 0).with("item", map[string]any{"id": "rs_1", "type": "reasoning", "summary": []any{}}))
	emit("response.output_item.done", nil, orderedJSON{}.with("output_index", 0).with("item", reasoning))
	emit("response.output_item.added", nil, orderedJSON{}.with("output_index", 1).with("item", map[string]any{"id": "msg_1", "type": "message", "status": "in_progress", "role": "assistant", "content": []any{}}))
	var text strings.Builder
	for i := 0; i < 500; i++ {
		delta := fmt.Sprintf("word%d ", i)
		text.WriteString(delta)
		emit("response.output_text.delta", nil, orderedJSON{}.with("item_id", "msg_1").with("output_index", 1).
			with("content_index", 0).with("delta", delta).with("logprobs", []any{}))
	}
	message := map[string]any{"id": "msg_1", "type": "message", "status": "completed", "role": "assistant",
		"content": []any{map[string]any{"type": "output_text", "text": text.String(), "annotations": []any{}, "logprobs": []any{}}}}
	emit("response.output_item.done", nil, orderedJSON{}.with("output_index", 1).with("item", message))
	patch := map[string]any{"id": "fc_1", "type": "custom_tool_call", "status": "completed", "call_id": "call_1", "name": "apply_patch",
		"input": "*** Begin Patch\n*** Update File: internal/proxy/proxy.go\n" + strings.Repeat("+\t// \"usage\": a long patch line\n", 3000) + "*** End Patch\n"}
	emit("response.output_item.done", nil, orderedJSON{}.with("output_index", 2).with("item", patch))
	emit("response.completed", realisticCodexResponse("completed", map[string]any{
		"input_tokens": 91234, "input_tokens_details": map[string]any{"cached_tokens": 88000},
		"output_tokens": 812, "output_tokens_details": map[string]any{"reasoning_tokens": 300}, "total_tokens": 92046,
	}, []any{reasoning, message, patch}, usageFirst), nil)
	return b.String()
}

// TestProxyRecordsTokenUsageForRealisticCodexHTTPTurn drives a long codex-tui
// HTTP Responses turn through the Codex route the way the team server serves
// it: zstd request body, SSE response flushed per event, and a
// response.completed event far larger than any fixed head or tail window.
func TestProxyRecordsTokenUsageForRealisticCodexHTTPTurn(t *testing.T) {
	for _, usageFirst := range []bool{false, true} {
		t.Run(fmt.Sprintf("usage_first=%v", usageFirst), func(t *testing.T) {
			testRealisticCodexHTTPTurn(t, usageFirst)
		})
	}
}

func testRealisticCodexHTTPTurn(t *testing.T, usageFirst bool) {
	stream := realisticCodexStream(t, usageFirst)
	if completed := stream[strings.LastIndex(stream, "event: response.completed"):]; len(completed) < 256<<10 {
		t.Fatalf("completed event is %d bytes, want a long turn's", len(completed))
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		for _, event := range strings.SplitAfter(stream, "\n\n") {
			_, _ = io.WriteString(w, event)
			flusher.Flush()
		}
	}))
	defer upstream.Close()
	upstreamURL, _ := url.Parse(upstream.URL)
	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	recorder := NewTokenUsageRecorder("", nil)
	subrouter := httptest.NewServer(Server{
		Upstream: upstreamURL,
		Accounts: []accounts.Account{{
			ID: "codex-a", AuthMode: accounts.AuthModeOAuth, Token: "token-a", AccountID: "acct-a",
		}},
		Sessions:     store,
		Scheduler:    selectacct.NewScheduler(nil),
		MaxBodyBytes: 1 << 20,
		TokenUsage:   recorder,
	}.Handler())
	defer subrouter.Close()

	// codex-tui compresses its request body with zstd.
	requestBody := `{"model":"gpt-6-astra","instructions":"x","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}],"stream":true,"store":false,"prompt_cache_key":"01a09d97-d4a4-7720-832f-7643b3ad6f3f"}`
	encoder, _ := zstd.NewWriter(nil)
	compressed := encoder.EncodeAll([]byte(requestBody), nil)
	request, _ := http.NewRequest(http.MethodPost, subrouter.URL+"/v1/responses", bytes.NewReader(compressed))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Content-Encoding", "zstd")
	request.Header.Set("Accept", "text/event-stream")
	request.Header.Set("Session_id", "01a09d97-d4a4-7720-832f-7643b3ad6f3f")
	request.Header.Set("User-Agent", "codex-tui/0.146.0 (Mac OS 26.4.1; arm64) ghostty/1.3.2 (codex-tui; 0.146.0)")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusOK || string(got) != stream {
		t.Fatalf("status %d, body changed (%d bytes, want %d)", response.StatusCode, len(got), len(stream))
	}

	var rows []TokenUsageRow
	deadline := time.Now().Add(5 * time.Second)
	for {
		rows, err = recorder.Rows(time.Now().Add(-time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) > 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %+v, want one", rows)
	}
	row := rows[0]
	if row.Model != "gpt-6-astra" || row.Requests != 1 || row.RequestsWithoutUsage != 0 ||
		row.InputTokens != 91234 || row.CachedInputTokens != 88000 || row.OutputTokens != 812 || row.ReasoningOutputTokens != 300 {
		t.Fatalf("row = %+v", row)
	}
}

// A long line's buffer must not stay pinned for the rest of the stream.
func TestTokenUsageScannerReleasesLongLineBuffer(t *testing.T) {
	scanner := newTokenUsageScanner("text/event-stream")
	scanner.Write([]byte(realisticCodexStream(t, true)))
	if got := cap(scanner.head); got > tokenUsageLineKeepBytes {
		t.Fatalf("scanner keeps a %d byte buffer after the stream, want at most %d", got, tokenUsageLineKeepBytes)
	}
	usage, model, ok := scanner.Finish()
	if !ok || model != "gpt-6-astra" || usage.InputTokens != 91234 || usage.OutputTokens != 812 {
		t.Fatalf("usage = %+v model %q ok %v", usage, model, ok)
	}
}
