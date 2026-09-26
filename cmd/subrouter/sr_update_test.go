//go:build !windows

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// The update tests never touch a real release, launchctl or systemctl: the
// release server, daemon health and service controller are all fakes, and the
// "binaries" are shell scripts that answer `version`.

type updateFixture struct {
	t          *testing.T
	root       string
	binary     string
	server     *httptest.Server
	u          *updater
	out        *bytes.Buffer
	controller *fakeUpdateController

	mu         sync.Mutex
	latest     string
	corruptSum bool
	broken     map[string]bool // versions whose daemon never becomes healthy
	healthy    bool
	running    string
	restarts   int
	upgrades   []string
}

type fakeUpdateController struct{ f *updateFixture }

func (c *fakeUpdateController) start() error     { return nil }
func (c *fakeUpdateController) stop() error      { return nil }
func (c *fakeUpdateController) installed() bool  { return true }
func (c *fakeUpdateController) remove() error    { return nil }
func (c *fakeUpdateController) describe() string { return "fake.service" }
func (c *fakeUpdateController) restart() error {
	c.f.restartFromDisk()
	return nil
}

func fakeBinaryScript(version string) []byte {
	return []byte(fmt.Sprintf("#!/bin/sh\nif [ \"$1\" = version ]; then echo \"subrouter %s (commit test, built now, go)\"; fi\nexit 0\n", version))
}

// restartFromDisk plays the daemon: it starts whatever binary is installed.
func (f *updateFixture) restartFromDisk() {
	version := f.u.binaryVersion(context.Background(), f.binary)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.restarts++
	f.running = version
	f.healthy = !f.broken[version]
}

func newUpdateFixture(t *testing.T, installed string) *updateFixture {
	t.Helper()
	root := t.TempDir()
	binDir := filepath.Join(root, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(binDir, "subrouter")
	if err := os.WriteFile(binary, fakeBinaryScript(installed), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, alias := range []string{"sr", "cx"} {
		if err := os.Symlink(binary, filepath.Join(binDir, alias)); err != nil {
			t.Fatal(err)
		}
	}
	f := &updateFixture{
		t:       t,
		root:    root,
		binary:  binary,
		out:     &bytes.Buffer{},
		latest:  "v1.1.0",
		broken:  map[string]bool{},
		healthy: true,
		running: installed,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		latest := f.latest
		f.mu.Unlock()
		http.Redirect(w, r, "/releases/tag/"+latest, http.StatusFound)
	})
	mux.HandleFunc("/releases/tag/", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("release page")) })
	mux.HandleFunc("/download/", func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/download/"), "/")
		if len(parts) != 2 {
			http.NotFound(w, r)
			return
		}
		tag, name := parts[0], parts[1]
		body := fakeBinaryScript(tag)
		switch name {
		case "SHA256SUMS":
			sum := sha256.Sum256(body)
			digest := hex.EncodeToString(sum[:])
			f.mu.Lock()
			if f.corruptSum {
				digest = strings.Repeat("0", 64)
			}
			f.mu.Unlock()
			fmt.Fprintf(w, "%s  %s\n%s  subrouter_%s_freebsd_386\n", digest, f.u.assetName(tag), strings.Repeat("1", 64), strings.TrimPrefix(tag, "v"))
		case f.u.assetName(tag):
			_, _ = w.Write(body)
		default:
			http.NotFound(w, r)
		}
	})
	mux.HandleFunc("/_subrouter/health", func(w http.ResponseWriter, _ *http.Request) {
		f.mu.Lock()
		healthy, running := f.healthy, f.running
		f.mu.Unlock()
		if !healthy {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "version": running})
	})
	f.server = httptest.NewServer(mux)
	t.Cleanup(f.server.Close)

	home := filepath.Join(root, "home")
	f.controller = &fakeUpdateController{f: f}
	f.u = &updater{
		goos:           "linux",
		goarch:         "amd64",
		home:           home,
		executable:     func() (string, error) { return binary, nil },
		lookPath:       func(string) (string, error) { return "", os.ErrNotExist },
		environ:        func(string) string { return "" },
		client:         f.server.Client(),
		latestURL:      f.server.URL + "/releases/latest",
		apiLatestURL:   f.server.URL + "/api/latest",
		downloadBase:   func(tag string) string { return f.server.URL + "/download/" + tag },
		healthBaseURL:  f.server.URL,
		healthTimeout:  time.Second,
		pollInterval:   20 * time.Millisecond,
		teamPlistGlob:  filepath.Join(root, "LaunchDaemons", "ai.manaflow.subrouter*.plist"),
		linuxTeamUnits: []string{filepath.Join(root, "etc", "subrouter-autoupdate.timer")},
		systemUnitPath: filepath.Join(root, "etc", "subrouter.service"),
		controllerFor:  func(installKind) serviceController { return f.controller },
		runBinary:      runBinaryOutput,
		upgradeWorker: func(_ context.Context, socket string) error {
			f.mu.Lock()
			f.upgrades = append(f.upgrades, socket)
			f.mu.Unlock()
			f.restartFromDisk()
			return nil
		},
		now:         time.Now,
		out:         f.out,
		in:          strings.NewReader(""),
		programName: "sr",
	}
	return f
}

// withUserUnit installs a user systemd unit whose ExecStart runs the binary.
func (f *updateFixture) withUserUnit() *updateFixture {
	unit := userSystemdUnitPath(f.u.home, defaultSystemdServiceName)
	if err := os.MkdirAll(filepath.Dir(unit), 0o755); err != nil {
		f.t.Fatal(err)
	}
	body := "[Service]\nExecStart=" + systemdQuote(f.binary) + " serve --addr 127.0.0.1:31415\n"
	if err := os.WriteFile(unit, []byte(body), 0o600); err != nil {
		f.t.Fatal(err)
	}
	return f
}

func (f *updateFixture) installedVersion() string {
	return f.u.binaryVersion(context.Background(), f.binary)
}

func TestUpdateCheckOnlyReports(t *testing.T) {
	f := newUpdateFixture(t, "v1.0.0").withUserUnit()
	if err := f.u.runUpdateArgs(context.Background(), []string{"--check"}); err != nil {
		t.Fatalf("update --check: %v\n%s", err, f.out)
	}
	out := f.out.String()
	for _, want := range []string{"installed  v1.0.0", "running    v1.0.0", "available  v1.1.0", "update available"} {
		if !strings.Contains(out, want) {
			t.Fatalf("--check output missing %q:\n%s", want, out)
		}
	}
	if got := f.installedVersion(); got != "v1.0.0" || f.restarts != 0 {
		t.Fatalf("--check changed the install: version=%s restarts=%d", got, f.restarts)
	}
}

func TestUpdateInstallsVerifiesAndKeepsBackup(t *testing.T) {
	f := newUpdateFixture(t, "v1.0.0").withUserUnit()
	if err := f.u.runUpdateArgs(context.Background(), []string{"--yes"}); err != nil {
		t.Fatalf("update: %v\n%s", err, f.out)
	}
	if got := f.installedVersion(); got != "v1.1.0" {
		t.Fatalf("installed version = %q, want v1.1.0", got)
	}
	if f.restarts != 1 || f.running != "v1.1.0" {
		t.Fatalf("daemon restarts=%d running=%q", f.restarts, f.running)
	}
	backup := filepath.Join(filepath.Dir(f.binary), updateBackupDirName, "subrouter-v1.0.0")
	if got := f.u.binaryVersion(context.Background(), backup); got != "v1.0.0" {
		t.Fatalf("backup %s reports %q", backup, got)
	}
	if got := f.u.binaryVersion(context.Background(), filepath.Join(filepath.Dir(f.binary), "sr")); got != "v1.1.0" {
		t.Fatalf("sr alias reports %q after update", got)
	}
	leftovers, _ := filepath.Glob(filepath.Join(filepath.Dir(f.binary), ".subrouter.*"))
	if len(leftovers) != 0 {
		t.Fatalf("staging files left behind: %v", leftovers)
	}
}

func TestUpdateInstallsRequestedVersion(t *testing.T) {
	f := newUpdateFixture(t, "v1.0.0").withUserUnit()
	if err := f.u.runUpdateArgs(context.Background(), []string{"--version", "0.9.0", "--yes"}); err != nil {
		t.Fatalf("update --version: %v\n%s", err, f.out)
	}
	if got := f.installedVersion(); got != "v0.9.0" {
		t.Fatalf("installed version = %q, want v0.9.0", got)
	}
}

func TestUpdateRestoresPreviousBinaryWhenHealthFails(t *testing.T) {
	f := newUpdateFixture(t, "v1.0.0").withUserUnit()
	f.broken["v1.1.0"] = true
	err := f.u.runUpdateArgs(context.Background(), []string{"--yes"})
	if err == nil || !strings.Contains(err.Error(), "restored") {
		t.Fatalf("update of a broken release: err=%v\n%s", err, f.out)
	}
	if got := f.installedVersion(); got != "v1.0.0" {
		t.Fatalf("installed version after failed update = %q, want v1.0.0", got)
	}
	if f.restarts != 2 || !f.healthy || f.running != "v1.0.0" {
		t.Fatalf("restore did not restart the previous binary: restarts=%d healthy=%v running=%q", f.restarts, f.healthy, f.running)
	}
}

func TestUpdateRestoresWhenDaemonReportsOldVersion(t *testing.T) {
	f := newUpdateFixture(t, "v1.0.0").withUserUnit()
	// A restart that never picks up the new binary: health stays on v1.0.0.
	f.u.controllerFor = func(installKind) serviceController { return &fakeController{present: true} }
	err := f.u.runUpdateArgs(context.Background(), []string{"--yes"})
	if err == nil || !strings.Contains(err.Error(), "did not report v1.1.0") {
		t.Fatalf("update with a stale daemon: err=%v\n%s", err, f.out)
	}
	if got := f.installedVersion(); got != "v1.0.0" {
		t.Fatalf("installed version = %q, want the restored v1.0.0", got)
	}
}

func TestUpdateRejectsChecksumMismatch(t *testing.T) {
	f := newUpdateFixture(t, "v1.0.0").withUserUnit()
	f.corruptSum = true
	err := f.u.runUpdateArgs(context.Background(), []string{"--yes"})
	if err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("update with a bad checksum: err=%v", err)
	}
	if got := f.installedVersion(); got != "v1.0.0" || f.restarts != 0 {
		t.Fatalf("bad checksum changed the install: version=%s restarts=%d", got, f.restarts)
	}
}

func TestUpdateRequiresYesWithoutTerminal(t *testing.T) {
	f := newUpdateFixture(t, "v1.0.0").withUserUnit()
	err := f.u.runUpdateArgs(context.Background(), nil)
	if err == nil || !strings.Contains(err.Error(), "--yes") {
		t.Fatalf("non-interactive update without --yes: err=%v", err)
	}
	if got := f.installedVersion(); got != "v1.0.0" {
		t.Fatalf("unconfirmed update changed the binary to %s", got)
	}
}

func TestUpdateAlreadyCurrentDoesNothing(t *testing.T) {
	f := newUpdateFixture(t, "v1.1.0").withUserUnit()
	if err := f.u.runUpdateArgs(context.Background(), []string{"--yes"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(f.out.String(), "already at v1.1.0") || f.restarts != 0 {
		t.Fatalf("current install was touched: restarts=%d\n%s", f.restarts, f.out)
	}
}

func TestUpdatePlainBinaryWithoutService(t *testing.T) {
	f := newUpdateFixture(t, "v1.0.0")
	if err := f.u.runUpdateArgs(context.Background(), []string{"--yes"}); err != nil {
		t.Fatalf("update: %v\n%s", err, f.out)
	}
	if got := f.installedVersion(); got != "v1.1.0" || f.restarts != 0 {
		t.Fatalf("plain binary update: version=%s restarts=%d", got, f.restarts)
	}
	if !strings.Contains(f.out.String(), string(installKindBinary)) {
		t.Fatalf("output does not name the install kind:\n%s", f.out)
	}
}

func TestUpdateSupervisedLaunchAgentUsesControlSocket(t *testing.T) {
	f := newUpdateFixture(t, "v1.0.0")
	f.u.goos = "darwin"
	f.u.goarch = "arm64"
	plist := launchAgentPath(f.u.home, defaultDaemonLabel)
	if err := os.MkdirAll(filepath.Dir(plist), 0o755); err != nil {
		t.Fatal(err)
	}
	body := `<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0"><dict>
<key>Label</key><string>ai.manaflow.subrouter</string>
<key>ProgramArguments</key><array>
<string>/Users/me/bin/subrouter-supervisor</string><string>supervise</string>
<string>--control-socket</string><string>/Users/me/.subrouter/supervisor.sock</string>
<string>--worker-bin</string><string>` + f.binary + `</string><string>--</string>
</array></dict></plist>`
	if err := os.WriteFile(plist, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := f.u.runUpdateArgs(context.Background(), []string{"--yes"}); err != nil {
		t.Fatalf("update: %v\n%s", err, f.out)
	}
	if len(f.upgrades) != 1 || f.upgrades[0] != "/Users/me/.subrouter/supervisor.sock" {
		t.Fatalf("supervisor upgrade calls = %v", f.upgrades)
	}
	if got := f.installedVersion(); got != "v1.1.0" {
		t.Fatalf("worker version = %q", got)
	}
}

func TestUpdateDefersWhileAnotherUpdateHoldsTheLease(t *testing.T) {
	f := newUpdateFixture(t, "v1.0.0").withUserUnit()
	dir := backupDir(f.binary)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	release, err := tryLockUpdateLease(filepath.Join(dir, ".update.lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	err = f.u.runUpdateArgs(context.Background(), []string{"--yes"})
	if err == nil || !strings.Contains(err.Error(), "another update or deployment") {
		t.Fatalf("concurrent update: err=%v", err)
	}
	if got := f.installedVersion(); got != "v1.0.0" {
		t.Fatalf("concurrent update changed the binary to %s", got)
	}
}

func TestUpdateRefusesTeamLaunchDaemon(t *testing.T) {
	f := newUpdateFixture(t, "v1.0.0")
	f.u.goos = "darwin"
	dir := filepath.Dir(f.u.teamPlistGlob)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := `<plist version="1.0"><dict><key>ProgramArguments</key><array>
<string>/usr/local/libexec/subrouter-supervisor</string><string>supervise</string>
<string>--worker-bin</string><string>` + f.binary + `</string></array></dict></plist>`
	if err := os.WriteFile(filepath.Join(dir, "ai.manaflow.subrouter-team.plist"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"--yes"}, {"--check"}} {
		err := f.u.runUpdateArgs(context.Background(), args)
		if err == nil || !strings.Contains(err.Error(), "subrouter-deploy.sh install-release") {
			t.Fatalf("update %v on a team host: err=%v", args, err)
		}
	}
	if err := f.u.runRollbackArgs(context.Background(), []string{"--yes"}); err == nil || !strings.Contains(err.Error(), "team LaunchDaemon") {
		t.Fatalf("rollback on a team host: err=%v", err)
	}
	if got := f.installedVersion(); got != "v1.0.0" {
		t.Fatalf("team binary was changed to %s", got)
	}
}

func TestUpdateRefusesLinuxTeamHost(t *testing.T) {
	f := newUpdateFixture(t, "v1.0.0")
	unit := f.u.linuxTeamUnits[0]
	if err := os.MkdirAll(filepath.Dir(unit), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(unit, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	err := f.u.runUpdateArgs(context.Background(), []string{"--yes"})
	if err == nil || !strings.Contains(err.Error(), "team deployment") {
		t.Fatalf("update on a GCP team host: err=%v", err)
	}
}

func TestUpdateRefusesPackageManagerWrappers(t *testing.T) {
	for _, tc := range []struct {
		shim string
		want string
	}{
		{"#!/usr/bin/env node\nrequire(\"./runner\").run(\"sr\");\n", "npm install -g subrouter@latest"},
		{"#!/usr/bin/python3\nfrom subrouter_cli import sr\n", "pip install --upgrade subrouter"},
	} {
		f := newUpdateFixture(t, "v1.0.0")
		cached := filepath.Join(f.root, "cache", "1.0.0", "subrouter_1.0.0_linux_amd64")
		if err := os.MkdirAll(filepath.Dir(cached), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(cached, fakeBinaryScript("v1.0.0"), 0o755); err != nil {
			t.Fatal(err)
		}
		shim := filepath.Join(f.root, "shim-sr")
		if err := os.WriteFile(shim, []byte(tc.shim), 0o755); err != nil {
			t.Fatal(err)
		}
		f.u.executable = func() (string, error) { return cached, nil }
		f.u.lookPath = func(string) (string, error) { return shim, nil }
		err := f.u.runUpdateArgs(context.Background(), []string{"--yes"})
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("wrapper install: err=%v, want %q", err, tc.want)
		}
		if got := f.u.binaryVersion(context.Background(), cached); got != "v1.0.0" {
			t.Fatalf("wrapper cache was modified: %s", got)
		}
	}
}

func TestRollbackRestoresNewestBackupAndRollsForward(t *testing.T) {
	f := newUpdateFixture(t, "v1.0.0").withUserUnit()
	if err := f.u.runUpdateArgs(context.Background(), []string{"--yes"}); err != nil {
		t.Fatalf("update: %v\n%s", err, f.out)
	}
	if err := f.u.runRollbackArgs(context.Background(), []string{"--yes"}); err != nil {
		t.Fatalf("rollback: %v\n%s", err, f.out)
	}
	if got := f.installedVersion(); got != "v1.0.0" || f.running != "v1.0.0" {
		t.Fatalf("after rollback installed=%s running=%s", got, f.running)
	}
	// The binary rolled back from is kept, so rollback --to can go forward.
	if err := f.u.runRollbackArgs(context.Background(), []string{"--to", "v1.1.0", "--yes"}); err != nil {
		t.Fatalf("rollback --to v1.1.0: %v\n%s", err, f.out)
	}
	if got := f.installedVersion(); got != "v1.1.0" {
		t.Fatalf("after rollback --to installed=%s", got)
	}
	f.out.Reset()
	if err := f.u.runRollbackArgs(context.Background(), []string{"--list"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(f.out.String(), "v1.0.0") || !strings.Contains(f.out.String(), "v1.1.0") {
		t.Fatalf("--list output:\n%s", f.out)
	}
}

func TestRollbackRestoresWhenBackupFailsHealth(t *testing.T) {
	f := newUpdateFixture(t, "v1.0.0").withUserUnit()
	if err := f.u.runUpdateArgs(context.Background(), []string{"--yes"}); err != nil {
		t.Fatalf("update: %v\n%s", err, f.out)
	}
	f.broken["v1.0.0"] = true
	if err := f.u.runRollbackArgs(context.Background(), []string{"--yes"}); err == nil {
		t.Fatal("rollback to an unhealthy backup succeeded")
	}
	if got := f.installedVersion(); got != "v1.1.0" || !f.healthy {
		t.Fatalf("failed rollback left installed=%s healthy=%v", got, f.healthy)
	}
}

func TestRollbackWithoutBackupsExplains(t *testing.T) {
	f := newUpdateFixture(t, "v1.0.0").withUserUnit()
	err := f.u.runRollbackArgs(context.Background(), []string{"--yes"})
	if err == nil || !strings.Contains(err.Error(), "no backup") {
		t.Fatalf("rollback with nothing kept: err=%v", err)
	}
}

func TestUpdateKeepsOnlyThreeBackups(t *testing.T) {
	f := newUpdateFixture(t, "v1.0.0").withUserUnit()
	for i, tag := range []string{"v1.1.0", "v1.2.0", "v1.3.0", "v1.4.0"} {
		f.latest = tag
		// Distinct modification times order the backups.
		f.u.now = func() time.Time { return time.Now().Add(time.Duration(i) * time.Minute) }
		if err := f.u.runUpdateArgs(context.Background(), []string{"--yes"}); err != nil {
			t.Fatalf("update to %s: %v\n%s", tag, err, f.out)
		}
	}
	backups := f.u.listBackups(f.binary)
	if len(backups) != updateKeepBackups {
		t.Fatalf("kept %d backups, want %d: %+v", len(backups), updateKeepBackups, backups)
	}
	if backups[0].version != "v1.3.0" || backups[2].version != "v1.1.0" {
		t.Fatalf("kept the wrong backups: %+v", backups)
	}
}

func TestSystemdExecStartBinary(t *testing.T) {
	for unit, want := range map[string]string{
		"[Service]\nExecStart=/usr/local/bin/subrouter serve --addr x\n":     `/usr/local/bin/subrouter`,
		"ExecStart=" + systemdQuote(`/home/a b/100%/subrouter`) + " serve\n": `/home/a b/100%/subrouter`,
		"ExecStartPre=/bin/true\nExecStart=-/opt/subrouter serve\n":          `/opt/subrouter`,
		"[Unit]\nDescription=none\n":                                         ``,
	} {
		if got := systemdExecStartBinary(unit); got != want {
			t.Fatalf("systemdExecStartBinary(%q) = %q, want %q", unit, got, want)
		}
	}
}

func TestResolveTagFallsBackToReleaseAPI(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/latest" {
			_, _ = w.Write([]byte(`{"tag_name":"v2.3.4"}`))
			return
		}
		http.Error(w, "rate limited", http.StatusForbidden)
	}))
	defer server.Close()
	u := &updater{client: server.Client(), latestURL: server.URL + "/releases/latest", apiLatestURL: server.URL + "/api/latest"}
	tag, err := u.resolveTag(context.Background(), "")
	if err != nil || tag != "v2.3.4" {
		t.Fatalf("resolveTag = %q, %v", tag, err)
	}
	if _, err := u.resolveTag(context.Background(), "latest; rm -rf /"); err == nil {
		t.Fatal("resolveTag accepted a malformed version")
	}
}

func TestPlistProgramArgumentsIgnoresNestedArrays(t *testing.T) {
	body := `<plist><dict>
<key>EnvironmentVariables</key><dict><key>ProgramArguments</key><string>nope</string></dict>
<key>ProgramArguments</key><array><string>/bin/subrouter</string><string>serve</string></array>
</dict></plist>`
	got, err := plistProgramArguments([]byte(body))
	if err != nil || len(got) != 2 || got[0] != "/bin/subrouter" || got[1] != "serve" {
		t.Fatalf("plistProgramArguments = %q, %v", got, err)
	}
}
