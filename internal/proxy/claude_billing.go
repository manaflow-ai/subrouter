package proxy

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/selectacct"
)

// claudeBillingRejected inspects only error envelopes, never successful content.
// The bounded prefix is restored so callers can still return the original error.
func claudeBillingRejected(response *http.Response) bool {
	if response == nil || response.Body == nil || (response.StatusCode != 400 && response.StatusCode != 402 && response.StatusCode != 403) {
		return false
	}
	body := response.Body
	prefix, err := io.ReadAll(io.LimitReader(body, usageLimitInspectMaxBytes+1))
	response.Body = prefixReadCloser{Reader: io.MultiReader(bytes.NewReader(prefix), body), Closer: body}
	if err != nil || int64(len(prefix)) > usageLimitInspectMaxBytes {
		return false
	}
	var envelope struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(prefix, &envelope) != nil {
		return false
	}
	message := strings.ToLower(strings.Join(strings.Fields(envelope.Error.Message), " "))
	return strings.Contains(message, "credit balance is too low") || strings.Contains(message, "credit balance too low") ||
		strings.Contains(message, "insufficient credits") || strings.Contains(message, "insufficient credit balance") ||
		envelope.Error.Type == "billing_error" || envelope.Error.Type == "insufficient_credit_error"
}

// claudeSubscriptionFirst removes known exhausted accounts, then keeps paid
// keys out of automatic routing whenever any subscription still has capacity.
// Explicit account selection returns before this policy is applied.
func claudeSubscriptionFirst(list []accounts.Account, scheduler selectacct.Scheduler) []accounts.Account {
	usable := make([]accounts.Account, 0, len(list))
	subscriptions := make([]accounts.Account, 0, len(list))
	for _, account := range list {
		if scheduler.Exhausted(account.Provider, account.ID) {
			continue
		}
		usable = append(usable, account)
		if account.AuthMode == accounts.AuthModeOAuth {
			subscriptions = append(subscriptions, account)
		}
	}
	if len(subscriptions) > 0 {
		return subscriptions
	}
	return usable
}
