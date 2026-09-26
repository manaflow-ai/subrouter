//go:build linux

package main

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

const processIdentityHelperEnv = "SUBROUTER_PROCESS_IDENTITY_HELPER"

func TestExecutableIdentityForProcessHandlesParenthesisSpaceInComm(t *testing.T) {
	if os.Getenv(processIdentityHelperEnv) == "1" {
		// /proc/self/comm names the thread-group leader, which is what
		// /proc/<pid>/stat reports. prctl(PR_SET_NAME) renamed only the
		// calling thread, and a test goroutine usually is not on the main
		// thread, so the parent waited out its deadline on an unrenamed
		// process.
		if err := os.WriteFile("/proc/self/comm", []byte("sr) worker"), 0); err != nil {
			os.Exit(2)
		}
		for {
			time.Sleep(time.Hour)
		}
	}

	child := exec.Command(os.Args[0], "-test.run", "^TestExecutableIdentityForProcessHandlesParenthesisSpaceInComm$", "-test.v")
	child.Env = append(os.Environ(), processIdentityHelperEnv+"=1")
	child.Stdout = io.Discard
	child.Stderr = io.Discard
	if err := child.Start(); err != nil {
		t.Fatalf("start helper: %v", err)
	}
	t.Cleanup(func() {
		_ = child.Process.Kill()
		_ = child.Wait()
	})

	statPath := fmt.Sprintf("/proc/%d/stat", child.Process.Pid)
	var stat []byte
	// A hang guard, not a latency bound: under -race on a loaded host the
	// re-executed test binary can take seconds to reach its prctl.
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		var err error
		stat, err = os.ReadFile(statPath)
		if err == nil && strings.Contains(string(stat), "(sr) worker)") {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !strings.Contains(string(stat), "(sr) worker)") {
		t.Fatalf("helper process name did not become visible in %s: %q", statPath, stat)
	}

	statText := string(stat)
	closing := strings.LastIndex(statText, ") ")
	if closing < 0 {
		t.Fatalf("helper stat has no closing delimiter: %q", statText)
	}
	fields := strings.Fields(statText[closing+2:])
	if len(fields) < 20 {
		t.Fatalf("helper stat has too few fields: %q", statText)
	}
	want := "linux:" + fields[19]
	identity, err := executableIdentityForProcess(child.Process.Pid)
	if err != nil {
		t.Fatalf("executable identity: %v", err)
	}
	if identity.StartIdentity != want {
		t.Fatalf("start identity = %q, want %q for stat %q", identity.StartIdentity, want, statText)
	}
}
