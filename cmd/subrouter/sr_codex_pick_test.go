package main

import (
	"bytes"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/manaflow-ai/subrouter/internal/accounts"
)

func TestTakeCodexAccountFlag(t *testing.T) {
	cases := []struct {
		args     []string
		selector string
		pick     bool
		rest     []string
	}{
		{nil, "", false, nil},
		{[]string{"exec", "hi"}, "", false, []string{"exec", "hi"}},
		{[]string{"--account"}, "", true, []string{}},
		{[]string{"--account", "--", "resume", "abc"}, "", true, []string{"resume", "abc"}},
		{[]string{"--account", "--model", "gpt"}, "", true, []string{"--model", "gpt"}},
		{[]string{"--account", "work", "--", "exec", "hi"}, "work", false, []string{"exec", "hi"}},
		{[]string{"--account=work", "resume", "abc"}, "work", false, []string{"resume", "abc"}},
	}
	for _, tc := range cases {
		options, rest, err := takeCodexAccountFlag(tc.args)
		if err != nil {
			t.Fatalf("%v: %v", tc.args, err)
		}
		if options.selector != tc.selector || options.pick != tc.pick || !reflect.DeepEqual(append([]string{}, rest...), append([]string{}, tc.rest...)) {
			t.Fatalf("%v: got %+v %#v", tc.args, options, rest)
		}
	}
	if _, _, err := takeCodexAccountFlag([]string{"--account="}); err == nil {
		t.Fatal("empty --account= accepted")
	}
	// A later --account belongs to Codex's own argv.
	if options, rest, _ := takeCodexAccountFlag([]string{"exec", "--account", "x"}); options.requested() || len(rest) != 3 {
		t.Fatalf("non-leading --account was taken: %+v %v", options, rest)
	}
}

func TestCodexAccountPickerUsesSharedHealth(t *testing.T) {
	codex := func(id string) remoteServerAccount {
		return remoteServerAccount{ID: id, Provider: accounts.ProviderCodex, AuthMode: accounts.AuthModeOAuth, Label: id}
	}
	eligible := []remoteServerAccount{codex("dead@example.com"), codex("ok@example.com"), codex("okish@example.com")}
	window := func(used float64) []accounts.UsageWindow {
		return []accounts.UsageWindow{
			{Name: "primary", UsedPercent: used, LimitWindowSeconds: 5 * 3600},
			{Name: "secondary", UsedPercent: used, LimitWindowSeconds: 7 * 86400},
		}
	}
	statuses := []remoteServerUsageStatus{
		{ID: "dead@example.com", Provider: accounts.ProviderCodex, AuthMode: accounts.AuthModeOAuth, Error: "codex refresh failed: invalid_grant"},
		{ID: "ok@example.com", Provider: accounts.ProviderCodex, AuthMode: accounts.AuthModeOAuth, Windows: window(10)},
		{ID: "okish@example.com", Provider: accounts.ProviderCodex, AuthMode: accounts.AuthModeOAuth, Windows: window(50)},
	}
	picker := newAccountPicker(accounts.ProviderCodex, eligible, statuses)
	if picker.entries[0].account.ID != "ok@example.com" || picker.entries[2].account.ID != "dead@example.com" {
		t.Fatalf("order = %v, %v, %v", picker.entries[0].account.ID, picker.entries[1].account.ID, picker.entries[2].account.ID)
	}
	if id, chosen, err := picker.choose("", true, nil); err != nil || !chosen || id != "ok@example.com" {
		t.Fatalf("Enter = %q %v %v", id, chosen, err)
	}
	_, _, err := picker.choose("3", true, nil)
	var unusable *claudePickerUnusableError
	if !errors.As(err, &unusable) || !strings.Contains(err.Error(), "sr add") || strings.Contains(err.Error(), "sr add claude") {
		t.Fatalf("broken Codex account refusal = %v", err)
	}
	if id, err := picker.resolveSelector("okish"); err != nil || id != "okish@example.com" {
		t.Fatalf("resolve okish = %q %v", id, err)
	}
	if _, err := picker.resolveSelector("ok"); err == nil {
		t.Fatal("ambiguous selector accepted")
	}
	if id, err := picker.resolveSelector("OK@example.com"); err != nil || id != "ok@example.com" {
		t.Fatalf("exact match = %q %v", id, err)
	}
	if err := picker.refuseBroken("dead@example.com"); err == nil {
		t.Fatal("--account with a dead account must be refused")
	}
	var out bytes.Buffer
	picker.display(&out, true)
	if !strings.Contains(out.String(), "Unusable, cannot be picked: 3") {
		t.Fatalf("display:\n%s", out.String())
	}
}
