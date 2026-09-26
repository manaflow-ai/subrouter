package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestOverloadRetryIntervalEnvironment(t *testing.T) {
	for _, key := range []string{"SUBROUTER_CLAUDE_OVERLOAD_RETRY_INTERVAL", "SUBROUTER_CODEX_CAPACITY_RETRY_INTERVAL", "SUBROUTER_CLAUDE_OVERLOAD_RETRY_HEADER", "SUBROUTER_CLAUDE_OVERLOAD_MAX_WAIT", "SUBROUTER_CODEX_CAPACITY_RETRY_MAX_WAIT"} {
		t.Setenv(key, "")
	}
	claude, err := claudeOverloadRetryConfigFromEnvironment()
	if err != nil || claude.Interval != 0 || claude.AllowHeader {
		t.Fatalf("claude = %+v err = %v, want defaults with the header off", claude, err)
	}

	t.Setenv("SUBROUTER_CLAUDE_OVERLOAD_RETRY_INTERVAL", "2s")
	t.Setenv("SUBROUTER_CODEX_CAPACITY_RETRY_INTERVAL", "500ms")
	t.Setenv("SUBROUTER_CLAUDE_OVERLOAD_RETRY_HEADER", "1")
	if claude, err = claudeOverloadRetryConfigFromEnvironment(); err != nil || claude.Interval != 2*time.Second || !claude.AllowHeader {
		t.Fatalf("claude = %+v err = %v, want 2s with the header allowed", claude, err)
	}
	codex, err := codexOverloadFailoverConfigFromEnvironment()
	if err != nil || codex.StayInterval != 500*time.Millisecond {
		t.Fatalf("codex = %+v err = %v, want the 500ms floor accepted", codex, err)
	}

	// Below the floor is rejected, not clamped.
	for _, bad := range []string{"499ms", "0", "fast"} {
		t.Setenv("SUBROUTER_CLAUDE_OVERLOAD_RETRY_INTERVAL", bad)
		if _, err := claudeOverloadRetryConfigFromEnvironment(); err == nil {
			t.Fatalf("SUBROUTER_CLAUDE_OVERLOAD_RETRY_INTERVAL=%q accepted", bad)
		} else if bad == "499ms" && !strings.Contains(err.Error(), "at least 500ms") {
			t.Fatalf("error = %v, want it to name the 500ms floor", err)
		}
		t.Setenv("SUBROUTER_CLAUDE_OVERLOAD_RETRY_INTERVAL", "")
		t.Setenv("SUBROUTER_CODEX_CAPACITY_RETRY_INTERVAL", bad)
		if _, err := codexOverloadFailoverConfigFromEnvironment(); err == nil {
			t.Fatalf("SUBROUTER_CODEX_CAPACITY_RETRY_INTERVAL=%q accepted", bad)
		}
		t.Setenv("SUBROUTER_CODEX_CAPACITY_RETRY_INTERVAL", "")
	}
}

func TestTakeOverloadRetryFlags(t *testing.T) {
	args, header, err := takeOverloadRetryFlags([]string{"exec", "--retry-interval", "2s", "--retry-max-wait=20m", "--", "--retry-interval", "9s"})
	if err != nil {
		t.Fatal(err)
	}
	if header != "interval=2s,max-wait=20m0s" {
		t.Fatalf("header = %q", header)
	}
	if got := strings.Join(args, " "); got != "exec -- --retry-interval 9s" {
		t.Fatalf("args = %q, want the flags removed before -- only", got)
	}
	if _, header, err = takeOverloadRetryFlags([]string{"--retry-max-wait", "60m"}); err != nil || header != "max-wait=1h0m0s" {
		t.Fatalf("max-wait 60m header = %q err = %v", header, err)
	}
	if args, header, err = takeOverloadRetryFlags([]string{"exec"}); err != nil || header != "" || len(args) != 1 {
		t.Fatalf("no flags: args=%v header=%q err=%v", args, header, err)
	}
	// A client cannot ask for an unbounded wait (0), nor past the 60m cap.
	for _, bad := range [][]string{{"--retry-interval", "100ms"}, {"--retry-interval", "2h"}, {"--retry-interval"}, {"--retry-max-wait", "2h"}, {"--retry-max-wait", "0"}, {"--retry-max-wait=soon"}} {
		if _, _, err := takeOverloadRetryFlags(bad); err == nil {
			t.Fatalf("%v accepted", bad)
		}
	}
}

// sr codex sends the header as a leaf of its provider table; sr claude as
// one of Claude's custom headers.
func TestOverloadRetryFlagsProduceHeader(t *testing.T) {
	codexArgs := codexOverloadRetryConfigArgs("interval=2s,max-wait=20m0s")
	if strings.Join(codexArgs, " ") != `-c model_providers.subrouter.http_headers.X-Subrouter-Retry="interval=2s,max-wait=20m0s"` {
		t.Fatalf("codex args = %v", codexArgs)
	}

	body, err := proxyClaudeLaunchSettingsWithRetry("http://127.0.0.1:31415", "token", t.TempDir(), "interval=2s,max-wait=20m0s")
	if err != nil {
		t.Fatal(err)
	}
	var settings struct {
		Env map[string]string `json:"env"`
	}
	if err := json.Unmarshal(body, &settings); err != nil {
		t.Fatal(err)
	}
	if headers := settings.Env["ANTHROPIC_CUSTOM_HEADERS"]; !strings.Contains(headers, "\nX-Subrouter-Retry: interval=2s,max-wait=20m0s") {
		t.Fatalf("ANTHROPIC_CUSTOM_HEADERS = %q, want the retry header", headers)
	}
	body, err = proxyClaudeLaunchSettings("http://127.0.0.1:31415", "token", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "X-Subrouter-Retry") {
		t.Fatal("launch settings without the flags carry a retry header")
	}
	if _, err := proxyClaudeLaunchSettingsWithRetry("http://127.0.0.1:31415", "token", t.TempDir(), "interval=2s\nX-Evil: 1"); err == nil {
		t.Fatal("a header value with a newline was accepted")
	}
}
