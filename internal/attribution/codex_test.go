package attribution

import "testing"

func TestExtractPromptReadsCodexResponsesInput(t *testing.T) {
	body := []byte(`{"model":"gpt-6","input":[
		{"type":"message","role":"user","content":[{"type":"input_text","text":"<environment_context>\n<cwd>/x</cwd>\n</environment_context>"}]},
		{"type":"message","role":"user","content":[{"type":"input_text","text":"rename the sidebar groups"}]}]}`)
	got, ok := ExtractPrompt(body)
	if !ok || got != "rename the sidebar groups" {
		t.Fatalf("prompt = %q, %v", got, ok)
	}
}

func TestExtractPromptReadsCodexWebSocketCreate(t *testing.T) {
	body := []byte(`{"type":"response.create","model":"gpt-6","previous_response_id":"resp_1","input":[
		{"type":"message","role":"user","content":[{"type":"input_text","text":"now add tests"}]}]}`)
	got, ok := ExtractPrompt(body)
	if !ok || got != "now add tests" {
		t.Fatalf("prompt = %q, %v", got, ok)
	}
}

func TestExtractPromptSkipsCodexToolOutputTurns(t *testing.T) {
	body := []byte(`{"type":"response.create","input":[
		{"type":"message","role":"user","content":[{"type":"input_text","text":"old ask"}]},
		{"type":"function_call","call_id":"c1","name":"shell","arguments":"{}"},
		{"type":"function_call_output","call_id":"c1","output":"ok"}]}`)
	if got, ok := ExtractPrompt(body); ok {
		t.Fatalf("tool output turn recorded: %q", got)
	}
}

func TestExtractPromptSkipsCodexHarnessOnlyMessages(t *testing.T) {
	body := []byte(`{"input":[{"type":"message","role":"user","content":[
		{"type":"input_text","text":"# AGENTS.md instructions for /x\n\n<INSTRUCTIONS>..."}]}]}`)
	if got, ok := ExtractPrompt(body); ok {
		t.Fatalf("harness context recorded as prompt: %q", got)
	}
}

func TestExtractPromptStillReadsClaude(t *testing.T) {
	got, ok := ExtractPrompt([]byte(`{"messages":[{"role":"user","content":"hi"}]}`))
	if !ok || got != "hi" {
		t.Fatalf("prompt = %q, %v", got, ok)
	}
}
