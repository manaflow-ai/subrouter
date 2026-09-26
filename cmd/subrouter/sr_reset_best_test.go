package main

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestResetWithoutTargetAsksServerForBest(t *testing.T) {
	var query string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/_subrouter/rate-limit-reset" {
			t.Errorf("unexpected request %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		query = r.URL.RawQuery
		_, _ = io.WriteString(w, `{"dry_run":true,"reset":0,"results":[{"email":"a@example.com","eligible":true,"dry_run":true}]}`)
	}))
	defer server.Close()

	var out bytes.Buffer
	runner := srRunner{out: &out, errOut: &out, client: server.Client()}
	if err := runner.resetAgainstServer(t.Context(), []string{"--dry-run"}, &srServerConfig{Name: "test", URL: server.URL}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(query, "best=true") || !strings.Contains(query, "dry_run=true") {
		t.Fatalf("query = %q, want best=true and dry_run=true", query)
	}
	if !strings.Contains(out.String(), "a@example.com") {
		t.Fatalf("output = %q", out.String())
	}
}

func TestResetWithoutTargetFallsBackOnOlderServer(t *testing.T) {
	var sawUsage bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/_subrouter/rate-limit-reset":
			http.Error(w, "email or all=true is required", http.StatusBadRequest)
		case "/_subrouter/usage-status":
			sawUsage = true
			_, _ = io.WriteString(w, `[]`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	var out bytes.Buffer
	runner := srRunner{out: &out, errOut: &out, client: server.Client()}
	if err := runner.resetAgainstServer(t.Context(), []string{"--dry-run"}, &srServerConfig{Name: "test", URL: server.URL}); err != nil {
		t.Fatalf("%v (%s)", err, out.String())
	}
	if !sawUsage {
		t.Fatal("older server: expected a client-side pick from usage-status")
	}
	if !strings.Contains(out.String(), "No accounts are eligible") {
		t.Fatalf("output = %q", out.String())
	}
}
