package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
)

// Selection uses a provider family; quota observations belong to the selected
// backend. Existing saved account identities remain authoritative.
func (s Server) selectedAccountProvider(ctx context.Context, provider accounts.Provider, id string) accounts.Provider {
	if provider != accounts.ProviderQwen {
		return provider
	}
	for _, candidate := range s.accountListContext(ctx) {
		if candidate.ID == id && candidate.Provider.Family() == provider {
			return accountProviderFor(candidate.Provider)
		}
	}
	return provider
}

type qwenErrorEnvelope struct {
	Error struct {
		Code  string `json:"code"`
		Type  string `json:"type"`
		Quota struct {
			ResetAt int64 `json:"reset_at"`
		} `json:"quota"`
	} `json:"error"`
	Code string `json:"code"`
}

func qwenErrorFromResponse(response *http.Response) (qwenErrorEnvelope, error) {
	var payload qwenErrorEnvelope
	if response == nil || response.Body == nil || !responseStatusCanExhaust(response.StatusCode) {
		return payload, nil
	}
	body := response.Body
	prefix, err := io.ReadAll(io.LimitReader(body, usageLimitInspectMaxBytes+1))
	response.Body = prefixReadCloser{Reader: io.MultiReader(bytes.NewReader(prefix), body), Closer: body}
	if err != nil {
		return payload, err
	}
	if int64(len(prefix)) <= usageLimitInspectMaxBytes {
		if json.Unmarshal(prefix, &payload) != nil {
			return qwenErrorEnvelope{}, nil
		}
	}
	return payload, nil
}

// Only explicit model-entitlement failures may try another Qwen backend.
// Invalid parameters and context limits must reach the client unchanged.
func responseQwenModelCapabilityFailure(response *http.Response) (bool, error) {
	if response == nil || (response.StatusCode != 400 && response.StatusCode != 403 && response.StatusCode != 404) {
		return false, nil
	}
	payload, err := qwenErrorFromResponse(response)
	if err != nil {
		return false, err
	}
	for _, code := range []string{payload.Error.Code, payload.Error.Type, payload.Code} {
		switch strings.ToLower(code) {
		case "model_not_found", "model_not_supported", "unsupported_model", "accessdenied.unpurchased":
			return true, nil
		}
	}
	return false, nil
}

func (p qwenErrorEnvelope) quotaFailure() bool {
	switch strings.ToLower(p.Error.Code) {
	case "insufficient_quota", "quota_exceeded", "usage_limit_reached":
		return true
	}
	return false
}

// Qwen.ai reports exhausted subscription credits as 402 insufficient_quota.
// A billing status without this account-quota evidence is not safe to replay.
func responseQwenQuotaFailure(response *http.Response) (bool, error) {
	payload, err := qwenErrorFromResponse(response)
	return payload.quotaFailure(), err
}

func qwenQuotaExpiry(body []byte, now time.Time) (time.Time, bool) {
	var payload qwenErrorEnvelope
	if json.Unmarshal(body, &payload) != nil || !payload.quotaFailure() || payload.Error.Quota.ResetAt <= 0 {
		return time.Time{}, false
	}
	until := time.Unix(payload.Error.Quota.ResetAt, 0)
	if until.Before(now.Add(time.Minute)) {
		until = now.Add(time.Minute)
	}
	if until.After(now.Add(8 * 24 * time.Hour)) {
		until = now.Add(8 * 24 * time.Hour)
	}
	return until, true
}

func qwenQuotaFailureJSON(body []byte) bool {
	var payload qwenErrorEnvelope
	return json.Unmarshal(body, &payload) == nil && payload.quotaFailure()
}
