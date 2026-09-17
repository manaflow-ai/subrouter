package proxy

import (
	"fmt"
	"net/http"
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
// account failover should act on: 408/5xx status, or a 2xx SSE stream that
// opens with a server-class response.failed. The returned response carries
// any peeked bytes stitched back in place. Quota, auth and client errors are
// not capacity failures and are returned untouched for the layers that own them.
func codexOverloadFailure(response *http.Response) (bool, string, *http.Response) {
	if response == nil {
		return false, "", response
	}
	if codexEgressPoolFailed(response.StatusCode) {
		return true, fmt.Sprintf("pool_status_%d", response.StatusCode), response
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return false, "", response
	}
	class, reason, replaced := azureCodexStreamFailureDetail(response)
	if class == codexFailureServer {
		if reason == "" {
			reason = "stream_failed"
		}
		return true, reason, replaced
	}
	return false, "", replaced
}

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
			if response.StatusCode >= 200 && response.StatusCode < 300 {
				t.server.noteCodexCapacitySuccess(accountID, azureCodexSessionKeyFor(t.agent, t.session))
			}
			return response, nil
		}
		t.server.noteCodexCapacityFailure(accountID, azureCodexSessionKeyFor(t.agent, t.session), t.poolModel, reason)
		if switched >= maxAccounts {
			t.logOverload("codex overload failover exhausted", accountID, reason, switched, "max_accounts")
			return response, nil
		}
		if !t.budget.consume() {
			t.logOverload("codex overload failover exhausted", accountID, reason, switched, "retry_budget")
			return response, nil
		}
		next, pickErr := t.server.oauthRetryAccount(req.Context(), accounts.ProviderCodex, t.agent, t.session, t.userEmail, t.poolModel, tried, false)
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
		setAccountAuthHeaders(attemptReq.Header, next)
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
	// lastAllowed is when this session last rerouted; it paces the
	// over-budget reroutes off a persistently constrained account.
	lastAllowed time.Time
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
	entry.lastAllowed = now
	r.entries[key] = entry
	return true
}

// allowPersistent lets a session past its reroute budget when the account it
// is bound to is persistently at capacity, paced to one reroute per
// codexCapacityPersistentRerouteInterval. The budget stops storms between
// accounts that fail at random; this keeps a session from being welded to an
// account that fails every time.
func (r *codexOverloadReroutes) allowPersistent(key string) bool {
	if r == nil || key == "" {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	entry := r.entries[key]
	if !entry.lastAllowed.IsZero() && now.Sub(entry.lastAllowed) < codexCapacityPersistentRerouteInterval {
		return false
	}
	entry.count++
	entry.expiresAt = now.Add(codexOverloadRerouteWindow)
	entry.lastAllowed = now
	r.entries[key] = entry
	return true
}

// codexOverloadWebSocketReroute records the failure, marks the account and
// reports whether the websocket turn should be closed 1012 so the reconnect
// lands on another account. False once the session has used its reroute
// budget, unless the account is persistently at capacity, in which case the
// session may still leave it once per codexCapacityPersistentRerouteInterval.
func (s Server) codexOverloadWebSocketReroute(agentType, sessionID, accountID, poolModel, reason string) bool {
	if !s.CodexOverloadFailover.enabled() {
		return false
	}
	key := azureCodexSessionKeyFor(agentType, sessionID)
	verdict := s.noteCodexCapacityFailure(accountID, key, poolModel, reason)
	if !s.codexOverloadRerouteCounts.allow(key, codexOverloadMaxWebSocketReroutes) {
		if (!verdict.Persistent && !verdict.Tainted && !s.codexCapacity.tainted(accountID)) ||
			!s.codexOverloadRerouteCounts.allowPersistent(key) {
			return false
		}
	}
	if s.Logger != nil {
		s.Logger.Warn("codex websocket turn hit a capacity error; rerouting session to another account",
			"agent", agentType, "session", sessionID, "account", accountID, "reason", reason,
			"streak", verdict.Streak, "persistent", verdict.Persistent, "retry", verdict.Retry)
	}
	return true
}
