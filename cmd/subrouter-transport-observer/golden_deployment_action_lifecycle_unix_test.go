//go:build !windows

package main

import (
	"context"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestGoldenMigrationValidationFailureRunsDeploymentActionCleanup(t *testing.T) {
	root := t.TempDir()
	requestPath := filepath.Join(root, "request-path")
	cleanupPath := filepath.Join(root, "cleanup-ran")
	sentinelPath := filepath.Join(root, "remote-lock-sentinel")
	childPath := filepath.Join(root, "child.pid")
	childReadyPath := filepath.Join(root, "child.ready")
	action := []string{
		os.Args[0], "-test.run=^TestGoldenDeploymentActionLifecycleHelper$", "--",
		"--helper-cleanup", cleanupPath,
		"--helper-sentinel", sentinelPath,
		"--helper-child-pid", childPath,
		"--helper-child-ready", childReadyPath,
	}
	runner := &goldenRunner{
		privateRoot: root,
		artifactDir: root,
		testMode:    true,
		options: goldenOptions{
			migrationSwitch: action,
		},
	}
	prior := goldenActionSummary{
		EvidenceFile:   filepath.Base(requestPath),
		EvidenceSHA256: strings.Repeat("a", 64),
		EvidenceValid:  true,
	}
	source := &goldenSession{}
	_, _, err := runner.runMigrationTransitionWithProof(
		context.Background(), "final-cutover", "migration-cleanup", prior,
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

func TestTerminateProcessGroupAllowsGracefulCleanup(t *testing.T) {
	root := t.TempDir()
	readyPath := filepath.Join(root, "ready")
	cleanupPath := filepath.Join(root, "cleanup")
	scriptPath := filepath.Join(root, "cleanup-helper.sh")
	script := `#!/usr/bin/env bash
set -euo pipefail
ready_path="$1"
cleanup_path="$2"
cleanup() {
  trap - TERM
  sleep 0.25
  printf 'complete\n' >"${cleanup_path}"
  exit 0
}
trap cleanup TERM
printf 'ready\n' >"${ready_path}"
while :; do sleep 1; done
`
	if err := os.WriteFile(scriptPath, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	command := exec.Command(scriptPath, readyPath, cleanupPath)
	command.Stdout, command.Stderr = io.Discard, io.Discard
	configureProcessGroup(command)
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		killProcessGroup(command)
		_ = command.Wait()
	})
	waitGoldenLifecycleFile(t, readyPath)
	terminateProcessGroup(command)
	if err := command.Wait(); err != nil {
		t.Fatalf("cleanup helper did not exit gracefully: %v", err)
	}
	if _, err := os.Stat(cleanupPath); err != nil {
		t.Fatalf("graceful cleanup did not complete: %v", err)
	}
}

func TestGoldenDeploymentActionLifecycleHelper(t *testing.T) {
	cleanupPath := goldenLifecycleArgument("--helper-cleanup")
	if cleanupPath == "" {
		t.Skip("deployment action helper")
	}
	sentinelPath := goldenLifecycleArgument("--helper-sentinel")
	childPath := goldenLifecycleArgument("--helper-child-pid")
	childReadyPath := goldenLifecycleArgument("--helper-child-ready")
	requestPath := goldenLifecycleArgument("--destination-proof-request")
	if sentinelPath == "" || childPath == "" || childReadyPath == "" || requestPath == "" {
		t.Fatal("deployment action helper arguments are incomplete")
	}

	termination := make(chan os.Signal, 1)
	signal.Notify(termination, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(termination)
	if err := os.WriteFile(sentinelPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	child := exec.Command(
		os.Args[0], "-test.run=^TestGoldenDeploymentActionLifecycleResistantChild$", "--",
		"--helper-child-ready", childReadyPath,
	)
	child.Stdout, child.Stderr = io.Discard, io.Discard
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(childPath, []byte(strconv.Itoa(child.Process.Pid)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	waitGoldenLifecycleFile(t, childReadyPath)
	requestTemporary := requestPath + ".tmp"
	if err := os.WriteFile(requestTemporary, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(requestTemporary, requestPath); err != nil {
		t.Fatal(err)
	}
	<-termination
	if err := os.Remove(sentinelPath); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cleanupPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestGoldenDeploymentActionLifecycleResistantChild(t *testing.T) {
	readyPath := goldenLifecycleArgument("--helper-child-ready")
	if readyPath == "" {
		t.Skip("deployment action resistant child")
	}
	signal.Ignore(os.Interrupt, syscall.SIGHUP, syscall.SIGTERM)
	if err := os.WriteFile(readyPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for {
		time.Sleep(time.Hour)
	}
}

func goldenLifecycleArgument(name string) string {
	for index := 0; index+1 < len(os.Args); index++ {
		if os.Args[index] == name {
			return os.Args[index+1]
		}
	}
	return ""
}

func waitGoldenLifecycleFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := os.Stat(path); err == nil {
			return
		} else if !os.IsNotExist(err) {
			t.Fatal(err)
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", path)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
