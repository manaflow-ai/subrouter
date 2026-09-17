package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
)

func TestSRServerCapacityRendersRowsAndJSON(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	store := accounts.DefaultCodexStore()
	server := srServerConfig{Name: "team", URL: "http://100.64.0.1:31415", AdminToken: "secret-token"}
	if err := defaultSRServerStore(store).save(srServerFile{Servers: []srServerConfig{server}}); err != nil {
		t.Fatal(err)
	}
	streakSince := time.Now().Add(-90 * time.Second).UTC().Format(time.RFC3339)
	markedUntil := time.Now().Add(4 * time.Minute).UTC().Format(time.RFC3339)
	payload := `{"generated_at":"2026-09-16T20:00:00Z","retry_window":"5m0s","persistent_rule":"streak >= 3 ...","accounts":[` +
		`{"id":"owner-1","label":"austin+4@manaflow.com [pro]","last_15m":{"failures":9,"episodes":3,"sessions":2,"successes":1,"rate":0.75},` +
		`"last_1h":{"failures":9,"episodes":3,"sessions":2,"successes":5,"rate":0.375},"last_24h":{"failures":9,"episodes":3,"sessions":2,"successes":40,"rate":0.0698},` +
		`"streak":4,"streak_episodes":2,"streak_since":"` + streakSince + `","persistent":true,"marked_until":"` + markedUntil + `","mark_ttl":"4m0s","by_reason":{"server_is_overloaded":9},"total_failures":9,"total_episodes":3,"total_successes":40},` +
		`{"id":"owner-2","email":"quiet@cmux.com","last_15m":{"rate":-1},"last_1h":{"rate":-1},"last_24h":{"failures":0,"episodes":0,"successes":12,"rate":0},"streak":0,"persistent":false,"total_successes":12}]}`
	var paths []string
	runner := srRunner{
		store:  store,
		out:    &bytes.Buffer{},
		errOut: io.Discard,
		client: &http.Client{Transport: srRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			paths = append(paths, req.URL.Path)
			if got := req.Header.Get("Authorization"); got != "Bearer secret-token" {
				t.Fatalf("Authorization = %q", got)
			}
			return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(payload))}, nil
		})},
	}
	if err := runner.serverCapacity(context.Background(), server, nil); err != nil {
		t.Fatal(err)
	}
	out := runner.out.(*bytes.Buffer).String()
	if len(paths) != 1 || paths[0] != "/_subrouter/codex-capacity" {
		t.Fatalf("paths %v", paths)
	}
	for _, want := range []string{"austin+4@manaflow.com [pro]", "3/4  75%", "3/8  38%", "3/43   7%", "PERSISTENT", "streak 4 (2 retries)", "held out"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "quiet@cmux.com") {
		t.Fatalf("account with no failures must not be listed:\n%s", out)
	}
	runner.out = &bytes.Buffer{}
	if err := runner.serverCapacity(context.Background(), server, []string{"--json"}); err != nil {
		t.Fatal(err)
	}
	if out := runner.out.(*bytes.Buffer).String(); !strings.Contains(out, `"persistent_rule"`) || !strings.Contains(out, `"owner-2"`) {
		t.Fatalf("json output:\n%s", out)
	}
	if err := runner.serverCapacity(context.Background(), server, []string{"--bogus"}); err == nil {
		t.Fatal("unknown flag must error")
	}
}
