package accounts

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

type codexUsageTestTransport func(*http.Request) (*http.Response, error)

func (f codexUsageTestTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func TestFetchCodexUsageRejectsUnknownAccountQuota(t *testing.T) {
	for _, test := range []struct {
		name string
		body string
	}{
		{"missing rate limit", `{"plan_type":"pro"}`},
		{"empty rate limit", `{"plan_type":"pro","rate_limit":{}}`},
		{"null utilization", `{"rate_limit":{"primary_window":{"used_percent":null,"limit_window_seconds":18000}}}`},
		{"omitted utilization", `{"rate_limit":{"primary_window":{"limit_window_seconds":18000}}}`},
		{"negative utilization", `{"rate_limit":{"primary_window":{"used_percent":-1}}}`},
		{"over-100 utilization", `{"rate_limit":{"primary_window":{"used_percent":101}}}`},
		{"unknown weekly alongside known session", `{"rate_limit":{"primary_window":{"used_percent":20,"limit_window_seconds":18000},"secondary_window":{"used_percent":null,"limit_window_seconds":604800}}}`},
		{"model quota without account quota", `{"rate_limit":{},"additional_rate_limits":[{"limit_name":"model","rate_limit":{"primary_window":{"used_percent":0}}}]}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			client := &http.Client{Transport: codexUsageTestTransport(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(test.body))}, nil
			})}
			details, err := FetchCodexUsageDetails(context.Background(), client, Account{AuthMode: AuthModeOAuth, Token: "test-token"})
			if err == nil || !strings.Contains(err.Error(), "quota is unknown") {
				t.Fatalf("unknown quota returned details %+v, error %v", details, err)
			}
		})
	}
}

func TestFetchCodexUsageKeepsExplicitZeroAndReached(t *testing.T) {
	for _, body := range []string{
		`{"rate_limit":{"primary_window":{"used_percent":0,"limit_window_seconds":18000}}}`,
		`{"rate_limit":{"limit_reached":true,"primary_window":{"used_percent":null}}}`,
	} {
		client := &http.Client{Transport: codexUsageTestTransport(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body))}, nil
		})}
		details, err := FetchCodexUsageDetails(context.Background(), client, Account{AuthMode: AuthModeOAuth, Token: "test-token"})
		if err != nil || len(details.Windows) != 1 {
			t.Fatalf("known quota returned details %+v, error %v", details, err)
		}
		if strings.Contains(body, "limit_reached") {
			if details.Windows[0].Name != "reached" || details.Windows[0].UsedPercent != 100 {
				t.Fatalf("explicit exhaustion = %+v, want reached at 100%%", details.Windows)
			}
		} else if details.Windows[0].UsedPercent != 0 {
			t.Fatalf("explicit zero utilization = %+v", details.Windows)
		}
	}
}

func TestCodexDisplayWindowsOmitsUnknownModelUtilization(t *testing.T) {
	var usage codexUsageResponse
	if err := json.Unmarshal([]byte(`{"rate_limit":{"primary_window":{"used_percent":20}},"additional_rate_limits":[{"limit_name":"model","rate_limit":{"primary_window":{"used_percent":null}}}]}`), &usage); err != nil {
		t.Fatal(err)
	}
	if windows := usage.displayWindows(); len(windows) != 1 || windows[0].UsedPercent != 20 || windows[0].Feature != "" {
		t.Fatalf("display windows = %+v, want only the known account utilization", windows)
	}
}

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

func TestExtraUsageRemainingRejectsNegativeInputs(t *testing.T) {
	for name, info := range map[string]ExtraUsageInfo{
		"negative limit": {MonthlyLimit: float64Ptr(-1), UsedCredits: float64Ptr(0)},
		"negative used":  {MonthlyLimit: float64Ptr(1), UsedCredits: float64Ptr(-1)},
	} {
		t.Run(name, func(t *testing.T) {
			if remaining, ok := info.Remaining(); ok || remaining != 0 {
				t.Fatalf("Remaining() = %v, %v; want 0, false", remaining, ok)
			}
		})
	}
}

func float64Ptr(value float64) *float64 { return &value }
