package proxy

import (
	"log/slog"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Capacity retry policy.
//
// OpenAI's "Selected model is at capacity" is probabilistic load shedding:
// pressing retry a few times usually gets the same request through, often on
// the same account. So the proxy retries capacity failures transparently, but
// only before any output reached the client (the stream peek guarantees a
// failure it classifies is pre-output). Only the wall-clock cap or the
// client's cancellation ends it.
//
// The session's prompt cache lives on its account, and moving a turn to
// another account re-bills the whole conversation as uncached input. So by
// default every retry stays on the session's account, and switching is
// opt-in (SUBROUTER_CODEX_OVERLOAD_FAILOVER=1):
//
//   - default (no configuration): wait it out on the same account with
//     jittered gaps of about 0.5s, 1s, 2s, 4s, 8s, then a steady ~9s (8-10s;
//     CodexOverloadFailoverConfig.StayInterval, the ramp capped at it), for
//     up to 4m of wall-clock time from the first attempt (StayMaxWait;
//     unbounded with SUBROUTER_CODEX_CAPACITY_RETRY_MAX_WAIT=0), inside Codex
//     CLI's 5-minute stream idle timeout. With an egress or Azure fallback
//     configured the operator default is capped at ~10s so the fallback takes
//     over sooner. Pool-wide shedding does not shorten this ladder: its
//     steady gaps are already slow. The account is not marked, so its sticky
//     sessions keep it. Then the failure goes back to the client (or on to
//     the egress and Azure fallbacks when configured).
//   - failover (opt-in): one same-account retry after a 250-750ms jittered
//     gap, then the overload failover to other accounts (100-400ms gaps),
//     all inside a ~10s budget (~3s while shedding). Failed accounts are
//     marked at capacity for the model pool.
//   - persist (opt-in): keep retrying until a budget (default 2m, from the
//     first attempt) expires. With the failover off it is a preset of the
//     same-account ladder (steady 1s gaps, capped at the longer of the
//     persist budget and the operator's wait, an unbounded operator wait
//     kept, not shortened by a configured fallback). With the failover on, after
//     the failover ladder, 0.5-2s jittered gaps across accounts. Enabled for
//     every request by SUBROUTER_CODEX_CAPACITY_RETRY=persist, or per request
//     by the X-Subrouter-Capacity-Retry: persist header. The headers are a
//     client choice, so they count only when the operator allows them:
//     SUBROUTER_CODEX_OVERLOAD_FAILOVER=1 or
//     SUBROUTER_CODEX_CAPACITY_RETRY_HEADER=1. With the failover on, at most
//     one persist loop per client session is in flight; concurrent requests
//     from the same session get the default policy.
//
// Client cancellation ends either loop immediately.
const (
	// CodexCapacityRetryHeader selects the policy per request: "persist" or
	// "default". It is stripped before the request goes upstream.
	CodexCapacityRetryHeader = "X-Subrouter-Capacity-Retry"
	// CodexCapacityRetryBudgetHeader overrides the persist budget for one
	// request: a Go duration ("90s", "5m") or a number of seconds.
	CodexCapacityRetryBudgetHeader = "X-Subrouter-Capacity-Retry-Budget"

	// codexCapacityDefaultRetryBudget bounds the failover ladder.
	codexCapacityDefaultRetryBudget = 10 * time.Second
	// codexCapacityFallbackStayRetryBudget caps the same-account ladder
	// (codexCapacityDefaultStayMaxWait) when a regional
	// egress or Azure fallback is configured: the operator set those up to
	// take over from a shedding pool, so they get their turn sooner.
	codexCapacityFallbackStayRetryBudget = 10 * time.Second
	codexCapacityDefaultPersistBudget    = 2 * time.Minute
	// codexCapacityMaxPersistBudget caps a caller-chosen budget: a request
	// holding an upstream slot for longer than this is not a retry any more.
	codexCapacityMaxPersistBudget = 10 * time.Minute
	// codexCapacitySameAccountRetries is how many times the failover policy
	// retries on the account that just shed the request before moving on.
	codexCapacitySameAccountRetries = 1
	// codexCapacityStayFirstGap and codexCapacityStayMaxGap shape the ramp
	// of the same-account ladder: the gap doubles from the first to the max
	// (or the interval, when smaller), then holds at the interval.
	codexCapacityStayFirstGap = 500 * time.Millisecond
	codexCapacityStayMaxGap   = 8 * time.Second
	// codexCapacityPersistStayInterval is persist mode's steady gap on the
	// same account.
	codexCapacityPersistStayInterval = time.Second
)

// ParseCodexCapacityRetryMode reads SUBROUTER_CODEX_CAPACITY_RETRY or the
// header value: "persist" or "default" (empty means default).
func ParseCodexCapacityRetryMode(raw string) (persist bool, ok bool) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", "default":
		return false, true
	case "persist":
		return true, true
	}
	return false, false
}

// ParseCodexCapacityRetryBudget reads a persist budget: a Go duration or a
// number of seconds, capped at 10m. Zero means unset or invalid.
func ParseCodexCapacityRetryBudget(raw string) time.Duration {
	return parseCodexCapacityRetryBudget(raw)
}

func parseCodexCapacityRetryBudget(raw string) time.Duration {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0
	}
	budget, err := time.ParseDuration(raw)
	if err != nil {
		seconds, numberErr := strconv.ParseFloat(raw, 64)
		if numberErr != nil {
			return 0
		}
		budget = time.Duration(seconds * float64(time.Second))
	}
	if budget <= 0 {
		return 0
	}
	return min(budget, codexCapacityMaxPersistBudget)
}

// codexCapacityRetryPolicy is one request's resolved policy.
type codexCapacityRetryPolicy struct {
	persist       bool
	persistBudget time.Duration
	// retry is the client's X-Subrouter-Retry override, where allowed.
	retry overloadRetryOverride
}

// stayPolicy is the request's same-account ladder while the failover is off:
// the operator's StayInterval/StayMaxWait, or the persist preset (1s gaps),
// with the client's X-Subrouter-Retry override on top. Persist only makes
// the gaps faster: its cap is the longer of the persist budget and the
// operator's wait, and an operator's unbounded wait stays unbounded. An
// explicit wait (persist, or a requested max-wait) is not shortened by a
// configured fallback.
func (c *CodexOverloadFailoverConfig) stayPolicy(policy codexCapacityRetryPolicy) (overloadRetryPolicy, bool) {
	var stay overloadRetryPolicy
	if c != nil {
		stay = overloadRetryPolicy{interval: c.StayInterval, maxWait: c.StayMaxWait, unbounded: c.StayUnbounded}
	}
	explicit := false
	if policy.persist {
		stay.interval = codexCapacityPersistStayInterval
		stay.maxWait = max(policy.persistBudget, stay.maxWaitOr(codexCapacityDefaultStayMaxWait))
		explicit = true
	}
	return policy.retry.apply(stay), explicit || policy.retry.maxWaitSet
}

// codexCapacityRetryPolicyFor resolves the request's policy: when the
// operator allows the headers (headersAllowed), they win over the
// environment, in both directions; otherwise they are ignored.
func (c *CodexOverloadFailoverConfig) codexCapacityRetryPolicyFor(r *http.Request, logger *slog.Logger) codexCapacityRetryPolicy {
	policy := codexCapacityRetryPolicy{persistBudget: codexCapacityDefaultPersistBudget}
	if c != nil {
		policy.persist = c.CapacityRetryPersist
		if c.CapacityRetryBudget > 0 {
			policy.persistBudget = min(c.CapacityRetryBudget, codexCapacityMaxPersistBudget)
		}
	}
	if r != nil && c.headersAllowed() {
		if persist, ok := ParseCodexCapacityRetryMode(r.Header.Get(CodexCapacityRetryHeader)); ok && strings.TrimSpace(r.Header.Get(CodexCapacityRetryHeader)) != "" {
			policy.persist = persist
		}
		if budget := parseCodexCapacityRetryBudget(r.Header.Get(CodexCapacityRetryBudgetHeader)); budget > 0 {
			policy.persistBudget = budget
		}
	}
	policy.retry = overloadRetryOverrideFor(r, c.headersAllowed(), logger)
	return policy
}

// headersAllowed reports whether a client may pick its own capacity retry
// policy by header. Persist lets a request hold an upstream slot for up to
// 10m of retries, so that is the operator's call: allowed with the account
// failover (as before the same-account default existed) or with the explicit
// SUBROUTER_CODEX_CAPACITY_RETRY_HEADER=1.
func (c *CodexOverloadFailoverConfig) headersAllowed() bool {
	return c.enabled() || (c != nil && c.CapacityRetryHeader)
}

// retryBudget is the default ladder's time budget: the configured one, else
// ~10s for the failover ladder and the operator's same-account cap (default
// 4m) without it. Zero means unbounded (StayUnbounded).
func (c *CodexOverloadFailoverConfig) retryBudget() time.Duration {
	if c != nil && c.RetryBudget > 0 {
		return c.RetryBudget
	}
	if c.enabled() {
		return codexCapacityDefaultRetryBudget
	}
	if c != nil && c.StayUnbounded {
		return 0
	}
	if c != nil && c.StayMaxWait > 0 {
		return c.StayMaxWait
	}
	return codexCapacityDefaultStayMaxWait
}

// stayDelay is the gap before same-account retry number retry (0-based) when
// the failover is off: 0.5s doubling to 8s (the ramp capped at interval),
// jittered by ±20%, then interval jittered by ±10% (8-10s for the default
// 9s), so a burst of shed requests does not come back in lockstep.
func (c *CodexOverloadFailoverConfig) stayDelay(retry int, interval time.Duration) time.Duration {
	if c != nil && c.sameAccountGap != nil {
		return c.sameAccountGap()
	}
	if retry < 5 {
		return codexJitter(min(codexCapacityStayFirstGap<<retry, codexCapacityStayMaxGap, interval), 0.2)
	}
	return codexJitter(interval, 0.1)
}

func (c *CodexOverloadFailoverConfig) sameAccountDelay() time.Duration {
	if c != nil && c.sameAccountGap != nil {
		return c.sameAccountGap()
	}
	return 250*time.Millisecond + rand.N(500*time.Millisecond)
}

func (c *CodexOverloadFailoverConfig) switchDelay() time.Duration {
	if c != nil && c.switchGap != nil {
		return c.switchGap()
	}
	return codexOverloadSwitchDelay()
}

func (c *CodexOverloadFailoverConfig) persistDelay() time.Duration {
	if c != nil && c.persistGap != nil {
		return c.persistGap()
	}
	return 500*time.Millisecond + rand.N(1500*time.Millisecond)
}

// codexPersistLoops admits at most one persist retry loop per client
// session, so a client that fires several requests at once cannot multiply
// its hammering of a pool that is shedding load.
type codexPersistLoops struct {
	mu     sync.Mutex
	active map[string]struct{}
}

func newCodexPersistLoops() *codexPersistLoops {
	return &codexPersistLoops{active: map[string]struct{}{}}
}

// acquire claims the session's slot. The release func is idempotent.
func (l *codexPersistLoops) acquire(key string) (func(), bool) {
	if l == nil {
		return func() {}, false
	}
	if key == "" {
		// No session identity to group by: allow, the time budget still
		// bounds the loop.
		return func() {}, true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, busy := l.active[key]; busy {
		return nil, false
	}
	l.active[key] = struct{}{}
	var once sync.Once
	return func() {
		once.Do(func() {
			l.mu.Lock()
			delete(l.active, key)
			l.mu.Unlock()
		})
	}, true
}
