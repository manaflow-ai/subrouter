package main

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
)

func TestParseWaitDuration(t *testing.T) {
	for raw, want := range map[string]int64{"": 0, "1d": 86400, "12h": 43200, "1d12h": 129600, "90m": 5400} {
		got, err := parseWaitDuration(raw)
		if err != nil || got != want {
			t.Fatalf("parseWaitDuration(%q) = %d, %v; want %d", raw, got, err, want)
		}
	}
	for _, raw := range []string{"x", "-1h", "1dx", "d", "999999999999d"} {
		if _, err := parseWaitDuration(raw); err == nil {
			t.Fatalf("parseWaitDuration(%q) accepted", raw)
		}
	}
}

// resetSweepServer serves a dry-run preview listing two eligible accounts and
// records every redeem request.
func resetSweepServer(t *testing.T, withWaits bool) (*httptest.Server, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var redeems []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("dry_run") == "true" {
			if withWaits {
				_, _ = io.WriteString(w, `{"dry_run":true,"results":[
					{"email":"a@example.com","eligible":true,"dry_run":true,"weekly_wait_seconds":400000},
					{"email":"b@example.com","eligible":true,"dry_run":true,"weekly_wait_seconds":100000}]}`)
			} else {
				_, _ = io.WriteString(w, `{"dry_run":true,"results":[{"email":"a@example.com","eligible":true,"dry_run":true}]}`)
			}
			return
		}
		mu.Lock()
		redeems = append(redeems, r.URL.RawQuery)
		mu.Unlock()
		_, _ = io.WriteString(w, `{"reset":1,"results":[{"email":"`+q.Get("email")+`","eligible":true,"reset":true}]}`)
	}))
	t.Cleanup(server.Close)
	return server, func() []string { mu.Lock(); defer mu.Unlock(); return append([]string(nil), redeems...) }
}

func TestResetAllRefusesWithoutConfirmation(t *testing.T) {
	server, redeems := resetSweepServer(t, true)
	var out bytes.Buffer
	runner := srRunner{out: &out, errOut: &out, client: server.Client()}
	err := runner.resetAgainstServer(t.Context(), []string{"--all"}, &srServerConfig{Name: "test", URL: server.URL})
	if err == nil || !strings.Contains(err.Error(), "--yes") {
		t.Fatalf("err = %v, want a request for --yes", err)
	}
	if got := redeems(); len(got) != 0 {
		t.Fatalf("redeemed without confirmation: %v", got)
	}
}

func TestResetAllRedeemsExactlyThePreviewedAccounts(t *testing.T) {
	server, redeems := resetSweepServer(t, true)
	var out bytes.Buffer
	runner := srRunner{out: &out, errOut: &out, in: strings.NewReader("y\n"), client: server.Client()}
	if err := runner.resetAgainstServer(t.Context(), []string{"--all", "--min-wait", "1d"}, &srServerConfig{Name: "test", URL: server.URL}); err != nil {
		t.Fatalf("%v (%s)", err, out.String())
	}
	got := redeems()
	if len(got) != 2 || !strings.Contains(got[0], "email=a%40example.com") || !strings.Contains(got[1], "email=b%40example.com") {
		t.Fatalf("redeem requests = %v, want one per previewed account", got)
	}
	for _, q := range got {
		if strings.Contains(q, "all=") {
			t.Fatalf("redeem request %q re-sent all=true", q)
		}
	}
	if !strings.Contains(out.String(), "waits ") {
		t.Fatalf("preview did not show waits: %s", out.String())
	}
}

func TestResetAllMinWaitRefusesOlderServer(t *testing.T) {
	server, redeems := resetSweepServer(t, false)
	var out bytes.Buffer
	runner := srRunner{out: &out, errOut: &out, client: server.Client()}
	err := runner.resetAgainstServer(t.Context(), []string{"--all", "--yes", "--min-wait", "1d"}, &srServerConfig{Name: "test", URL: server.URL})
	if err == nil || !strings.Contains(err.Error(), "--min-wait") {
		t.Fatalf("err = %v, want an older-server --min-wait refusal", err)
	}
	if got := redeems(); len(got) != 0 {
		t.Fatalf("redeemed on an older server: %v", got)
	}
}

func TestResetCandidatesListsWithoutRedeeming(t *testing.T) {
	server, redeems := resetSweepServer(t, true)
	var out bytes.Buffer
	runner := srRunner{out: &out, errOut: &out, client: server.Client()}
	if err := runner.resetAgainstServer(t.Context(), []string{"--candidates"}, &srServerConfig{Name: "test", URL: server.URL}); err != nil {
		t.Fatal(err)
	}
	if len(redeems()) != 0 || !strings.Contains(out.String(), "2 account(s) eligible") {
		t.Fatalf("redeems = %v, output = %s", redeems(), out.String())
	}
}

func TestResetFlagConflicts(t *testing.T) {
	var out bytes.Buffer
	runner := srRunner{out: &out, errOut: &out}
	fixed := &srServerConfig{Name: "test", URL: "http://127.0.0.1:1"}
	for _, args := range [][]string{
		{"--local"},
		{"a@example.com", "--min-wait", "1d"},
		{"--candidates", "--all"},
		{"--min-wait", "soon"},
	} {
		if err := runner.resetAgainstServer(t.Context(), args, fixed); err == nil {
			t.Fatalf("%v: expected an error", args)
		}
	}
}

// bestIgnoringMinWaitServer is a server that understands best=true but not
// min_wait_seconds: it always offers the same account with a short wait.
func bestIgnoringMinWaitServer(t *testing.T, reportWait bool) (*httptest.Server, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var redeems []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("dry_run") == "true" {
			wait := ""
			if reportWait {
				wait = `,"weekly_wait_seconds":3600`
			}
			_, _ = io.WriteString(w, `{"dry_run":true,"results":[{"email":"soon@example.com","eligible":true,"dry_run":true`+wait+`}]}`)
			return
		}
		mu.Lock()
		redeems = append(redeems, r.URL.RawQuery)
		mu.Unlock()
		_, _ = io.WriteString(w, `{"reset":1,"results":[{"email":"soon@example.com","eligible":true,"reset":true}]}`)
	}))
	t.Cleanup(server.Close)
	return server, func() []string { mu.Lock(); defer mu.Unlock(); return append([]string(nil), redeems...) }
}

func TestResetDefaultPickMinWaitNeverSpendsOnShortWait(t *testing.T) {
	for _, reportWait := range []bool{true, false} {
		server, redeems := bestIgnoringMinWaitServer(t, reportWait)
		var out bytes.Buffer
		runner := srRunner{out: &out, errOut: &out, client: server.Client()}
		err := runner.resetAgainstServer(t.Context(), []string{"--min-wait", "1d"}, &srServerConfig{Name: "test", URL: server.URL})
		if err == nil {
			t.Fatalf("reportWait=%t: expected an error, output %s", reportWait, out.String())
		}
		if got := redeems(); len(got) != 0 {
			t.Fatalf("reportWait=%t: redeemed %v despite --min-wait 1d", reportWait, got)
		}
	}
}

func TestResetAllNonTerminalStdinNeedsYes(t *testing.T) {
	server, redeems := resetSweepServer(t, true)
	devNull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer devNull.Close()
	var out bytes.Buffer
	runner := srRunner{out: &out, errOut: &out, in: devNull, client: server.Client()}
	err = runner.resetAgainstServer(t.Context(), []string{"--all"}, &srServerConfig{Name: "test", URL: server.URL})
	if err == nil || !strings.Contains(err.Error(), "--yes") {
		t.Fatalf("err = %v, want a request for --yes", err)
	}
	if got := redeems(); len(got) != 0 {
		t.Fatalf("redeemed without confirmation: %v", got)
	}
}
