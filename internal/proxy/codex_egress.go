package proxy

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// CodexEgressConfig routes a Codex Responses request out of a different
// region when the pool answers a capacity failure. OpenAI serves each model
// and tier from regional pools, and a US pool that returns
// server_is_overloaded often has a European or Asian sibling with headroom
// for the very same account at the very same second. Each proxy is an HTTP
// CONNECT proxy reachable from the router (a tag:egress node on the tailnet).
//
// Unset, nothing here runs and the request path is byte-for-byte the old one.
type CodexEgressConfig struct {
	Proxies []*url.URL
	// PinStorePath persists which sessions are pinned to an egress so a
	// worker upgrade does not bounce every live conversation back through the
	// pool that just failed it.
	PinStorePath string
}

func (c *CodexEgressConfig) configured() bool {
	return c != nil && len(c.Proxies) > 0
}

// names lists the proxy hosts for health and logs. Never the full URL, which
// may carry credentials.
func (c *CodexEgressConfig) names() []string {
	if c == nil {
		return nil
	}
	names := make([]string, 0, len(c.Proxies))
	for _, proxy := range c.Proxies {
		names = append(names, proxy.Host)
	}
	return names
}

// ParseCodexEgressProxies parses a comma-separated list of proxy URLs.
func ParseCodexEgressProxies(raw string) ([]*url.URL, error) {
	var proxies []*url.URL
	for _, field := range strings.Split(raw, ",") {
		field = strings.TrimSpace(field)
		if field == "" {
			continue
		}
		parsed, err := url.Parse(field)
		if err != nil {
			return nil, fmt.Errorf("codex egress proxy %q: %w", field, err)
		}
		if parsed.Scheme != "http" && parsed.Scheme != "https" {
			return nil, fmt.Errorf("codex egress proxy %q: scheme must be http or https", field)
		}
		if parsed.Host == "" {
			return nil, fmt.Errorf("codex egress proxy %q: missing host", field)
		}
		proxies = append(proxies, parsed)
	}
	return proxies, nil
}

// codexEgressTransports builds one outbound transport per proxy. Built once
// at Handler() time so every request shares the pooled connections.
func codexEgressTransports(config *CodexEgressConfig) []http.RoundTripper {
	if !config.configured() {
		return nil
	}
	transports := make([]http.RoundTripper, 0, len(config.Proxies))
	for _, proxy := range config.Proxies {
		transport := NewOutboundTransport()
		transport.Proxy = http.ProxyURL(proxy)
		transports = append(transports, transport)
	}
	return transports
}

// codexEgressPoolFailed reports whether a pool status is the provider's own
// fault. Auth and quota statuses are deliberately excluded: 401/402/403 and
// 429 follow the account, not the region, and moving them elsewhere only
// hides the real cause from the layers that mark accounts exhausted.
func codexEgressPoolFailed(status int) bool {
	return status == http.StatusRequestTimeout || status >= 500
}

// codexEgressFallbackTransport replays a Codex request through the next
// regional egress when the pool fails it. It sits directly under the Azure
// fallback in the transport stack: a regional retry is the same model on the
// same account, so it must be tried before paying a second provider.
type codexEgressFallbackTransport struct {
	base       http.RoundTripper
	server     *Server
	sessionKey string
	agent      string
	replayBody func() ([]byte, bool)
}

func (t codexEgressFallbackTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	transports := t.server.codexEgressTransports
	if len(transports) == 0 {
		return base.RoundTrip(req)
	}
	// A pinned session skips the pool that already failed it. Should every
	// egress fail as well, the pool gets one more chance below.
	if pinned, found := t.server.codexEgressSessions.lookup(t.sessionKey); found {
		response, err, served := t.tryEgress(req, pinned, "pinned")
		if served {
			return response, err
		}
		t.server.codexEgressSessions.unpin(t.sessionKey)
	}
	response, err := base.RoundTrip(req)
	if req.Context().Err() != nil {
		return response, err
	}
	reason := "transport_error"
	if err == nil {
		if !codexEgressPoolFailed(response.StatusCode) {
			if response.StatusCode < 200 || response.StatusCode >= 300 {
				// 4xx: the request's own fault, or auth/quota, which the
				// layers above own.
				return response, nil
			}
			class, replaced := azureCodexStreamFailure(response)
			response = replaced
			if class != codexFailureServer {
				return response, nil
			}
			reason = "pool_stream_failed"
		} else {
			reason = fmt.Sprintf("pool_status_%d", response.StatusCode)
		}
	}
	start := azureCodexEndpointIndex(t.sessionKey, len(transports))
	fallback, fallbackErr, served := t.tryEgress(req, start, reason)
	if !served {
		return response, err
	}
	if response != nil && response.Body != nil {
		_ = response.Body.Close()
	}
	return fallback, fallbackErr
}

// tryEgress replays the request through each egress in turn, starting at
// `start`. It reports served=true with the first response that is not a
// pool failure, pinning the session to that egress. A response that fails
// the same way is closed and the next egress is tried.
func (t codexEgressFallbackTransport) tryEgress(req *http.Request, start int, reason string) (*http.Response, error, bool) {
	transports := t.server.codexEgressTransports
	count := len(transports)
	if count == 0 {
		return nil, nil, false
	}
	body, ok := t.replayBody()
	if !ok {
		return nil, nil, false
	}
	var lastResponse *http.Response
	var lastErr error
	for attempt := range count {
		if req.Context().Err() != nil {
			break
		}
		index := (start + attempt) % count
		egress := t.server.CodexEgress.Proxies[index].Host
		replay := req.Clone(req.Context())
		replay.Body = io.NopCloser(bytes.NewReader(body))
		replay.ContentLength = int64(len(body))
		replay.GetBody = func() (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader(body)), nil
		}
		response, err := transports[index].RoundTrip(replay)
		if lastResponse != nil && lastResponse.Body != nil {
			_ = lastResponse.Body.Close()
		}
		lastResponse, lastErr = response, err
		if err != nil {
			t.server.logCodexEgress("codex egress attempt failed", reason, egress, 0, err)
			continue
		}
		if codexEgressPoolFailed(response.StatusCode) {
			t.server.logCodexEgress("codex egress attempt failed", reason, egress, response.StatusCode, nil)
			continue
		}
		if response.StatusCode >= 200 && response.StatusCode < 300 {
			class, replaced := azureCodexStreamFailure(response)
			lastResponse = replaced
			if class == codexFailureServer {
				t.server.logCodexEgress("codex egress attempt failed", reason, egress, response.StatusCode, nil)
				continue
			}
		}
		t.server.codexEgressSessions.pin(t.sessionKey, index)
		t.server.logCodexEgress("serving codex via regional egress", reason, egress, lastResponse.StatusCode, nil)
		return lastResponse, nil, true
	}
	if lastResponse != nil && lastResponse.Body != nil {
		_ = lastResponse.Body.Close()
	}
	_ = lastErr
	return nil, nil, false
}

func (s *Server) logCodexEgress(message, reason, egress string, status int, err error) {
	if s == nil || s.Logger == nil {
		return
	}
	attrs := []any{"reason", reason, "egress", egress}
	if status != 0 {
		attrs = append(attrs, "status", status)
	}
	if err != nil {
		attrs = append(attrs, "error", err.Error())
	}
	s.Logger.Warn(message, attrs...)
}

// codexEgressWebSocketDivert pins a Codex websocket session to an egress when
// an upstream capacity error would otherwise end the turn. Same mechanism as
// the Azure divert: the relay cannot re-route a live socket, so the pin plus a
// 1012 close hands the session back to the client, whose reconnect is refused
// with 426 and lands on the HTTP transport, where the pin serves it through
// the egress.
func (s Server) codexEgressWebSocketDivert(agentType, sessionID string) bool {
	if !s.CodexEgress.configured() {
		return false
	}
	key := azureCodexSessionKeyFor(agentType, sessionID)
	if key == "" {
		return false
	}
	s.codexEgressSessions.pin(key, azureCodexEndpointIndex(key, len(s.CodexEgress.Proxies)))
	if s.Logger != nil {
		s.Logger.Warn("codex websocket turn hit a capacity error; pinning session to a regional egress",
			"agent", agentType, "session", sessionID)
	}
	return true
}
