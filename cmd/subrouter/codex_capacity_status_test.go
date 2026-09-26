package main

import (
	"strings"
	"testing"
	"time"
)

func TestCodexCapacityStatusLine(t *testing.T) {
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	lines := codexCapacityStatusLines([]codexCapacitySheddingView{
		{Model: "gpt-6-astra", Tier: "priority", FailureRatio: 0.72, Samples: 50, Shedding: true, Since: now.Add(-90 * time.Second), RetryBudget: "3s"},
		{Model: "gpt-6-astra", Tier: "default", FailureRatio: 0.1, Samples: 20},
	}, now)
	if len(lines) != 1 {
		t.Fatalf("lines = %q, want one line for the shedding pool", lines)
	}
	line := lines[0]
	for _, want := range []string{"gpt-6-astra", "priority", "72%", "1m30s", "3s"} {
		if !strings.Contains(line, want) {
			t.Fatalf("line %q missing %q", line, want)
		}
	}
	if got := codexCapacityStatusLines(nil, now); len(got) != 0 {
		t.Fatalf("no shedding printed %q", got)
	}
}
