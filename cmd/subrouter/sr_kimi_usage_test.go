package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
)

func TestKimiMonthlyUsageIsDisplayedWithoutInventingWeeklyQuota(t *testing.T) {
	row := srUsageRow{email: "kimi-subscription:work", provider: accounts.ProviderKimi, authMode: accounts.AuthModeOAuth, quotaUsageKnown: true, windows: []accounts.UsageWindow{
		{Name: "5h", UsedPercent: 12.5, LimitWindowSeconds: 18000, ResetAfterSeconds: 3600},
		{Name: "monthly", UsedPercent: 40, ResetAfterSeconds: 2 * 86400},
		{Name: "monthly-code", UsedPercent: 75, ResetAfterSeconds: 2 * 86400},
	}}
	row.score = scoreFromWindows(row.email, row.windows)
	var out bytes.Buffer
	displayUsageRows(&out, []srUsageRow{row}, false)
	text := out.String()
	for _, want := range []string{"Monthly", "Code/mo", "60%/2d", "25%/2d"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q in Kimi report:\n%s", want, text)
		}
	}
	if strings.Contains(text, "Weekly") {
		t.Fatalf("monthly quota mislabeled weekly:\n%s", text)
	}
	_, weekly := srStatusQuotaWindows(row)
	if weekly != nil {
		t.Fatal("monthly quota leaked into weekly JSON summary")
	}
}

func TestStatusJSONPreservesDetailedMonthlyWindows(t *testing.T) {
	reset := time.Now().AddDate(0, 1, 0).UTC().Truncate(time.Second)
	row := srUsageRow{email: "kimi-subscription:work", provider: accounts.ProviderKimi, authMode: accounts.AuthModeOAuth, windows: []accounts.UsageWindow{{Name: "monthly-code", UsedPercent: 37.5, ResetAt: reset}}}
	data, err := json.Marshal(srStatusAccountFromRow(row, time.Now()))
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := json.Unmarshal(data, &document); err != nil {
		t.Fatal(err)
	}
	windows, ok := document["windows"].([]any)
	if !ok || len(windows) != 1 {
		t.Fatalf("detailed windows omitted: %s", data)
	}
	window := windows[0].(map[string]any)
	if window["name"] != "monthly-code" || window["used_pct"] != 37.5 || window["left_pct"] != 62.5 || window["reset_at"] != reset.Format(time.RFC3339) {
		t.Fatalf("wrong detailed monthly data: %s", data)
	}
	if window["used"] != nil || window["limit"] != nil || window["remaining"] != nil {
		t.Fatal("count quantities were invented for ratio-only quota")
	}
}
