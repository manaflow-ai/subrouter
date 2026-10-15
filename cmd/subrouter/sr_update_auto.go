package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// Automatic client updates.
//
// Teammates kept running whatever sr they first installed: nothing updated
// a laptop client except a macOS LaunchAgent piping install.sh from main.
// `sr update --auto` is the same verified update as `sr update --yes`
// (release resolved from GitHub, SHA256SUMS checked, staged binary's version
// checked, backup kept, atomic rename, health verified, automatic restore),
// made safe to run unattended:
//
//   - it runs at most once per autoUpdateInterval, recorded in a stamp file,
//     so a launcher can trigger it on every start without network cost;
//   - what a manual update refuses (a team host, a package manager's
//     wrapper, a build newer than the latest release) is a quiet no-op;
//   - it never restarts a daemon under a request: an unsupervised daemon is
//     replaced only when its drain-status reports nothing in flight across
//     idleSamples probes, and otherwise the update is deferred, untouched, for
//     autoUpdateBusyRetry. A supervised LaunchAgent swaps workers behind its
//     open listener, which is safe at any time;
//   - output goes to a log file, never the terminal it was started from.
//
// Every interactive sr command starts it, detached, when the stamp is due
// (maybeStartAutoUpdate). SUBROUTER_NO_AUTO_UPDATE=1 turns that off.
const (
	autoUpdateInterval     = 24 * time.Hour
	autoUpdateBusyRetry    = time.Hour
	autoUpdateErrorRetry   = 6 * time.Hour
	autoUpdateLogMaxBytes  = 1 << 20
	autoUpdateIdleSamples  = 3
	autoUpdateIdleInterval = time.Second
	autoUpdateDisableEnv   = "SUBROUTER_NO_AUTO_UPDATE"
)

// errAutoUpdateBusy defers an automatic update while the daemon it would
// restart is serving requests.
var errAutoUpdateBusy = errors.New("the daemon has requests in flight; deferring the update")

// autoUpdateRefusal is a manual update's refusal, which the automatic one
// treats as nothing to do.
type autoUpdateRefusal struct{ err error }

func (r autoUpdateRefusal) Error() string { return r.err.Error() }
func (r autoUpdateRefusal) Unwrap() error { return r.err }

type autoUpdateStamp struct {
	NextCheck  time.Time `json:"next_check"`
	LastResult string    `json:"last_result,omitempty"`
}

func readAutoUpdateStamp(path string) (autoUpdateStamp, bool) {
	var stamp autoUpdateStamp
	body, err := os.ReadFile(path)
	if err != nil || json.Unmarshal(body, &stamp) != nil || stamp.NextCheck.IsZero() {
		return autoUpdateStamp{}, false
	}
	return stamp, true
}

func writeAutoUpdateStamp(path string, stamp autoUpdateStamp) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	stamp.NextCheck = stamp.NextCheck.UTC()
	body, err := json.Marshal(stamp)
	if err != nil {
		return err
	}
	return writeFileAtomic(path, append(body, '\n'), 0o644)
}

// openAutoUpdateLog appends to the log, starting it over once it passes
// autoUpdateLogMaxBytes.
func openAutoUpdateLog(path string) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	flags := os.O_CREATE | os.O_WRONLY | os.O_APPEND
	if info, err := os.Stat(path); err == nil && info.Size() > autoUpdateLogMaxBytes {
		flags |= os.O_TRUNC
	}
	return os.OpenFile(path, flags, 0o644)
}

// autoUpdate runs `sr update --auto`.
func (u *updater) autoUpdate(ctx context.Context) error {
	if u.autoStampPath == "" || u.autoLogPath == "" {
		return nil
	}
	now := u.now()
	// One automatic update at a time, from the due check through the final
	// stamp: a second one waits for the next trigger instead.
	if err := os.MkdirAll(filepath.Dir(u.autoStampPath), 0o755); err != nil {
		return nil
	}
	release, err := tryLockUpdateLease(u.autoStampPath + ".lock")
	if err != nil {
		return nil
	}
	defer release()
	if stamp, ok := readAutoUpdateStamp(u.autoStampPath); ok && now.Before(stamp.NextCheck) {
		return nil
	}
	// Claim the slot first: launchers that start while this runs see a stamp
	// that is not due and do not start a second one.
	if err := writeAutoUpdateStamp(u.autoStampPath, autoUpdateStamp{NextCheck: now.Add(autoUpdateBusyRetry), LastResult: "running"}); err != nil {
		return nil
	}
	logFile, err := openAutoUpdateLog(u.autoLogPath)
	if err != nil {
		return nil
	}
	defer logFile.Close()
	u.out = logFile
	fmt.Fprintf(logFile, "%s %s update --auto\n", now.UTC().Format(time.RFC3339), u.programName)

	err = u.update(ctx, updateOptions{yes: true, auto: true})
	next, result := now.Add(autoUpdateInterval), "ok"
	var refusal autoUpdateRefusal
	switch {
	case err == nil:
	case errors.As(err, &refusal):
		result = "skipped"
		fmt.Fprintf(logFile, "skipped: %v\n", refusal.err)
	case errors.Is(err, errAutoUpdateBusy):
		next, result = now.Add(autoUpdateBusyRetry), "deferred"
		fmt.Fprintf(logFile, "deferred: %v\n", err)
	default:
		next, result = now.Add(autoUpdateErrorRetry), "failed"
		fmt.Fprintf(logFile, "failed: %v\n", err)
	}
	_ = writeAutoUpdateStamp(u.autoStampPath, autoUpdateStamp{NextCheck: next, LastResult: result})
	// The caller is a detached background process; the log and the stamp
	// carry the outcome.
	return nil
}

// autoRestartBusy reports whether an automatic update must not restart the
// daemon now. It probes right before each step that would restart it, which
// narrows the window a new request can open to the restart itself; draining
// first would close it, but would turn every request in that moment away.
func (u *updater) autoRestartBusy(ctx context.Context, opts updateOptions, target installTarget) bool {
	return opts.auto && target.hasDaemon() && target.controlSocket == "" && !u.daemonIdle(ctx)
}

// daemonIdle reports whether the local daemon has no proxy requests in
// flight across every probe. An unreadable answer counts as busy: an update
// is never worth a cut request.
func (u *updater) daemonIdle(ctx context.Context) bool {
	statusURL, err := url.JoinPath(u.healthBaseURL, "/_subrouter/drain-status")
	if err != nil {
		return false
	}
	samples := u.idleSamples
	if samples <= 0 {
		samples = autoUpdateIdleSamples
	}
	for i := 0; i < samples; i++ {
		if i > 0 {
			select {
			case <-ctx.Done():
				return false
			case <-time.After(u.idleInterval):
			}
		}
		active, ok := u.activeProxyRequests(ctx, statusURL)
		if !ok || active > 0 {
			return false
		}
	}
	return true
}

func (u *updater) activeProxyRequests(ctx context.Context, statusURL string) (int64, bool) {
	probeCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(probeCtx, http.MethodGet, statusURL, nil)
	if err != nil {
		return 0, false
	}
	response, err := fallbackHTTPClient().Do(request)
	if err != nil {
		return 0, false
	}
	defer response.Body.Close()
	var status struct {
		Active *int64 `json:"active_proxy_requests"`
	}
	if response.StatusCode != http.StatusOK ||
		json.NewDecoder(io.LimitReader(response.Body, 64<<10)).Decode(&status) != nil || status.Active == nil {
		return 0, false
	}
	return *status.Active, true
}

// autoUpdateSkipsCommand lists the commands that never start an automatic
// update: the update commands themselves, the daemon and its supervisor,
// installers, and one-shot introspection.
var autoUpdateSkipsCommand = map[string]bool{
	"update": true, "rollback": true,
	"serve": true, "supervise": true, "front": true, "listener-transfer": true,
	"naked": true, "direct": true, "probe": true,
	"install-daemon": true, "install-systemd": true, "install-launchd": true,
	"help": true, "-h": true, "--help": true,
}

// autoUpdateTriggerDue reports whether this invocation should start a
// background `update --auto`.
func autoUpdateTriggerDue(program string, args []string, getenv func(string) string, stampPath string, now time.Time) bool {
	if getenv(autoUpdateDisableEnv) != "" || stampPath == "" || len(args) == 0 {
		return false
	}
	if program != "sr" && program != "subrouter" {
		return false
	}
	if autoUpdateSkipsCommand[args[0]] || isVersionCommand(args[0]) {
		return false
	}
	stamp, ok := readAutoUpdateStamp(stampPath)
	return !ok || !now.Before(stamp.NextCheck)
}

// maybeStartAutoUpdate starts `update --auto` detached from this process
// when it is due. It never blocks or fails the command that triggered it.
func maybeStartAutoUpdate(program string, args []string) {
	if testing.Testing() || runtime.GOOS == "windows" {
		return
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return
	}
	if !autoUpdateTriggerDue(program, args, os.Getenv, autoUpdateStampPath(home), time.Now()) {
		return
	}
	exe, err := os.Executable()
	if err != nil {
		return
	}
	cmd := exec.Command(exe, "update", "--auto")
	cmd.SysProcAttr = detachedProcessAttributes()
	if cmd.Start() == nil {
		_ = cmd.Process.Release()
	}
}

func autoUpdateStampPath(home string) string {
	return filepath.Join(home, ".subrouter", "auto-update.json")
}

func autoUpdateLogPath(home string) string {
	return filepath.Join(home, ".subrouter", "logs", "auto-update.log")
}
