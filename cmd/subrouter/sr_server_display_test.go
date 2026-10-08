package main

import (
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/selectacct"
)

func TestServerUsageRowsClassify429AsThrottleAndKeepResetWindows(t *testing.T) {
	rows := usageRowsFromServerUsageStatuses([]remoteServerUsageStatus{{
		ID:       "claude@example.com",
		Provider: accounts.ProviderClaude,
		AuthMode: accounts.AuthModeOAuth,
		Error:    "usage fetch failed: 429 Too Many Requests",
		Windows: []accounts.UsageWindow{{
			Name: "7d", UsedPercent: 26, ResetAfterSeconds: int64(time.Hour / time.Second),
		}},
	}})
	if len(rows) != 1 || !rows[0].usageThrottled {
		t.Fatalf("rows = %+v, want one throttled row", rows)
	}
	if got := usageGridState(rows[0]); got != "error" {
		t.Fatalf("state = %q, want error", got)
	}
	if got := compactPickReason(rows[0]); got == "usage unavailable" || got == "usage throttled" {
		t.Fatalf("cached reset information was discarded: %q", got)
	}

	rows = usageRowsFromServerUsageStatuses([]remoteServerUsageStatus{
		{ID: "claude@example.com", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Error: "usage fetch failed: 401 Unauthorized"},
	})
	if len(rows) != 1 || rows[0].usageThrottled || rows[0].err == nil {
		t.Fatalf("401 row = %+v, want authentication error", rows)
	}
}

func TestServerUsageRowsTreatHeaderlessThrottleAsUsableWithoutQuotaEvidence(t *testing.T) {
	rows := usageRowsFromServerUsageStatuses([]remoteServerUsageStatus{{
		ID: "claude@example.com", Provider: accounts.ProviderClaude,
		AuthMode: accounts.AuthModeOAuth, UsageThrottled: true,
	}})
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want one", len(rows))
	}
	if got := usageGridState(rows[0]); got != "throttled" {
		t.Fatalf("state = %q, want throttled", got)
	}
	if !usableForNewSession(rows[0].score) {
		t.Fatalf("headerless telemetry throttle should remain score-eligible: %+v", rows[0])
	}
	if got := compactPickReason(rows[0]); got != "usage throttled" {
		t.Fatalf("Use = %q, want usage throttled", got)
	}
}

func TestCompactPickReasonDoesNotCallModelScopedThrottleFableOut(t *testing.T) {
	row := srUsageRow{
		provider:       accounts.ProviderClaude,
		authMode:       accounts.AuthModeOAuth,
		usageThrottled: true,
		windows: []accounts.UsageWindow{{
			Name: "oauth-apps-weekly", Feature: "claude-fable-5", UsedPercent: 100,
			LimitWindowSeconds: int64(7 * 24 * time.Hour / time.Second), ResetAfterSeconds: 30 * 60,
		}},
		score: selectacct.Score{AccountID: "claude@example.com", Headroom: 1, ShortHeadroom: 1},
	}
	if got := compactPickReason(row); got != "usage throttled" {
		t.Fatalf("model-scoped throttle Use = %q, want usage throttled", got)
	}
}

func TestCompactWindowStatusMarksStaleExhaustion(t *testing.T) {
	window := accounts.UsageWindow{
		Name: "5h", UsedPercent: 100, LimitWindowSeconds: int64(5 * time.Hour / time.Second),
		ResetAt: time.Now().Add(-time.Minute),
	}
	if got := compactWindowStatus(window); got != "0%/stale" {
		t.Fatalf("stale window = %q, want 0%%/stale", got)
	}
}

func TestServerUsageRowsShowLabelForOwnerKeyedCodexAccounts(t *testing.T) {
	rows := usageRowsFromServerUsageStatuses([]remoteServerUsageStatus{
		{ID: "codex-owner-9213a25e", Provider: accounts.ProviderCodex, AuthMode: accounts.AuthModeOAuth, Label: "lawrence@example.com", Email: "lawrence@example.com", PlanType: "team"},
		{ID: "lawrence@example.com", Provider: accounts.ProviderCodex, AuthMode: accounts.AuthModeOAuth, Label: "lawrence@example.com", Email: "lawrence@example.com"},
		{ID: "apikey:ops", Provider: accounts.ProviderCodex, AuthMode: accounts.AuthModeAPIKey, Label: "ops"},
	})
	if len(rows) != 3 {
		t.Fatalf("rows = %d", len(rows))
	}
	if got := displayUsageAccountName(rows[0]); got != "lawrence@example.com" {
		t.Fatalf("owner-keyed row = %q", got)
	}
	if got := usageGridPlan(rows[0]); got != "team" {
		t.Fatalf("owner-keyed plan column = %q, want team", got)
	}
	if got := displayUsageAccountName(rows[1]); got != "lawrence@example.com" {
		t.Fatalf("legacy row = %q", got)
	}
	rows[1].displayAccount = ""
	if got := displayUsageAccountName(rows[1]); got != "lawrence@example.com" {
		t.Fatalf("legacy row = %q", got)
	}
	if got := displayUsageAccountName(rows[2]); got != "ops (api key)" {
		t.Fatalf("api key row = %q", got)
	}
}
