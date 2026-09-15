package accounts

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

const codexUsageURL = "https://chatgpt.com/backend-api/wham/usage"

type UsageWindow struct {
	Name               string
	UsedPercent        float64
	LimitWindowSeconds int64
	ResetAfterSeconds  int64
	// Feature is the upstream limit_name of the additional (per-model) rate
	// limit this window belongs to, e.g. "GPT-5.3-Codex-Spark". Empty for the
	// account-wide primary/secondary windows. Used to route a request to its
	// model-specific quota pool without matching on display strings.
	Feature string
}

type CodexUsageDetails struct {
	PlanType           string
	Windows            []UsageWindow
	BaseWindows        []UsageWindow
	Credits            *CreditsInfo
	ComplimentaryReset *ComplimentaryResetInfo
	RawRateLimit       codexRateLimitDetails
}

type CreditsInfo struct {
	HasCredits       bool
	Unlimited        bool
	Balance          string
	Limit            string
	Used             string
	LimitReset       string
	AutoTopUpKnown   bool
	AutoTopUpEnabled bool
}

type ComplimentaryResetInfo struct {
	Known     bool   `json:"known"`
	Available bool   `json:"available,omitempty"`
	Consumed  bool   `json:"consumed,omitempty"`
	Eligible  *bool  `json:"eligible,omitempty"`
	Remaining *int   `json:"remaining,omitempty"`
	Used      *int   `json:"used,omitempty"`
	Total     *int   `json:"total,omitempty"`
	Status    string `json:"status,omitempty"`
	ResetsAt  string `json:"resets_at,omitempty"`
	Source    string `json:"source,omitempty"`
}

type codexUsageResponse struct {
	PlanType             string                     `json:"plan_type"`
	RateLimit            codexRateLimitDetails      `json:"rate_limit"`
	Credits              *codexCreditsInfo          `json:"credits"`
	AdditionalRateLimits []codexAdditionalRateLimit `json:"additional_rate_limits"`
	ComplimentaryReset   *ComplimentaryResetInfo    `json:"-"`
}

type codexRateLimitDetails struct {
	Allowed         bool              `json:"allowed"`
	LimitReached    bool              `json:"limit_reached"`
	PrimaryWindow   *codexLimitWindow `json:"primary_window"`
	SecondaryWindow *codexLimitWindow `json:"secondary_window"`
}

type codexLimitWindow struct {
	UsedPercent        float64 `json:"used_percent"`
	LimitWindowSeconds int64   `json:"limit_window_seconds"`
	ResetAfterSeconds  int64   `json:"reset_after_seconds"`
	ResetAt            int64   `json:"reset_at"`
}

type codexCreditsInfo struct {
	HasCredits bool   `json:"has_credits"`
	Unlimited  bool   `json:"unlimited"`
	Balance    string `json:"balance"`
}

type codexAdditionalRateLimit struct {
	MeteredFeature string                `json:"metered_feature"`
	LimitName      string                `json:"limit_name"`
	RateLimit      codexRateLimitDetails `json:"rate_limit"`
}

func (u *codexUsageResponse) UnmarshalJSON(data []byte) error {
	type alias codexUsageResponse
	var decoded alias
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	*u = codexUsageResponse(decoded)
	u.ComplimentaryReset = parseComplimentaryResetInfo(data)
	return nil
}

func FetchCodexUsage(ctx context.Context, client *http.Client, account Account) ([]UsageWindow, error) {
	details, err := FetchCodexUsageDetails(ctx, client, account)
	if err != nil {
		return nil, err
	}
	return details.Windows, nil
}

func FetchCodexUsageDetails(ctx context.Context, client *http.Client, account Account) (CodexUsageDetails, error) {
	if account.AuthMode != AuthModeOAuth {
		return CodexUsageDetails{}, fmt.Errorf("usage is only available for OAuth accounts")
	}
	if account.Token == "" {
		return CodexUsageDetails{}, fmt.Errorf("account has no access token")
	}
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, codexUsageURL, nil)
	if err != nil {
		return CodexUsageDetails{}, err
	}
	req.Header.Set("Authorization", account.AuthorizationHeader())
	if account.AccountID != "" {
		req.Header.Set("ChatGPT-Account-ID", account.AccountID)
	}

	res, err := client.Do(req)
	if err != nil {
		return CodexUsageDetails{}, err
	}
	defer res.Body.Close()

	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return CodexUsageDetails{}, fmt.Errorf("usage fetch failed: %s", res.Status)
	}

	var usage codexUsageResponse
	if err := json.NewDecoder(res.Body).Decode(&usage); err != nil {
		return CodexUsageDetails{}, err
	}
	details := CodexUsageDetails{
		PlanType:           usage.PlanType,
		Windows:            usage.displayWindows(),
		BaseWindows:        usage.windows(),
		ComplimentaryReset: usage.ComplimentaryReset,
		RawRateLimit:       usage.RateLimit,
	}
	if usage.Credits != nil {
		details.Credits = &CreditsInfo{
			HasCredits: usage.Credits.HasCredits,
			Unlimited:  usage.Credits.Unlimited,
			Balance:    usage.Credits.Balance,
		}
	}
	return details, nil
}

func parseComplimentaryResetInfo(data []byte) *ComplimentaryResetInfo {
	var root map[string]any
	if err := json.Unmarshal(data, &root); err != nil {
		return nil
	}
	return findComplimentaryResetInfo(root, "")
}

func findComplimentaryResetInfo(value any, path string) *ComplimentaryResetInfo {
	object, ok := value.(map[string]any)
	if !ok {
		return nil
	}
	for key, child := range object {
		childPath := key
		if path != "" {
			childPath = path + "." + key
		}
		if isComplimentaryResetKey(key) {
			if info := parseComplimentaryResetCandidate(child, childPath); info != nil {
				return info
			}
		}
	}
	for key, child := range object {
		if isRateLimitResetKey(key) {
			continue
		}
		childPath := key
		if path != "" {
			childPath = path + "." + key
		}
		if info := findComplimentaryResetInfo(child, childPath); info != nil {
			return info
		}
	}
	return nil
}

func isComplimentaryResetKey(key string) bool {
	normalized := normalizeUsageKey(key)
	return (strings.Contains(normalized, "complimentary") && strings.Contains(normalized, "reset")) ||
		(strings.Contains(normalized, "onetime") && strings.Contains(normalized, "reset")) ||
		strings.Contains(normalized, "resetcredit")
}

func isRateLimitResetKey(key string) bool {
	normalized := normalizeUsageKey(key)
	return normalized == "resetafterseconds" || normalized == "resetat" || normalized == "resetsat"
}

func normalizeUsageKey(value string) string {
	var b strings.Builder
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z':
			b.WriteRune(r)
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r + ('a' - 'A'))
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		}
	}
	return b.String()
}

func parseComplimentaryResetCandidate(value any, source string) *ComplimentaryResetInfo {
	info := &ComplimentaryResetInfo{Known: true, Source: source}
	switch typed := value.(type) {
	case bool:
		normalizedSource := normalizeUsageKey(source)
		if strings.Contains(normalizedSource, "used") ||
			strings.Contains(normalizedSource, "consumed") ||
			strings.Contains(normalizedSource, "redeemed") {
			info.Consumed = typed
			info.Available = !typed
		} else {
			info.Available = typed
			info.Consumed = !typed
		}
	case string:
		applyComplimentaryResetStatus(info, typed)
	case map[string]any:
		applyComplimentaryResetObject(info, typed)
	default:
		return nil
	}
	if info.Remaining != nil {
		info.Available = *info.Remaining > 0
		if *info.Remaining == 0 {
			info.Consumed = true
		}
	}
	if info.Total != nil && info.Used != nil && *info.Total > 0 {
		info.Consumed = *info.Used >= *info.Total
		info.Available = *info.Used < *info.Total
	}
	return info
}

func applyComplimentaryResetObject(info *ComplimentaryResetInfo, object map[string]any) {
	if value, ok := stringField(object, "status", "state"); ok {
		info.Status = value
		applyComplimentaryResetStatus(info, value)
	}
	if value, ok := boolField(object, "available", "is_available", "can_reset", "can_use", "claimable"); ok {
		info.Available = value
	}
	if value, ok := boolField(object, "consumed", "is_consumed", "used", "is_used", "redeemed", "is_redeemed"); ok {
		info.Consumed = value
	}
	if value, ok := boolField(object, "eligible", "is_eligible"); ok {
		info.Eligible = &value
		if !value {
			info.Available = false
		}
	}
	if value, ok := intField(object, "remaining", "remaining_count", "available_count"); ok {
		info.Remaining = &value
	}
	if value, ok := intField(object, "used_count", "uses", "redeemed_count"); ok {
		info.Used = &value
	}
	if value, ok := intField(object, "total", "total_count", "limit"); ok {
		info.Total = &value
	}
	if value, ok := stringField(object, "resets_at", "reset_at", "expires_at"); ok {
		info.ResetsAt = value
	}
}

func applyComplimentaryResetStatus(info *ComplimentaryResetInfo, status string) {
	normalized := normalizeUsageKey(status)
	switch normalized {
	case "available", "eligible", "unused", "unclaimed", "claimable":
		info.Available = true
		info.Consumed = false
	case "consumed", "used", "redeemed", "claimed":
		info.Consumed = true
		info.Available = false
	case "ineligible", "unavailable", "disabled":
		value := false
		info.Eligible = &value
		info.Available = false
	}
}

func boolField(object map[string]any, names ...string) (bool, bool) {
	for _, name := range names {
		value, ok := object[name]
		if !ok {
			continue
		}
		if parsed, ok := value.(bool); ok {
			return parsed, true
		}
	}
	return false, false
}

func intField(object map[string]any, names ...string) (int, bool) {
	for _, name := range names {
		value, ok := object[name]
		if !ok {
			continue
		}
		switch parsed := value.(type) {
		case float64:
			return int(parsed), true
		case int:
			return parsed, true
		}
	}
	return 0, false
}

func stringField(object map[string]any, names ...string) (string, bool) {
	for _, name := range names {
		value, ok := object[name]
		if !ok {
			continue
		}
		if parsed, ok := value.(string); ok {
			return parsed, true
		}
	}
	return "", false
}

func (u codexUsageResponse) windows() []UsageWindow {
	var windows []UsageWindow
	appendDetails := func(prefix string, details codexRateLimitDetails) {
		if details.PrimaryWindow != nil {
			windows = append(windows, UsageWindow{
				Name:               prefix + "primary",
				UsedPercent:        details.PrimaryWindow.UsedPercent,
				LimitWindowSeconds: details.PrimaryWindow.LimitWindowSeconds,
				ResetAfterSeconds:  details.PrimaryWindow.resetAfterSeconds(),
			})
		}
		if details.SecondaryWindow != nil {
			windows = append(windows, UsageWindow{
				Name:               prefix + "secondary",
				UsedPercent:        details.SecondaryWindow.UsedPercent,
				LimitWindowSeconds: details.SecondaryWindow.LimitWindowSeconds,
				ResetAfterSeconds:  details.SecondaryWindow.resetAfterSeconds(),
			})
		}
		if details.LimitReached {
			windows = append(windows, UsageWindow{Name: prefix + "reached", UsedPercent: 100})
		}
	}

	appendDetails("", u.RateLimit)
	return windows
}

func (u codexUsageResponse) displayWindows() []UsageWindow {
	windows := u.windows()
	appendDetails := func(prefix, feature string, details codexRateLimitDetails) {
		if details.PrimaryWindow != nil {
			windows = append(windows, UsageWindow{
				Name:               prefix + "primary",
				Feature:            feature,
				UsedPercent:        details.PrimaryWindow.UsedPercent,
				LimitWindowSeconds: details.PrimaryWindow.LimitWindowSeconds,
				ResetAfterSeconds:  details.PrimaryWindow.resetAfterSeconds(),
			})
		}
		if details.SecondaryWindow != nil {
			windows = append(windows, UsageWindow{
				Name:               prefix + "secondary",
				Feature:            feature,
				UsedPercent:        details.SecondaryWindow.UsedPercent,
				LimitWindowSeconds: details.SecondaryWindow.LimitWindowSeconds,
				ResetAfterSeconds:  details.SecondaryWindow.resetAfterSeconds(),
			})
		}
		if details.LimitReached {
			windows = append(windows, UsageWindow{Name: prefix + "reached", Feature: feature, UsedPercent: 100})
		}
	}
	for _, additional := range u.AdditionalRateLimits {
		name := additional.LimitName
		if name == "" {
			name = additional.MeteredFeature
		}
		if name == "" {
			name = "unknown"
		}
		appendDetails(name+"/", name, additional.RateLimit)
	}
	return windows
}

func (w codexLimitWindow) resetAfterSeconds() int64 {
	if w.ResetAfterSeconds > 0 {
		return w.ResetAfterSeconds
	}
	if w.ResetAt <= 0 {
		return 0
	}
	remaining := w.ResetAt - time.Now().Unix()
	if remaining < 0 {
		return 0
	}
	return remaining
}
