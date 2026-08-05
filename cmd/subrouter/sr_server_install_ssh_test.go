package main

import (
	"bytes"
	"context"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/manaflow-ai/subrouter/internal/accounts"
)

// stdinCapturingRunner records commands with their standard input, so a test
// can prove a credential travelled over stdin rather than in argv.
type stdinCapturingRunner struct {
	mu       sync.Mutex
	commands [][]string
	stdins   []string
}

func (r *stdinCapturingRunner) Run(ctx context.Context, name string, args []string, stdin io.Reader, stdout io.Writer, stderr io.Writer) error {
	return r.RunWithEnv(ctx, name, args, nil, stdin, stdout, stderr)
}

func (r *stdinCapturingRunner) RunWithEnv(_ context.Context, name string, args []string, _ []string, stdin io.Reader, _ io.Writer, _ io.Writer) error {
	body := ""
	if stdin != nil {
		raw, _ := io.ReadAll(stdin)
		body = string(raw)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.commands = append(r.commands, append([]string{name}, args...))
	r.stdins = append(r.stdins, body)
	return nil
}

func (r *stdinCapturingRunner) Output(context.Context, string, []string) ([]byte, error) {
	return nil, nil
}

func TestServerInstallWithoutAnyTargetNamesBothOptions(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	store := accounts.DefaultCodexStore()
	if err := defaultSRServerStore(store).save(srServerFile{
		Servers: []srServerConfig{{Name: "mac-mini", URL: "http://100.64.0.9:31415"}},
	}); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	runner := srRunner{program: "sr", store: store, in: strings.NewReader(""), out: &out, errOut: &out}
	err := runner.run(context.Background(), []string{"server", "install", "mac-mini"})
	if err == nil {
		t.Fatal("expected install to fail without a target")
	}
	if !strings.Contains(err.Error(), "--ssh-host") {
		t.Fatalf("error did not offer the SSH option: %v", err)
	}
}

func TestServerInstallOverSSHSendsTokensOnStdin(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	store := accounts.DefaultCodexStore()
	if err := defaultSRServerStore(store).save(srServerFile{
		Servers: []srServerConfig{{
			Name:    "mac-mini",
			URL:     "http://100.64.0.9:31415",
			SSHHost: "worker@mac-mini",
		}},
	}); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	fake := &stdinCapturingRunner{}
	runner := srRunner{program: "sr", store: store, in: strings.NewReader(""), out: &out, errOut: &out, cmd: fake}
	if err := runner.run(context.Background(), []string{"server", "install", "mac-mini"}); err != nil {
		t.Fatal(err)
	}

	if len(fake.commands) != 1 {
		t.Fatalf("commands = %#v, want one ssh invocation", fake.commands)
	}
	command := fake.commands[0]
	if command[0] != "ssh" {
		t.Fatalf("command = %q, want ssh", command[0])
	}
	if !containsString(command, "worker@mac-mini") {
		t.Fatalf("ssh destination missing: %#v", command)
	}
	script := command[len(command)-1]
	if !strings.Contains(script, "install-launchd") || !strings.Contains(script, "Darwin)") {
		t.Fatalf("remote script does not select the macOS installer:\n%s", script)
	}
	if !strings.Contains(script, "--admin-token-stdin") || !strings.Contains(script, "--account-import-token-stdin") {
		t.Fatalf("remote script does not read tokens from stdin:\n%s", script)
	}

	server, ok, err := defaultSRServerStore(store).find("mac-mini")
	if err != nil || !ok {
		t.Fatalf("server lookup failed: %v", err)
	}
	if server.AdminToken == "" || server.AccountImportToken == "" {
		t.Fatal("install did not generate and persist both control tokens")
	}
	if server.AdminToken == server.AccountImportToken {
		t.Fatal("admin and account import tokens must differ")
	}
	// The tokens must reach the host over stdin only. Anything in argv is
	// visible in the remote process list.
	for _, secret := range []string{server.AdminToken, server.AccountImportToken} {
		for _, argument := range command {
			if strings.Contains(argument, secret) {
				t.Fatalf("token leaked into ssh arguments: %#v", command)
			}
		}
		if !strings.Contains(fake.stdins[0], secret) {
			t.Fatalf("token was not sent on stdin: %q", fake.stdins[0])
		}
	}
	if strings.Contains(out.String(), server.AdminToken) {
		t.Fatalf("install printed a token:\n%s", out.String())
	}
}

func TestServerAddPreservesSSHHostOnUpdate(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	store := accounts.DefaultCodexStore()

	var out bytes.Buffer
	runner := srRunner{program: "sr", store: store, in: strings.NewReader(""), out: &out, errOut: &out}
	if err := runner.run(context.Background(), []string{
		"server", "add", "mac-mini",
		"--url", "http://100.64.0.9:31415",
		"--ssh-host", "worker@mac-mini",
	}); err != nil {
		t.Fatal(err)
	}
	if err := runner.run(context.Background(), []string{
		"server", "add", "mac-mini",
		"--url", "http://100.64.0.10:31415",
	}); err != nil {
		t.Fatal(err)
	}

	server, ok, err := defaultSRServerStore(store).find("mac-mini")
	if err != nil || !ok {
		t.Fatalf("server lookup failed: %v", err)
	}
	if server.SSHHost != "worker@mac-mini" {
		t.Fatalf("ssh host = %q, want it preserved across an update", server.SSHHost)
	}
	if server.URL != "http://100.64.0.10:31415" {
		t.Fatalf("url = %q, want the updated value", server.URL)
	}
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
