//go:build !windows

package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func runDeployTestCommand(command *exec.Cmd) ([]byte, error) {
	originalPath := command.Path
	if filepath.Base(originalPath) == "bash" {
		originalPath = "/bin/bash"
	}
	originalArgs := append([]string(nil), command.Args[1:]...)
	command.Path = "/bin/sh"
	command.Args = append([]string{
		"sh", "-c", `"$@"; status=$?; :; exit "$status"`, "subrouter-deploy-test", originalPath,
	}, originalArgs...)
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Cancel = func() error {
		return killDeployTestProcessGroup(command)
	}
	command.WaitDelay = time.Second
	return command.CombinedOutput()
}

func killDeployTestProcessGroup(command *exec.Cmd) error {
	if command == nil || command.Process == nil {
		return nil
	}
	err := syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
	if errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return err
}

func TestDeployTestCommandCancelsDescendantProcessGroup(t *testing.T) {
	requireDeployScriptTools(t, "bash")
	pidPath := filepath.Join(t.TempDir(), "child.pid")
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	command := exec.CommandContext(ctx, mustLookPath(t, "bash"), "-c", `
sleep 30 &
child=$!
printf '%s\n' "$child" >"$DEPLOY_TEST_CHILD_PID"
wait "$child"
`)
	command.Env = append(os.Environ(), "DEPLOY_TEST_CHILD_PID="+pidPath)
	if output, err := runDeployTestCommand(command); err == nil || !errors.Is(ctx.Err(), context.DeadlineExceeded) {
		t.Fatalf("bounded command did not time out: err=%v context=%v output=%s", err, ctx.Err(), output)
	}
	body, err := os.ReadFile(pidPath)
	if err != nil {
		t.Fatal(err)
	}
	childPID, err := strconv.Atoi(strings.TrimSpace(string(body)))
	if err != nil || childPID <= 0 {
		t.Fatalf("invalid child PID %q: %v", body, err)
	}
	deadline := time.Now().Add(time.Second)
	for {
		err := syscall.Kill(childPID, syscall.Signal(0))
		if errors.Is(err, syscall.ESRCH) {
			return
		}
		if err != nil {
			t.Fatalf("probe child PID %d: %v", childPID, err)
		}
		if time.Now().After(deadline) {
			t.Fatalf("descendant PID %d survived process-group cancellation", childPID)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
