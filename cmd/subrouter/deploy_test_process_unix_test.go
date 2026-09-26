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
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
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
	// Long enough for bash to start and record the child on a loaded host;
	// the child sleeps far longer, so the command still has to be canceled.
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
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
	deadline := time.Now().Add(3 * time.Second)
	for {
		err := syscall.Kill(childPID, syscall.Signal(0))
		if errors.Is(err, syscall.ESRCH) || deployTestProcessIsZombie(childPID) {
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

// deployTestProcessIsZombie reports whether pid has exited but was not reaped.
// An orphaned descendant is reparented to PID 1, and container inits that do
// not reap orphans leave it a zombie that still answers signal 0. Without /proc
// (macOS) this returns false and the signal probe alone decides.
func deployTestProcessIsZombie(pid int) bool {
	stat, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return false
	}
	end := strings.LastIndexByte(string(stat), ')')
	return end >= 0 && end+2 < len(stat) && stat[end+2] == 'Z'
}

func configureTestProcessGroup(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}

func deployTestProcessGroupSupported() bool { return true }

func terminateTestProcessGroup(command *exec.Cmd) {
	if command.Process == nil {
		return
	}
	_ = syscall.Kill(-command.Process.Pid, syscall.SIGTERM)
	time.Sleep(time.Second)
	_ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
}

func processExistsForDeployTest(pid int) bool {
	err := syscall.Kill(pid, syscall.Signal(0))
	return err == nil || !errors.Is(err, syscall.ESRCH)
}
