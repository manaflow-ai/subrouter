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

	"github.com/gorilla/websocket"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/selectacct"
	"github.com/manaflow-ai/subrouter/session"
)

// CodexOverloadFailoverConfig shapes the Codex capacity retry. Measured
// 2026-09-09/10 across five regions and four hours: whether gpt-6-astra
// "fast" returns server_is_overloaded is a stable property of the ACCOUNT
// (some accounts never fail, most fail about half the time under load), not
// of the region or the hour. So the retry most likely to clear the error is
// the same request on a different account. But the session's prompt cache
// lives on its account, and a switch re-bills the whole conversation, so the
// switch is opt-in.
//
// A nil or disabled config still retries capacity failures on the session's
// own account (codex_capacity_retry.go); Enabled adds the account failover,
// the capacity marks and the websocket reroute.
type CodexOverloadFailoverConfig struct {
	// Enabled turns on switching accounts on capacity failures
	// (SUBROUTER_CODEX_OVERLOAD_FAILOVER=1).
	Enabled bool
	// MaxAccounts bounds how many further accounts one request may try.
	MaxAccounts int
	// MarkTTL is how long an overloaded account stays out of routing for the
	// model pool that failed. Short: overload is a probability, not a state.
	MarkTTL time.Duration
	// RetryBudget overrides the default capacity retry policy's wall-time
	// budget, same-account or failover ladder alike. Zero means StayMaxWait
	// for the same-account ladder, 10s with the failover.
	RetryBudget time.Duration
	// StayInterval is the same-account ladder's steady gap after its ramp,
	// which is capped at it (SUBROUTER_CODEX_CAPACITY_RETRY_INTERVAL). Zero
	// means ~9s.
	StayInterval time.Duration
	// StayMaxWait caps the same-account ladder in wall-clock time from the
	// first attempt (SUBROUTER_CODEX_CAPACITY_RETRY_MAX_WAIT). Zero means 4m;
	// see StayUnbounded.
	StayMaxWait time.Duration
	// StayUnbounded removes that cap
	// (SUBROUTER_CODEX_CAPACITY_RETRY_MAX_WAIT=0): the request waits until
	// the client disconnects.
	StayUnbounded bool
	// CapacityRetryPersist turns on persist mode for every request
	// (SUBROUTER_CODEX_CAPACITY_RETRY=persist); where the headers are allowed
	// a request header can still opt out, or in when this is off.
	CapacityRetryPersist bool
	// CapacityRetryHeader lets clients choose the policy with the
	// X-Subrouter-Capacity-Retry and X-Subrouter-Retry headers without the
	// account failover
	// (SUBROUTER_CODEX_CAPACITY_RETRY_HEADER=1). With the failover on the
	// headers are always honored.
	CapacityRetryHeader bool
	// CapacityRetryBudget is the persist-mode budget. Zero means 2m.
	CapacityRetryBudget time.Duration

	// Test seams for the jittered gaps; nil uses the real jitter.
	sameAccountGap func() time.Duration
	switchGap      func() time.Duration
	persistGap     func() time.Duration
	// stayRetryLimit, when positive, bounds the same-account ladder by count
	// so tests can assert exact attempt counts; production has no count
	// bound, only the wall-clock cap and cancellation.
	stayRetryLimit int
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

// codexOverloadFailoverTransport replays a capacity-failed Codex request: on
// the same account by default, and on the next best account when the
// failover is enabled. It sits above the pool retry stack and below the
// regional egress and Azure fallbacks, which only run once it gives up.
type codexOverloadFailoverTransport struct {
	base      http.RoundTripper
	server    *Server
	agent     string
	session   string
	userEmail string
	account   string
	poolModel string
	// budget and attempt: see replayablePostRetryTransport.
	budget  *attemptBudget
	attempt *upstreamAttempt
	// policy is the request's capacity retry policy (default or persist).
	policy codexCapacityRetryPolicy
	// serviceTier is the request's service_tier; with poolModel it names
	// the capacity pool a failure is marked in.
	serviceTier string
	// Test seams: the clock and the gap sleep. Nil uses the real ones.
	now   func() time.Time
	sleep func(context.Context, time.Duration) bool
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
	if t.attempt == nil {
		t.attempt = standaloneUpstreamAttempt(req, t.server, accounts.Account{ID: t.account, Provider: accounts.ProviderCodex}, t.budget)
		t.attempt.provider, t.attempt.path, t.attempt.poolModel = accounts.ProviderCodex, req.URL.Path, t.poolModel
	}
	a := t.attempt
	config := t.server.CodexOverloadFailover
	ctx := req.Context()
	pickCtx := withCodexServiceTier(ctx, t.serviceTier)
	started := t.clock()
	deadline := started.Add(t.server.codexDefaultRetryBudget(t.poolModel, t.serviceTier))
	// The same-account ladder (failover off) has its own policy and budget.
	stayPolicy, explicitStay := config.stayPolicy(t.policy)
	stayInterval := stayPolicy.intervalOr(codexCapacityDefaultStayInterval)
	stayBudget, stayUnbounded := t.server.codexStayBudget(stayPolicy, explicitStay)
	stayDeadline := started.Add(stayBudget)
	var stayLog overloadRetryLog
	// releaseHeld ends this request's count in the held-in-overload gauge;
	// set on its first same-account retry.
	var releaseHeld func()
	defer func() {
		if releaseHeld != nil {
			releaseHeld()
		}
	}()
	attemptReq := req
	// addressed is the account attemptReq carries credentials for. accountID
	// is the account that answered last, which a lower layer's failover can
	// make different.
	addressed := a.current()
	accountID := t.account
	tried := map[string]struct{}{}
	if accountID != "" {
		tried[accountID] = struct{}{}
	}
	maxAccounts := config.maxAccounts()
	switched := 0
	sameAccountLeft := codexCapacitySameAccountRetries
	stayRetries := 0
	persisting := false
	var persistDeadline time.Time
	releasePersist := func() {}
	defer func() { releasePersist() }()
	for attempt := 1; ; attempt++ {
		response, err := a.send(base, attemptReq, addressed)
		if err != nil || !a.replayable() || ctx.Err() != nil {
			return response, err
		}
		// The usage-limit layer below may have failed over again; credit the
		// account that actually answered.
		if routed, ok := routedResponseAccount(response); ok && routed.ID != "" {
			accountID = routed.ID
			tried[accountID] = struct{}{}
		}
		failed, reason, response := codexOverloadFailure(response)
		if !failed {
			if codexSuccessStatus(response.StatusCode) {
				t.server.clearAccountCapacity(accountID, t.poolModel)
				t.server.recordCodexCapacityOutcome(t.poolModel, t.serviceTier, false)
			}
			if t.account != "" && accountID != t.account &&
				codexSuccessStatus(response.StatusCode) && t.server.Sessions != nil {
				// Pin the conversation to the account that served it,
				// whether this layer's failover or the layer below (quota or
				// model failover) moved it there, so the next turn does not
				// start on the one that failed. Only once the turn completes
				// (terminal success event or clean EOF), like the
				// usage-limit layer: a stream that fails after its 2xx
				// headers must not move the session. CompareAndPut from the
				// request's original account, so it is a no-op when the
				// layer below already committed the same move.
				if err := t.server.commitSuccessfulHTTPResponse(response, t.agent, t.session, t.account, accountID, t.userEmail); err != nil && t.server.Logger != nil {
					t.server.Logger.Warn("codex overload failover could not commit session reassignment", "session", t.session, "account", accountID, "error", err)
				}
			}
			return response, nil
		}
		// Every failure classified here is pre-output: the stream peek
		// holds the response until the first visible output, so nothing has
		// reached the client yet and a replay cannot duplicate anything.
		if config.enabled() {
			// Marks steer later placement away from this account, and after
			// repeated failures evict its sticky sessions. That is a switch,
			// so it belongs to the opt-in failover only.
			hint := codexCapacityRetryHint(response)
			t.server.markAccountOverloaded(accountID, t.poolModel, t.serviceTier, config.capacityMarkTTL(hint))
		}
		t.server.recordCodexCapacityOutcome(t.poolModel, t.serviceTier, true)

		var plan codexCapacityAttemptPlan
		planned := false
		if !persisting {
			if config.enabled() {
				plan, planned = t.planDefaultRetry(pickCtx, accountID, reason, tried, &sameAccountLeft, &switched, maxAccounts, deadline)
			} else {
				plan, planned = t.planStayRetry(accountID, reason, &stayRetries, stayInterval, stayDeadline, stayUnbounded)
				if planned && releaseHeld == nil {
					releaseHeld = t.server.enterOverloadHold(accounts.ProviderCodex)
				}
			}
			// Without the failover, persist mode is a preset of the
			// same-account ladder above; the persist loop is the failover's.
			if !planned && t.policy.persist && config.enabled() {
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
			plan, planned = t.planPersistRetry(pickCtx, accountID, tried, persistDeadline)
			if !planned {
				t.logOverload("codex capacity persist retry exhausted", accountID, reason, switched, "persist_budget")
			}
		}
		if !planned {
			return response, nil
		}
		if plan.next == nil && accountID != addressed.ID {
			// "Same account" is the account that answered, not the one this
			// layer addressed: replaying the old request would send it back
			// through the account the layer below already left.
			current, ok := t.server.codexAccountByID(ctx, accountID)
			if !ok {
				return response, nil
			}
			plan.next = &current
		}
		if !t.sleepContext(ctx, plan.gap) {
			return response, nil
		}
		// Same account: attemptReq again, keeping its auth headers and the
		// account it is addressed to. Switch: the client request retargeted
		// to plan.next.
		var nextReq *http.Request
		var replayErr error
		if plan.next == nil {
			nextReq, replayErr = a.replay(attemptReq, nil)
		} else {
			nextReq, replayErr = a.replay(req, plan.next)
		}
		if replayErr != nil {
			return response, nil
		}
		if response.Body != nil {
			_ = response.Body.Close()
		}
		previous := accountID
		attemptReq = nextReq
		if plan.next != nil {
			accountID = plan.next.ID
			tried[accountID] = struct{}{}
			if t.server.SchedulerRef != nil {
				t.server.SchedulerRef.NoteRouted(accounts.ProviderCodex, accountID)
			}
			// The layers below start from the account the request is
			// addressed to, so a quota/auth failure from next is charged to
			// next, not to the account that was merely overloaded.
			addressed = accounts.Account{
				ID: plan.next.ID, Provider: accounts.ProviderCodex, CredentialVersion: plan.next.CredentialIdentity(),
			}
		}
		// The same-account wait can run for minutes: log its first retry,
		// then about once a minute. Account switches log every time.
		now := t.clock()
		if t.server.Logger != nil && (plan.phase != "same_account" || config.enabled() || stayLog.due(now)) {
			t.server.Logger.Warn("retrying codex request after capacity failure",
				"agent", t.agent, "session", t.session, "reason", reason, "phase", plan.phase,
				"previous_account", previous, "account", accountID, "attempt", attempt+1,
				"gap", plan.gap.String(), "elapsed", now.Sub(started).Round(time.Millisecond).String())
		}
	}
}

func (t codexOverloadFailoverTransport) clock() time.Time {
	if t.now != nil {
		return t.now()
	}
	return time.Now()
}

func (t codexOverloadFailoverTransport) sleepContext(ctx context.Context, d time.Duration) bool {
	if t.sleep != nil {
		return t.sleep(ctx, d)
	}
	return codexSleepContext(ctx, d)
}

// planStayRetry is the default ladder while the failover is off: retries on
// the same account, ramping up to the steady interval, until the next wait
// would end past the wall-clock deadline (never, when unbounded; the
// client's cancellation still ends it). It does not draw from the request's
// shared attempt budget, which is sized for account failover inside the pool
// stack below and would otherwise cut the ladder to a few quick tries.
func (t codexOverloadFailoverTransport) planStayRetry(accountID, reason string, retries *int, interval time.Duration, deadline time.Time, unbounded bool) (codexCapacityAttemptPlan, bool) {
	config := t.server.CodexOverloadFailover
	if config != nil && config.stayRetryLimit > 0 && *retries >= config.stayRetryLimit {
		t.logOverload("codex capacity retry exhausted", accountID, reason, 0, "max_retries")
		return codexCapacityAttemptPlan{}, false
	}
	gap := config.stayDelay(*retries, interval)
	if !unbounded && t.clock().Add(gap).After(deadline) {
		t.logOverload("codex capacity retry exhausted", accountID, reason, 0, "time_budget")
		return codexCapacityAttemptPlan{}, false
	}
	*retries++
	return codexCapacityAttemptPlan{gap: gap, phase: "same_account"}, true
}

// planDefaultRetry is the failover ladder: one quick retry on the same
// account, then the overload failover to other accounts, all inside the
// time budget and the request's shared attempt budget.
func (t codexOverloadFailoverTransport) planDefaultRetry(ctx context.Context, accountID, reason string, tried map[string]struct{}, sameAccountLeft, switched *int, maxAccounts int, deadline time.Time) (codexCapacityAttemptPlan, bool) {
	config := t.server.CodexOverloadFailover
	if *sameAccountLeft > 0 {
		*sameAccountLeft--
		gap := config.sameAccountDelay()
		if time.Now().Add(gap).After(deadline) {
			t.logOverload("codex capacity retry exhausted", accountID, reason, *switched, "time_budget")
			return codexCapacityAttemptPlan{}, false
		}
		if !t.attempt.consume() {
			t.logOverload("codex capacity retry exhausted", accountID, reason, *switched, "retry_budget")
			return codexCapacityAttemptPlan{}, false
		}
		return codexCapacityAttemptPlan{gap: gap, phase: "same_account"}, true
	}
	// The same account shed the request again (its capacity mark now
	// ranks it last for this pool): move on.
	if *switched >= maxAccounts {
		t.logOverload("codex overload failover exhausted", accountID, reason, *switched, "max_accounts")
		return codexCapacityAttemptPlan{}, false
	}
	gap := config.switchDelay()
	if time.Now().Add(gap).After(deadline) {
		t.logOverload("codex overload failover exhausted", accountID, reason, *switched, "time_budget")
		return codexCapacityAttemptPlan{}, false
	}
	if !t.attempt.consume() {
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

// planPersistRetry keeps going after the default ladder. With the failover
// off it stays on the same account. With it on: the best untried account, or
// once every account has been tried, the best account again (shedding is a
// probability, so a retried account can pass). Gaps are 0.5-2s so a
// persisting client does not hammer the pool.
func (t codexOverloadFailoverTransport) planPersistRetry(ctx context.Context, accountID string, tried map[string]struct{}, deadline time.Time) (codexCapacityAttemptPlan, bool) {
	config := t.server.CodexOverloadFailover
	gap := config.persistDelay()
	if time.Now().Add(gap).After(deadline) {
		return codexCapacityAttemptPlan{}, false
	}
	if !config.enabled() {
		return codexCapacityAttemptPlan{gap: gap, phase: "persist_same_account"}, true
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

// codexAccountByID finds a Codex account of the current inventory by ID.
func (s *Server) codexAccountByID(ctx context.Context, id string) (accounts.Account, bool) {
	if s == nil || id == "" {
		return accounts.Account{}, false
	}
	for _, account := range s.accountListContext(ctx) {
		if account.ID == id && accountProviderOrCodex(account) == accounts.ProviderCodex {
			return account, true
		}
	}
	return accounts.Account{}, false
}

func (t codexOverloadFailoverTransport) logOverload(message, accountID, reason string, switched int, why string) {
	if t.server == nil || t.server.Logger == nil {
		return
	}
	t.server.Logger.Warn(message, "agent", t.agent, "session", t.session, "account", accountID,
		"reason", reason, "switched", switched, "why", why)
}

// markAccountOverloaded records a capacity mark for the account in the
// (model, service tier) pool for a short window and returns the account's
// consecutive failures there. It is not a quota mark: headroom and sticky
// sessions are untouched, the account only ranks below unmarked ones for
// this pool, and a session leaves it only once it keeps failing
// (selectacct.CapacityStickyEvictFailures).
func (s *Server) markAccountOverloaded(accountID, model, tier string, ttl time.Duration) int {
	if s == nil || s.SchedulerRef == nil || accountID == "" {
		return 0
	}
	failures := s.SchedulerRef.MarkCapacityUntil(accounts.ProviderCodex, accountID, model, tier, time.Now().Add(ttl))
	if s.Logger != nil {
		s.Logger.Warn("codex account marked at capacity for this model pool", "account", accountID,
			"pool", selectacct.CapacityPoolKey(model, tier), "failures", failures, "ttl", ttl.String())
	}
	return failures
}

// clearAccountCapacity drops the account's capacity marks for the model
// after it served the model successfully.
func (s *Server) clearAccountCapacity(accountID, model string) {
	if s == nil || s.SchedulerRef == nil || accountID == "" {
		return
	}
	if s.SchedulerRef.ClearCapacity(accounts.ProviderCodex, accountID, model) && s.Logger != nil {
		s.Logger.Info("codex account served the model again; capacity mark cleared", "account", accountID, "model", model)
	}
}

// withCapacityMarks attaches the Codex capacity marks for the request's
// (model, tier) pool to a scheduler. tier may be selectacct.CapacityAnyTier.
func (s Server) withCapacityMarks(scheduler selectacct.Scheduler, provider accounts.Provider, model, tier string) selectacct.Scheduler {
	if s.SchedulerRef == nil || accountProviderFor(provider) != accounts.ProviderCodex {
		return scheduler
	}
	marks := s.SchedulerRef.CapacityMarks(accounts.ProviderCodex, model, tier)
	if len(marks) == 0 {
		return scheduler
	}
	return scheduler.WithCapacityMarks(marks)
}

// codexCapacitySelectionTier is the capacity pool tier for placing a
// request: the body's service_tier, or every tier for a websocket upgrade,
// whose turns name their tier only after the connection is up.
func codexCapacitySelectionTier(r *http.Request, maxBodyBytes int64) string {
	if r == nil || websocket.IsWebSocketUpgrade(r) {
		return selectacct.CapacityAnyTier
	}
	return session.ExtractServiceTier(r, maxBodyBytes)
}

type codexServiceTierContextKey struct{}

// withCodexServiceTier carries the request's service tier to account
// pickers that only receive a context (oauthRetryCandidate).
func withCodexServiceTier(ctx context.Context, tier string) context.Context {
	return context.WithValue(ctx, codexServiceTierContextKey{}, tier)
}

// codexServiceTierFromContext is the tier withCodexServiceTier stored, or
// CapacityAnyTier when the caller did not know it.
func codexServiceTierFromContext(ctx context.Context) string {
	if ctx != nil {
		if tier, ok := ctx.Value(codexServiceTierContextKey{}).(string); ok {
			return tier
		}
	}
	return selectacct.CapacityAnyTier
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
// account. Only with the opt-in failover: a reroute is an account switch.
// False once the session has used its reroute budget: 3 per 10
// minutes by default, 20 for a session in persist mode, which also waits a
// jittered 0.5-2s before the close so its reconnects do not hammer the pool.
func (s Server) codexOverloadWebSocketReroute(ctx context.Context, agentType, sessionID, accountID, model, tier string, body []byte, persist bool) bool {
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
	s.markAccountOverloaded(accountID, model, tier, s.CodexOverloadFailover.capacityMarkTTL(codexRetryHintJSON(body)))
	s.recordCodexCapacityOutcome(model, tier, true)
	if s.Logger != nil {
		s.Logger.Warn("codex websocket turn hit a capacity error; closing 1012 so the session reconnects",
			"agent", agentType, "session", sessionID, "account", accountID, "persist", persist)
	}
	if persist {
		_ = codexSleepContext(ctx, s.CodexOverloadFailover.persistDelay())
	}
	return true
}
