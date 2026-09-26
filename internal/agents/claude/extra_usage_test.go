package claude

import (
	"encoding/json"
	"testing"
)

func TestExtraUsageInfoFromUsageCarriesDisplayMetadata(t *testing.T) {
	body := []byte(`{
		"extra_usage": {
			"is_enabled": true,
			"monthly_limit": 5000,
			"used_credits": 877,
			"utilization": 17.54,
			"disabled_reason": "out_of_credits"
		},
		"spend": {
			"balance": {"amount_minor": 123, "currency": "USD", "exponent": 2},
			"auto_reload": false
		}
	}`)
	var usage UsageResponse
	if err := json.Unmarshal(body, &usage); err != nil {
		t.Fatalf("decode usage response: %v", err)
	}
	info := ExtraUsageInfoFromUsage(&usage)
	if info == nil {
		t.Fatal("expected extra usage info")
	}
	if !info.IsEnabled || info.DisabledReason != "out_of_credits" {
		t.Fatalf("unexpected enablement fields: %+v", info)
	}
	if info.CreditsBalance == nil || *info.CreditsBalance != 123 {
		t.Fatalf("credits balance = %v, want 123 cents", info.CreditsBalance)
	}
	if info.AutoReload == nil || *info.AutoReload {
		t.Fatalf("auto reload = %v, want false", info.AutoReload)
	}
	remaining, ok := info.Remaining()
	if !ok || remaining != 5000-877 {
		t.Fatalf("remaining = %v, %v; want 4123 cents", remaining, ok)
	}
}

func TestExtraUsageInfoFromUsageToleratesNullAndUnexpectedSpendShapes(t *testing.T) {
	for _, tc := range []struct {
		name       string
		body       string
		wantReload *bool
	}{
		{
			// The shape observed live: both fields null. Null auto_reload is
			// the never-enrolled state, which Claude's settings page renders
			// as "Auto-reload off".
			name:       "null fields",
			body:       `{"extra_usage": {"is_enabled": false, "monthly_limit": 5000, "used_credits": 1080}, "spend": {"balance": null, "auto_reload": null}}`,
			wantReload: func() *bool { b := false; return &b }(),
		},
		{
			// Defensive: auto_reload as an object must not fail the fetch.
			name:       "object auto_reload",
			body:       `{"extra_usage": {"is_enabled": true, "monthly_limit": 5000, "used_credits": 0}, "spend": {"balance": null, "auto_reload": {"enabled": true}}}`,
			wantReload: func() *bool { b := true; return &b }(),
		},
		{
			// No spend block at all: auto-reload state is unknown.
			name: "no spend block",
			body: `{"extra_usage": {"is_enabled": true, "monthly_limit": 5000, "used_credits": 0}}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var usage UsageResponse
			if err := json.Unmarshal([]byte(tc.body), &usage); err != nil {
				t.Fatalf("decode: %v", err)
			}
			info := ExtraUsageInfoFromUsage(&usage)
			if info == nil {
				t.Fatal("expected extra usage info")
			}
			if info.CreditsBalance != nil {
				t.Fatalf("unexpected credits balance: %v", *info.CreditsBalance)
			}
			switch {
			case tc.wantReload == nil && info.AutoReload != nil:
				t.Fatalf("auto reload = %v, want unknown", *info.AutoReload)
			case tc.wantReload != nil && (info.AutoReload == nil || *info.AutoReload != *tc.wantReload):
				t.Fatalf("auto reload = %v, want %v", info.AutoReload, *tc.wantReload)
			}
		})
	}
}

func TestExtraUsageInfoFromUsageNil(t *testing.T) {
	if ExtraUsageInfoFromUsage(nil) != nil {
		t.Fatal("nil usage must map to nil info")
	}
	if ExtraUsageInfoFromUsage(&UsageResponse{}) != nil {
		t.Fatal("missing extra_usage must map to nil info")
	}
}
