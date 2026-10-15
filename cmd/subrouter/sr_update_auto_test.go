//go:build !windows

package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func (f *updateFixture) autoLog() string {
	body, _ := os.ReadFile(f.u.autoLogPath)
	return string(body)
}

func (f *updateFixture) nextAutoCheck() time.Time {
	stamp, ok := readAutoUpdateStamp(f.u.autoStampPath)
	if !ok {
		f.t.Fatalf("no auto-update stamp at %s", f.u.autoStampPath)
	}
	return stamp.NextCheck
}

func within(t *testing.T, got, want time.Time, slack time.Duration) {
	t.Helper()
	if got.Before(want.Add(-slack)) || got.After(want.Add(slack)) {
		t.Fatalf("next check %s, want about %s", got.Format(time.RFC3339), want.Format(time.RFC3339))
	}
}

// A due auto update installs the release like `sr update --yes`, restarts
// the idle daemon, writes nothing to the terminal, logs what it did, and
// schedules the next check a day out.
func TestAutoUpdateInstallsWhenDueAndIdle(t *testing.T) {
	f := newUpdateFixture(t, "v1.0.0").withUserUnit()
	if err := f.u.runUpdateArgs(context.Background(), []string{"--auto"}); err != nil {
		t.Fatalf("update --auto: %v\n%s", err, f.autoLog())
	}
	if got := f.installedVersion(); got != "v1.1.0" || f.restarts != 1 || f.running != "v1.1.0" {
		t.Fatalf("version=%s restarts=%d running=%s, want v1.1.0 installed and serving", got, f.restarts, f.running)
	}
	if f.out.Len() != 0 {
		t.Fatalf("auto update wrote to the terminal:\n%s", f.out)
	}
	if log := f.autoLog(); !strings.Contains(log, "updated to v1.1.0") {
		t.Fatalf("auto-update log does not record the update:\n%s", log)
	}
	within(t, f.nextAutoCheck(), time.Now().Add(autoUpdateInterval), time.Minute)
}

// Before the stamp is due nothing is fetched or touched.
func TestAutoUpdateWaitsForItsStamp(t *testing.T) {
	f := newUpdateFixture(t, "v1.0.0").withUserUnit()
	next := time.Now().Add(3 * time.Hour).UTC().Truncate(time.Second)
	if err := writeAutoUpdateStamp(f.u.autoStampPath, autoUpdateStamp{NextCheck: next}); err != nil {
		t.Fatal(err)
	}
	if err := f.u.runUpdateArgs(context.Background(), []string{"--auto"}); err != nil {
		t.Fatal(err)
	}
	if got := f.installedVersion(); got != "v1.0.0" || f.restarts != 0 {
		t.Fatalf("version=%s restarts=%d, want an untouched install before the stamp is due", got, f.restarts)
	}
	if got := f.nextAutoCheck(); !got.Equal(next) {
		t.Fatalf("stamp moved to %s", got)
	}
}

// A daemon that is serving requests is never restarted under them: the
// update defers before replacing anything and retries within the hour.
func TestAutoUpdateDefersWhileTheDaemonIsServing(t *testing.T) {
	f := newUpdateFixture(t, "v1.0.0").withUserUnit()
	f.active = 2
	if err := f.u.runUpdateArgs(context.Background(), []string{"--auto"}); err != nil {
		t.Fatal(err)
	}
	if got := f.installedVersion(); got != "v1.0.0" || f.restarts != 0 {
		t.Fatalf("version=%s restarts=%d, want nothing replaced while requests are in flight", got, f.restarts)
	}
	if log := f.autoLog(); !strings.Contains(log, "in flight") {
		t.Fatalf("auto-update log does not say why it deferred:\n%s", log)
	}
	within(t, f.nextAutoCheck(), time.Now().Add(autoUpdateBusyRetry), time.Minute)
}

// A supervised install swaps workers behind the open listener, so in-flight
// requests do not hold it back.
func TestAutoUpdateSupervisedInstallDoesNotWaitForIdle(t *testing.T) {
	f := newUpdateFixture(t, "v1.0.0")
	f.u.goos = "darwin"
	f.u.goarch = "arm64"
	f.active = 3
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
	if err := f.u.runUpdateArgs(context.Background(), []string{"--auto"}); err != nil {
		t.Fatalf("update --auto: %v\n%s", err, f.autoLog())
	}
	if got := f.installedVersion(); got != "v1.1.0" || len(f.upgrades) != 1 {
		t.Fatalf("version=%s upgrades=%v, want a hot swap through the supervisor", got, f.upgrades)
	}
}

// What a manual update refuses (a team host here) is a quiet no-op for the
// automatic one: no error, no change, and no retry until the next day.
func TestAutoUpdateRefusalIsAQuietNoOp(t *testing.T) {
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
	if err := f.u.runUpdateArgs(context.Background(), []string{"--auto"}); err != nil {
		t.Fatalf("auto update on a team host returned %v, want a quiet no-op", err)
	}
	if got := f.installedVersion(); got != "v1.0.0" || f.out.Len() != 0 {
		t.Fatalf("version=%s terminal=%q, want nothing changed or printed", got, f.out)
	}
	within(t, f.nextAutoCheck(), time.Now().Add(autoUpdateInterval), time.Minute)
}

// A build newer than the latest release (one from main) is left alone.
func TestAutoUpdateLeavesANewerBuild(t *testing.T) {
	f := newUpdateFixture(t, "v1.2.0").withUserUnit()
	if err := f.u.runUpdateArgs(context.Background(), []string{"--auto"}); err != nil {
		t.Fatal(err)
	}
	if got := f.installedVersion(); got != "v1.2.0" || f.restarts != 0 {
		t.Fatalf("version=%s restarts=%d, want the newer build kept", got, f.restarts)
	}
}

// A failed update (the new daemon never becomes healthy) is restored by the
// normal path, and the automatic one backs off for hours instead of retrying
// a bad release on every launch.
func TestAutoUpdateFailureRestoresAndBacksOff(t *testing.T) {
	f := newUpdateFixture(t, "v1.0.0").withUserUnit()
	f.broken["v1.1.0"] = true
	_ = f.u.runUpdateArgs(context.Background(), []string{"--auto"})
	if got := f.installedVersion(); got != "v1.0.0" || f.running != "v1.0.0" {
		t.Fatalf("version=%s running=%s, want the previous binary restored", got, f.running)
	}
	within(t, f.nextAutoCheck(), time.Now().Add(autoUpdateErrorRetry), time.Minute)
}

func TestAutoUpdateTriggerOnlyForInteractiveCommandsWhenDue(t *testing.T) {
	dir := t.TempDir()
	stamp := filepath.Join(dir, "auto-update.json")
	now := time.Now()
	env := func(map[string]string) func(string) string {
		return func(string) string { return "" }
	}
	cases := []struct {
		name    string
		program string
		args    []string
		env     map[string]string
		next    time.Time
		want    bool
	}{
		{name: "launch with no stamp", program: "sr", args: []string{"claude"}, want: true},
		{name: "status when due", program: "sr", args: []string{"status"}, next: now.Add(-time.Minute), want: true},
		{name: "not yet due", program: "sr", args: []string{"claude"}, next: now.Add(time.Hour), want: false},
		{name: "update itself", program: "sr", args: []string{"update", "--auto"}, want: false},
		{name: "rollback", program: "sr", args: []string{"rollback"}, want: false},
		{name: "daemon", program: "sr", args: []string{"serve"}, want: false},
		{name: "supervisor", program: "subrouter", args: []string{"supervise"}, want: false},
		{name: "version", program: "sr", args: []string{"version"}, want: false},
		{name: "no command", program: "sr", args: nil, want: false},
		{name: "opted out", program: "sr", args: []string{"claude"}, env: map[string]string{"SUBROUTER_NO_AUTO_UPDATE": "1"}, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_ = os.Remove(stamp)
			if !tc.next.IsZero() {
				if err := writeAutoUpdateStamp(stamp, autoUpdateStamp{NextCheck: tc.next}); err != nil {
					t.Fatal(err)
				}
			}
			lookup := env(tc.env)
			if tc.env != nil {
				lookup = func(key string) string { return tc.env[key] }
			}
			if got := autoUpdateTriggerDue(tc.program, tc.args, lookup, stamp, now); got != tc.want {
				t.Fatalf("autoUpdateTriggerDue = %v, want %v", got, tc.want)
			}
		})
	}
}

// A request that starts while the release downloads still holds the restart
// back: the daemon is probed again right before the binary is replaced.
func TestAutoUpdateDefersWhenARequestStartsDuringTheDownload(t *testing.T) {
	f := newUpdateFixture(t, "v1.0.0").withUserUnit()
	f.busyAfterProbes = f.u.idleSamples
	if err := f.u.runUpdateArgs(context.Background(), []string{"--auto"}); err != nil {
		t.Fatal(err)
	}
	if got := f.installedVersion(); got != "v1.0.0" || f.restarts != 0 {
		t.Fatalf("version=%s restarts=%d, want nothing replaced once a request is in flight", got, f.restarts)
	}
	within(t, f.nextAutoCheck(), time.Now().Add(autoUpdateBusyRetry), time.Minute)
}

// Only one automatic update runs at a time; another one started meanwhile
// leaves everything, including the stamp, to the first.
func TestAutoUpdateLeavesARunningOneAlone(t *testing.T) {
	f := newUpdateFixture(t, "v1.0.0").withUserUnit()
	if err := os.MkdirAll(filepath.Dir(f.u.autoStampPath), 0o755); err != nil {
		t.Fatal(err)
	}
	release, err := tryLockUpdateLease(f.u.autoStampPath + ".lock")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if err := f.u.runUpdateArgs(context.Background(), []string{"--auto"}); err != nil {
		t.Fatal(err)
	}
	if got := f.installedVersion(); got != "v1.0.0" || f.restarts != 0 {
		t.Fatalf("version=%s restarts=%d, want the second automatic update to do nothing", got, f.restarts)
	}
	if _, ok := readAutoUpdateStamp(f.u.autoStampPath); ok {
		t.Fatal("the second automatic update wrote the stamp")
	}
}
