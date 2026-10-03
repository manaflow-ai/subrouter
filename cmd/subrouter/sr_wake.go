package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/manaflow-ai/subrouter/internal/storepath"
	"github.com/manaflow-ai/subrouter/wake"
)

func (r srRunner) wake(args []string) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		fmt.Fprintln(r.out, "usage: sr auto-resume status|list|show <id>|schedule [options]|now [codex|claude|all]|cancel <id>|cancel --agent <agent>|cancel --all|enable|disable <agent>|early <agent> <enable|disable>")
		return nil
	}
	store := wake.NewStore(storepath.StateDir() + "/wake.json")
	now := time.Now().UTC()
	switch args[0] {
	case "status":
		return autoResumeStatus(store, r.out)
	case "list":
		alarms, err := store.List(now)
		if err != nil {
			return err
		}
		if len(alarms) == 0 {
			fmt.Fprintln(r.out, "no wake alarms")
			return nil
		}
		for _, a := range alarms {
			fmt.Fprintf(r.out, "%s\t%s\t%s\t%s\t%s\t%s\n", a.ID, a.Status, a.Agent, a.Action, a.WakeAt.Local().Format(time.RFC3339), a.SurfaceID)
		}
		return nil
	case "show":
		if len(args) != 2 {
			return fmt.Errorf("usage: sr wake show <id>")
		}
		a, ok, err := store.Get(args[1], now)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("wake alarm %q not found", args[1])
		}
		fmt.Fprintf(r.out, "id=%s status=%s agent=%s action=%s wake_at=%s expires_at=%s session=%s surface=%s machine=%s attempt=%d jitter=%ds accelerated_at=%s\n", a.ID, a.Status, a.Agent, a.Action, a.WakeAt.Format(time.RFC3339), a.ExpiresAt.Format(time.RFC3339), a.SessionID, a.SurfaceID, a.Machine, a.Attempt, a.JitterSeconds, a.AcceleratedAt.Format(time.RFC3339))
		return nil
	case "schedule":
		return scheduleWake(store, args[1:], now, r.out)
	case "update":
		return updateWake(store, args[1:], now, r.out)
	case "worker":
		return runWakeWorker(args[1:], store, r.out)
	case "install":
		serverURL, err := r.wakeServerURL()
		if err != nil {
			return err
		}
		return installWakeLaunchd(r.out, serverURL)
	case "uninstall":
		return uninstallWakeLaunchd(r.out)
	case "now":
		return wakeNow(store, args[1:], now, r.out)
	case "cancel":
		return cancelWake(store, args[1:], now, r.out)
	case "enable", "disable":
		if len(args) != 2 || (args[1] != "codex" && args[1] != "claude") {
			return fmt.Errorf("usage: sr wake %s <codex|claude>", args[0])
		}
		// Configuration wiring is intentionally separate from the durable alarm
		// queue; this command currently records the requested policy in the same
		// state root for the shared watcher to consume.
		cfg := wake.NewConfig(storepath.StateDir() + "/wake-config.json")
		if err := cfg.SetEnabled(args[1], args[0] == "enable"); err != nil {
			return err
		}
		if args[0] == "enable" {
			serverURL, err := r.wakeServerURL()
			if err != nil {
				return err
			}
			if err := ensureWakeLaunchd(r.out, serverURL); err != nil {
				return err
			}
		} else {
			claudeEnabled, err := cfg.Enabled("claude")
			if err != nil {
				return err
			}
			codexEnabled, err := cfg.Enabled("codex")
			if err != nil {
				return err
			}
			if !claudeEnabled && !codexEnabled {
				if err := uninstallWakeLaunchd(r.out); err != nil {
					return err
				}
			}
		}
		fmt.Fprintf(r.out, "%s auto-resume %s\n", args[1], map[bool]string{true: "enabled", false: "disabled"}[args[0] == "enable"])
		return nil
	case "policy":
		return updateWakePolicy(args[1:], r.out)
	case "early":
		if len(args) < 2 || len(args) > 3 || (args[1] != "codex" && args[1] != "claude") {
			return fmt.Errorf("usage: sr wake early <codex|claude> [enable|disable]")
		}
		cfg := wake.NewConfig(storepath.StateDir() + "/wake-config.json")
		if len(args) == 3 {
			if args[2] != "enable" && args[2] != "disable" {
				return fmt.Errorf("usage: sr wake early <codex|claude> [enable|disable]")
			}
			if err := cfg.SetEarlyOnRecovery(args[1], args[2] == "enable"); err != nil {
				return err
			}
		}
		early, err := cfg.EarlyOnRecovery(args[1])
		if err != nil {
			return err
		}
		fmt.Fprintf(r.out, "early auto-resume for %s: %s\n", args[1], map[bool]string{true: "enabled", false: "disabled"}[early])
		return nil
	default:
		return fmt.Errorf("unknown wake command %q", args[0])
	}
}

func autoResumeStatus(store *wake.Store, out interface{ Write([]byte) (int, error) }) error {
	cfg := wake.NewConfig(storepath.StateDir() + "/wake-config.json")
	for _, agent := range []string{"claude", "codex"} {
		enabled, err := cfg.Enabled(agent)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "%s auto-resume: %s\n", agent, map[bool]string{true: "enabled", false: "disabled"}[enabled])
	}
	alarms, err := store.List(time.Now().UTC())
	if err != nil {
		return err
	}
	scheduled := 0
	for _, alarm := range alarms {
		if alarm.Status == wake.StatusScheduled || alarm.Status == wake.StatusFired {
			scheduled++
		}
	}
	uid := strconv.Itoa(os.Getuid())
	worker := exec.Command("launchctl", "print", "gui/"+uid+"/"+wakeLaunchdLabel).Run() == nil
	fmt.Fprintf(out, "worker: %s\n", map[bool]string{true: "running", false: "not running"}[worker])
	fmt.Fprintf(out, "alarms: %d scheduled or firing\n", scheduled)
	return nil
}

func printLocalWakeSummary(out interface{ Write([]byte) (int, error) }, serverURL string) {
	target, err := url.Parse(serverURL)
	if err != nil || (target.Hostname() != "127.0.0.1" && target.Hostname() != "localhost" && target.Hostname() != "::1") {
		return
	}
	alarms, err := wake.NewStore(storepath.StateDir() + "/wake.json").List(time.Now().UTC())
	if err != nil {
		return
	}
	scheduled := 0
	for _, alarm := range alarms {
		if alarm.Status == wake.StatusScheduled || alarm.Status == wake.StatusFired {
			scheduled++
		}
	}
	if scheduled == 0 {
		fmt.Fprintln(out, "Wake alarms: none scheduled")
	} else {
		fmt.Fprintf(out, "Wake alarms: %d scheduled or firing (use 'sr auto-resume list' for details)\n", scheduled)
	}
}

func updateWakePolicy(args []string, out interface{ Write([]byte) (int, error) }) error {
	if len(args) < 1 || (args[0] != "codex" && args[0] != "claude") {
		return fmt.Errorf("usage: sr wake policy <codex|claude> [--allow-continue|--no-continue] [--max-goal-attempts N] [--continue-after N] [--cooldown DURATION]")
	}
	agent := args[0]
	cfg := wake.NewConfig(storepath.StateDir() + "/wake-config.json")
	current, err := cfg.Policy(agent)
	if err != nil {
		return err
	}
	for i := 1; i < len(args); i++ {
		switch args[i] {
		case "--allow-continue":
			current.AllowContinue = true
		case "--no-continue":
			current.AllowContinue = false
		case "--max-goal-attempts", "--continue-after":
			if i+1 >= len(args) {
				return fmt.Errorf("%s requires a positive integer", args[i])
			}
			n, err := strconv.Atoi(args[i+1])
			if err != nil || n <= 0 {
				return fmt.Errorf("%s requires a positive integer", args[i])
			}
			if args[i] == "--max-goal-attempts" {
				current.MaxGoalAttempts = n
			} else {
				current.ContinueAfter = n
			}
			i++
		case "--cooldown":
			if i+1 >= len(args) {
				return fmt.Errorf("--cooldown requires a duration")
			}
			d, err := wake.ParseDuration(args[i+1])
			if err != nil || d <= 0 {
				return fmt.Errorf("invalid cooldown")
			}
			current.CooldownSeconds = int(d / time.Second)
			if current.CooldownSeconds <= 0 {
				return fmt.Errorf("cooldown must be at least one second")
			}
			i++
		default:
			return fmt.Errorf("unknown policy option %q", args[i])
		}
	}
	if err := cfg.SetPolicy(agent, current); err != nil {
		return err
	}
	fmt.Fprintf(out, "updated %s wake policy: allow_continue=%t max_goal_attempts=%d continue_after=%d cooldown=%ds\n", agent, current.AllowContinue, current.MaxGoalAttempts, current.ContinueAfter, current.CooldownSeconds)
	return nil
}

const wakeLaunchdLabel = "ai.manaflow.subrouter.wake"

func wakeLaunchdPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "LaunchAgents", wakeLaunchdLabel+".plist"), nil
}

// desiredWakeLaunchd returns the worker plist this sr would install.
func desiredWakeLaunchd(serverURL string) (path, plist string, err error) {
	path, err = wakeLaunchdPath()
	if err != nil {
		return "", "", err
	}
	executable, err := os.Executable()
	if err != nil {
		return "", "", err
	}
	cmuxPath, err := wakeCmuxPath()
	if err != nil {
		return "", "", err
	}
	logDir := storepath.StateDir()
	return path, wakeLaunchdPlist(executable, storepath.StateDir(), logDir, cmuxPath, serverURL), nil
}

func installWakeLaunchd(out interface{ Write([]byte) (int, error) }, serverURL string) error {
	path, plist, err := desiredWakeLaunchd(serverURL)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if err := os.MkdirAll(storepath.StateDir(), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(plist), 0o600); err != nil {
		return err
	}
	uid := strconv.Itoa(os.Getuid())
	// A worker already loaded from an older plist keeps its old arguments
	// until it is booted out; bootstrap alone would fail on the loaded label.
	_ = exec.Command("launchctl", "bootout", wakeLaunchdServiceTarget()).Run()
	if err := exec.Command("launchctl", "bootstrap", "gui/"+uid, path).Run(); err != nil {
		return fmt.Errorf("wrote %s but launchctl bootstrap failed: %w", path, err)
	}
	fmt.Fprintf(out, "installed %s\n", path)
	return nil
}

// ensureWakeLaunchd installs the worker unless one is already loaded from
// exactly the plist this sr would write. A worker installed with other
// arguments, such as a stale --server, is replaced.
func ensureWakeLaunchd(out interface{ Write([]byte) (int, error) }, serverURL string) error {
	path, plist, err := desiredWakeLaunchd(serverURL)
	if err != nil {
		return err
	}
	current, readErr := os.ReadFile(path)
	if readErr == nil && string(current) == plist &&
		exec.Command("launchctl", "print", wakeLaunchdServiceTarget()).Run() == nil {
		return nil
	}
	return installWakeLaunchd(out, serverURL)
}

// wakeLaunchdServiceTarget names the worker for launchctl print and bootout,
// which take the domain and label as one gui/<uid>/<label> argument.
func wakeLaunchdServiceTarget() string {
	return "gui/" + strconv.Itoa(os.Getuid()) + "/" + wakeLaunchdLabel
}

// defaultWakeServerURL is the proxy a machine that hosts its own pool serves.
const defaultWakeServerURL = "http://127.0.0.1:31415"

// wakeServerURL is the pool server whose recovery status the worker follows.
// A client machine's pool runs on another host; following loopback there
// would leave its worker waiting on a proxy that does not exist.
func (r srRunner) wakeServerURL() (string, error) {
	server, ok, err := r.selectedRemoteServer()
	if err != nil {
		// Falling back to loopback here would install a worker that waits on a
		// proxy this machine does not run.
		return "", fmt.Errorf("resolve the pool server for the wake worker: %w", err)
	}
	if !ok || strings.TrimSpace(server.URL) == "" {
		return defaultWakeServerURL, nil
	}
	return strings.TrimSuffix(strings.TrimRight(server.URL, "/"), "/v1"), nil
}

func wakeLaunchdPlist(executable, stateDir, logDir, cmuxPath, serverURL string) string {
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
<key>Label</key><string>%s</string>
<key>ProgramArguments</key><array><string>%s</string><string>wake</string><string>worker</string><string>--cmux</string><string>%s</string><string>--server</string><string>%s</string></array>
<key>EnvironmentVariables</key><dict><key>SUBROUTER_STATE_DIR</key><string>%s</string></dict>
<key>RunAtLoad</key><true/><key>KeepAlive</key><true/><key>ThrottleInterval</key><integer>10</integer>
<key>StandardOutPath</key><string>%s</string><key>StandardErrorPath</key><string>%s</string>
</dict></plist>
`, wakeLaunchdLabel, plistXMLString(executable), plistXMLString(cmuxPath), plistXMLString(serverURL), plistXMLString(stateDir), plistXMLString(filepath.Join(logDir, "wake-worker.log")), plistXMLString(filepath.Join(logDir, "wake-worker.err.log")))
}

func plistXMLString(value string) string {
	var escaped strings.Builder
	if err := xml.EscapeText(&escaped, []byte(value)); err != nil {
		return value
	}
	return escaped.String()
}

func uninstallWakeLaunchd(out interface{ Write([]byte) (int, error) }) error {
	path, err := wakeLaunchdPath()
	if err != nil {
		return err
	}
	_ = exec.Command("launchctl", "bootout", wakeLaunchdServiceTarget()).Run()
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	fmt.Fprintf(out, "uninstalled %s\n", path)
	return nil
}

func runWakeWorker(args []string, store *wake.Store, out interface{ Write([]byte) (int, error) }) error {
	once, interval, spacing, cmuxPath, serverURL := false, 15*time.Second, 5*time.Second, "cmux", defaultWakeServerURL
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--once":
			once = true
		case "--interval", "--spacing":
			if i+1 >= len(args) {
				return fmt.Errorf("%s requires a duration", args[i])
			}
			d, err := time.ParseDuration(args[i+1])
			if err != nil || d <= 0 {
				return fmt.Errorf("invalid %s", args[i])
			}
			if args[i] == "--interval" {
				interval = d
			} else {
				spacing = d
			}
			i++
		case "--cmux":
			if i+1 >= len(args) {
				return fmt.Errorf("--cmux requires a path")
			}
			cmuxPath = args[i+1]
			i++
		case "--server":
			if i+1 >= len(args) {
				return fmt.Errorf("--server requires a URL")
			}
			serverURL = strings.TrimRight(args[i+1], "/")
			i++
		default:
			return fmt.Errorf("unknown worker option %q", args[i])
		}
	}
	lock, err := wake.AcquireWorkerLock(storePath(store) + ".worker.lock")
	if err != nil {
		return err
	}
	defer lock()
	startedAt := time.Now().UTC()
	initial := true
	lastReadinessCheck := time.Time{}
	pass := func() error {
		if err := syncRecoveryAlarms(store, serverURL, cmuxPath, startedAt, initial); err != nil {
			return err
		}
		initial = false
		if lastReadinessCheck.IsZero() || time.Since(lastReadinessCheck) >= time.Minute {
			lastReadinessCheck = time.Now()
			if err := accelerateRecoveredQuotaAlarms(store, serverURL, lastReadinessCheck); err != nil {
				fmt.Fprintf(out, "wake worker early auto-resume: %v\n", err)
			}
		}
		return dispatchDueWakeAlarms(store, serverURL, cmuxPath, spacing, startedAt, out)
	}
	if once {
		return pass()
	}
	if err := pass(); err != nil {
		fmt.Fprintf(out, "wake worker: %v\n", err)
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for range ticker.C {
		if err := pass(); err != nil {
			fmt.Fprintf(out, "wake worker: %v\n", err)
		}
	}
	return nil
}

// accelerateRecoveredQuotaAlarms uses the proxy's fresh subscription evidence
// to move only matching automatic quota alarms earlier. The shared worker
// remains the sole component that ultimately sends a terminal command.
func accelerateRecoveredQuotaAlarms(store *wake.Store, serverURL string, now time.Time) error {
	alarms, err := store.List(now)
	if err != nil {
		return err
	}
	cfg := wake.NewConfig(storepath.StateDir() + "/wake-config.json")
	checked := map[string]bool{}
	for _, alarm := range alarms {
		if !alarm.Automatic || alarm.Status != wake.StatusScheduled ||
			(alarm.Kind != wake.KindCodexQuota && alarm.Kind != wake.KindClaudeQuota) ||
			!alarm.WakeAt.After(now.Add(time.Minute)) {
			continue
		}
		enabled, err := cfg.Enabled(alarm.Agent)
		if err != nil {
			return err
		}
		early, err := cfg.EarlyOnRecovery(alarm.Agent)
		if err != nil {
			return err
		}
		if !enabled || !early {
			continue
		}
		key := alarm.Agent + "\x00" + alarm.Pool + "\x00" + alarm.ObservedAt.Format(time.RFC3339Nano)
		ready, seen := checked[key]
		if !seen {
			ready, err = fetchRecoveryReadiness(serverURL, alarm.Agent, alarm.Pool, alarm.ObservedAt)
			if err != nil {
				return err
			}
			checked[key] = ready
		}
		if !ready {
			continue
		}
		_, err = store.Update(alarm.ID, now, func(a *wake.Alarm) error {
			if a.Status != wake.StatusScheduled || !a.WakeAt.After(now.Add(time.Minute)) {
				return nil
			}
			a.WakeAt = now.Add(time.Minute)
			a.AcceleratedAt = now
			return nil
		})
		if err != nil {
			return err
		}
	}
	return nil
}

func fetchRecoveryReadiness(serverURL, agent, pool string, observedAt time.Time) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
	defer cancel()
	query := url.Values{"agent": {agent}, "pool": {pool}, "observed_at": {observedAt.Format(time.RFC3339Nano)}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(serverURL, "/")+"/_subrouter/recovery-readiness?"+query.Encode(), nil)
	if err != nil {
		return false, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("recovery-readiness returned %s", resp.Status)
	}
	var result struct {
		Ready bool `json:"ready"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return false, err
	}
	return result.Ready, nil
}

type recoveryWireState struct {
	Agent             string    `json:"agent"`
	SessionID         string    `json:"session_id"`
	Kind              string    `json:"kind"`
	Pool              string    `json:"pool"`
	LastActivityAt    time.Time `json:"last_activity_at"`
	LastFailureAt     time.Time `json:"last_failure_at"`
	ResetAt           time.Time `json:"reset_at"`
	ProviderHealthyAt time.Time `json:"provider_healthy_at"`
	LastSuccessAt     time.Time `json:"last_success_at"`
	Failures          int       `json:"failures"`
	GoalAttempts      int       `json:"goal_attempts"`
	ContinueSent      bool      `json:"continue_sent"`
	ReplayPending     bool      `json:"replay_pending"`
	LastReplayAt      time.Time `json:"last_replay_at"`
	LastReplayAction  string    `json:"last_replay_action"`
	LastReplayOutcome string    `json:"last_replay_outcome"`
}
type cmuxSessionWire struct {
	Agent     string `json:"agent"`
	SessionID string `json:"session_id"`
	SurfaceID string `json:"surface_id"`
	UpdatedAt string `json:"updated_at"`
}
type cmuxSessionsWire struct {
	Sessions []cmuxSessionWire `json:"sessions"`
}

func syncRecoveryAlarms(store *wake.Store, serverURL, cmuxPath string, _ time.Time, initial bool) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(serverURL, "/")+"/_subrouter/recovery-status", nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("recovery-status returned %s", resp.Status)
	}
	var states []recoveryWireState
	if err := json.NewDecoder(resp.Body).Decode(&states); err != nil {
		return err
	}
	sessions, err := readCMUXSessions(cmuxPath)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	existing, err := store.List(now)
	if err != nil {
		return err
	}
	for _, alarm := range existing {
		if !alarm.Automatic || alarm.Status != wake.StatusScheduled ||
			(alarm.Kind != wake.KindCodexQuota && alarm.Kind != wake.KindClaudeQuota) {
			continue
		}
		for _, state := range states {
			if state.Agent != alarm.Agent || state.SessionID != alarm.SessionID {
				continue
			}
			if state.LastSuccessAt.After(alarm.ObservedAt) || state.LastFailureAt.After(alarm.ObservedAt) {
				_, err := store.Update(alarm.ID, now, func(a *wake.Alarm) error {
					a.Status = wake.StatusStale
					a.LastError = "session made a newer request after the quota failure"
					return nil
				})
				if err != nil {
					return err
				}
			}
			break
		}
	}
	for _, state := range states {
		if (state.Agent != "codex" && state.Agent != "claude") || state.LastFailureAt.IsZero() || state.LastFailureAt.Before(now.Add(-8*time.Hour)) {
			continue
		}
		// Let the failed response finish updating cmux before capturing the
		// alarm's activity baseline. Later changes can then be treated as
		// possible manual interaction without a permanent grace blind spot.
		// A command typed during this delay without a completed provider turn
		// has not established recovery; a later response or screen update will
		// supersede the alarm, and the operator can cancel it explicitly.
		if now.Before(state.LastFailureAt.Add(30 * time.Second)) {
			continue
		}
		if (state.Kind == wake.KindCodexQuota || state.Kind == wake.KindClaudeQuota) && state.LastSuccessAt.After(state.LastFailureAt) {
			continue
		}
		enabled, err := wake.NewConfig(storepath.StateDir() + "/wake-config.json").Enabled(state.Agent)
		if err != nil {
			return err
		}
		if !enabled {
			// Disabled auto-resume must not enqueue alarms that wait for
			// a later enable. Explicit schedule commands remain usable.
			continue
		}
		var matched cmuxSessionWire
		found := false
		for _, candidate := range sessions {
			if candidate.Agent == state.Agent && candidate.SessionID == state.SessionID {
				matched, found = candidate, true
				break
			}
		}
		if !found || matched.SurfaceID == "" {
			continue
		}
		lastActive := state.LastActivityAt
		if parsed, err := time.Parse(time.RFC3339, matched.UpdatedAt); err == nil && parsed.After(lastActive) {
			lastActive = parsed
		}
		if !wake.EligibleForAutomatic(lastActive, now, initial, !initial) {
			continue
		}
		already := false
		for _, alarm := range existing {
			if alarm.Agent == state.Agent && alarm.SessionID == state.SessionID && alarm.Kind == state.Kind && alarm.ObservedAt.Equal(state.LastFailureAt) {
				already = true
				break
			}
		}
		if already {
			continue
		}
		wakeAt := state.LastFailureAt.Add(time.Minute)
		if state.ResetAt.After(wakeAt) {
			wakeAt = state.ResetAt.Add(2 * time.Minute)
		}
		screen, err := readSurface(cmuxPath, matched.SurfaceID)
		if err != nil {
			continue
		}
		action := resumeActionForSurface(screen)
		if state.Agent == "codex" && state.Kind == wake.KindCodexProvider {
			policy := wake.DefaultGoalResumePolicy()
			if configured, err := wake.NewConfig(storepath.StateDir() + "/wake-config.json").Policy("codex"); err == nil {
				policy.AllowContinue = configured.AllowContinue
				policy.MaxGoalAttempts = configured.MaxGoalAttempts
				policy.ContinueAfter = configured.ContinueAfter
				policy.Cooldown = time.Duration(configured.CooldownSeconds) * time.Second
			}
			next := policy.Next(wake.ResumeState{Failures: state.Failures, GoalAttempts: state.GoalAttempts, ContinueSent: state.ContinueSent, LastFailureAt: state.LastFailureAt, GenerationBegan: false}, now, !state.ProviderHealthyAt.IsZero() && state.ProviderHealthyAt.After(state.LastFailureAt))
			switch next {
			case wake.ResumeWait, wake.ResumeStop:
				continue
			case wake.ResumeProbe:
				// A probe is deliberately terminally cheap: validate the exact
				// surface before allowing the expensive replay. The provider's
				// next response records whether generation actually began.
				if err := validateSurface(cmuxPath, matched.SurfaceID); err != nil {
					continue
				}
			case wake.ResumeContinue:
				action = "continue"
			case wake.ResumeGoal:
				// Preserve the session-aware choice. A regular session must not
				// receive a goal-only command just because the provider policy
				// selected its goal-replay branch.
				if resumeActionForSurface(screen) == "/goal resume" {
					action = "/goal resume"
				}
			}
			wakeAt = state.LastFailureAt.Add(policy.CooldownFor(state.Failures))
		}
		_, err = store.Put(wake.Alarm{Kind: state.Kind, Agent: state.Agent, SessionID: state.SessionID, SurfaceID: matched.SurfaceID, Machine: "local", Pool: state.Pool, Action: action, WakeAt: wakeAt, ExpiresAt: wakeAt.Add(7 * 24 * time.Hour), JitterSeconds: 30, SessionLastActiveAt: lastActive, ObservedAt: state.LastFailureAt, Automatic: true}, now)
		if err != nil {
			return err
		}
		existing, _ = store.List(now)
	}
	return nil
}

func readCMUXSessions(cmuxPath string) ([]cmuxSessionWire, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	body, err := exec.CommandContext(ctx, cmuxPath, "sessions", "--json", "--all").Output()
	if err != nil {
		return nil, fmt.Errorf("cmux sessions: %w", err)
	}
	var payload cmuxSessionsWire
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("decode cmux sessions: %w", err)
	}
	return payload.Sessions, nil
}

func storePath(store *wake.Store) string {
	// Store intentionally keeps its path private; the worker lock is colocated
	// with the configured state root through this stable default path.
	return storepath.StateDir() + "/wake.json"
}

func dispatchDueWakeAlarms(store *wake.Store, serverURL, cmuxPath string, spacing time.Duration, workerStartedAt time.Time, out interface{ Write([]byte) (int, error) }) error {
	now := time.Now().UTC()
	alarms, err := store.List(now)
	if err != nil {
		return err
	}
	sent := 0
	var sessions []cmuxSessionWire
	sessionsLoaded := false
	for _, alarm := range alarms {
		if alarm.Status != wake.StatusScheduled || alarmDueAt(alarm).After(now) {
			continue
		}
		if alarm.Automatic {
			enabled, err := wake.NewConfig(storepath.StateDir() + "/wake-config.json").Enabled(alarm.Agent)
			if err != nil {
				return err
			}
			if !enabled {
				continue
			}
		}
		if alarm.Automatic {
			if alarm.Kind == wake.KindCodexProvider && alarm.SessionLastActiveAt.Before(now.Add(-8*time.Hour)) {
				_, _ = store.Update(alarm.ID, now, func(a *wake.Alarm) error {
					a.Status = wake.StatusStale
					a.LastError = "provider alarm session is older than eight hours"
					return nil
				})
				continue
			}
			// A saved automatic alarm may wait days for quota. Require its
			// exact binding; explicit manual alarms retain their surface check.
			if !sessionsLoaded {
				sessions, err = readCMUXSessions(cmuxPath)
				if err != nil {
					return err
				}
				sessionsLoaded = true
			}
			matched := false
			var sessionUpdatedAt time.Time
			for _, session := range sessions {
				if session.Agent == alarm.Agent && session.SessionID == alarm.SessionID && session.SurfaceID == alarm.SurfaceID {
					matched = true
					sessionUpdatedAt, _ = time.Parse(time.RFC3339Nano, session.UpdatedAt)
					break
				}
			}
			if !matched {
				_, _ = store.Update(alarm.ID, now, func(a *wake.Alarm) error {
					a.Status = wake.StatusStale
					a.LastError = "exact cmux session binding is missing"
					return nil
				})
				continue
			}
			if sessionUpdatedAt.IsZero() {
				_, _ = store.Update(alarm.ID, now, func(a *wake.Alarm) error {
					a.Status = wake.StatusStale
					a.LastError = "cmux session activity time is missing or invalid"
					return nil
				})
				continue
			}
			// The saved activity timestamp includes the quota error's own
			// screen update. Any subsequent change can be a manual resume.
			if !alarm.SessionLastActiveAt.IsZero() && sessionUpdatedAt.After(alarm.SessionLastActiveAt) {
				_, _ = store.Update(alarm.ID, now, func(a *wake.Alarm) error {
					a.Status = wake.StatusStale
					a.LastError = "cmux session was active after the quota failure"
					return nil
				})
				continue
			}
		}
		if err := validateSurface(cmuxPath, alarm.SurfaceID); err != nil {
			_, _ = store.Update(alarm.ID, now, func(a *wake.Alarm) error { a.Status = wake.StatusStale; a.LastError = err.Error(); return nil })
			continue
		}
		_, _ = store.Update(alarm.ID, now, func(a *wake.Alarm) error { a.Status = wake.StatusFired; a.Attempt++; return nil })
		text := alarm.Action
		if !strings.HasSuffix(text, "\n") {
			text += "\n"
		}
		if err := exec.Command(cmuxPath, "send", "--surface", alarm.SurfaceID, text).Run(); err != nil {
			_, _ = store.Update(alarm.ID, now, func(a *wake.Alarm) error { a.Status = wake.StatusFailed; a.LastError = err.Error(); return nil })
			continue
		}
		_ = postRecoveryGeneration(serverURL, alarm)
		_, _ = store.Update(alarm.ID, now, func(a *wake.Alarm) error { a.Status = wake.StatusCompleted; return nil })
		fmt.Fprintf(out, "woke %s on %s\n", alarm.ID, alarm.Agent)
		sent++
		if sent > 0 {
			time.Sleep(spacing)
		}
	}
	return nil
}

func postRecoveryGeneration(serverURL string, alarm wake.Alarm) error {
	body := strings.NewReader(fmt.Sprintf(`{"agent":%q,"session_id":%q,"action":%q,"dispatched_at":%q}`, alarm.Agent, alarm.SessionID, alarm.Action, time.Now().UTC().Format(time.RFC3339Nano)))
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(serverURL, "/")+"/_subrouter/recovery-status", body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("recovery update returned %s", resp.Status)
	}
	return nil
}

func alarmDueAt(alarm wake.Alarm) time.Time {
	if alarm.JitterSeconds <= 0 {
		return alarm.WakeAt
	}
	digest := sha256.Sum256([]byte(alarm.ID))
	var n uint64
	for _, b := range digest[:8] {
		n = (n << 8) | uint64(b)
	}
	return alarm.WakeAt.Add(time.Duration(n%uint64(alarm.JitterSeconds+1)) * time.Second)
}

func readSurface(cmuxPath, surface string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, cmuxPath, "read-screen", "--surface", surface, "--lines", "80")
	body, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("surface validation failed: %w", err)
	}
	screen := strings.TrimSpace(string(body))
	if screen == "" {
		return "", fmt.Errorf("surface validation returned an empty screen")
	}
	return screen, nil
}

func validateSurface(cmuxPath, surface string) error {
	_, err := readSurface(cmuxPath, surface)
	return err
}

func resumeActionForSurface(screen string) string {
	lower := strings.ToLower(screen)
	for _, marker := range []string{"goal paused", "goal stalled", "pursuing goal"} {
		if strings.Contains(lower, marker) {
			return "/goal resume"
		}
	}
	return "continue"
}

func updateWake(store *wake.Store, args []string, now time.Time, out interface{ Write([]byte) (int, error) }) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: sr wake update <id> [--delay DURATION] [--expires-in DURATION] [--action ACTION] [--jitter SECONDS]")
	}
	id := args[0]
	vals := map[string]string{}
	for i := 1; i < len(args); i++ {
		if !strings.HasPrefix(args[i], "--") || i+1 >= len(args) {
			return fmt.Errorf("update options must be --name value")
		}
		vals[strings.TrimPrefix(args[i], "--")] = args[i+1]
		i++
	}
	a, err := store.Update(id, now, func(a *wake.Alarm) error {
		if v := vals["delay"]; v != "" {
			d, err := wake.ParseDuration(v)
			if err != nil {
				return err
			}
			a.WakeAt = now.Add(d)
		}
		if v := vals["expires-in"]; v != "" {
			d, err := wake.ParseDuration(v)
			if err != nil {
				return err
			}
			a.ExpiresAt = now.Add(d)
		}
		if v := vals["action"]; v != "" {
			a.Action = v
		}
		if v := vals["jitter"]; v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n < 0 {
				return fmt.Errorf("jitter must be a non-negative number of seconds")
			}
			a.JitterSeconds = n
		}
		return nil
	})
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "updated %s for %s at %s\n", a.ID, a.Agent, a.WakeAt.Format(time.RFC3339))
	return nil
}

func scheduleWake(store *wake.Store, args []string, now time.Time, out interface{ Write([]byte) (int, error) }) error {
	vals := map[string]string{"kind": "quota", "action": "continue", "after": "0s", "expires-in": "7d", "jitter": "0"}
	for i := 0; i < len(args); i++ {
		if !strings.HasPrefix(args[i], "--") || i+1 >= len(args) {
			return fmt.Errorf("schedule options must be --name value")
		}
		vals[strings.TrimPrefix(args[i], "--")] = args[i+1]
		i++
	}
	for _, key := range []string{"agent", "session", "surface"} {
		if vals[key] == "" {
			return fmt.Errorf("schedule requires --%s", key)
		}
	}
	if vals["kind"] == "quota" {
		if vals["agent"] == "claude" {
			vals["kind"] = wake.KindClaudeQuota
		} else {
			vals["kind"] = wake.KindCodexQuota
		}
	}
	after, err := wake.ParseDuration(vals["after"])
	if err != nil {
		return err
	}
	expiry, err := wake.ParseDuration(vals["expires-in"])
	if err != nil {
		return err
	}
	jitter, err := strconv.Atoi(vals["jitter"])
	if err != nil || jitter < 0 {
		return fmt.Errorf("jitter must be a non-negative number of seconds")
	}
	lastActive := now
	if vals["last-active"] != "" {
		lastActive, err = time.Parse(time.RFC3339, vals["last-active"])
		if err != nil {
			return fmt.Errorf("last-active must be RFC3339: %w", err)
		}
	}
	a, err := store.Put(wake.Alarm{Kind: vals["kind"], Agent: vals["agent"], SessionID: vals["session"], SurfaceID: vals["surface"], Machine: vals["machine"], Pool: vals["pool"], Action: vals["action"], WakeAt: now.Add(after), ExpiresAt: now.Add(expiry), JitterSeconds: jitter, SessionLastActiveAt: lastActive, Automatic: false}, now)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "scheduled %s for %s at %s\n", a.ID, a.Agent, a.WakeAt.Format(time.RFC3339))
	return nil
}

func wakeNow(store *wake.Store, args []string, now time.Time, out interface{ Write([]byte) (int, error) }) error {
	agent := ""
	if len(args) > 1 {
		return fmt.Errorf("usage: sr wake now [codex|claude|all]")
	}
	if len(args) == 1 && args[0] != "all" {
		agent = args[0]
	}
	if agent != "" && agent != "codex" && agent != "claude" {
		return fmt.Errorf("usage: sr wake now [codex|claude|all]")
	}
	alarms, err := store.List(now)
	if err != nil {
		return err
	}
	count := 0
	for _, a := range alarms {
		if a.Status != wake.StatusScheduled || (agent != "" && a.Agent != agent) {
			continue
		}
		if _, err := store.Update(a.ID, now, func(x *wake.Alarm) error { x.WakeAt = now; return nil }); err != nil {
			return err
		}
		count++
	}
	fmt.Fprintf(out, "made %d wake alarm(s) eligible now\n", count)
	return nil
}

func cancelWake(store *wake.Store, args []string, now time.Time, out interface{ Write([]byte) (int, error) }) error {
	if len(args) == 1 && args[0] == "--all" {
		n, err := store.CancelAll(now)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "cancelled %d wake alarm(s)\n", n)
		return nil
	}
	if len(args) == 2 && args[0] == "--agent" {
		n, err := store.CancelAgent(args[1], now)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "cancelled %d %s wake alarm(s)\n", n, args[1])
		return nil
	}
	if len(args) != 1 {
		return fmt.Errorf("usage: sr wake cancel <id>|--agent <agent>|--all")
	}
	if _, err := store.Cancel(args[0], now); err != nil {
		return err
	}
	fmt.Fprintf(out, "cancelled %s\n", args[0])
	return nil
}

// wakeCmuxPath finds the cmux CLI the worker drives. A shell, or a GUI app,
// often lacks it on PATH, so the standard install locations are tried too.
func wakeCmuxPath() (string, error) {
	if path, err := exec.LookPath("cmux"); err == nil {
		return path, nil
	}
	home, _ := os.UserHomeDir()
	for _, candidate := range []string{
		filepath.Join(home, ".local", "bin", "cmux"),
		"/Applications/cmux.app/Contents/Resources/bin/cmux",
		filepath.Join(home, "Applications", "cmux.app", "Contents", "Resources", "bin", "cmux"),
	} {
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() && info.Mode()&0o111 != 0 {
			return candidate, nil
		}
	}
	return "", errors.New("cannot install wake worker: cmux was not found on PATH or in /Applications/cmux.app; install cmux first")
}
