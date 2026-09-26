package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func refusedDialError() error {
	return &net.OpError{Op: "dial", Net: "tcp", Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)}
}

func restartTestStatus(t *testing.T, online bool) tailscaleStatusLoader {
	t.Helper()
	status, err := json.Marshal(tailscaleStatusDocument{Peer: map[string]tailscaleNodeStatus{
		"peer": {ID: "node-1", DNSName: "router.example.", Online: online},
	}})
	if err != nil {
		t.Fatal(err)
	}
	return func(context.Context) ([]byte, error) { return status, nil }
}

func shortenRestartRetry(t *testing.T) {
	t.Helper()
	previous := serverRestartRetryInterval
	serverRestartRetryInterval = time.Millisecond
	t.Cleanup(func() { serverRestartRetryInterval = previous })
}

func TestHealTailscaleServerWaitsThroughRefusedRestart(t *testing.T) {
	shortenRestartRetry(t)
	var probes atomic.Int32
	client := &http.Client{Transport: testRoundTripFunc(func(*http.Request) (*http.Response, error) {
		if probes.Add(1) <= 6 {
			return nil, refusedDialError()
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("{}"))}, nil
	})}
	server := srServerConfig{Name: "team", URL: "http://router.example:31415", TailscaleNodeID: "node-1"}
	var warnings bytes.Buffer
	got, err := healTailscaleServer(t.Context(), srServerStore{}, server, client, &warnings, restartTestStatus(t, true))
	if err != nil {
		t.Fatalf("heal during restart: %v (warnings %q)", err, warnings.String())
	}
	if got.URL != server.URL {
		t.Fatalf("URL = %q, want unchanged %q", got.URL, server.URL)
	}
	if !strings.Contains(warnings.String(), "probably restarting") || !strings.Contains(warnings.String(), "answering again") {
		t.Fatalf("warnings = %q", warnings.String())
	}
}

func TestHealTailscaleServerDoesNotWaitForWedgedHost(t *testing.T) {
	shortenRestartRetry(t)
	var probes atomic.Int32
	client := &http.Client{Transport: testRoundTripFunc(func(*http.Request) (*http.Response, error) {
		probes.Add(1)
		return nil, context.DeadlineExceeded
	})}
	server := srServerConfig{Name: "team", URL: "http://router.example:31415", TailscaleNodeID: "node-1"}
	_, err := healTailscaleServer(t.Context(), srServerStore{}, server, client, io.Discard, restartTestStatus(t, true))
	var repair tailscaleRepairFailure
	if !errors.As(err, &repair) {
		t.Fatalf("err = %v, want repair failure", err)
	}
	if probes.Load() != 1 {
		t.Fatalf("probes = %d, want one round without waiting", probes.Load())
	}
}

func TestHealTailscaleServerDoesNotWaitForOfflineNode(t *testing.T) {
	shortenRestartRetry(t)
	var probes atomic.Int32
	client := &http.Client{Transport: testRoundTripFunc(func(*http.Request) (*http.Response, error) {
		probes.Add(1)
		return nil, refusedDialError()
	})}
	server := srServerConfig{Name: "team", URL: "http://router.example:31415", TailscaleNodeID: "node-1"}
	if _, err := healTailscaleServer(t.Context(), srServerStore{}, server, client, io.Discard, restartTestStatus(t, false)); err == nil {
		t.Fatal("offline node unexpectedly healed")
	}
	if probes.Load() != 1 {
		t.Fatalf("probes = %d, want one round without waiting", probes.Load())
	}
}

func TestHealTailscaleServerRestartWaitIsBounded(t *testing.T) {
	shortenRestartRetry(t)
	t.Setenv(serverRestartGraceEnv, "20ms")
	client := &http.Client{Transport: testRoundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusServiceUnavailable, Body: io.NopCloser(strings.NewReader(""))}, nil
	})}
	server := srServerConfig{Name: "team", URL: "http://router.example:31415", TailscaleNodeID: "node-1"}
	started := time.Now()
	_, err := healTailscaleServer(t.Context(), srServerStore{}, server, client, io.Discard, restartTestStatus(t, true))
	if err == nil || !strings.Contains(err.Error(), "no discovered endpoint passed") {
		t.Fatalf("err = %v", err)
	}
	if elapsed := time.Since(started); elapsed < 20*time.Millisecond || elapsed > 5*time.Second {
		t.Fatalf("elapsed = %s, want bounded by the configured grace", elapsed)
	}
}

func TestConnectionRefusedRecognizesWinsockErrno(t *testing.T) {
	windows := &net.OpError{Op: "dial", Net: "tcp", Err: os.NewSyscallError("connectex", windowsConnectionRefused)}
	if !connectionRefused(windows) {
		t.Fatal("WSAECONNREFUSED was not treated as a refused connection")
	}
	if !connectionRefused(refusedDialError()) {
		t.Fatal("ECONNREFUSED was not treated as a refused connection")
	}
	if connectionRefused(context.DeadlineExceeded) {
		t.Fatal("a timeout was treated as a refused connection")
	}
}

func TestHealTailscaleServerRestartWaitStopsAtDeadline(t *testing.T) {
	previous := serverRestartRetryInterval
	serverRestartRetryInterval = time.Hour
	t.Cleanup(func() { serverRestartRetryInterval = previous })
	t.Setenv(serverRestartGraceEnv, "50ms")
	client := &http.Client{Transport: testRoundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, refusedDialError()
	})}
	server := srServerConfig{Name: "team", URL: "http://router.example:31415", TailscaleNodeID: "node-1"}
	started := time.Now()
	if _, err := healTailscaleServer(t.Context(), srServerStore{}, server, client, io.Discard, restartTestStatus(t, true)); err == nil {
		t.Fatal("refusing server unexpectedly healed")
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("elapsed = %s, want the wait capped at the grace deadline", elapsed)
	}
}

func TestServerRestartGraceParsesOverride(t *testing.T) {
	for raw, want := range map[string]time.Duration{
		"":      defaultServerRestartGrace,
		"0":     0,
		"45":    45 * time.Second,
		"3m":    3 * time.Minute,
		"bogus": defaultServerRestartGrace,
		"-1s":   defaultServerRestartGrace,
	} {
		t.Setenv(serverRestartGraceEnv, raw)
		if got := serverRestartGrace(); got != want {
			t.Errorf("grace(%q) = %s, want %s", raw, got, want)
		}
	}
}
