package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/manaflow-ai/subrouter/internal/broker"
	"github.com/manaflow-ai/subrouter/internal/buildversion"
)

// sr host makes another machine of mine use this machine's pool. The host
// installs Subrouter from GitHub itself; only a server name and URL (or a
// team login it approves in this machine's browser) cross the SSH session.
//
// Routes, in order of preference:
//   - team:   the host signs in to the same cmux.com team and leases
//     credentials itself, so provider traffic leaves from the host.
//   - direct: the host reaches the pool server over its own network.
//   - tunnel: a reverse SSH forward from this machine. The host loses the
//     pool while this machine sleeps and API traffic hops through it, so it
//     is chosen only when nothing else reaches.

type hostRoute string

const (
	hostRouteAuto   hostRoute = "auto"
	hostRouteTeam   hostRoute = "team"
	hostRouteDirect hostRoute = "direct"
	hostRouteTunnel hostRoute = "tunnel"
)

const (
	hostBinDir        = "$HOME/.local/bin"
	hostVersionMarker = "$HOME/.local/share/subrouter/host-version"
	hostModulePath    = "github.com/manaflow-ai/subrouter/cmd/subrouter"
	hostDefaultPort   = 31415
	hostTunnelPrefix  = "ai.manaflow.subrouter.host."
)

// Test seams.
var (
	hostLaunchAgentsDir = func() (string, error) {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(home, "Library", "LaunchAgents"), nil
	}
	hostLogsDir = func() (string, error) {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(home, "Library", "Logs"), nil
	}
	hostOpenURL   = openBrowser
	hostGOOS      = runtime.GOOS
	hostBuildInfo = buildversion.Get
	hostNow       = time.Now
)

type attachedHost struct {
	SSHHost string    `json:"sshHost"`
	Route   hostRoute `json:"route"`
	// Server is the servers.json entry written on the host (direct/tunnel).
	Server string `json:"server,omitempty"`
	// URL is the pool as the host reaches it.
	URL         string    `json:"url,omitempty"`
	TeamID      string    `json:"teamId,omitempty"`
	TunnelLabel string    `json:"tunnelLabel,omitempty"`
	AttachedAt  time.Time `json:"attachedAt"`
}

type attachedHostsFile struct {
	Hosts []attachedHost `json:"hosts"`
}

type hostStore struct{ Path string }

func (r srRunner) hostStore() hostStore {
	return hostStore{Path: filepath.Join(r.store.StoreDir(), "hosts.json")}
}

func (s hostStore) load() (attachedHostsFile, error) {
	body, err := os.ReadFile(s.Path)
	if errors.Is(err, os.ErrNotExist) {
		return attachedHostsFile{}, nil
	}
	if err != nil {
		return attachedHostsFile{}, err
	}
	var file attachedHostsFile
	if err := json.Unmarshal(body, &file); err != nil {
		return attachedHostsFile{}, fmt.Errorf("read %s: %w", s.Path, err)
	}
	return file, nil
}

func (s hostStore) save(file attachedHostsFile) error {
	sort.Slice(file.Hosts, func(i, j int) bool { return file.Hosts[i].SSHHost < file.Hosts[j].SSHHost })
	body, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.Path), 0o700); err != nil {
		return err
	}
	tmp := s.Path + ".tmp"
	if err := os.WriteFile(tmp, append(body, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.Path)
}

func (f attachedHostsFile) find(host string) (attachedHost, bool) {
	for _, h := range f.Hosts {
		if h.SSHHost == host {
			return h, true
		}
	}
	return attachedHost{}, false
}

func (f *attachedHostsFile) put(host attachedHost) {
	for i := range f.Hosts {
		if f.Hosts[i].SSHHost == host.SSHHost {
			f.Hosts[i] = host
			return
		}
	}
	f.Hosts = append(f.Hosts, host)
}

func (f *attachedHostsFile) remove(host string) {
	out := f.Hosts[:0]
	for _, h := range f.Hosts {
		if h.SSHHost != host {
			out = append(out, h)
		}
	}
	f.Hosts = out
}

const srHostHelp = `sr host - Make another machine of yours use this machine's pool

Usage:
  sr host attach <ssh-host> [--route auto|team|direct|tunnel] [--url <pool-url>]...
                            [--version auto|latest|vX.Y.Z|<git-ref>] [--port 31415]
  sr host status [<ssh-host>]
  sr host detach <ssh-host>

attach installs Subrouter on the host from GitHub (on the host itself) and
points it at this machine's pool. It prefers, in order: signing the host in to
the same cmux.com team (you approve once in this machine's browser), a pool URL
the host reaches directly (this machine's server URL, or --url), and last a
reverse SSH tunnel from this machine, which stops when this machine sleeps.
--version auto matches this machine's build: the same release, or the same
commit built with go on the host. Re-running attach is safe.
`

func (r srRunner) host(ctx context.Context, args []string) error {
	if len(args) == 0 {
		fmt.Fprint(r.out, srHostHelp)
		return nil
	}
	switch args[0] {
	case "attach", "add":
		return r.hostAttach(ctx, args[1:])
	case "status", "list", "ls":
		if len(args) > 2 {
			return fmt.Errorf("usage: sr host status [<ssh-host>]")
		}
		return r.hostStatus(ctx, args[1:])
	case "detach", "remove", "rm":
		if len(args) != 2 {
			return fmt.Errorf("usage: sr host detach <ssh-host>")
		}
		return r.hostDetach(ctx, args[1])
	case "help", "-h", "--help":
		fmt.Fprint(r.out, srHostHelp)
		return nil
	default:
		return fmt.Errorf("unknown host command %q\n%s", args[0], srHostHelp)
	}
}

func validSSHHost(host string) error {
	host = strings.TrimSpace(host)
	if host == "" || strings.HasPrefix(host, "-") || strings.ContainsAny(host, " \t\n'\"`$;&|<>\\") {
		return fmt.Errorf("invalid ssh host %q", host)
	}
	return nil
}

type repeatedHostURLFlag []string

func (f *repeatedHostURLFlag) String() string { return strings.Join(*f, ",") }
func (f *repeatedHostURLFlag) Set(v string) error {
	*f = append(*f, strings.TrimSpace(v))
	return nil
}

// localPool is what this machine uses, which the host should share.
type localPool struct {
	source broker.CredentialSource
	teamID string
	team   string
	server srServerConfig
}

func (r srRunner) hostLocalPool() (localPool, error) {
	config, err := cloudModeConfig()
	if err != nil {
		return localPool{}, fmt.Errorf("load credential storage: %w", err)
	}
	switch source := config.EffectiveCredentialSource(); source {
	case broker.CredentialSourceTeam, broker.CredentialSourceHosted:
		if (source == broker.CredentialSourceTeam && !config.TeamModeReady()) ||
			(source == broker.CredentialSourceHosted && !config.HostedReady()) {
			return localPool{}, fmt.Errorf("this machine uses %s storage but is not signed in to a team; run 'sr login'", source)
		}
		return localPool{source: source, teamID: config.TeamID, team: config.TeamName}, nil
	case broker.CredentialSourceLocal:
		return localPool{}, fmt.Errorf("this machine keeps credentials local (sr storage local), so no other machine can share its pool; switch to team storage first")
	default:
		server, ok, err := r.selectedRemoteServer()
		if err != nil {
			return localPool{}, err
		}
		if !ok {
			return localPool{}, fmt.Errorf("this machine has no default pool server and is not signed in to a team; nothing to share")
		}
		if isBuiltInRemoteName(server.Name) {
			return localPool{}, fmt.Errorf("the default server %q is built in; share a named server or a team instead", server.Name)
		}
		return localPool{source: broker.CredentialSourceLegacy, server: server}, nil
	}
}

func (r srRunner) hostAttach(ctx context.Context, args []string) error {
	usage := fmt.Errorf("usage: sr host attach <ssh-host> [--route auto|team|direct|tunnel] [--url <pool-url>] [--version auto] [--port 31415]")
	flags := flag.NewFlagSet("sr host attach", flag.ContinueOnError)
	flags.SetOutput(r.errOut)
	routeFlag := flags.String("route", string(hostRouteAuto), "auto, team, direct or tunnel")
	version := flags.String("version", "auto", "auto, latest, a release tag, or a git ref built on the host")
	port := flags.Int("port", hostDefaultPort, "host loopback port for the tunnel route")
	var urls repeatedHostURLFlag
	flags.Var(&urls, "url", "pool URL the host may reach directly; repeatable")
	host, err := parseFlagsOneName(flags, args, usage)
	if err != nil {
		return err
	}
	if err := validSSHHost(host); err != nil {
		return err
	}
	route := hostRoute(strings.TrimSpace(*routeFlag))
	switch route {
	case hostRouteAuto, hostRouteTeam, hostRouteDirect, hostRouteTunnel:
	default:
		return fmt.Errorf("--route must be auto, team, direct or tunnel")
	}
	if *port < 1 || *port > 65535 {
		return fmt.Errorf("--port must be 1..65535")
	}
	spec, err := hostInstallSpecFor(*version, hostBuildInfo())
	if err != nil {
		return err
	}
	pool, err := r.hostLocalPool()
	if err != nil {
		return err
	}
	store := r.hostStore()
	state, err := store.load()
	if err != nil {
		return err
	}
	previous, hadPrevious := state.find(host)

	fmt.Fprintf(r.out, "Installing Subrouter on %s (%s)...\n", host, spec)
	if err := r.hostSSH(ctx, host, hostInstallScript(spec), nil, r.out); err != nil {
		return fmt.Errorf("install Subrouter on %s: %w", host, err)
	}

	next, err := r.hostConnect(ctx, host, pool, route, urls, *port, previous)
	if err != nil {
		return err
	}
	next.AttachedAt = hostNow().UTC()
	if hadPrevious && previous.Route == hostRouteTunnel && previous.TunnelLabel != next.TunnelLabel {
		if err := r.hostStopTunnel(ctx, previous.TunnelLabel); err != nil {
			fmt.Fprintf(r.errOut, "warning: stop old tunnel %s: %v\n", previous.TunnelLabel, err)
		}
	}
	if err := r.hostWriteMarker(ctx, next); err != nil {
		fmt.Fprintf(r.errOut, "warning: could not record the route on %s (sr status there will not show it): %v\n", host, err)
	}
	state.put(next)
	if err := store.save(state); err != nil {
		return err
	}
	fmt.Fprintf(r.out, "%s now uses %s.\n", host, describeHostRoute(next, pool))
	fmt.Fprintf(r.out, "Try it: ssh %s '~/.local/bin/sr claude'\n", host)
	return nil
}

func describeHostRoute(h attachedHost, pool localPool) string {
	switch h.Route {
	case hostRouteTeam:
		name := pool.team
		if name == "" {
			name = h.TeamID
		}
		return fmt.Sprintf("team %s directly (provider traffic leaves from the host)", name)
	case hostRouteDirect:
		return fmt.Sprintf("pool %s directly at %s", h.Server, h.URL)
	default:
		return fmt.Sprintf("pool %s through a reverse tunnel from this machine (stops while this machine sleeps)", h.Server)
	}
}

func (r srRunner) hostConnect(ctx context.Context, host string, pool localPool, route hostRoute, urls []string, port int, previous attachedHost) (attachedHost, error) {
	if pool.source == broker.CredentialSourceTeam || pool.source == broker.CredentialSourceHosted {
		if route != hostRouteAuto && route != hostRouteTeam {
			return attachedHost{}, fmt.Errorf("this machine's pool is a cmux.com team; the host joins it with --route team")
		}
		return r.hostJoinTeam(ctx, host, pool)
	}
	if route == hostRouteTeam {
		return attachedHost{}, fmt.Errorf("this machine is not signed in to a cmux.com team; run 'sr login' here first, or use --route direct|tunnel")
	}
	server := pool.server
	if route == hostRouteAuto || route == hostRouteDirect {
		candidates := hostDirectCandidates(server, urls)
		for _, candidate := range candidates {
			ok, err := r.hostProbe(ctx, host, candidate)
			if err != nil {
				return attachedHost{}, err
			}
			if ok {
				fmt.Fprintf(r.out, "%s reaches %s directly at %s.\n", host, server.Name, candidate)
				if err := r.hostWriteServer(ctx, host, server, candidate); err != nil {
					return attachedHost{}, err
				}
				return attachedHost{SSHHost: host, Route: hostRouteDirect, Server: server.Name, URL: candidate}, nil
			}
			fmt.Fprintf(r.out, "%s cannot reach %s.\n", host, candidate)
		}
		if route == hostRouteDirect {
			if len(candidates) == 0 {
				return attachedHost{}, fmt.Errorf("no direct URL to try: %s is %s on this machine; pass --url with an address the host can reach", server.Name, server.URL)
			}
			return attachedHost{}, fmt.Errorf("%s reaches none of: %s", host, strings.Join(candidates, ", "))
		}
	}
	return r.hostAttachTunnel(ctx, host, server, port, previous)
}

// hostDirectCandidates lists pool URLs worth probing from the host: explicit
// --url values first, then this machine's own URL unless it is loopback
// (which on the host would mean the host itself).
func hostDirectCandidates(server srServerConfig, urls []string) []string {
	var out []string
	seen := map[string]bool{}
	add := func(raw string) {
		root := codexProxyRootURL(raw)
		if root == "" || seen[root] {
			return
		}
		seen[root] = true
		out = append(out, root)
	}
	for _, u := range urls {
		add(u)
	}
	if parsed, err := url.Parse(server.URL); err == nil && !isLoopbackServerHost(parsed.Hostname()) {
		add(server.URL)
	}
	return out
}

func (r srRunner) hostProbe(ctx context.Context, host, root string) (bool, error) {
	out, err := r.hostSSHOutput(ctx, host, hostProbeScript(root, 1))
	if err != nil {
		return false, fmt.Errorf("probe %s from %s: %w", root, host, err)
	}
	return strings.Contains(out, "health=ok"), nil
}

func (r srRunner) hostWriteServer(ctx context.Context, host string, server srServerConfig, hostURL string) error {
	script := hostServerAddScript(server.Name, hostURL, server.TailscaleNodeID, isLoopbackURL(hostURL))
	// The tenant key authorizes the pool, so it travels on stdin rather than in
	// the SSH command line.
	stdin := strings.NewReader(strings.TrimSpace(server.TenantKey) + "\n")
	if err := r.hostSSH(ctx, host, script, stdin, r.out); err != nil {
		return fmt.Errorf("configure %s on %s: %w", server.Name, host, err)
	}
	return nil
}

func isLoopbackURL(raw string) bool {
	parsed, err := url.Parse(raw)
	return err == nil && isLoopbackServerHost(parsed.Hostname())
}

func (r srRunner) hostJoinTeam(ctx context.Context, host string, pool localPool) (attachedHost, error) {
	out, err := r.hostSSHOutput(ctx, host, hostSRCommand("storage")+" 2>/dev/null || true")
	if err != nil {
		return attachedHost{}, err
	}
	if !strings.Contains(out, pool.teamID) {
		fmt.Fprintf(r.out, "Signing %s in to cmux.com team %s. Approve it in the browser tab this opens.\n", host, pool.team)
		watcher := &approvalURLWatcher{out: r.out, open: hostOpenURL}
		login := hostSRCommand("login", "--no-browser", "--team", pool.teamID)
		if err := r.hostSSH(ctx, host, login, nil, watcher); err != nil {
			return attachedHost{}, fmt.Errorf("sign %s in to cmux.com: %w", host, err)
		}
	} else {
		fmt.Fprintf(r.out, "%s is already signed in to team %s.\n", host, pool.team)
	}
	setup := hostSRCommand("setup", "--yes", "--storage", string(pool.source))
	if err := r.hostSSH(ctx, host, setup, nil, r.out); err != nil {
		return attachedHost{}, fmt.Errorf("set up the Subrouter daemon on %s: %w", host, err)
	}
	return attachedHost{
		SSHHost: host,
		Route:   hostRouteTeam,
		TeamID:  pool.teamID,
		URL:     fmt.Sprintf("http://127.0.0.1:%d", hostDefaultPort),
	}, nil
}

// approvalURLWatcher streams the host's login output and opens the approval
// URL in this machine's browser, where the user is already signed in.
type approvalURLWatcher struct {
	out    io.Writer
	open   func(string)
	mu     sync.Mutex
	buf    bytes.Buffer
	armed  bool
	opened bool
}

func (w *approvalURLWatcher) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, err := w.out.Write(p); err != nil {
		return 0, err
	}
	w.buf.Write(p)
	for {
		line, err := w.buf.ReadString('\n')
		if err != nil {
			// Keep the partial line for the next write.
			rest := line
			w.buf.Reset()
			w.buf.WriteString(rest)
			break
		}
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "Approve Subrouter at:") {
			w.armed = true
			continue
		}
		if w.armed && !w.opened && strings.HasPrefix(line, "https://") {
			w.opened = true
			w.open(line)
		}
	}
	return len(p), nil
}

func hostTunnelLabel(host string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(host) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '.':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	return hostTunnelPrefix + b.String()
}

// hostTunnelTarget is where this machine forwards the host's port: the pool
// server as this machine reaches it.
func hostTunnelTarget(server srServerConfig) (target string, path string, err error) {
	parsed, err := url.Parse(strings.TrimSpace(server.URL))
	if err != nil || parsed.Host == "" {
		return "", "", fmt.Errorf("server %s has an invalid URL %q", server.Name, server.URL)
	}
	if !strings.EqualFold(parsed.Scheme, "http") {
		return "", "", fmt.Errorf("%s is %s; a tunnel can only carry plain http pools (TLS names would not match on the host). Pass --url with an address the host reaches", server.Name, parsed.Scheme)
	}
	port := parsed.Port()
	if port == "" {
		port = "80"
	}
	return net.JoinHostPort(parsed.Hostname(), port), strings.TrimRight(parsed.EscapedPath(), "/"), nil
}

func hostTunnelArgs(host, target string, port int) []string {
	return []string{
		"/usr/bin/ssh", "-N",
		"-o", "ControlPath=none",
		"-o", "ExitOnForwardFailure=yes",
		"-o", "BatchMode=yes",
		"-o", "ServerAliveInterval=30",
		"-o", "ServerAliveCountMax=3",
		"-R", fmt.Sprintf("127.0.0.1:%d:%s", port, target),
		host,
	}
}

func hostTunnelPlist(label string, args []string, logPath string) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>` + xmlEscapeText(label) + `</string>
  <key>ProgramArguments</key>
  <array>
`)
	for _, arg := range args {
		b.WriteString("    <string>" + xmlEscapeText(arg) + "</string>\n")
	}
	b.WriteString(`  </array>
  <key>RunAtLoad</key>
  <true/>
  <key>KeepAlive</key>
  <true/>
  <key>ThrottleInterval</key>
  <integer>15</integer>
  <key>StandardErrorPath</key>
  <string>` + xmlEscapeText(logPath) + `</string>
</dict>
</plist>
`)
	return b.String()
}

func xmlEscapeText(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&apos;").Replace(s)
}

func (r srRunner) hostAttachTunnel(ctx context.Context, host string, server srServerConfig, port int, previous attachedHost) (attachedHost, error) {
	if hostGOOS != "darwin" {
		return attachedHost{}, fmt.Errorf("%s cannot reach %s directly, and the reverse-tunnel fallback needs launchd on macOS; pass --url with an address the host reaches", host, server.Name)
	}
	target, path, err := hostTunnelTarget(server)
	if err != nil {
		return attachedHost{}, err
	}
	label := hostTunnelLabel(host)
	agents, err := hostLaunchAgentsDir()
	if err != nil {
		return attachedHost{}, err
	}
	logs, err := hostLogsDir()
	if err != nil {
		return attachedHost{}, err
	}
	plistPath := filepath.Join(agents, label+".plist")
	body := hostTunnelPlist(label, hostTunnelArgs(host, target, port), filepath.Join(logs, label+".log"))
	hostURL := fmt.Sprintf("http://127.0.0.1:%d%s", port, path)
	root := codexProxyRootURL(hostURL)

	existing, _ := os.ReadFile(plistPath)
	loaded := r.hostTunnelLoaded(ctx, label)
	unchanged := string(existing) == body && loaded
	if !unchanged {
		// Something else answering on the host port (an older hand-made
		// tunnel, or a local daemon) would make our forward fail forever.
		if !loaded {
			busy, err := r.hostProbe(ctx, host, root)
			if err != nil {
				return attachedHost{}, err
			}
			if busy {
				return attachedHost{}, fmt.Errorf("127.0.0.1:%d on %s already answers as a Subrouter; stop whatever holds it (an older tunnel or daemon) or pass --port", port, host)
			}
		}
		fmt.Fprintf(r.out, "Nothing reaches %s from %s directly; starting a reverse tunnel from this machine (%s).\n", server.Name, host, label)
		if err := os.MkdirAll(agents, 0o755); err != nil {
			return attachedHost{}, err
		}
		if err := os.MkdirAll(logs, 0o755); err != nil {
			return attachedHost{}, err
		}
		if err := os.WriteFile(plistPath, []byte(body), 0o644); err != nil {
			return attachedHost{}, err
		}
		domain := hostLaunchDomain()
		if loaded {
			_ = r.commandRunner().Run(ctx, "launchctl", []string{"bootout", domain + "/" + label}, nil, io.Discard, io.Discard)
		}
		if err := r.commandRunner().Run(ctx, "launchctl", []string{"bootstrap", domain, plistPath}, nil, r.out, r.errOut); err != nil {
			return attachedHost{}, fmt.Errorf("load tunnel %s: %w", label, err)
		}
	} else {
		fmt.Fprintf(r.out, "Reverse tunnel %s is already running.\n", label)
	}
	out, err := r.hostSSHOutput(ctx, host, hostProbeScript(root, 30))
	if err != nil {
		return attachedHost{}, err
	}
	if !strings.Contains(out, "health=ok") {
		return attachedHost{}, fmt.Errorf("tunnel %s started but %s does not answer on %s; see ~/Library/Logs/%s.log", label, server.Name, hostURL, label)
	}
	if err := r.hostWriteServer(ctx, host, server, hostURL); err != nil {
		return attachedHost{}, err
	}
	return attachedHost{SSHHost: host, Route: hostRouteTunnel, Server: server.Name, URL: hostURL, TunnelLabel: label}, nil
}

func hostLaunchDomain() string {
	return "gui/" + strconv.Itoa(os.Getuid())
}

func (r srRunner) hostTunnelLoaded(ctx context.Context, label string) bool {
	_, err := r.commandRunner().Output(ctx, "launchctl", []string{"print", hostLaunchDomain() + "/" + label})
	return err == nil
}

func (r srRunner) hostStopTunnel(ctx context.Context, label string) error {
	if label == "" || hostGOOS != "darwin" {
		return nil
	}
	if r.hostTunnelLoaded(ctx, label) {
		if err := r.commandRunner().Run(ctx, "launchctl", []string{"bootout", hostLaunchDomain() + "/" + label}, nil, io.Discard, r.errOut); err != nil {
			return err
		}
	}
	agents, err := hostLaunchAgentsDir()
	if err != nil {
		return err
	}
	if err := os.Remove(filepath.Join(agents, label+".plist")); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func (r srRunner) hostDetach(ctx context.Context, host string) error {
	if err := validSSHHost(host); err != nil {
		return err
	}
	store := r.hostStore()
	state, err := store.load()
	if err != nil {
		return err
	}
	attached, ok := state.find(host)
	if !ok {
		fmt.Fprintf(r.out, "%s is not attached.\n", host)
		return nil
	}
	var script string
	switch attached.Route {
	case hostRouteTeam:
		script = hostSRCommand("logout")
	default:
		script = hostServerRemoveScript(attached.Server, attached.URL)
	}
	script += "\n" + hostRemoveMarkerScript()
	if err := r.hostSSH(ctx, host, script, nil, r.out); err != nil {
		// The local half (tunnel, record) still comes down so a host that is
		// gone for good can be detached.
		fmt.Fprintf(r.errOut, "warning: could not clean up %s: %v\n", host, err)
	}
	if attached.Route == hostRouteTunnel {
		if err := r.hostStopTunnel(ctx, attached.TunnelLabel); err != nil {
			return fmt.Errorf("stop tunnel %s: %w", attached.TunnelLabel, err)
		}
	}
	state.remove(host)
	if err := store.save(state); err != nil {
		return err
	}
	fmt.Fprintf(r.out, "Detached %s.\n", host)
	return nil
}

func (r srRunner) hostStatus(ctx context.Context, args []string) error {
	state, err := r.hostStore().load()
	if err != nil {
		return err
	}
	hosts := state.Hosts
	if len(args) == 1 {
		h, ok := state.find(args[0])
		if !ok {
			return fmt.Errorf("%s is not attached; run: sr host attach %s", args[0], args[0])
		}
		hosts = []attachedHost{h}
	}
	if len(hosts) == 0 {
		fmt.Fprintln(r.out, "No hosts attached. Run: sr host attach <ssh-host>")
		return nil
	}
	for i, h := range hosts {
		if i > 0 {
			fmt.Fprintln(r.out)
		}
		switch h.Route {
		case hostRouteTeam:
			fmt.Fprintf(r.out, "%s: team %s (leases credentials itself)\n", h.SSHHost, h.TeamID)
		case hostRouteDirect:
			fmt.Fprintf(r.out, "%s: %s directly at %s\n", h.SSHHost, h.Server, h.URL)
		default:
			fmt.Fprintf(r.out, "%s: %s through this machine's tunnel at %s\n", h.SSHHost, h.Server, h.URL)
		}
		if h.Route == hostRouteTunnel {
			tunnelState := "stopped"
			if r.hostTunnelLoaded(ctx, h.TunnelLabel) {
				tunnelState = "running"
			}
			fmt.Fprintf(r.out, "  tunnel:  %s (%s)\n", tunnelState, h.TunnelLabel)
		}
		out, err := r.hostSSHOutput(ctx, h.SSHHost, hostStatusScript(codexProxyRootURL(h.URL)))
		if err != nil {
			fmt.Fprintf(r.out, "  host:    unreachable (%v)\n", err)
			continue
		}
		installed, health := parseHostStatus(out)
		if installed == "" {
			installed = "unknown"
		}
		fmt.Fprintf(r.out, "  sr:      %s\n", installed)
		fmt.Fprintf(r.out, "  pool:    %s\n", health)
	}
	return nil
}

func parseHostStatus(out string) (installed, health string) {
	health = "down"
	scanner := bufio.NewScanner(strings.NewReader(out))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		switch {
		case strings.HasPrefix(line, "installed="):
			installed = strings.TrimPrefix(line, "installed=")
		case line == "health=ok":
			health = "ok"
		}
	}
	return installed, health
}

// hostInstallSpec says which Subrouter build the host installs.
type hostInstallSpec struct {
	// kind is "release" (a tag or latest), "source" (go install at ref), or
	// "auto-source" (source, falling back to the latest release when the host
	// has no Go toolchain or the commit is not published).
	kind string
	ref  string
}

func (s hostInstallSpec) String() string {
	switch s.kind {
	case "release":
		if s.ref == "latest" {
			return "latest release"
		}
		return "release " + s.ref
	default:
		return "commit " + s.ref + " built on the host"
	}
}

var (
	hostReleaseTag = regexp.MustCompile(`^v?[0-9]+\.[0-9]+\.[0-9]+$`)
	hostGitRef     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]*$`)
	hostCommitHash = regexp.MustCompile(`^[0-9a-f]{7,40}$`)
)

func hostInstallSpecFor(requested string, local buildversion.Info) (hostInstallSpec, error) {
	requested = strings.TrimSpace(requested)
	switch {
	case requested == "" || requested == "auto":
		// Match this machine's build so both ends behave the same.
		if hostReleaseTag.MatchString(local.Version) && !local.Modified {
			return hostInstallSpec{kind: "release", ref: "v" + strings.TrimPrefix(local.Version, "v")}, nil
		}
		if hostCommitHash.MatchString(local.Commit) && !local.Modified {
			return hostInstallSpec{kind: "auto-source", ref: local.Commit}, nil
		}
		return hostInstallSpec{kind: "release", ref: "latest"}, nil
	case requested == "latest":
		return hostInstallSpec{kind: "release", ref: "latest"}, nil
	case hostReleaseTag.MatchString(requested):
		return hostInstallSpec{kind: "release", ref: "v" + strings.TrimPrefix(requested, "v")}, nil
	case hostGitRef.MatchString(requested) && !strings.Contains(requested, ".."):
		return hostInstallSpec{kind: "source", ref: requested}, nil
	default:
		return hostInstallSpec{}, fmt.Errorf("--version must be auto, latest, a release tag, or a git ref")
	}
}

// hostInstallScript installs into ~/.local/bin on the host, from GitHub, and
// records what it installed so a re-run is a no-op. Old releases have no
// `version` command, so the marker file is the source of truth.
func hostInstallScript(spec hostInstallSpec) string {
	return strings.Join([]string{
		"set -eu",
		`dir="` + hostBinDir + `"`,
		`marker="` + hostVersionMarker + `"`,
		"kind=" + shellQuote(spec.kind),
		"ref=" + shellQuote(spec.ref),
		`have=""`,
		`if [ -x "$dir/subrouter" ] && [ -f "$marker" ]; then have="$(cat "$marker")"; fi`,
		`mkdir -p "$dir" "$(dirname "$marker")"`,
		`install_release() {`,
		`  v="$1"`,
		`  if [ "$v" = latest ]; then`,
		`    v="$(curl -fsSIL -o /dev/null -w '%{url_effective}' https://github.com/manaflow-ai/subrouter/releases/latest | sed -n 's#.*/tag/\(v\{0,1\}[^/?#]*\).*#\1#p' | head -n 1)"`,
		`    [ -n "$v" ] || { echo "could not resolve the latest Subrouter release" >&2; return 1; }`,
		`  fi`,
		`  v="v${v#v}"`,
		`  if [ "$have" = "release $v" ]; then echo "Subrouter $v is already installed in $dir"; return 0; fi`,
		`  curl -fsSL ` + shellQuote(publicInstallScriptURL) + ` | SUBROUTER_VERSION="${v#v}" SUBROUTER_INSTALL_DIR="$dir" sh`,
		`  printf 'release %s\n' "$v" >"$marker"`,
		`}`,
		`find_go() {`,
		`  for g in "$(command -v go 2>/dev/null || true)" "$HOME/.local/bin/go" /usr/local/go/bin/go "$HOME/go/bin/go" "$HOME/.local/go/bin/go" /opt/homebrew/bin/go /usr/lib/go/bin/go; do`,
		`    if [ -n "$g" ] && [ -x "$g" ]; then echo "$g"; return 0; fi`,
		`  done`,
		`  return 1`,
		`}`,
		`install_source() {`,
		`  case "$ref" in *[!0-9a-f]*) pinned=0 ;; *) pinned=1 ;; esac`,
		`  if [ "$pinned" = 1 ] && [ "$have" = "source $ref" ]; then echo "Subrouter $ref is already installed in $dir"; return 0; fi`,
		`  go_bin="$(find_go)" || { echo "no Go toolchain on this host" >&2; return 2; }`,
		`  echo "Building ` + hostModulePath + `@$ref with $go_bin"`,
		`  GOBIN="$dir" GOFLAGS=-trimpath "$go_bin" install "` + hostModulePath + `@$ref" || return 2`,
		`  ln -sf subrouter "$dir/sr"`,
		`  ln -sf subrouter "$dir/cx"`,
		`  printf 'source %s\n' "$ref" >"$marker"`,
		`}`,
		`case "$kind" in`,
		`release) install_release "$ref" ;;`,
		`source) install_source ;;`,
		`auto-source) install_source || { echo "falling back to the latest release" >&2; install_release latest; } ;;`,
		`esac`,
	}, "\n")
}

func hostSRCommand(args ...string) string {
	quoted := make([]string, 0, len(args))
	for _, arg := range args {
		quoted = append(quoted, shellQuote(arg))
	}
	return `"` + hostBinDir + `/sr" ` + strings.Join(quoted, " ")
}

// hostProbeScript prints health=ok once the pool answers, trying for up to
// attempts seconds.
func hostProbeScript(root string, attempts int) string {
	return strings.Join([]string{
		"i=0",
		fmt.Sprintf("while [ \"$i\" -lt %d ]; do", attempts),
		"  if curl -fsS --connect-timeout 5 -m 30 -o /dev/null " + shellQuote(root+"/_subrouter/health") + " 2>/dev/null; then echo health=ok; exit 0; fi",
		"  i=$((i+1))",
		fmt.Sprintf("  [ \"$i\" -lt %d ] && sleep 1", attempts),
		"done",
		"echo health=down",
	}, "\n")
}

func hostServerAddScript(name, hostURL, tailscaleNodeID string, loopback bool) string {
	add := hostSRCommand("server", "add", name, "--url", hostURL, "--default")
	if id := strings.TrimSpace(tailscaleNodeID); id != "" && !loopback {
		add += " --tailscale-node-id " + shellQuote(id)
	}
	return strings.Join([]string{
		"set -eu",
		"tenant_key=''",
		"read -r tenant_key || true",
		`if [ -n "$tenant_key" ]; then set -- --tenant-key "$tenant_key"; else set --; fi`,
		add + ` "$@"`,
	}, "\n")
}

// hostServerRemoveScript removes the entry attach wrote, but only while it
// still points where attach left it.
func hostServerRemoveScript(name, hostURL string) string {
	return strings.Join([]string{
		"set -eu",
		`file="$HOME/.subrouter/codex/servers.json"`,
		`if [ -f "$file" ] && grep -qF ` + shellQuote(`"url": "`+hostURL+`"`) + ` "$file"; then`,
		"  " + hostSRCommand("server", "remove", name),
		"else",
		"  echo " + shellQuote("no "+name+" entry at "+hostURL+" on this host; leaving servers.json alone"),
		"fi",
	}, "\n")
}

func hostStatusScript(root string) string {
	return strings.Join([]string{
		`marker="` + hostVersionMarker + `"`,
		`if [ -f "$marker" ]; then echo "installed=$(cat "$marker")"; fi`,
		hostProbeScript(root, 1),
	}, "\n")
}

func (r srRunner) hostSSH(ctx context.Context, host, script string, stdin io.Reader, stdout io.Writer) error {
	if stdin == nil {
		stdin = strings.NewReader("")
	}
	args := []string{
		"-o", "BatchMode=yes",
		"-o", "ControlPath=none",
		"-o", "ConnectTimeout=20",
		host,
		// The login shell may not be POSIX (fish), so run the script with sh.
		"sh -c " + shellQuote(script),
	}
	return r.commandRunner().Run(ctx, "ssh", args, stdin, stdout, r.errOut)
}

func (r srRunner) hostSSHOutput(ctx context.Context, host, script string) (string, error) {
	var out bytes.Buffer
	err := r.hostSSH(ctx, host, script, nil, &out)
	return out.String(), err
}

// hostAttachMarker is written on the host by attach so tools running there
// (sr status, the Claude status line) can say how the pool is reached. A
// session on a tunneled host otherwise sees only API errors when the machine
// carrying the tunnel sleeps.
type hostAttachMarker struct {
	Pool       string    `json:"pool,omitempty"`
	Route      hostRoute `json:"route"`
	Via        string    `json:"via"`
	URL        string    `json:"url,omitempty"`
	AttachedAt time.Time `json:"attachedAt"`
}

const hostAttachMarkerFile = "host-attach.json"

func loadHostAttachMarker(storeDir string) (hostAttachMarker, bool) {
	body, err := os.ReadFile(filepath.Join(storeDir, hostAttachMarkerFile))
	if err != nil {
		return hostAttachMarker{}, false
	}
	var marker hostAttachMarker
	if json.Unmarshal(body, &marker) != nil || marker.Route == "" {
		return hostAttachMarker{}, false
	}
	return marker, true
}

func hostLocalName() string {
	name, err := os.Hostname()
	if err != nil || strings.TrimSpace(name) == "" {
		return "the attaching machine"
	}
	if i := strings.IndexByte(name, '.'); i > 0 {
		name = name[:i]
	}
	return name
}

// describe is the suffix for "Server: ..." headings on the host.
func (m hostAttachMarker) describe() string {
	switch m.Route {
	case hostRouteTunnel:
		return "via a reverse tunnel from " + m.Via + " (down while " + m.Via + " sleeps)"
	case hostRouteDirect:
		return "direct, attached from " + m.Via
	case hostRouteTeam:
		return "team lease, attached from " + m.Via
	}
	return ""
}

// withHostRoute adds the route to a pooled session's status line. Only the
// tunnel is worth the space: it is the route that fails for reasons outside
// the host.
func withHostRoute(line string, view sessionStatusView, marker hostAttachMarker, ok bool) string {
	if !ok || marker.Route != hostRouteTunnel {
		return line
	}
	down := "tunnel from " + marker.Via + " is down (asleep or offline?)"
	switch {
	case view.Stale && view.AccountID == "":
		return "sr: pool unreachable · " + down
	case view.Stale:
		return line + " · " + down
	default:
		return line + " · via " + marker.Via + " tunnel"
	}
}

func (r srRunner) serverHeading(server srServerConfig) string {
	heading := fmt.Sprintf("Server: %s (%s)", server.Name, redactedServerURL(server.URL))
	if marker, ok := loadHostAttachMarker(r.store.StoreDir()); ok && marker.Pool == server.Name && marker.URL == strings.TrimRight(server.URL, "/") {
		heading += " · " + marker.describe()
	}
	return heading
}

func hostWriteMarkerScript() string {
	return strings.Join([]string{
		"set -eu",
		`dir="$HOME/.subrouter/codex"`,
		`mkdir -p "$dir"`,
		`cat >"$dir/` + hostAttachMarkerFile + `.tmp"`,
		`mv -f "$dir/` + hostAttachMarkerFile + `.tmp" "$dir/` + hostAttachMarkerFile + `"`,
	}, "\n")
}

func hostRemoveMarkerScript() string {
	return `rm -f "$HOME/.subrouter/codex/` + hostAttachMarkerFile + `"`
}

func (r srRunner) hostWriteMarker(ctx context.Context, h attachedHost) error {
	body, err := json.Marshal(hostAttachMarker{Pool: h.Server, Route: h.Route, Via: hostLocalName(), URL: h.URL, AttachedAt: h.AttachedAt})
	if err != nil {
		return err
	}
	return r.hostSSH(ctx, h.SSHHost, hostWriteMarkerScript(), bytes.NewReader(append(body, '\n')), io.Discard)
}
