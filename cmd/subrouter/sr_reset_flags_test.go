package main

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// TestResetFlagsAfterEmailAreHonored guards against `sr reset <email>
// --dry-run` spending a real credit: Go's flag package stops at the first
// positional argument, so a trailing --dry-run was silently dropped.
func TestResetFlagsAfterEmailAreHonored(t *testing.T) {
	var mu sync.Mutex
	var queries []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/_subrouter/rate-limit-reset" {
			http.NotFound(w, r)
			return
		}
		mu.Lock()
		queries = append(queries, r.URL.RawQuery)
		mu.Unlock()
		_, _ = io.WriteString(w, `{"dry_run":true,"reset":0,"results":[]}`)
	}))
	defer server.Close()

	for _, args := range [][]string{
		{"--dry-run", "a@example.com"},
		{"a@example.com", "--dry-run"},
	} {
		queries = nil
		var out bytes.Buffer
		runner := srRunner{out: &out, errOut: &out, client: server.Client()}
		if err := runner.resetAgainstServer(t.Context(), args, &srServerConfig{Name: "test", URL: server.URL}); err != nil {
			t.Fatalf("%v: %v (%s)", args, err, out.String())
		}
		if len(queries) != 1 || !bytes.Contains([]byte(queries[0]), []byte("dry_run=true")) {
			t.Fatalf("%v: reset request queries = %q, want one with dry_run=true", args, queries)
		}
	}
}

func TestResetRejectsUnknownTrailingArguments(t *testing.T) {
	var called bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		_, _ = io.WriteString(w, `{"reset":0,"results":[]}`)
	}))
	defer server.Close()
	for _, args := range [][]string{
		{"a@example.com", "b@example.com"},
		{"a@example.com", "--dryrun"},
	} {
		called = false
		var out bytes.Buffer
		runner := srRunner{out: &out, errOut: &out, client: server.Client()}
		err := runner.resetAgainstServer(t.Context(), args, &srServerConfig{Name: "test", URL: server.URL})
		if err == nil || called {
			t.Fatalf("%v: err = %v, request sent = %t; want an error and no request", args, err, called)
		}
	}
}
