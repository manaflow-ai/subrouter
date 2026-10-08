package main

import (
	"bytes"
	"context"
	"flag"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/manaflow-ai/subrouter/internal/accounts"
)

var updateStatusGolden = flag.Bool("update-status-golden", false, "rewrite testdata/sr_status_text.golden from the current renderer")

const (
	srStatusUsageFixture = "testdata/sr_status_usage_status.json"
	srStatusTextGolden   = "testdata/sr_status_text.golden"
	srStatusServerURLKey = "http://SERVER"
)

// srStatusFixtureServer serves the recorded usage-status fixture and answers
// every other status section with 404, so the trailing best-effort sections
// stay silent and the rendered text depends only on the fixture.
func srStatusFixtureServer(t *testing.T) *httptest.Server {
	t.Helper()
	fixture, err := os.ReadFile(srStatusUsageFixture)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/_subrouter/usage-status" {
			http.NotFound(w, req)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(fixture)
	}))
	t.Cleanup(server.Close)
	return server
}

// runSRStatusAgainstFixture runs `sr <args>` against a default remote server
// that serves the fixture, the path `sr status` takes on a client whose
// credential storage points at a team router.
func runSRStatusAgainstFixture(t *testing.T, args []string) (string, error) {
	t.Helper()
	t.Setenv("COLUMNS", "200")
	t.Setenv("NO_COLOR", "1")
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("SUBROUTER_CLOUD_CONFIG", filepath.Join(home, "cloud.json"))
	store := accounts.DefaultCodexStore()
	server := srStatusFixtureServer(t)
	if err := defaultSRServerStore(store).save(srServerFile{
		Default: "team",
		Servers: []srServerConfig{{Name: "team", URL: server.URL, AdminToken: "fixture-admin-token"}},
	}); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	runner := srRunner{
		program: "sr",
		store:   store,
		out:     &out,
		errOut:  &out,
		client:  server.Client(),
	}
	err := runner.run(context.Background(), args)
	return strings.ReplaceAll(out.String(), server.URL, srStatusServerURLKey), err
}

// TestSRStatusTextMatchesGolden pins the human `sr status` table byte for
// byte, so machine-readable output can be added without moving a column.
func TestSRStatusTextMatchesGolden(t *testing.T) {
	got, err := runSRStatusAgainstFixture(t, []string{"status"})
	if err != nil {
		t.Fatal(err)
	}
	if *updateStatusGolden {
		if err := os.WriteFile(srStatusTextGolden, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(srStatusTextGolden)
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		t.Fatalf("sr status text changed.\n--- want\n%s\n--- got\n%s", want, got)
	}
}
