package proxy

import (
	"encoding/json"
	"testing"
)

// Bedrock refuses each of these with a 400 for the whole request, so the
// fallback chain answered nothing while the subscription pool was exhausted.
func TestStripBedrockUnsupportedFields(t *testing.T) {
	cases := []struct {
		name, body string
		absent     []string
		thinking   string
	}{
		{"disabled thinking is dropped", `{"model":"m","thinking":{"type":"disabled"},"messages":[]}`, []string{"thinking"}, ""},
		{"enabled thinking is kept", `{"model":"m","thinking":{"type":"enabled","budget_tokens":1024},"messages":[]}`, nil, `{"type":"enabled","budget_tokens":1024}`},
		{"unsupported display is dropped", `{"model":"m","thinking":{"type":"adaptive","display":"full"},"messages":[]}`, nil, `{"type":"adaptive"}`},
		{"output_config and safeguards are dropped", `{"model":"m","output_config":{"effort":"high"},"safeguards":{},"messages":[{"role":"user","content":"x","output_config":{}}]}`, []string{"output_config", "safeguards"}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got map[string]json.RawMessage
			if err := json.Unmarshal(stripBedrockUnsupportedFields([]byte(tc.body)), &got); err != nil {
				t.Fatal(err)
			}
			for _, field := range tc.absent {
				if _, ok := got[field]; ok {
					t.Fatalf("%s survived: %s", field, got[field])
				}
			}
			if tc.thinking != "" && string(got["thinking"]) != tc.thinking {
				t.Fatalf("thinking = %s, want %s", got["thinking"], tc.thinking)
			}
			var messages []map[string]json.RawMessage
			_ = json.Unmarshal(got["messages"], &messages)
			for _, message := range messages {
				if _, ok := message["output_config"]; ok {
					t.Fatal("messages[].output_config survived")
				}
			}
		})
	}
}
