package claude

import (
	"net/http"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
)

// ObservedFeatureUsageWindows reads model-scoped quota from a real Claude
// response. Share the existing header parser so fractional (0..1) and
// percentage (1..100) utilization have exactly the same interpretation as
// normal quota probes. Account-wide usage stays with the usage endpoint.
func ObservedFeatureUsageWindows(header http.Header, now time.Time) []accounts.UsageWindow {
	var observed []accounts.UsageWindow
	for _, window := range usageWindowsFromFableHeaders(header, now) {
		if window.Feature != "" {
			observed = append(observed, window)
		}
	}
	return observed
}
