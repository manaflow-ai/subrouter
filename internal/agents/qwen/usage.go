package qwen

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
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

// UsageDetails contains the quota windows returned by the Token Plan console.
// Omitted windows remain unknown rather than implying zero or unlimited usage.
type UsageDetails struct {
	FiveHour *accounts.UsageWindow
	Weekly   *accounts.UsageWindow
	Monthly  *accounts.UsageWindow
	Windows  []accounts.UsageWindow
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

type usagePayload struct{ Windows []tokenPlanWindow }

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
	windows := payload.usageWindows(now)
	details := UsageDetails{Windows: windows}
	for i := range windows {
		switch windows[i].Name {
		case "5h":
			details.FiveHour = &windows[i]
		case "7d":
			details.Weekly = &windows[i]
		case "monthly":
			details.Monthly = &windows[i]
		}
	}
	return details, nil
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
		bodyBytes, _ := io.ReadAll(io.LimitReader(res.Body, (1<<20)+1))
		if res.StatusCode == http.StatusUnauthorized {
			return ErrConsoleLoginRequired
		}
		if res.StatusCode == http.StatusForbidden && len(bodyBytes) <= 1<<20 {
			var rejection any
			if json.Unmarshal(bodyBytes, &rejection) == nil &&
				containsConsoleErrorCode(rejection, "BailianGateway.Login.NotLogined") {
				return ErrConsoleLoginRequired
			}
		}
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
			// Alibaba uses both `code` and `errorCode` (including nested
			// envelopes) for the same console failure. Treat documented
			// spellings equivalently so expired telemetry login is not
			// misreported as generic unavailable quota.
			if strings.EqualFold(key, "code") || strings.EqualFold(key, "errorCode") || strings.EqualFold(key, "error_code") {
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
			var payload usagePayload
			keys := make([]string, 0, len(object))
			for key := range object {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			byPrefix := map[string]*tokenPlanWindow{}
			for _, field := range keys {
				parts := tokenPlanWindowField.FindStringSubmatch(field)
				if parts == nil {
					continue
				}
				count, _ := strconv.Atoi(parts[1])
				if count < 1 || count > 366 {
					continue
				}
				value, ok := quotaNumber(object[field])
				if !ok {
					continue
				}
				prefix := "per" + parts[1] + parts[2]
				window := byPrefix[prefix]
				if window == nil {
					window = &tokenPlanWindow{Count: count, Unit: parts[2]}
					byPrefix[prefix] = window
				}
				if parts[3] == "Percentage" {
					if value < 0 || value > 1 {
						continue
					}
					v := value
					window.Ratio = &v
				} else if value > 0 && value < float64(math.MaxInt64) {
					v := int64(value)
					window.ResetMillis = &v
				}
			}
			for _, window := range byPrefix {
				payload.Windows = append(payload.Windows, *window)
			}
			sort.Slice(payload.Windows, func(i, j int) bool {
				if payload.Windows[i].order() == payload.Windows[j].order() {
					return payload.Windows[i].Unit < payload.Windows[j].Unit
				}
				return payload.Windows[i].order() < payload.Windows[j].order()
			})
			return payload, nil
		}
		queue = appendConsoleChildren(queue, object)
	}
	return usagePayload{}, fmt.Errorf("Qwen Token Plan usage response contained no quota fields")
}

func hasUsageField(object map[string]any) bool {
	for key := range object {
		if tokenPlanWindowField.MatchString(key) {
			return true
		}
	}
	return false
}

var tokenPlanWindowField = regexp.MustCompile(`^per(\d+)(Hour|Day|Week|Month)(Percentage|ResetTime)$`)

type tokenPlanWindow struct {
	Count       int
	Unit        string
	Ratio       *float64
	ResetMillis *int64
}

func quotaNumber(value any) (float64, bool) {
	var number float64
	switch v := value.(type) {
	case float64:
		number = v
	case string:
		var err error
		number, err = strconv.ParseFloat(v, 64)
		if err != nil {
			return 0, false
		}
	default:
		return 0, false
	}
	return number, !math.IsNaN(number) && !math.IsInf(number, 0)
}

func (w tokenPlanWindow) order() int {
	switch w.Unit {
	case "Hour":
		return w.Count
	case "Day":
		return w.Count * 24
	case "Week":
		return w.Count * 168
	default:
		return w.Count * 24 * 31
	}
}

func (p usagePayload) usageWindows(now time.Time) []accounts.UsageWindow {
	var windows []accounts.UsageWindow
	for _, w := range p.Windows {
		name := strconv.Itoa(w.Count)
		duration := time.Duration(0)
		switch w.Unit {
		case "Hour":
			name += "h"
			duration = time.Duration(w.Count) * time.Hour
		case "Day":
			name += "d"
			duration = time.Duration(w.Count) * 24 * time.Hour
		case "Week":
			name = strconv.Itoa(w.Count*7) + "d"
			duration = time.Duration(w.Count) * 7 * 24 * time.Hour
		case "Month":
			name += "-month"
			if w.Count == 1 {
				name = "monthly"
			}
		}
		if window := quotaWindow(name, duration, w.Ratio, w.ResetMillis, now); window != nil {
			windows = append(windows, *window)
		}
	}
	return windows
}

func findSubscriptionDetails(value any) (SubscriptionDetails, error) {
	if details, ok, err := findSubscriptionDetailsByKeys(value, []string{"specCode", "spec_code"}, false); err != nil {
		return SubscriptionDetails{}, err
	} else if ok {
		return details, nil
	}
	if details, ok, err := findSubscriptionDetailsByKeys(value, []string{"planName", "plan_name"}, true); err != nil {
		return SubscriptionDetails{}, err
	} else if ok {
		return details, nil
	}
	return SubscriptionDetails{}, fmt.Errorf("Qwen Token Plan subscription response contained no plan")
}

func findSubscriptionDetailsByKeys(value any, planKeys []string, requireKnownPersonalTier bool) (SubscriptionDetails, bool, error) {
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
				return SubscriptionDetails{}, false, ErrConsoleLoginRequired
			}
			if code == "" {
				code = "unknown error"
			}
			return SubscriptionDetails{}, false, fmt.Errorf("Qwen Token Plan console error: %s", code)
		}
		spec := firstSubscriptionString(object, planKeys...)
		if spec != "" {
			if requireKnownPersonalTier && (!subscriptionShaped(object) || !knownPersonalTier(spec)) {
				queue = appendConsoleChildren(queue, object)
				continue
			}
			status := firstSubscriptionString(object, "status")
			instanceCode := firstSubscriptionString(object, "instanceCode", "instance_code")
			return SubscriptionDetails{
				Plan:         displayPlan(spec),
				Status:       strings.ToLower(strings.TrimSpace(status)),
				InstanceCode: instanceCode,
				StartsAt:     millisTime(object["startTime"]),
				ExpiresAt:    millisTime(object["endTime"]),
			}, true, nil
		}
		queue = appendConsoleChildren(queue, object)
	}
	return SubscriptionDetails{}, false, nil
}

func subscriptionShaped(object map[string]any) bool {
	for _, key := range []string{"instanceCode", "instance_code", "startTime", "endTime"} {
		if _, ok := object[key]; ok {
			return true
		}
	}
	return false
}

func knownPersonalTier(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "lite", "standard", "pro", "max":
		return true
	default:
		return false
	}
}

func firstSubscriptionString(object map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := object[key].(string); ok {
			if value = strings.TrimSpace(value); value != "" {
				return value
			}
		}
	}
	return ""
}

func appendConsoleChildren(queue []any, object map[string]any) []any {
	keys := make([]string, 0, len(object))
	for key := range object {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		child := object[key]
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
		window.ResetAt = reset
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
