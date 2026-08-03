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
	"testing"
	"time"
)

func TestGoldenStartLocalDaemonUsesKernelPortAndReapsProcessGroupWhenReadinessIsCanceled(t *testing.T) {
	root := t.TempDir()
	argumentsPath := filepath.Join(root, "arguments")
	leaderPath := filepath.Join(root, "leader.pid")
	childPath := filepath.Join(root, "child.pid")
	clientPath := filepath.Join(root, "never-ready-subrouter")
	script := fmt.Sprintf(`#!/bin/sh
printf '%%s\n' "$@" > %q
printf '%%s\n' "$$" > %q
sleep 300 &
child=$!
printf '%%s\n' "$child" > %q
wait "$child"
`, argumentsPath, leaderPath, childPath)
	if err := os.WriteFile(clientPath, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}

	runner := &goldenRunner{
		privateRoot: filepath.Join(root, "private"),
		evidence:    &jsonlRecorder{writer: io.Discard},
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	var startErr error
	go func() {
		_, _, startErr = runner.startLocalDaemon(ctx, clientPath, filepath.Join(root, "cloud.json"))
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			return
		}
		runner.mu.Lock()
		processes := append([]*exec.Cmd(nil), runner.processes...)
		runner.mu.Unlock()
		for _, command := range processes {
			if command == nil || command.Process == nil || command.ProcessState != nil {
				continue
			}
			killProcessGroup(command)
			_ = command.Wait()
		}
	})

	leaderPID := readGoldenLifecyclePID(t, leaderPath)
	childPID := readGoldenLifecyclePID(t, childPath)
	arguments, err := os.ReadFile(argumentsPath)
	if err != nil {
		t.Fatal(err)
	}
	fields := strings.Fields(string(arguments))
	address := ""
	for index := 0; index+1 < len(fields); index++ {
		if fields[index] == "--addr" {
			address = fields[index+1]
			break
		}
	}
	if address != "127.0.0.1:0" {
		t.Fatalf("daemon address = %q, want kernel-assigned 127.0.0.1:0", address)
	}
	cancel()
	select {
	case <-done:
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
			if parseErr == nil && pid > 0 {
				return pid
			}
		} else if !os.IsNotExist(err) {
			t.Fatal(err)
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for a valid PID in %s", path)
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
