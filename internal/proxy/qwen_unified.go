package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"

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

// Only explicit model-entitlement failures may try another Qwen backend.
// Invalid parameters and context limits must reach the client unchanged.
func responseQwenModelCapabilityFailure(response *http.Response) (bool, error) {
	if response == nil || response.Body == nil || (response.StatusCode != 400 && response.StatusCode != 403 && response.StatusCode != 404) {
		return false, nil
	}
	body := response.Body
	prefix, err := io.ReadAll(io.LimitReader(body, usageLimitInspectMaxBytes+1))
	response.Body = prefixReadCloser{Reader: io.MultiReader(bytes.NewReader(prefix), body), Closer: body}
	if err != nil {
		return false, err
	}
	if int64(len(prefix)) > usageLimitInspectMaxBytes {
		return false, nil
	}
	var payload struct {
		Error struct {
			Code string `json:"code"`
			Type string `json:"type"`
		} `json:"error"`
		Code string `json:"code"`
	}
	if json.Unmarshal(prefix, &payload) != nil {
		return false, nil
	}
	for _, code := range []string{payload.Error.Code, payload.Error.Type, payload.Code} {
		switch strings.ToLower(code) {
		case "model_not_found", "model_not_supported", "unsupported_model", "accessdenied.unpurchased":
			return true, nil
		}
	}
	return false, nil
}
