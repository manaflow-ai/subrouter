package main

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func healthServer(t *testing.T, status int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/_subrouter/health" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestHealthURLForStripsVersionSuffix(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"http://host:31415/v1", "http://host:31415/_subrouter/health"},
		{"http://host:31415", "http://host:31415/_subrouter/health"},
		{"https://subrouter.cmux.dev/v1/", "https://subrouter.cmux.dev/_subrouter/health"},
	} {
		got, err := healthURLFor(tc.in)
		if err != nil {
			t.Fatalf("healthURLFor(%q): %v", tc.in, err)
		}
		if got != tc.want {
			t.Fatalf("healthURLFor(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestSameEndpointTreatsHTTPAndHTTPSAsDifferentOrigins(t *testing.T) {
	if sameEndpoint(
		"http://127.0.0.1:31415/v1",
		"https://127.0.0.1:31415/v1",
	) {
		t.Fatal("HTTP and HTTPS endpoints were treated as the same origin")
	}
}

func TestHealthURLForRejectsRelative(t *testing.T) {
	if _, err := healthURLFor("cmux-mac-mini:31415"); err == nil {
		t.Fatal("expected error for non-absolute base URL")
	}
	if _, err := healthURLFor("   "); err == nil {
		t.Fatal("expected error for empty base URL")
	}
}

func TestServerHealthy(t *testing.T) {
	ok := healthServer(t, http.StatusOK)
	if !serverHealthy(context.Background(), fallbackHTTPClient(), ok.URL+"/v1") {
		t.Fatal("healthy server reported unhealthy")
	}

	bad := healthServer(t, http.StatusInternalServerError)
	if serverHealthy(context.Background(), fallbackHTTPClient(), bad.URL+"/v1") {
		t.Fatal("500 reported healthy")
	}

	// A closed listener stands in for an unreachable host.
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	deadURL := dead.URL
	dead.Close()
	if serverHealthy(context.Background(), fallbackHTTPClient(), deadURL+"/v1") {
		t.Fatal("closed server reported healthy")
	}
}

func TestWithLocalFallbackKeepsHealthyPrimary(t *testing.T) {
	primary := healthServer(t, http.StatusOK)
	local := healthServer(t, http.StatusOK)

	var warn bytes.Buffer
	got := withLocalFallbackTo(context.Background(), fallbackHTTPClient(), primary.URL+"/v1", local.URL+"/v1", nil, &warn)
	if got != primary.URL+"/v1" {
		t.Fatalf("got %q, want primary %q", got, primary.URL+"/v1")
	}
	if warn.Len() != 0 {
		t.Fatalf("unexpected warning: %q", warn.String())
	}
}

func TestWithLocalFallbackUsesLocalWhenPrimaryDown(t *testing.T) {
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	deadURL := dead.URL + "/v1"
	dead.Close()
	local := healthServer(t, http.StatusOK)

	var warn bytes.Buffer
	got := withLocalFallbackTo(context.Background(), fallbackHTTPClient(), deadURL, local.URL+"/v1", nil, &warn)
	if got != local.URL+"/v1" {
		t.Fatalf("got %q, want local %q", got, local.URL+"/v1")
	}
	if warn.Len() == 0 {
		t.Fatal("expected a warning when falling back")
	}
}

func TestWithLocalFallbackKeepsPrimaryWhenLocalAlsoDown(t *testing.T) {
	deadPrimary := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	primaryURL := deadPrimary.URL + "/v1"
	deadPrimary.Close()
	deadLocal := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	localURL := deadLocal.URL + "/v1"
	deadLocal.Close()

	var warn bytes.Buffer
	got := withLocalFallbackTo(context.Background(), fallbackHTTPClient(), primaryURL, localURL, nil, &warn)
	if got != primaryURL {
		t.Fatalf("got %q, want primary %q preserved", got, primaryURL)
	}
	if warn.Len() != 0 {
		t.Fatalf("unexpected warning: %q", warn.String())
	}
}

func TestWithLocalFallbackRespectsDisableEnv(t *testing.T) {
	t.Setenv("SUBROUTER_DISABLE_FALLBACK", "1")
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	deadURL := dead.URL + "/v1"
	dead.Close()
	local := healthServer(t, http.StatusOK)

	got := withLocalFallbackTo(context.Background(), fallbackHTTPClient(), deadURL, local.URL+"/v1", nil, nil)
	if got != deadURL {
		t.Fatalf("got %q, want unchanged %q when fallback disabled", got, deadURL)
	}
}

func TestWithLocalFallbackDoesNotFailOverOntoItself(t *testing.T) {
	local := healthServer(t, http.StatusOK)
	// Same origin as the local daemon: nothing to fail over to.
	got := withLocalFallbackTo(context.Background(), fallbackHTTPClient(), local.URL+"/v1", local.URL+"/v1", nil, nil)
	if got != local.URL+"/v1" {
		t.Fatalf("got %q, want %q", got, local.URL+"/v1")
	}
}

func TestCodexBaseURLWithFallbackSwapsUnreachableDefault(t *testing.T) {
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	deadURL := dead.URL
	dead.Close()
	local := healthServer(t, http.StatusOK)
	t.Setenv("SUBROUTER_LOCAL_BASE_URL", local.URL+"/v1")

	store := srServerStore{Path: filepath.Join(t.TempDir(), "servers.json")}
	if err := store.save(srServerFile{
		Default: "team",
		Servers: []srServerConfig{{Name: "team", URL: deadURL}},
	}); err != nil {
		t.Fatal(err)
	}

	var warn bytes.Buffer
	got, err := codexBaseURLWithFallback(store, &warn)
	if err != nil {
		t.Fatal(err)
	}
	if got != local.URL+"/v1" {
		t.Fatalf("base URL = %q, want local %q", got, local.URL+"/v1")
	}
}

func TestCodexBaseURLWithFallbackHonoursExplicitPin(t *testing.T) {
	t.Setenv(
		"SUBROUTER_CLOUD_CONFIG",
		filepath.Join(t.TempDir(), "cloud.json"),
	)
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	deadURL := dead.URL + "/v1"
	dead.Close()
	local := healthServer(t, http.StatusOK)
	t.Setenv("SUBROUTER_LOCAL_BASE_URL", local.URL+"/v1")
	t.Setenv("SUBROUTER_CODEX_BASE_URL", deadURL)

	store := srServerStore{Path: filepath.Join(t.TempDir(), "servers.json")}
	got, err := codexBaseURLWithFallback(store, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got != deadURL {
		t.Fatalf("base URL = %q, want pinned %q", got, deadURL)
	}
}

func TestEnsureLocalHealthyStartsDeadDaemon(t *testing.T) {
	// The daemon is "started" by flipping the handler to healthy.
	var healthy atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !healthy.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	started := 0
	start := func() error { started++; healthy.Store(true); return nil }

	var warn bytes.Buffer
	if !ensureLocalHealthy(context.Background(), fallbackHTTPClient(), srv.URL+"/v1", start, &warn) {
		t.Fatal("expected daemon to become healthy after autostart")
	}
	if started != 1 {
		t.Fatalf("start called %d times, want 1", started)
	}
	if !strings.Contains(warn.String(), "starting it") {
		t.Fatalf("expected an autostart notice, got %q", warn.String())
	}
}

func TestEnsureLocalHealthySkipsStartWhenAlreadyUp(t *testing.T) {
	local := healthServer(t, http.StatusOK)
	started := 0
	start := func() error { started++; return nil }

	if !ensureLocalHealthy(context.Background(), fallbackHTTPClient(), local.URL+"/v1", start, nil) {
		t.Fatal("healthy daemon reported unhealthy")
	}
	if started != 0 {
		t.Fatalf("start called %d times on a healthy daemon, want 0", started)
	}
}

func TestEnsureLocalHealthyReportsStartFailure(t *testing.T) {
	dead := healthServer(t, http.StatusServiceUnavailable)
	start := func() error { return errors.New("launchctl exploded") }

	var warn bytes.Buffer
	if ensureLocalHealthy(context.Background(), fallbackHTTPClient(), dead.URL+"/v1", start, &warn) {
		t.Fatal("expected failure when the starter errors")
	}
	if !strings.Contains(warn.String(), "launchctl exploded") {
		t.Fatalf("expected the start error surfaced, got %q", warn.String())
	}
}

func TestAutostartRunsForLocalPinEvenWhenFallbackDisabled(t *testing.T) {
	t.Setenv("SUBROUTER_DISABLE_FALLBACK", "1")
	var healthy atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !healthy.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	started := 0
	start := func() error { started++; healthy.Store(true); return nil }

	got := withLocalFallbackTo(context.Background(), fallbackHTTPClient(), srv.URL+"/v1", srv.URL+"/v1", start, nil)
	if got != srv.URL+"/v1" {
		t.Fatalf("pinned local URL changed to %q", got)
	}
	if started != 1 {
		t.Fatalf("start called %d times, want 1", started)
	}
}

func TestClaudeLaunchesAgent(t *testing.T) {
	for _, args := range [][]string{nil, {}, {"run"}, {"--model", "opus"}, {"-p", "hi"}} {
		if !claudeLaunchesAgent(args) {
			t.Errorf("claudeLaunchesAgent(%v) = false, want true", args)
		}
	}
	for _, args := range [][]string{{"list"}, {"ls"}, {"status"}, {"add"}, {"push"}, {"help"}, {"--help"}} {
		if claudeLaunchesAgent(args) {
			t.Errorf("claudeLaunchesAgent(%v) = true, want false", args)
		}
	}
}
