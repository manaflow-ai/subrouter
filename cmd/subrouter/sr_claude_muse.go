package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/manaflow-ai/subrouter/internal/agents/claude"
)

const (
	claudeMuseModel       = "muse-wonder-1.0"
	claudeMuseAPIBaseURL  = "https://api.meta.ai/v1"
	claudeMuseKeychainSvc = "ai.meta.dev.credentials"
	claudeMuseMaxRetries  = 15
)

// claudeMuse launches the installed Claude Code client against a local bridge
// that uses the already-authenticated Muse Code subscription. The Muse API
// credential is read only in-process from Keychain and is never placed in the
// child's environment, argv, settings, or logs.
func (r srRunner) claudeMuse(ctx context.Context, args []string) error {
	claudePath, ok := claude.DetectCLI()
	if !ok {
		return fmt.Errorf("Claude CLI not found. Install from https://claude.ai/download")
	}
	apiKey, err := museKeychainAPIKey(ctx)
	if err != nil {
		return err
	}
	bridge, err := newClaudeMuseBridge(apiKey)
	if err != nil {
		return err
	}
	defer bridge.Close()

	customHeaders := claudeMuseCustomHeaders()
	launchArgs := claudeMuseArgs(args, bridge.BaseURL(), customHeaders)
	cmd := exec.CommandContext(ctx, claudePath, launchArgs...)
	cmd.Stdin = r.in
	cmd.Stdout = r.out
	cmd.Stderr = r.errOut
	env := envWithoutSubrouterControl(os.Environ())
	env = envWithout(env, claudeRoutingEnvKeys)
	env = envWithout(env, []string{"META_API_KEY"})
	env = envWithoutPrefix(env, "ANTHROPIC_DEFAULT_")
	env = envWithoutPrefix(env, "ANTHROPIC_CUSTOM_MODEL_")
	env = append(env,
		"ANTHROPIC_BASE_URL="+bridge.BaseURL(),
		"ANTHROPIC_AUTH_TOKEN=muse-local",
		"ANTHROPIC_CUSTOM_HEADERS="+customHeaders,
		"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1",
	)
	cmd.Env = env
	fmt.Fprintf(r.errOut, "Claude Code -> Muse Wonder subscription via local bridge %s (use /model and /effort)\n", bridge.BaseURL())
	return cmd.Run()
}

func museKeychainAPIKey(ctx context.Context) (string, error) {
	if value := strings.TrimSpace(os.Getenv("META_API_KEY")); value != "" {
		return value, nil
	}
	cmd := exec.CommandContext(ctx, "security", "find-generic-password", "-s", claudeMuseKeychainSvc, "-a", "meta", "-w")
	output, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("read Muse subscription from macOS Keychain: %w; run `muse login`", err)
	}
	var stored struct {
		APIKey      string `json:"api_key"`
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(output), &stored); err != nil {
		return "", fmt.Errorf("decode Muse Keychain credentials: %w; run `muse login`", err)
	}
	if value := strings.TrimSpace(stored.APIKey); value != "" {
		return value, nil
	}
	if value := strings.TrimSpace(stored.AccessToken); value != "" {
		return value, nil
	}
	return "", errors.New("Muse Keychain credentials contain no API key; run `muse login`")
}

func claudeMuseArgs(args []string, baseURL, customHeaders string) []string {
	for _, arg := range args {
		if arg == "--settings" || strings.HasPrefix(arg, "--settings=") {
			return args
		}
	}
	hook := "/usr/bin/curl -fsS --max-time 10 -X POST -H 'Content-Type: application/json' --data-binary @- " + shellQuote(baseURL+"/hooks/pre-compact") + " >/dev/null"
	settings, err := json.Marshal(map[string]any{
		"disableAutoMode": "disable",
		"permissions": map[string]any{
			"disableAutoMode": "disable",
			"defaultMode":     "default",
		},
		"env": map[string]string{
			"ANTHROPIC_BASE_URL":                   baseURL,
			"ANTHROPIC_AUTH_TOKEN":                 "muse-local",
			"ANTHROPIC_CUSTOM_HEADERS":             customHeaders,
			"CLAUDE_CODE_MAX_RETRIES":              "15",
			"CLAUDE_CODE_MAX_CONCURRENT_SUBAGENTS": "200",
		},
		"hooks": claudeMuseHooks(hook),
		"modelPicker": map[string]any{
			"replaceBuiltInOptions": true,
			"options": []map[string]any{{
				"model": "muse-wonder-1.0", "label": "Muse Wonder", "description": "Muse Wonder through your Muse subscription.", "behavesAs": "claude-opus-4-6",
			}},
		},
	})
	if err != nil {
		return args
	}
	return append([]string{"--settings", string(settings), "--model", claudeMuseModel}, args...)
}

func claudeMuseCustomHeaders() string {
	headers := "X-Subrouter-Agent: claude-muse"
	if sessionKey := claudeProxySessionKey(); sessionKey != "" {
		headers += "\nX-Subrouter-Session: " + sessionKey
	}
	return headers
}

func claudeMuseHooks(compactionHook string) map[string]any {
	hooks := map[string]any{
		"PreCompact": []any{map[string]any{
			"matcher": "",
			"hooks":   []any{map[string]any{"type": "command", "command": compactionHook, "timeout": 10}},
		}},
	}
	if claudeCodexInLinkedSubmoduleWorktree() {
		hooks["WorktreeCreate"] = []any{map[string]any{"hooks": []any{map[string]any{
			"type": "command", "command": "printf '%s\\n' \"$PWD\"", "timeout": 5,
		}}}}
	}
	return hooks
}

type claudeMuseBridge struct {
	server *http.Server
	ln     net.Listener
	client *http.Client
	apiKey string
	mu     sync.Mutex
}

func newClaudeMuseBridge(apiKey string) (*claudeMuseBridge, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("start local Muse bridge: %w", err)
	}
	b := &claudeMuseBridge{ln: ln, apiKey: apiKey, client: &http.Client{Timeout: 0}}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/messages", b.messages)
	mux.HandleFunc("/messages", b.messages)
	mux.HandleFunc("/v1/messages/count_tokens", b.countTokens)
	mux.HandleFunc("/messages/count_tokens", b.countTokens)
	mux.HandleFunc("/hooks/pre-compact", b.preCompact)
	b.server = &http.Server{Handler: mux, ReadHeaderTimeout: 15 * time.Second}
	go func() { _ = b.server.Serve(ln) }()
	return b, nil
}

func (b *claudeMuseBridge) BaseURL() string { return "http://" + b.ln.Addr().String() }

func (b *claudeMuseBridge) Close() {
	if b.server != nil {
		_ = b.server.Shutdown(context.Background())
	}
}

func (b *claudeMuseBridge) preCompact(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, `{}`)
}

func (b *claudeMuseBridge) countTokens(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	var request map[string]any
	_ = json.Unmarshal(body, &request)
	encoded, _ := json.Marshal(request)
	w.Header().Set("Content-Type", "application/json")
	_, _ = fmt.Fprintf(w, `{"input_tokens":%d}`, maxInt(1, len(encoded)/4))
}

func (b *claudeMuseBridge) messages(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 64<<20))
	if err != nil {
		museClaudeError(w, http.StatusBadRequest, err.Error())
		return
	}
	translated, stream, err := translateMuseClaudeRequest(body, r.Header.Get("X-Subrouter-Session"))
	if err != nil {
		museClaudeError(w, http.StatusBadRequest, err.Error())
		return
	}
	upstream, err := b.doRequest(r.Context(), translated)
	if err != nil {
		museClaudeError(w, http.StatusBadGateway, err.Error())
		return
	}
	defer upstream.Body.Close()
	if stream {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("X-Subrouter-Bridge", "claude-muse")
		w.WriteHeader(http.StatusOK)
		_ = translateMuseStream(w, upstream.Body)
		return
	}
	response, err := io.ReadAll(io.LimitReader(upstream.Body, 64<<20))
	if err != nil {
		museClaudeError(w, http.StatusBadGateway, err.Error())
		return
	}
	translatedResponse, err := translateMuseResponse(response)
	if err != nil {
		museClaudeError(w, http.StatusBadGateway, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Subrouter-Bridge", "claude-muse")
	_, _ = w.Write(translatedResponse)
}

func (b *claudeMuseBridge) doRequest(ctx context.Context, body []byte) (*http.Response, error) {
	var last error
	for attempt := 1; attempt <= claudeMuseMaxRetries; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, claudeMuseAPIBaseURL+"/responses", bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+b.apiKey)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Client", "muse-code")
		response, err := b.client.Do(req)
		if err == nil && response.StatusCode < 500 && response.StatusCode != http.StatusTooManyRequests {
			return response, nil
		}
		if response != nil {
			last = fmt.Errorf("Muse API HTTP %s", response.Status)
			_, _ = io.Copy(io.Discard, response.Body)
			_ = response.Body.Close()
		}
		if err != nil {
			last = err
		}
		if attempt == claudeMuseMaxRetries {
			break
		}
		delay := time.Duration(attempt) * 250 * time.Millisecond
		if delay > 3*time.Second {
			delay = 3 * time.Second
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
	if last == nil {
		last = errors.New("Muse API request failed")
	}
	return nil, last
}

type museClaudeRequest struct {
	Model        string            `json:"model"`
	System       json.RawMessage   `json:"system"`
	Messages     []json.RawMessage `json:"messages"`
	Tools        []json.RawMessage `json:"tools"`
	ToolChoice   json.RawMessage   `json:"tool_choice"`
	Stream       bool              `json:"stream"`
	MaxTokens    int               `json:"max_tokens"`
	OutputConfig struct {
		Effort string `json:"effort"`
	} `json:"output_config"`
}

func translateMuseClaudeRequest(body []byte, session string) ([]byte, bool, error) {
	var request museClaudeRequest
	if err := json.Unmarshal(body, &request); err != nil {
		return nil, false, fmt.Errorf("decode Claude request: %w", err)
	}
	instructions := museAnthropicText(request.System)
	input := make([]any, 0, len(request.Messages))
	for _, message := range request.Messages {
		items, err := translateMuseMessage(message)
		if err != nil {
			return nil, false, err
		}
		input = append(input, items...)
	}
	tools := make([]any, 0, len(request.Tools))
	for _, raw := range request.Tools {
		var tool struct {
			Name        string          `json:"name"`
			Description string          `json:"description"`
			InputSchema json.RawMessage `json:"input_schema"`
		}
		if json.Unmarshal(raw, &tool) != nil || tool.Name == "" {
			continue
		}
		parameters := any(map[string]any{"type": "object", "properties": map[string]any{}})
		if len(tool.InputSchema) > 0 {
			_ = json.Unmarshal(tool.InputSchema, &parameters)
		}
		tools = append(tools, map[string]any{"type": "function", "name": tool.Name, "description": tool.Description, "parameters": parameters})
	}
	effort := request.OutputConfig.Effort
	if effort == "" || effort == "auto" {
		effort = "high"
	}
	if effort == "ultra" {
		effort = "max"
	}
	payload := map[string]any{
		"model": claudeMuseModel, "instructions": instructions, "input": input, "tools": tools,
		"tool_choice": "auto", "parallel_tool_calls": true, "reasoning": map[string]any{"effort": effort},
		"store": false, "stream": request.Stream, "include": []string{"reasoning.encrypted_content"},
	}
	if request.MaxTokens > 0 {
		payload["max_output_tokens"] = request.MaxTokens
	}
	if session = strings.TrimSpace(session); session != "" {
		payload["prompt_cache_key"] = "claude-muse:" + session
	}
	if choice := translateMuseToolChoice(request.ToolChoice); choice != nil {
		payload["tool_choice"] = choice
	}
	encoded, err := json.Marshal(payload)
	return encoded, request.Stream, err
}

func translateMuseToolChoice(raw json.RawMessage) any {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var choice struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	if json.Unmarshal(raw, &choice) != nil {
		return nil
	}
	switch choice.Type {
	case "auto":
		return "auto"
	case "any":
		return "required"
	case "tool":
		if choice.Name != "" {
			return map[string]any{"type": "function", "name": choice.Name}
		}
	}
	return nil
}

func translateMuseMessage(raw json.RawMessage) ([]any, error) {
	var message struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(raw, &message); err != nil {
		return nil, err
	}
	if message.Role != "user" && message.Role != "assistant" {
		return nil, nil
	}
	var text string
	if json.Unmarshal(message.Content, &text) == nil {
		return []any{museResponsesMessage(message.Role, text)}, nil
	}
	var blocks []map[string]json.RawMessage
	if err := json.Unmarshal(message.Content, &blocks); err != nil {
		return nil, err
	}
	items := make([]any, 0, len(blocks))
	var textParts []string
	flush := func() {
		if len(textParts) > 0 {
			items = append(items, museResponsesMessage(message.Role, strings.Join(textParts, "\n")))
			textParts = nil
		}
	}
	for _, block := range blocks {
		var kind string
		_ = json.Unmarshal(block["type"], &kind)
		switch kind {
		case "text":
			var value string
			_ = json.Unmarshal(block["text"], &value)
			textParts = append(textParts, value)
		case "tool_use":
			flush()
			var id, name string
			var input any = map[string]any{}
			_ = json.Unmarshal(block["id"], &id)
			_ = json.Unmarshal(block["name"], &name)
			_ = json.Unmarshal(block["input"], &input)
			args, _ := json.Marshal(input)
			items = append(items, map[string]any{"type": "function_call", "call_id": id, "name": name, "arguments": string(args)})
		case "tool_result":
			flush()
			var callID string
			_ = json.Unmarshal(block["tool_use_id"], &callID)
			items = append(items, map[string]any{"type": "function_call_output", "call_id": callID, "output": museAnthropicText(block["content"])})
		}
	}
	flush()
	return items, nil
}

func museResponsesMessage(role, text string) map[string]any {
	contentType := "input_text"
	if role == "assistant" {
		contentType = "output_text"
	}
	return map[string]any{"type": "message", "role": role, "content": []any{map[string]any{"type": contentType, "text": text}}}
}

func museAnthropicText(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text
	}
	var blocks []map[string]json.RawMessage
	if json.Unmarshal(raw, &blocks) != nil {
		return ""
	}
	var parts []string
	for _, block := range blocks {
		var value string
		if json.Unmarshal(block["text"], &value) == nil && value != "" {
			parts = append(parts, value)
		}
	}
	return strings.Join(parts, "\n")
}

func translateMuseStream(w http.ResponseWriter, body io.Reader) error {
	state := &museStreamState{w: w, blocks: map[int]int{}}
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 64*1024), 8*1024*1024)
	var data strings.Builder
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if data.Len() > 0 {
				if err := state.consume([]byte(data.String())); err != nil {
					return err
				}
				data.Reset()
			}
			continue
		}
		if strings.HasPrefix(line, "data:") {
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.WriteString(strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
	}
	if data.Len() > 0 {
		if err := state.consume([]byte(data.String())); err != nil {
			return err
		}
	}
	return scanner.Err()
}

type museStreamState struct {
	w                                       http.ResponseWriter
	flusher                                 http.Flusher
	started                                 bool
	messageID                               string
	model                                   string
	nextIndex                               int
	blocks                                  map[int]int
	toolCalls                               bool
	inputTokens, outputTokens, cachedTokens int
}

func (s *museStreamState) consume(data []byte) error {
	if bytes.Equal(data, []byte("[DONE]")) {
		return nil
	}
	var event map[string]json.RawMessage
	if err := json.Unmarshal(data, &event); err != nil {
		return err
	}
	var eventType string
	_ = json.Unmarshal(event["type"], &eventType)
	switch eventType {
	case "response.created", "response.in_progress":
		var response struct {
			ID    string `json:"id"`
			Model string `json:"model"`
		}
		_ = json.Unmarshal(event["response"], &response)
		s.messageID, s.model = response.ID, response.Model
		s.start()
	case "response.output_item.added":
		var index int
		_ = json.Unmarshal(event["output_index"], &index)
		var item struct {
			Type   string `json:"type"`
			ID     string `json:"id"`
			CallID string `json:"call_id"`
			Name   string `json:"name"`
		}
		_ = json.Unmarshal(event["item"], &item)
		if item.Type == "function_call" {
			id := item.CallID
			if id == "" {
				id = item.ID
			}
			s.start()
			s.startBlock(index, "tool_use", map[string]any{"type": "tool_use", "id": id, "name": item.Name, "input": map[string]any{}})
			s.toolCalls = true
		}
	case "response.output_text.delta":
		var index int
		var delta string
		_ = json.Unmarshal(event["output_index"], &index)
		_ = json.Unmarshal(event["delta"], &delta)
		s.start()
		s.startBlock(index, "text", map[string]any{"type": "text", "text": ""})
		s.emit("content_block_delta", map[string]any{"type": "content_block_delta", "index": s.blocks[index], "delta": map[string]any{"type": "text_delta", "text": delta}})
	case "response.function_call_arguments.delta":
		var index int
		var delta string
		_ = json.Unmarshal(event["output_index"], &index)
		_ = json.Unmarshal(event["delta"], &delta)
		if _, ok := s.blocks[index]; ok {
			s.emit("content_block_delta", map[string]any{"type": "content_block_delta", "index": s.blocks[index], "delta": map[string]any{"type": "input_json_delta", "partial_json": delta}})
		}
	case "response.output_item.done":
		var index int
		_ = json.Unmarshal(event["output_index"], &index)
		if block, ok := s.blocks[index]; ok {
			s.emit("content_block_stop", map[string]any{"type": "content_block_stop", "index": block})
			delete(s.blocks, index)
		}
	case "response.completed", "response.done":
		var response struct {
			Model string `json:"model"`
			Usage struct {
				InputTokens        int `json:"input_tokens"`
				OutputTokens       int `json:"output_tokens"`
				InputTokensDetails struct {
					CachedTokens int `json:"cached_tokens"`
				} `json:"input_tokens_details"`
			} `json:"usage"`
		}
		_ = json.Unmarshal(event["response"], &response)
		s.model = response.Model
		s.inputTokens, s.outputTokens, s.cachedTokens = response.Usage.InputTokens, response.Usage.OutputTokens, response.Usage.InputTokensDetails.CachedTokens
		s.finish()
	case "error", "response.failed", "response.incomplete":
		return fmt.Errorf("Muse Responses stream failed: %s", string(data))
	}
	return nil
}

func (s *museStreamState) start() {
	if s.started {
		return
	}
	s.started = true
	if s.messageID == "" {
		s.messageID = "msg_claude_muse"
	}
	if s.model == "" {
		s.model = claudeMuseModel
	}
	s.flusher, _ = s.w.(http.Flusher)
	s.emit("message_start", map[string]any{"type": "message_start", "message": map[string]any{"id": s.messageID, "type": "message", "role": "assistant", "model": s.model, "content": []any{}, "stop_reason": nil, "stop_sequence": nil, "usage": map[string]any{"input_tokens": 0, "output_tokens": 0}}})
}
func (s *museStreamState) startBlock(outputIndex int, kind string, content any) {
	if _, ok := s.blocks[outputIndex]; ok {
		return
	}
	index := s.nextIndex
	s.nextIndex++
	s.blocks[outputIndex] = index
	s.emit("content_block_start", map[string]any{"type": "content_block_start", "index": index, "content_block": content})
	_ = kind
}
func (s *museStreamState) finish() {
	s.start()
	for output, index := range s.blocks {
		s.emit("content_block_stop", map[string]any{"type": "content_block_stop", "index": index})
		delete(s.blocks, output)
	}
	stop := "end_turn"
	if s.toolCalls {
		stop = "tool_use"
	}
	s.emit("message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": stop, "stop_sequence": nil}, "usage": museClaudeUsage(s.inputTokens, s.outputTokens, s.cachedTokens)})
	s.emit("message_stop", map[string]any{"type": "message_stop"})
}
func (s *museStreamState) emit(event string, payload any) {
	body, _ := json.Marshal(payload)
	_, _ = fmt.Fprintf(s.w, "event: %s\ndata: %s\n\n", event, body)
	if s.flusher != nil {
		s.flusher.Flush()
	}
}

func translateMuseResponse(body []byte) ([]byte, error) {
	var response struct {
		ID     string `json:"id"`
		Model  string `json:"model"`
		Output []struct {
			Type      string `json:"type"`
			ID        string `json:"id"`
			CallID    string `json:"call_id"`
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
			Content   []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
		Usage struct {
			InputTokens        int `json:"input_tokens"`
			OutputTokens       int `json:"output_tokens"`
			InputTokensDetails struct {
				CachedTokens int `json:"cached_tokens"`
			} `json:"input_tokens_details"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		return nil, err
	}
	content := []any{}
	stop := "end_turn"
	for _, item := range response.Output {
		switch item.Type {
		case "message":
			for _, part := range item.Content {
				if part.Type == "output_text" && part.Text != "" {
					content = append(content, map[string]any{"type": "text", "text": part.Text})
				}
			}
		case "function_call":
			stop = "tool_use"
			var input any = map[string]any{}
			_ = json.Unmarshal([]byte(item.Arguments), &input)
			id := item.CallID
			if id == "" {
				id = item.ID
			}
			content = append(content, map[string]any{"type": "tool_use", "id": id, "name": item.Name, "input": input})
		}
	}
	if response.ID == "" {
		response.ID = "msg_claude_muse"
	}
	return json.Marshal(map[string]any{"id": response.ID, "type": "message", "role": "assistant", "model": claudeMuseModel, "content": content, "stop_reason": stop, "stop_sequence": nil, "usage": museClaudeUsage(response.Usage.InputTokens, response.Usage.OutputTokens, response.Usage.InputTokensDetails.CachedTokens)})
}

func museClaudeUsage(input, output, cached int) map[string]any {
	if cached < 0 {
		cached = 0
	}
	uncached := input - cached
	if uncached < 0 {
		uncached = 0
	}
	return map[string]any{"input_tokens": uncached, "output_tokens": output, "cache_creation_input_tokens": 0, "cache_read_input_tokens": cached}
}
func museClaudeError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"type": "error", "error": map[string]any{"type": "api_error", "message": message}})
}
func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
