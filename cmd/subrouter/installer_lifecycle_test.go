package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/manaflow-ai/subrouter/internal/accounts"
)

// useTempSystemdEtc points the systemd installer and controller at a scratch
// /etc so tests never read or write the host's configuration.
func useTempSystemdEtc(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	previous := systemdEtcDir
	systemdEtcDir = dir
	t.Cleanup(func() { systemdEtcDir = previous })
	for _, sub := range []string{"default", filepath.Join("systemd", "system")} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// saw reports whether exactly this command line was run.
func (r *recordingRunner) saw(call string) bool {
	for _, got := range r.calls {
		if got == call {
			return true
		}
	}
	return false
}

func writeTestFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// A reinstall or credential rotation that does not pass --addr must keep the
// existing listen address, switch interval and session store instead of
// silently rebinding the flag defaults (0.0.0.0:31415, 10m).
func TestInstallSystemdPreservesExistingAddrIntervalAndSessions(t *testing.T) {
	useTempSystemdEtc(t)
	writeTestFile(t, systemdDefaultPath(systemdConfig{ServiceName: defaultSystemdServiceName}), strings.Join([]string{
		"SUBROUTER_ADDR=127.0.0.1:4000",
		"SUBROUTER_STATE_DIR=/var/lib/subrouter",
		"SUBROUTER_SESSIONS=/srv/subrouter/sessions.json",
		"SUBROUTER_SR_SWITCH_INTERVAL=0",
		"",
	}, "\n"))

	config, err := parseInstallSystemdArgs([]string{"--replace-legacy=false"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if config.Addr != "127.0.0.1:4000" {
		t.Errorf("addr = %q, want preserved 127.0.0.1:4000", config.Addr)
	}
	if config.SRSwitchInterval != "0" {
		t.Errorf("sr switch interval = %q, want preserved 0", config.SRSwitchInterval)
	}
	if config.SessionsPath != "/srv/subrouter/sessions.json" {
		t.Errorf("sessions = %q, want preserved /srv/subrouter/sessions.json", config.SessionsPath)
	}

	explicit, err := parseInstallSystemdArgs([]string{
		"--replace-legacy=false",
		"--addr", "0.0.0.0:9000",
		"--cx-switch-interval", "5m",
		"--sessions", "/tmp/sessions.json",
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if explicit.Addr != "0.0.0.0:9000" || explicit.SRSwitchInterval != "5m" || explicit.SessionsPath != "/tmp/sessions.json" {
		t.Errorf("explicit flags were overridden by the EnvironmentFile: %+v", explicit)
	}
}

// Rewriting the socket unit with a new ListenStream has no effect on an active
// socket until the socket unit itself is restarted.
func TestApplySystemdUnitsRestartsSocketWhenItChanged(t *testing.T) {
	useTempSystemdEtc(t)
	config := systemdConfig{ServiceName: defaultSystemdServiceName, User: "subrouter", Group: "subrouter", Addr: "127.0.0.1:4000", Start: true}
	oldSocket, err := systemdSocket(systemdConfig{ServiceName: defaultSystemdServiceName, Addr: "0.0.0.0:31415"})
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, systemdSocketPath(config), oldSocket)
	newSocket, err := systemdSocket(config)
	if err != nil {
		t.Fatal(err)
	}

	runner := &recordingRunner{}
	if err := applySystemdUnits(config, "SUBROUTER_ADDR=127.0.0.1:4000\n", "[Unit]\n", newSocket, nil, runner); err != nil {
		t.Fatal(err)
	}
	if !runner.sawContaining("systemctl restart subrouter.socket") {
		t.Fatalf("changed socket unit was not restarted: %v", runner.calls)
	}
	if !runner.saw("systemctl restart subrouter") {
		t.Fatalf("service was not restarted: %v", runner.calls)
	}

	// Unchanged socket: only the service restarts, so an unrelated reinstall
	// does not drop the listening socket.
	unchanged := &recordingRunner{}
	if err := applySystemdUnits(config, "SUBROUTER_ADDR=127.0.0.1:4000\n", "[Unit]\n", newSocket, nil, unchanged); err != nil {
		t.Fatal(err)
	}
	if unchanged.sawContaining("restart subrouter.socket") {
		t.Fatalf("unchanged socket unit was restarted: %v", unchanged.calls)
	}
	if !unchanged.saw("systemctl restart subrouter") {
		t.Fatalf("service was not restarted: %v", unchanged.calls)
	}
}

// `sr up` against an agent that is already loaded must not kill the healthy
// running daemon with `kickstart -k`.
func TestLaunchdStartLeavesLoadedAgentRunning(t *testing.T) {
	runner := &recordingRunner{fail: map[string]error{
		"launchctl bootstrap": errors.New("Bootstrap failed: 5: Input/output error (already loaded)"),
	}}
	controller := launchdController{label: defaultDaemonLabel, home: t.TempDir(), runner: runner}
	if err := controller.start(); err != nil {
		t.Fatalf("start of a loaded agent failed: %v", err)
	}
	if runner.sawContaining("kickstart -k") {
		t.Fatalf("start killed the running daemon: %v", runner.calls)
	}
	if !runner.sawContaining("launchctl kickstart gui/") {
		t.Fatalf("start did not ensure the loaded agent is running: %v", runner.calls)
	}
}

// A bootstrap failure for an agent that is not loaded is a real error, not
// something to paper over with a kickstart.
func TestLaunchdStartReportsBootstrapFailureWhenNotLoaded(t *testing.T) {
	runner := &recordingRunner{fail: map[string]error{
		"launchctl bootstrap": errors.New("Bootstrap failed: 5: Input/output error"),
		"launchctl print":     errors.New("Could not find service"),
	}}
	controller := launchdController{label: defaultDaemonLabel, home: t.TempDir(), runner: runner}
	if err := controller.start(); err == nil {
		t.Fatalf("start succeeded although the agent could not be loaded: %v", runner.calls)
	}
	if runner.sawContaining("kickstart -k") {
		t.Fatalf("start used kickstart -k: %v", runner.calls)
	}
}

// For the socket-activated system install, stopping only the service leaves
// subrouter.socket listening (and it restarts the service on the next
// connection); removing only the service leaves the socket unit behind, which
// then makes refuseSystemSystemdConflict block a later `sr setup`.
func TestSystemdControllerSystemStopAndRemoveIncludeSocket(t *testing.T) {
	useTempSystemdEtc(t)
	config := systemdConfig{ServiceName: defaultSystemdServiceName}
	writeTestFile(t, systemdUnitPath(config), "[Unit]\n")
	writeTestFile(t, systemdSocketPath(config), "[Socket]\n")

	runner := &recordingRunner{}
	controller := systemdController{service: defaultSystemdServiceName, home: t.TempDir(), runner: runner}
	if err := controller.stop(); err != nil {
		t.Fatal(err)
	}
	if !runner.sawContaining("systemctl stop subrouter.socket") {
		t.Fatalf("stop left the socket listening: %v", runner.calls)
	}

	runner.calls = nil
	if err := controller.remove(); err != nil {
		t.Fatal(err)
	}
	if !runner.sawContaining("systemctl disable --now subrouter.socket") {
		t.Fatalf("remove left the socket enabled: %v", runner.calls)
	}
	if _, err := os.Stat(systemdSocketPath(config)); !os.IsNotExist(err) {
		t.Fatalf("socket unit file still present after remove: %v", err)
	}
	if err := refuseSystemSystemdConflict(systemdUnitPath(config), systemdSocketPath(config)); err != nil {
		t.Fatalf("setup would still be blocked after remove: %v", err)
	}
}

// The per-user systemd unit has no socket, so its commands must not mention one.
func TestSystemdControllerUserStopDoesNotTouchSocket(t *testing.T) {
	useTempSystemdEtc(t)
	home := t.TempDir()
	unit := userSystemdUnitPath(home, defaultSystemdServiceName)
	if err := os.MkdirAll(filepath.Dir(unit), 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, unit, "[Unit]\n")
	runner := &recordingRunner{}
	controller := systemdController{service: defaultSystemdServiceName, home: home, runner: runner}
	if err := controller.stop(); err != nil {
		t.Fatal(err)
	}
	if err := controller.remove(); err != nil {
		t.Fatal(err)
	}
	if runner.sawContaining(".socket") {
		t.Fatalf("user unit commands mention a socket: %v", runner.calls)
	}
}

// The scheduled task runs %LOCALAPPDATA%\Subrouter\bin\subrouter.exe, so the
// installer has to put the executable there.
func TestInstallWindowsTaskInstallsExecutable(t *testing.T) {
	paths := newWindowsPaths(t.TempDir())
	runner := &recordingRunner{}
	if err := installWindowsTask(paths, windowsTaskConfig{Addr: "127.0.0.1:31415"}, runner); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(paths.Exe)
	if err != nil {
		t.Fatalf("task executable was not installed: %v", err)
	}
	if info.Size() == 0 {
		t.Fatal("task executable is empty")
	}
}

func saveGCPServer(t *testing.T) accounts.CodexStore {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	store := accounts.DefaultCodexStore()
	if err := defaultSRServerStore(store).save(srServerFile{
		Servers: []srServerConfig{{
			Name:        "prod",
			URL:         "http://100.64.0.1:31415",
			GCPInstance: "subrouter-prod",
			GCPZone:     "us-central1-a",
		}},
	}); err != nil {
		t.Fatal(err)
	}
	return store
}

// Only flags the operator passed are forwarded to install-systemd, so a
// credential rotation keeps the host's configured address and interval.
func TestServerInstallGCPForwardsOnlyExplicitFlags(t *testing.T) {
	store := saveGCPServer(t)
	var out bytes.Buffer
	fake := &stdinCapturingRunner{}
	runner := srRunner{program: "sr", store: store, in: strings.NewReader(""), out: &out, errOut: &out, cmd: fake}
	if err := runner.run(context.Background(), []string{"server", "install", "prod"}); err != nil {
		t.Fatal(err)
	}
	script := fake.commands[0][len(fake.commands[0])-1]
	for _, flag := range []string{"--addr", "switch-interval", "--extra-args"} {
		if strings.Contains(script, flag) {
			t.Fatalf("unrequested %s forwarded; it would override the host's config:\n%s", flag, script)
		}
	}

	fake = &stdinCapturingRunner{}
	runner.cmd = fake
	if err := runner.run(context.Background(), []string{
		"server", "install", "prod",
		"--addr", "10.0.0.5:31415", "--sr-switch-interval", "0", "--extra-args", "--fetch-usage=false",
	}); err != nil {
		t.Fatal(err)
	}
	script = fake.commands[0][len(fake.commands[0])-1]
	for _, want := range []string{"--addr '10.0.0.5:31415'", "--sr-switch-interval '0'", "--extra-args '--fetch-usage=false'"} {
		if !strings.Contains(script, want) {
			t.Fatalf("explicit flag %q not forwarded:\n%s", want, script)
		}
	}
}

// The SSH install path cannot honor --addr and friends on every platform it
// supports, so it must refuse them rather than silently drop them.
func TestServerInstallOverSSHRejectsUnforwardedFlags(t *testing.T) {
	for _, args := range [][]string{
		{"--addr", "127.0.0.1:4000"},
		{"--sr-switch-interval", "0"},
		{"--cx-switch-interval", "0"},
		{"--extra-args", "--fetch-usage=false"},
	} {
		home := t.TempDir()
		t.Setenv("HOME", home)
		store := accounts.DefaultCodexStore()
		if err := defaultSRServerStore(store).save(srServerFile{
			Servers: []srServerConfig{{Name: "mac-mini", URL: "http://100.64.0.9:31415", SSHHost: "worker@mac-mini"}},
		}); err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		fake := &stdinCapturingRunner{}
		runner := srRunner{program: "sr", store: store, in: strings.NewReader(""), out: &out, errOut: &out, cmd: fake}
		err := runner.run(context.Background(), append([]string{"server", "install", "mac-mini"}, args...))
		if err == nil {
			t.Fatalf("%v was silently ignored on the SSH path", args)
		}
		if len(fake.commands) != 0 {
			t.Fatalf("%v: ran %v despite rejecting the flag", args, fake.commands)
		}
	}
}
