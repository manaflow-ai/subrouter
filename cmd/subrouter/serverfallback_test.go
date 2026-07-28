package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
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
	got := withLocalFallbackTo(context.Background(), fallbackHTTPClient(), primary.URL+"/v1", local.URL+"/v1", &warn)
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
	got := withLocalFallbackTo(context.Background(), fallbackHTTPClient(), deadURL, local.URL+"/v1", &warn)
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
	got := withLocalFallbackTo(context.Background(), fallbackHTTPClient(), primaryURL, localURL, &warn)
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

	got := withLocalFallbackTo(context.Background(), fallbackHTTPClient(), deadURL, local.URL+"/v1", nil)
	if got != deadURL {
		t.Fatalf("got %q, want unchanged %q when fallback disabled", got, deadURL)
	}
}

func TestWithLocalFallbackDoesNotFailOverOntoItself(t *testing.T) {
	local := healthServer(t, http.StatusOK)
	// Same origin as the local daemon: nothing to fail over to.
	got := withLocalFallbackTo(context.Background(), fallbackHTTPClient(), local.URL+"/v1", local.URL+"/v1", nil)
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
