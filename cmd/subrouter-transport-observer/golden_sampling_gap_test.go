package main

import (
	"testing"
	"time"
)

// The gap rule has to separate a sampler that lost the process tree from a
// runner that was briefly busy. Failing on any interval over the 100ms target
// made this required check fail on pull requests touching nothing near it.
func TestGoldenSamplingGapSeparatesBlindSpotsFromRunnerNoise(t *testing.T) {
	for _, testCase := range []struct {
		name           string
		maxGap         time.Duration
		gapsOverTarget int
		samples        int
		unacceptable   bool
	}{
		{"steady sampling", 40 * time.Millisecond, 0, 500, false},
		{"one hiccup in a long run", 300 * time.Millisecond, 1, 500, false},
		{"one hiccup in a short session", 300 * time.Millisecond, 1, 8, false},
		{"two hiccups in a short session", 300 * time.Millisecond, 2, 8, true},
		{"hiccups that stop being rare", 300 * time.Millisecond, 40, 500, true},
		{"a blind spot long enough to hide a spike", 2 * time.Second, 1, 500, true},
		{"no samples at all", 0, 0, 0, false},
		{"exactly at the target is not a gap", goldenProcessSampleMaxGap, 0, 100, false},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			got := goldenSamplingGapUnacceptable(testCase.maxGap, testCase.gapsOverTarget, testCase.samples)
			if got != testCase.unacceptable {
				t.Fatalf("goldenSamplingGapUnacceptable(%s, %d, %d) = %v, want %v",
					testCase.maxGap, testCase.gapsOverTarget, testCase.samples, got, testCase.unacceptable)
			}
		})
	}
}

// A stalled sampler must fail even when it produced few samples, because the
// rarity rule has nothing to divide by.
func TestGoldenSamplingGapFailsAStalledSamplerWithFewSamples(t *testing.T) {
	if !goldenSamplingGapUnacceptable(5*time.Second, 1, 3) {
		t.Fatal("a multi-second gap must fail regardless of sample count")
	}
}
