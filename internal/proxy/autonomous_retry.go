package proxy

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
)

// AgentRetryPolicyHeader selects the lifetime and fallback policy for one
// pooled agent request. The sr Codex launcher sends "autonomous";
// clients that do not send the header retain the bounded server defaults.
// "bounded" is an explicit opt-out for launchers with a caller-selected cap.
const AgentRetryPolicyHeader = "X-Subrouter-Retry-Policy"

type agentRetryPolicy uint8

const (
	agentRetryBounded agentRetryPolicy = iota
	agentRetryAutonomous
)

func agentRetryPolicyFor(r *http.Request) agentRetryPolicy {
	if r == nil {
		return agentRetryBounded
	}
	switch strings.ToLower(strings.TrimSpace(r.Header.Get(AgentRetryPolicyHeader))) {
	case "autonomous":
		return agentRetryAutonomous
	case "":
	default:
		return agentRetryBounded
	}
	// Older sr codex launchers never auto-update and still mark their
	// requests capacity-retryable instead. Give them the same silent retry so
	// a capacity turn is not handed back to Codex as a visible "continue".
	if r.Header.Get(CodexCapacityRetryableHeader) == "1" {
		return agentRetryAutonomous
	}
	return agentRetryBounded
}

func (p agentRetryPolicy) autonomous() bool { return p == agentRetryAutonomous }

// autonomousRetryInScope limits the wrapper policy to locally selected pooled
// traffic. Forced accounts and centrally brokered/bound credentials retain
// their existing ownership and retry contracts; X-Subrouter-No-Retry always
// wins for deployment canaries.
func autonomousRetryInScope(policy agentRetryPolicy, noRetry, forcedAccount, externallyBound bool) bool {
	return policy.autonomous() && !noRetry && !forcedAccount && !externallyBound
}

// autonomousAgentRetryTransport repeats a complete same-provider pool pass
// after a transient response. The inner transports retain responsibility for
// account rotation and safe stream peeking; this outer loop only sees failures
// that they have determined can be replayed before client-visible output.
// There is no attempt limit: cancellation is the bound.
type autonomousAgentRetryTransport struct {
	base     http.RoundTripper
	server   *Server
	logger   *slog.Logger
	provider accounts.Provider
	model    string
	agent    string
	session  string
	account  string
	// budget is shared by the nested retry layers for one pool pass. Each
	// autonomous pass replenishes the same bounded allowance, preserving the
	// no-multiplication guarantee inside the pass.
	budget         *attemptBudget
	retriesPerPass int

	// Test seam; nil uses sleepForRetry.
	sleep func(context.Context, time.Duration) bool
	now   func() time.Time
}

func (t autonomousAgentRetryTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	attemptReq := req
	currentAccount := t.account
	var releaseRetryWait func()
	defer func() {
		if releaseRetryWait != nil {
			releaseRetryWait()
		}
	}()
	for attempt := 1; ; attempt++ {
		if attempt > 1 {
			t.budget.replenish(t.retriesPerPass)
		}
		response, err := base.RoundTrip(attemptReq)
		nextTemplate := attemptReq
		var routed accounts.Account
		preserveRoutedAttempt := false
		if actual, ok := routedResponseAccount(response); ok {
			routed = actual
			currentAccount = actual.ID
			// The inner pool pass may have rotated after quota/auth trouble.
			// Preserve that exact provider request (URL and auth) for the next
			// autonomous pass so a transient failure does not jump back to the
			// original account and throw away the prompt cache it just chose.
			if response.Request != nil && response.Request.URL != nil && response.Request.Method != "" {
				nextTemplate = response.Request
				preserveRoutedAttempt = true
			}
		} else if routedRequest, actual, ok := routedErrorAttempt(err); ok {
			routed = actual
			currentAccount = actual.ID
			nextTemplate = routedRequest
			preserveRoutedAttempt = true
		}
		retryResponse, replaced, responseReason := autonomousRetryableResponse(t.provider, response)
		response = replaced
		retry := retryResponse ||
			(err != nil && retryablePostTransportError(err))
		if !retry || req.GetBody == nil || req.Context().Err() != nil {
			return response, err
		}

		body, bodyErr := req.GetBody()
		if bodyErr != nil {
			return response, err
		}
		if response != nil && response.Body != nil {
			_ = response.Body.Close()
		}

		wait := autonomousRetryBackoff(attempt)
		if releaseRetryWait != nil {
			releaseRetryWait()
		}
		reason := responseReason
		if reason == "" {
			reason = autonomousRetryReason(response, err)
		}
		if t.server != nil {
			releaseRetryWait = t.server.beginRetryWait(t.agent, t.session, RetryStatus{
				Provider:    t.provider,
				Model:       t.model,
				AccountID:   currentAccount,
				Attempt:     attempt + 1,
				Reason:      reason,
				NextRetryAt: t.clock().Add(wait),
			})
		}
		if t.logger != nil {
			fields := []any{
				"provider", t.provider, "agent", t.agent, "session", t.session,
				"account", currentAccount, "attempt", attempt + 1, "next_in", wait.String(),
			}
			if response != nil {
				fields = append(fields, "status", response.StatusCode)
			} else {
				fields = append(fields, "error", err)
			}
			t.logger.Warn("autonomous agent request is retrying upstream", fields...)
		}
		if !t.sleepContext(req.Context(), wait) {
			return response, req.Context().Err()
		}
		if releaseRetryWait != nil {
			releaseRetryWait()
			releaseRetryWait = nil
		}

		nextContext := req.Context()
		if preserveRoutedAttempt {
			nextContext = withAttemptAccount(nextContext, routed)
		}
		attemptReq = nextTemplate.Clone(nextContext)
		attemptReq.Body = body
		attemptReq.GetBody = req.GetBody
		attemptReq.ContentLength = req.ContentLength
	}
}

func autonomousRetryReason(response *http.Response, err error) string {
	if response != nil {
		return fmt.Sprintf("http_%d", response.StatusCode)
	}
	if err != nil {
		return "transport_error"
	}
	return "retryable_failure"
}

func autonomousRetryableResponse(provider accounts.Provider, response *http.Response) (bool, *http.Response, string) {
	if response == nil {
		return false, response, ""
	}
	if response.StatusCode >= http.StatusOK && response.StatusCode < http.StatusMultipleChoices {
		switch provider {
		case accounts.ProviderClaude:
			if !claudeResponseRejected(response.Header) {
				if claudeStreamOverloaded(response) {
					return true, response, "stream_overloaded"
				}
			}
		case accounts.ProviderCodex:
			failed, reason, replaced := codexOverloadFailure(response)
			if failed {
				return true, replaced, reason
			}
			response = replaced
		}
	}
	if response.StatusCode == http.StatusTooManyRequests {
		switch provider {
		case accounts.ProviderClaude:
			// The inner pool pass already rotated through eligible accounts.
			// A final explicit rejection is quota, not transient overload.
			return !claudeResponseRejected(response.Header), response, ""
		case accounts.ProviderCodex:
			if peeked, ok := response.Body.(*codexPeekedBody); ok {
				return peeked.class != codexFailureQuota && peeked.class != codexFailureClient, response, "pool_status_429"
			}
			_, response = codexCapacityBody(response)
			if peeked, ok := response.Body.(*codexPeekedBody); ok {
				return peeked.class != codexFailureQuota && peeked.class != codexFailureClient, response, "pool_status_429"
			}
		}
		return true, response, ""
	}
	return response.StatusCode == http.StatusRequestTimeout ||
		response.StatusCode >= http.StatusInternalServerError, response, ""
}

func autonomousRetryBackoff(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	wait := time.Second << min(attempt-1, 4)
	return min(wait, 15*time.Second)
}

func (t autonomousAgentRetryTransport) sleepContext(ctx context.Context, wait time.Duration) bool {
	if t.sleep != nil {
		return t.sleep(ctx, wait)
	}
	return sleepForRetry(ctx, wait)
}

func (t autonomousAgentRetryTransport) clock() time.Time {
	if t.now != nil {
		return t.now()
	}
	return time.Now()
}
