package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
)

// A reset whose redemption failed must not exit 0: scripts and the user's
// shell would otherwise believe the account was un-cooked.
func TestResetRemoteReturnsErrorWhenRedemptionFails(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/_subrouter/rate-limit-reset" {
			http.NotFound(w, r)
			return
		}
		_, _ = io.WriteString(w, `{"reset":0,"results":[{"email":"a@example.com","eligible":true,"error":"wham 500"}]}`)
	}))
	defer server.Close()
	var out bytes.Buffer
	runner := srRunner{out: &out, errOut: &out, client: server.Client()}
	err := runner.resetAgainstServer(t.Context(), []string{"a@example.com"}, &srServerConfig{Name: "test", URL: server.URL})
	if err == nil {
		t.Fatalf("reset returned nil after a failed redemption; output:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "wham 500") {
		t.Fatalf("failure detail not printed:\n%s", out.String())
	}
}

// With no email, a usage fetch failure used to be skipped silently; when
// every fetch failed the command printed "no eligible accounts" and exited 0.
func TestResetLocalReportsUsageFetchFailures(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("SUBROUTER_SERVER", "")
	t.Setenv("SUBROUTER_CODEX_SERVER", "")
	store := accounts.DefaultCodexStore()
	for _, email := range []string{"a@example.com", "b@example.com"} {
		if err := store.SaveStored(accounts.StoredCodexAccount{
			Email:   email,
			AddedAt: time.Now().UTC().Format(time.RFC3339),
			Auth:    testCodexAuth(email, "acct_"+email[:1]),
		}); err != nil {
			t.Fatal(err)
		}
	}
	var out bytes.Buffer
	runner := srRunner{
		store:  store,
		out:    &out,
		errOut: &out,
		client: &http.Client{Transport: srRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusBadGateway,
				Status:     "502 Bad Gateway",
				Body:       io.NopCloser(strings.NewReader("upstream down")),
				Header:     http.Header{},
				Request:    req,
			}, nil
		})},
	}
	err := runner.reset(context.Background(), nil)
	if err == nil {
		t.Fatalf("reset returned nil when every usage fetch failed; output:\n%s", out.String())
	}
	for _, email := range []string{"a@example.com", "b@example.com"} {
		if !strings.Contains(out.String(), email) {
			t.Fatalf("fetch failure for %s not printed:\n%s", email, out.String())
		}
	}
}
