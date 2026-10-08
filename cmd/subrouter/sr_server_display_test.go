package main

import (
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
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
	if got := usageGridState(rows[0]); got != "throttled" {
		t.Fatalf("state = %q, want throttled", got)
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
