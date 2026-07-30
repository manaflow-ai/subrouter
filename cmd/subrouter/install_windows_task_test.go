package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type recordingRunner struct {
	calls []string
	fail  map[string]error
}

func (r *recordingRunner) Run(name string, args ...string) error {
	call := name + " " + strings.Join(args, " ")
	r.calls = append(r.calls, call)
	for prefix, err := range r.fail {
		if strings.Contains(call, prefix) {
			return err
		}
	}
	return nil
}

func (r *recordingRunner) sawContaining(fragment string) bool {
	for _, call := range r.calls {
		if strings.Contains(call, fragment) {
			return true
		}
	}
	return false
}

// The daemon holds credentials, so it must never listen anywhere but loopback.
func TestWindowsTaskRefusesNonLoopbackAddress(t *testing.T) {
	for _, addr := range []string{"0.0.0.0:31415", "192.168.1.10:31415", ":31415"} {
		if _, err := windowsTaskArguments(windowsTaskConfig{Exe: `C:\s.exe`, Addr: addr}); err == nil {
			t.Errorf("address %q was accepted; it must be rejected", addr)
		}
	}
	args, err := windowsTaskArguments(windowsTaskConfig{Exe: `C:\s.exe`, Addr: "127.0.0.1:31415"})
	if err != nil {
		t.Fatalf("loopback address rejected: %v", err)
	}
	if args != "serve --addr 127.0.0.1:31415" {
		t.Fatalf("arguments = %q", args)
	}
}

func TestWindowsTaskArgumentsIncludeSwitchIntervalWhenSet(t *testing.T) {
	args, err := windowsTaskArguments(windowsTaskConfig{
		Exe: `C:\s.exe`, Addr: "127.0.0.1:31415", SRSwitchInterval: "10m",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(args, "--sr-switch-interval 10m") {
		t.Fatalf("arguments = %q, want the switch interval", args)
	}
	off, err := windowsTaskArguments(windowsTaskConfig{
		Exe: `C:\s.exe`, Addr: "127.0.0.1:31415", SRSwitchInterval: "0",
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(off, "sr-switch-interval") {
		t.Fatalf("arguments = %q, want no interval when disabled", off)
	}
}

// The task definition carries the three properties that justify choosing Task
// Scheduler over a Run-key entry: start at logon, restart after failure, and no
// elevation.
func TestWindowsTaskDefinitionHasLogonRestartAndNoElevation(t *testing.T) {
	definition, err := windowsTaskXML(windowsTaskConfig{
		Exe:              `C:\Users\lc\AppData\Local\Subrouter\bin\subrouter.exe`,
		Addr:             "127.0.0.1:31415",
		WorkingDirectory: `C:\Users\lc\AppData\Local\Subrouter`,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"<LogonTrigger>",
		"<RestartOnFailure>",
		"<Count>3</Count>",
		"<Interval>PT1M</Interval>",
		"<RunLevel>LeastPrivilege</RunLevel>",
		"<MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy>",
		"<ExecutionTimeLimit>PT0S</ExecutionTimeLimit>",
		"subrouter.exe",
		"serve --addr 127.0.0.1:31415",
	} {
		if !strings.Contains(definition, want) {
			t.Errorf("task definition missing %q", want)
		}
	}
	if strings.Contains(definition, "HighestAvailable") {
		t.Error("task requests elevation; per-user credentials must not run elevated")
	}
}

func TestWindowsPathsStayUnderLocalAppData(t *testing.T) {
	paths := newWindowsPaths(`C:\Users\lc\AppData\Local`)
	root := filepath.Join(`C:\Users\lc\AppData\Local`, "Subrouter")
	if paths.Root != root {
		t.Fatalf("root = %q", paths.Root)
	}
	for name, dir := range map[string]string{
		"bin": paths.BinDir, "logs": paths.LogDir, "state": paths.StateDir,
		"config": paths.ConfigDir, "tasks": paths.TaskXMLDir,
	} {
		if !strings.HasPrefix(dir, root) {
			t.Errorf("%s dir %q escaped %q", name, dir, root)
		}
	}
	// Roaming would sync credentials into a domain profile.
	if strings.Contains(paths.Root, "Roaming") {
		t.Error("paths must not use the roaming profile")
	}
}

func TestInstallWindowsTaskRegistersAndWritesUTF16(t *testing.T) {
	paths := newWindowsPaths(t.TempDir())
	runner := &recordingRunner{}
	if err := installWindowsTask(paths, windowsTaskConfig{Addr: "127.0.0.1:31415"}, runner); err != nil {
		t.Fatal(err)
	}
	if !runner.sawContaining("schtasks /Create /TN " + defaultWindowsTaskName) {
		t.Fatalf("calls = %v", runner.calls)
	}
	// /F makes reinstallation repeatable instead of failing on an existing task.
	if !runner.sawContaining("/F") {
		t.Fatalf("registration is not idempotent: %v", runner.calls)
	}
	body, err := os.ReadFile(filepath.Join(paths.TaskXMLDir, "daemon.xml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(body) < 2 || body[0] != 0xFF || body[1] != 0xFE {
		t.Fatal("task XML lacks a UTF-16LE BOM; schtasks /XML rejects UTF-8")
	}
	for _, dir := range []string{paths.BinDir, paths.LogDir, paths.StateDir, paths.ConfigDir} {
		info, err := os.Stat(dir)
		if err != nil {
			t.Fatalf("stat %s: %v", dir, err)
		}
		if perm := info.Mode().Perm(); perm != 0o700 {
			t.Errorf("%s mode %o, want 700", dir, perm)
		}
	}
}

func TestWindowsControllerIssuesExpectedCommands(t *testing.T) {
	runner := &recordingRunner{}
	controller := windowsController{name: defaultWindowsTaskName, runner: runner}

	if err := controller.start(); err != nil {
		t.Fatal(err)
	}
	if err := controller.stop(); err != nil {
		t.Fatal(err)
	}
	if err := controller.restart(); err != nil {
		t.Fatal(err)
	}
	if err := controller.remove(); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"/Run", "/End", "/Delete"} {
		if !runner.sawContaining("schtasks " + want) {
			t.Errorf("missing schtasks %s in %v", want, runner.calls)
		}
	}
	if !controller.installed() {
		t.Error("installed() should be true when /Query succeeds")
	}
	if got := controller.describe(); !strings.Contains(got, defaultWindowsTaskName) {
		t.Errorf("describe() = %q", got)
	}
}

// restart must survive a task that is not currently running, which is the
// common case after a crash.
func TestWindowsControllerRestartsWhenNotRunning(t *testing.T) {
	runner := &recordingRunner{fail: map[string]error{"/End": fmt.Errorf("task is not running")}}
	controller := windowsController{name: defaultWindowsTaskName, runner: runner}
	if err := controller.restart(); err != nil {
		t.Fatalf("restart failed because the task was stopped: %v", err)
	}
	if !runner.sawContaining("schtasks /Run") {
		t.Fatalf("restart never started the task: %v", runner.calls)
	}
}

func TestWindowsControllerReportsMissingTask(t *testing.T) {
	runner := &recordingRunner{fail: map[string]error{"/Query": fmt.Errorf("cannot find the task")}}
	controller := windowsController{name: defaultWindowsTaskName, runner: runner}
	if controller.installed() {
		t.Error("installed() should be false when /Query fails")
	}
}

// The credential directory must end up user-only, with inheritance removed.
func TestSecureWindowsCredentialDirLocksToOneUser(t *testing.T) {
	runner := &recordingRunner{}
	if err := secureWindowsCredentialDir(`C:\state`, "lc", runner); err != nil {
		t.Fatal(err)
	}
	if !runner.sawContaining("icacls C:\\state /inheritance:r") {
		t.Fatalf("inheritance was not removed: %v", runner.calls)
	}
	if !runner.sawContaining(`/grant:r lc:(OI)(CI)F`) {
		t.Fatalf("user was not granted sole access: %v", runner.calls)
	}
	if err := secureWindowsCredentialDir(`C:\state`, "", runner); err == nil {
		t.Error("locking without a user name should fail rather than silently skip")
	}
}
