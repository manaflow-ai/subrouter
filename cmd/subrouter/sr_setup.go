package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/internal/broker"
)

// setupWaitTimeout bounds how long `sr setup` waits for the daemon to answer
// after starting it. Installation is fast; anything slower is a real failure.
const setupWaitTimeout = 15 * time.Second

// runSetup brings this machine to a working state and is safe to re-run. It
// installs the supervised daemon if missing, starts it, waits for health, and
// then reports the single most useful next step.
func runSetup(ctx context.Context, store accounts.CodexStore, args []string, out io.Writer) error {
	flags := flag.NewFlagSet("setup", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	var skipInstall bool
	flags.BoolVar(&skipInstall, "no-install", false, "do not install or update the daemon; only start and verify it")
	if err := flags.Parse(args); err != nil {
		return err
	}

	controller, err := newServiceController()
	if err != nil {
		return err
	}

	if !skipInstall {
		switch runtime.GOOS {
		case "darwin":
			fmt.Fprintln(out, "installing the local daemon...")
			if err := installDaemon(nil); err != nil {
				return fmt.Errorf("install daemon: %w", err)
			}
		case "linux":
			fmt.Fprintln(out, "installing the local user daemon...")
			home, err := os.UserHomeDir()
			if err != nil {
				return err
			}
			if err := installUserSystemd(home, commandRunner{}); err != nil {
				return fmt.Errorf("install user daemon: %w", err)
			}
		default:
			return fmt.Errorf("daemon installation is not supported on %s", runtime.GOOS)
		}
	}

	if !controller.installed() {
		return fmt.Errorf("no local daemon installed after setup; run '%s install-daemon' manually", programBase())
	}

	fmt.Fprintf(out, "starting %s...\n", controller.describe())
	if err := controller.start(); err != nil {
		return fmt.Errorf("start %s: %w", controller.describe(), err)
	}

	local := localBaseURL()
	if !waitForHealth(ctx, local, setupWaitTimeout) {
		return fmt.Errorf("daemon did not become healthy at %s within %s; check '%s server logs'", local, setupWaitTimeout, programBase())
	}
	fmt.Fprintf(out, "daemon healthy at %s\n", local)

	cloudConfig, err := cloudModeConfig()
	if err != nil {
		return err
	}
	if cloudConfig.TeamModeReady() {
		shared, err := broker.NewClient(cloudConfig).ListAccounts(ctx)
		if err != nil {
			return fmt.Errorf("list shared team accounts: %w", err)
		}
		if len(shared) == 0 {
			fmt.Fprintf(
				out,
				"\nNo shared accounts yet. Start with one canary:\n  %s account import --only <label> --dry-run\n",
				programBase(),
			)
			return nil
		}
		fmt.Fprintf(
			out,
			"\n%d shared account(s) available from %s. Try:\n  %s codex\n  %s claude\n",
			len(shared),
			cloudConfig.TeamName,
			programBase(),
			programBase(),
		)
		return nil
	}

	list, err := store.List()
	if err != nil {
		return err
	}
	if len(list) == 0 {
		fmt.Fprintf(out, "\nNo accounts yet. Add one with:\n  %s add\n", programBase())
		return nil
	}
	fmt.Fprintf(out, "\n%d account(s) available. Try:\n  %s codex\n  %s claude\n", len(list), programBase(), programBase())
	return nil
}

// waitForHealth polls a base URL until it answers or the deadline passes.
func waitForHealth(ctx context.Context, baseURL string, timeout time.Duration) bool {
	client := fallbackHTTPClient()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if serverHealthy(ctx, client, baseURL) {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(250 * time.Millisecond):
		}
	}
	return false
}

// runCleanup removes this machine's daemon. Credentials are preserved unless
// --purge is passed, because losing a refresh-token chain means re-running OAuth
// for every account.
func runCleanup(store accounts.CodexStore, args []string, out io.Writer) error {
	flags := flag.NewFlagSet("cleanup", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	var yes, purge bool
	flags.BoolVar(&yes, "yes", false, "perform the removal instead of printing the plan")
	flags.BoolVar(&purge, "purge", false, "also delete stored accounts and credentials")
	if err := flags.Parse(args); err != nil {
		return err
	}

	controller, err := newServiceController()
	if err != nil {
		return err
	}
	return runCleanupWith(controller, store, yes, purge, out)
}

// runCleanupWith is runCleanup with an injected controller so tests do not
// depend on what this machine happens to have installed.
func runCleanupWith(controller serviceController, store accounts.CodexStore, yes, purge bool, out io.Writer) error {
	plan := []string{}
	if controller.installed() {
		plan = append(plan, fmt.Sprintf("stop and remove %s", controller.describe()))
	}
	if purge {
		plan = append(plan, fmt.Sprintf("delete %s (all stored accounts and credentials)", store.StoreDir()))
		if cloudPath, err := broker.DefaultConfigPath(); err == nil {
			plan = append(
				plan,
				fmt.Sprintf("revoke and delete the cmux.com session in %s", cloudPath),
			)
		}
	}
	if len(plan) == 0 {
		fmt.Fprintln(out, "nothing to clean up")
		return nil
	}

	fmt.Fprintln(out, "cleanup will:")
	for _, step := range plan {
		fmt.Fprintf(out, "  - %s\n", step)
	}
	if !yes {
		fmt.Fprintf(out, "\nre-run with --yes to proceed\n")
		return nil
	}

	if controller.installed() {
		description := controller.describe()
		if err := controller.stop(); err != nil {
			// A daemon that is already stopped is not a failure worth aborting on.
			fmt.Fprintf(out, "warning: stop %s: %v\n", description, err)
		}
		if err := controller.remove(); err != nil {
			return err
		}
		fmt.Fprintf(out, "removed %s\n", description)
	}
	if purge {
		cloudPath, err := broker.DefaultConfigPath()
		if err != nil {
			return err
		}
		cloudConfig, loadErr := broker.LoadConfig(cloudPath)
		if loadErr == nil && cloudConfig.LoggedIn() {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			logoutErr := broker.NewClient(cloudConfig).Logout(ctx)
			cancel()
			if logoutErr != nil {
				return fmt.Errorf(
					"revoke cmux.com session before purge: %w",
					logoutErr,
				)
			}
		}
		if err := broker.DeleteConfig(cloudPath); err != nil {
			return err
		}
		fmt.Fprintf(out, "deleted %s\n", cloudPath)
		if err := os.RemoveAll(store.StoreDir()); err != nil {
			return err
		}
		fmt.Fprintf(out, "deleted %s\n", store.StoreDir())
	}
	return nil
}

// doctorCheck is one diagnosed condition. Status is "ok", "warn" or "fail".
type doctorCheck struct {
	status string
	title  string
	detail string
}

// runDoctor reports whether traffic can actually flow, in the order a failure
// would bite: daemon installed, daemon healthy, configured server healthy,
// accounts present. It exits non-zero when something is broken so it can gate
// scripts.
func runDoctor(ctx context.Context, store accounts.CodexStore, out io.Writer) error {
	controller, err := newServiceController()
	return runDoctorWith(ctx, controller, err, store, out)
}

// runDoctorWith is runDoctor with an injected controller, so tests exercise the
// reporting logic rather than this machine's launchd state.
func runDoctorWith(ctx context.Context, controller serviceController, controllerErr error, store accounts.CodexStore, out io.Writer) error {
	checks := []doctorCheck{}
	client := fallbackHTTPClient()
	local := localBaseURL()
	cloudConfig, cloudConfigErr := cloudModeConfig()
	source := cloudConfig.EffectiveCredentialSource()
	teamReady := cloudConfigErr == nil && cloudConfig.TeamModeReady()

	switch {
	case cloudConfigErr != nil:
		checks = append(checks, doctorCheck{"fail", "credential storage", cloudConfigErr.Error()})
	case source == broker.CredentialSourceTeam && !cloudConfig.LoggedIn():
		checks = append(checks, doctorCheck{"fail", "team vault", fmt.Sprintf("not signed in; run '%s login'", programBase())})
	case source == broker.CredentialSourceTeam && cloudConfig.TeamID == "":
		checks = append(checks, doctorCheck{"fail", "team vault", fmt.Sprintf("no team selected; run '%s team use <team>'", programBase())})
	case source == broker.CredentialSourceTeam:
		checks = append(checks, doctorCheck{"ok", "team vault", fmt.Sprintf("%s (%s)", cloudConfig.TeamName, cloudConfig.TeamID)})
	case source == broker.CredentialSourceLocal:
		checks = append(checks, doctorCheck{"ok", "credential storage", fmt.Sprintf("local (%s)", store.StoreDir())})
	default:
		checks = append(checks, doctorCheck{"warn", "credential storage", "legacy remote server"})
	}

	err := controllerErr
	switch {
	case err != nil:
		checks = append(checks, doctorCheck{"warn", "supervisor", err.Error()})
	case !controller.installed():
		checks = append(checks, doctorCheck{"warn", "daemon installed", fmt.Sprintf("no local daemon; run '%s setup'", programBase())})
	default:
		checks = append(checks, doctorCheck{"ok", "daemon installed", controller.describe()})
	}

	localOK := serverHealthy(ctx, client, local)
	if localOK {
		checks = append(checks, doctorCheck{"ok", "local daemon", local})
	} else if source == broker.CredentialSourceTeam ||
		source == broker.CredentialSourceLocal {
		checks = append(checks, doctorCheck{"fail", "local daemon", fmt.Sprintf("%s is not answering; run '%s daemon start'", local, programBase())})
	} else {
		checks = append(checks, doctorCheck{"warn", "local daemon", fmt.Sprintf("%s is not answering; run '%s daemon start'", local, programBase())})
	}

	if teamReady {
		checks = append(checks, doctorCheck{"ok", "provider egress", "this machine via the local daemon"})
		shared, listErr := broker.NewClient(cloudConfig).ListAccounts(ctx)
		switch {
		case listErr != nil:
			checks = append(checks, doctorCheck{"fail", "shared accounts", listErr.Error()})
		case len(shared) == 0:
			checks = append(checks, doctorCheck{"fail", "shared accounts", fmt.Sprintf("none; start with '%s account import --only <label> --dry-run'", programBase())})
		default:
			ready := 0
			for _, item := range shared {
				if item.Health == nil || item.Health.OK {
					ready++
				}
			}
			if ready == 0 {
				checks = append(checks, doctorCheck{"fail", "shared accounts", fmt.Sprintf("%d require repair", len(shared))})
			} else if ready < len(shared) {
				checks = append(checks, doctorCheck{"warn", "shared accounts", fmt.Sprintf("%d ready, %d require repair", ready, len(shared)-ready)})
			} else {
				checks = append(checks, doctorCheck{"ok", "shared accounts", fmt.Sprintf("%d available", ready)})
			}
		}
	} else {
		if source == broker.CredentialSourceLocal {
			checks = append(checks, doctorCheck{"ok", "provider egress", "this machine via the local daemon"})
		} else {
			configured, configuredErr := defaultCodexBaseURLForHealth()
			if configuredErr != nil {
				checks = append(checks, doctorCheck{"warn", "configured server", configuredErr.Error()})
			} else if configured == "" || sameEndpoint(configured, local) {
				checks = append(checks, doctorCheck{"ok", "configured server", "local"})
			} else if serverHealthy(ctx, client, configured) {
				checks = append(checks, doctorCheck{"ok", "configured server", configured})
			} else if localOK && !fallbackDisabled() {
				checks = append(checks, doctorCheck{"warn", "configured server", fmt.Sprintf("%s unreachable; codex will fall back to %s", configured, local)})
			} else {
				checks = append(checks, doctorCheck{"fail", "configured server", fmt.Sprintf("%s unreachable and no local fallback available", configured)})
			}
		}

		list, listErr := store.List()
		switch {
		case listErr != nil:
			checks = append(checks, doctorCheck{"fail", "accounts", listErr.Error()})
		case len(list) == 0:
			checks = append(checks, doctorCheck{"fail", "accounts", fmt.Sprintf("none stored; run '%s add'", programBase())})
		default:
			checks = append(checks, doctorCheck{"ok", "accounts", fmt.Sprintf("%d stored", len(list))})
		}
	}

	failed := false
	for _, check := range checks {
		fmt.Fprintf(out, "%s  %-20s %s\n", doctorMark(check.status), check.title, check.detail)
		if check.status == "fail" {
			failed = true
		}
	}
	if failed {
		return errors.New("doctor found a blocking problem")
	}
	return nil
}

func doctorMark(status string) string {
	switch status {
	case "ok":
		return "ok  "
	case "warn":
		return "warn"
	default:
		return "FAIL"
	}
}

// programBase is the invoked binary name, so help text matches how the user
// actually called it (sr, cx or subrouter).
func programBase() string {
	base := filepath.Base(os.Args[0])
	if strings.TrimSpace(base) == "" || base == "." {
		return "sr"
	}
	return base
}
