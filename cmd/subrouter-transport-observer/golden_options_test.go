package main

import (
	"strings"
	"testing"
)

func TestGoldenOptionsRequireV0153Candidate(t *testing.T) {
	previousHooks := goldenTestHooks
	goldenTestHooks.enabled = false
	t.Cleanup(func() { goldenTestHooks = previousHooks })

	args := []string{
		"--predecessor-version", "v0.1.51",
		"--predecessor-sha256", goldenPinnedPredecessorSHA256,
		"--candidate-tag", "v0.1.53",
		"--candidate-sha256", strings.Repeat("b", 64),
		"--candidate-revision", strings.Repeat("c", 40),
		"--deploy-evidence-validator", "validator",
		"--stream-lines", "100",
		"--migration-prepare", "true",
		"--migration-switch", "true",
		"--legacy-retirement", "true",
		"--activate", "true",
		"--rollback", "true",
		"--old-generation-check", "true",
	}
	if _, err := parseGoldenArgs(args); err != nil {
		t.Fatalf("v0.1.53 candidate was rejected: %v", err)
	}
	for index := range args {
		if args[index] == "v0.1.53" {
			args[index] = "v0.1.52"
			break
		}
	}
	if _, err := parseGoldenArgs(args); err == nil {
		t.Fatal("superseded v0.1.52 candidate was accepted")
	}
}
