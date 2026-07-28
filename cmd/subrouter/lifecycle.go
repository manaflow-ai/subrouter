package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/manaflow-ai/subrouter/internal/accounts"
)

// serviceController starts, stops and inspects the installed local daemon. The
// two implementations wrap launchd and systemd rather than managing processes
// directly, so `sr up` and a reboot converge on the same supervised state.
type serviceController interface {
	start() error
	stop() error
	restart() error
	// installed reports whether a unit or agent exists to act on.
	installed() bool
	// describe names the managed unit for user-facing messages.
	describe() string
}

// launchdController manages the per-user LaunchAgent written by install-daemon.
type launchdController struct {
	label  string
	home   string
	runner commandRunner
}

func (c launchdController) plist() string { return launchAgentPath(c.home, c.label) }

func (c launchdController) installed() bool {
	_, err := os.Stat(c.plist())
	return err == nil
}

func (c launchdController) describe() string { return c.label }

func (c launchdController) domain() string {
	return fmt.Sprintf("gui/%d", os.Getuid())
}

func (c launchdController) start() error {
	if err := c.runner.Run("launchctl", "bootstrap", c.domain(), c.plist()); err != nil {
		// Already bootstrapped is not a failure; kickstart still brings it up.
		return c.runner.Run("launchctl", "kickstart", "-k", c.domain()+"/"+c.label)
	}
	return nil
}

func (c launchdController) stop() error {
	return c.runner.Run("launchctl", "bootout", c.domain(), c.plist())
}

func (c launchdController) restart() error {
	return restartLaunchAgent(c.plist(), c.label, c.runner)
}

// systemdController manages the system service written by install-systemd.
type systemdController struct {
	service string
	runner  commandRunner
}

func (c systemdController) installed() bool {
	_, err := os.Stat(systemdUnitPath(systemdConfig{ServiceName: c.service}))
	return err == nil
}

func (c systemdController) describe() string { return c.service + ".service" }

func (c systemdController) start() error {
	return c.runner.Run("systemctl", "start", c.service)
}

func (c systemdController) stop() error {
	return c.runner.Run("systemctl", "stop", c.service)
}

func (c systemdController) restart() error {
	return c.runner.Run("systemctl", "restart", c.service)
}

// newServiceController picks the supervisor for the running OS.
func newServiceController() (serviceController, error) {
	switch runtime.GOOS {
	case "darwin":
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		return launchdController{label: defaultDaemonLabel, home: home, runner: commandRunner{}}, nil
	case "linux":
		return systemdController{service: defaultSystemdServiceName, runner: commandRunner{}}, nil
	default:
		return nil, fmt.Errorf("managing the local daemon is not supported on %s", runtime.GOOS)
	}
}

// runServerLifecycle implements `sr up`, `sr down` and `sr restart`.
func runServerLifecycle(action string, out io.Writer) error {
	controller, err := newServiceController()
	if err != nil {
		return err
	}
	if !controller.installed() {
		return fmt.Errorf("no local Subrouter daemon installed; run '%s install-daemon' first", filepath.Base(os.Args[0]))
	}
	switch action {
	case "up", "start":
		if err := controller.start(); err != nil {
			return fmt.Errorf("start %s: %w", controller.describe(), err)
		}
		fmt.Fprintf(out, "started %s\n", controller.describe())
	case "down", "stop":
		if err := controller.stop(); err != nil {
			return fmt.Errorf("stop %s: %w", controller.describe(), err)
		}
		fmt.Fprintf(out, "stopped %s\n", controller.describe())
	case "restart":
		if err := controller.restart(); err != nil {
			return fmt.Errorf("restart %s: %w", controller.describe(), err)
		}
		fmt.Fprintf(out, "restarted %s\n", controller.describe())
	default:
		return fmt.Errorf("unknown server action %q", action)
	}
	return nil
}

// runServerHealth implements `sr up --check` style reporting: it prints whether
// the configured server and the local daemon each answer their health probe, so
// an operator can tell a routing problem from a dead host without curl.
func runServerHealth(ctx context.Context, out io.Writer) error {
	client := fallbackHTTPClient()
	local := localBaseURL()
	fmt.Fprintf(out, "%-40s %s\n", local, healthLabel(serverHealthy(ctx, client, local)))

	configured, err := defaultCodexBaseURLForHealth()
	if err != nil {
		return err
	}
	if configured != "" && !sameEndpoint(configured, local) {
		fmt.Fprintf(out, "%-40s %s\n", configured, healthLabel(serverHealthy(ctx, client, configured)))
	}
	return nil
}

func healthLabel(ok bool) string {
	if ok {
		return "healthy"
	}
	return "UNREACHABLE"
}

// defaultCodexBaseURLForHealth resolves the configured server without applying
// fallback, so the health report shows what is configured rather than what a
// probe would substitute.
func defaultCodexBaseURLForHealth() (string, error) {
	store := defaultSRServerStore(accounts.DefaultCodexStore())
	file, err := store.load()
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(file.Default) == "" {
		return "", nil
	}
	server, ok := file.find(file.Default)
	if !ok {
		return "", nil
	}
	return codexBaseURLForServer(server), nil
}
