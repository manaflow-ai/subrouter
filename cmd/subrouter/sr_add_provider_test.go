package main

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
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

// "sr add codex --device-auth" must support headless enrollment without
// silently falling back to the browser OAuth flow.
func TestAddCodexWithDeviceAuthReachesIsolatedLoginWithFlag(t *testing.T) {
	for _, provider := range []string{"codex", "Codex", "openai", "chatgpt"} {
		t.Run(provider, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			store := accounts.DefaultCodexStore()
			auth := testCodexAuth("device@example.com", "acct_device")
			fake := &recordingSRCommandRunner{loginAuth: auth}
			var out bytes.Buffer
			runner := srRunner{program: "sr", store: store, in: strings.NewReader(""), out: &out, errOut: &out, cmd: fake}
			if err := runner.addProvider(t.Context(), []string{provider, "--device-auth"}); err != nil {
				t.Fatal(err)
			}
			if fake.countCommand("codex", "login", "--device-auth") != 1 {
				t.Fatal("expected exactly one isolated device-auth login")
			}
			for _, token := range []string{auth.Tokens.AccessToken, auth.Tokens.RefreshToken, auth.Tokens.IDToken} {
				if strings.Contains(out.String(), token) {
					t.Error("login output exposed an OAuth token")
				}
			}
		})
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

// Reject injected/unsupported arguments before running OAuth or making a
// request to a server. This also covers aliases and malformed boolean flags.
func TestAddCodexRejectsArgumentsBeforeSideEffects(t *testing.T) {
	for _, remote := range []bool{false, true} {
		path := "local"
		if remote {
			path = "remote"
		}
		t.Run(path, func(t *testing.T) {
			for _, provider := range []string{"codex", "openai", "chatgpt"} {
				for _, args := range [][]string{{"--bogus"}, {"--device-auth=invalid"}, {"--device-auth", "unexpected"}, {"--device-auth", "--config", "malicious"}} {
					t.Run(provider+"/"+strings.Join(args, "_"), func(t *testing.T) {
						t.Setenv("HOME", t.TempDir())
						fake := &recordingSRCommandRunner{}
						requests := 0
						client := &http.Client{Transport: srRoundTripFunc(func(*http.Request) (*http.Response, error) {
							requests++
							return nil, errors.New("unexpected network request")
						})}
						var out bytes.Buffer
						runner := srRunner{program: "sr", cmd: fake, client: client, out: &out, errOut: &out}
						input := append([]string{provider}, args...)
						var err error
						if remote {
							err = runner.runRemoteAccountCommand(t.Context(), srServerConfig{Name: "test", URL: "https://router.example.com"}, append([]string{"add"}, input...))
						} else {
							err = runner.addProvider(t.Context(), input)
						}
						if err == nil {
							t.Fatal("invalid arguments accepted")
						}
						if len(fake.commands) != 0 || requests != 0 {
							t.Fatalf("invalid arguments caused side effects: commands=%d requests=%d", len(fake.commands), requests)
						}
					})
				}
			}
		})
	}
}

func TestAddCodexDeviceAuthPreservesActiveAuthAndCleansTemporaryHome(t *testing.T) {
	for _, complete := range []bool{true, false} {
		name := "success"
		if !complete {
			name = "incomplete_auth"
		}
		t.Run(name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			t.Setenv("CODEX_HOME", t.TempDir())
			active := testCodexAuth("active@example.com", "acct_active")
			if err := accounts.WriteActiveCodexAuth(active); err != nil {
				t.Fatal(err)
			}
			activePath := accounts.DefaultCodexAuthPath()
			before, err := os.ReadFile(activePath)
			if err != nil {
				t.Fatal(err)
			}
			auth := testCodexAuth("device@example.com", "acct_device")
			if !complete {
				auth.Tokens.RefreshToken = ""
			}
			loginHome := ""
			fake := &recordingSRCommandRunner{loginAuth: auth, onLogin: func(env []string) {
				loginHome = filepath.Dir(authPathFromEnv(env))
				if loginHome == filepath.Dir(activePath) {
					t.Fatal("login reused active Codex home")
				}
				info, err := os.Stat(loginHome)
				if err != nil {
					t.Fatal(err)
				}
				if info.Mode().Perm() != 0o700 {
					t.Fatal("temporary login home is not private")
				}
			}}
			var out bytes.Buffer
			runner := srRunner{program: "sr", store: accounts.DefaultCodexStore(), cmd: fake, in: strings.NewReader(""), out: &out, errOut: &out}
			err = runner.addProvider(t.Context(), []string{"codex", "--device-auth"})
			if (err == nil) != complete {
				t.Fatalf("unexpected login result: complete=%t err=%v", complete, err)
			}
			after, err := os.ReadFile(activePath)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(before, after) {
				t.Error("active auth changed during isolated login")
			}
			if loginHome == "" {
				t.Fatal("isolated login did not run")
			}
			if _, err := os.Stat(loginHome); !os.IsNotExist(err) {
				t.Error("temporary login home was not removed")
			}
			stored, found, err := runner.store.FindStored("device@example.com")
			if err != nil {
				t.Fatal(err)
			}
			if found != complete {
				t.Fatalf("stored account presence = %t, want %t", found, complete)
			}
			if found && stored.OAuthCredentialOrigin != accounts.CodexOAuthOriginIsolatedServerLogin {
				t.Error("missing isolated login provenance")
			}
		})
	}
}
