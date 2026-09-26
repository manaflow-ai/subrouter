package main

import (
	"bytes"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
)

func win(name string, used float64, limit, reset int64) accounts.UsageWindow {
	return accounts.UsageWindow{Name: name, UsedPercent: used, LimitWindowSeconds: limit, ResetAfterSeconds: reset}
}

func credit(remaining int) *accounts.ComplimentaryResetInfo {
	return &accounts.ComplimentaryResetInfo{Known: true, Available: true, Remaining: &remaining}
}

// tempWindows: only the 5h window is maxed; the weekly window is healthy.
// A credit would restart the weekly window and waste 80% of it.
func tempWindows(fiveHourReset int64) []accounts.UsageWindow {
	return []accounts.UsageWindow{
		win("primary", 100, 18000, fiveHourReset),
		win("secondary", 20, 604800, 586000),
	}
}

// weeklyCookedWindows: the weekly window is full and resets on its own
// after weeklyReset seconds.
func weeklyCookedWindows(weeklyReset int64) []accounts.UsageWindow {
	return []accounts.UsageWindow{win("primary", 100, 604800, weeklyReset)}
}

func TestGTOResetCandidatesRankByWeeklyWaitAndSkipShortOnly(t *testing.T) {
	codexRow := func(email string, windows []accounts.UsageWindow, cooked, temp bool, reset *accounts.ComplimentaryResetInfo) srUsageRow {
		return srUsageRow{email: email, authMode: accounts.AuthModeOAuth, provider: accounts.ProviderCodex,
			windows: windows, cooked: cooked, tempCooked: temp, complimentaryReset: reset}
	}
	rows := []srUsageRow{
		codexRow("usable@x.com", nil, false, false, nil),
		codexRow("short-only@x.com", tempWindows(360), false, true, credit(3)),
		codexRow("late-cook@x.com", weeklyCookedWindows(3600*20), true, false, credit(1)),
		codexRow("early-cook@x.com", weeklyCookedWindows(3600*24*5), true, false, credit(2)),
		codexRow("no-credit@x.com", weeklyCookedWindows(3600*24*6), true, false, &accounts.ComplimentaryResetInfo{Known: true, Available: false}),
	}
	usable, cands := gtoResetCandidates(rows)
	if usable != 1 {
		t.Fatalf("usableNow = %d, want 1 (a 5h-cooked account is not usable either)", usable)
	}
	var order []string
	for _, c := range cands {
		order = append(order, c.email)
	}
	if got := strings.Join(order, ","); got != "early-cook@x.com,late-cook@x.com" {
		t.Fatalf("candidates = %s, want weekly-cooked accounts by longest wait; the 5h-only account must never be a candidate", got)
	}
	if v := cands[0].windowValue(); v < 0.71 || v > 0.72 {
		t.Fatalf("5-day wait value = %.3f, want 5/7", v)
	}
}

func TestAssessResetValue(t *testing.T) {
	day := int64(24 * 3600)
	v, ok := assessResetValue(2, []gtoResetCandidate{{email: "a", weeklyWaitSeconds: 5 * day}})
	if !ok || !strings.Contains(v, "GOOD VALUE") || !strings.Contains(v, "71%") || !strings.Contains(v, "2 Codex account(s) are still usable") {
		t.Fatalf("a 5-day wait is a good use even with usable accounts around; got ok=%v %q", ok, v)
	}
	v, ok = assessResetValue(0, []gtoResetCandidate{{email: "a", weeklyWaitSeconds: 2 * day}})
	if !ok || !strings.Contains(v, "FAIR VALUE") {
		t.Fatalf("2-day wait: ok=%v %q", ok, v)
	}
	v, ok = assessResetValue(0, []gtoResetCandidate{{email: "a", weeklyWaitSeconds: 3600}})
	if ok || !strings.Contains(v, "LOW VALUE") {
		t.Fatalf("1-hour wait: ok=%v %q", ok, v)
	}
	if _, ok := assessResetValue(0, nil); ok {
		t.Fatal("no candidates should not be worthwhile")
	}
}

func TestUsageGridResetCellShowsCount(t *testing.T) {
	rem := 3
	row := srUsageRow{
		provider: accounts.ProviderCodex, authMode: accounts.AuthModeOAuth,
		complimentaryReset: &accounts.ComplimentaryResetInfo{Known: true, Available: true, Remaining: &rem},
	}
	if got := usageGridResetCell(row).Text; got != "3 left" {
		t.Fatalf("reset cell = %q, want %q (count must win over 'avail')", got, "3 left")
	}
	zero := 0
	row.complimentaryReset = &accounts.ComplimentaryResetInfo{Known: true, Remaining: &zero}
	if got := usageGridResetCell(row).Text; got != "used" {
		t.Fatalf("zero-credit cell = %q, want used", got)
	}
}

func TestPrintAccountCountSummary(t *testing.T) {
	rows := []srUsageRow{
		{email: "a@x.com", provider: accounts.ProviderCodex},
		{email: "b@x.com", provider: accounts.ProviderCodex},
		{email: "default", provider: accounts.ProviderClaude},
	}
	var out bytes.Buffer
	printAccountCountSummary(&out, rows)
	got := out.String()
	if !strings.Contains(got, "2 Codex") || !strings.Contains(got, "1 Claude profile") || !strings.Contains(got, "(3 accounts)") {
		t.Fatalf("summary = %q, want counts + total", got)
	}
}

func TestSoonestCreditExpiryAndFormat(t *testing.T) {
	now := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	credits := []accounts.RateLimitResetCredit{
		{ExpiresAt: now.Add(72 * time.Hour).Format(time.RFC3339)},
		{ExpiresAt: now.Add(24 * time.Hour).Format(time.RFC3339)},
		{ExpiresAt: ""}, // no expiry, ignored
	}
	exp, ok := soonestCreditExpiry(credits)
	if !ok {
		t.Fatal("expected an expiry")
	}
	if got := formatExpiryFromNow(exp, now); !strings.Contains(got, "expires in 1d") {
		t.Fatalf("format = %q, want soonest (1d)", got)
	}
	if _, ok := soonestCreditExpiry([]accounts.RateLimitResetCredit{{ExpiresAt: ""}}); ok {
		t.Fatal("no parseable expiry should report none")
	}
}

func TestPrintResetCredits(t *testing.T) {
	now := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	entries := []resetCreditsAccount{
		{Email: "a@x.com", Count: 2, Credits: []accounts.RateLimitResetCredit{{ExpiresAt: now.Add(48 * time.Hour).Format(time.RFC3339)}}},
		{Email: "b@x.com", Count: 0},
		{Email: "c@x.com", Error: "token expired"},
	}
	var out bytes.Buffer
	printResetCredits(&out, now, entries)
	got := out.String()
	if !strings.Contains(got, "a@x.com") || !strings.Contains(got, "expires in 2d") {
		t.Fatalf("missing account/expiry: %q", got)
	}
	if strings.Contains(got, "b@x.com") {
		t.Fatalf("zero-credit account should be omitted: %q", got)
	}
	if !strings.Contains(got, "token expired") {
		t.Fatalf("error account should be shown: %q", got)
	}
	if !strings.Contains(got, "2 reset credit(s) across 1 account(s)") {
		t.Fatalf("totals wrong: %q", got)
	}
}

func TestDisplayUsageRowsPerGroupRestartsNumbering(t *testing.T) {
	rem := 1
	rows := []srUsageRow{
		{email: "codexone@x.com", provider: accounts.ProviderCodex, authMode: accounts.AuthModeOAuth, complimentaryReset: &accounts.ComplimentaryResetInfo{Known: true, Remaining: &rem}},
		{email: "codextwo@x.com", provider: accounts.ProviderCodex, authMode: accounts.AuthModeOAuth},
		{email: "claudeprof", provider: accounts.ProviderClaude},
	}
	var out bytes.Buffer
	displayUsageRowsPerGroup(&out, rows)
	ansi := regexp.MustCompile(`\x1b\[[0-9;]*m`)
	nums := map[string]string{}
	for _, line := range strings.Split(ansi.ReplaceAllString(out.String(), ""), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		for _, e := range []string{"codexone@x.com", "codextwo@x.com", "claudeprof"} {
			if strings.Contains(line, e) {
				nums[e] = fields[0]
			}
		}
	}
	if nums["codexone@x.com"] != "1" || nums["codextwo@x.com"] != "2" {
		t.Fatalf("codex numbering = %v, want 1,2", nums)
	}
	if nums["claudeprof"] != "1" {
		t.Fatalf("claude numbering = %q, want restart at 1", nums["claudeprof"])
	}
}
