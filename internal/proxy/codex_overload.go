package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"strconv"
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
	// RetryBudget bounds the default capacity retry policy (same-account
	// retry plus account failover) in wall time. Zero means 10s.
	RetryBudget time.Duration
	// CapacityRetryPersist turns on persist mode for every request
	// (SUBROUTER_CODEX_CAPACITY_RETRY=persist); a request header can still
	// opt out, or in when this is off.
	CapacityRetryPersist bool
	// CapacityRetryBudget is the persist-mode budget. Zero means 2m.
	CapacityRetryBudget time.Duration

	// Test seams for the jittered gaps; nil uses the real jitter.
	sameAccountGap func() time.Duration
	switchGap      func() time.Duration
	persistGap     func() time.Duration
}

const (
	codexOverloadDefaultMaxAccounts = 3
	codexOverloadDefaultMarkTTL     = 2 * time.Minute
	// codexOverloadMaxWebSocketReroutes bounds 1012 reconnect storms for one
	// session; past it the websocket path falls through to the egress and
	// Azure diverts.
	codexOverloadMaxWebSocketReroutes = 3
	// codexOverloadMaxPersistWebSocketReroutes is the same bound for a
	// session that asked to persist through capacity failures.
	codexOverloadMaxPersistWebSocketReroutes = 20
	codexOverloadRerouteWindow               = 10 * time.Minute
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
		if !codexEventStream(response) {
			// The status decides; the peek only keeps the body's retry
			// hints (retry_after_ms, resets_in_seconds) for the mark TTL.
			_, response = codexCapacityBody(response)
		}
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
	if err != nil || len(peeked) > codexCapacityBodyPeekBytes {
		response.Body = readCloser{Reader: io.MultiReader(bytes.NewReader(peeked), tail), Closer: rest}
		return false, response
	}
	payload := bytes.TrimSpace(peeked)
	class, capacity := codexTurnFailure(payload)
	response.Body = &codexPeekedBody{
		Reader:   io.MultiReader(bytes.NewReader(peeked), tail),
		Closer:   rest,
		class:    class,
		capacity: capacity,
		payload:  payload,
	}
	return class == codexFailureServer && capacity, response
}

type errorReader struct{ err error }

func (r errorReader) Read([]byte) (int, error) { return 0, r.err }

const (
	codexCapacityMarkMinTTL = 30 * time.Second
	codexCapacityMarkMaxTTL = 5 * time.Minute
	// codexCapacityMarkJitter spreads the moment a crowd of marked accounts
	// re-enters routing, so they do not all hit the pool again together.
	codexCapacityMarkJitter = 0.2
)

// codexCapacityRetryHint returns how long the upstream asked callers to stay
// away, or zero: Retry-After-Ms or Retry-After headers first, then the
// failure payload the classifier kept (retry_after_ms, retry_after,
// resets_in_seconds at any level). It never reads the body.
func codexCapacityRetryHint(response *http.Response) time.Duration {
	if response == nil {
		return 0
	}
	if raw := strings.TrimSpace(response.Header.Get("Retry-After-Ms")); raw != "" {
		if ms, err := strconv.ParseFloat(raw, 64); err == nil && ms > 0 {
			return time.Duration(ms * float64(time.Millisecond))
		}
	}
	now := time.Now()
	if until := parseRetryAfter(strings.TrimSpace(response.Header.Get("Retry-After")), now); !until.IsZero() {
		return until.Sub(now)
	}
	if peeked, ok := response.Body.(*codexPeekedBody); ok {
		return codexRetryHintJSON(peeked.payload)
	}
	return 0
}

// codexRetryHintJSON digs a retry hint out of a failure payload: top level,
// error, response or response.error.
func codexRetryHintJSON(payload []byte) time.Duration {
	if len(payload) == 0 {
		return 0
	}
	var event map[string]any
	if json.Unmarshal(payload, &event) != nil {
		return 0
	}
	return codexRetryHintMap(event, 0)
}

func codexRetryHintMap(event map[string]any, depth int) time.Duration {
	if event == nil || depth > 3 {
		return 0
	}
	if ms, ok := numberField(event, "retry_after_ms"); ok && ms > 0 {
		return time.Duration(ms * float64(time.Millisecond))
	}
	for _, key := range []string{"retry_after", "resets_in_seconds"} {
		if seconds, ok := numberField(event, key); ok && seconds > 0 {
			return time.Duration(seconds * float64(time.Second))
		}
		if raw, ok := event[key].(string); ok {
			if seconds, err := strconv.ParseFloat(strings.TrimSuffix(strings.TrimSpace(raw), "s"), 64); err == nil && seconds > 0 {
				return time.Duration(seconds * float64(time.Second))
			}
		}
	}
	for _, key := range []string{"error", "response"} {
		if nested, ok := event[key].(map[string]any); ok {
			if hint := codexRetryHintMap(nested, depth+1); hint > 0 {
				return hint
			}
		}
	}
	return 0
}

// capacityMarkTTL is how long a capacity-failed account stays out of the
// pool: the upstream's hint clamped to [30s, 5m], else the configured TTL,
// either way jittered by ±20%.
func (c *CodexOverloadFailoverConfig) capacityMarkTTL(hint time.Duration) time.Duration {
	if hint <= 0 {
		return codexJitter(c.markTTL(), codexCapacityMarkJitter)
	}
	ttl := codexJitter(hint, codexCapacityMarkJitter)
	return min(max(ttl, codexCapacityMarkMinTTL), codexCapacityMarkMaxTTL)
}

func codexJitter(value time.Duration, fraction float64) time.Duration {
	return time.Duration(float64(value) * (1 - fraction + 2*fraction*rand.Float64()))
}

// codexOverloadSwitchDelay is the pause before retrying on the next account:
// 100-400ms, so a burst of failed requests does not stampede the accounts that
// are still healthy in the same instant the pool shed load.
func codexOverloadSwitchDelay() time.Duration {
	return 100*time.Millisecond + rand.N(300*time.Millisecond)
}

// codexSleepContext waits d, returning false if the request ended first.
func codexSleepContext(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
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
	// policy is the request's capacity retry policy (default or persist).
	policy codexCapacityRetryPolicy
}

// codexCapacityAttemptPlan is where and when the next capacity retry goes.
type codexCapacityAttemptPlan struct {
	// next is the account to switch to; nil retries on the current one.
	next  *accounts.Account
	gap   time.Duration
	phase string
}

func (t codexOverloadFailoverTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	config := t.server.CodexOverloadFailover
	ctx := req.Context()
	started := time.Now()
	deadline := started.Add(config.retryBudget())
	attemptReq := req
	accountID := t.account
	tried := map[string]struct{}{}
	if accountID != "" {
		tried[accountID] = struct{}{}
	}
	maxAccounts := config.maxAccounts()
	switched := 0
	sameAccountLeft := codexCapacitySameAccountRetries
	persisting := false
	var persistDeadline time.Time
	releasePersist := func() {}
	defer func() { releasePersist() }()
	for attempt := 1; ; attempt++ {
		response, err := base.RoundTrip(attemptReq)
		if err != nil || req.GetBody == nil || ctx.Err() != nil {
			return response, err
		}
		failed, reason, response := codexOverloadFailure(response)
		if !failed {
			if accountID != t.account && response.StatusCode >= http.StatusOK &&
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
		// Every failure classified here is pre-output: the stream peek
		// holds the response until the first visible output, so nothing has
		// reached the client yet and a replay cannot duplicate anything.
		hint := codexCapacityRetryHint(response)

		var plan codexCapacityAttemptPlan
		planned := false
		if !persisting {
			plan, planned = t.planDefaultRetry(ctx, accountID, reason, hint, tried, &sameAccountLeft, &switched, maxAccounts, deadline)
			if !planned && t.policy.persist {
				release, ok := t.server.codexPersistLoops.acquire(azureCodexSessionKeyFor(t.agent, t.session))
				if ok {
					releasePersist = release
					persisting = true
					persistDeadline = started.Add(t.policy.persistBudget)
				} else {
					t.logOverload("codex capacity persist retry skipped", accountID, reason, switched, "session_persist_loop_in_flight")
				}
			}
		}
		if persisting {
			plan, planned = t.planPersistRetry(ctx, accountID, hint, tried, persistDeadline)
			if !planned {
				t.logOverload("codex capacity persist retry exhausted", accountID, reason, switched, "persist_budget")
			}
		}
		if !planned {
			return response, nil
		}
		if !codexSleepContext(ctx, plan.gap) {
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
		if plan.next == nil {
			// Same account: keep its auth headers, fresh body.
			nextReq := attemptReq.Clone(ctx)
			nextReq.Body = body
			nextReq.GetBody = req.GetBody
			nextReq.ContentLength = req.ContentLength
			attemptReq = nextReq
		} else {
			accountID = plan.next.ID
			tried[accountID] = struct{}{}
			if t.server.SchedulerRef != nil {
				t.server.SchedulerRef.NoteRouted(accounts.ProviderCodex, accountID)
			}
			attemptReq = req.Clone(ctx)
			attemptReq.Body = body
			attemptReq.GetBody = req.GetBody
			attemptReq.ContentLength = req.ContentLength
			setAccountAuthHeaders(attemptReq.Header, *plan.next, t.poolModel)
		}
		if t.server.Logger != nil {
			t.server.Logger.Warn("retrying codex request after capacity failure",
				"agent", t.agent, "session", t.session, "reason", reason, "phase", plan.phase,
				"previous_account", previous, "account", accountID, "attempt", attempt+1,
				"gap", plan.gap.String(), "elapsed", time.Since(started).Round(time.Millisecond).String())
		}
	}
}

// planDefaultRetry is the default ladder: one quick retry on the same
// account, then the overload failover to other accounts, all inside the
// time budget and the request's shared attempt budget.
func (t codexOverloadFailoverTransport) planDefaultRetry(ctx context.Context, accountID, reason string, hint time.Duration, tried map[string]struct{}, sameAccountLeft, switched *int, maxAccounts int, deadline time.Time) (codexCapacityAttemptPlan, bool) {
	config := t.server.CodexOverloadFailover
	if *sameAccountLeft > 0 {
		*sameAccountLeft--
		gap := config.sameAccountDelay()
		if time.Now().Add(gap).After(deadline) {
			t.logOverload("codex capacity retry exhausted", accountID, reason, *switched, "time_budget")
			return codexCapacityAttemptPlan{}, false
		}
		if !t.budget.consume() {
			t.logOverload("codex capacity retry exhausted", accountID, reason, *switched, "retry_budget")
			return codexCapacityAttemptPlan{}, false
		}
		return codexCapacityAttemptPlan{gap: gap, phase: "same_account"}, true
	}
	// The same account shed the request again: take it out of this pool's
	// rotation for a while and move on.
	t.server.markAccountOverloaded(accountID, t.poolModel, config.capacityMarkTTL(hint))
	if *switched >= maxAccounts {
		t.logOverload("codex overload failover exhausted", accountID, reason, *switched, "max_accounts")
		return codexCapacityAttemptPlan{}, false
	}
	gap := config.switchDelay()
	if time.Now().Add(gap).After(deadline) {
		t.logOverload("codex overload failover exhausted", accountID, reason, *switched, "time_budget")
		return codexCapacityAttemptPlan{}, false
	}
	if !t.budget.consume() {
		t.logOverload("codex overload failover exhausted", accountID, reason, *switched, "retry_budget")
		return codexCapacityAttemptPlan{}, false
	}
	next, pickErr := t.server.oauthRetryCandidate(ctx, accounts.ProviderCodex, t.agent, t.session, t.userEmail, t.poolModel, tried, false, false)
	if pickErr != nil {
		t.logOverload("codex overload failover has no alternate account", accountID, reason, *switched, pickErr.Error())
		return codexCapacityAttemptPlan{}, false
	}
	*switched++
	return codexCapacityAttemptPlan{next: &next, gap: gap, phase: "switch_account"}, true
}

// planPersistRetry keeps going after the default ladder: the best untried
// account, or once every account has been tried, the best account again
// (shedding is a probability, so a retried account can pass). Gaps are
// 0.5-2s so a persisting client does not hammer the pool.
func (t codexOverloadFailoverTransport) planPersistRetry(ctx context.Context, accountID string, hint time.Duration, tried map[string]struct{}, deadline time.Time) (codexCapacityAttemptPlan, bool) {
	config := t.server.CodexOverloadFailover
	t.server.markAccountOverloaded(accountID, t.poolModel, config.capacityMarkTTL(hint))
	gap := config.persistDelay()
	if time.Now().Add(gap).After(deadline) {
		return codexCapacityAttemptPlan{}, false
	}
	next, pickErr := t.server.oauthRetryCandidate(ctx, accounts.ProviderCodex, t.agent, t.session, t.userEmail, t.poolModel, tried, false, false)
	if pickErr != nil && ctx.Err() == nil {
		for id := range tried {
			delete(tried, id)
		}
		next, pickErr = t.server.oauthRetryCandidate(ctx, accounts.ProviderCodex, t.agent, t.session, t.userEmail, t.poolModel, tried, false, false)
	}
	if pickErr != nil || next.ID == accountID {
		// Nothing else to pick: the same account again.
		return codexCapacityAttemptPlan{gap: gap, phase: "persist_same_account"}, true
	}
	return codexCapacityAttemptPlan{next: &next, gap: gap, phase: "persist"}, true
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
// account. False once the session has used its reroute budget: 3 per 10
// minutes by default, 20 for a session in persist mode, which also waits a
// jittered 0.5-2s before the close so its reconnects do not hammer the pool.
func (s Server) codexOverloadWebSocketReroute(ctx context.Context, agentType, sessionID, accountID, poolModel string, body []byte, persist bool) bool {
	if !s.CodexOverloadFailover.enabled() {
		return false
	}
	key := azureCodexSessionKeyFor(agentType, sessionID)
	limit := codexOverloadMaxWebSocketReroutes
	if persist {
		limit = codexOverloadMaxPersistWebSocketReroutes
	}
	if !s.codexOverloadRerouteCounts.allow(key, limit) {
		return false
	}
	s.markAccountOverloaded(accountID, poolModel, s.CodexOverloadFailover.capacityMarkTTL(codexRetryHintJSON(body)))
	if s.Logger != nil {
		s.Logger.Warn("codex websocket turn hit a capacity error; rerouting session to another account",
			"agent", agentType, "session", sessionID, "account", accountID, "persist", persist)
	}
	if persist {
		_ = codexSleepContext(ctx, s.CodexOverloadFailover.persistDelay())
	}
	return true
}
