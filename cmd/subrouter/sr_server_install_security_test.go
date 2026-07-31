package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/manaflow-ai/subrouter/internal/accounts"
)

func TestSRServerInstallDoesNotEnableTailscaleSSH(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("TAILSCALE_AUTH_KEY", "tailscale-auth-test-secret")
	store := accounts.DefaultCodexStore()
	fake := &recordingSRCommandRunner{}
	var out bytes.Buffer
	runner := srRunner{store: store, out: &out, errOut: &out, cmd: fake}

	if err := runner.run(context.Background(), []string{
		"server", "add", "community",
		"--url", "http://100.64.0.1:31415",
		"--gcp-instance", "subrouter-community",
		"--gcp-zone", "us-central1-a",
	}); err != nil {
		t.Fatal(err)
	}
	if err := runner.run(context.Background(), []string{"server", "install", "community"}); err != nil {
		t.Fatal(err)
	}

	installCommand := strings.Join(fake.commands[len(fake.commands)-1], " ")
	if strings.Contains(installCommand, "tailscale up") && strings.Contains(installCommand, " --ssh ") {
		t.Fatalf("server install enabled an unnecessary SSH service:\n%s", installCommand)
	}
}

func TestSRServerStoreTightensStaleTemporaryFilePermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "servers.json")
	if err := os.WriteFile(path+".tmp", []byte("stale\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	store := srServerStore{Path: path}
	if err := store.save(srServerFile{Servers: []srServerConfig{{
		Name:       "team",
		URL:        "https://subrouter.example.com",
		AdminToken: "secret-token",
	}}}); err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("server config mode = %o, want 600", got)
	}
}
