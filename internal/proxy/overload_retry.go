package proxy

import (
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

// ClaudeOverloadRetryConfig shapes the same-account Claude overload ladder.
// A nil config uses the defaults.
type ClaudeOverloadRetryConfig struct {
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
