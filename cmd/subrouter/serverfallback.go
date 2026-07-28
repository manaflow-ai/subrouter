package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// localFallbackBaseURL is where the per-machine daemon listens. It is the same
// address install-daemon and install-systemd bind.
const localFallbackBaseURL = "http://127.0.0.1:31415/v1"

// fallbackProbeTimeout bounds each health probe. A host that has exhausted its
// kernel socket buffers drops SYNs instead of refusing them, so an unbounded
// dial hangs the agent launch indefinitely rather than failing over.
const fallbackProbeTimeout = 1500 * time.Millisecond

// healthURLFor derives the health endpoint from a Subrouter base URL. Base URLs
// carry an optional /v1 suffix for OpenAI-compatible clients, while the health
// route is served at the origin.
func healthURLFor(baseURL string) (string, error) {
	trimmed := strings.TrimSpace(baseURL)
	if trimmed == "" {
		return "", fmt.Errorf("empty base URL")
	}
	parsed, err := url.Parse(trimmed)
	if err != nil {
		return "", err
	}
	if parsed.Scheme == "" || parsed.Host == "" {
		return "", fmt.Errorf("base URL %q is not absolute", baseURL)
	}
	parsed.Path = "/_subrouter/health"
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed.String(), nil
}

// sameEndpoint reports whether two base URLs address the same origin, so a
// server that is already local is never "failed over" onto itself.
func sameEndpoint(a, b string) bool {
	parsedA, errA := url.Parse(strings.TrimSpace(a))
	parsedB, errB := url.Parse(strings.TrimSpace(b))
	if errA != nil || errB != nil {
		return false
	}
	return strings.EqualFold(parsedA.Host, parsedB.Host)
}

// serverHealthy reports whether a Subrouter server answers its health probe.
func serverHealthy(ctx context.Context, client *http.Client, baseURL string) bool {
	healthURL, err := healthURLFor(baseURL)
	if err != nil {
		return false
	}
	probeCtx, cancel := context.WithTimeout(ctx, fallbackProbeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(probeCtx, http.MethodGet, healthURL, nil)
	if err != nil {
		return false
	}
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<10))
	return resp.StatusCode == http.StatusOK
}

// fallbackDisabled lets an operator pin the configured server even when it is
// down, for cases where routing through local accounts is worse than failing.
func fallbackDisabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("SUBROUTER_DISABLE_FALLBACK"))) {
	case "", "0", "false", "no":
		return false
	default:
		return true
	}
}

// localBaseURL resolves where the per-machine daemon listens, allowing an
// override for non-default installs and for tests.
func localBaseURL() string {
	if override := strings.TrimSpace(os.Getenv("SUBROUTER_LOCAL_BASE_URL")); override != "" {
		return override
	}
	return localFallbackBaseURL
}

// withLocalFallback returns baseURL when the configured server answers its
// health probe. When it does not and the local daemon does, it returns the local
// base URL so agents keep working through this machine's own accounts instead of
// hanging against a wedged host.
func withLocalFallback(ctx context.Context, client *http.Client, baseURL string, warn io.Writer) string {
	return withLocalFallbackTo(ctx, client, baseURL, localBaseURL(), defaultDaemonStarter(), warn)
}

// daemonStarter brings the local daemon up. It is injected so tests can drive
// autostart without touching launchd.
type daemonStarter func() error

// autostartTimeout bounds how long an agent launch waits for a daemon it just
// started. Longer than a healthy start needs, shorter than a user will tolerate.
const autostartTimeout = 10 * time.Second

// defaultDaemonStarter starts the installed daemon, or reports why it cannot.
func defaultDaemonStarter() daemonStarter {
	return func() error {
		controller, err := newServiceController()
		if err != nil {
			return err
		}
		if !controller.installed() {
			return fmt.Errorf("no local daemon installed; run '%s setup'", programBase())
		}
		return controller.start()
	}
}

// ensureLocalHealthy probes the local daemon and starts it if nothing answers,
// so `sr codex` works on a cold machine without a separate `sr server up`.
func ensureLocalHealthy(ctx context.Context, client *http.Client, local string, start daemonStarter, warn io.Writer) bool {
	if serverHealthy(ctx, client, local) {
		return true
	}
	if start == nil {
		return false
	}
	if warn != nil {
		fmt.Fprintln(warn, "subrouter: local daemon is not running, starting it...")
	}
	if err := start(); err != nil {
		if warn != nil {
			fmt.Fprintf(warn, "subrouter: could not start the local daemon: %v\n", err)
		}
		return false
	}
	if !waitForHealth(ctx, local, autostartTimeout) {
		if warn != nil {
			fmt.Fprintf(warn, "subrouter: local daemon did not become healthy within %s\n", autostartTimeout)
		}
		return false
	}
	return true
}

// withLocalFallbackTo is withLocalFallback with an explicit local address and an
// injected starter.
//
// Order matters: a healthy configured server always wins, so autostart only runs
// when nothing is already serving the request.
func withLocalFallbackTo(ctx context.Context, client *http.Client, baseURL, local string, start daemonStarter, warn io.Writer) string {
	if serverHealthy(ctx, client, baseURL) {
		return baseURL
	}
	// The configured target is down. If it *is* the local daemon, starting it
	// fixes the configured target itself rather than substituting anything, so
	// this path runs even when cross-host fallback is disabled.
	if sameEndpoint(baseURL, local) {
		ensureLocalHealthy(ctx, client, local, start, warn)
		return baseURL
	}
	if fallbackDisabled() {
		return baseURL
	}
	if !ensureLocalHealthy(ctx, client, local, start, warn) {
		return baseURL
	}
	if warn != nil {
		fmt.Fprintf(warn, "subrouter: %s is unreachable; falling back to the local daemon at %s\n", baseURL, local)
	}
	return local
}

// fallbackHTTPClient is the probe client. It deliberately does not follow
// redirects and keeps no idle connections, so a probe never wedges a later call.
func fallbackHTTPClient() *http.Client {
	return &http.Client{
		Timeout: fallbackProbeTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// claudeManagementSubcommands never launch the agent, so they must not trigger
// a daemon autostart.
var claudeManagementSubcommands = map[string]struct{}{
	"add": {}, "login": {},
	"list": {}, "ls": {}, "status": {},
	"switch": {}, "use": {},
	"remove": {}, "rm": {},
	"env":  {},
	"pick": {},
	"push": {}, "upload": {},
	"help": {}, "-h": {}, "--help": {},
}

// claudeLaunchesAgent reports whether `sr claude <args>` will start Claude Code.
// No arguments means the interactive launcher, which does.
func claudeLaunchesAgent(args []string) bool {
	if len(args) == 0 {
		return true
	}
	_, management := claudeManagementSubcommands[args[0]]
	return !management
}
