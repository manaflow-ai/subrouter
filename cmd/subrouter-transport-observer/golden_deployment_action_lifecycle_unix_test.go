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
	childReadyPath := filepath.Join(root, "child.ready")
	resistantChildPath := filepath.Join(root, "resistant-child")
	actionPath := filepath.Join(root, "migration-action")
	resistantChild := fmt.Sprintf(`#!/bin/bash
(trap '' HUP INT TERM; exec sleep 300) &
child=$!
printf '%%s\n' "$child" > %q
: > %q
`, childPath, childReadyPath)
	writeGoldenExecutable(t, resistantChildPath, resistantChild)
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
%q
while [ ! -e %q ]; do sleep 0.01; done
printf '{}\n' > "${request_path}.tmp"
mv "${request_path}.tmp" "$request_path"
while :; do sleep 300; done
`, sentinelPath, cleanupPath, sentinelPath, resistantChildPath, childReadyPath)
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
