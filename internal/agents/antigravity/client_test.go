package antigravity

import (
	"encoding/json"
	"testing"
)

func TestGeminiRequestToCloudCodePreservesRequestFields(t *testing.T) {
	body, err := GeminiRequestToCloudCode([]byte(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"temperature":0.2},"futureField":{"keep":true}}`), "project-a", "gemini-3-flash", "session-a", "request-a", "trajectory-a")
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if got["project"] != "project-a" || got["model"] != "gemini-3-flash" || got["requestId"] != "request-a" {
		t.Fatalf("envelope metadata = %#v", got)
	}
	request, ok := got["request"].(map[string]any)
	if !ok || request["sessionId"] != "session-a" {
		t.Fatalf("request = %#v", got["request"])
	}
	if _, ok := request["contents"].([]any); !ok {
		t.Fatalf("contents missing: %#v", request)
	}
	if _, ok := request["futureField"].(map[string]any); !ok {
		t.Fatalf("unknown field was dropped: %#v", request)
	}
	labels := request["labels"].(map[string]any)
	if labels["trajectory_id"] != "trajectory-a" || labels["model_enum"] != "gemini-3-flash" {
		t.Fatalf("labels = %#v", labels)
	}
}

func TestGeminiRequestToCloudCodeRequiresBinding(t *testing.T) {
	if _, err := GeminiRequestToCloudCode([]byte(`{"contents":[]}`), "", "model", "session", "request", ""); err == nil {
		t.Fatal("expected missing project error")
	}
	if _, err := GeminiRequestToCloudCode([]byte(`[]`), "project", "model", "session", "request", ""); err == nil {
		t.Fatal("expected object-shape error")
	}
}

func TestCloudCodeGenerationPath(t *testing.T) {
	if got := CloudCodeGenerationPath(false); got != "/v1internal:generateContent" {
		t.Fatalf("non-stream path = %q", got)
	}
	if got := CloudCodeGenerationPath(true); got != "/v1internal:streamGenerateContent?alt=sse" {
		t.Fatalf("stream path = %q", got)
	}
}
