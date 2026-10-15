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
func srStatusFixtureServer(t *testing.T, fixturePaths ...string) *httptest.Server {
	t.Helper()
	fixturePath := srStatusUsageFixture
	if len(fixturePaths) > 0 {
		fixturePath = fixturePaths[0]
	}
	fixture, err := os.ReadFile(fixturePath)
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
func runSRStatusAgainstFixture(t *testing.T, args []string, fixturePaths ...string) (string, error) {
	t.Helper()
	if os.Getenv("COLUMNS") == "" {
		t.Setenv("COLUMNS", "200")
	}
	t.Setenv("NO_COLOR", "1")
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("SUBROUTER_CLOUD_CONFIG", filepath.Join(home, "cloud.json"))
	store := accounts.DefaultCodexStore()
	server := srStatusFixtureServer(t, fixturePaths...)
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

// Exercise the status command through the remote usage-status response, including
// tier/credit metadata and revoked credentials that previously crowded out quota.
func TestSRStatusQuotaTableFitsTerminal(t *testing.T) {
	for _, width := range []string{"200", "0"} {
		t.Run(width, func(t *testing.T) {
			t.Setenv("COLUMNS", width)
			assertSRStatusQuotaTable(t)
		})
	}
}

func assertSRStatusQuotaTable(t *testing.T) {
	t.Helper()
	got, err := runSRStatusAgainstFixture(t, []string{"status"}, "testdata/sr_status_layout.json")
	if err != nil {
		t.Fatal(err)
	}
	claudeStart := strings.Index(got, "Claude profiles")
	if claudeStart < 0 {
		t.Fatalf("missing Claude table:\n%s", got)
	}
	for _, section := range []string{got[:claudeStart], got[claudeStart:]} {
		var header string
		for _, line := range strings.Split(section, "\n") {
			if strings.Contains(line, "Account") && strings.Contains(line, "State") {
				header = line
				break
			}
		}
		if !strings.Contains(header, "5h") || !strings.Contains(header, "7d") {
			t.Fatalf("missing explicit quota columns: %q", header)
		}
	}
	claude := got[claudeStart:]
	for _, unwanted := range []string{"Plan", "unknown", "ac...", "co...", "re..."} {
		if strings.Contains(claude, unwanted) {
			t.Fatalf("Claude table contains %q:\n%s", unwanted, got)
		}
	}
	if !strings.Contains(claude, "State") || !strings.Contains(claude, "active, cooked") {
		t.Fatalf("Claude state must remain visible without a terminal:\n%s", claude)
	}
	for _, want := range []string{
		"primary.subscription.account@example.test", "primary.codex.account@example.test",
		"active, cooked", "revoked", "100%/4h4m", "0%/5d13h",
		"$187.20/$200 exp 10-24", "revoked 10-09 01:00Z",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q:\n%s", want, got)
		}
	}
	for _, line := range strings.Split(claude, "\n") {
		if strings.Contains(line, "Account") && !strings.HasSuffix(strings.TrimSpace(line), "Detail") {
			t.Fatalf("Detail must be the last column: %q", line)
		}
	}
	if os.Getenv("COLUMNS") == "200" {
		assertUsageGridLineWidths(t, got, 200)
	}
	t.Logf("status (COLUMNS=%s):\n%s", os.Getenv("COLUMNS"), got)
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
