package claude

import (
	"math"
	"net/http"
	"strconv"
	"testing"
	"time"
)

func TestObservedFeatureUsageWindowsIgnoresIncompleteOrInvalidHeaders(t *testing.T) {
	now := time.Now()
	for _, input := range []struct {
		label       string
		status      string
		utilization string
	}{
		{label: "only status allowed", status: "allowed"},
		{label: "invalid utilization", status: "allowed", utilization: "garbage"},
		{label: "NaN utilization", status: "allowed", utilization: "NaN"},
		{label: "negative utilization", status: "allowed", utilization: "-1"},
	} {
		h := make(http.Header)
		h.Set("anthropic-ratelimit-unified-7d_oi-status", input.status)
		if input.utilization != "" {
			h.Set("anthropic-ratelimit-unified-7d_oi-utilization", input.utilization)
		}
		if windows := ObservedFeatureUsageWindows(h, now); len(windows) != 0 {
			t.Errorf("%s should be ignored, got %+v", input.label, windows)
		}
	}
	h := make(http.Header)
	h.Set("anthropic-ratelimit-unified-5h-utilization", "0.5")
	if windows := ObservedFeatureUsageWindows(h, now); len(windows) != 0 {
		t.Fatalf("account-wide usage is not supplemental model quota: %+v", windows)
	}
}

func TestObservedFeatureUsageWindowsParsesRealModelBuckets(t *testing.T) {
	now := time.Now()
	h := make(http.Header)
	h.Set("anthropic-ratelimit-unified-7d_oi-utilization", "0.4")
	h.Set("anthropic-ratelimit-unified-7d_oi-status", "allowed")
	h.Set("anthropic-ratelimit-unified-7d_oi-reset", strconv.FormatInt(now.Add(time.Hour).Unix(), 10))
	h.Set("anthropic-ratelimit-unified-7d_opus-status", "rejected")
	h.Set("anthropic-ratelimit-unified-7d_opus-reset", strconv.FormatInt(now.Add(2*time.Hour).Unix(), 10))
	h.Set("anthropic-ratelimit-unified-7d_sonnet-utilization", "0.2")

	windows := ObservedFeatureUsageWindows(h, now)
	if len(windows) != 3 {
		t.Fatalf("windows=%+v; want Fable, Opus, Sonnet", windows)
	}
	byFeature := map[string]float64{}
	for _, window := range windows {
		byFeature[window.Feature] = window.UsedPercent
	}
	if math.Abs(byFeature[FableFeature]-40) > 0.0001 || byFeature[OpusFeature] != 100 || byFeature[SonnetFeature] != 20 {
		t.Fatalf("incorrect feature quota values: %+v", byFeature)
	}
	for _, window := range windows {
		if window.Feature == OpusFeature && window.ResetAt.IsZero() {
			t.Fatal("explicit exhausted model must retain its reset timestamp")
		}
	}
}
