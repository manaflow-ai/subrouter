package antigravity

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
)

const (
	usageTimeout          = 8 * time.Second
	usageResponseLimit    = 1 << 20
	cloudCodeBaseURL      = "https://daily-cloudcode-pa.googleapis.com"
	googleUserInfoURL     = "https://www.googleapis.com/oauth2/v2/userinfo"
	loadCodeAssistPath    = "/v1internal:loadCodeAssist"
	fetchModelsPath       = "/v1internal:fetchAvailableModels"
	retrieveSummaryPath   = "/v1internal:retrieveUserQuotaSummary"
	retrieveUserQuotaPath = "/v1internal:retrieveUserQuota"
)

// UsageDetails is account-specific telemetry returned by Google's supported
// OAuth services. Identity comes from the userinfo endpoint rather than from
// unsigned ID-token claims. Missing quota values remain absent: zero is quota
// exhaustion, not a synonym for unknown.
type UsageDetails struct {
	Email   string
	Plan    string
	Windows []accounts.UsageWindow
}

type usageFetcher struct {
	baseURL     string
	userInfoURL string
}

var antigravityUsageFetcher = usageFetcher{
	baseURL:     cloudCodeBaseURL,
	userInfoURL: googleUserInfoURL,
}

// FetchUsage retrieves bounded, read-only telemetry for exactly the bearer
// credential supplied by the managed profile. It never attaches to the AGY
// TUI or assumes that a host language-server session represents that profile.
func FetchUsage(ctx context.Context, client *http.Client, accessToken string, now time.Time) (UsageDetails, error) {
	return antigravityUsageFetcher.fetch(ctx, client, accessToken, now)
}

func (f usageFetcher) fetch(ctx context.Context, client *http.Client, accessToken string, now time.Time) (UsageDetails, error) {
	if strings.TrimSpace(accessToken) == "" {
		return UsageDetails{}, errors.New("Antigravity OAuth access token is missing")
	}
	if client == nil {
		client = http.DefaultClient
	}
	ctx, cancel := context.WithTimeout(ctx, usageTimeout)
	defer cancel()

	var identity struct {
		Email         string `json:"email"`
		VerifiedEmail bool   `json:"verified_email"`
	}
	details := UsageDetails{}
	// userinfo may be outside the grant's scopes. Its absence must not erase
	// otherwise valid quota, and an unverified address is never displayed.
	if err := f.call(ctx, client, http.MethodGet, f.userInfoURL, accessToken, nil, &identity); err == nil && identity.VerifiedEmail {
		details.Email = strings.TrimSpace(identity.Email)
	}

	var assist struct {
		PlanInfo *struct {
			PlanType string `json:"planType"`
		} `json:"planInfo"`
		CurrentTier *struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"currentTier"`
		Project json.RawMessage `json:"cloudaicompanionProject"`
	}
	metadata := map[string]any{"metadata": map[string]string{
		"ideType": "ANTIGRAVITY", "platform": "PLATFORM_UNSPECIFIED", "pluginType": "GEMINI",
	}}
	if err := f.call(ctx, client, http.MethodPost, f.baseURL+loadCodeAssistPath, accessToken, metadata, &assist); err != nil {
		return UsageDetails{}, fmt.Errorf("load Antigravity subscription: %w", err)
	}
	details.Plan = usagePlan(assist.PlanInfo, assist.CurrentTier)
	project := projectID(assist.Project)
	body := map[string]any{}
	if project != "" {
		body["project"] = project
	}

	// Newer services publish named 5-hour/weekly quota groups. Prefer those,
	// because they preserve independent Gemini and Claude/GPT exhaustion. Older
	// accounts publish only per-model quota; that remains useful, but is not
	// relabelled as a cadence the provider did not expose.
	summaryBody := map[string]any{
		"metadata": map[string]string{
			"ideName": "antigravity", "extensionName": "antigravity", "locale": "en", "ideVersion": "unknown",
		},
	}
	if project != "" {
		summaryBody["project"] = project
	}
	var quota any
	quotaErr := f.call(ctx, client, http.MethodPost, f.baseURL+retrieveSummaryPath, accessToken, summaryBody, &quota)
	if quotaErr == nil {
		details.Windows = quotaSummaryWindows(quota, now)
	}
	if len(details.Windows) == 0 {
		// Legacy remote services expose per-model quota, not named cadences.
		// Query it only after the summary endpoint is absent or unusable.
		var legacyQuota any
		if err := f.call(ctx, client, http.MethodPost, f.baseURL+retrieveUserQuotaPath, accessToken, body, &legacyQuota); err == nil {
			details.Windows = legacyQuotaWindows(legacyQuota, now)
		}
	}
	if len(details.Windows) == 0 {
		var models any
		if err := f.call(ctx, client, http.MethodPost, f.baseURL+fetchModelsPath, accessToken, body, &models); err != nil {
			if quotaErr != nil {
				return details, errors.Join(fmt.Errorf("retrieve Antigravity quota: %w", quotaErr), fmt.Errorf("fetch Antigravity models: %w", err))
			}
			return details, fmt.Errorf("fetch Antigravity models: %w", err)
		}
		details.Windows = modelQuotaWindows(models, now)
	}
	return details, nil
}

func (f usageFetcher) call(ctx context.Context, client *http.Client, method, endpoint, token string, body any, out any) error {
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Scheme != "https" && parsed.Scheme != "http" {
		return errors.New("invalid telemetry endpoint")
	}
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if parsed.Host == "cloudcode-pa.googleapis.com" || parsed.Host == "daily-cloudcode-pa.googleapis.com" || strings.HasPrefix(parsed.Host, "127.0.0.1:") {
		req.Header.Set("User-Agent", "antigravity")
	}
	res, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = res.Body.Close() }()
	limited := io.LimitReader(res.Body, usageResponseLimit+1)
	payload, err := io.ReadAll(limited)
	if err != nil {
		return err
	}
	if len(payload) > usageResponseLimit {
		return errors.New("telemetry response exceeds 1 MiB")
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return fmt.Errorf("HTTP %d", res.StatusCode)
	}
	if err := json.Unmarshal(payload, out); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}

func usagePlan(plan *struct {
	PlanType string `json:"planType"`
}, tier *struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}) string {
	if plan != nil && strings.TrimSpace(plan.PlanType) != "" {
		return strings.TrimSpace(plan.PlanType)
	}
	if tier == nil {
		return "subscription"
	}
	switch strings.ToLower(strings.TrimSpace(tier.ID)) {
	case "standard-tier":
		return "Paid"
	case "free-tier":
		return "Starter"
	case "legacy-tier":
		return "Legacy"
	}
	if name := strings.TrimSpace(tier.Name); name != "" {
		return name
	}
	return "subscription"
}

func projectID(raw json.RawMessage) string {
	var value string
	if json.Unmarshal(raw, &value) == nil {
		return strings.TrimSpace(value)
	}
	var object struct {
		ID        string `json:"id"`
		ProjectID string `json:"projectId"`
	}
	if json.Unmarshal(raw, &object) != nil {
		return ""
	}
	if strings.TrimSpace(object.ID) != "" {
		return strings.TrimSpace(object.ID)
	}
	return strings.TrimSpace(object.ProjectID)
}

func quotaSummaryWindows(payload any, now time.Time) []accounts.UsageWindow {
	root, ok := payload.(map[string]any)
	if !ok {
		return nil
	}
	for _, key := range []string{"response", "summary"} {
		if nested, ok := root[key].(map[string]any); ok {
			root = nested
			break
		}
	}
	groups, _ := root["groups"].([]any)
	var windows []accounts.UsageWindow
	for _, groupValue := range groups {
		group, _ := groupValue.(map[string]any)
		buckets, _ := group["buckets"].([]any)
		for _, bucketValue := range buckets {
			bucket, _ := bucketValue.(map[string]any)
			if disabled, _ := bucket["disabled"].(bool); disabled {
				continue
			}
			bucketIdentity := firstString(bucket, "bucketId", "displayName", "description")
			family := quotaFamily(firstString(group, "displayName", "description") + " " + bucketIdentity)
			cadence, seconds := quotaCadence(bucketIdentity)
			remaining, known := remainingFraction(bucket)
			if family == "" || cadence == "" || !known {
				continue
			}
			windows = append(windows, usageWindow(family+" "+cadence, family, seconds, remaining, firstString(bucket, "resetTime"), now))
		}
	}
	return mostConstrainedByName(windows)
}

func legacyQuotaWindows(payload any, now time.Time) []accounts.UsageWindow {
	root, ok := payload.(map[string]any)
	if !ok {
		return nil
	}
	buckets, _ := root["buckets"].([]any)
	models := map[string]any{}
	for _, bucketValue := range buckets {
		bucket, _ := bucketValue.(map[string]any)
		modelID := firstString(bucket, "modelId")
		if modelID == "" {
			continue
		}
		models[modelID] = map[string]any{"quotaInfo": bucket}
	}
	return modelQuotaWindows(map[string]any{"models": models}, now)
}

func modelQuotaWindows(payload any, now time.Time) []accounts.UsageWindow {
	root, ok := payload.(map[string]any)
	if !ok {
		return nil
	}
	models, _ := root["models"].(map[string]any)
	var windows []accounts.UsageWindow
	for modelID, modelValue := range models {
		model, _ := modelValue.(map[string]any)
		quota, _ := model["quotaInfo"].(map[string]any)
		remaining, known := remainingFraction(quota)
		if !known {
			continue
		}
		modelID = strings.TrimSpace(modelID)
		if modelID == "" {
			continue
		}
		windows = append(windows, usageWindow(modelID, modelID, 0, remaining, firstString(quota, "resetTime"), now))
	}
	return mostConstrainedByName(windows)
}

func usageWindow(name, feature string, seconds int64, remaining float64, reset string, now time.Time) accounts.UsageWindow {
	remaining = math.Max(0, math.Min(1, remaining))
	window := accounts.UsageWindow{Name: name, Feature: feature, UsedPercent: (1 - remaining) * 100, LimitWindowSeconds: seconds}
	if parsed, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(reset)); err == nil && parsed.After(now) {
		window.ResetAfterSeconds = int64(math.Ceil(parsed.Sub(now).Seconds()))
	}
	return window
}

func remainingFraction(value map[string]any) (float64, bool) {
	if value == nil {
		return 0, false
	}
	if remaining, ok := number(value["remainingFraction"]); ok {
		return remaining, true
	}
	if nested, ok := value["remaining"].(map[string]any); ok {
		if remaining, ok := number(nested["remainingFraction"]); ok {
			return remaining, true
		}
		if nested["case"] == "remainingFraction" {
			return number(nested["value"])
		}
	}
	return 0, false
}

func number(value any) (float64, bool) {
	switch value := value.(type) {
	case float64:
		return value, !math.IsNaN(value) && !math.IsInf(value, 0)
	case json.Number:
		parsed, err := value.Float64()
		return parsed, err == nil
	default:
		return 0, false
	}
}

func firstString(value map[string]any, keys ...string) string {
	for _, key := range keys {
		if text, ok := value[key].(string); ok && strings.TrimSpace(text) != "" {
			return strings.TrimSpace(text)
		}
	}
	return ""
}

func quotaFamily(value string) string {
	value = strings.ToLower(value)
	if strings.Contains(value, "gemini") {
		return "gemini"
	}
	if strings.Contains(value, "claude") || strings.Contains(value, "gpt") || strings.Contains(value, "openai") {
		return "claude-gpt"
	}
	if strings.Contains(value, "3p-") || strings.HasPrefix(value, "3p") {
		return "claude-gpt"
	}
	return ""
}

func quotaCadence(value string) (string, int64) {
	value = strings.ToLower(value)
	if strings.Contains(value, "5h") || strings.Contains(value, "5-hour") || strings.Contains(value, "5 hour") || strings.Contains(value, "session") {
		return "5h", int64((5 * time.Hour) / time.Second)
	}
	if strings.Contains(value, "weekly") || strings.Contains(value, "week") || strings.Contains(value, "7d") {
		return "weekly", int64((7 * 24 * time.Hour) / time.Second)
	}
	return "", 0
}

func mostConstrainedByName(windows []accounts.UsageWindow) []accounts.UsageWindow {
	byName := make(map[string]accounts.UsageWindow)
	for _, window := range windows {
		if current, ok := byName[window.Name]; !ok || window.UsedPercent > current.UsedPercent {
			byName[window.Name] = window
		}
	}
	out := make([]accounts.UsageWindow, 0, len(byName))
	for _, window := range byName {
		out = append(out, window)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
