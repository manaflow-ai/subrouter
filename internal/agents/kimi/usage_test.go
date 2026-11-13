package kimi

import (
	"context"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/account"
	"github.com/manaflow-ai/subrouter/internal/accounts"
)

func fetchUsageFixture(t *testing.T, payload string) []accounts.UsageWindow {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, payload)
	}))
	defer server.Close()
	old := usageURL
	usageURL = server.URL
	t.Cleanup(func() { usageURL = old })
	_, windows, err := (Store{}).FetchUsage(context.Background(), server.Client(), account.Account{Token: "fixture-access"})
	if err != nil {
		t.Fatal(err)
	}
	return windows
}

func TestFetchUsagePreservesMonthlyTotalAndCodeRatios(t *testing.T) {
	windows := fetchUsageFixture(t, `{
	 "limits":[{"window":{"duration":300,"timeUnit":"TIME_UNIT_MINUTE"},"detail":{"limit":"100","remaining":"100","resetTime":"2026-11-13T11:14:08Z"}}],
	 "usages":{
	  "limit_5h":{"used_ratio":0,"reset_time":"2026-11-13T11:14:07Z"},
	  "limit_month_total":{"used_ratio":1,"reset_time":"2026-12-09T00:00:00Z"},
	  "limit_month_code":{"used_ratio":0.375,"reset_time":"2026-12-09T00:00:00Z"}
	 }}`)
	if len(windows) != 3 {
		t.Fatalf("got %d quota windows, want session plus both monthly limits: %+v", len(windows), windows)
	}
	byName := map[string]accounts.UsageWindow{}
	for _, w := range windows {
		byName[w.Name] = w
	}
	for name, want := range map[string]float64{"5h": 0, "monthly": 100, "monthly-code": 37.5} {
		w, ok := byName[name]
		if !ok || math.Abs(w.UsedPercent-want) > 1e-9 {
			t.Fatalf("%s window=%+v, want %.1f%%", name, w, want)
		}
	}
	for _, name := range []string{"monthly", "monthly-code"} {
		w := byName[name]
		if w.ResetAt.Format(time.RFC3339) != "2026-12-09T00:00:00Z" {
			t.Fatal("monthly reset was discarded", w)
		}
		if w.LimitWindowSeconds != 0 {
			t.Fatal("calendar month was replaced with an invented fixed duration", w)
		}
	}
	if _, ok := byName["weekly"]; ok {
		t.Fatal("monthly quota was labeled weekly")
	}
}

func TestFetchUsagePrefersDetailedRatiosWithoutDuplicatingLegacyWindows(t *testing.T) {
	windows := fetchUsageFixture(t, `{"usage":{"used":50,"limit":100},"limits":[{"window":{"duration":300,"timeUnit":"TIME_UNIT_MINUTE"},"detail":{"remaining":88,"limit":100}}],"usages":{"limit_5h":{"used_ratio":0.125},"limit_7d":{"used_ratio":0.25}}}`)
	if len(windows) != 2 {
		t.Fatalf("duplicate quota windows: %+v", windows)
	}
	for _, w := range windows {
		want := 25.0
		if w.Name == "5h" {
			want = 12.5
		}
		if w.UsedPercent != want {
			t.Fatalf("ratio did not replace rounded legacy usage: %+v", w)
		}
	}
}

func TestFetchUsagePreservesQuotaQuantitiesWithoutInventingRatioCounts(t *testing.T) {
	windows := fetchUsageFixture(t, `{"usage":{"limit":"200","remaining":"150"},"usages":{"limit_month_code":{"used_ratio":0.25}}}`)
	if len(windows) != 2 {
		t.Fatalf("quota windows: %+v", windows)
	}
	weekly, monthly := windows[0], windows[1]
	if weekly.Used == nil || *weekly.Used != 50 || weekly.Remaining == nil || *weekly.Remaining != 150 || weekly.Limit == nil || *weekly.Limit != 200 {
		t.Fatalf("legacy quantities lost: %+v", weekly)
	}
	if monthly.Used != nil || monthly.Remaining != nil || monthly.Limit != nil {
		t.Fatal("ratio was converted into fabricated counts")
	}
}

func TestFetchUsageRetainsLegacyResetWhenDetailedRatioOmitsIt(t *testing.T) {
	windows := fetchUsageFixture(t, `{"limits":[{"window":{"duration":300,"timeUnit":"TIME_UNIT_MINUTE"},"detail":{"remaining":90,"limit":100,"resetTime":"2026-12-09T00:00:00Z"}}],"usages":{"limit_5h":{"used_ratio":0.105}}}`)
	if len(windows) != 1 || windows[0].UsedPercent != 10.5 || windows[0].ResetAt.Format(time.RFC3339) != "2026-12-09T00:00:00Z" || windows[0].Limit == nil || windows[0].Remaining == nil {
		t.Fatalf("available quota detail was dropped: %+v", windows)
	}
}

func TestFetchUsageRejectsInvalidDetailedQuotaInsteadOfReportingPartialHeadroom(t *testing.T) {
	for _, ratio := range []string{`null`, `"NaN"`, `"+Inf"`, `-0.1`, `"invalid"`} {
		t.Run(ratio, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.WriteString(w, `{"limits":[{"window":{"duration":300,"timeUnit":"TIME_UNIT_MINUTE"},"detail":{"limit":100,"remaining":100}}],"usages":{"limit_month_total":{"used_ratio":`+ratio+`}}}`)
			}))
			defer server.Close()
			old := usageURL
			usageURL = server.URL
			t.Cleanup(func() { usageURL = old })
			if _, _, err := (Store{}).FetchUsage(t.Context(), server.Client(), account.Account{Token: "fixture-access"}); err == nil {
				t.Fatal("invalid monthly quota was hidden behind healthy session quota")
			}
		})
	}
}
