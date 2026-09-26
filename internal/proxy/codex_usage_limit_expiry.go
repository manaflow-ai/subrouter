package proxy

import (
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
)

// codexWorkspaceExhaustionTTL holds out an account whose usage_limit_reached
// carries a workspace-level reason (credits depleted, spend cap) but no reset
// time. Only a human refill or cap change recovers it, so re-probing every
// few minutes only re-delivers the same refusal; one probe per hour is enough
// for the account to rejoin once someone has acted.
const codexWorkspaceExhaustionTTL = time.Hour

// codexUsageLimitDetails is the subset of a Codex usage_limit_reached error
// that decides how long the account stays out: the provider's own reset time
// when it gives one, or the workspace-level reason when it does not.
type codexUsageLimitDetails struct {
	ResetsInSeconds      float64
	ResetsAt             time.Time
	RateLimitReachedType string
}

// codexUsageLimitExpiry reads a Codex usage_limit_reached payload (HTTP body
// or in-stream event, at top level, under error, or under response.error)
// and returns when the account should rejoin routing. ok is false when the
// payload is not a usage limit or carries nothing beyond the bare code, in
// which case the caller keeps its default hold-out. The result is clamped to
// [now+1m, now+8d] like the Claude header path.
func codexUsageLimitExpiry(body []byte, now time.Time) (time.Time, bool) {
	var event map[string]any
	if err := json.Unmarshal(body, &event); err != nil {
		return time.Time{}, false
	}
	// HTTP bodies name the limit in error.type; stream events name it in a
	// code under error or response.error. Accept either shape.
	if !usageLimitMap(event) && codexTurnFailureClass(body) != codexFailureQuota {
		return time.Time{}, false
	}
	details := codexUsageLimitDetailsFromMap(event)
	until := time.Time{}
	switch {
	case details.ResetsInSeconds > 0:
		until = now.Add(time.Duration(details.ResetsInSeconds * float64(time.Second)))
	case !details.ResetsAt.IsZero():
		until = details.ResetsAt
	case codexWorkspaceLimitType(details.RateLimitReachedType):
		until = now.Add(codexWorkspaceExhaustionTTL)
	default:
		return time.Time{}, false
	}
	if min := now.Add(time.Minute); until.Before(min) {
		until = min
	}
	if max := now.Add(8 * 24 * time.Hour); until.After(max) {
		until = max
	}
	return until, true
}

// codexWorkspaceLimitType reports a rate_limit_reached_type that names a
// workspace-level refusal rather than the user's own rolling window.
func codexWorkspaceLimitType(value string) bool {
	value = strings.ToLower(strings.TrimSpace(value))
	return value != "" && value != "rate_limit_reached" && strings.HasPrefix(value, "workspace_")
}

func codexUsageLimitDetailsFromMap(event map[string]any) codexUsageLimitDetails {
	var details codexUsageLimitDetails
	if seconds, ok := numberField(event, "resets_in_seconds"); ok && seconds > 0 {
		details.ResetsInSeconds = seconds
	}
	if at, ok := timeField(event, "resets_at"); ok {
		details.ResetsAt = at
	}
	if value := strings.TrimSpace(stringField(event, "rate_limit_reached_type")); value != "" {
		details.RateLimitReachedType = value
	}
	for _, key := range []string{"error", "response"} {
		nested, ok := event[key].(map[string]any)
		if !ok {
			continue
		}
		inner := codexUsageLimitDetailsFromMap(nested)
		if details.ResetsInSeconds == 0 {
			details.ResetsInSeconds = inner.ResetsInSeconds
		}
		if details.ResetsAt.IsZero() {
			details.ResetsAt = inner.ResetsAt
		}
		if details.RateLimitReachedType == "" {
			details.RateLimitReachedType = inner.RateLimitReachedType
		}
	}
	return details
}

func numberField(event map[string]any, key string) (float64, bool) {
	switch value := event[key].(type) {
	case float64:
		return value, true
	case string:
		parsed, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
		return parsed, err == nil
	}
	return 0, false
}

func timeField(event map[string]any, key string) (time.Time, bool) {
	switch value := event[key].(type) {
	case float64:
		if value <= 0 {
			return time.Time{}, false
		}
		return time.Unix(int64(value), 0), true
	case string:
		trimmed := strings.TrimSpace(value)
		if epoch, err := strconv.ParseInt(trimmed, 10, 64); err == nil && epoch > 0 {
			return time.Unix(epoch, 0), true
		}
		if parsed, err := time.Parse(time.RFC3339, trimmed); err == nil {
			return parsed, true
		}
	}
	return time.Time{}, false
}

// markCodexAccountExhaustedFromBody marks a Codex account out for as long as
// its usage-limit payload says, falling back to the default hold-out when the
// payload gives no reset or workspace reason. Other providers keep the
// default path unchanged.
func (s Server) markCodexAccountExhaustedFromBody(provider accounts.Provider, accountID, poolKey string, body []byte) {
	if s.SchedulerRef == nil {
		return
	}
	if provider == accounts.ProviderCodex {
		if until, ok := codexUsageLimitExpiry(body, time.Now()); ok {
			s.logCodexUsageLimitHoldout(accountID, poolKey, body, until)
			s.SchedulerRef.MarkExhaustedUntil(schedulerAccountProvider(provider), accountID, poolKey, until)
			return
		}
	}
	s.markAccountExhausted(provider, accountID, poolKey)
}

// markAccountExhaustedFromResponseBodyForAccount is the HTTP-path variant:
// a Codex usage-limit body with its own reset or workspace reason wins over
// the header-derived default; everything else keeps the response path.
func (s Server) markAccountExhaustedFromResponseBodyForAccount(account accounts.Account, poolKey string, status int, header map[string][]string, body []byte) {
	if s.SchedulerRef == nil {
		return
	}
	if account.Provider == accounts.ProviderCodex && status != 401 && status != 403 {
		if until, ok := codexUsageLimitExpiry(body, time.Now()); ok {
			s.logCodexUsageLimitHoldout(account.ID, poolKey, body, until)
			s.SchedulerRef.MarkExhaustedUntil(schedulerAccountProvider(account.Provider), account.ID, poolKey, until)
			return
		}
	}
	s.markAccountExhaustedFromResponseForAccount(account, poolKey, status, header)
}

func (s Server) logCodexUsageLimitHoldout(accountID, poolKey string, body []byte, until time.Time) {
	if s.Logger == nil {
		return
	}
	var event map[string]any
	_ = json.Unmarshal(body, &event)
	details := codexUsageLimitDetailsFromMap(event)
	s.Logger.Warn("codex account cooked by usage limit; holding it out",
		"account", accountID, "pool", poolKey,
		"rate_limit_reached_type", details.RateLimitReachedType,
		"until", until.UTC().Format(time.RFC3339), "hold", time.Until(until).Round(time.Second).String())
}
