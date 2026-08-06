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

	"github.com/manaflow-ai/subrouter/internal/accounts"
)

// writeDoctorServerFile points a store's servers.json at one default server.
func writeDoctorServerFile(t *testing.T, store accounts.CodexStore, server srServerConfig) {
	t.Helper()
	file := srServerFile{Servers: []srServerConfig{server}, Default: server.Name}
	body, err := json.Marshal(file)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(store.StoreDir(), "servers.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestDoctorFailsWhenServerEntryHasNoImportCredential(t *testing.T) {
	isolateCloudConfig(t)
	local := healthServer(t, http.StatusOK)
	t.Setenv("SUBROUTER_LOCAL_BASE_URL", local.URL+"/v1")
	t.Setenv("SUBROUTER_CODEX_SERVER", "")

	// The server is the authority on whether it can accept an import, so this
	// stub answers the preflight the way a token-protected server does. A bare
	// address would make the test depend on whatever listens on the developer's
	// own machine.
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "protected account import credential required", http.StatusUnauthorized)
	}))
	t.Cleanup(remote.Close)

	store := emptyStore(t)
	writeDoctorServerFile(t, store, srServerConfig{
		Name: "mac-mini",
		URL:  remote.URL,
	})

	var out bytes.Buffer
	_ = runDoctorWith(context.Background(), &fakeController{present: true}, nil, store, &out)
	if !strings.Contains(out.String(), "FAIL  server account import") {
		t.Fatalf("expected a failing account import check:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "no protected HTTP account-import credential") {
		t.Fatalf("expected the fix to name the missing credential:\n%s", out.String())
	}
}

func TestDoctorFailsWhenServerRejectsImportCredential(t *testing.T) {
	isolateCloudConfig(t)
	local := healthServer(t, http.StatusOK)
	t.Setenv("SUBROUTER_LOCAL_BASE_URL", local.URL+"/v1")
	t.Setenv("SUBROUTER_CODEX_SERVER", "")

	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "protected account import credential required", http.StatusUnauthorized)
	}))
	t.Cleanup(remote.Close)

	store := emptyStore(t)
	writeDoctorServerFile(t, store, srServerConfig{
		Name:               "mac-mini",
		URL:                remote.URL,
		AccountImportToken: "stale-token",
	})

	var out bytes.Buffer
	_ = runDoctorWith(context.Background(), &fakeController{present: true}, nil, store, &out)
	if !strings.Contains(out.String(), "FAIL  server account import") {
		t.Fatalf("expected a failing account import check:\n%s", out.String())
	}
}

func TestDoctorPassesWhenServerAcceptsImports(t *testing.T) {
	isolateCloudConfig(t)
	local := healthServer(t, http.StatusOK)
	t.Setenv("SUBROUTER_LOCAL_BASE_URL", local.URL+"/v1")
	t.Setenv("SUBROUTER_CODEX_SERVER", "")

	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != serverAccountImportPath {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer import-token" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(remote.Close)

	store := emptyStore(t)
	writeDoctorServerFile(t, store, srServerConfig{
		Name:               "mac-mini",
		URL:                remote.URL,
		AccountImportToken: "import-token",
	})

	var out bytes.Buffer
	_ = runDoctorWith(context.Background(), &fakeController{present: true}, nil, store, &out)
	if !strings.Contains(out.String(), "ok    server account import") {
		t.Fatalf("expected a passing account import check:\n%s", out.String())
	}
}
