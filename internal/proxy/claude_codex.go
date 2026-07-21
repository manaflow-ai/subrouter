package proxy

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/internal/session"
)

const (
	claudeCodexPrefix          = "/claude-codex"
	claudeCodexModel           = "gpt-5.6-sol"
	claudeCodexReasoningEffort = "medium"
)

var claudeCodexModels = map[string]string{
	"claude-codex-sol":   "gpt-5.6-sol",
	"claude-codex-terra": "gpt-5.6-terra",
	"claude-codex-luna":  "gpt-5.6-luna",
	"gpt-5.6-sol":        "gpt-5.6-sol",
	"gpt-5.6-terra":      "gpt-5.6-terra",
	"gpt-5.6-luna":       "gpt-5.6-luna",
}

type claudeCodexCompactionHook struct {
	SessionID          string `json:"session_id"`
	HookEventName      string `json:"hook_event_name"`
	Trigger            string `json:"trigger"`
	CustomInstructions string `json:"custom_instructions"`
}

type claudeCodexPendingCompaction struct {
	Trigger   string
	CreatedAt time.Time
}

var claudeCodexPendingCompactions sync.Map

func ClaudeCodexModelName() string {
	return claudeCodexModel
}

func ClaudeCodexReasoningEffortName() string {
	return claudeCodexReasoningEffort
}

type claudeCodexRequest struct {
	System       json.RawMessage   `json:"system"`
	Messages     []json.RawMessage `json:"messages"`
	Tools        []json.RawMessage `json:"tools"`
	ToolChoice   json.RawMessage   `json:"tool_choice"`
	Model        string            `json:"model"`
	OutputConfig struct {
		Effort string `json:"effort"`
	} `json:"output_config"`
	Stream bool `json:"stream"`
}

func claudeCodexRequestPath(path string) bool {
	return path == claudeCodexPrefix || strings.HasPrefix(path, claudeCodexPrefix+"/")
}

func (s Server) serveClaudeCodex(w http.ResponseWriter, r *http.Request, agentType, sessionID string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if strings.HasSuffix(r.URL.Path, "/hooks/pre-compact") {
		s.serveClaudeCodexPreCompactHook(w, r)
		return
	}
	if strings.HasSuffix(r.URL.Path, "/count_tokens") {
		s.serveClaudeCodexCountTokens(w, r)
		return
	}
	if !strings.HasSuffix(r.URL.Path, "/messages") {
		http.NotFound(w, r)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, replayablePostMaxBodyBytes+1))
	if err != nil {
		http.Error(w, "read Claude request", http.StatusBadRequest)
		return
	}
	if int64(len(body)) > replayablePostMaxBodyBytes {
		http.Error(w, "Claude request exceeds bridge limit", http.StatusRequestEntityTooLarge)
		return
	}
	responsesBody, stream, err := translateClaudeRequest(body)
	if err != nil {
		writeClaudeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	compaction, isCompaction := claudeCodexCompactionForRequest(sessionID, body)

	// Use a distinct session namespace while selecting from the Codex account
	// pool. A Claude session and a native Codex session may carry the same ID;
	// they must not steal each other's sticky assignment.
	bridgeAgent := "claude-codex"
	if agentType == "" {
		agentType = bridgeAgent
	}
	selectionRequest := r.Clone(r.Context())
	selectionRequest.Body = io.NopCloser(bytes.NewReader(responsesBody))
	selectionRequest.ContentLength = int64(len(responsesBody))
	selectionRequest.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(responsesBody)), nil
	}
	account, sessionID, userEmail, err := s.accountForSessionProvider(accounts.ProviderCodex, bridgeAgent, sessionID, selectionRequest)
	if err != nil {
		writeClaudeError(w, http.StatusServiceUnavailable, "api_error", err.Error())
		return
	}
	account, err = s.refreshSelectedAccount(r.Context(), accounts.ProviderCodex, bridgeAgent, sessionID, userEmail, selectionRequest, account)
	if err != nil {
		writeClaudeError(w, http.StatusServiceUnavailable, "api_error", "refresh selected Codex account: "+err.Error())
		return
	}

	upstream := s.upstreamForRequest("/v1/responses", account)
	if upstream == nil {
		writeClaudeError(w, http.StatusServiceUnavailable, "api_error", "no Codex upstream configured")
		return
	}
	target := cloneURL(upstream)
	target.Scheme = websocketScheme(target.Scheme)
	target.Path = joinURLPath(upstream.Path, s.pathForUpstream("/v1/responses", account))
	target.RawQuery = ""
	headers := http.Header{}
	setAccountAuthHeaders(headers, account)
	// When one Subrouter is used as this server's forced upstream, preserve the
	// bridge's Codex namespace so the upstream Subrouter selects its own pooled
	// ChatGPT credential. Direct chatgpt.com connections never receive internal
	// Subrouter headers.
	if !strings.EqualFold(target.Hostname(), "chatgpt.com") {
		headers.Set("X-Subrouter-Agent", bridgeAgent)
		headers.Set("X-Subrouter-Session", sessionID)
	}
	dialer := *websocket.DefaultDialer
	dialer.EnableCompression = true
	connection, handshake, err := dialer.DialContext(r.Context(), target.String(), headers)
	if err != nil {
		status := http.StatusBadGateway
		message := "Codex Responses WebSocket connection failed"
		if handshake != nil {
			status = handshake.StatusCode
			if handshake.Body != nil {
				defer handshake.Body.Close()
				body, _ := io.ReadAll(io.LimitReader(handshake.Body, 64*1024))
				if len(body) > 0 {
					message += ": " + string(body)
				}
			}
		}
		writeClaudeError(w, status, "api_error", message)
		return
	}
	defer connection.Close()
	var wsRequest map[string]any
	if err := json.Unmarshal(responsesBody, &wsRequest); err != nil {
		writeClaudeError(w, http.StatusInternalServerError, "api_error", "encode Codex request")
		return
	}
	wsRequest["type"] = "response.create"
	upstreamModel, _ := wsRequest["model"].(string)
	reasoning, _ := wsRequest["reasoning"].(map[string]any)
	reasoningEffort, _ := reasoning["effort"].(string)
	// Keep successive turns from one Claude Code conversation on the same
	// OpenAI prompt-cache routing key. The translated instructions, tools, and
	// prior messages remain deterministic, so their shared prefix can be reused.
	if sessionID != "" {
		wsRequest["prompt_cache_key"] = "claude-codex:" + sessionID
	}
	if isCompaction {
		wsRequest["client_metadata"] = claudeCodexCompactionMetadata(sessionID, compaction.Trigger)
	}
	if err := connection.WriteJSON(wsRequest); err != nil {
		writeClaudeError(w, http.StatusBadGateway, "api_error", "send Codex request")
		return
	}

	w.Header().Set("X-Subrouter-Bridge", "claude-codex")
	w.Header().Set("X-Subrouter-Upstream-Provider", "codex-chatgpt")
	w.Header().Set("X-Subrouter-Upstream-Model", upstreamModel)
	w.Header().Set("X-Subrouter-Reasoning-Effort", reasoningEffort)
	if isCompaction {
		w.Header().Set("X-Subrouter-Compaction", "codex-responses")
	}
	if s.Logger != nil {
		s.Logger.Info("claude codex bridge request", "agent", bridgeAgent, "session", sessionID, "account", account.ID, "model", upstreamModel, "reasoning_effort", reasoningEffort, "request_kind", map[bool]string{true: "compaction", false: "turn"}[isCompaction], "upstream", upstream.Host)
	}
	if s.SchedulerRef != nil {
		s.SchedulerRef.NoteRouted(accounts.ProviderCodex, account.ID)
	}

	if stream {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		if err := translateCodexWebsocketStream(w, connection); err != nil && s.Logger != nil && r.Context().Err() == nil {
			s.Logger.Error("claude codex stream translation failed", "session", sessionID, "error", err)
		}
		return
	}

	responseBody, err := collectCodexWebsocketResponse(connection)
	if err != nil {
		writeClaudeError(w, http.StatusBadGateway, "api_error", err.Error())
		return
	}
	translated, err := translateCodexResponse(bytes.NewReader(responseBody))
	if err != nil {
		writeClaudeError(w, http.StatusBadGateway, "api_error", "invalid Codex response")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(translated)
}

func (s Server) serveClaudeCodexPreCompactHook(w http.ResponseWriter, r *http.Request) {
	var hook claudeCodexCompactionHook
	if err := json.NewDecoder(io.LimitReader(r.Body, 64*1024)).Decode(&hook); err != nil {
		writeClaudeError(w, http.StatusBadRequest, "invalid_request_error", "invalid PreCompact hook payload")
		return
	}
	if hook.HookEventName != "PreCompact" || hook.SessionID == "" || len(hook.SessionID) > 256 {
		writeClaudeError(w, http.StatusBadRequest, "invalid_request_error", "invalid PreCompact hook event")
		return
	}
	trigger := hook.Trigger
	if trigger != "manual" && trigger != "auto" {
		trigger = "auto"
	}
	claudeCodexPendingCompactions.Store(hook.SessionID, claudeCodexPendingCompaction{
		Trigger: trigger, CreatedAt: time.Now(),
	})
	if s.Logger != nil {
		s.Logger.Info("claude codex compaction hook", "session", hook.SessionID, "trigger", trigger)
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, "{}\n")
}

func claudeCodexCompactionForRequest(sessionID string, body []byte) (claudeCodexPendingCompaction, bool) {
	if !isClaudeCompactionRequest(body) {
		return claudeCodexPendingCompaction{}, false
	}
	if value, ok := claudeCodexPendingCompactions.LoadAndDelete(sessionID); ok {
		pending, valid := value.(claudeCodexPendingCompaction)
		if valid && time.Since(pending.CreatedAt) <= 5*time.Minute {
			return pending, true
		}
	}
	return claudeCodexPendingCompaction{Trigger: "auto", CreatedAt: time.Now()}, true
}

func isClaudeCompactionRequest(body []byte) bool {
	var request struct {
		Messages []json.RawMessage `json:"messages"`
	}
	if json.Unmarshal(body, &request) != nil {
		return false
	}
	for i := len(request.Messages) - 1; i >= 0; i-- {
		var message struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		}
		if json.Unmarshal(request.Messages[i], &message) != nil || message.Role != "user" {
			continue
		}
		text := anthropicText(message.Content)
		return strings.Contains(text, "Your task is to create a detailed summary of the conversation so far") ||
			strings.Contains(text, "Before providing your final summary, wrap your analysis in <analysis> tags")
	}
	return false
}

func claudeCodexCompactionMetadata(sessionID, trigger string) map[string]string {
	metadata := map[string]any{
		"session_id":   sessionID,
		"thread_id":    sessionID,
		"request_kind": "compaction",
		"compaction": map[string]any{
			"trigger": trigger, "reason": map[bool]string{true: "user_requested", false: "context_limit"}[trigger == "manual"],
			"implementation": "responses", "phase": "standalone_turn", "strategy": "memento",
		},
	}
	encoded, _ := json.Marshal(metadata)
	return map[string]string{
		"session_id":            sessionID,
		"thread_id":             sessionID,
		"x-codex-turn-metadata": string(encoded),
	}
}

func (s Server) serveClaudeCodexCountTokens(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, replayablePostMaxBodyBytes+1))
	if err != nil {
		writeClaudeError(w, http.StatusBadRequest, "invalid_request_error", "read request")
		return
	}
	// Claude Code uses this as a preflight estimate. The Responses request is
	// the authoritative token count; this conservative byte estimate avoids an
	// extra paid model request while preserving Claude Code's context guard.
	tokens := (len(body) + 2) / 3
	if tokens < 1 {
		tokens = 1
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Subrouter-Bridge", "claude-codex")
	writeJSON(w, map[string]any{"input_tokens": tokens})
}

func translateClaudeRequest(body []byte) ([]byte, bool, error) {
	var request claudeCodexRequest
	if err := json.Unmarshal(body, &request); err != nil {
		return nil, false, fmt.Errorf("decode Claude request: %w", err)
	}
	model, err := resolveClaudeCodexModel(request.Model)
	if err != nil {
		return nil, false, err
	}
	effort, err := resolveClaudeCodexEffort(request.OutputConfig.Effort)
	if err != nil {
		return nil, false, err
	}
	instructions := anthropicText(request.System)
	input := make([]any, 0, len(request.Messages))
	for _, raw := range request.Messages {
		items, err := translateClaudeMessage(raw)
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
		if err := json.Unmarshal(raw, &tool); err != nil || tool.Name == "" {
			continue
		}
		parameters := any(map[string]any{"type": "object", "properties": map[string]any{}})
		if len(tool.InputSchema) > 0 {
			_ = json.Unmarshal(tool.InputSchema, &parameters)
		}
		tools = append(tools, map[string]any{
			"type": "function", "name": tool.Name, "description": tool.Description, "parameters": parameters,
		})
	}
	payload := map[string]any{
		"model":               model,
		"instructions":        instructions,
		"input":               input,
		"tools":               tools,
		"tool_choice":         "auto",
		"parallel_tool_calls": true,
		"reasoning":           map[string]any{"effort": effort},
		"store":               false,
		"stream":              true,
		"include":             []string{"reasoning.encrypted_content"},
	}
	if choice := translateClaudeToolChoice(request.ToolChoice); choice != nil {
		payload["tool_choice"] = choice
	}
	translated, err := json.Marshal(payload)
	return translated, request.Stream, err
}

func resolveClaudeCodexModel(model string) (string, error) {
	if model == "" {
		return claudeCodexModel, nil
	}
	if upstream, ok := claudeCodexModels[model]; ok {
		return upstream, nil
	}
	return "", fmt.Errorf("unsupported Claude-Codex model %q; choose Sol, Terra, or Luna with /model", model)
}

func claudeCodexClientModel(model string) string {
	switch model {
	case "gpt-5.6-terra":
		return "claude-codex-terra"
	case "gpt-5.6-luna":
		return "claude-codex-luna"
	default:
		return "claude-codex-sol"
	}
}

func resolveClaudeCodexEffort(effort string) (string, error) {
	if effort == "" || effort == "auto" {
		return claudeCodexReasoningEffort, nil
	}
	switch effort {
	case "low", "medium", "high", "xhigh", "max":
		return effort, nil
	default:
		return "", fmt.Errorf("unsupported Claude-Codex effort %q; choose low, medium, high, xhigh, or max", effort)
	}
}

func translateClaudeMessage(raw json.RawMessage) ([]any, error) {
	var message struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(raw, &message); err != nil {
		return nil, fmt.Errorf("decode Claude message: %w", err)
	}
	if message.Role != "user" && message.Role != "assistant" {
		return nil, nil
	}
	var text string
	if json.Unmarshal(message.Content, &text) == nil {
		return []any{responsesMessage(message.Role, text)}, nil
	}
	var blocks []map[string]json.RawMessage
	if err := json.Unmarshal(message.Content, &blocks); err != nil {
		return nil, fmt.Errorf("decode Claude content: %w", err)
	}
	items := make([]any, 0, len(blocks)+1)
	textParts := make([]string, 0, len(blocks))
	flushText := func() {
		if len(textParts) == 0 {
			return
		}
		items = append(items, responsesMessage(message.Role, strings.Join(textParts, "\n")))
		textParts = textParts[:0]
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
			flushText()
			var id, name string
			var input any = map[string]any{}
			_ = json.Unmarshal(block["id"], &id)
			_ = json.Unmarshal(block["name"], &name)
			_ = json.Unmarshal(block["input"], &input)
			arguments, _ := json.Marshal(input)
			items = append(items, map[string]any{"type": "function_call", "call_id": id, "name": name, "arguments": string(arguments)})
		case "tool_result":
			flushText()
			var callID string
			_ = json.Unmarshal(block["tool_use_id"], &callID)
			items = append(items, map[string]any{"type": "function_call_output", "call_id": callID, "output": anthropicText(block["content"])})
		}
	}
	flushText()
	return items, nil
}

func responsesMessage(role, text string) map[string]any {
	contentType := "input_text"
	if role == "assistant" {
		contentType = "output_text"
	}
	return map[string]any{
		"type": "message", "role": role,
		"content": []any{map[string]any{"type": contentType, "text": text}},
	}
}

func anthropicText(raw json.RawMessage) string {
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
	parts := make([]string, 0, len(blocks))
	for _, block := range blocks {
		var value string
		if json.Unmarshal(block["text"], &value) == nil && value != "" {
			parts = append(parts, value)
		}
	}
	return strings.Join(parts, "\n")
}

func translateClaudeToolChoice(raw json.RawMessage) any {
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
	case "none":
		return "none"
	case "tool":
		return map[string]any{"type": "function", "name": choice.Name}
	default:
		return nil
	}
}

func translateCodexResponse(body io.Reader) ([]byte, error) {
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
	if err := json.NewDecoder(body).Decode(&response); err != nil {
		return nil, err
	}
	content := make([]any, 0, len(response.Output))
	stopReason := "end_turn"
	for _, item := range response.Output {
		switch item.Type {
		case "message":
			for _, part := range item.Content {
				if part.Type == "output_text" && part.Text != "" {
					content = append(content, map[string]any{"type": "text", "text": part.Text})
				}
			}
		case "function_call":
			stopReason = "tool_use"
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
		response.ID = "msg_subrouter_codex"
	}
	return json.Marshal(map[string]any{
		"id": response.ID, "type": "message", "role": "assistant", "model": claudeCodexClientModel(response.Model),
		"content": content, "stop_reason": stopReason, "stop_sequence": nil,
		"usage": claudeUsage(response.Usage.InputTokens, response.Usage.OutputTokens, response.Usage.InputTokensDetails.CachedTokens),
	})
}

type claudeCodexStreamState struct {
	w            http.ResponseWriter
	flusher      http.Flusher
	started      bool
	messageID    string
	model        string
	nextIndex    int
	blocks       map[int]int
	blockTypes   map[int]string
	toolCalls    bool
	inputTokens  int
	outputTokens int
	cachedTokens int
}

func translateCodexWebsocketStream(w http.ResponseWriter, connection *websocket.Conn) error {
	state := &claudeCodexStreamState{w: w, blocks: map[int]int{}, blockTypes: map[int]string{}}
	state.flusher, _ = w.(http.Flusher)
	for {
		messageType, body, err := connection.ReadMessage()
		if err != nil {
			return fmt.Errorf("read Responses WebSocket: %w", err)
		}
		if messageType != websocket.TextMessage {
			continue
		}
		terminal, eventErr := codexWebsocketTerminal(body)
		if eventErr != nil {
			return eventErr
		}
		if err := state.consume(body); err != nil {
			return err
		}
		if terminal {
			return nil
		}
	}
}

func collectCodexWebsocketResponse(connection *websocket.Conn) ([]byte, error) {
	response := map[string]any{
		"id":     "resp_subrouter_codex",
		"model":  claudeCodexModel,
		"output": []any{},
		"usage":  map[string]any{},
	}
	output := make([]any, 0)
	for {
		messageType, body, err := connection.ReadMessage()
		if err != nil {
			return nil, fmt.Errorf("read Responses WebSocket: %w", err)
		}
		if messageType != websocket.TextMessage {
			continue
		}
		terminal, eventErr := codexWebsocketTerminal(body)
		if eventErr != nil {
			return nil, eventErr
		}
		var event map[string]json.RawMessage
		if json.Unmarshal(body, &event) != nil {
			continue
		}
		var eventType string
		_ = json.Unmarshal(event["type"], &eventType)
		switch eventType {
		case "response.created":
			var created map[string]any
			if json.Unmarshal(event["response"], &created) == nil {
				if id, ok := created["id"].(string); ok && id != "" {
					response["id"] = id
				}
				if model, ok := created["model"].(string); ok && model != "" {
					response["model"] = model
				}
			}
		case "response.output_item.done":
			var item any
			if json.Unmarshal(event["item"], &item) == nil {
				output = append(output, item)
			}
		case "response.completed", "response.done":
			var completed map[string]any
			if json.Unmarshal(event["response"], &completed) == nil {
				if id, ok := completed["id"].(string); ok && id != "" {
					response["id"] = id
				}
				if model, ok := completed["model"].(string); ok && model != "" {
					response["model"] = model
				}
				if usage, ok := completed["usage"]; ok {
					response["usage"] = usage
				}
				if completedOutput, ok := completed["output"].([]any); ok && len(completedOutput) > 0 {
					output = completedOutput
				}
			}
		}
		if terminal {
			response["output"] = output
			return json.Marshal(response)
		}
	}
}

func codexWebsocketTerminal(body []byte) (bool, error) {
	var event map[string]any
	if err := json.Unmarshal(body, &event); err != nil {
		return false, nil
	}
	eventType, _ := event["type"].(string)
	switch eventType {
	case "response.completed", "response.done":
		return true, nil
	case "error", "response.failed", "response.incomplete":
		message := "Codex Responses WebSocket failed"
		if nested, ok := event["error"].(map[string]any); ok {
			if value, ok := nested["message"].(string); ok && value != "" {
				message = value
			}
		}
		return true, fmt.Errorf("%s", message)
	default:
		return false, nil
	}
}

func translateCodexStream(w http.ResponseWriter, body io.Reader) error {
	state := &claudeCodexStreamState{w: w, blocks: map[int]int{}, blockTypes: map[int]string{}}
	state.flusher, _ = w.(http.Flusher)
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
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

func (s *claudeCodexStreamState) consume(data []byte) error {
	if bytes.Equal(data, []byte("[DONE]")) {
		return nil
	}
	var event map[string]json.RawMessage
	if err := json.Unmarshal(data, &event); err != nil {
		return fmt.Errorf("decode Responses event: %w", err)
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
		if response.Model != "" {
			s.model = claudeCodexClientModel(response.Model)
		}
		s.start(response.ID)
	case "response.output_item.added":
		s.start("")
		var outputIndex int
		var item struct {
			Type   string `json:"type"`
			ID     string `json:"id"`
			CallID string `json:"call_id"`
			Name   string `json:"name"`
		}
		_ = json.Unmarshal(event["output_index"], &outputIndex)
		_ = json.Unmarshal(event["item"], &item)
		if item.Type == "function_call" {
			id := item.CallID
			if id == "" {
				id = item.ID
			}
			s.startBlock(outputIndex, "tool_use", map[string]any{"type": "tool_use", "id": id, "name": item.Name, "input": map[string]any{}})
			s.toolCalls = true
		}
	case "response.output_text.delta":
		var outputIndex int
		var delta string
		_ = json.Unmarshal(event["output_index"], &outputIndex)
		_ = json.Unmarshal(event["delta"], &delta)
		s.start("")
		s.startBlock(outputIndex, "text", map[string]any{"type": "text", "text": ""})
		s.emit("content_block_delta", map[string]any{"type": "content_block_delta", "index": s.blocks[outputIndex], "delta": map[string]any{"type": "text_delta", "text": delta}})
	case "response.function_call_arguments.delta":
		var outputIndex int
		var delta string
		_ = json.Unmarshal(event["output_index"], &outputIndex)
		_ = json.Unmarshal(event["delta"], &delta)
		s.emit("content_block_delta", map[string]any{"type": "content_block_delta", "index": s.blocks[outputIndex], "delta": map[string]any{"type": "input_json_delta", "partial_json": delta}})
	case "response.output_item.done":
		var outputIndex int
		_ = json.Unmarshal(event["output_index"], &outputIndex)
		if _, ok := s.blocks[outputIndex]; ok {
			s.emit("content_block_stop", map[string]any{"type": "content_block_stop", "index": s.blocks[outputIndex]})
			delete(s.blocks, outputIndex)
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
		if response.Model != "" {
			s.model = claudeCodexClientModel(response.Model)
		}
		s.inputTokens = response.Usage.InputTokens
		s.outputTokens = response.Usage.OutputTokens
		s.cachedTokens = response.Usage.InputTokensDetails.CachedTokens
		s.finish()
	case "error", "response.failed", "response.incomplete":
		return fmt.Errorf("Responses stream failed: %s", string(data))
	}
	return nil
}

func (s *claudeCodexStreamState) start(id string) {
	if s.started {
		return
	}
	if id == "" {
		id = "msg_subrouter_codex"
	}
	s.messageID = id
	s.started = true
	if s.model == "" {
		s.model = "claude-codex-sol"
	}
	s.emit("message_start", map[string]any{"type": "message_start", "message": map[string]any{
		"id": id, "type": "message", "role": "assistant", "model": s.model,
		"content": []any{}, "stop_reason": nil, "stop_sequence": nil,
		"usage": map[string]any{"input_tokens": 0, "output_tokens": 0},
	}})
}

func (s *claudeCodexStreamState) startBlock(outputIndex int, kind string, content any) {
	if _, ok := s.blocks[outputIndex]; ok {
		return
	}
	index := s.nextIndex
	s.nextIndex++
	s.blocks[outputIndex] = index
	s.blockTypes[outputIndex] = kind
	s.emit("content_block_start", map[string]any{"type": "content_block_start", "index": index, "content_block": content})
}

func (s *claudeCodexStreamState) finish() {
	if !s.started {
		s.start("")
	}
	for outputIndex, index := range s.blocks {
		s.emit("content_block_stop", map[string]any{"type": "content_block_stop", "index": index})
		delete(s.blocks, outputIndex)
	}
	stopReason := "end_turn"
	if s.toolCalls {
		stopReason = "tool_use"
	}
	s.emit("message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": stopReason, "stop_sequence": nil}, "usage": claudeUsage(s.inputTokens, s.outputTokens, s.cachedTokens)})
	s.emit("message_stop", map[string]any{"type": "message_stop"})
}

func claudeUsage(inputTokens, outputTokens, cachedTokens int) map[string]any {
	if cachedTokens < 0 {
		cachedTokens = 0
	}
	uncachedTokens := inputTokens - cachedTokens
	if uncachedTokens < 0 {
		uncachedTokens = 0
	}
	return map[string]any{
		"input_tokens":                uncachedTokens,
		"output_tokens":               outputTokens,
		"cache_creation_input_tokens": 0,
		"cache_read_input_tokens":     cachedTokens,
	}
}

func (s *claudeCodexStreamState) emit(event string, payload any) {
	body, _ := json.Marshal(payload)
	_, _ = fmt.Fprintf(s.w, "event: %s\ndata: %s\n\n", event, body)
	if s.flusher != nil {
		s.flusher.Flush()
	}
}

func translateCodexError(w http.ResponseWriter, response *http.Response) {
	body, _ := io.ReadAll(io.LimitReader(response.Body, 64*1024))
	message := "Codex upstream returned HTTP " + response.Status
	var payload map[string]any
	if json.Unmarshal(body, &payload) == nil {
		if nested, ok := payload["error"].(map[string]any); ok {
			if value, ok := nested["message"].(string); ok && value != "" {
				message = value
			}
		} else if value, ok := payload["message"].(string); ok && value != "" {
			message = value
		}
	}
	writeClaudeError(w, response.StatusCode, "api_error", message)
}

func writeClaudeError(w http.ResponseWriter, status int, errorType, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"type": "error", "error": map[string]any{"type": errorType, "message": message}})
}

// Keep the compiler honest when this file is built independently of the main
// proxy handler's session extraction path.
var _ = session.NormalizeModel
