package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
)

type ProviderKeyProbe struct {
	State           string
	Models          int
	QuotaStatus     string
	QuotaUsageKnown bool
	Windows         []accounts.UsageWindow
	Credits         *accounts.CreditsInfo
}

// ProbeProviderKey asks the provider's registry-declared health endpoint
// whether an API key is accepted. Quota visibility is deliberately separate:
// a valid inference key may not be authorized to read a membership quota API.
func ProbeProviderKey(ctx context.Context, client *http.Client, provider accounts.Provider, upstream, token string) (state string, models int) {
	probe := ProbeProviderKeyStatus(ctx, client, provider, upstream, token)
	return probe.State, probe.Models
}

// ProbeProviderKeyStatus verifies the inference credential and, when the
// provider exposes it at the same endpoint, returns key-scoped quota details.
func ProbeProviderKeyStatus(ctx context.Context, client *http.Client, provider accounts.Provider, upstream, token string) ProviderKeyProbe {
	probe := ProviderKeyProbe{Models: -1}
	url := ProviderHealthURL(provider, upstream)
	if url == "" {
		return probe
	}
	if client == nil {
		client = &http.Client{Timeout: 6 * time.Second}
	}
	// Never forward a provider credential to a redirect target. The caller may
	// supply a default client whose redirect policy otherwise preserves
	// Authorization on a same-host hop, including a scheme downgrade.
	probeClient := *client
	probeClient.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		probe.State = "unreachable"
		return probe
	}
	req.Header.Set("Authorization", "Bearer "+token)
	res, err := probeClient.Do(req)
	if err != nil {
		probe.State = "unreachable"
		return probe
	}
	defer func() { _ = res.Body.Close() }()
	switch {
	case res.StatusCode == http.StatusUnauthorized:
		probe.State = "bad key"
		return probe
	case res.StatusCode == http.StatusForbidden:
		probe.State = "denied"
		return probe
	case res.StatusCode < 200 || res.StatusCode >= 300:
		probe.State = fmt.Sprintf("http %d", res.StatusCode)
		return probe
	}
	probe.State = "auth ok"
	if provider == accounts.ProviderOpenRouter {
		body, readErr := io.ReadAll(io.LimitReader(res.Body, 1<<20))
		if readErr != nil {
			return probe
		}
		probe = decodeOpenRouterKeyProbe(probe, strings.NewReader(string(body)))
		// /key reports a per-key spending limit, not the account balance shown
		// by the OpenRouter dashboard. Fetch /credits for the actual remaining
		// pay-as-you-go balance; leave Credits unset if that optional telemetry
		// endpoint is unavailable rather than displaying the key limit as cash.
		if credits, ok := fetchOpenRouterCredits(ctx, probeClient, upstream, token); ok {
			if probe.Credits != nil {
				credits.Limit = probe.Credits.Limit
				credits.Used = probe.Credits.Used
				credits.LimitReset = probe.Credits.LimitReset
			}
			probe.Credits = credits
		} else if probe.Credits != nil {
			// /key filled Balance from limit_remaining, which is the key's
			// spending headroom and not account cash. With /credits
			// unavailable there is no account balance to report, so drop it
			// rather than let the key quota masquerade as a balance — that
			// conflation is exactly what this probe exists to remove. The key
			// limit, usage and reset stay: they are still true.
			probe.Credits.Balance = ""
			probe.Credits.HasCredits = probe.Credits.Limit != "" || probe.Credits.Used != ""
		}
		return probe
	}
	var payload struct {
		Data []struct{} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&payload); err != nil {
		return probe
	}
	probe.Models = len(payload.Data)
	return probe
}

func fetchOpenRouterCredits(ctx context.Context, client http.Client, upstream, token string) (*accounts.CreditsInfo, bool) {
	base, err := url.Parse(strings.TrimRight(upstream, "/"))
	if err != nil || base.Scheme == "" || base.Host == "" {
		return nil, false
	}
	base.Path = strings.TrimRight(base.Path, "/") + "/credits"
	base.RawQuery = ""
	base.Fragment = ""
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base.String(), nil)
	if err != nil {
		return nil, false
	}
	req.Header.Set("Authorization", "Bearer "+token)
	res, err := client.Do(req)
	if err != nil {
		return nil, false
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return nil, false
	}
	var payload struct {
		Data struct {
			TotalCredits *float64 `json:"total_credits"`
			TotalUsage   *float64 `json:"total_usage"`
		} `json:"data"`
	}
	if json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&payload) != nil ||
		payload.Data.TotalCredits == nil || payload.Data.TotalUsage == nil ||
		math.IsNaN(*payload.Data.TotalCredits) || math.IsInf(*payload.Data.TotalCredits, 0) ||
		math.IsNaN(*payload.Data.TotalUsage) || math.IsInf(*payload.Data.TotalUsage, 0) {
		return nil, false
	}
	remaining := *payload.Data.TotalCredits - *payload.Data.TotalUsage
	if remaining < 0 {
		remaining = 0
	}
	return &accounts.CreditsInfo{HasCredits: true, Balance: strconv.FormatFloat(remaining, 'f', -1, 64)}, true
}

func decodeOpenRouterKeyProbe(probe ProviderKeyProbe, body io.Reader) ProviderKeyProbe {
	var payload struct {
		Data struct {
			Limit          *float64 `json:"limit"`
			LimitRemaining *float64 `json:"limit_remaining"`
			LimitReset     *string  `json:"limit_reset"`
		} `json:"data"`
	}
	if json.NewDecoder(io.LimitReader(body, 1<<20)).Decode(&payload) != nil ||
		payload.Data.Limit == nil || payload.Data.LimitRemaining == nil ||
		*payload.Data.Limit < 0 || math.IsNaN(*payload.Data.Limit) || math.IsInf(*payload.Data.Limit, 0) ||
		math.IsNaN(*payload.Data.LimitRemaining) || math.IsInf(*payload.Data.LimitRemaining, 0) {
		return probe
	}
	limit := *payload.Data.Limit
	if limit == 0 && *payload.Data.LimitRemaining != 0 {
		return probe
	}
	remaining := math.Max(0, math.Min(limit, *payload.Data.LimitRemaining))
	usedPercent := 100.0
	if limit > 0 {
		usedPercent = 100 * (limit - remaining) / limit
	}
	cadence := "limit"
	windowSeconds := int64(0)
	if payload.Data.LimitReset != nil {
		switch strings.ToLower(strings.TrimSpace(*payload.Data.LimitReset)) {
		case "daily":
			cadence, windowSeconds = "daily", int64((24*time.Hour)/time.Second)
		case "weekly":
			cadence, windowSeconds = "weekly", int64((7*24*time.Hour)/time.Second)
		case "monthly":
			cadence, windowSeconds = "monthly", int64((30*24*time.Hour)/time.Second)
		}
	}
	probe.QuotaStatus = "live"
	if remaining == 0 {
		probe.QuotaStatus = "exhausted"
	}
	probe.QuotaUsageKnown = true
	probe.Windows = []accounts.UsageWindow{{Name: cadence, UsedPercent: usedPercent, LimitWindowSeconds: windowSeconds}}
	probe.Credits = &accounts.CreditsInfo{
		HasCredits: true,
		Balance:    strconv.FormatFloat(remaining, 'f', -1, 64),
		Limit:      strconv.FormatFloat(limit, 'f', -1, 64),
		Used:       strconv.FormatFloat(limit-remaining, 'f', -1, 64),
		LimitReset: cadence,
	}
	return probe
}
