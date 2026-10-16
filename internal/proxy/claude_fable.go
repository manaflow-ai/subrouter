package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/manaflow-ai/subrouter/session"
)

// claudeFableModel reports whether a Claude request targets a Fable model.
func claudeFableModel(model string) bool {
	return strings.Contains(strings.ToLower(model), "fable")
}

// claudeBedrockFallbackModel reports whether a Claude request for this model
// may use the fallback chain: Fable always, every model once a Bedrock model
// is configured for all of them.
func (s Server) claudeBedrockFallbackModel(model string) bool {
	return claudeFableModel(model) || s.ClaudeBedrockModelID != ""
}

// claudeBedrockModelID is the Bedrock inference profile the chain uses.
func (s Server) claudeBedrockModelID() string {
	if s.ClaudeBedrockModelID != "" {
		return s.ClaudeBedrockModelID
	}
	return bedrockFableModelID
}

// claudeFableEnabled reports whether a Fable fallback route is configured (the
// Bedrock gateway or a dedicated Anthropic API key). Fable routing order is
// subscription pool first, then Bedrock, then the API key; this only gates the
// fallback stages, never the pool.
func (s Server) claudeFableEnabled() bool {
	if s.ClaudeFableAPIKey != "" {
		return true
	}
	return s.Bedrock != nil && s.Bedrock.configured()
}

// claudeFableRequest reports whether this is a Claude Messages call for a Fable
// model. ExtractModel restores the request body after probing it.
func (s Server) claudeFableRequest(r *http.Request) bool {
	if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/v1/messages") {
		return false
	}
	return claudeFableModel(session.ExtractModel(r, s.MaxBodyBytes))
}

// serveClaudeFableBedrockPrimary serves a Fable request from AWS Bedrock before
// the subscription pool is consulted (FableBedrockPrimary mode). It streams the
// response only on a 2xx from Bedrock; on any non-2xx status or a Bedrock error
// it restores the request body and returns false so the caller continues to the
// normal pool path (which keeps its own Bedrock/API-key fallback). This makes
// Bedrock the primary Fable route without ever hard-failing when Bedrock is
// throttled or unreachable.
// writeAnthropicError writes an Anthropic-format error body.
func writeAnthropicError(w http.ResponseWriter, status int, errorType, message string) {
	body, _ := json.Marshal(map[string]any{
		"type":  "error",
		"error": map[string]string{"type": errorType, "message": message},
	})
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func (s Server) serveClaudeFableBedrockPrimary(w http.ResponseWriter, r *http.Request) bool {
	body, err := io.ReadAll(io.LimitReader(r.Body, replayablePostMaxBodyBytes))
	if err != nil {
		return false
	}
	restore := func() {
		r.Body = io.NopCloser(bytes.NewReader(body))
		r.ContentLength = int64(len(body))
		r.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(body)), nil }
	}
	resp, err := s.claudeFableBedrockResponse(r.Context(), body)
	if err != nil {
		if s.Logger != nil && r.Context().Err() == nil {
			s.Logger.Warn("fable bedrock-primary request failed", "error", err, "surfaced", s.ClaudeBedrockPrimary)
		}
		if s.ClaudeBedrockPrimary {
			// Bedrock is the Claude route, so its refusal is the answer;
			// the pool behind it may be switched off and would only hide it.
			writeAnthropicError(w, http.StatusServiceUnavailable, "overloaded_error", "subrouter Bedrock route unavailable: "+err.Error())
			return true
		}
		restore()
		return false
	}
	if resp.StatusCode == http.StatusBadRequest && s.Logger != nil {
		// Roles and block types only, no content: enough to add the next
		// Bedrock shape rewrite.
		s.Logger.Warn("claude bedrock 400 request shape", "shape", bedrockMessageShape(stripBedrockUnsupportedFields(body)))
	}
	if (resp.StatusCode < 200 || resp.StatusCode >= 300) && !s.ClaudeBedrockPrimary {
		if s.Logger != nil {
			s.Logger.Warn("fable bedrock-primary non-2xx, falling through to pool", "status", resp.StatusCode)
		}
		_ = resp.Body.Close()
		restore()
		return false
	}
	if s.Logger != nil {
		s.Logger.Info("serving claude fable via bedrock-primary", "status", resp.StatusCode)
	}
	defer resp.Body.Close()
	for key, values := range resp.Header {
		if isHopByHopHeader(key) {
			continue
		}
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
	markUpstreamResponse(r.Context(), true)
	w.WriteHeader(resp.StatusCode)
	flushingCopy(w, resp.Body, nil)
	return true
}

// serveClaudeFableFallback serves a Fable request straight off the fallback
// chain (Bedrock, then the dedicated API key). Used when the subscription pool
// cannot even start: no usable Claude OAuth account or selection failed. It
// restores the request body and returns false when no fallback produced a
// response, so the caller can surface its own error.
func (s Server) serveClaudeFableFallback(w http.ResponseWriter, r *http.Request) bool {
	body, err := io.ReadAll(io.LimitReader(r.Body, replayablePostMaxBodyBytes))
	if err != nil {
		return false
	}
	resp, ok := s.claudeFableFallbackResponse(r, body)
	if !ok {
		r.Body = io.NopCloser(bytes.NewReader(body))
		r.ContentLength = int64(len(body))
		r.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(body)), nil }
		return false
	}
	if s.Logger != nil {
		// Same signal the transport-level give-up logs emit; without it a
		// handler-level fallback (no usable OAuth account at selection) is
		// invisible and Bedrock activity cannot be attributed to the chain.
		s.Logger.Warn("serving claude fable via fallback chain", "reason", "no_usable_account", "fallback_status", resp.StatusCode)
	}
	defer resp.Body.Close()
	for key, values := range resp.Header {
		if isHopByHopHeader(key) {
			continue
		}
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
	markUpstreamResponse(r.Context(), true)
	w.WriteHeader(resp.StatusCode)
	flushingCopy(w, resp.Body, nil)
	return true
}

// claudeFableFallbackResponse runs the Fable fallback chain for a request the
// subscription pool could not serve: AWS Bedrock first, then the dedicated
// Anthropic API key when Bedrock is throttled, overloaded, or unreachable. The
// returned response is native Anthropic shape (JSON or SSE). ok is false when
// no fallback is configured or every configured stage failed without producing
// an HTTP response, so the caller keeps whatever it was about to return.
func (s Server) claudeFableFallbackResponse(r *http.Request, body []byte) (*http.Response, bool) {
	if s.Bedrock != nil && s.Bedrock.configured() {
		resp, err := s.claudeFableBedrockResponse(r.Context(), body)
		if err == nil {
			if !claudeFableBedrockUnusable(resp.StatusCode) || s.ClaudeFableAPIKey == "" || !claudeFableModel(bodyModel(body)) {
				return resp, true
			}
			if s.Logger != nil {
				s.Logger.Warn("claude-fable bedrock unusable, falling back to api key", "status", resp.StatusCode)
			}
			_ = resp.Body.Close()
		} else {
			if r.Context().Err() != nil {
				return nil, false
			}
			if s.Logger != nil {
				s.Logger.Error("claude-fable bedrock request failed", "error", err)
			}
		}
	}
	// The API key is Fable's alone and has no cap; other models that reach
	// the chain through ClaudeBedrockModelID stop at Bedrock.
	if s.ClaudeFableAPIKey != "" && claudeFableModel(bodyModel(body)) {
		resp, err := s.claudeFableAPIKeyResponse(r, body)
		if err != nil {
			if s.Logger != nil && r.Context().Err() == nil {
				s.Logger.Error("claude-fable api-key request failed", "error", err)
			}
			return nil, false
		}
		return resp, true
	}
	return nil, false
}

// claudeFableBedrockUnusable reports whether a Bedrock response should push the
// request to the next fallback stage. Any non-2xx counts: 429 is consumed TPM
// quota, 5xx (e.g. 503 "Bedrock is unable to process your request") is a
// Bedrock-side failure, and 4xx covers auth/validation/model-access errors the
// Anthropic API may still accept. Credential-source rotation already happened
// before this status surfaced, and the API key is the chain's last stage, so a
// request that fails everywhere surfaces the API key's own error.
func claudeFableBedrockUnusable(status int) bool {
	return status < 200 || status >= 300
}

// claudeFableAPIKeyResponse forwards a Fable request to Anthropic using the
// dedicated Fable API key (x-api-key). It strips the context_management field
// that the Messages API rejects with "Extra inputs are not permitted", and drops
// the OAuth-only auth so the api key is authoritative. The response is native
// Anthropic (SSE or JSON), so it forwards with no transcoding.
func (s Server) claudeFableAPIKeyResponse(r *http.Request, body []byte) (*http.Response, error) {
	body = stripClaudeUnsupportedFields(body)

	upstream := s.ClaudeUpstream
	if upstream == nil {
		upstream = &url.URL{Scheme: "https", Host: "api.anthropic.com"}
	}
	target := *upstream
	target.Path = strings.TrimRight(upstream.Path, "/") + r.URL.Path
	target.RawQuery = r.URL.RawQuery

	outReq, err := http.NewRequestWithContext(r.Context(), http.MethodPost, target.String(), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	for key, values := range r.Header {
		lower := strings.ToLower(key)
		if isHopByHopHeader(key) || lower == "authorization" || lower == "x-api-key" || lower == "host" || lower == "content-length" {
			continue
		}
		for _, value := range values {
			outReq.Header.Add(key, value)
		}
	}
	// The handler-level fallback passes the client's unstripped request:
	// Subrouter control headers (session, user email, lease, retry policy)
	// and forwarding headers must not reach api.anthropic.com.
	session.StripSubrouterHeaders(outReq.Header)
	stripOutboundForwardingHeaders(outReq.Header)
	stripClientAcceptEncoding(outReq.Header)
	for key := range outReq.Header {
		if strings.HasPrefix(strings.ToLower(key), "x-subrouter-") {
			outReq.Header.Del(key)
		}
	}
	outReq.Header.Set("X-Api-Key", s.ClaudeFableAPIKey)
	removeCommaHeaderValue(outReq.Header, "Anthropic-Beta", claudeOAuthBetaHeader)
	if outReq.Header.Get("Anthropic-Version") == "" {
		outReq.Header.Set("Anthropic-Version", "2023-06-01")
	}
	outReq.Host = target.Host
	outReq.ContentLength = int64(len(body))

	transport := s.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	return transport.RoundTrip(outReq)
}

// stripClaudeUnsupportedFields removes request fields the Anthropic Messages API
// rejects for direct (non-OAuth) calls, notably context_management, which Claude
// Code sends but the endpoint refuses with "Extra inputs are not permitted".
func stripClaudeUnsupportedFields(body []byte) []byte {
	var payload map[string]json.RawMessage
	if json.Unmarshal(body, &payload) != nil {
		return body
	}
	changed := false
	for _, field := range []string{"context_management"} {
		if _, ok := payload[field]; ok {
			delete(payload, field)
			changed = true
		}
	}
	if !changed {
		return body
	}
	if rebuilt, err := json.Marshal(payload); err == nil {
		return rebuilt
	}
	return body
}

// stripBedrockUnsupportedFields removes what Anthropic-mode Claude Code sends
// and Bedrock's schema refuses with a 400 for the whole request. The direct
// Messages API accepts these, so only the Bedrock stage strips them.
func stripBedrockUnsupportedFields(body []byte) []byte {
	var payload map[string]json.RawMessage
	if json.Unmarshal(body, &payload) != nil {
		return body
	}
	changed := false
	// Claude Code in Anthropic mode sends these; Bedrock's schema has none of
	// them and refuses the whole request.
	for _, field := range []string{"output_config", "safeguards"} {
		if _, ok := payload[field]; ok {
			delete(payload, field)
			changed = true
		}
	}
	// Claude Code also puts output_config on messages; Bedrock refuses it
	// there with "messages.N.output_config: Extra inputs are not permitted".
	if raw, ok := payload["messages"]; ok {
		var messages []map[string]json.RawMessage
		if json.Unmarshal(raw, &messages) == nil {
			stripped := false
			kept := messages[:0]
			for _, message := range messages {
				if _, ok := message["output_config"]; ok {
					delete(message, "output_config")
					stripped = true
				}
				content, contentChanged, keep := bedrockMessageContent(message)
				if !keep {
					stripped = true
					continue
				}
				if contentChanged {
					message["content"] = content
					stripped = true
				}
				kept = append(kept, message)
			}
			messages = kept
			if stripped {
				if rebuilt, err := json.Marshal(messages); err == nil {
					payload["messages"] = rebuilt
					changed = true
				}
			}
		}
	}
	// Bedrock takes thinking.display only as "summarized" or "omitted".
	if raw, ok := payload["thinking"]; ok {
		var thinking map[string]json.RawMessage
		if json.Unmarshal(raw, &thinking) == nil {
			if display, ok := thinking["display"]; ok {
				var value string
				_ = json.Unmarshal(display, &value)
				if value != "summarized" && value != "omitted" {
					delete(thinking, "display")
					if rebuilt, err := json.Marshal(thinking); err == nil {
						payload["thinking"] = rebuilt
						changed = true
					}
				}
			}
		}
	}
	if !changed {
		return body
	}
	if rebuilt, err := json.Marshal(payload); err == nil {
		return rebuilt
	}
	return body
}

// bedrockMessageContent rewrites one message's content into a shape Bedrock
// accepts. Anthropic takes a mid-conversation system message with string
// content and "tool_addition" blocks; Bedrock refuses both ("system content
// must contain at least one block", "Input tag 'tool_addition' ... does not
// match"). keep is false for a system message with nothing left in it.
func bedrockMessageContent(message map[string]json.RawMessage) (json.RawMessage, bool, bool) {
	raw, ok := message["content"]
	if !ok {
		return nil, false, true
	}
	var role string
	_ = json.Unmarshal(message["role"], &role)
	var text string
	if json.Unmarshal(raw, &text) == nil {
		if role != "system" {
			return nil, false, true
		}
		if strings.TrimSpace(text) == "" {
			return nil, false, false
		}
		blocks, err := json.Marshal([]map[string]string{{"type": "text", "text": text}})
		if err != nil {
			return nil, false, true
		}
		return blocks, true, true
	}
	var blocks []map[string]json.RawMessage
	if json.Unmarshal(raw, &blocks) != nil {
		return nil, false, true
	}
	changed := false
	out := blocks[:0]
	for _, block := range blocks {
		var blockType string
		_ = json.Unmarshal(block["type"], &blockType)
		if blockType == "tool_addition" {
			changed = true
			continue
		}
		out = append(out, block)
	}
	if len(out) == 0 {
		if role == "system" {
			return nil, false, false
		}
		if !changed {
			return nil, false, true
		}
		out = append(out, map[string]json.RawMessage{
			"type": json.RawMessage(`"text"`),
			"text": json.RawMessage(`"(tool list updated)"`),
		})
	}
	if !changed {
		return nil, false, true
	}
	rebuilt, err := json.Marshal(out)
	if err != nil {
		return nil, false, true
	}
	return rebuilt, true, true
}

// bedrockMessageShape lists each message's role and content block types.
func bedrockMessageShape(body []byte) string {
	var payload struct {
		System   json.RawMessage `json:"system"`
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if json.Unmarshal(body, &payload) != nil {
		return "unparsed"
	}
	var b strings.Builder
	b.WriteString("system=" + rawKind(payload.System))
	for i, message := range payload.Messages {
		fmt.Fprintf(&b, " %d:%s=%s", i, message.Role, rawKind(message.Content))
		if b.Len() > 3000 {
			b.WriteString(" ...")
			break
		}
	}
	return b.String()
}

func rawKind(raw json.RawMessage) string {
	var blocks []struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(raw, &blocks) == nil {
		types := make([]string, len(blocks))
		for i, block := range blocks {
			types[i] = block.Type
		}
		return "[" + strings.Join(types, ",") + "]"
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return fmt.Sprintf("string(%d)", len(text))
	}
	if len(raw) == 0 {
		return "none"
	}
	return "other"
}

// bodyModel is the model a Messages request body names.
func bodyModel(body []byte) string {
	var payload struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(body, &payload)
	return payload.Model
}
