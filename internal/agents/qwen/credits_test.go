package qwen

import (
	"encoding/json"
	"math"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
)

func TestExtraCreditsKeepQuantitiesSeparateFromMonthlyQuota(t *testing.T) {
	var response any
	if err := json.Unmarshal([]byte(`{"data":{"DataV2":{"data":{"success":true,"data":{"totalCredits":40000,"remainingCredits":12500.75,"activeCount":2}}}}}`), &response); err != nil {
		t.Fatal(err)
	}
	credits, err := findExtraCredits(response)
	if err != nil || credits.Balance != "12500.75" || credits.Total == nil || *credits.Total != 40000 || credits.Unit != "Credits" || !credits.HasCredits {
		t.Fatalf("missing extra-pack quantities: err=%v", err)
	}
	windows := []accounts.UsageWindow{{Name: "monthly", UsedPercent: 100, ResetAt: time.Now().Add(time.Hour)}, {Name: "5h", UsedPercent: 100, LimitWindowSeconds: 18000}}
	serving := ServingQuotaWindows(windows, credits)
	if len(serving) != 1 || serving[0].Name != "5h" || len(windows) != 2 {
		t.Fatal("extra packs must preserve short rate limits and original monthly display")
	}
	if _, cooked := accounts.WeeklyCookedWindow(ServingQuotaWindows(windows[:1], credits)); cooked {
		t.Fatal("active extra Credits incorrectly marked the account exhausted")
	}
}

func TestExtraCreditsUnknownAndEmptyRemainDistinct(t *testing.T) {
	for _, bad := range []any{
		map[string]any{},
		map[string]any{"remainingCredits": nil, "totalCredits": float64(10)},
		map[string]any{"remainingCredits": float64(11), "totalCredits": float64(10)},
		map[string]any{"remainingCredits": float64(-1), "totalCredits": float64(10)},
		map[string]any{"remainingCredits": math.NaN(), "totalCredits": float64(10)},
		map[string]any{"success": false, "remainingCredits": float64(5), "totalCredits": float64(10)},
	} {
		if credits, err := findExtraCredits(bad); err == nil || credits != nil {
			t.Fatal("invalid or missing quantities were treated as a known balance")
		}
	}
	credits, err := findExtraCredits(map[string]any{"remainingCredits": float64(0), "totalCredits": float64(0)})
	if err != nil || credits == nil || credits.HasCredits || credits.Balance != "0" {
		t.Fatal("known zero extra-pack balance was lost")
	}
	windows := []accounts.UsageWindow{{Name: "monthly", UsedPercent: 100}}
	if len(ServingQuotaWindows(windows, credits)) != 1 {
		t.Fatal("empty extra packs hid monthly exhaustion")
	}
	credits.Balance, credits.HasCredits = "NaN", true
	if len(ServingQuotaWindows(windows, credits)) != 1 {
		t.Fatal("invalid balance authorized extra Credits")
	}
}
