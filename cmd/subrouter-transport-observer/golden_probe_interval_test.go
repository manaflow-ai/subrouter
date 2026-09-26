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

func TestGoldenProbeValidationEnforcesProductionCadence(t *testing.T) {
	previous := goldenTestHooks
	t.Cleanup(func() { goldenTestHooks = previous })
	start := time.Date(2026, time.August, 3, 12, 0, 0, 0, time.UTC)
	end := start.Add(2 * time.Second)
	labels := []string{"public-health", "public-ready", "local-health", "local-ready"}
	// One probe every two intervals: production needs one per interval, while
	// an orchestration test with a widened schedule tolerance accepts it.
	stats := &goldenProbeStats{}
	for _, label := range labels {
		for stamp := start; !stamp.After(end); stamp = stamp.Add(2 * goldenProbeInterval) {
			stats.events = append(stats.events, goldenProbeEvent{
				Kind: "probe", Timestamp: stamp.Format(time.RFC3339Nano), Label: label, OK: true,
			})
		}
	}

	goldenTestHooks.enabled = false
	goldenTestHooks.probeScheduleTolerance = 0
	if got := fixedGoldenFailure(stats.validateInterval(start, end)); got != "health_probe_frequency_low" {
		t.Fatalf("production failure = %q, want health_probe_frequency_low", got)
	}

	goldenTestHooks.enabled = true
	goldenTestHooks.probeScheduleTolerance = time.Second
	if err := stats.validateInterval(start, end); err != nil {
		t.Fatalf("orchestration test cadence rejected probes within its tolerance: %v", err)
	}
}

func TestGoldenSamplingGapLimitsFollowTestHooksOnlyInTestMode(t *testing.T) {
	previous := goldenTestHooks
	t.Cleanup(func() { goldenTestHooks = previous })
	goldenTestHooks.processSampleMaxGap = time.Second
	goldenTestHooks.processSampleHardCeiling = 5 * time.Second

	goldenTestHooks.enabled = false
	if !goldenSamplingGapUnacceptable(goldenProcessSampleHardCeiling+time.Millisecond, 1, 100) {
		t.Fatal("production run accepted a sampling gap above the hard ceiling")
	}
	if !goldenSamplingGapUnacceptable(goldenProcessSampleMaxGap+time.Millisecond, 10, 100) {
		t.Fatal("production run accepted frequent gaps over the target")
	}

	goldenTestHooks.enabled = true
	if goldenSamplingGapUnacceptable(goldenProcessSampleHardCeiling+time.Millisecond, 1, 100) {
		t.Fatal("test run rejected a single gap below its widened ceiling")
	}
	if !goldenSamplingGapUnacceptable(5*time.Second+time.Millisecond, 1, 100) {
		t.Fatal("test run accepted a gap above its widened ceiling")
	}
}
