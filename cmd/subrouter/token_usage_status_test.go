package main

import (
	"strings"
	"testing"

	"github.com/manaflow-ai/subrouter/internal/proxy"
)

func TestTokenUsageStatusLinesShowSwitchesAndLatency(t *testing.T) {
	rows := []proxy.TokenUsageRow{
		{Provider: "codex", AccountID: "a", Requests: 90, InputTokens: 9_000_000, CachedInputTokens: 7_200_000,
			TTFBCount: 90, TTFBMsMax: 3100, TTFBMsBuckets: []int64{0, 60, 20, 5, 5}},
		{Provider: "codex", AccountID: "b", Requests: 10, InputTokens: 1_000_000, CachedInputTokens: 800_000,
			AccountSwitches: 4, AccountSwitchesInRequest: 1, AccountSwitchInputTokens: 1_200_000,
			TTFBCount: 10, TTFBMsMax: 900, TTFBMsBuckets: []int64{0, 5, 5},
			UpstreamErrors: map[string]int64{"429": 2, "500": 1}},
		// A daemon without the new fields: tokens only.
		{Provider: "claude", AccountID: "c", Requests: 3, InputTokens: 3000, CachedInputTokens: 1500},
		{Provider: "kimi", AccountID: "d"},
	}
	lines := tokenUsageStatusLines(rows, "24h")
	want := []string{
		"Token usage (24h)     claude 3 turns · 3.0K in, 50% cached",
		"                      codex 100 turns · 10.0M in, 80% cached · 4 account switches (1 mid-request), 1.2M input tokens sent cold · TTFB p95 2s · 3 upstream errors",
	}
	if strings.Join(lines, "\n") != strings.Join(want, "\n") {
		t.Fatalf("lines:\n%s\nwant:\n%s", strings.Join(lines, "\n"), strings.Join(want, "\n"))
	}
}

func TestTokenUsageStatusLinesShowZeroSwitchesFromNewDaemon(t *testing.T) {
	lines := tokenUsageStatusLines([]proxy.TokenUsageRow{{Provider: "codex", Requests: 2, TTFBCount: 2, TTFBMsMax: 200, TTFBMsBuckets: []int64{2}}}, "24h")
	if len(lines) != 1 || !strings.Contains(lines[0], "0 account switches · TTFB p95 200ms") {
		t.Fatalf("lines = %q", lines)
	}
}
