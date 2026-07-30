package proxy

import (
	"bytes"
	"encoding/json"
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
			s.Logger.Warn("fable bedrock-primary request failed, falling through to pool", "error", err)
		}
		restore()
		return false
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
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
			if !claudeFableBedrockUnusable(resp.StatusCode) || s.ClaudeFableAPIKey == "" {
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
	if s.ClaudeFableAPIKey != "" {
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
