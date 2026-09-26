package main

import (
	"context"
	"runtime"
	"strings"
	"testing"

	baseaccount "github.com/manaflow-ai/subrouter/account"
)

func TestParseAntigravityNativeArgsSeparatesProfileSelection(t *testing.T) {
	selector, picker, vendor, err := parseAntigravityNativeArgs([]string{"--account", "daniel@example.com", "--", "--model", "gemini"})
	if err != nil {
		t.Fatal(err)
	}
	if selector != "daniel@example.com" || picker || strings.Join(vendor, " ") != "-- --model gemini" {
		t.Fatalf("selector=%q picker=%v vendor=%v", selector, picker, vendor)
	}
	selector, picker, vendor, err = parseAntigravityNativeArgs([]string{"--account", "--model", "gemini"})
	if err != nil || selector != "" || !picker || strings.Join(vendor, " ") != "--model gemini" {
		t.Fatalf("bare picker selector=%q picker=%v vendor=%v err=%v", selector, picker, vendor, err)
	}
}

func TestChooseAntigravityProfileAcceptsLabelEmailAndID(t *testing.T) {
	profiles := []baseaccount.Account{{ID: "antigravity-subscription:work", Label: "work", Email: "work@example.com"}}
	for _, selector := range []string{"work", "work@example.com", "antigravity-subscription:work"} {
		got, err := chooseAntigravityProfile(strings.NewReader(""), &strings.Builder{}, profiles, selector, false)
		if err != nil || got.Label != "work" {
			t.Fatalf("selector %q -> %+v, err=%v", selector, got, err)
		}
	}
}

func TestAntigravityNativeLaunchRequiresMacOSProfiles(t *testing.T) {
	if runtime.GOOS == "darwin" {
		t.Skip("covered by the macOS Keychain integration boundary")
	}
	err := (srRunner{}).launchAntigravityNative(context.Background(), nil)
	if err == nil || !strings.Contains(err.Error(), "requires macOS Keychain") {
		t.Fatalf("error=%v", err)
	}
}
