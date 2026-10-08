package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/manaflow-ai/subrouter/internal/accounts"
)

func ledgerLaunchFiles(t *testing.T, storeDir string) []string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(storeDir, sessionLedgerDirName, "launches", "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	return files
}

// TestPooledClaudeRejectsBadSettingsBeforeLedgerLaunch checks that a bad
// --settings value fails the pooled launch before sr records it in the
// session ledger or asks the server for anything.
func TestPooledClaudeRejectsBadSettingsBeforeLedgerLaunch(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("SUBROUTER_CLOUD_CONFIG", filepath.Join(home, "cloud.json"))
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		http.Error(w, "unexpected request", http.StatusTeapot)
	}))
	defer server.Close()
	store := accounts.CodexStore{Dir: filepath.Join(home, "store", "accounts")}
	if err := defaultSRServerStore(store).save(srServerFile{
		Default: "team",
		Servers: []srServerConfig{{Name: "team", URL: server.URL}},
	}); err != nil {
		t.Fatal(err)
	}
	runner := srRunner{store: store, client: server.Client(), in: strings.NewReader(""), out: io.Discard, errOut: io.Discard}
	for name, args := range map[string][]string{
		"missing file": {"--settings", filepath.Join(home, "missing.json"), "-p", "hi"},
		"bad json":     {"--settings", "{not json", "-p", "hi"},
		"no value":     {"-p", "hi", "--settings"},
	} {
		t.Run(name, func(t *testing.T) {
			err := runner.proxyClaudeSelectedRemote(t.Context(), args, claudeProxyLaunchOptions{})
			if err == nil || !strings.Contains(err.Error(), "--settings") {
				t.Fatalf("launch error = %v, want a --settings error", err)
			}
			if files := ledgerLaunchFiles(t, store.StoreDir()); len(files) != 0 {
				t.Fatalf("bad --settings left launch records: %v", files)
			}
		})
	}
	if got := requests.Load(); got != 0 {
		t.Fatalf("bad --settings still sent %d server request(s)", got)
	}
}

// TestPooledClaudeFinishesLedgerLaunchOnPreRunFailure checks that a launch
// that was recorded and then fails before Claude starts is closed with the
// error, so no open launch record remains.
func TestPooledClaudeFinishesLedgerLaunchOnPreRunFailure(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	store := accounts.CodexStore{Dir: filepath.Join(home, "store", "accounts")}
	runner := srRunner{store: store, in: strings.NewReader(""), out: io.Discard, errOut: io.Discard}
	args := []string{"--settings", filepath.Join(home, "missing.json"), "-p", "hi"}
	runner = runner.beginClaudeSessionLaunch("team", args, "", "")
	if runner.sessionLaunchID == "" {
		t.Fatal("launch was not recorded")
	}
	configDir := filepath.Join(home, "proxy-config")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := runner.launchProxyClaude(t.Context(), args, "http://127.0.0.1:31415", "subrouter", configDir, "", ""); err == nil {
		t.Fatal("launch with a missing --settings file succeeded")
	}
	record, ok, err := newSessionLedger(store.StoreDir()).loadLaunch(runner.sessionLaunchID)
	if err != nil || !ok {
		t.Fatalf("load launch: ok=%t err=%v", ok, err)
	}
	if record.EndedAt == nil || !strings.Contains(record.ExitError, "--settings") {
		t.Fatalf("pre-run failure left an open launch record: %+v", record)
	}
}
