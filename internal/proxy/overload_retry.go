package proxy

import (
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
)

// Same-account overload retry, shared shape for Claude and Codex.
//
// Overload should be rare and brief. The session's prompt cache lives on its
// account, so by default an overloaded request waits it out there: a short
// ramp of growing gaps, then a steady gap, until a wall-clock cap measured
// from the first attempt. The cap keeps the client answered with a clean
// overload error before its own request timeout, rather than a timeout.
const (
	// Claude: 1s, 2s, 4s, 8s, then every 15s for up to 8m. Claude Code gives
	// a request 10 minutes, so 8m leaves it room to receive the 529.
	claudeOverloadDefaultInterval = 15 * time.Second
	claudeOverloadDefaultMaxWait  = 8 * time.Minute
	// claudeOverloadMaxRetryAfter caps one Retry-After-driven wait.
	claudeOverloadMaxRetryAfter = 15 * time.Second

	// Codex: 0.5s doubling to 8s, then every ~9s (8-10s with jitter) for up
	// to 4m, inside Codex CLI's 5-minute stream idle timeout.
	codexCapacityDefaultStayInterval = 9 * time.Second
	codexCapacityDefaultStayMaxWait  = 4 * time.Minute

	// overloadRetryLogEvery is how often a long same-account wait is logged
	// after its first retry.
	overloadRetryLogEvery = time.Minute
)

const (
	// OverloadRetryHeader lets a client shape its own same-account overload
	// wait: "interval=2s,max-wait=20m", either key optional, max-wait=0
	// meaning until the client disconnects. Honored only when the operator
	// allows it (SUBROUTER_CLAUDE_OVERLOAD_RETRY_HEADER=1,
	// SUBROUTER_CODEX_CAPACITY_RETRY_HEADER=1 or the Codex failover), and
	// stripped before the request goes upstream.
	OverloadRetryHeader = "X-Subrouter-Retry"
	// OverloadRetryMinInterval is the floor for a configured or requested
	// steady gap.
	OverloadRetryMinInterval = 500 * time.Millisecond
	// OverloadRetryMaxWaitCap caps a client-requested max-wait.
	OverloadRetryMaxWaitCap = 60 * time.Minute
)

// overloadRetryPolicy is one request's same-account overload ladder: the
// steady gap after the ramp (the ramp is capped at it) and the wall-clock cap.
// Zero values mean the provider default; unbounded means no cap, so only the
// client's disconnect ends the wait.
type overloadRetryPolicy struct {
	interval  time.Duration
	maxWait   time.Duration
	unbounded bool
}

func (p overloadRetryPolicy) intervalOr(fallback time.Duration) time.Duration {
	if p.interval > 0 {
		return p.interval
	}
	return fallback
}

func (p overloadRetryPolicy) maxWaitOr(fallback time.Duration) time.Duration {
	if p.maxWait > 0 {
		return p.maxWait
	}
	return fallback
}

// overloadRetryOverride is a client's X-Subrouter-Retry request, already
// clamped: each key applies only when it was given.
type overloadRetryOverride struct {
	interval   time.Duration
	maxWait    time.Duration
	maxWaitSet bool
	unbounded  bool
}

func (o overloadRetryOverride) empty() bool { return o.interval <= 0 && !o.maxWaitSet }

// apply lays the override over the operator's policy.
func (o overloadRetryOverride) apply(policy overloadRetryPolicy) overloadRetryPolicy {
	if o.interval > 0 {
		policy.interval = o.interval
	}
	if o.maxWaitSet {
		policy.maxWait, policy.unbounded = o.maxWait, o.unbounded
	}
	return policy
}

// parseOverloadRetryHeader reads "interval=2s,max-wait=20m". An interval
// below the 500ms floor is raised to it and a max-wait above 60m lowered to
// it; max-wait=0 means no cap. Malformed or unknown entries are ignored and
// reported in problems, for a debug log.
func parseOverloadRetryHeader(raw string) (overloadRetryOverride, []string) {
	var override overloadRetryOverride
	var problems []string
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		key, value, ok := strings.Cut(part, "=")
		key = strings.ToLower(strings.TrimSpace(key))
		value = strings.TrimSpace(value)
		d, err := time.ParseDuration(value)
		if err != nil && value == "0" {
			d, err = 0, nil
		}
		if !ok || err != nil || d < 0 {
			problems = append(problems, part)
			continue
		}
		switch key {
		case "interval":
			if d == 0 {
				problems = append(problems, part)
				continue
			}
			override.interval = max(d, OverloadRetryMinInterval)
		case "max-wait":
			override.maxWaitSet = true
			override.unbounded = d == 0
			override.maxWait = min(d, OverloadRetryMaxWaitCap)
		default:
			problems = append(problems, part)
		}
	}
	return override, problems
}

// overloadRetryOverrideFor reads the request's X-Subrouter-Retry header when
// the operator allows it; otherwise the header is ignored.
func overloadRetryOverrideFor(r *http.Request, allowed bool, logger *slog.Logger) overloadRetryOverride {
	if r == nil || !allowed {
		return overloadRetryOverride{}
	}
	raw := strings.TrimSpace(r.Header.Get(OverloadRetryHeader))
	if raw == "" {
		return overloadRetryOverride{}
	}
	override, problems := parseOverloadRetryHeader(raw)
	if len(problems) > 0 && logger != nil {
		logger.Debug("ignoring malformed X-Subrouter-Retry entries", "entries", problems)
	}
	return override
}

// FormatOverloadRetryHeader builds an X-Subrouter-Retry value for a client;
// a zero interval or a negative maxWait leaves that key out, maxWait 0 means
// no cap.
func FormatOverloadRetryHeader(interval, maxWait time.Duration) string {
	var parts []string
	if interval > 0 {
		parts = append(parts, "interval="+interval.String())
	}
	if maxWait >= 0 {
		parts = append(parts, "max-wait="+maxWait.String())
	}
	return strings.Join(parts, ",")
}

// ClaudeOverloadRetryConfig shapes the same-account Claude overload ladder.
// A nil config uses the defaults.
type ClaudeOverloadRetryConfig struct {
	// AllowHeader honors a client's X-Subrouter-Retry header
	// (SUBROUTER_CLAUDE_OVERLOAD_RETRY_HEADER=1).
	AllowHeader bool
	// Interval is the steady gap after the 1s/2s/4s/8s ramp, which is capped
	// at it. Zero means 15s.
	Interval time.Duration
	// MaxWait caps the wait in wall-clock time from the first attempt. Zero
	// means 8m; see Unbounded.
	MaxWait time.Duration
	// Unbounded removes the cap (SUBROUTER_CLAUDE_OVERLOAD_MAX_WAIT=0): the
	// request waits until the client disconnects.
	Unbounded bool
}

// policy is the operator's ladder for every Claude request.
func (c *ClaudeOverloadRetryConfig) policy() overloadRetryPolicy {
	if c == nil {
		return overloadRetryPolicy{}
	}
	return overloadRetryPolicy{interval: c.Interval, maxWait: c.MaxWait, unbounded: c.Unbounded}
}

// policyFor is the operator's ladder with the request's X-Subrouter-Retry
// override, when the operator allows it.
func (c *ClaudeOverloadRetryConfig) policyFor(r *http.Request, logger *slog.Logger) overloadRetryPolicy {
	return overloadRetryOverrideFor(r, c != nil && c.AllowHeader, logger).apply(c.policy())
}

// claudeOverloadGap is the wait before same-account retry number retry
// (0-based): 1s, 2s, 4s, 8s, then interval, with the ramp capped at interval.
func claudeOverloadGap(retry int, interval time.Duration) time.Duration {
	if retry < 4 {
		return min(time.Second<<retry, interval)
	}
	return interval
}

// overloadHeldGauge counts requests currently waiting out an overload on
// their own account, per provider, for /_subrouter/health.
type overloadHeldGauge struct {
	claude atomic.Int64
	codex  atomic.Int64
}

func newOverloadHeldGauge() *overloadHeldGauge { return &overloadHeldGauge{} }

func (g *overloadHeldGauge) counter(provider accounts.Provider) *atomic.Int64 {
	if g == nil {
		return nil
	}
	switch provider {
	case accounts.ProviderClaude:
		return &g.claude
	case accounts.ProviderCodex:
		return &g.codex
	}
	return nil
}

// enter counts one held request and returns its idempotent release.
func (g *overloadHeldGauge) enter(provider accounts.Provider) func() {
	counter := g.counter(provider)
	if counter == nil {
		return func() {}
	}
	counter.Add(1)
	var once sync.Once
	return func() { once.Do(func() { counter.Add(-1) }) }
}

func (g *overloadHeldGauge) snapshot() map[string]int64 {
	if g == nil {
		return nil
	}
	return map[string]int64{
		string(accounts.ProviderClaude): g.claude.Load(),
		string(accounts.ProviderCodex):  g.codex.Load(),
	}
}

// enterOverloadHold counts a request as held in same-account overload retry.
func (s *Server) enterOverloadHold(provider accounts.Provider) func() {
	if s == nil {
		return func() {}
	}
	return s.overloadHeld.enter(provider)
}

// overloadRetryLog rate-limits the retry log of one request: its first
// retry, then about once a minute.
type overloadRetryLog struct {
	last time.Time
}

func (l *overloadRetryLog) due(now time.Time) bool {
	if l.last.IsZero() || now.Sub(l.last) >= overloadRetryLogEvery {
		l.last = now
		return true
	}
	return false
}
