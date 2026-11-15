package qwen

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
)

const addonSummaryAPI = "zeldaHttp.apikeyMgr./tokenplan/personal/api/v2/addon/summary"
const extraCreditsSource = "token-plan-extra"

// FetchExtraCreditsIn reads active extra packs separately from the monthly
// quota. Alibaba deducts these Credits only after the monthly quota is spent.
func FetchExtraCreditsIn(ctx context.Context, client *http.Client, root, accountID string) (*accounts.CreditsInfo, error) {
	config, err := readConsoleConfigIn(root, accountID)
	if err != nil {
		return nil, err
	}
	var response any
	if err := callConsole(ctx, client, config, addonSummaryAPI, map[string]any{"commodityCode": "sfm_tokenplansoloaddon_public_intl"}, &response); err != nil {
		return nil, err
	}
	return findExtraCredits(response)
}

func findExtraCredits(value any) (*accounts.CreditsInfo, error) {
	queue := []any{value}
	for len(queue) > 0 {
		value, queue = queue[0], queue[1:]
		object, ok := value.(map[string]any)
		if !ok {
			continue
		}
		if success, ok := object["success"].(bool); ok && !success {
			return nil, errors.New("Qwen extra-pack quota request was rejected")
		}
		if _, ok := object["remainingCredits"]; ok {
			remaining, hasRemaining := quotaNumber(object["remainingCredits"])
			total, hasTotal := quotaNumber(object["totalCredits"])
			if !hasRemaining || !hasTotal || remaining < 0 || total < 0 || remaining > total {
				return nil, errors.New("Qwen extra-pack quota response has invalid Credit quantities")
			}
			return &accounts.CreditsInfo{HasCredits: remaining > 0, Balance: strconv.FormatFloat(remaining, 'f', -1, 64), Unit: "Credits", Source: extraCreditsSource, Total: &total}, nil
		}
		queue = appendConsoleChildren(queue, object)
	}
	return nil, errors.New("Qwen extra-pack quota response omitted Credit quantities")
}

// ServingQuotaWindows preserves independent rate limits while allowing active
// extra packs to serve after monthly exhaustion. Display keeps the original
// monthly window; only eligibility uses this view.
func ServingQuotaWindows(windows []accounts.UsageWindow, credits *accounts.CreditsInfo) []accounts.UsageWindow {
	if credits == nil || credits.Source != extraCreditsSource || !credits.HasCredits {
		return windows
	}
	remaining, ok := quotaNumber(credits.Balance)
	if !ok || remaining <= 0 {
		return windows
	}
	var serving []accounts.UsageWindow
	for _, window := range windows {
		if window.Name == "monthly" && window.UsedPercent >= 100 && (window.ResetAt.IsZero() || window.ResetAt.After(time.Now())) {
			continue
		}
		serving = append(serving, window)
	}
	return serving
}
