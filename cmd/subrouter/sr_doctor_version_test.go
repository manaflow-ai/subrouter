package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/manaflow-ai/subrouter/internal/buildversion"
)

func versionHealthServer(t *testing.T, version string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/_subrouter/health" {
			http.NotFound(w, r)
			return
		}
		body := map[string]any{"ok": true}
		if version != "" {
			body["version"] = version
		}
		_ = json.NewEncoder(w).Encode(body)
	}))
	t.Cleanup(server.Close)
	return server
}

func noTeamPlists(t *testing.T) {
	t.Helper()
	old := doctorTeamPlistGlob
	doctorTeamPlistGlob = filepath.Join(t.TempDir(), "none-*.plist")
	t.Cleanup(func() { doctorTeamPlistGlob = old })
}

func runDoctorForTest(t *testing.T, baseURL string) string {
	t.Helper()
	isolateCloudConfig(t)
	t.Setenv("SUBROUTER_LOCAL_BASE_URL", baseURL+"/v1")
	var out bytes.Buffer
	_ = runDoctorWith(context.Background(), &fakeController{present: true}, nil, emptyStore(t), &out)
	return out.String()
}

func TestDoctorReportsMatchingVersions(t *testing.T) {
	noTeamPlists(t)
	server := versionHealthServer(t, buildversion.Version())
	out := runDoctorForTest(t, server.URL)
	if !strings.Contains(out, "version") || !strings.Contains(out, "(CLI and daemon)") {
		t.Fatalf("doctor did not report matching versions:\n%s", out)
	}
}

func TestDoctorSuggestsUpdateOnVersionSkew(t *testing.T) {
	noTeamPlists(t)
	server := versionHealthServer(t, "v0.0.1-old")
	out := runDoctorForTest(t, server.URL)
	if !strings.Contains(out, "daemon v0.0.1-old") || !strings.Contains(out, "update'") {
		t.Fatalf("doctor did not flag the skew with an update hint:\n%s", out)
	}
}

func TestDoctorFlagsDaemonWithoutVersion(t *testing.T) {
	noTeamPlists(t)
	server := versionHealthServer(t, "")
	out := runDoctorForTest(t, server.URL)
	if !strings.Contains(out, "predates version reporting") {
		t.Fatalf("doctor did not flag a daemon without a version:\n%s", out)
	}
}

func TestDoctorReportsTeamAutoupdatePin(t *testing.T) {
	dir := t.TempDir()
	old := doctorTeamPlistGlob
	doctorTeamPlistGlob = filepath.Join(dir, "ai.manaflow.subrouter*.plist")
	t.Cleanup(func() { doctorTeamPlistGlob = old })
	plist := filepath.Join(dir, "ai.manaflow.subrouter-team.plist")
	if err := os.WriteFile(plist, []byte("<plist/>"), 0o644); err != nil {
		t.Fatal(err)
	}
	server := versionHealthServer(t, "v0.0.1-old")

	out := runDoctorForTest(t, server.URL)
	if !strings.Contains(out, "ai.manaflow.subrouter-team installs new releases") {
		t.Fatalf("doctor did not report unpinned autoupdate:\n%s", out)
	}
	if !strings.Contains(out, "subrouter-deploy.sh install-release") {
		t.Fatalf("team host skew should point at the deploy script:\n%s", out)
	}

	sentinel := plist + ".supervisor-transaction/upgrade-inhibited"
	if err := os.MkdirAll(filepath.Dir(sentinel), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sentinel, []byte("pinned at v1.2.3 by subrouter-deploy.sh pin (me) at now; clear with subrouter-deploy.sh unpin\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out = runDoctorForTest(t, server.URL)
	if !strings.Contains(out, "ai.manaflow.subrouter-team pinned at v1.2.3") {
		t.Fatalf("doctor did not report the pin:\n%s", out)
	}

	if err := os.WriteFile(sentinel, []byte("subrouter-guard.sh rolled back worker abc at now; clear this file after review\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out = runDoctorForTest(t, server.URL)
	if !strings.Contains(out, "paused: subrouter-guard.sh rolled back") {
		t.Fatalf("doctor did not report the guard's rollback pause:\n%s", out)
	}
}
