package main

import (
	"io"
	"path/filepath"
	"reflect"
	"testing"
)

// Go's flag package stops at the first positional argument, so a flag typed
// after an account or server name used to be silently ignored. These tests
// pin that subcommands taking positionals accept flags on either side.

func TestQwenLoginAcceptsFlagsAfterAccount(t *testing.T) {
	selector, console, err := parseQwenLoginArgs([]string{"acct", "--console-account", "me@example.com"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if selector != "acct" || console != "me@example.com" {
		t.Fatalf("selector=%q console=%q", selector, console)
	}
}

func TestServerAddAcceptsFlagsBeforeName(t *testing.T) {
	store := srServerStore{Path: filepath.Join(t.TempDir(), "servers.json")}
	runner := srRunner{program: "sr", out: io.Discard, errOut: io.Discard}
	if err := runner.serverAdd(store, []string{"--url", "http://one.invalid:31415", "--no-codex-config", "team"}); err != nil {
		t.Fatal(err)
	}
	server, ok, err := store.find("team")
	if err != nil || !ok {
		t.Fatalf("find server: ok=%v err=%v", ok, err)
	}
	if server.URL != "http://one.invalid:31415" {
		t.Fatalf("URL = %q", server.URL)
	}
}

func TestServerAddRejectsExtraPositionalInsteadOfDroppingFlags(t *testing.T) {
	store := srServerStore{Path: filepath.Join(t.TempDir(), "servers.json")}
	runner := srRunner{program: "sr", out: io.Discard, errOut: io.Discard}
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CODEX_HOME", t.TempDir())
	err := runner.serverAdd(store, []string{"team", "--no-codex-config", "--url", "http://one.invalid:31415", "extra", "--default"})
	if err == nil {
		t.Fatal("server add accepted a stray positional and ignored the flags after it")
	}
	if _, ok, _ := store.find("team"); ok {
		t.Fatal("server was saved despite the usage error")
	}
}

func TestTenantArgsAcceptFlagsAfterPositionals(t *testing.T) {
	runner := srRunner{program: "sr", out: io.Discard, errOut: io.Discard}
	positional, _, remote, err := runner.parseTenantArgs("create", []string{"acme", "--server", "local"}, 1)
	if err != nil {
		t.Fatal(err)
	}
	if remote || !reflect.DeepEqual(positional, []string{"acme"}) {
		t.Fatalf("positional=%q remote=%v", positional, remote)
	}
}
