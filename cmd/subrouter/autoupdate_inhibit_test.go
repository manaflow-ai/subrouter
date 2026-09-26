//go:build darwin

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestMacOSAutoUpdaterDefersDuringDeploymentTransaction(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "upgrade-inhibited")
	if err := os.WriteFile(marker, []byte("transaction\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	repoRoot := filepath.Clean(filepath.Join("..", ".."))
	command := exec.Command("bash", filepath.Join(repoRoot, "deploy", "macos", "subrouter-autoupdate.sh"))
	command.Env = append(os.Environ(),
		"SUBROUTER_UPGRADE_INHIBIT_FILE="+marker,
		"SUBROUTER_MUTATION_LOCK_FILE="+filepath.Join(t.TempDir(), "mutation.lock"),
	)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("autoupdater with active transaction marker: %v\n%s", err, output)
	}
	if !strings.Contains(string(output), "worker update deferred") {
		t.Fatalf("autoupdater did not report a deferred update: %s", output)
	}
}

func TestMacOSAutoUpdaterDefersBeforeAnyMutationWhenLeaseIsHeld(t *testing.T) {
	lock := filepath.Join(t.TempDir(), "supervisor-mutation.lock")
	file, err := os.OpenFile(lock, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = file.Close()
		t.Fatalf("acquire mutation lease: %v", err)
	}
	t.Cleanup(func() {
		_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
		_ = file.Close()
	})
	repoRoot := filepath.Clean(filepath.Join("..", ".."))
	command := exec.Command("bash", filepath.Join(repoRoot, "deploy", "macos", "subrouter-autoupdate.sh"))
	command.Env = append(os.Environ(), "SUBROUTER_MUTATION_LOCK_FILE="+lock)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("autoupdater with held mutation lease: %v\n%s", err, output)
	}
	if !strings.Contains(string(output), "mutation lease; update deferred") {
		t.Fatalf("autoupdater did not defer before mutation: %s", output)
	}
}
