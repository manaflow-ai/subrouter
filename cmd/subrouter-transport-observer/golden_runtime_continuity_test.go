package main

import (
	"strings"
	"testing"
)

func TestGoldenStableLocalEgressRejectsSocketSetChanges(t *testing.T) {
	socketA := strings.Repeat("a", 64)
	socketB := strings.Repeat("b", 64)
	socketC := strings.Repeat("c", 64)
	tests := []struct {
		name   string
		before []string
		after  []string
		want   string
	}{
		{name: "unrelated new socket", before: []string{socketA}, after: []string{socketA, socketB}, want: "local_egress_unrelated_socket"},
		{name: "one bound socket disappears", before: []string{socketA, socketB}, after: []string{socketA}, want: "local_egress_socket_disappeared"},
		{name: "reconnected socket", before: []string{socketA, socketB}, after: []string{socketA, socketC}, want: "local_egress_socket_reconnected"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			before := map[string]goldenProcessEvidence{
				"local-daemon": {Label: "local-daemon", RemoteSocketIDs: test.before},
			}
			after := map[string]goldenProcessEvidence{
				"local-daemon": {Label: "local-daemon", RemoteSocketIDs: test.after},
			}
			if got := fixedGoldenFailure(requireStableLocalEgress(before, after)); got != test.want {
				t.Fatalf("failure = %q, want %q", got, test.want)
			}
		})
	}
}

func TestGoldenCounterContinuityRejectsActionRebaselining(t *testing.T) {
	tests := []struct {
		name string
		edit func(*goldenSummary)
	}{
		{
			name: "slot restart between activation and rollback",
			edit: func(summary *goldenSummary) {
				summary.Rollback.canonical.Metrics.RetiringSlot.NRestarts = goldenDeployCounter{
					Before: goldenInt64(1), After: goldenInt64(1),
				}
			},
		},
		{
			name: "slot oom between rollback and retirement",
			edit: func(summary *goldenSummary) {
				summary.OldGenerationCleanup.canonical.Metrics.OldSlot = validGoldenServiceMetrics(goldenRSSLimitBytes)
				summary.OldGenerationCleanup.canonical.Metrics.OldSlot.OOMKill = goldenDeployCounter{
					Before: goldenInt64(1), After: goldenInt64(1),
				}
			},
		},
		{
			name: "front oom between migration transitions",
			edit: func(summary *goldenSummary) {
				summary.MigrationRollback.migrationCanonical.Metrics.Front.OOMKill = goldenDeployCounter{
					Before: goldenInt64(1), After: goldenInt64(1),
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			summary := validGoldenAcceptanceSummary()
			test.edit(&summary)
			if got := fixedGoldenFailure(validateGoldenSummary(summary, true)); got != "server_counter_continuity_invalid" {
				t.Fatalf("failure = %q, want server_counter_continuity_invalid", got)
			}
		})
	}
}

func TestGoldenAgentPayloadRequiresExactOrderedNumberedLines(t *testing.T) {
	nonce := "nonce_0123456789abcdef"
	marker := "SR_GOLDEN_COMPLETE_0123456789abcdef"
	tests := []struct {
		name string
		text string
		ok   bool
	}{
		{name: "valid", text: nonce + "\n1 x\n2 x\n3 x\n" + marker, ok: true},
		{name: "duplicate nonce", text: nonce + "\n" + nonce + "\n1 x\n2 x\n3 x\n" + marker},
		{name: "missing numbered line", text: nonce + "\n1 x\n3 x\n" + marker},
		{name: "duplicate numbered line", text: nonce + "\n1 x\n2 x\n2 x\n3 x\n" + marker},
		{name: "reordered numbered lines", text: nonce + "\n2 x\n1 x\n3 x\n" + marker},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			evidence, err := validateGoldenAgentMessagePayload(test.text, nonce, marker, 3)
			if test.ok {
				if err != nil {
					t.Fatal(err)
				}
				if evidence.NumberedLineCount != 3 || len(evidence.NumberedLinesSHA256) != 64 {
					t.Fatalf("evidence = %#v", evidence)
				}
				return
			}
			if err == nil {
				t.Fatalf("accepted malformed payload: %#v", evidence)
			}
		})
	}
}
