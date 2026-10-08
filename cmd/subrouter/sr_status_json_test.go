package main

import (
	"encoding/json"
	"errors"
	"math"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
)

func srStatusCodexWindows(shortUsed, longUsed float64) []accounts.UsageWindow {
	return []accounts.UsageWindow{
		{Name: "primary", UsedPercent: shortUsed, LimitWindowSeconds: 5 * 3600, ResetAfterSeconds: 3600},
		{Name: "secondary", UsedPercent: longUsed, LimitWindowSeconds: 7 * 24 * 3600, ResetAfterSeconds: 5 * 86400},
	}
}

func srStatusCodexRow(shortUsed, longUsed float64) srUsageRow {
	windows := srStatusCodexWindows(shortUsed, longUsed)
	row := srUsageRow{
		email:    "acct",
		provider: accounts.ProviderCodex,
		authMode: accounts.AuthModeOAuth,
		windows:  windows,
		score:    scoreFromWindows("acct", windows),
	}
	row.cooked, row.cookedReason = cookedFromWindows(windows)
	row.tempCooked, row.tempCookedReason = tempCookedFromWindows(windows)
	return row
}

func TestSRStatusAccountStateMapping(t *testing.T) {
	healthy := srStatusCodexRow(10, 20)
	tests := []struct {
		name string
		row  func() srUsageRow
		want string
	}{
		{"ready", func() srUsageRow { return healthy }, srStatusStateReady},
		{"rec", func() srUsageRow { r := healthy; r.gtoRecommended = true; return r }, srStatusStateRec},
		{"active", func() srUsageRow { r := healthy; r.active = true; return r }, srStatusStateActive},
		{"active rec", func() srUsageRow { r := healthy; r.active = true; r.gtoRecommended = true; return r }, srStatusStateActive},
		{"protected", func() srUsageRow { return srStatusCodexRow(10, 75) }, srStatusStateProtected},
		{"active protected", func() srUsageRow { r := srStatusCodexRow(10, 75); r.active = true; return r }, srStatusStateActive},
		{"temp", func() srUsageRow { return srStatusCodexRow(100, 20) }, srStatusStateTemp},
		{"cooked", func() srUsageRow { return srStatusCodexRow(0, 100) }, srStatusStateCooked},
		{"active cooked", func() srUsageRow { r := srStatusCodexRow(0, 100); r.active = true; return r }, srStatusStateCooked},
		{"throttled", func() srUsageRow { r := healthy; r.usageThrottled = true; return r }, srStatusStateThrottled},
		{"error", func() srUsageRow { r := healthy; r.err = errors.New("refresh failed"); return r }, srStatusStateError},
		{"cooked error", func() srUsageRow { r := srStatusCodexRow(0, 100); r.err = errors.New("x"); return r }, srStatusStateError},
		{"keyed bad key", func() srUsageRow {
			return srUsageRow{email: "openrouter:k", provider: accounts.ProviderOpenRouter, authMode: accounts.AuthModeAPIKey, providerHealth: "bad key"}
		}, srStatusStateError},
		{"keyed exhausted", func() srUsageRow {
			return srUsageRow{email: "openrouter:k", provider: accounts.ProviderOpenRouter, authMode: accounts.AuthModeAPIKey, providerHealth: "auth ok", quotaStatus: "exhausted"}
		}, srStatusStateCooked},
		{"keyed unchecked", func() srUsageRow {
			return srUsageRow{email: "openrouter:k", provider: accounts.ProviderOpenRouter, authMode: accounts.AuthModeAPIKey}
		}, srStatusStateReady},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			row := tt.row()
			if got := srStatusAccountState(row); got != tt.want {
				t.Fatalf("state = %q, want %q (text State=%q Use=%q)", got, tt.want, usageGridState(row), compactPickReason(row))
			}
		})
	}
}

// TestSRStatusUsableMatchesTextMarkers keeps summary.usable equal to what a
// reader of the text table counts: rows whose State or Use cell says cooked,
// temp, or error are not usable.
func TestSRStatusUsableMatchesTextMarkers(t *testing.T) {
	rows := []srUsageRow{
		srStatusCodexRow(10, 20), srStatusCodexRow(10, 75), srStatusCodexRow(100, 20), srStatusCodexRow(0, 100),
	}
	errored := srStatusCodexRow(10, 20)
	errored.err = errors.New("usage unavailable")
	rows = append(rows, errored)
	for _, row := range rows {
		text := usageGridState(row) + " " + compactPickReason(row)
		textUsable := !strings.Contains(text, "cooked") && !strings.Contains(text, "temp") && !strings.Contains(text, "error") && !strings.Contains(text, "unavailable")
		if got := srStatusUsableState(srStatusAccountState(row)); got != textUsable {
			t.Fatalf("usable = %t, text %q says %t", got, text, textUsable)
		}
	}
}

func decodeSRStatusJSON(t *testing.T, text string) (srStatusJSON, map[string]any) {
	t.Helper()
	var typed srStatusJSON
	decoder := json.NewDecoder(strings.NewReader(text))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&typed); err != nil {
		t.Fatalf("decode typed: %v\n%s", err, text)
	}
	if decoder.More() {
		t.Fatalf("stdout holds more than one JSON document:\n%s", text)
	}
	var raw map[string]any
	if err := json.Unmarshal([]byte(text), &raw); err != nil {
		t.Fatal(err)
	}
	return typed, raw
}

func srStatusSortedKeys(object map[string]any) []string {
	keys := make([]string, 0, len(object))
	for key := range object {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func TestSRStatusJSONSchemaAgainstFixture(t *testing.T) {
	before := time.Now().UTC().Truncate(time.Second)
	text, err := runSRStatusAgainstFixture(t, []string{"status", "--json"})
	if err != nil {
		t.Fatal(err)
	}
	status, raw := decodeSRStatusJSON(t, text)

	if got, want := srStatusSortedKeys(raw), []string{"generated_at", "providers", "schema_version", "server"}; !slices.Equal(got, want) {
		t.Fatalf("top-level keys = %v, want %v", got, want)
	}
	if status.SchemaVersion != 1 || status.Server == nil || *status.Server != "team" {
		t.Fatalf("schema_version=%d server=%v", status.SchemaVersion, status.Server)
	}
	if _, err := time.Parse(time.RFC3339, status.GeneratedAt); err != nil || !strings.HasSuffix(status.GeneratedAt, "Z") {
		t.Fatalf("generated_at %q is not RFC3339 UTC: %v", status.GeneratedAt, err)
	}
	wantAccountKeys := []string{"extra_usage_usd", "id", "label", "plan", "provider", "session_left_pct", "session_reset_at", "state", "weekly_left_pct", "weekly_reset_at"}
	for providerName, provider := range raw["providers"].(map[string]any) {
		providerObject := provider.(map[string]any)
		if got := srStatusSortedKeys(providerObject); !slices.Equal(got, []string{"accounts", "summary"}) {
			t.Fatalf("%s keys = %v", providerName, got)
		}
		if got := srStatusSortedKeys(providerObject["summary"].(map[string]any)); !slices.Equal(got, []string{"total", "usable", "weekly_left_sum_pct"}) {
			t.Fatalf("%s summary keys = %v", providerName, got)
		}
		for _, account := range providerObject["accounts"].([]any) {
			if got := srStatusSortedKeys(account.(map[string]any)); !slices.Equal(got, wantAccountKeys) {
				t.Fatalf("%s account keys = %v, want %v (unknown fields must be null, not omitted)", providerName, got, wantAccountKeys)
			}
		}
	}
	if got := srStatusSortedKeys(raw["providers"].(map[string]any)); !slices.Equal(got, []string{"claude", "codex", "kimi"}) {
		t.Fatalf("providers = %v", got)
	}

	type expect struct {
		state          string
		plan           string
		session        float64
		weekly         float64
		extraUSD       float64
		hasSessionPct  bool
		hasWeeklyPct   bool
		hasSessionTime bool
		hasExtra       bool
	}
	want := map[string]expect{
		"codex-a1":  {state: "rec", plan: "pro", session: 90, weekly: 63, hasSessionPct: true, hasWeeklyPct: true, hasSessionTime: true},
		"codex-a2":  {state: "active", plan: "pro", session: 95, weekly: 20, hasSessionPct: true, hasWeeklyPct: true, hasSessionTime: true},
		"codex-a3":  {state: "cooked", plan: "team", session: 100, weekly: 0, hasSessionPct: true, hasWeeklyPct: true},
		"claude-a4": {state: "active", session: 99, weekly: 73, hasSessionPct: true, hasWeeklyPct: true, hasSessionTime: true},
		"claude-a5": {state: "protected", session: 80, weekly: 30, hasSessionPct: true, hasWeeklyPct: true, hasSessionTime: true, extraUSD: 0.98, hasExtra: true},
		"claude-a6": {state: "temp", session: 0, weekly: 34, hasSessionPct: true, hasWeeklyPct: true, hasSessionTime: true},
		"claude-a7": {state: "cooked", session: 100, weekly: 0, hasSessionPct: true, hasWeeklyPct: true, hasSessionTime: true, extraUSD: 37.5, hasExtra: true},
		"claude-a8": {state: "error"},
		"kimi:main": {state: "ready", plan: "API key"},
	}
	seen := 0
	for providerName, provider := range status.Providers {
		for _, account := range provider.Accounts {
			seen++
			exp, ok := want[account.ID]
			if !ok {
				t.Fatalf("unexpected account %q", account.ID)
			}
			if account.Provider != providerName {
				t.Fatalf("%s provider = %q under %q", account.ID, account.Provider, providerName)
			}
			if account.State != exp.state {
				t.Errorf("%s state = %q, want %q", account.ID, account.State, exp.state)
			}
			if (account.Plan == nil) != (exp.plan == "") || (account.Plan != nil && *account.Plan != exp.plan) {
				t.Errorf("%s plan = %v, want %q", account.ID, account.Plan, exp.plan)
			}
			checkPct := func(field string, got *float64, has bool, want float64) {
				if (got != nil) != has || (got != nil && math.Abs(*got-want) > 0.001) {
					t.Errorf("%s %s = %v, want %v (present=%t)", account.ID, field, got, want, has)
				}
			}
			checkPct("session_left_pct", account.SessionLeftPct, exp.hasSessionPct, exp.session)
			checkPct("weekly_left_pct", account.WeeklyLeftPct, exp.hasWeeklyPct, exp.weekly)
			checkPct("extra_usage_usd", account.ExtraUsageUSD, exp.hasExtra, exp.extraUSD)
			if (account.SessionResetAt != nil) != exp.hasSessionTime {
				t.Errorf("%s session_reset_at = %v, want present=%t", account.ID, account.SessionResetAt, exp.hasSessionTime)
			}
			for _, reset := range []*string{account.SessionResetAt, account.WeeklyResetAt} {
				if reset == nil {
					continue
				}
				parsed, err := time.Parse(time.RFC3339, *reset)
				if err != nil || !strings.HasSuffix(*reset, "Z") {
					t.Errorf("%s reset %q is not RFC3339 UTC", account.ID, *reset)
				}
				if parsed.Before(before) {
					t.Errorf("%s reset %q is in the past", account.ID, *reset)
				}
			}
		}
	}
	if seen != len(want) {
		t.Fatalf("saw %d accounts, want %d", seen, len(want))
	}
	if reset := status.Providers["codex"].Accounts[0].WeeklyResetAt; reset != nil {
		parsed, _ := time.Parse(time.RFC3339, *reset)
		if offset := parsed.Sub(before); offset < 72*time.Hour-time.Minute || offset > 72*time.Hour+time.Minute {
			t.Fatalf("codex-a1 weekly reset %s is %s after now, want 3d", *reset, offset)
		}
	}

	summaries := map[string]srStatusSummaryJSON{
		"codex":  {Usable: 2, Total: 3, WeeklyLeftSumPct: 83},
		"claude": {Usable: 2, Total: 5, WeeklyLeftSumPct: 103},
		"kimi":   {Usable: 1, Total: 1, WeeklyLeftSumPct: 0},
	}
	for name, wantSummary := range summaries {
		if got := status.Providers[name].Summary; got != wantSummary {
			t.Errorf("%s summary = %+v, want %+v", name, got, wantSummary)
		}
	}
}

func TestSRBareJSONMatchesStatusJSON(t *testing.T) {
	stripGeneratedAt := func(text string) srStatusJSON {
		status, _ := decodeSRStatusJSON(t, text)
		status.GeneratedAt = ""
		for _, provider := range status.Providers {
			for i := range provider.Accounts {
				provider.Accounts[i].SessionResetAt = nil
				provider.Accounts[i].WeeklyResetAt = nil
			}
		}
		return status
	}
	statusText, err := runSRStatusAgainstFixture(t, []string{"status", "--json"})
	if err != nil {
		t.Fatal(err)
	}
	bareText, err := runSRStatusAgainstFixture(t, []string{"--json"})
	if err != nil {
		t.Fatal(err)
	}
	statusJSON, _ := json.Marshal(stripGeneratedAt(statusText))
	bareJSON, _ := json.Marshal(stripGeneratedAt(bareText))
	if string(statusJSON) != string(bareJSON) {
		t.Fatalf("sr --json differs from sr status --json:\n%s\n%s", bareJSON, statusJSON)
	}
}

func TestSRStatusRejectsUnknownArguments(t *testing.T) {
	for _, args := range [][]string{{"status", "extra"}, {"status", "--yaml"}} {
		if _, err := runSRStatusAgainstFixture(t, args); err == nil {
			t.Fatalf("sr %v succeeded, want a usage error", args)
		}
	}
}
