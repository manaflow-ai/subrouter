package accounts

import "testing"

func TestWeeklyCookedWindow(t *testing.T) {
	const week = 604800
	const fiveHours = 18000
	cases := []struct {
		name    string
		windows []UsageWindow
		want    bool
	}{
		{"weekly primary full", []UsageWindow{{Name: "primary", UsedPercent: 100, LimitWindowSeconds: week}}, true},
		{"weekly secondary full", []UsageWindow{
			{Name: "primary", UsedPercent: 10, LimitWindowSeconds: fiveHours},
			{Name: "secondary", UsedPercent: 100, LimitWindowSeconds: week},
		}, true},
		{"legacy secondary without length", []UsageWindow{{Name: "secondary", UsedPercent: 100}}, true},
		{"claude 7d by name", []UsageWindow{{Name: "7d", UsedPercent: 100}}, true},
		{"model-scoped weekly full is ignored", []UsageWindow{
			{Name: "primary", UsedPercent: 20, LimitWindowSeconds: week},
			{Name: "spark/primary", Feature: "spark", UsedPercent: 100, LimitWindowSeconds: week},
		}, false},
		{"reached with no weekly window reported", []UsageWindow{{Name: "reached", UsedPercent: 100}}, true},
		{"reached from the 5h window while weekly has room", []UsageWindow{
			{Name: "primary", UsedPercent: 100, LimitWindowSeconds: fiveHours},
			{Name: "secondary", UsedPercent: 40, LimitWindowSeconds: week},
			{Name: "reached", UsedPercent: 100},
		}, false},
		{"reached explained by a full 5h window, no weekly reported", []UsageWindow{
			{Name: "primary", UsedPercent: 100, LimitWindowSeconds: fiveHours},
			{Name: "reached", UsedPercent: 100},
		}, false},
		{"only 5h full", []UsageWindow{{Name: "primary", UsedPercent: 100, LimitWindowSeconds: fiveHours}}, false},
		{"none", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, got := WeeklyCookedWindow(tc.windows); got != tc.want {
				t.Fatalf("WeeklyCookedWindow = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestDescribeAccountWindows(t *testing.T) {
	got := DescribeAccountWindows([]UsageWindow{
		{Name: "primary", UsedPercent: 100, LimitWindowSeconds: 18000},
		{Name: "secondary", UsedPercent: 40, LimitWindowSeconds: 604800},
		{Name: "spark/primary", Feature: "spark", UsedPercent: 100, LimitWindowSeconds: 604800},
		{Name: "reached", UsedPercent: 100},
	})
	if want := "primary 5h 100%, secondary 7d 40%, limit_reached"; got != want {
		t.Fatalf("DescribeAccountWindows = %q, want %q", got, want)
	}
	if got := DescribeAccountWindows(nil); got != "no account-wide windows reported" {
		t.Fatalf("empty = %q", got)
	}
}

func TestCodexUsageShape(t *testing.T) {
	got := codexUsageShape(codexRateLimitDetails{
		PrimaryWindow: &codexLimitWindow{LimitWindowSeconds: 604800},
	})
	if want := "primary=604800s secondary=none limit_reached=false"; got != want {
		t.Fatalf("codexUsageShape = %q, want %q", got, want)
	}
}
