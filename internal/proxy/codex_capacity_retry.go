package proxy

import (
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
// failure it classifies is pre-output), and never without a bound.
//
// The session's prompt cache lives on its account, and moving a turn to
// another account re-bills the whole conversation as uncached input. So by
// default every retry stays on the session's account, and switching is
// opt-in (SUBROUTER_CODEX_OVERLOAD_FAILOVER=1):
//
//   - default (no configuration): retry on the same account with growing,
//     jittered gaps (about 0.5s, 1s, 2s, 4s, 8s, 8s, ...; at most
//     codexCapacityStayMaxRetries retries) inside a ~30s budget (~10s when an
//     egress or Azure fallback is configured), which shrinks to ~3s while the
//     model is shedding pool-wide. The account is not marked, so its sticky
//     sessions keep it. Then the failure goes back to the client (or on to
//     the egress and Azure fallbacks when configured).
//   - failover (opt-in): one same-account retry after a 250-750ms jittered
//     gap, then the overload failover to other accounts (100-400ms gaps),
//     all inside a ~10s budget (~3s while shedding). Failed accounts are
//     marked at capacity for the model pool.
//   - persist (opt-in): after the ladder above, keep retrying with 0.5-2s
//     jittered gaps until a budget (default 2m) expires: on the same account
//     when the failover is off, across accounts when it is on. Enabled for
//     every request by SUBROUTER_CODEX_CAPACITY_RETRY=persist, or per request
//     by the X-Subrouter-Capacity-Retry: persist header. The headers are a
//     client choice, so they count only when the operator allows them:
//     SUBROUTER_CODEX_OVERLOAD_FAILOVER=1 or
//     SUBROUTER_CODEX_CAPACITY_RETRY_HEADER=1. At most one persist
//     loop per client session is in flight; concurrent requests from the same
//     session get the default policy.
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
	// codexCapacityStayRetryBudget bounds the default same-account ladder:
	// long enough for a shedding burst to pass, short enough that the client
	// is answered well inside a minute.
	codexCapacityStayRetryBudget = 30 * time.Second
	// codexCapacityFallbackStayRetryBudget caps that ladder when a regional
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
	// codexCapacityStayMaxRetries bounds the same-account ladder used when
	// the account failover is off; the time budget usually ends it first.
	codexCapacityStayMaxRetries = 8
	// codexCapacityStayFirstGap and codexCapacityStayMaxGap shape that
	// ladder: the gap doubles from the first to the max.
	codexCapacityStayFirstGap = 500 * time.Millisecond
	codexCapacityStayMaxGap   = 8 * time.Second
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
}

// codexCapacityRetryPolicyFor resolves the request's policy: when the
// operator allows the headers (headersAllowed), they win over the
// environment, in both directions; otherwise they are ignored.
func (c *CodexOverloadFailoverConfig) codexCapacityRetryPolicyFor(r *http.Request) codexCapacityRetryPolicy {
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
// ~10s for the failover ladder and ~30s for the same-account ladder.
func (c *CodexOverloadFailoverConfig) retryBudget() time.Duration {
	if c != nil && c.RetryBudget > 0 {
		return c.RetryBudget
	}
	if c.enabled() {
		return codexCapacityDefaultRetryBudget
	}
	return codexCapacityStayRetryBudget
}

// stayDelay is the gap before same-account retry number retry (0-based) when
// the failover is off: 0.5s doubling to 8s, jittered by ±20% so a burst of
// shed requests does not come back in lockstep.
func (c *CodexOverloadFailoverConfig) stayDelay(retry int) time.Duration {
	if c != nil && c.sameAccountGap != nil {
		return c.sameAccountGap()
	}
	gap := codexCapacityStayMaxGap
	if retry < 5 {
		gap = min(codexCapacityStayFirstGap<<retry, codexCapacityStayMaxGap)
	}
	return codexJitter(gap, 0.2)
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
