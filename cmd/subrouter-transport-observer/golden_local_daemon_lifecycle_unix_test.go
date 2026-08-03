//go:build !windows

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestGoldenStartLocalDaemonReapsProcessGroupWhenReadinessIsCanceled(t *testing.T) {
	root := t.TempDir()
	leaderPath := filepath.Join(root, "leader.pid")
	childPath := filepath.Join(root, "child.pid")
	clientPath := filepath.Join(root, "never-ready-subrouter")
	script := fmt.Sprintf(`#!/bin/sh
printf '%%s\n' "$$" > %q
sleep 300 &
child=$!
printf '%%s\n' "$child" > %q
wait "$child"
`, leaderPath, childPath)
	if err := os.WriteFile(clientPath, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}

	runner := &goldenRunner{
		privateRoot: filepath.Join(root, "private"),
		evidence:    &jsonlRecorder{writer: io.Discard},
	}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, _, err := runner.startLocalDaemon(ctx, clientPath, filepath.Join(root, "cloud.json"))
		result <- err
	}()

	leaderPID := readGoldenLifecyclePID(t, leaderPath)
	childPID := readGoldenLifecyclePID(t, childPath)
	cancel()
	var startErr error
	select {
	case startErr = <-result:
	case <-time.After(2 * time.Second):
		t.Fatal("startLocalDaemon did not return after cancellation")
	}
	if !errors.Is(startErr, context.Canceled) {
		t.Fatalf("start error = %v, want context.Canceled", startErr)
	}

	runner.mu.Lock()
	processes := append([]*exec.Cmd(nil), runner.processes...)
	stderrDone := runner.localStderrDone
	runner.mu.Unlock()
	if len(processes) != 1 {
		t.Fatalf("started processes = %d, want 1", len(processes))
	}
	command := processes[0]
	t.Cleanup(func() {
		_ = syscall.Kill(-leaderPID, syscall.SIGKILL)
		if command.ProcessState == nil {
			_ = command.Wait()
		}
	})
	if command.ProcessState == nil {
		t.Fatal("startLocalDaemon returned without reaping the canceled daemon")
	}
	select {
	case <-stderrDone:
	case <-time.After(time.Second):
		t.Fatal("startLocalDaemon returned before the daemon stderr reader finished")
	}
	waitGoldenLifecycleProcessGone(t, leaderPID)
	waitGoldenLifecycleProcessGone(t, childPID)
}

func readGoldenLifecyclePID(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		data, err := os.ReadFile(path)
		if err == nil {
			pid, parseErr := strconv.Atoi(strings.TrimSpace(string(data)))
			if parseErr != nil || pid <= 0 {
				t.Fatalf("invalid PID in %s: %q", path, data)
			}
			return pid
		}
		if !os.IsNotExist(err) {
			t.Fatal(err)
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", path)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func waitGoldenLifecycleProcessGone(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for {
		process, err := os.FindProcess(pid)
		if err != nil || !processAlive(process) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("process %d remained alive after startup cancellation", pid)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
