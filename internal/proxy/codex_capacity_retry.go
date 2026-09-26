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
// failure it classifies is pre-output), and never without a bound:
//
//   - default: one same-account retry after a 250-750ms jittered gap (the
//     session's prompt cache lives on that account), then the overload
//     failover to other accounts (100-400ms gaps), all inside a ~10s budget
//     that shrinks to ~3s while the model is shedding pool-wide. Then the
//     failure goes back to the client.
//   - persist (opt-in): after the default ladder, keep retrying with 0.5-2s
//     jittered gaps across accounts until a budget (default 2m) expires.
//     Enabled for every request by SUBROUTER_CODEX_CAPACITY_RETRY=persist, or
//     per request by the X-Subrouter-Capacity-Retry: persist header. At most
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

	codexCapacityDefaultRetryBudget   = 10 * time.Second
	codexCapacityDefaultPersistBudget = 2 * time.Minute
	// codexCapacityMaxPersistBudget caps a caller-chosen budget: a request
	// holding an upstream slot for longer than this is not a retry any more.
	codexCapacityMaxPersistBudget = 10 * time.Minute
	// codexCapacitySameAccountRetries is how many times the default policy
	// retries on the account that just shed the request before moving on.
	codexCapacitySameAccountRetries = 1
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

// codexCapacityRetryPolicyFor resolves the request's policy: the header wins
// over the environment, in both directions.
func (c *CodexOverloadFailoverConfig) codexCapacityRetryPolicyFor(r *http.Request) codexCapacityRetryPolicy {
	policy := codexCapacityRetryPolicy{persistBudget: codexCapacityDefaultPersistBudget}
	if c != nil {
		policy.persist = c.CapacityRetryPersist
		if c.CapacityRetryBudget > 0 {
			policy.persistBudget = min(c.CapacityRetryBudget, codexCapacityMaxPersistBudget)
		}
	}
	if r != nil {
		if persist, ok := ParseCodexCapacityRetryMode(r.Header.Get(CodexCapacityRetryHeader)); ok && strings.TrimSpace(r.Header.Get(CodexCapacityRetryHeader)) != "" {
			policy.persist = persist
		}
		if budget := parseCodexCapacityRetryBudget(r.Header.Get(CodexCapacityRetryBudgetHeader)); budget > 0 {
			policy.persistBudget = budget
		}
	}
	return policy
}

func (c *CodexOverloadFailoverConfig) retryBudget() time.Duration {
	if c == nil || c.RetryBudget <= 0 {
		return codexCapacityDefaultRetryBudget
	}
	return c.RetryBudget
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
