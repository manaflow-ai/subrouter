package main

import (
	"bytes"
	"context"
	"errors"
	"github.com/manaflow-ai/subrouter/internal/accounts"
	"net/http"
	"strings"
	"testing"
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

func TestProviderFirstAddAliasesNormalizeWithoutChangingArguments(t *testing.T) {
	for _, provider := range []string{"codex", "claude"} {
		input := []string{provider, "add", "--device-auth"}
		got := normalizeProviderAddArgs(input)
		if strings.Join(got, " ") != "add "+provider+" --device-auth" {
			t.Fatalf("normalized %v", got)
		}
		if input[0] != provider {
			t.Fatal("normalization mutated caller arguments")
		}
	}
}

func TestRemoteAddClaudeDispatchesToClaudeEnrollment(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	called := false
	client := &http.Client{Transport: srRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Host != "api.anthropic.com" {
			t.Fatalf("wrong provider: %s", request.URL.Host)
		}
		called = true
		return nil, errors.New("stop before storing test credential")
	})}
	var out bytes.Buffer
	runner := srRunner{program: "sr", store: accounts.CodexStore{Dir: t.TempDir()}, in: strings.NewReader(""), out: &out, errOut: &out, client: client}
	err := runner.runRemoteAccountCommand(t.Context(), srServerConfig{Name: "selected", URL: "https://router.example.com"}, []string{"add", "claude", "work", "--token", testSetupToken})
	if err == nil || !called {
		t.Fatalf("Claude enrollment not reached: called=%v err=%v", called, err)
	}
}
