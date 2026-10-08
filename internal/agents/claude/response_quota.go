package claude

import (
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
)

// ObservedFeatureUsageWindows extracts feature-scoped quota observations from
// a *real* Claude response. Ordinary 5h/7d balances remain the responsibility
// of the regular usage endpoint. Missing/malformed utilization is not
// evidence of unused quota, even if a response contains an "allowed" status.
func ObservedFeatureUsageWindows(header http.Header, now time.Time) []accounts.UsageWindow {
	if len(header) == 0 {
		return nil
	}
	candidates := usageWindowsFromFableHeaders(header, now)
	windows := make([]accounts.UsageWindow, 0, len(candidates))
	for _, window := range candidates {
		var prefix string
		switch window.Name {
		case FableWindowName:
			prefix = "7d_oi"
		case "opus-weekly":
			prefix = "7d_opus"
		case "sonnet-weekly":
			prefix = "7d_sonnet"
		default:
			continue
		}
		root := "anthropic-ratelimit-unified-" + prefix + "-"
		status := strings.ToLower(strings.TrimSpace(header.Get(root + "status")))
		if status == "rejected" {
			window.UsedPercent = 100
		} else {
			raw := strings.TrimSpace(header.Get(root + "utilization"))
			if raw == "" {
				continue
			}
			utilization, err := strconv.ParseFloat(raw, 64)
			if err != nil || math.IsNaN(utilization) || math.IsInf(utilization, 0) || utilization < 0 {
				continue
			}
			window.UsedPercent = math.Min(utilization*100, 100)
		}
		windows = append(windows, window)
	}
	return windows
}
