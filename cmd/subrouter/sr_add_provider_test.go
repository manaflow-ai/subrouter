package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/manaflow-ai/subrouter/internal/accounts"
)

// "sr add" with no argument must not silently pick a provider. A Claude user
// who runs it and gets a ChatGPT login has been sent somewhere they did not ask
// to go, and on a pipe it must say what to run rather than block on a read.
func TestAddWithoutProviderRefusesNonInteractively(t *testing.T) {
	var out, errOut bytes.Buffer
	runner := srRunner{program: "sr", in: strings.NewReader(""), out: &out, errOut: &errOut}
	err := runner.addProvider(context.Background(), nil)
	if err == nil {
		t.Fatal("bare 'sr add' on a pipe did not error")
	}
	for _, want := range []string{"sr add codex", "sr add claude"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not tell the user to run %q", err.Error(), want)
		}
	}
}

func TestAddRejectsUnknownProvider(t *testing.T) {
	var out, errOut bytes.Buffer
	runner := srRunner{program: "sr", in: strings.NewReader(""), out: &out, errOut: &errOut}
	err := runner.addProvider(context.Background(), []string{"gemini"})
	if err == nil || !strings.Contains(err.Error(), "gemini") {
		t.Fatalf("error = %v, want it to name the unknown provider", err)
	}
	if !strings.Contains(err.Error(), "sr add codex") {
		t.Errorf("error %q does not suggest a valid provider", err.Error())
	}
}

// An unrecognized flag after "add codex" must name the binary the user
// actually ran, not a hardcoded "sr" -- this is also invoked as "subrouter"
// and "cx".
func TestAddCodexUsageErrorNamesActualProgram(t *testing.T) {
	var out, errOut bytes.Buffer
	runner := srRunner{program: "cx", in: strings.NewReader(""), out: &out, errOut: &errOut}
	err := runner.addProvider(context.Background(), []string{"codex", "--bogus-flag"})
	if err == nil || !strings.Contains(err.Error(), "cx add codex") {
		t.Fatalf("error = %v, want it to say %q", err, "cx add codex")
	}
	if strings.Contains(err.Error(), "sr add codex") {
		t.Fatalf("error = %v, hardcoded 'sr' instead of the running program", err)
	}
}

// "sr add codex --device-auth" is the only way to add a Codex account
// headlessly. It must not be swallowed silently and fall back to the
// browser OAuth flow.
func TestAddCodexWithDeviceAuthReachesIsolatedLoginWithFlag(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	store := accounts.DefaultCodexStore()
	fake := &recordingSRCommandRunner{loginAuth: testCodexAuth("device@example.com", "acct_device")}
	var out bytes.Buffer
	runner := srRunner{program: "sr", store: store, in: strings.NewReader(""), out: &out, errOut: &out, cmd: fake}
	if err := runner.addProvider(context.Background(), []string{"codex", "--device-auth"}); err != nil {
		t.Fatal(err)
	}
	if !fake.hasCommand("codex", "login", "--device-auth") {
		t.Fatalf("missing isolated login command with --device-auth: %#v", fake.commands)
	}
}

// The bare command must keep using the browser OAuth flow it always has;
// only an explicit --device-auth switches to device auth.
func TestAddCodexWithoutDeviceAuthOmitsFlag(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	store := accounts.DefaultCodexStore()
	fake := &recordingSRCommandRunner{loginAuth: testCodexAuth("browser@example.com", "acct_browser")}
	var out bytes.Buffer
	runner := srRunner{program: "sr", store: store, in: strings.NewReader(""), out: &out, errOut: &out, cmd: fake}
	if err := runner.addProvider(context.Background(), []string{"codex"}); err != nil {
		t.Fatal(err)
	}
	if !fake.hasCommand("codex", "login") {
		t.Fatalf("missing isolated login command: %#v", fake.commands)
	}
	if fake.hasCommand("codex", "login", "--device-auth") {
		t.Fatalf("bare 'sr add codex' must not pass --device-auth: %#v", fake.commands)
	}
}

// Aliases exist because users type what their vendor calls itself.
func TestProviderAliasesResolve(t *testing.T) {
	for _, alias := range []string{"codex", "Codex", "openai", "chatgpt", "claude", "CLAUDE", "anthropic"} {
		var out, errOut bytes.Buffer
		runner := srRunner{program: "sr", in: strings.NewReader(""), out: &out, errOut: &errOut}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		err := runner.addProvider(ctx, []string{alias})
		// These reach the real login paths, which fail in a test environment.
		// What matters is that they are not rejected as unknown providers.
		if err != nil && strings.Contains(err.Error(), "unknown provider") {
			t.Errorf("alias %q was rejected as unknown", alias)
		}
	}
}
