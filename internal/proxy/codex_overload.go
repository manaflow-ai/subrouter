package proxy

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
)

// CodexOverloadFailoverConfig retries a Codex request on another account when
// the pool answers a capacity failure. Measured 2026-09-09/10 across five
// regions and four hours: whether gpt-6-astra "fast" returns
// server_is_overloaded is a stable property of the ACCOUNT (some accounts
// never fail, most fail about half the time under load), not of the region
// or the hour. So the retry that actually clears the error is the same
// request on a different account, which is the same shape as the existing
// usage-limit failover.
//
// Disabled, nothing here runs.
type CodexOverloadFailoverConfig struct {
	Enabled bool
	// MaxAccounts bounds how many further accounts one request may try.
	MaxAccounts int
	// MarkTTL is how long an overloaded account stays out of routing for the
	// model pool that failed. Short: overload is a probability, not a state.
	MarkTTL time.Duration
}

const (
	codexOverloadDefaultMaxAccounts = 3
	codexOverloadDefaultMarkTTL     = 2 * time.Minute
	// codexOverloadMaxWebSocketReroutes bounds 1012 reconnect storms for one
	// session; past it the websocket path falls through to the egress and
	// Azure diverts.
	codexOverloadMaxWebSocketReroutes = 3
	codexOverloadRerouteWindow        = 10 * time.Minute
)

func (c *CodexOverloadFailoverConfig) enabled() bool {
	return c != nil && c.Enabled
}

func (c *CodexOverloadFailoverConfig) maxAccounts() int {
	if c == nil || c.MaxAccounts <= 0 {
		return codexOverloadDefaultMaxAccounts
	}
	return c.MaxAccounts
}

func (c *CodexOverloadFailoverConfig) markTTL() time.Duration {
	if c == nil || c.MarkTTL <= 0 {
		return codexOverloadDefaultMarkTTL
	}
	return c.MarkTTL
}

// codexOverloadFailure classifies a pool response as a capacity failure the
// account failover should act on: 408/5xx status, a 2xx SSE stream that opens
// with a server-class response.failed, or a body on any other status that
// names model capacity explicitly (codexCapacityBody). The returned response
// carries any peeked bytes stitched back in place. Quota, auth and client
// errors are not capacity failures and are returned untouched for the layers
// that own them.
func codexOverloadFailure(response *http.Response) (bool, string, *http.Response) {
	if response == nil {
		return false, "", response
	}
	if codexEgressPoolFailed(response.StatusCode) {
		return true, fmt.Sprintf("pool_status_%d", response.StatusCode), response
	}
	if codexSuccessStatus(response.StatusCode) && codexEventStream(response) {
		class, replaced := azureCodexStreamFailure(response)
		if class == codexFailureServer {
			return true, "pool_stream_failed", replaced
		}
		return false, "", replaced
	}
	capacity, replaced := codexCapacityBody(response)
	if capacity {
		return true, "pool_capacity_body", replaced
	}
	return false, "", replaced
}

func codexSuccessStatus(status int) bool {
	return status >= http.StatusOK && status < http.StatusMultipleChoices
}

// codexCapacityBodyPeekBytes bounds how much of a non-stream body the
// capacity check reads. Capacity errors are a few hundred bytes; a larger
// body is a real response and is left alone.
const codexCapacityBodyPeekBytes = 64 * 1024

// codexCapacityBody reports whether a response that is not a 2xx SSE stream
// carries a body naming model capacity: a 4xx JSON error, a non-2xx SSE error
// event, or a 2xx JSON error body. Codex renders all of them as "Selected
// model is at capacity", but the status alone (400, 429, 200) says nothing.
// Only an explicit capacity code or message counts; an unknown code on a
// non-5xx status is the request's business, not the pool's. 2xx SSE streams
// are the stream sniff's job and report false here. The body is stitched back
// either way.
func codexCapacityBody(response *http.Response) (bool, *http.Response) {
	if response == nil || response.Body == nil || response.Body == http.NoBody {
		return false, response
	}
	success := codexSuccessStatus(response.StatusCode)
	if codexEventStream(response) {
		if success {
			return false, response
		}
		class, capacity, replaced := codexStreamPeek(response)
		return class == codexFailureServer && capacity, replaced
	}
	switch {
	case response.StatusCode >= http.StatusBadRequest:
	case success && strings.Contains(strings.ToLower(response.Header.Get("Content-Type")), "json"):
	default:
		return false, response
	}
	rest := response.Body
	peeked, err := io.ReadAll(io.LimitReader(rest, codexCapacityBodyPeekBytes+1))
	var tail io.Reader = rest
	if err != nil {
		tail = errorReader{err: err}
	}
	response.Body = readCloser{Reader: io.MultiReader(bytes.NewReader(peeked), tail), Closer: rest}
	if err != nil || len(peeked) > codexCapacityBodyPeekBytes {
		return false, response
	}
	return codexCapacityFailureJSON(bytes.TrimSpace(peeked)), response
}

type errorReader struct{ err error }

func (r errorReader) Read([]byte) (int, error) { return 0, r.err }

// codexOverloadFailoverTransport replays a capacity-failed Codex request on
// the next best account. It sits above the pool retry stack and below the
// regional egress and Azure fallbacks: accounts first, because that is the
// lever the data supports; egress and Azure only when every tried account
// fails the same way.
type codexOverloadFailoverTransport struct {
	base      http.RoundTripper
	server    *Server
	agent     string
	session   string
	userEmail string
	account   string
	poolModel string
	budget    *attemptBudget
}

func (t codexOverloadFailoverTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	config := t.server.CodexOverloadFailover
	attemptReq := req
	accountID := t.account
	tried := map[string]struct{}{}
	if accountID != "" {
		tried[accountID] = struct{}{}
	}
	maxAccounts := config.maxAccounts()
	for switched := 0; ; switched++ {
		response, err := base.RoundTrip(attemptReq)
		if err != nil || req.GetBody == nil || req.Context().Err() != nil {
			return response, err
		}
		failed, reason, response := codexOverloadFailure(response)
		if !failed {
			if switched > 0 && response.StatusCode >= http.StatusOK &&
				response.StatusCode < http.StatusMultipleChoices && t.server.Sessions != nil {
				// The candidate picker no longer commits the session on
				// main; pin the conversation to the account that served it
				// so the next turn does not start on the one that failed.
				if _, err := t.server.commitSessionReassignment(t.agent, t.session, t.account, accountID, t.userEmail); err != nil && t.server.Logger != nil {
					t.server.Logger.Warn("codex overload failover could not commit session reassignment", "session", t.session, "account", accountID, "error", err)
				}
			}
			return response, nil
		}
		t.server.markAccountOverloaded(accountID, t.poolModel, config.markTTL())
		if switched >= maxAccounts {
			t.logOverload("codex overload failover exhausted", accountID, reason, switched, "max_accounts")
			return response, nil
		}
		if !t.budget.consume() {
			t.logOverload("codex overload failover exhausted", accountID, reason, switched, "retry_budget")
			return response, nil
		}
		next, pickErr := t.server.oauthRetryCandidate(req.Context(), accounts.ProviderCodex, t.agent, t.session, t.userEmail, t.poolModel, tried, false, false)
		if pickErr != nil {
			t.logOverload("codex overload failover has no alternate account", accountID, reason, switched, pickErr.Error())
			return response, nil
		}
		body, bodyErr := req.GetBody()
		if bodyErr != nil {
			return response, nil
		}
		if response.Body != nil {
			_ = response.Body.Close()
		}
		previous := accountID
		accountID = next.ID
		tried[accountID] = struct{}{}
		if t.server.SchedulerRef != nil {
			t.server.SchedulerRef.NoteRouted(accounts.ProviderCodex, accountID)
		}
		attemptReq = req.Clone(req.Context())
		attemptReq.Body = body
		attemptReq.GetBody = req.GetBody
		attemptReq.ContentLength = req.ContentLength
		setAccountAuthHeaders(attemptReq.Header, next, t.poolModel)
		if t.server.Logger != nil {
			t.server.Logger.Warn("retrying codex request on another account after capacity failure",
				"agent", t.agent, "session", t.session, "reason", reason,
				"previous_account", previous, "account", accountID, "switch", switched+1, "max_accounts", maxAccounts)
		}
	}
}

func (t codexOverloadFailoverTransport) logOverload(message, accountID, reason string, switched int, why string) {
	if t.server == nil || t.server.Logger == nil {
		return
	}
	t.server.Logger.Warn(message, "agent", t.agent, "session", t.session, "account", accountID,
		"reason", reason, "switched", switched, "why", why)
}

// markAccountOverloaded takes the account out of routing for this model pool
// for a short window. Pool-scoped: the same account keeps serving other
// models, and a capacity failure says nothing about its quota.
func (s *Server) markAccountOverloaded(accountID, poolModel string, ttl time.Duration) {
	if s == nil || s.SchedulerRef == nil || accountID == "" {
		return
	}
	s.SchedulerRef.MarkExhaustedUntil(accounts.ProviderCodex, accountID, poolModel, time.Now().Add(ttl))
	if s.Logger != nil {
		s.Logger.Warn("codex account marked overloaded for this model pool", "account", accountID, "pool", poolModel, "ttl", ttl.String())
	}
}

// codexOverloadReroutes counts websocket 1012 reroutes per session so one
// session cannot bounce forever between overloaded accounts.
type codexOverloadReroutes struct {
	mu      sync.Mutex
	entries map[string]codexOverloadRerouteEntry
}

type codexOverloadRerouteEntry struct {
	count     int
	expiresAt time.Time
}

func newCodexOverloadReroutes() *codexOverloadReroutes {
	return &codexOverloadReroutes{entries: map[string]codexOverloadRerouteEntry{}}
}

// allow records one reroute and reports whether it is within budget.
func (r *codexOverloadReroutes) allow(key string, limit int) bool {
	if r == nil || key == "" {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	for k, entry := range r.entries {
		if !entry.expiresAt.After(now) {
			delete(r.entries, k)
		}
	}
	entry := r.entries[key]
	if entry.count >= limit {
		return false
	}
	entry.count++
	entry.expiresAt = now.Add(codexOverloadRerouteWindow)
	r.entries[key] = entry
	return true
}

// codexOverloadWebSocketReroute marks the account and reports whether the
// websocket turn should be closed 1012 so the reconnect lands on another
// account. False once the session has used its reroute budget.
func (s Server) codexOverloadWebSocketReroute(agentType, sessionID, accountID, poolModel string) bool {
	if !s.CodexOverloadFailover.enabled() {
		return false
	}
	key := azureCodexSessionKeyFor(agentType, sessionID)
	if !s.codexOverloadRerouteCounts.allow(key, codexOverloadMaxWebSocketReroutes) {
		return false
	}
	s.markAccountOverloaded(accountID, poolModel, s.CodexOverloadFailover.markTTL())
	if s.Logger != nil {
		s.Logger.Warn("codex websocket turn hit a capacity error; rerouting session to another account",
			"agent", agentType, "session", sessionID, "account", accountID)
	}
	return true
}
