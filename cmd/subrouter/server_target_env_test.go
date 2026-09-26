package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/manaflow-ai/subrouter/internal/accounts"
)

func TestExplicitServerTargetPrecedence(t *testing.T) {
	for _, tc := range []struct {
		server, codex, want string
	}{
		{"", "", ""},
		{"", "legacy", "legacy"},
		{"neutral", "", "neutral"},
		{"neutral", "legacy", "neutral"},
		{"  ", " legacy ", "legacy"},
		{"local", "legacy", "local"},
	} {
		t.Setenv("SUBROUTER_SERVER", tc.server)
		t.Setenv("SUBROUTER_CODEX_SERVER", tc.codex)
		if got := explicitServerTarget(); got != tc.want {
			t.Fatalf("SUBROUTER_SERVER=%q SUBROUTER_CODEX_SERVER=%q: target = %q, want %q", tc.server, tc.codex, got, tc.want)
		}
	}
}

// SUBROUTER_SERVER must be an explicit one-command target for sr account
// commands exactly like SUBROUTER_CODEX_SERVER, even when this machine
// normally uses the team vault. Before, run() only looked at the Codex name
// while selectedRemoteServer preferred SUBROUTER_SERVER.
func TestSRCommandHonorsProviderNeutralServerEnvOverTeamVault(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	saveReadyCloudConfig(t)
	var hits atomic.Int32
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		hits.Add(1)
		if req.URL.Path == "/_subrouter/accounts" {
			_ = json.NewEncoder(w).Encode([]map[string]any{})
			return
		}
		http.NotFound(w, req)
	}))
	defer remote.Close()
	store := accounts.DefaultCodexStore()
	if err := defaultSRServerStore(store).save(srServerFile{Servers: []srServerConfig{{Name: "neutral", URL: remote.URL, AdminToken: "admin"}}}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SUBROUTER_CODEX_SERVER", "")
	t.Setenv("SUBROUTER_SERVER", "neutral")
	var out bytes.Buffer
	runner := srRunner{store: store, out: &out, errOut: &out, client: remote.Client()}
	if err := runner.run(context.Background(), []string{"list"}); err != nil {
		t.Fatalf("list: %v (%s)", err, out.String())
	}
	if hits.Load() == 0 || !strings.Contains(out.String(), "Server: neutral") {
		t.Fatalf("list did not target SUBROUTER_SERVER; hits=%d output=%q", hits.Load(), out.String())
	}
}
