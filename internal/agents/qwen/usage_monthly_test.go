package qwen

import (
	"encoding/json"
	"testing"
	"time"
)

func TestTokenPlanReadsMonthlyWindowWithoutInventingDuration(t *testing.T) {
	reset := time.Now().Add(20 * 24 * time.Hour).UnixMilli()
	var raw any
	body := `{"data":{"DataV2":{"data":{"per1MonthPercentage":0.375,"per1MonthResetTime":` + jsonNumber(reset) + `}}}}`
	if err := json.Unmarshal([]byte(body), &raw); err != nil {
		t.Fatal(err)
	}
	payload, err := findUsagePayload(raw)
	if err != nil {
		t.Fatal(err)
	}
	windows := payload.usageWindows(time.Now())
	if len(windows) != 1 || windows[0].Name != "monthly" || windows[0].UsedPercent != 37.5 || windows[0].LimitWindowSeconds != 0 || windows[0].ResetAt.UnixMilli() != reset {
		t.Fatalf("monthly quota lost or mislabeled: %+v", windows)
	}
	if windows[0].Used != nil || windows[0].Remaining != nil || windows[0].Limit != nil {
		t.Fatal("percentage-only payload fabricated credit quantities")
	}
}

func TestTokenPlanReadsMixedVendorWindowsAndNumericStrings(t *testing.T) {
	payload, err := findUsagePayload(map[string]any{"per5HourPercentage": "0.1", "per1WeekPercentage": 0.2, "per1MonthPercentage": 0.3})
	if err != nil {
		t.Fatal(err)
	}
	windows := payload.usageWindows(time.Now())
	if len(windows) != 3 {
		t.Fatalf("windows=%+v", windows)
	}
	for i, name := range []string{"5h", "7d", "monthly"} {
		if windows[i].Name != name {
			t.Fatalf("window %d=%s want %s", i, windows[i].Name, name)
		}
	}
}
