package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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

func TestClaudePromptCacheHint(t *testing.T) {
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	for idle, want := range map[time.Duration]string{
		3 * time.Minute:  "likely warm",
		30 * time.Minute: "1h cache TTL was used",
		3 * time.Hour:    "no longer matters",
	} {
		if got := claudePromptCacheHint(now.Add(-idle), now); !strings.Contains(got, want) {
			t.Errorf("idle %v: hint %q, want %q", idle, got, want)
		}
	}
}

func TestClaudeAccountPickerResumeAffinity(t *testing.T) {
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	eligible, statuses := pickerFixture()

	// A healthy last account becomes the Enter default even with less headroom.
	picker := newClaudeAccountPicker(eligible, statuses)
	picker.applyResumeAffinity(sessionAccountSpan{AccountID: "healthy-low", Label: "healthy-low", To: now.Add(-3 * time.Minute)}, now)
	if id, _, _ := picker.choose("", true, eligible); id != "healthy-low" {
		t.Fatalf("Enter = %q, want the account that last ran the session", id)
	}
	var out bytes.Buffer
	picker.display(&out, true)
	for _, want := range []string{"Recommended: 2) healthy-low", "last ran this session, 3m ago", "prompt cache likely warm"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("display missing %q:\n%s", want, out.String())
		}
	}

	// A dead or protected last account never becomes the default.
	for _, dead := range []string{"default", "protected"} {
		picker = newClaudeAccountPicker(eligible, statuses)
		picker.applyResumeAffinity(sessionAccountSpan{AccountID: dead, To: now.Add(-time.Minute)}, now)
		if id, _, _ := picker.choose("", true, eligible); id != "healthy-high" {
			t.Fatalf("%s: Enter = %q, want the healthiest account", dead, id)
		}
		if !strings.Contains(picker.defaultReason, "recommending the healthiest") {
			t.Fatalf("%s: reason = %q", dead, picker.defaultReason)
		}
	}

	picker = newClaudeAccountPicker(eligible, statuses)
	picker.applyResumeAffinity(sessionAccountSpan{AccountID: "gone", To: now}, now)
	if picker.defaultIndex != 0 || !strings.Contains(picker.defaultReason, "no longer in the pool") {
		t.Fatalf("removed account: default %d reason %q", picker.defaultIndex, picker.defaultReason)
	}

	// Once the cache has expired, affinity buys nothing: keep the healthiest.
	picker = newClaudeAccountPicker(eligible, statuses)
	picker.applyResumeAffinity(sessionAccountSpan{AccountID: "healthy-low", To: now.Add(-2 * time.Hour)}, now)
	if picker.defaultIndex != 0 || !strings.Contains(picker.defaultReason, "expired") {
		t.Fatalf("cold session: default %d reason %q", picker.defaultIndex, picker.defaultReason)
	}

	// Without health data a pinned Enter must not pin an unknown account.
	picker = newClaudeAccountPicker(eligible, nil)
	picker.applyResumeAffinity(sessionAccountSpan{AccountID: "default", To: now}, now)
	if picker.defaultIndex != -1 {
		t.Fatalf("no-usage picker gained a default: %d", picker.defaultIndex)
	}

	// No healthy account: the reason must not claim a recommendation.
	picker = newClaudeAccountPicker(eligible[:1], statuses)
	picker.applyResumeAffinity(sessionAccountSpan{AccountID: "default", To: now}, now)
	if strings.Contains(picker.defaultReason, "recommending") || !strings.Contains(picker.defaultReason, "no account has headroom") {
		t.Fatalf("reason = %q", picker.defaultReason)
	}
}

// A pooled --resume must not steer the session back to an account that can
// no longer serve it; the pool picks instead, and the user is told why. An
// account whose health is merely unknown, or protected, keeps the preference.
func TestResumePreferenceHealthGate(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	store := accounts.DefaultCodexStore()
	_, statuses := pickerFixture()
	statuses = append(statuses, remoteServerUsageStatus{ID: "unpolled", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth})
	serverHTTP := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/_subrouter/usage-status" {
			http.NotFound(w, req)
			return
		}
		_ = json.NewEncoder(w).Encode(statuses)
	}))
	defer serverHTTP.Close()
	server := srServerConfig{Name: "team", URL: serverHTTP.URL}
	ledger := newSessionLedger(store.StoreDir())
	for session, account := range map[string]string{
		"s-dead": "default", "s-ok": "healthy-low", "s-protected": "protected",
		"s-unpolled": "unpolled", "s-missing": "not-in-status",
	} {
		if _, _, err := ledger.observe(sessionObservation{Agent: "claude", SessionID: session, AccountID: account, Label: account}); err != nil {
			t.Fatal(err)
		}
	}
	var errOut bytes.Buffer
	runner := srRunner{store: store, out: &bytes.Buffer{}, errOut: &errOut, client: serverHTTP.Client()}
	resume := func(session string) string {
		errOut.Reset()
		return runner.resumePreferredClaudeAccount(context.Background(), server, []string{"--resume", session}, "", "")
	}
	if got := resume("s-dead"); got != "" || !strings.Contains(errOut.String(), "cannot take a new session now, so the pool will pick") {
		t.Fatalf("dead account: %q %q", got, errOut.String())
	}
	for session, want := range map[string]string{"s-ok": "healthy-low", "s-protected": "protected", "s-unpolled": "unpolled", "s-missing": "not-in-status"} {
		if got := resume(session); got != want {
			t.Fatalf("%s: preferred %q, want %q (%s)", session, got, want, errOut.String())
		}
	}
	if got := resume("s-ok"); got == "" || !strings.Contains(errOut.String(), "prompt cache likely warm") {
		t.Fatalf("notice = %q", errOut.String())
	}

	// A cold session is not steered at all.
	ledger.now = func() time.Time { return time.Now().Add(-2 * time.Hour) }
	if _, _, err := ledger.observe(sessionObservation{Agent: "claude", SessionID: "s-cold", AccountID: "healthy-low"}); err != nil {
		t.Fatal(err)
	}
	if got := resume("s-cold"); got != "" || !strings.Contains(errOut.String(), "prompt cache has expired") {
		t.Fatalf("cold session: %q %q", got, errOut.String())
	}
}
