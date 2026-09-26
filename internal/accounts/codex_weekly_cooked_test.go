package accounts

import "testing"

func TestWeeklyLimitCooked(t *testing.T) {
	const week = 604800
	const fiveHours = 18000
	cases := []struct {
		name string
		rl   codexRateLimitDetails
		want bool
	}{
		{"limit reached flag", codexRateLimitDetails{LimitReached: true}, true},
		{"legacy secondary full without window length", codexRateLimitDetails{
			SecondaryWindow: &codexLimitWindow{UsedPercent: 100},
		}, true},
		{"weekly secondary full", codexRateLimitDetails{
			PrimaryWindow:   &codexLimitWindow{UsedPercent: 3, LimitWindowSeconds: fiveHours},
			SecondaryWindow: &codexLimitWindow{UsedPercent: 100, LimitWindowSeconds: week},
		}, true},
		// Upstream now reports the weekly window as primary with no secondary.
		{"weekly primary full, no secondary", codexRateLimitDetails{
			PrimaryWindow: &codexLimitWindow{UsedPercent: 100, LimitWindowSeconds: week, ResetAfterSeconds: 440286},
		}, true},
		{"weekly primary partial", codexRateLimitDetails{
			PrimaryWindow: &codexLimitWindow{UsedPercent: 80, LimitWindowSeconds: week},
		}, false},
		{"only 5h window full", codexRateLimitDetails{
			PrimaryWindow:   &codexLimitWindow{UsedPercent: 100, LimitWindowSeconds: fiveHours},
			SecondaryWindow: &codexLimitWindow{UsedPercent: 40, LimitWindowSeconds: week},
		}, false},
		{"limit reached from 5h while weekly has room", codexRateLimitDetails{
			LimitReached:    true,
			PrimaryWindow:   &codexLimitWindow{UsedPercent: 100, LimitWindowSeconds: fiveHours},
			SecondaryWindow: &codexLimitWindow{UsedPercent: 40, LimitWindowSeconds: week},
		}, false},
		{"empty", codexRateLimitDetails{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := WeeklyLimitCooked(CodexUsageDetails{RawRateLimit: tc.rl}); got != tc.want {
				t.Fatalf("WeeklyLimitCooked = %v, want %v", got, tc.want)
			}
		})
	}
}
