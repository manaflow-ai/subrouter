package accounts

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const codexUsageURL = "https://chatgpt.com/backend-api/wham/usage"

type UsageWindow struct {
	Name               string
	UsedPercent        float64
	LimitWindowSeconds int64
	// ResetAfterSeconds is measured from when the usage was fetched. It is
	// kept for older clients; newer ones read ResetAt.
	ResetAfterSeconds int64
	// ResetAt is the absolute reset time, taken from the provider when it
	// reports one, else fetch time plus ResetAfterSeconds. Zero from servers
	// that predate it; see ResetTime.
	ResetAt time.Time `json:"reset_at,omitzero"`
	// Feature is the upstream limit_name of the additional (per-model) rate
	// limit this window belongs to, e.g. "GPT-5.3-Codex-Spark". Empty for the
	// account-wide primary/secondary windows. Used to route a request to its
	// model-specific quota pool without matching on display strings.
	Feature string
	// ExtraUsage carries Claude's paid-usage allowance on the synthetic
	// "extra" window. It is status and fallback-routing metadata, not a
	// subscription quota window, so scoring code must exclude it.
	ExtraUsage *ExtraUsageInfo `json:"extra_usage,omitempty"`
}

// ExtraUsageInfo describes Claude's optional paid usage budget. Anthropic
// reports MonthlyLimit and UsedCredits in US cents; Utilization is percent.
// AutoReload reports Anthropic's auto-reload toggle (null from the API reads
// as off, matching the Claude settings page). DisabledReason, CreditsBalance,
// and AutoReload are display metadata only — routing stays with Remaining.
// CreditsBalance is the prepaid credit remainder in cents; the OAuth usage
// API has only ever returned null for it, so `sr status` fills it locally
// from the claude.ai web session API (see sr_claude_balance.go).
// ResetTime returns when the window resets: ResetAt when set, else
// fetchedAt plus ResetAfterSeconds for payloads from older servers. It is
// zero when the reset is unknown.
func (w UsageWindow) ResetTime(fetchedAt time.Time) time.Time {
	if !w.ResetAt.IsZero() {
		return w.ResetAt
	}
	if w.ResetAfterSeconds <= 0 || fetchedAt.IsZero() {
		return time.Time{}
	}
	return fetchedAt.Add(time.Duration(w.ResetAfterSeconds) * time.Second)
}

// ResetsAsOf returns a copy of windows whose ResetAfterSeconds is recomputed
// from ResetAt as of now, so relative displays do not lag by the age of the
// reading. Windows without ResetAt keep their ResetAfterSeconds.
func ResetsAsOf(windows []UsageWindow, now time.Time) []UsageWindow {
	if windows == nil {
		return nil
	}
	out := append([]UsageWindow(nil), windows...)
	for i := range out {
		if out[i].ResetAt.IsZero() {
			continue
		}
		out[i].ResetAfterSeconds = max(0, int64(out[i].ResetAt.Sub(now).Seconds()))
	}
	return out
}

type ExtraUsageInfo struct {
	// EnablementUnknown marks display-only balance records without OAuth settings.
	EnablementUnknown bool     `json:"enablement_unknown,omitempty"`
	IsEnabled         bool     `json:"is_enabled"`
	MonthlyLimit      *float64 `json:"monthly_limit,omitempty"`
	UsedCredits       *float64 `json:"used_credits,omitempty"`
	Utilization       *float64 `json:"utilization,omitempty"`
	// DisabledReason is Anthropic's machine reason when IsEnabled is false,
	// e.g. "out_of_credits".
	DisabledReason string `json:"disabled_reason,omitempty"`
	// CreditsBalance is the remaining prepaid credit balance in cents.
	CreditsBalance *float64 `json:"credits_balance,omitempty"`
	AutoReload     *bool    `json:"auto_reload,omitempty"`
}

// Remaining reports the known positive balance. Both the configured limit and
// used amount must be present: unknown balance must never authorize paid use.
func (e *ExtraUsageInfo) Remaining() (float64, bool) {
	if e == nil || e.MonthlyLimit == nil || e.UsedCredits == nil || *e.MonthlyLimit < 0 || *e.UsedCredits < 0 {
		return 0, false
	}
	remaining := *e.MonthlyLimit - *e.UsedCredits
	if remaining < 0 {
		remaining = 0
	}
	return remaining, true
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
	HasCredits bool
	Unlimited  bool
	Balance    string
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
	// A missing or null usage value is unknown, not an unused quota window.
	// Keep this private so programmatically constructed windows retain their
	// existing meaning, including an explicitly supplied zero percent.
	unreportedUsedPercent bool
}

func (w *codexLimitWindow) UnmarshalJSON(data []byte) error {
	type alias codexLimitWindow
	var decoded struct {
		alias
		UsedPercent *float64 `json:"used_percent"`
	}
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	*w = codexLimitWindow(decoded.alias)
	w.unreportedUsedPercent = decoded.UsedPercent == nil
	if decoded.UsedPercent != nil {
		w.UsedPercent = *decoded.UsedPercent
	}
	return nil
}

func (w *codexLimitWindow) usageKnown() bool {
	return w != nil && !w.unreportedUsedPercent &&
		!math.IsNaN(w.UsedPercent) && !math.IsInf(w.UsedPercent, 0) &&
		w.UsedPercent >= 0 && w.UsedPercent <= 100
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
	if !usage.RateLimit.LimitReached {
		if (usage.RateLimit.PrimaryWindow != nil && !usage.RateLimit.PrimaryWindow.usageKnown()) ||
			(usage.RateLimit.SecondaryWindow != nil && !usage.RateLimit.SecondaryWindow.usageKnown()) ||
			(usage.RateLimit.PrimaryWindow == nil && usage.RateLimit.SecondaryWindow == nil) {
			return CodexUsageDetails{}, fmt.Errorf("Codex usage quota is unknown: account-wide utilization was not reported")
		}
	}
	details := CodexUsageDetails{
		PlanType:           usage.PlanType,
		Windows:            usage.displayWindows(),
		BaseWindows:        usage.windows(),
		ComplimentaryReset: usage.ComplimentaryReset,
		RawRateLimit:       usage.RateLimit,
	}
	logCodexUsageShapeOnce(usage.RateLimit)
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
	now := time.Now()
	var windows []UsageWindow
	appendDetails := func(prefix string, details codexRateLimitDetails) {
		if details.PrimaryWindow.usageKnown() {
			windows = append(windows, UsageWindow{
				Name:               prefix + "primary",
				UsedPercent:        details.PrimaryWindow.UsedPercent,
				LimitWindowSeconds: details.PrimaryWindow.LimitWindowSeconds,
				ResetAfterSeconds:  details.PrimaryWindow.resetAfterSeconds(now),
				ResetAt:            details.PrimaryWindow.resetAt(now),
			})
		}
		if details.SecondaryWindow.usageKnown() {
			windows = append(windows, UsageWindow{
				Name:               prefix + "secondary",
				UsedPercent:        details.SecondaryWindow.UsedPercent,
				LimitWindowSeconds: details.SecondaryWindow.LimitWindowSeconds,
				ResetAfterSeconds:  details.SecondaryWindow.resetAfterSeconds(now),
				ResetAt:            details.SecondaryWindow.resetAt(now),
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
	now := time.Now()
	appendDetails := func(prefix, feature string, details codexRateLimitDetails) {
		if details.PrimaryWindow.usageKnown() {
			windows = append(windows, UsageWindow{
				Name:               prefix + "primary",
				Feature:            feature,
				UsedPercent:        details.PrimaryWindow.UsedPercent,
				LimitWindowSeconds: details.PrimaryWindow.LimitWindowSeconds,
				ResetAfterSeconds:  details.PrimaryWindow.resetAfterSeconds(now),
				ResetAt:            details.PrimaryWindow.resetAt(now),
			})
		}
		if details.SecondaryWindow.usageKnown() {
			windows = append(windows, UsageWindow{
				Name:               prefix + "secondary",
				Feature:            feature,
				UsedPercent:        details.SecondaryWindow.UsedPercent,
				LimitWindowSeconds: details.SecondaryWindow.LimitWindowSeconds,
				ResetAfterSeconds:  details.SecondaryWindow.resetAfterSeconds(now),
				ResetAt:            details.SecondaryWindow.resetAt(now),
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

// resetAt prefers the provider's absolute reset_at and otherwise anchors
// reset_after_seconds to now, the fetch time.
func (w codexLimitWindow) resetAt(now time.Time) time.Time {
	if w.ResetAt > 0 {
		return time.Unix(w.ResetAt, 0)
	}
	if w.ResetAfterSeconds > 0 {
		return now.Add(time.Duration(w.ResetAfterSeconds) * time.Second)
	}
	return time.Time{}
}

func (w codexLimitWindow) resetAfterSeconds(now time.Time) int64 {
	if w.ResetAfterSeconds > 0 {
		return w.ResetAfterSeconds
	}
	if w.ResetAt <= 0 {
		return 0
	}
	remaining := w.ResetAt - now.Unix()
	if remaining < 0 {
		return 0
	}
	return remaining
}

// WeeklyLimitCooked reports whether an account is blocked by its account-wide
// weekly rate-limit window. See WeeklyCookedWindow for the rule.
func WeeklyLimitCooked(details CodexUsageDetails) bool {
	_, cooked := WeeklyCookedWindow(codexUsageResponse{RateLimit: details.RawRateLimit}.windows())
	return cooked
}

// seenCodexUsageShapes records which rate-limit layouts this process has
// already logged, so each distinct layout is logged once.
var seenCodexUsageShapes sync.Map

// logCodexUsageShapeOnce logs the layout of a Codex usage response the first
// time this process sees it. Upstream moved the weekly window from
// secondary_window to primary_window for some accounts without notice; a new
// layout showing up in the log is the early signal for the next such change.
func logCodexUsageShapeOnce(rl codexRateLimitDetails) {
	shape := codexUsageShape(rl)
	if _, loaded := seenCodexUsageShapes.LoadOrStore(shape, struct{}{}); loaded {
		return
	}
	slog.Info("codex usage response layout observed", "layout", shape)
}

func codexUsageShape(rl codexRateLimitDetails) string {
	slot := func(w *codexLimitWindow) string {
		if w == nil {
			return "none"
		}
		if w.LimitWindowSeconds <= 0 {
			return "unknown"
		}
		return strconv.FormatInt(w.LimitWindowSeconds, 10) + "s"
	}
	return "primary=" + slot(rl.PrimaryWindow) + " secondary=" + slot(rl.SecondaryWindow) +
		" limit_reached=" + strconv.FormatBool(rl.LimitReached)
}
