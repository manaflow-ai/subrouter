package kimi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/manaflow-ai/subrouter/account"
	"github.com/manaflow-ai/subrouter/internal/accounts"
)

const defaultUsageURL = "https://api.kimi.com/coding/v1/usages"

// usageURL is variable only so unit tests can point the store at an httptest
// server. Production always uses Kimi For Coding's managed usage endpoint.
var usageURL = defaultUsageURL

type usagePayload struct {
	Usage  quotaDetail            `json:"usage"`
	Usages map[string]ratioDetail `json:"usages"`
	Limits []struct {
		Window quotaWindow `json:"window"`
		Detail quotaDetail `json:"detail"`
	} `json:"limits"`
}

type ratioDetail struct {
	UsedRatio json.RawMessage `json:"used_ratio"`
	ResetTime string          `json:"reset_time"`
}

type quotaWindow struct {
	Duration int64  `json:"duration"`
	TimeUnit string `json:"timeUnit"`
}

type quotaDetail struct {
	Used      json.RawMessage `json:"used"`
	Remaining json.RawMessage `json:"remaining"`
	Limit     json.RawMessage `json:"limit"`
	ResetTime string          `json:"resetTime"`
}

// FetchUsage implements proxy.OAuthUsageSource. Kimi has shipped both
// {used, limit} and {remaining, limit} response shapes, so the parser accepts
// both instead of coupling sr status to one CLI release.
func (s Store) FetchUsage(ctx context.Context, client *http.Client, acct account.Account) (string, []accounts.UsageWindow, error) {
	if strings.TrimSpace(acct.Token) == "" {
		return "subscription", nil, fmt.Errorf("Kimi usage: no access token")
	}
	if client == nil {
		client = &http.Client{Timeout: 8 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, usageURL, nil)
	if err != nil {
		return "subscription", nil, err
	}
	req.Header.Set("Authorization", "Bearer "+acct.Token)
	req.Header.Set("Accept", "application/json")
	requestClient := *client
	requestClient.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	}
	res, err := requestClient.Do(req)
	if err != nil {
		return "subscription", nil, fmt.Errorf("Kimi usage: %w", err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		_, _ = io.CopyN(io.Discard, res.Body, 4096)
		return "subscription", nil, fmt.Errorf("Kimi usage returned HTTP %d", res.StatusCode)
	}
	var payload usagePayload
	if err := json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&payload); err != nil {
		return "subscription", nil, fmt.Errorf("decode Kimi usage: %w", err)
	}
	now := time.Now()
	windows := make([]accounts.UsageWindow, 0, len(payload.Limits)+1)
	if window, ok := kimiUsageWindow("weekly", int64((7*24*time.Hour)/time.Second), payload.Usage, now); ok {
		windows = append(windows, window)
	}
	for _, limit := range payload.Limits {
		seconds, label, ok := kimiWindowDuration(limit.Window)
		if !ok {
			continue
		}
		if window, ok := kimiUsageWindow(label, seconds, limit.Detail, now); ok {
			windows = append(windows, window)
		}
	}
	// New memberships expose ratios independently of the legacy rounded
	// quantities. Preserve both monthly pools and prefer the precise ratio
	// when the same session/weekly window appears in both response shapes.
	for _, definition := range []struct {
		key, name string
		seconds   int64
	}{
		{"limit_5h", "5h", 5 * 60 * 60},
		{"limit_7d", "weekly", 7 * 24 * 60 * 60},
		{"limit_month_total", "monthly", 0},
		{"limit_month_code", "monthly-code", 0},
	} {
		detail, present := payload.Usages[definition.key]
		if !present {
			continue
		}
		ratio, valid := quotaValue(detail.UsedRatio)
		if !valid || ratio < 0 {
			return "subscription", nil, fmt.Errorf("Kimi usage %s contained an invalid usage ratio", definition.key)
		}
		window := accounts.UsageWindow{Name: definition.name, UsedPercent: math.Min(ratio, 1) * 100, LimitWindowSeconds: definition.seconds}
		setResetTime(&window, detail.ResetTime, now)
		replaced := false
		for i := range windows {
			if windows[i].Name == window.Name {
				window.Used, window.Remaining, window.Limit = windows[i].Used, windows[i].Remaining, windows[i].Limit
				if window.ResetAt.IsZero() {
					window.ResetAt, window.ResetAfterSeconds = windows[i].ResetAt, windows[i].ResetAfterSeconds
				}
				windows[i] = window
				replaced = true
				break
			}
		}
		if !replaced {
			windows = append(windows, window)
		}
	}
	if len(windows) == 0 {
		return "subscription", nil, fmt.Errorf("Kimi usage response contained no quota windows")
	}
	return "subscription", windows, nil
}

func kimiUsageWindow(name string, seconds int64, detail quotaDetail, now time.Time) (accounts.UsageWindow, bool) {
	limit, ok := quotaValue(detail.Limit)
	if !ok || limit <= 0 {
		return accounts.UsageWindow{}, false
	}
	used, haveUsed := quotaValue(detail.Used)
	if remaining, haveRemaining := quotaValue(detail.Remaining); haveRemaining {
		used = limit - remaining
		haveUsed = true
	}
	if !haveUsed {
		return accounts.UsageWindow{}, false
	}
	quantityUsed := used
	if used < 0 {
		used = 0
	}
	if used > limit {
		used = limit
	}
	window := accounts.UsageWindow{
		Name:               name,
		UsedPercent:        100 * used / limit,
		LimitWindowSeconds: seconds,
		Used:               &quantityUsed,
		Limit:              &limit,
	}
	if remaining, ok := quotaValue(detail.Remaining); ok {
		window.Remaining = &remaining
	}
	setResetTime(&window, detail.ResetTime, now)
	return window, true
}

func setResetTime(window *accounts.UsageWindow, value string, now time.Time) {
	if reset, err := time.Parse(time.RFC3339Nano, value); err == nil {
		window.ResetAt = reset
		window.ResetAfterSeconds = max(0, int64(reset.Sub(now).Seconds()))
	}
}

func quotaValue(raw json.RawMessage) (float64, bool) {
	value := strings.Trim(strings.TrimSpace(string(raw)), `"`)
	if value == "" || value == "null" {
		return 0, false
	}
	n, err := strconv.ParseFloat(value, 64)
	return n, err == nil && !math.IsNaN(n) && !math.IsInf(n, 0)
}

func kimiWindowDuration(window quotaWindow) (int64, string, bool) {
	var unit time.Duration
	switch window.TimeUnit {
	case "TIME_UNIT_MINUTE":
		unit = time.Minute
	case "TIME_UNIT_HOUR":
		unit = time.Hour
	case "TIME_UNIT_DAY":
		unit = 24 * time.Hour
	case "TIME_UNIT_WEEK":
		unit = 7 * 24 * time.Hour
	default:
		return 0, "", false
	}
	duration := time.Duration(window.Duration) * unit
	if duration <= 0 {
		return 0, "", false
	}
	if duration == 7*24*time.Hour {
		return int64(duration / time.Second), "weekly", true
	}
	label := duration.String()
	if duration%(7*24*time.Hour) == 0 {
		label = fmt.Sprintf("%dw", duration/(7*24*time.Hour))
	} else if duration%(24*time.Hour) == 0 {
		label = fmt.Sprintf("%dd", duration/(24*time.Hour))
	} else if duration%time.Hour == 0 {
		label = fmt.Sprintf("%dh", duration/time.Hour)
	}
	return int64(duration / time.Second), label, true
}

func max(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
