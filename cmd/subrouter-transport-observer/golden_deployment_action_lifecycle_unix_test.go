//go:build !windows

package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGoldenMigrationValidationFailureRunsDeploymentActionCleanup(t *testing.T) {
	root := t.TempDir()
	requestPath := filepath.Join(root, "request-path")
	cleanupPath := filepath.Join(root, "cleanup-ran")
	sentinelPath := filepath.Join(root, "remote-lock-sentinel")
	childPath := filepath.Join(root, "child.pid")
	actionPath := filepath.Join(root, "migration-action")
	script := fmt.Sprintf(`#!/bin/bash
set -eu
request_path=""
while [ "$#" -gt 0 ]; do
  case "$1" in
    --destination-proof-request)
      request_path="$2"
      shift 2
      ;;
    *)
      shift
      ;;
  esac
done
cleanup() {
  status=$?
  trap - EXIT INT TERM
  rm -f %q
  : > %q
  exit "$status"
}
trap cleanup EXIT INT TERM
: > %q
(trap '' TERM; while :; do sleep 300; done) &
child=$!
printf '%%s\n' "$child" > %q
printf '{}\n' > "${request_path}.tmp"
mv "${request_path}.tmp" "$request_path"
while :; do sleep 300; done
`, sentinelPath, cleanupPath, sentinelPath, childPath)
	writeGoldenExecutable(t, actionPath, script)

	runner := &goldenRunner{
		privateRoot: root,
		artifactDir: root,
		testMode:    true,
		options: goldenOptions{
			migrationSwitch: []string{actionPath},
		},
	}
	prior := goldenActionSummary{
		EvidenceFile:   filepath.Base(requestPath),
		EvidenceSHA256: strings.Repeat("a", 64),
		EvidenceValid:  true,
	}
	source := &goldenSession{}
	_, _, err := runner.runMigrationTransitionWithProof(
		context.Background(), "rehearsal-cutover", "migration-cleanup", prior,
		goldenCycleInputs{}, []*goldenSession{source}, []*goldenContinuityMonitor{{}},
	)
	if got := fixedGoldenFailure(err); got != "migration_destination_proof_request_invalid" {
		t.Fatalf("failure = %q, want migration_destination_proof_request_invalid", got)
	}
	if _, err := os.Stat(cleanupPath); err != nil {
		t.Fatalf("deployment action cleanup did not run: %v", err)
	}
	if _, err := os.Stat(sentinelPath); !os.IsNotExist(err) {
		t.Fatalf("deployment lock sentinel survived cleanup: %v", err)
	}
	childPID := readGoldenLifecyclePID(t, childPath)
	childProcess, err := os.FindProcess(childPID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = childProcess.Kill() })
	waitGoldenLifecycleProcessGone(t, childPID)
}
