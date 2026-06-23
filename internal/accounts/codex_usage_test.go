package accounts

import (
	"encoding/json"
	"testing"
)

func TestDisplayWindowsTagsAdditionalLimitsWithFeature(t *testing.T) {
	// Mirrors a real chatgpt usage response: an account-wide rate_limit plus one
	// additional rate limit whose machine name is a rotating codename
	// (codex_bengalfox) but whose limit_name is the model display name. The
	// Feature must be set from limit_name so routing keys off it rather than the
	// codename or a display-string substring.
	raw := `{
		"plan_type": "pro",
		"rate_limit": {
			"allowed": true,
			"primary_window": {"used_percent": 7, "limit_window_seconds": 18000, "reset_after_seconds": 3600},
			"secondary_window": {"used_percent": 2, "limit_window_seconds": 604800, "reset_after_seconds": 600000}
		},
		"additional_rate_limits": [
			{
				"metered_feature": "codex_bengalfox",
				"limit_name": "GPT-5.3-Codex-Spark",
				"rate_limit": {
					"primary_window": {"used_percent": 0, "limit_window_seconds": 18000, "reset_after_seconds": 3600}
				}
			}
		]
	}`
	var usage codexUsageResponse
	if err := json.Unmarshal([]byte(raw), &usage); err != nil {
		t.Fatal(err)
	}

	accountWide := 0
	featureWindows := 0
	for _, w := range usage.displayWindows() {
		if w.Feature == "" {
			accountWide++
			continue
		}
		if w.Feature != "GPT-5.3-Codex-Spark" {
			t.Fatalf("window %q has feature %q, want GPT-5.3-Codex-Spark", w.Name, w.Feature)
		}
		featureWindows++
	}
	if accountWide == 0 {
		t.Fatal("expected account-wide windows tagged with an empty Feature")
	}
	if featureWindows == 0 {
		t.Fatal("expected the additional rate limit window tagged with its limit_name as Feature")
	}
}

func TestCodexUsageParsesComplimentaryResetInfo(t *testing.T) {
	raw := `{
		"plan_type": "pro",
		"rate_limit": {"allowed": true},
		"complimentary_session_reset": {
			"eligible": true,
			"available": false,
			"consumed": true,
			"used_count": 1,
			"total_count": 1
		}
	}`
	var usage codexUsageResponse
	if err := json.Unmarshal([]byte(raw), &usage); err != nil {
		t.Fatal(err)
	}

	info := usage.ComplimentaryReset
	if info == nil || !info.Known {
		t.Fatalf("complimentary reset info missing: %+v", usage.ComplimentaryReset)
	}
	if !info.Consumed || info.Available {
		t.Fatalf("reset state = consumed:%v available:%v, want consumed only", info.Consumed, info.Available)
	}
	if info.Eligible == nil || !*info.Eligible {
		t.Fatalf("eligible = %+v, want true", info.Eligible)
	}
	if info.Used == nil || *info.Used != 1 || info.Total == nil || *info.Total != 1 {
		t.Fatalf("counts = used:%v total:%v, want 1/1", info.Used, info.Total)
	}
	if info.Source != "complimentary_session_reset" {
		t.Fatalf("source = %q, want complimentary_session_reset", info.Source)
	}
}

func TestCodexUsageParsesOneTimeResetRemainingCount(t *testing.T) {
	raw := `{
		"plan_type": "pro",
		"rate_limit": {"allowed": true},
		"rewards": {
			"one_time_reset": {
				"remaining": 1,
				"total": 1
			}
		}
	}`
	var usage codexUsageResponse
	if err := json.Unmarshal([]byte(raw), &usage); err != nil {
		t.Fatal(err)
	}

	info := usage.ComplimentaryReset
	if info == nil || !info.Available || info.Consumed {
		t.Fatalf("reset state = %+v, want available", info)
	}
	if info.Source != "rewards.one_time_reset" {
		t.Fatalf("source = %q, want rewards.one_time_reset", info.Source)
	}
}
