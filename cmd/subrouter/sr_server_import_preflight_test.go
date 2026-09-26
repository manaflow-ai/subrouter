package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/manaflow-ai/subrouter/internal/accounts"
)

// A server that authenticates by tailnet identity has no credential for the
// client to carry, so the preflight must ask it rather than refusing locally.
func TestAccountImportPreflightSucceedsWithoutAStoredCredential(t *testing.T) {
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Error("client sent an Authorization header it does not have")
		}
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer remote.Close()

	var out bytes.Buffer
	runner := srRunner{program: "sr", out: &out, errOut: &out, client: remote.Client()}
	err := runner.ensureServerAccountImportAvailable(
		context.Background(),
		srServerConfig{Name: "mac-mini", URL: remote.URL},
	)
	if err != nil {
		t.Fatalf("preflight failed against a credential-free server: %v", err)
	}
}

func TestAccountImportProviderPreflightRejectsAnOlderServerBeforeOAuth(t *testing.T) {
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"ok":true,"providers":["codex","claude"]}`)
	}))
	defer remote.Close()

	runner := srRunner{program: "sr", client: remote.Client()}
	err := runner.ensureServerAccountImportProviderAvailable(
		context.Background(),
		srServerConfig{Name: "old-server", URL: remote.URL},
		accounts.ProviderKimi,
	)
	if err == nil || !strings.Contains(err.Error(), "does not advertise kimi account import") {
		t.Fatalf("provider preflight error = %v", err)
	}
}

func TestAccountImportPreflightExplainsAMissingCredential(t *testing.T) {
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "protected account import credential required", http.StatusUnauthorized)
	}))
	defer remote.Close()

	var out bytes.Buffer
	runner := srRunner{program: "sr", out: &out, errOut: &out, client: remote.Client()}
	err := runner.ensureServerAccountImportAvailable(
		context.Background(),
		srServerConfig{Name: "mac-mini", URL: remote.URL},
	)
	if err == nil || !strings.Contains(err.Error(), "has no protected HTTP account-import credential") {
		t.Fatalf("error = %v, want the missing-credential guidance", err)
	}
}

func TestAccountImportPreflightExplainsARejectedCredential(t *testing.T) {
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "protected account import credential required", http.StatusUnauthorized)
	}))
	defer remote.Close()

	var out bytes.Buffer
	runner := srRunner{program: "sr", out: &out, errOut: &out, client: remote.Client()}
	err := runner.ensureServerAccountImportAvailable(
		context.Background(),
		srServerConfig{Name: "mac-mini", URL: remote.URL, AccountImportToken: "stale"},
	)
	if err == nil || !strings.Contains(err.Error(), "rejected its protected HTTP account-import credential") {
		t.Fatalf("error = %v, want the rotation guidance", err)
	}
}
