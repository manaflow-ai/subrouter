package main

import (
	"strings"
	"testing"

	"github.com/manaflow-ai/subrouter/internal/accounts"
)

func win(name string, used float64, limit, reset int64) accounts.UsageWindow {
	return accounts.UsageWindow{Name: name, UsedPercent: used, LimitWindowSeconds: limit, ResetAfterSeconds: reset}
}

func credit(remaining int) *accounts.ComplimentaryResetInfo {
	return &accounts.ComplimentaryResetInfo{Known: true, Available: true, Remaining: &remaining}
}

// tempWindows: only the 5h window is maxed, weekly token healthy. A reset yields
// a fully usable account.
func tempWindows(fiveHourReset int64) []accounts.UsageWindow {
	return []accounts.UsageWindow{
		win("primary", 100, 18000, fiveHourReset),
		win("secondary", 20, 604800, 586000),
	}
}

// cookedWindows: the weekly request-count window is maxed too, so a 5h reset may
// not fully un-cook the account.
func cookedWindows(requestLimitReset int64) []accounts.UsageWindow {
	return []accounts.UsageWindow{
		win("primary", 100, 18000, 180),
		win("request-limit", 100, 604800, requestLimitReset),
		win("secondary", 20, 604800, 586000),
	}
}

func TestGTOResetMetricsTempVsCooked(t *testing.T) {
	hr, downtime, weekly := gtoResetMetrics(tempWindows(360))
	if weekly {
		t.Fatal("temp account should not be weekly exhausted")
	}
	if downtime != 360 {
		t.Fatalf("temp downtime = %d, want 360", downtime)
	}
	if hr < 0.79 || hr > 0.81 {
		t.Fatalf("temp post-reset headroom = %f, want ~0.80", hr)
	}

	hr, downtime, weekly = gtoResetMetrics(cookedWindows(540))
	if !weekly {
		t.Fatal("cooked account should be weekly exhausted (request-limit maxed)")
	}
	if downtime != 540 {
		t.Fatalf("cooked downtime = %d, want 540 (latest saturated reset)", downtime)
	}
	if hr != 0 {
		t.Fatalf("cooked post-reset headroom = %f, want 0 (weekly maxed)", hr)
	}
}

func TestGTOResetCandidatesRankingAndUsableCount(t *testing.T) {
	rows := []srUsageRow{
		{email: "usable@x.com", authMode: accounts.AuthModeOAuth, provider: accounts.ProviderCodex},
		{
			email: "cooked@x.com", authMode: accounts.AuthModeOAuth, provider: accounts.ProviderCodex,
			windows: cookedWindows(540), cooked: true, complimentaryReset: credit(2),
		},
		{
			email: "temp-more-headroom@x.com", authMode: accounts.AuthModeOAuth, provider: accounts.ProviderCodex,
			windows: tempWindows(360), tempCooked: true, complimentaryReset: credit(3),
		},
		{
			email: "no-credit@x.com", authMode: accounts.AuthModeOAuth, provider: accounts.ProviderCodex,
			windows: tempWindows(360), tempCooked: true, complimentaryReset: &accounts.ComplimentaryResetInfo{Known: true, Available: false},
		},
	}

	usable, cands := gtoResetCandidates(rows)
	if usable != 1 {
		t.Fatalf("usableNow = %d, want 1", usable)
	}
	if len(cands) != 2 {
		t.Fatalf("candidates = %d, want 2 (no-credit excluded)", len(cands))
	}
	// Temp (not weekly-exhausted, high headroom) must rank above the cooked one.
	if cands[0].email != "temp-more-headroom@x.com" {
		t.Fatalf("first candidate = %s, want temp-more-headroom@x.com", cands[0].email)
	}
	if cands[1].email != "cooked@x.com" {
		t.Fatalf("second candidate = %s, want cooked@x.com", cands[1].email)
	}
}

func TestAssessResetValue(t *testing.T) {
	// Anyone usable -> low value.
	if _, ok := assessResetValue(2, []gtoResetCandidate{{email: "a", downtimeSavedSeconds: 100000}}); ok {
		t.Fatal("usable accounts present should read low value")
	}
	// Everyone self-heals soon -> low value.
	v, ok := assessResetValue(0, []gtoResetCandidate{{email: "a", downtimeSavedSeconds: 360}})
	if ok || !strings.Contains(v, "LOW VALUE") {
		t.Fatalf("near-term recovery should be low value, got ok=%v verdict=%q", ok, v)
	}
	// All cooked, far-out recovery -> good value.
	v, ok = assessResetValue(0, []gtoResetCandidate{{email: "a", downtimeSavedSeconds: 3 * 60 * 60}})
	if !ok || !strings.Contains(v, "GOOD VALUE") {
		t.Fatalf("far recovery should be good value, got ok=%v verdict=%q", ok, v)
	}
	// No candidates.
	if _, ok := assessResetValue(0, nil); ok {
		t.Fatal("no candidates should not be worthwhile")
	}
}
