package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
)

// Anthropic bills a subscription OAuth credential used by a client that is
// not Claude Code (Pi, OpenCode and the like) from the account's extra usage
// instead of its plan limits, and refuses the request with a 400 whose message
// starts "Third-party apps now draw from your extra usage, not your plan
// limits...". The refusal is about the client, not the account: every other
// OAuth account answers the same way, and the account that answered stays
// healthy for Claude Code traffic. So subrouter never fails such a request
// over to another OAuth account and never marks or cools the account. An API
// key is billed per token whatever the client is, so the request moves to an
// untried Claude API-key account when one can take it; otherwise the client
// gets one clear, non-retryable error that names the cause.
//
// subrouter never disguises a client as Claude Code (headers, user agent,
// system prompt or anything else) to avoid this; routing it to an API key, or
// telling the user to add one, is the only remedy.

// claudeThirdPartyExtraUsageReply is the message of subrouter's own answer.
const claudeThirdPartyExtraUsageReply = "subrouter: this client isn't Claude Code, so Anthropic bills its subscription (OAuth) requests from extra usage instead of plan limits, and refused this one. Retrying on another subscription account fails the same way. Add a Claude API-key account to subrouter so it can route this client there, or use Claude Code."

// claudeThirdPartyExtraUsageMessage matches Anthropic's refusal of a
// non-Claude-Code client on a subscription credential. It keys on the stable
// phrases ("third-party app" and "extra usage") rather than the whole
// sentence, so a reworded refusal still matches.
func claudeThirdPartyExtraUsageMessage(body []byte) bool {
	message := normalizedProviderErrorMessage(body)
	if message == "" {
		return false
	}
	thirdParty := strings.Contains(message, "third-party app") || strings.Contains(message, "third party app")
	return thirdParty && strings.Contains(message, "extra usage")
}

func claudeThirdPartyExtraUsageStatus(code int) bool {
	return code == http.StatusBadRequest || code == http.StatusPaymentRequired || code == http.StatusForbidden
}

// responseClaudeThirdPartyExtraUsage reports whether an Anthropic error
// response is the third-party extra-usage refusal. It reads at most
// usageLimitInspectMaxBytes and always leaves the body readable for the
// caller. Successful responses (including streams) are never read.
func responseClaudeThirdPartyExtraUsage(response *http.Response) (bool, error) {
	if response == nil || response.Body == nil || !claudeThirdPartyExtraUsageStatus(response.StatusCode) {
		return false, nil
	}
	body := response.Body
	prefix, err := io.ReadAll(io.LimitReader(body, usageLimitInspectMaxBytes+1))
	if err != nil || int64(len(prefix)) > usageLimitInspectMaxBytes {
		response.Body = prefixReadCloser{Reader: io.MultiReader(bytes.NewReader(prefix), body), Closer: body}
		return false, err
	}
	closeErr := body.Close()
	response.Body = io.NopCloser(bytes.NewReader(prefix))
	if closeErr != nil {
		return false, closeErr
	}
	return claudeThirdPartyExtraUsageMessage(decodedUpstreamBody(prefix, response.Header)), nil
}

// claudeAPIKeyRetryCandidate picks an untried Claude API-key account that can
// still take a request (not exhausted, not held out by the controller).
// Subscription accounts are never returned.
func (s Server) claudeAPIKeyRetryCandidate(ctx context.Context, poolModel string, tried map[string]struct{}) (accounts.Account, bool) {
	scheduler := s.scheduler().ForModel(poolModel)
	now := time.Now()
	var keys []accounts.Account
	for _, candidate := range filterAccountsForProvider(s.accountListContext(ctx), accounts.ProviderClaude) {
		if candidate.AuthMode != accounts.AuthModeAPIKey {
			continue
		}
		if _, done := tried[candidate.ID]; done {
			continue
		}
		provider := schedulerAccountProvider(candidate.Provider)
		if scheduler.Exhausted(provider, candidate.ID) {
			continue
		}
		if s.SchedulerRef != nil {
			if _, blocked := s.SchedulerRef.ExplicitBlockedUntilFor(provider, candidate.ID, poolModel, now); blocked {
				continue
			}
		}
		keys = append(keys, candidate)
	}
	if len(keys) == 0 {
		return accounts.Account{}, false
	}
	picked, err := pickRoutingAccount(scheduler, keys)
	if err != nil {
		return accounts.Account{}, false
	}
	return picked, true
}

// claudeThirdPartyExtraUsageResponse replaces the upstream refusal with
// subrouter's own explanation, in Anthropic's error shape and with the
// upstream status, marked non-retryable so SDK clients do not loop on it.
func claudeThirdPartyExtraUsageResponse(upstream *http.Response, req *http.Request) *http.Response {
	status := http.StatusBadRequest
	if upstream != nil && upstream.StatusCode >= 400 && upstream.StatusCode < 500 {
		status = upstream.StatusCode
	}
	if upstream != nil && upstream.Request != nil {
		// Keep the upstream request: it carries the routed-account tag.
		req = upstream.Request
	}
	if upstream != nil && upstream.Body != nil {
		_ = upstream.Body.Close()
	}
	body, _ := json.Marshal(map[string]any{
		"type": "error",
		"error": map[string]string{
			"type":    "invalid_request_error",
			"message": claudeThirdPartyExtraUsageReply,
		},
	})
	header := make(http.Header)
	header.Set("Content-Type", "application/json")
	header.Set("Content-Length", strconv.Itoa(len(body)))
	header.Set("X-Should-Retry", "false")
	header.Set("X-Subrouter-Error", "claude_third_party_extra_usage")
	return &http.Response{
		StatusCode:    status,
		Status:        strconv.Itoa(status) + " " + http.StatusText(status),
		Header:        header,
		Body:          io.NopCloser(bytes.NewReader(body)),
		ContentLength: int64(len(body)),
		Request:       req,
	}
}
