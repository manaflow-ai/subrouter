package claude

import (
	"net/http"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
)

// ObservedUsageWindows reads quota windows from a real Claude response. Share
// the existing header parser so fractional (0..1) and percentage (1..100)
// utilization have exactly the same interpretation as normal quota probes.
func ObservedUsageWindows(header http.Header, now time.Time) []accounts.UsageWindow {
	return usageWindowsFromFableHeaders(header, now)
}

// ObservedFeatureUsageWindows reads model-scoped quota from a real Claude
// response. Account-wide windows are available from ObservedUsageWindows so
// the controller can schedule the next check from the provider reset.
func ObservedFeatureUsageWindows(header http.Header, now time.Time) []accounts.UsageWindow {
	var observed []accounts.UsageWindow
	for _, window := range ObservedUsageWindows(header, now) {
		if window.Feature != "" {
			observed = append(observed, window)
		}
	}
	return observed
}
