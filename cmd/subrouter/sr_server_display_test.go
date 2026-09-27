package main

import (
	"testing"

	"github.com/manaflow-ai/subrouter/internal/accounts"
)

func TestServerUsageRowsShowLabelForOwnerKeyedCodexAccounts(t *testing.T) {
	rows := usageRowsFromServerUsageStatuses([]remoteServerUsageStatus{
		{ID: "codex-owner-9213a25e", Provider: accounts.ProviderCodex, AuthMode: accounts.AuthModeOAuth, Label: "lawrence@example.com [team]", Email: "lawrence@example.com", PlanType: "pro"},
		{ID: "lawrence@example.com", Provider: accounts.ProviderCodex, AuthMode: accounts.AuthModeOAuth, Label: "lawrence@example.com", Email: "lawrence@example.com", PlanType: "pro"},
		{ID: "pro@example.com", Provider: accounts.ProviderCodex, AuthMode: accounts.AuthModeOAuth, Label: "pro@example.com [pro]", Email: "pro@example.com", PlanType: "pro"},
		{ID: "apikey:ops", Provider: accounts.ProviderCodex, AuthMode: accounts.AuthModeAPIKey, Label: "ops"},
	})
	if len(rows) != 4 {
		t.Fatalf("rows = %d", len(rows))
	}
	if got := displayUsageAccountName(rows[0]); got != "lawrence@example.com [team]" {
		t.Fatalf("owner-keyed row = %q", got)
	}
	if got := displayUsageAccountName(rows[1]); got != "lawrence@example.com" {
		t.Fatalf("legacy row = %q", got)
	}
	if got := displayUsageAccountName(rows[2]); got != "pro@example.com" {
		t.Fatalf("plan label = %q, want plan suffix removed", got)
	}
	rows[1].displayAccount = ""
	if got := displayUsageAccountName(rows[1]); got != "lawrence@example.com" {
		t.Fatalf("legacy row = %q", got)
	}
	if got := displayUsageAccountName(rows[3]); got != "ops (api key)" {
		t.Fatalf("api key row = %q", got)
	}
}
