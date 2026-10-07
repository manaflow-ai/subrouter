package proxy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	agentclaude "github.com/manaflow-ai/subrouter/internal/agents/claude"
)

// The ordinary OAuth usage endpoint is still checked at its existing cadence.
// Only the supplementary Messages probe is reused: its extra weekly windows
// change on a slower schedule, and a real quota rejection is handled on the
// inference path independently of this cache.
const claudeSupplementalProbeTTL = 10 * time.Minute

type claudeSupplementalUsage struct {
	windows []accounts.UsageWindow
	at      time.Time
}

func claudeSupplementalCacheKey(account accounts.Account) string {
	// A re-login or access-token rotation bypasses the prior probe result.
	digest := sha256.Sum256([]byte(account.Token))
	return string(account.Provider) + "\x00" + account.ID + "\x00" + hex.EncodeToString(digest[:])
}

func supplementalClaudeWindows(windows []accounts.UsageWindow) []accounts.UsageWindow {
	out := make([]accounts.UsageWindow, 0, len(windows))
	for _, window := range windows {
		// Only model-specific quota buckets belong in the supplemental cache.
		// Global 5h/7d and extra-usage balance must come from the current
		// ordinary status response; caching them would resurrect stale spend
		// or balance information on later incomplete responses.
		if window.Feature == "" {
			continue
		}
		out = append(out, window)
	}
	return out
}

// An authoritative exhausted model bucket needs no synthetic Messages probe
// before its reported reset. Healthy/unknown buckets keep a short freshness
// TTL because their utilization may continue changing.
func claudeSupplementalFreshEnough(cached claudeSupplementalUsage, now time.Time) bool {
	if now.Sub(cached.at) < claudeSupplementalProbeTTL {
		return true
	}
	if len(cached.windows) == 0 {
		return false
	}
	for _, window := range cached.windows {
		if window.UsedPercent < 100 || window.ResetAt.IsZero() || !now.Before(window.ResetAt) {
			return false
		}
	}
	return true
}

func (r *AccountRef) cachedClaudeSupplemental(account accounts.Account, now time.Time) ([]accounts.UsageWindow, bool) {
	r.usageWindowsMu.Lock()
	defer r.usageWindowsMu.Unlock()
	cached, ok := r.claudeSupplemental[claudeSupplementalCacheKey(account)]
	if !ok || !claudeSupplementalFreshEnough(cached, now) {
		return nil, false
	}
	for _, window := range cached.windows {
		if window.UsedPercent >= 100 && window.ResetAt.IsZero() {
			// Exhaustion with an unknown reset has no defensible cache lifetime.
			// Request fresh evidence at the next ordinary quota refresh.
			return nil, false
		}
		if !window.ResetAt.IsZero() && !now.Before(window.ResetAt) {
			// A quota reset invalidates previously observed utilization.
			return nil, false
		}
	}
	// A successful empty probe also counts; it prevents needless periodic
	// retries when Anthropic returned no supplementary headers.
	copyOfWindows := append([]accounts.UsageWindow(nil), cached.windows...)
	for i := range copyOfWindows {
		if !copyOfWindows[i].ResetAt.IsZero() {
			remaining := int64(copyOfWindows[i].ResetAt.Sub(now).Seconds())
			if remaining < 0 {
				remaining = 0
			}
			copyOfWindows[i].ResetAfterSeconds = remaining
		}
	}
	return copyOfWindows, true
}

func (r *AccountRef) rememberClaudeSupplemental(account accounts.Account, windows []accounts.UsageWindow) {
	supplemental := supplementalClaudeWindows(windows)
	for _, window := range supplemental {
		if window.UsedPercent >= 100 && window.ResetAt.IsZero() {
			// This authoritative result supersedes any older cached bucket.
			// Since its reset is unknown, keep no cache entry for this token.
			r.usageWindowsMu.Lock()
			delete(r.claudeSupplemental, claudeSupplementalCacheKey(account))
			r.usageWindowsMu.Unlock()
			return
		}
	}
	r.usageWindowsMu.Lock()
	defer r.usageWindowsMu.Unlock()
	if r.claudeSupplemental == nil {
		r.claudeSupplemental = map[string]claudeSupplementalUsage{}
	}
	now := time.Now()
	for key, value := range r.claudeSupplemental {
		if !claudeSupplementalFreshEnough(value, now) {
			delete(r.claudeSupplemental, key)
		}
	}
	r.claudeSupplemental[claudeSupplementalCacheKey(account)] = claudeSupplementalUsage{
		windows: append([]accounts.UsageWindow(nil), supplemental...),
		at:      now,
	}
}

// fetchClaudeUsageWindowsReusingSupplemental keeps normal usage checks fresh
// while avoiding a second synthetic Messages request at every cache expiry.
// It never delays inference, and keeps the preexisting error/fallback behavior
// if the ordinary usage endpoint fails.
func (r *AccountRef) fetchClaudeUsageWindowsReusingSupplemental(
	ctx context.Context, client *http.Client, account accounts.Account,
) ([]accounts.UsageWindow, error) {
	usage, err := agentclaude.FetchUsage(ctx, client, account.Token)
	windows := claudeUsageWindows(usage)
	if usageWindowNamed(windows, agentclaude.FableWindowName) {
		// The ordinary endpoint's model bucket is newer than our probe cache.
		// Save it so a later ordinary response that omits the bucket does not
		// resurrect older supplemental evidence.
		r.rememberClaudeSupplemental(account, windows)
		return windows, nil
	}

	// A successful primary fetch can use prior supplementary evidence without
	// another model request. On primary errors, leave the normal recovery
	// probe intact so its primary windows may recover a usable status.
	if err == nil && len(windows) > 0 {
		if supplemental, ok := r.cachedClaudeSupplemental(account, time.Now()); ok {
			return mergeUsageWindows(windows, supplemental), nil
		}
	}

	fableWindows, probeErr := agentclaude.FetchFableUsageWindows(ctx, client, account.Token)
	if probeErr == nil {
		if err == nil {
			r.rememberClaudeSupplemental(account, fableWindows)
		}
		if len(fableWindows) > 0 && (err == nil || fableProbeHasPrimaryWindows(fableWindows)) {
			windows = mergeUsageWindows(windows, fableWindows)
		}
	} else if err != nil {
		return nil, probeErr
	}
	if err != nil && len(windows) == 0 {
		return nil, err
	}
	return windows, nil
}
