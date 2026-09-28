package accounts

import (
	"encoding/json"
	"testing"
	"time"
)

func TestCodexWindowsCarryAbsoluteResetAt(t *testing.T) {
	raw := `{
		"rate_limit": {
			"primary_window": {"used_percent": 7, "limit_window_seconds": 18000, "reset_at": 1900000000},
			"secondary_window": {"used_percent": 2, "limit_window_seconds": 604800, "reset_after_seconds": 3600}
		}
	}`
	var usage codexUsageResponse
	if err := json.Unmarshal([]byte(raw), &usage); err != nil {
		t.Fatal(err)
	}
	before := time.Now()
	windows := usage.windows()
	after := time.Now()
	if got := windows[0].ResetAt; !got.Equal(time.Unix(1900000000, 0)) {
		t.Fatalf("primary ResetAt = %v, want the provider reset_at", got)
	}
	secondary := windows[1].ResetAt
	if secondary.Before(before.Add(time.Hour)) || secondary.After(after.Add(time.Hour)) {
		t.Fatalf("secondary ResetAt = %v, want fetch time + 1h", secondary)
	}
}

func TestUsageWindowResetAtWireFormat(t *testing.T) {
	reset := time.Date(2026, 9, 28, 3, 19, 0, 0, time.UTC)
	data, err := json.Marshal(UsageWindow{Name: "5h", ResetAfterSeconds: 60, ResetAt: reset})
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]any
	if err := json.Unmarshal(data, &wire); err != nil {
		t.Fatal(err)
	}
	if wire["reset_at"] != "2026-09-28T03:19:00Z" || wire["ResetAfterSeconds"] != float64(60) {
		t.Fatalf("wire = %s, want reset_at RFC3339 and ResetAfterSeconds kept", data)
	}
	data, _ = json.Marshal(UsageWindow{Name: "5h"})
	var empty map[string]any
	if err := json.Unmarshal(data, &empty); err != nil || empty["reset_at"] != nil {
		t.Fatalf("zero ResetAt should be omitted: %s", data)
	}
}

func TestResetTimeFallsBackForOldServerPayload(t *testing.T) {
	// An older server sends only ResetAfterSeconds, relative to its fetch.
	var window UsageWindow
	if err := json.Unmarshal([]byte(`{"Name":"5h","UsedPercent":40,"ResetAfterSeconds":600}`), &window); err != nil {
		t.Fatal(err)
	}
	if !window.ResetAt.IsZero() {
		t.Fatalf("ResetAt = %v, want zero", window.ResetAt)
	}
	fetchedAt := time.Date(2026, 9, 28, 3, 0, 0, 0, time.UTC)
	if got, want := window.ResetTime(fetchedAt), fetchedAt.Add(10*time.Minute); !got.Equal(want) {
		t.Fatalf("ResetTime = %v, want %v", got, want)
	}
	if got := window.ResetTime(time.Time{}); !got.IsZero() {
		t.Fatalf("ResetTime without fetch time = %v, want zero", got)
	}
	if got := ResetsAsOf([]UsageWindow{window}, fetchedAt.Add(time.Hour))[0].ResetAfterSeconds; got != 600 {
		t.Fatalf("ResetsAsOf changed an old-server window to %d", got)
	}
}

func TestResetTimePrefersResetAt(t *testing.T) {
	reset := time.Date(2026, 9, 28, 3, 19, 0, 0, time.UTC)
	window := UsageWindow{ResetAfterSeconds: 9999, ResetAt: reset}
	if got := window.ResetTime(reset.Add(-time.Hour)); !got.Equal(reset) {
		t.Fatalf("ResetTime = %v, want %v", got, reset)
	}
	windows := []UsageWindow{window}
	got := ResetsAsOf(windows, reset.Add(-2*time.Minute))
	if got[0].ResetAfterSeconds != 120 || windows[0].ResetAfterSeconds != 9999 {
		t.Fatalf("ResetsAsOf = %d (input %d), want 120 without mutating input", got[0].ResetAfterSeconds, windows[0].ResetAfterSeconds)
	}
	if got := ResetsAsOf(windows, reset.Add(time.Minute))[0].ResetAfterSeconds; got != 0 {
		t.Fatalf("past reset = %d, want 0", got)
	}
}
