package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/manaflow-ai/subrouter/internal/accounts"
)

func pickerFixture() ([]remoteServerAccount, []remoteServerUsageStatus) {
	oauth := func(id string) remoteServerAccount {
		return remoteServerAccount{ID: id, Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Label: id}
	}
	eligible := []remoteServerAccount{oauth("default"), oauth("healthy-low"), oauth("healthy-high"), oauth("protected"), oauth("on-hold")}
	window := func(used float64) []accounts.UsageWindow {
		return []accounts.UsageWindow{
			{Name: "five_hour", UsedPercent: used, LimitWindowSeconds: 5 * 3600, ResetAfterSeconds: 3600},
			{Name: "seven_day", UsedPercent: used, LimitWindowSeconds: 7 * 86400, ResetAfterSeconds: 86400},
		}
	}
	status := func(id string, used float64, errText string) remoteServerUsageStatus {
		return remoteServerUsageStatus{ID: id, Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Label: id, Windows: window(used), Error: errText, AuthChecked: true, AuthValid: errText == ""}
	}
	statuses := []remoteServerUsageStatus{
		status("default", 0, `Claude OAuth refresh failed: 400 Bad Request: {"error": "invalid_grant"}`),
		status("healthy-low", 50, ""),
		status("healthy-high", 10, ""),
		status("protected", 80, ""),
		status("on-hold", 0, `Claude OAuth refresh failed: 403: {"error":{"type":"account_on_hold"}}`),
	}
	return eligible, statuses
}

func TestClaudeAccountPickerOrdersByHealthAndDefaultsToHealthiest(t *testing.T) {
	eligible, statuses := pickerFixture()
	picker := newClaudeAccountPicker(eligible, statuses)
	var order []string
	for _, entry := range picker.entries {
		order = append(order, entry.account.ID)
	}
	want := []string{"healthy-high", "healthy-low", "protected", "default", "on-hold"}
	if strings.Join(order, ",") != strings.Join(want, ",") {
		t.Fatalf("order = %v, want %v", order, want)
	}
	if picker.defaultIndex != 0 {
		t.Fatalf("default index = %d", picker.defaultIndex)
	}
	for _, pinned := range []bool{true, false} {
		id, chosen, err := picker.choose("", pinned, eligible)
		if err != nil || !chosen || id != "healthy-high" {
			t.Fatalf("Enter (pinned=%v) = %q %v %v, want healthy-high", pinned, id, chosen, err)
		}
	}
	if id, chosen, err := picker.choose("0", false, eligible); err != nil || !chosen || id != "" {
		t.Fatalf("0 = automatic, got %q %v %v", id, chosen, err)
	}

	var out bytes.Buffer
	picker.display(&out, false)
	text := out.String()
	for _, want := range []string{
		"Recommended: 1) healthy-high",
		"default (profile name)",
		"Unusable, cannot be picked: 4, 5",
		"invalid_grant",
		"sr add claude",
		"restricted by Anthropic",
		"protected",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("picker display missing %q:\n%s", want, text)
		}
	}
	if !strings.Contains(picker.prompt(false), "Enter = 1") {
		t.Fatalf("prompt = %q", picker.prompt(false))
	}
}

func TestClaudeAccountPickerRefusesBrokenAccounts(t *testing.T) {
	eligible, statuses := pickerFixture()
	picker := newClaudeAccountPicker(eligible, statuses)
	for _, answer := range []string{"4", "5", "default", "on-hold"} {
		_, _, err := picker.choose(answer, true, eligible)
		var unusable *claudePickerUnusableError
		if !errors.As(err, &unusable) {
			t.Fatalf("choose(%q) = %v, want an unusable-account refusal", answer, err)
		}
	}
	if _, _, err := picker.choose("4", true, eligible); err == nil || !strings.Contains(err.Error(), "needs re-login") {
		t.Fatalf("refusal should say the account needs re-login: %v", err)
	}
	if id, _, err := picker.choose("3", true, eligible); err != nil || id != "protected" {
		t.Fatalf("a protected account stays selectable: %q %v", id, err)
	}
}

func TestClaudeAccountPickerWithoutUsageKeepsLegacyBehavior(t *testing.T) {
	eligible, _ := pickerFixture()
	picker := newClaudeAccountPicker(eligible, nil)
	if picker.defaultIndex != -1 {
		t.Fatalf("no usage means no recommendation, got %d", picker.defaultIndex)
	}
	if _, chosen, err := picker.choose("", true, eligible); err != nil || chosen {
		t.Fatalf("pinned Enter without a recommendation cancels, got %v %v", chosen, err)
	}
	if id, chosen, err := picker.choose("", false, eligible); err != nil || !chosen || id != "" {
		t.Fatalf("pooled Enter without a recommendation is automatic, got %q %v %v", id, chosen, err)
	}
	var out bytes.Buffer
	picker.display(&out, true)
	if !strings.Contains(out.String(), "1) default (profile name)") {
		t.Fatalf("display = %s", out.String())
	}
}

func TestClaudeAccountPickerNoHealthyAccountHasNoDefault(t *testing.T) {
	eligible, statuses := pickerFixture()
	eligible = eligible[:1] // only the dead "default"
	picker := newClaudeAccountPicker(eligible, statuses)
	if picker.defaultIndex != -1 {
		t.Fatal("a broken account must never be the Enter default")
	}
	if _, chosen, _ := picker.choose("", true, eligible); chosen {
		t.Fatal("pinned Enter with no healthy account must cancel")
	}
}
