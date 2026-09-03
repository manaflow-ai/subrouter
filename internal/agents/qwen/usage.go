package qwen

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/internal/storepath"
)

const (
	usageAPI         = "zeldaHttp.apikeyMgr./tokenplan/personal/api/v2/usage"
	subscriptionAPI  = "zeldaHttp.apikeyMgr./tokenplan/personal/api/v2/subscription"
	commodityCode    = "sfm_tokenplansolo_public_intl"
	defaultRegion    = "ap-southeast-1"
	defaultSite      = "international"
	internationalURL = "https://bailian-singapore-cs.alibabacloud.com"
	domesticURL      = "https://modelstudio-cs.console.aliyun.com"
)

var ErrConsoleLoginRequired = errors.New("Qwen Token Plan console login required")

// UsageDetails is the pair of independent fixed-window quotas returned by the
// Token Plan console. A nil window means Alibaba omitted that window; callers
// must not infer either zero usage or unlimited quota from its absence.
type UsageDetails struct {
	FiveHour *accounts.UsageWindow
	Weekly   *accounts.UsageWindow
}

// SubscriptionDetails identifies the purchased Token Plan independently of
// the user-supplied Subrouter label.
type SubscriptionDetails struct {
	Plan         string
	Status       string
	InstanceCode string
	StartsAt     time.Time
	ExpiresAt    time.Time
}

type consoleConfig struct {
	AccessToken        string `json:"access_token"`
	ConsoleRegion      string `json:"console_region"`
	ConsoleSite        string `json:"console_site"`
	ConsoleSwitchAgent *int64 `json:"console_switch_agent"`
}

type usagePayload struct {
	Per5HourPercentage *float64 `json:"per5HourPercentage"`
	Per5HourResetTime  *int64   `json:"per5HourResetTime"`
	Per1WeekPercentage *float64 `json:"per1WeekPercentage"`
	Per1WeekResetTime  *int64   `json:"per1WeekResetTime"`
}

// ConsoleConfigDir is the isolated Bailian CLI profile for one Subrouter
// account. Keeping one directory per account avoids a global CLI profile
// switch silently making every Qwen row report the same subscription.
func ConsoleConfigDir(accountID string) string {
	return ConsoleConfigDirIn(DefaultConsoleRoot(), accountID)
}

func DefaultConsoleRoot() string {
	return filepath.Join(storepath.StateDir(), "qwen-console")
}

func ConsoleRootForStore(store accounts.CodexStore) string {
	return filepath.Join(filepath.Dir(store.StoreDir()), "qwen-console")
}

// ConsoleRootForScope isolates client-side console credentials for remote
// servers and hosted tenants whose account labels may overlap. safeFilename
// hashes the scope, so URLs and tenant keys never appear in the filesystem.
func ConsoleRootForScope(scope string) string {
	sum := sha256.Sum256([]byte(scope))
	return filepath.Join(DefaultConsoleRoot(), "scopes", fmt.Sprintf("%x", sum[:12]))
}

func ConsoleConfigDirIn(root, accountID string) string {
	return filepath.Join(root, safeFilename(accountID))
}

func ConsoleConfigPath(accountID string) string {
	return ConsoleConfigPathIn(DefaultConsoleRoot(), accountID)
}

func ConsoleConfigPathIn(root, accountID string) string {
	return filepath.Join(ConsoleConfigDirIn(root, accountID), "config.json")
}

// HasConsoleCredential reports whether status can query this account without
// treating a missing optional profile as an error.
func HasConsoleCredential(accountID string) (bool, error) {
	return HasConsoleCredentialIn(DefaultConsoleRoot(), accountID)
}

func HasConsoleCredentialIn(root, accountID string) (bool, error) {
	config, err := readConsoleConfigIn(root, accountID)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(config.AccessToken) != "", nil
}

// FetchUsage reads the account's isolated console credential and calls the
// same international CLI gateway used by Alibaba's official Bailian CLI.
func FetchUsage(ctx context.Context, client *http.Client, accountID string) (UsageDetails, error) {
	return FetchUsageIn(ctx, client, DefaultConsoleRoot(), accountID)
}

func FetchUsageIn(ctx context.Context, client *http.Client, root, accountID string) (UsageDetails, error) {
	config, err := readConsoleConfigIn(root, accountID)
	if err != nil {
		return UsageDetails{}, err
	}
	if strings.TrimSpace(config.AccessToken) == "" {
		return UsageDetails{}, fmt.Errorf("Qwen Token Plan console credential is missing")
	}
	var response any
	if err := callConsole(ctx, client, config, usageAPI, nil, &response); err != nil {
		return UsageDetails{}, err
	}
	payload, err := findUsagePayload(response)
	if err != nil {
		return UsageDetails{}, err
	}
	now := time.Now()
	return UsageDetails{
		FiveHour: quotaWindow("5h", 5*time.Hour, payload.Per5HourPercentage, payload.Per5HourResetTime, now),
		Weekly:   quotaWindow("7d", 7*24*time.Hour, payload.Per1WeekPercentage, payload.Per1WeekResetTime, now),
	}, nil
}

// FetchSubscription returns the vendor-owned Lite/Pro identity for a Token
// Plan. This avoids guessing the plan from an arbitrary local account label.
func FetchSubscription(ctx context.Context, client *http.Client, accountID string) (SubscriptionDetails, error) {
	return FetchSubscriptionIn(ctx, client, DefaultConsoleRoot(), accountID)
}

func FetchSubscriptionIn(ctx context.Context, client *http.Client, root, accountID string) (SubscriptionDetails, error) {
	config, err := readConsoleConfigIn(root, accountID)
	if err != nil {
		return SubscriptionDetails{}, err
	}
	if strings.TrimSpace(config.AccessToken) == "" {
		return SubscriptionDetails{}, fmt.Errorf("Qwen Token Plan console credential is missing")
	}
	var response any
	data := map[string]any{
		"queryInstanceInfoRequest": map[string]any{"commodityCode": commodityCode},
	}
	if err := callConsole(ctx, client, config, subscriptionAPI, data, &response); err != nil {
		return SubscriptionDetails{}, err
	}
	return findSubscriptionDetails(response)
}

func callConsole(ctx context.Context, client *http.Client, config consoleConfig, api string, data map[string]any, output any) error {
	if client == nil {
		client = &http.Client{Timeout: 8 * time.Second}
	}
	requestClient := *client
	requestClient.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	requestCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	region := strings.TrimSpace(config.ConsoleRegion)
	if region == "" {
		region = defaultRegion
	}
	site := strings.TrimSpace(config.ConsoleSite)
	if site == "" {
		site = defaultSite
	}
	gateway, action := consoleGateway(region, site)
	if data == nil {
		data = map[string]any{}
	}
	cornerstone := map[string]any{
		"protocol":       "V2",
		"console":        "ONE_CONSOLE",
		"productCode":    "p_efm",
		"switchUserType": 3,
		"consoleSite":    "BAILIAN_ALIYUN",
	}
	if config.ConsoleSwitchAgent != nil {
		cornerstone["switchAgent"] = *config.ConsoleSwitchAgent
	}
	data["cornerstoneParam"] = cornerstone
	params := map[string]any{"Api": api, "V": "1.0", "Data": data}
	encodedParams, err := json.Marshal(params)
	if err != nil {
		return err
	}
	body := url.Values{"params": {string(encodedParams)}, "region": {region}}
	endpoint := gateway + "/cli/api.json?action=" + url.QueryEscape(action) + "&product=sfm_bailian&api=" + url.QueryEscape(api)
	req, err := http.NewRequestWithContext(requestCtx, http.MethodPost, endpoint, strings.NewReader(body.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Authorization", "Bearer "+config.AccessToken)
	res, err := requestClient.Do(req)
	if err != nil {
		return fmt.Errorf("Qwen Token Plan console request: %w", err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		_, _ = io.CopyN(io.Discard, res.Body, 4096)
		return fmt.Errorf("Qwen Token Plan console returned HTTP %d", res.StatusCode)
	}
	bodyBytes, err := io.ReadAll(io.LimitReader(res.Body, (1<<20)+1))
	if err != nil {
		return fmt.Errorf("read Qwen Token Plan console response: %w", err)
	}
	if len(bodyBytes) > 1<<20 {
		return fmt.Errorf("Qwen Token Plan console response exceeds 1 MiB")
	}
	var envelope any
	if err := json.Unmarshal(bodyBytes, &envelope); err != nil {
		return fmt.Errorf("decode Qwen Token Plan console response: %w", err)
	}
	if containsConsoleErrorCode(envelope, "BailianGateway.Login.NotLogined") {
		return ErrConsoleLoginRequired
	}
	if err := json.Unmarshal(bodyBytes, output); err != nil {
		return fmt.Errorf("decode Qwen Token Plan console response: %w", err)
	}
	return nil
}

func containsConsoleErrorCode(value any, code string) bool {
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			if strings.EqualFold(key, "code") {
				if value, ok := child.(string); ok && value == code {
					return true
				}
			}
			if containsConsoleErrorCode(child, code) {
				return true
			}
		}
	case []any:
		for _, child := range typed {
			if containsConsoleErrorCode(child, code) {
				return true
			}
		}
	}
	return false
}

// StatusError turns the two independent console fetch failures into one
// operator-facing error. Alibaba returns the same expired-login response from
// both endpoints, so identical messages are collapsed instead of printed
// twice.
func StatusError(accountID string, errs ...error) error {
	unique := make([]string, 0, len(errs))
	seen := make(map[string]struct{}, len(errs))
	for _, err := range errs {
		if err == nil {
			continue
		}
		if errors.Is(err, ErrConsoleLoginRequired) {
			return fmt.Errorf("Qwen console login needed; run sr qwen login %s: %w", shellQuoteArgument(accountID), ErrConsoleLoginRequired)
		}
		message := err.Error()
		if _, ok := seen[message]; ok {
			continue
		}
		seen[message] = struct{}{}
		unique = append(unique, message)
	}
	if len(unique) == 0 {
		return nil
	}
	return errors.New(strings.Join(unique, "\n"))
}

func shellQuoteArgument(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func readConsoleConfigIn(root, accountID string) (consoleConfig, error) {
	body, err := os.ReadFile(ConsoleConfigPathIn(root, accountID))
	if err != nil {
		return consoleConfig{}, err
	}
	var config consoleConfig
	if err := json.Unmarshal(body, &config); err != nil {
		return consoleConfig{}, fmt.Errorf("parse Qwen console credential: %w", err)
	}
	return config, nil
}

func consoleGateway(region, site string) (gateway, action string) {
	if region == defaultRegion {
		if site == "domestic" {
			return domesticURL, "IntlBroadScopeAspnGateway"
		}
		return internationalURL, "IntlBroadScopeAspnGateway"
	}
	if site == "international" {
		return "https://bailian-cs.console.alibabacloud.com", "BroadScopeAspnGateway"
	}
	return "https://bailian-cs.console.aliyun.com", "BroadScopeAspnGateway"
}

func findUsagePayload(value any) (usagePayload, error) {
	queue := []any{value}
	for len(queue) > 0 {
		value = queue[0]
		queue = queue[1:]
		object, ok := value.(map[string]any)
		if !ok {
			continue
		}
		if success, ok := object["success"].(bool); ok && !success {
			code, _ := object["errorCode"].(string)
			if code == "BailianGateway.Login.NotLogined" {
				return usagePayload{}, ErrConsoleLoginRequired
			}
			if code == "" {
				code = "unknown error"
			}
			return usagePayload{}, fmt.Errorf("Qwen Token Plan console error: %s", code)
		}
		if hasUsageField(object) {
			body, err := json.Marshal(object)
			if err != nil {
				return usagePayload{}, err
			}
			var payload usagePayload
			if err := json.Unmarshal(body, &payload); err != nil {
				return usagePayload{}, err
			}
			return payload, nil
		}
		queue = appendConsoleChildren(queue, object)
	}
	return usagePayload{}, fmt.Errorf("Qwen Token Plan usage response contained no quota fields")
}

func hasUsageField(object map[string]any) bool {
	for _, key := range []string{"per5HourPercentage", "per5HourResetTime", "per1WeekPercentage", "per1WeekResetTime"} {
		if _, ok := object[key]; ok {
			return true
		}
	}
	return false
}

func findSubscriptionDetails(value any) (SubscriptionDetails, error) {
	queue := []any{value}
	for len(queue) > 0 {
		value = queue[0]
		queue = queue[1:]
		object, ok := value.(map[string]any)
		if !ok {
			continue
		}
		if success, ok := object["success"].(bool); ok && !success {
			code, _ := object["errorCode"].(string)
			if code == "BailianGateway.Login.NotLogined" {
				return SubscriptionDetails{}, ErrConsoleLoginRequired
			}
			if code == "" {
				code = "unknown error"
			}
			return SubscriptionDetails{}, fmt.Errorf("Qwen Token Plan console error: %s", code)
		}
		spec, _ := object["specCode"].(string)
		if spec != "" {
			status, _ := object["status"].(string)
			instanceCode, _ := object["instanceCode"].(string)
			return SubscriptionDetails{
				Plan:         displayPlan(spec),
				Status:       strings.ToLower(strings.TrimSpace(status)),
				InstanceCode: instanceCode,
				StartsAt:     millisTime(object["startTime"]),
				ExpiresAt:    millisTime(object["endTime"]),
			}, nil
		}
		queue = appendConsoleChildren(queue, object)
	}
	return SubscriptionDetails{}, fmt.Errorf("Qwen Token Plan subscription response contained no plan")
}

func appendConsoleChildren(queue []any, object map[string]any) []any {
	for _, child := range object {
		switch child := child.(type) {
		case map[string]any:
			queue = append(queue, child)
		case []any:
			queue = append(queue, child...)
		}
	}
	return queue
}

func displayPlan(spec string) string {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return ""
	}
	return strings.ToUpper(spec[:1]) + strings.ToLower(spec[1:])
}

func millisTime(value any) time.Time {
	var millis int64
	switch value := value.(type) {
	case float64:
		millis = int64(value)
	case json.Number:
		millis, _ = value.Int64()
	case int64:
		millis = value
	}
	if millis <= 0 {
		return time.Time{}
	}
	return time.UnixMilli(millis)
}

func quotaWindow(name string, duration time.Duration, ratio *float64, resetMillis *int64, now time.Time) *accounts.UsageWindow {
	if ratio == nil {
		return nil
	}
	used := *ratio * 100
	if used < 0 {
		used = 0
	}
	if used > 100 {
		used = 100
	}
	window := &accounts.UsageWindow{Name: name, UsedPercent: used, LimitWindowSeconds: int64(duration / time.Second)}
	if resetMillis != nil {
		reset := time.UnixMilli(*resetMillis)
		window.ResetAfterSeconds = max(0, int64(reset.Sub(now).Seconds()))
	}
	return window
}

func safeFilename(value string) string {
	var out strings.Builder
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '.' || r == '_' || r == '-' {
			out.WriteRune(r)
		} else {
			out.WriteByte('_')
		}
	}
	if out.Len() == 0 {
		out.WriteString("account")
	}
	sum := sha256.Sum256([]byte(value))
	return out.String() + "-" + fmt.Sprintf("%x", sum[:6])
}
