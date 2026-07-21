package proxy

import (
	"bytes"
	"context"
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

	"github.com/gorilla/websocket"
	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/internal/selectacct"
	"github.com/manaflow-ai/subrouter/internal/session"
)

func TestTranslateClaudeRequestUsesGPT56SolMediumAndTools(t *testing.T) {
	body := []byte(`{
		"model":"claude-codex-sol","stream":true,
		"system":[{"type":"text","text":"You are Claude Code."}],
		"messages":[
			{"role":"user","content":[{"type":"text","text":"inspect"}]},
			{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"Read","input":{"path":"a.go"}}]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"package main"}]}
		],
		"tools":[{"name":"Read","description":"read a file","input_schema":{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}}],
		"tool_choice":{"type":"auto"}
	}`)
	translated, stream, err := translateClaudeRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	if !stream {
		t.Fatal("stream = false, want true")
	}
	var payload map[string]any
	if err := json.Unmarshal(translated, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["model"] != claudeCodexModel {
		t.Fatalf("model = %v, want %s", payload["model"], claudeCodexModel)
	}
	reasoning, _ := payload["reasoning"].(map[string]any)
	if reasoning["effort"] != claudeCodexReasoningEffort {
		t.Fatalf("reasoning = %v", reasoning)
	}
	if payload["instructions"] != "You are Claude Code." {
		t.Fatalf("instructions = %v", payload["instructions"])
	}
	input, _ := payload["input"].([]any)
	if len(input) != 3 {
		t.Fatalf("input items = %d, want 3", len(input))
	}
	call, _ := input[1].(map[string]any)
	if call["type"] != "function_call" || call["call_id"] != "toolu_1" || call["name"] != "Read" {
		t.Fatalf("function call = %#v", call)
	}
	result, _ := input[2].(map[string]any)
	if result["type"] != "function_call_output" || result["call_id"] != "toolu_1" {
		t.Fatalf("function result = %#v", result)
	}
	tools, _ := payload["tools"].([]any)
	tool, _ := tools[0].(map[string]any)
	if tool["name"] != "Read" {
		t.Fatalf("tool = %#v", tool)
	}
}

func TestClaudeCodexCompactionDetectionAndMetadata(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"Your task is to create a detailed summary of the conversation so far. Preserve state."}]}]}`)
	if !isClaudeCompactionRequest(body) {
		t.Fatal("expected Claude compaction request to be detected")
	}
	claudeCodexPendingCompactions.Store("compact-session", claudeCodexPendingCompaction{Trigger: "manual", CreatedAt: time.Now()})
	pending, ok := claudeCodexCompactionForRequest("compact-session", body)
	if !ok || pending.Trigger != "manual" {
		t.Fatalf("compaction = %#v, %v", pending, ok)
	}
	metadata := claudeCodexCompactionMetadata("compact-session", pending.Trigger)
	var turn map[string]any
	if err := json.Unmarshal([]byte(metadata["x-codex-turn-metadata"]), &turn); err != nil {
		t.Fatal(err)
	}
	if turn["request_kind"] != "compaction" {
		t.Fatalf("turn metadata = %#v", turn)
	}
	compaction, _ := turn["compaction"].(map[string]any)
	if compaction["trigger"] != "manual" || compaction["implementation"] != "responses" {
		t.Fatalf("compaction metadata = %#v", compaction)
	}
}

func TestTranslateClaudeRequestRoutesModelAndEffort(t *testing.T) {
	tests := []struct {
		model      string
		effort     string
		wantModel  string
		wantEffort string
	}{
		{model: "claude-codex-sol", effort: "low", wantModel: "gpt-5.6-sol", wantEffort: "low"},
		{model: "claude-codex-terra", effort: "high", wantModel: "gpt-5.6-terra", wantEffort: "high"},
		{model: "claude-codex-luna", effort: "max", wantModel: "gpt-5.6-luna", wantEffort: "max"},
	}
	for _, test := range tests {
		t.Run(test.model+"/"+test.effort, func(t *testing.T) {
			body := []byte(fmt.Sprintf(`{"model":%q,"output_config":{"effort":%q},"messages":[{"role":"user","content":"hello"}]}`, test.model, test.effort))
			translated, _, err := translateClaudeRequest(body)
			if err != nil {
				t.Fatal(err)
			}
			var payload struct {
				Model     string `json:"model"`
				Reasoning struct {
					Effort string `json:"effort"`
				} `json:"reasoning"`
			}
			if err := json.Unmarshal(translated, &payload); err != nil {
				t.Fatal(err)
			}
			if payload.Model != test.wantModel || payload.Reasoning.Effort != test.wantEffort {
				t.Fatalf("route = %s/%s, want %s/%s", payload.Model, payload.Reasoning.Effort, test.wantModel, test.wantEffort)
			}
		})
	}
}

func TestTranslateClaudeRequestUsesOutputTextForAssistantHistory(t *testing.T) {
	body := []byte(`{
		"stream":true,
		"messages":[
			{"role":"user","content":"first"},
			{"role":"assistant","content":"answer"},
			{"role":"user","content":"second"}
		]
	}`)
	translated, _, err := translateClaudeRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	var payload struct {
		Input []struct {
			Role    string `json:"role"`
			Content []struct {
				Type string `json:"type"`
			} `json:"content"`
		} `json:"input"`
	}
	if err := json.Unmarshal(translated, &payload); err != nil {
		t.Fatal(err)
	}
	if got := payload.Input[0].Content[0].Type; got != "input_text" {
		t.Fatalf("user content type = %q, want input_text", got)
	}
	if got := payload.Input[1].Content[0].Type; got != "output_text" {
		t.Fatalf("assistant content type = %q, want output_text", got)
	}
	if got := payload.Input[2].Content[0].Type; got != "input_text" {
		t.Fatalf("user content type = %q, want input_text", got)
	}
}

func TestTranslateCodexResponseReturnsAnthropicToolUse(t *testing.T) {
	response := `{
		"id":"resp_1","model":"gpt-5.6-terra",
		"output":[
			{"type":"message","content":[{"type":"output_text","text":"Checking."}]},
			{"type":"function_call","id":"fc_1","call_id":"call_1","name":"Read","arguments":"{\"path\":\"a.go\"}"}
		],
		"usage":{"input_tokens":42,"output_tokens":7,"input_tokens_details":{"cached_tokens":30}}
	}`
	translated, err := translateCodexResponse(strings.NewReader(response))
	if err != nil {
		t.Fatal(err)
	}
	var message map[string]any
	if err := json.Unmarshal(translated, &message); err != nil {
		t.Fatal(err)
	}
	if message["model"] != "claude-codex-terra" || message["stop_reason"] != "tool_use" {
		t.Fatalf("message identity = %#v", message)
	}
	content, _ := message["content"].([]any)
	if len(content) != 2 {
		t.Fatalf("content blocks = %d, want 2", len(content))
	}
	tool, _ := content[1].(map[string]any)
	if tool["type"] != "tool_use" || tool["id"] != "call_1" || tool["name"] != "Read" {
		t.Fatalf("tool block = %#v", tool)
	}
	usage, _ := message["usage"].(map[string]any)
	if usage["input_tokens"] != float64(12) || usage["cache_read_input_tokens"] != float64(30) {
		t.Fatalf("usage = %#v", usage)
	}
}

func TestTranslateCodexStreamReturnsAnthropicSSE(t *testing.T) {
	upstream := strings.Join([]string{
		`data: {"type":"response.created","response":{"id":"resp_stream","model":"gpt-5.6-luna"}}`,
		``,
		`data: {"type":"response.output_item.added","output_index":0,"item":{"type":"message","id":"msg_1"}}`,
		``,
		`data: {"type":"response.output_text.delta","output_index":0,"delta":"hello"}`,
		``,
		`data: {"type":"response.output_item.done","output_index":0,"item":{"type":"message"}}`,
		``,
		`data: {"type":"response.completed","response":{"usage":{"input_tokens":12,"output_tokens":3,"input_tokens_details":{"cached_tokens":8}}}}`,
		``,
	}, "\n")
	recorder := httptest.NewRecorder()
	if err := translateCodexStream(recorder, strings.NewReader(upstream)); err != nil {
		t.Fatal(err)
	}
	got := recorder.Body.String()
	for _, want := range []string{
		`event: message_start`,
		`"model":"claude-codex-luna"`,
		`event: content_block_start`,
		`"text":"hello","type":"text_delta"`,
		`event: content_block_stop`,
		`"stop_reason":"end_turn"`,
		`"cache_read_input_tokens":8`,
		`"input_tokens":4`,
		`event: message_stop`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in stream:\n%s", want, got)
		}
	}
}

func TestClaudeCodexBridgeRoutesThroughCodexOAuthAccount(t *testing.T) {
	var upstreamPath, upstreamAuth, upstreamAccount string
	var upstreamPayload map[string]any
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamPath = r.URL.Path
		upstreamAuth = r.Header.Get("Authorization")
		upstreamAccount = r.Header.Get("ChatGPT-Account-ID")
		connection, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer connection.Close()
		if err := connection.ReadJSON(&upstreamPayload); err != nil {
			t.Fatal(err)
		}
		_ = connection.WriteJSON(map[string]any{"type": "response.created", "response": map[string]any{"id": "resp_bridge", "model": claudeCodexModel}})
		_ = connection.WriteJSON(map[string]any{"type": "response.output_item.done", "output_index": 0, "item": map[string]any{"type": "message", "content": []any{map[string]any{"type": "output_text", "text": "BRIDGE_OK"}}}})
		_ = connection.WriteJSON(map[string]any{"type": "response.completed", "response": map[string]any{"id": "resp_bridge", "model": claudeCodexModel, "usage": map[string]any{"input_tokens": 9, "output_tokens": 2}}})
	}))
	defer upstream.Close()
	upstreamURL, err := url.Parse(upstream.URL + "/backend-api/codex")
	if err != nil {
		t.Fatal(err)
	}
	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	account := accounts.Account{ID: "pro@example.com", Provider: accounts.ProviderCodex, AuthMode: accounts.AuthModeOAuth, Token: "chatgpt-token", AccountID: "acct_pro"}
	handler := Server{
		CodexUpstream: upstreamURL,
		Accounts:      []accounts.Account{account},
		Sessions:      store,
		SchedulerRef: selectacct.NewSchedulerRef(selectacct.NewScheduler([]selectacct.Score{
			{AccountID: account.ID, Provider: accounts.ProviderCodex, Headroom: 1, ShortHeadroom: 1},
		})),
		ScoreAccounts: func(context.Context, []accounts.Account) ([]selectacct.Score, int) {
			return []selectacct.Score{{AccountID: account.ID, Provider: accounts.ProviderCodex, Headroom: 1, ShortHeadroom: 1}}, 1
		},
		UsageScoreTTL: 0,
		MaxBodyBytes:  1 << 20,
	}.Handler()
	server := httptest.NewServer(handler)
	defer server.Close()

	requestBody := `{"model":"claude-codex-sol","stream":false,"system":"test","messages":[{"role":"user","content":"reply"}],"tools":[]}`
	req, err := http.NewRequest(http.MethodPost, server.URL+"/claude-codex/v1/messages", bytes.NewBufferString(requestBody))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer ignored-claude-token")
	req.Header.Set("X-Subrouter-Agent", "claude")
	req.Header.Set("X-Subrouter-Session", "claude-session-1")
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.StatusCode, body)
	}
	if upstreamPath != "/backend-api/codex/responses" {
		t.Fatalf("upstream path = %q", upstreamPath)
	}
	if upstreamAuth != "Bearer chatgpt-token" || upstreamAccount != "acct_pro" {
		t.Fatalf("upstream auth/account = %q/%q", upstreamAuth, upstreamAccount)
	}
	if upstreamPayload["model"] != claudeCodexModel {
		t.Fatalf("upstream model = %v", upstreamPayload["model"])
	}
	if upstreamPayload["prompt_cache_key"] != "claude-codex:claude-session-1" {
		t.Fatalf("prompt cache key = %v", upstreamPayload["prompt_cache_key"])
	}
	reasoning, _ := upstreamPayload["reasoning"].(map[string]any)
	if reasoning["effort"] != claudeCodexReasoningEffort {
		t.Fatalf("upstream reasoning = %#v", reasoning)
	}
	if response.Header.Get("X-Subrouter-Bridge") != "claude-codex" || response.Header.Get("X-Subrouter-Upstream-Provider") != "codex-chatgpt" {
		t.Fatalf("bridge headers = %v", response.Header)
	}
	var message map[string]any
	if err := json.Unmarshal(body, &message); err != nil {
		t.Fatal(err)
	}
	if message["model"] != "claude-codex-sol" {
		t.Fatalf("Claude-facing model = %v", message["model"])
	}
}
