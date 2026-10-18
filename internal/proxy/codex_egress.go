package proxy

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
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
	// MaxAttempts caps how many egresses one request tries before handing
	// the pool's failure back to the layers above. With a node in every
	// region, trying them all in series would hold a turn for a minute when
	// the capacity shortage is global. 0 means DefaultCodexEgressMaxAttempts.
	MaxAttempts int
}

// DefaultCodexEgressMaxAttempts is the per-request egress budget when
// CodexEgressConfig.MaxAttempts is unset.
const DefaultCodexEgressMaxAttempts = 4

func (c *CodexEgressConfig) maxAttempts() int {
	if c == nil || c.MaxAttempts <= 0 {
		return DefaultCodexEgressMaxAttempts
	}
	return c.MaxAttempts
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
		// net/http sends Proxy-Authorization from URL userinfo. Refuse to
		// put credentials on a cleartext proxy connection where they can be
		// observed by any host on the path.
		if parsed.User != nil && parsed.Scheme == "http" {
			return nil, fmt.Errorf("codex egress proxy %q: credentials require an https proxy", field)
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
		// Egress replays reach chatgpt.com too, whose streams carry no
		// Content-Type.
		transports = append(transports, sniffContentTypeTransport{base: transport})
	}
	return transports
}

// codexEgressPoolFailed reports whether a pool status is the provider's own
// fault. Auth and quota statuses are deliberately excluded: 401/402/403 and
// 429 follow the account, not the region, and moving them elsewhere only
// hides the real cause from the layers that mark accounts exhausted.
func codexEgressPoolFailed(status int) bool {
	return status == http.StatusRequestTimeout || (status >= 500 && status < 600)
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
	// attempt is the request's shared state; see replayablePostRetryTransport.
	attempt *upstreamAttempt
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
	if t.attempt == nil {
		t.attempt = standaloneUpstreamAttempt(req, t.server, accounts.Account{}, nil)
	}
	// An egress replay is this layer's input through another region: the same
	// account, whatever the layers below switched to for the pool attempt.
	addressed := t.attempt.current()
	// A pinned session skips the pool that already failed it. Should every
	// egress fail as well, the pool gets one more chance below. Both passes
	// share one MaxAttempts budget, so a pinned session in a global shortage
	// does not get twice the replays.
	budget := t.server.CodexEgress.maxAttempts()
	if pinned, found := t.server.codexEgressSessions.lookup(t.sessionKey); found {
		response, err, served, used := t.tryEgress(req, addressed, pinned, true, budget, "pinned")
		budget -= used
		if served && !codexEgressAccountFailure(response) {
			return response, err
		}
		// A quota or credential failure follows the account, not the region.
		// Drop the pin and let the full stack below (account failover, then
		// Azure) handle it, instead of handing the client a 429 the pool's
		// own layers would have rerouted.
		if response != nil && response.Body != nil {
			_ = response.Body.Close()
		}
		t.server.codexEgressSessions.unpin(t.sessionKey)
		t.server.logCodexEgress("codex egress pin dropped after account-level failure", "pinned", "", statusOf(response), err)
	}
	response, err := t.attempt.send(base, req, addressed)
	if req.Context().Err() != nil {
		return response, err
	}
	reason := "transport_error"
	if err == nil {
		// Anything but a capacity failure (408/5xx, a failed stream, or a
		// body naming capacity on any status) is the request's own fault, or
		// auth/quota, which the layers above own.
		failed, why, replaced := codexOverloadFailure(response)
		response = replaced
		if !failed {
			return response, nil
		}
		reason = why
	}
	start := azureCodexEndpointIndex(t.sessionKey, len(transports))
	fallback, fallbackErr, served, _ := t.tryEgress(req, addressed, start, false, budget, reason)
	if !served {
		if retry, ok := t.server.codexFallbackRetryRequest(req, t.attempt, response); ok && !t.server.AzureCodex.configured() {
			if response != nil && response.Body != nil {
				_ = response.Body.Close()
			}
			return base.RoundTrip(retry)
		}
		return response, err
	}
	if t.server.recoveryCounters != nil {
		t.server.recoveryCounters.add(accounts.ProviderCodex, "handoff_503", time.Now())
	}
	if response != nil && response.Body != nil {
		_ = response.Body.Close()
	}
	return fallback, fallbackErr
}

// tryEgress replays the request through up to `budget` egresses, healthy
// ones first, walking the ring from `start` (first regardless when
// keepStart). It reports served=true with the first response that is not a
// pool failure, pinning the session to that egress. A response that fails
// the same way is closed, the egress is marked cooling, and the next is tried.
// used counts the replays sent.
func (t codexEgressFallbackTransport) tryEgress(req *http.Request, addressed accounts.Account, start int, keepStart bool, budget int, reason string) (*http.Response, error, bool, int) {
	transports := t.server.codexEgressTransports
	count := len(transports)
	if count == 0 || budget <= 0 {
		return nil, nil, false, 0
	}
	health := t.server.codexRegions
	model := t.attempt.poolModel
	failedAt := func(index int, status int, why error) {
		health.note(model, index+1, true)
		t.attempt.noteCodexRegionResult(index+1, true)
		event := CodexCapacityEvent{
			Transport: "http", Region: health.name(index + 1), Account: addressed.ID, Model: model,
			Reason: reason, Agent: t.attempt.agent, Session: t.attempt.session, Outcome: codexCapacityOutcomeAttempt,
		}
		if why != nil {
			event.Reason = "egress_transport_error"
		} else if status != 0 {
			event.Reason = fmt.Sprintf("status_%d", status)
		}
		t.server.recordCodexCapacityEvent(event)
	}
	used := 0
	var lastResponse *http.Response
	var lastErr error
	for _, index := range health.egressOrder(model, count, start, budget, keepStart) {
		if req.Context().Err() != nil {
			break
		}
		egress := t.server.CodexEgress.Proxies[index].Host
		replay, replayErr := t.attempt.replay(req, nil)
		if replayErr != nil {
			break
		}
		// The egress transports replace the wrapped base, so they need their
		// own in-flight wrapper to count this physical attempt.
		response, err := t.attempt.send(inflightAttemptTransport{base: transports[index], attempt: t.attempt}, replay, addressed)
		used++
		if lastResponse != nil && lastResponse.Body != nil {
			_ = lastResponse.Body.Close()
		}
		lastResponse, lastErr = response, err
		if err != nil {
			// A client that went away is not the region's fault, and an
			// account paused by admission is the account's.
			if req.Context().Err() == nil && !errors.Is(err, errAdmissionPaused) {
				failedAt(index, 0, err)
			}
			t.server.logCodexEgress("codex egress attempt failed", reason, egress, 0, err)
			continue
		}
		if codexEgressPoolFailed(response.StatusCode) {
			failedAt(index, response.StatusCode, nil)
			t.server.logCodexEgress("codex egress attempt failed", reason, egress, response.StatusCode, nil)
			continue
		}
		if response.StatusCode >= 200 && response.StatusCode < 300 {
			class, replaced := azureCodexStreamFailure(response)
			lastResponse = replaced
			if class == codexFailureServer {
				failedAt(index, 0, nil)
				t.server.logCodexEgress("codex egress attempt failed", reason, egress, response.StatusCode, nil)
				continue
			}
			if class == codexFailureQuota {
				// The account is out, not the region. Hand it back as a
				// 429-shaped account failure so the caller drops the pin
				// and the usage-limit layers reroute it.
				lastResponse = codexEgressQuotaAsStatus(replaced)
				return lastResponse, nil, true, used
			}
		}
		if capacity, replaced := codexCapacityBody(lastResponse); capacity {
			lastResponse = replaced
			failedAt(index, 0, nil)
			t.server.logCodexEgress("codex egress attempt failed", reason, egress, response.StatusCode, nil)
			continue
		}
		// Only pin a successful response. Auth, quota, redirects, and other
		// non-2xx results belong to the account/pool layers above this one.
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			return response, nil, true, used
		}
		health.note(model, index+1, false)
		t.attempt.noteCodexRegionResult(index+1, false)
		t.server.codexEgressSessions.pin(t.sessionKey, index)
		t.server.logCodexEgress("serving codex via regional egress", reason, egress, lastResponse.StatusCode, nil)
		return lastResponse, nil, true, used
	}
	if lastResponse != nil && lastResponse.Body != nil {
		_ = lastResponse.Body.Close()
	}
	_ = lastErr
	return nil, nil, false, used
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
func (s Server) codexEgressWebSocketDivert(agentType, sessionID, model string) bool {
	if !s.CodexEgress.configured() {
		return false
	}
	key := azureCodexSessionKeyFor(agentType, sessionID)
	if key == "" {
		return false
	}
	count := len(s.CodexEgress.Proxies)
	index := azureCodexEndpointIndex(key, count)
	if order := s.codexRegions.egressOrder(model, count, index, 1, false); len(order) == 1 {
		index = order[0]
	}
	s.codexEgressSessions.pin(key, index)
	if s.Logger != nil {
		s.Logger.Warn("codex websocket turn hit a capacity error; pinning session to a regional egress",
			"agent", agentType, "session", sessionID)
	}
	return true
}

// codexEgressAccountFailure reports whether a response served through an
// egress is an account-level refusal (quota, auth) that the pool's own
// failover layers must see. 429 usage limits arrive both as a status and as
// an in-stream usage_limit_reached event.
func codexEgressAccountFailure(response *http.Response) bool {
	if response == nil {
		return false
	}
	switch response.StatusCode {
	case http.StatusTooManyRequests, http.StatusUnauthorized, http.StatusForbidden, http.StatusPaymentRequired:
		return true
	}
	return false
}

func statusOf(response *http.Response) int {
	if response == nil {
		return 0
	}
	return response.StatusCode
}

// codexEgressQuotaAsStatus rewraps a 2xx stream that opened with a usage
// limit as a 429 carrying the same body, which is the shape the account
// failover layers already understand.
func codexEgressQuotaAsStatus(response *http.Response) *http.Response {
	if response == nil {
		return nil
	}
	response.StatusCode = http.StatusTooManyRequests
	response.Status = "429 Too Many Requests"
	return response
}
