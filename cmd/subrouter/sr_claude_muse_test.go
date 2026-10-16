package main

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestTranslateMuseClaudeRequest(t *testing.T) {
	body := []byte(`{"model":"muse-wonder-1.0","system":"You are concise.","messages":[{"role":"user","content":"hello"}],"tools":[{"name":"Read","description":"read a file","input_schema":{"type":"object","properties":{"path":{"type":"string"}}}}],"stream":true,"output_config":{"effort":"high"}}`)
	translated, stream, err := translateMuseClaudeRequest(body, "session-1")
	if err != nil {
		t.Fatal(err)
	}
	if !stream {
		t.Fatal("stream flag was not preserved")
	}
	var payload map[string]any
	if err := json.Unmarshal(translated, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["model"] != claudeMuseModel || payload["prompt_cache_key"] != "claude-muse:session-1" {
		t.Fatalf("payload identity = %#v", payload)
	}
	tools, ok := payload["tools"].([]any)
	if !ok || len(tools) != 1 || tools[0].(map[string]any)["name"] != "Read" {
		t.Fatalf("tools = %#v", payload["tools"])
	}
}

func TestClaudeMuseArgsDefaultToHighEffort(t *testing.T) {
	args := claudeMuseArgs(nil, "http://127.0.0.1:1234", "X-Subrouter-Agent: claude-muse")
	if len(args) < 2 || args[0] != "--settings" {
		t.Fatalf("args = %#v", args)
	}
	var settings map[string]any
	if err := json.Unmarshal([]byte(args[1]), &settings); err != nil {
		t.Fatal(err)
	}
	if settings["effortLevel"] != "high" {
		t.Fatalf("effortLevel = %#v, want high", settings["effortLevel"])
	}
}

func TestTranslateMuseResponse(t *testing.T) {
	body := []byte(`{"id":"resp_1","model":"muse-wonder-1.0","output":[{"type":"message","content":[{"type":"output_text","text":"ready"}]},{"type":"function_call","call_id":"call_1","name":"Read","arguments":"{\"path\":\"a.go\"}"}],"usage":{"input_tokens":10,"output_tokens":4,"input_tokens_details":{"cached_tokens":6}}}`)
	translated, err := translateMuseResponse(body)
	if err != nil {
		t.Fatal(err)
	}
	var message map[string]any
	if err := json.Unmarshal(translated, &message); err != nil {
		t.Fatal(err)
	}
	if message["stop_reason"] != "tool_use" {
		t.Fatalf("stop reason = %#v", message["stop_reason"])
	}
	if !strings.Contains(string(translated), `"cache_read_input_tokens":6`) || !strings.Contains(string(translated), `"name":"Read"`) {
		t.Fatalf("translated response = %s", translated)
	}
}

func TestTranslateMuseStream(t *testing.T) {
	var output bytes.Buffer
	response := httptest.NewRecorder()
	stream := strings.NewReader("data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\",\"model\":\"muse-wonder-1.0\"}}\n\ndata: {\"type\":\"response.output_text.delta\",\"output_index\":0,\"delta\":\"ready\"}\n\ndata: {\"type\":\"response.output_item.done\",\"output_index\":0}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"model\":\"muse-wonder-1.0\",\"usage\":{\"input_tokens\":3,\"output_tokens\":1}}}\n\n")
	if err := translateMuseStream(response, stream); err != nil {
		t.Fatal(err)
	}
	output.Write(response.Body.Bytes())
	result := output.String()
	for _, marker := range []string{"message_start", "content_block_delta", "ready", "message_delta", "message_stop"} {
		if !strings.Contains(result, marker) {
			t.Fatalf("stream missing %q: %s", marker, result)
		}
	}
}
