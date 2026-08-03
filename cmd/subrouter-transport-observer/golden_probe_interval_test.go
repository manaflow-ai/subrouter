package main

import (
	"testing"
	"time"
)

func TestGoldenProbeIntervalRejectsDroppedHundredMillisecondTick(t *testing.T) {
	start := time.Date(2026, time.August, 3, 12, 0, 0, 0, time.UTC)
	end := start.Add(time.Second)
	labels := []string{"public-health", "public-ready", "local-health", "local-ready"}
	stats := &goldenProbeStats{}
	for _, label := range labels {
		for tick := 0; tick <= 10; tick++ {
			if tick == 5 {
				continue
			}
			stamp := start.Add(time.Duration(tick) * goldenProbeInterval)
			stats.events = append(stats.events, goldenProbeEvent{
				Kind: "probe", Timestamp: stamp.Format(time.RFC3339Nano), Label: label, OK: true,
			})
		}
	}

	if got := fixedGoldenFailure(stats.validateInterval(start, end)); got != "health_probe_gap" {
		t.Fatalf("failure = %q, want health_probe_gap", got)
	}
}
