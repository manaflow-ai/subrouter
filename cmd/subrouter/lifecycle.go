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
	"github.com/manaflow-ai/subrouter/internal/broker"
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
	// remove deletes the unit or agent definition after stopping it.
	remove() error
	// describe names the managed unit for user-facing messages.
	describe() string
}

// launchdController manages the per-user LaunchAgent written by install-daemon.
type launchdController struct {
	label  string
	home   string
	runner taskRunner
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
	service := c.domain() + "/" + c.label
	if err := c.runner.Run("launchctl", "bootstrap", c.domain(), c.plist()); err != nil {
		// Bootstrap also fails when the agent is already loaded. Only then is
		// it not a failure; any other bootstrap error is reported as is.
		if c.runner.Run("launchctl", "print", service) != nil {
			return err
		}
		// Kickstart without -k starts a loaded-but-stopped agent and leaves a
		// running (healthy) daemon alone instead of killing it.
		return c.runner.Run("launchctl", "kickstart", service)
	}
	return nil
}

func (c launchdController) stop() error {
	return c.runner.Run("launchctl", "bootout", c.domain(), c.plist())
}

func (c launchdController) restart() error {
	return restartLaunchAgent(c.plist(), c.label, c.runner)
}

func (c launchdController) remove() error {
	_ = c.runner.Run("launchctl", "bootout", c.domain(), c.plist())
	if err := os.Remove(c.plist()); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// systemdController manages the system service written by install-systemd.
type systemdController struct {
	service string
	home    string
	runner  taskRunner
}

func (c systemdController) userUnit() string {
	return userSystemdUnitPath(c.home, c.service)
}

func (c systemdController) userInstalled() bool {
	_, err := os.Stat(c.userUnit())
	return err == nil
}

func (c systemdController) systemInstalled() bool {
	_, err := os.Stat(systemdUnitPath(systemdConfig{ServiceName: c.service}))
	return err == nil
}

func (c systemdController) installed() bool {
	return c.userInstalled() || c.systemInstalled()
}

func (c systemdController) describe() string {
	if c.userInstalled() {
		return c.service + ".service (user)"
	}
	return c.service + ".service"
}

func (c systemdController) run(args ...string) error {
	if c.userInstalled() {
		args = append([]string{"--user"}, args...)
	}
	return c.runner.Run("systemctl", args...)
}

func (c systemdController) start() error {
	return c.run("start", c.service)
}

// units lists what a lifecycle action acts on. The system install is socket
// activated: stopping only the service leaves subrouter.socket listening, and
// the next connection starts the service again. The socket goes first so it
// cannot re-activate the service while it is being stopped. The per-user unit
// has no socket.
func (c systemdController) units() []string {
	if c.userInstalled() {
		return []string{c.service}
	}
	return []string{c.service + ".socket", c.service}
}

func (c systemdController) stop() error {
	return c.run(append([]string{"stop"}, c.units()...)...)
}

func (c systemdController) restart() error {
	return c.run("restart", c.service)
}

func (c systemdController) remove() error {
	userUnit := c.userInstalled()
	run := func(args ...string) error {
		if userUnit {
			args = append([]string{"--user"}, args...)
		}
		return c.runner.Run("systemctl", args...)
	}
	_ = run(append([]string{"disable", "--now"}, c.units()...)...)
	config := systemdConfig{ServiceName: c.service}
	files := []string{systemdUnitPath(config), systemdSocketPath(config)}
	if userUnit {
		files = []string{c.userUnit()}
	}
	for _, file := range files {
		if err := os.Remove(file); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return run("daemon-reload")
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
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		return systemdController{
			service: defaultSystemdServiceName,
			home:    home,
			runner:  commandRunner{},
		}, nil
	case "windows":
		localAppData := os.Getenv("LOCALAPPDATA")
		if strings.TrimSpace(localAppData) == "" {
			return nil, fmt.Errorf("LOCALAPPDATA is not set; cannot locate the per-user daemon")
		}
		paths := newWindowsPaths(localAppData)
		return windowsController{
			name:    defaultWindowsTaskName,
			xmlPath: filepath.Join(paths.TaskXMLDir, "daemon.xml"),
			runner:  commandRunner{},
		}, nil
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
	waitForReady := func() bool {
		return waitForHealth(context.Background(), localBaseURL(), autostartTimeout)
	}
	return runServerLifecycleWith(controller, action, waitForReady, out)
}

// runServerLifecycleWith performs a lifecycle action and waits for the local
// health endpoint before reporting a successful start or restart.
func runServerLifecycleWith(
	controller serviceController,
	action string,
	waitForReady func() bool,
	out io.Writer,
) error {
	if !controller.installed() {
		return fmt.Errorf("no local Subrouter daemon installed; run '%s setup' first", programBase())
	}
	switch action {
	case "up", "start":
		if err := controller.start(); err != nil {
			return fmt.Errorf("start %s: %w", controller.describe(), err)
		}
		if waitForReady != nil && !waitForReady() {
			return fmt.Errorf("start %s: daemon did not become healthy", controller.describe())
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
		if waitForReady != nil && !waitForReady() {
			return fmt.Errorf("restart %s: daemon did not become healthy", controller.describe())
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
	config, err := cloudModeConfig()
	if err != nil {
		return "", err
	}
	if config.EffectiveCredentialSource() == broker.CredentialSourceTeam ||
		config.EffectiveCredentialSource() == broker.CredentialSourceLocal {
		return localBaseURL(), nil
	}
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
	return codexBaseURLForServer(server)
}
