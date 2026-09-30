package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/term"
)

// sr host watch keeps attached hosts' pool health on screen. It is meant to
// run in a cmux Dock pane: there it also labels every workspace whose remote
// destination is an attached host with a sidebar status pill, so a Claude
// session on big-red shows "lawrence ✓ tunnel" or "pool down" next to its
// workspace without anything on big-red having to reach cmux.

const hostCmuxStatusKey = "sr-pool"

var hostInCmux = func() bool {
	return strings.TrimSpace(os.Getenv("CMUX_WORKSPACE_ID")) != "" ||
		strings.TrimSpace(os.Getenv("CMUX_SOCKET_PATH")) != ""
}

type hostCheck struct {
	host      attachedHost
	reachable bool
	healthy   bool
	tunnelUp  bool
	installed string
	version   string
	rollout   string
}

// pill is the sidebar status text and color for this host.
func (c hostCheck) pill() (text, color string) {
	name := c.host.Server
	if c.host.Route == hostRouteTeam {
		name = "team"
	}
	switch {
	case !c.reachable:
		return name + " ? host unreachable", "#FF9F0A"
	case c.healthy && c.host.Route == hostRouteTunnel:
		return name + " ✓ tunnel", "#34C759"
	case c.healthy:
		return name + " ✓", "#34C759"
	case c.host.Route == hostRouteTunnel && !c.tunnelUp:
		return name + " ✗ tunnel stopped", "#FF3B30"
	default:
		return name + " ✗ pool down", "#FF3B30"
	}
}

func (r srRunner) hostWatch(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("sr host watch", flag.ContinueOnError)
	flags.SetOutput(r.errOut)
	interval := flags.Duration("interval", time.Minute, "time between checks")
	once := flags.Bool("once", false, "check once and exit")
	noCmux := flags.Bool("no-cmux", false, "do not set cmux sidebar status pills")
	if err := parseFlagsNoPositionals(flags, args); err != nil {
		return err
	}
	if *interval < 10*time.Second {
		return fmt.Errorf("--interval must be at least 10s")
	}
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	var pills *hostCmuxPills
	if !*noCmux && hostInCmux() {
		pills = &hostCmuxPills{r: r, last: map[cmuxWorkspaceKey]string{}}
		// A pill left behind by a dead watcher would claim health nobody is
		// checking, so they come down on the way out. --once is a snapshot.
		if !*once {
			defer pills.clearAll(context.Background())
		}
	}
	clear := isTerminalWriter(r.out)
	for {
		state, err := r.hostStore().load()
		if err != nil {
			return err
		}
		checks := r.hostCheckAll(ctx, state.Hosts)
		if ctx.Err() != nil {
			return nil
		}
		if clear {
			fmt.Fprint(r.out, "\033[H\033[2J")
		}
		renderHostWatch(r.out, checks, hostNow(), *interval, *once)
		if pills != nil {
			if err := pills.update(ctx, checks); err != nil {
				fmt.Fprintf(r.out, "cmux: %v\n", err)
			}
		}
		if *once {
			return nil
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(*interval):
		}
	}
}

func isTerminalWriter(w io.Writer) bool {
	f, ok := w.(*os.File)
	return ok && term.IsTerminal(int(f.Fd()))
}

func (r srRunner) hostCheckAll(ctx context.Context, hosts []attachedHost) []hostCheck {
	checks := make([]hostCheck, len(hosts))
	var wg sync.WaitGroup
	for i, h := range hosts {
		wg.Add(1)
		go func(i int, h attachedHost) {
			defer wg.Done()
			checks[i] = r.hostCheckOne(ctx, h)
		}(i, h)
	}
	wg.Wait()
	return checks
}

func (r srRunner) hostCheckOne(ctx context.Context, h attachedHost) hostCheck {
	check := hostCheck{host: h}
	if h.Route == hostRouteTunnel {
		check.tunnelUp = r.hostTunnelLoaded(ctx, h.TunnelLabel)
	}
	out, err := r.hostSSHOutput(ctx, h.SSHHost, hostStatusScript(codexProxyRootURL(h.URL)))
	if err != nil {
		return check
	}
	check.reachable = true
	installed, health := parseHostStatus(out)
	check.installed = installed
	check.healthy = health == "ok"
	check.version, check.rollout = parseHostVisibility(out)
	return check
}

func renderHostWatch(w io.Writer, checks []hostCheck, now time.Time, interval time.Duration, once bool) {
	heading := "Pool hosts · " + now.Local().Format("15:04:05")
	if !once {
		heading += " · every " + interval.String()
	}
	fmt.Fprintln(w, heading)
	if len(checks) == 0 {
		fmt.Fprintln(w, "No hosts attached. Run: sr host attach <ssh-host>")
		return
	}
	for _, c := range checks {
		route := string(c.host.Route)
		if c.host.Route == hostRouteTunnel {
			if c.tunnelUp {
				route += " (running)"
			} else {
				route += " (stopped)"
			}
		}
		text, _ := c.pill()
		if c.version != "" {
			text += " · server " + c.version
		}
		if c.rollout != "" {
			text += " · " + c.rollout
		}
		fmt.Fprintf(w, "  %-12s %-20s %s\n", c.host.SSHHost, route, text)
	}
}

// hostCmuxPills mirrors each check onto the cmux workspaces whose remote
// destination is that host, in every cmux window. It only calls cmux when a
// pill changes.
type hostCmuxPills struct {
	r    srRunner
	last map[cmuxWorkspaceKey]string // pill text currently shown
}

type cmuxWorkspaceKey struct{ window, workspace string }

type cmuxWorkspaceRow struct {
	ID          string `json:"id"`
	WorkspaceID string `json:"workspace_id"`
	Remote      *struct {
		Destination string `json:"destination"`
	} `json:"remote"`
}

func (row cmuxWorkspaceRow) id() string {
	if row.ID != "" {
		return row.ID
	}
	return row.WorkspaceID
}

func parseCmuxWorkspaces(body []byte) ([]cmuxWorkspaceRow, error) {
	var rows []cmuxWorkspaceRow
	if err := json.Unmarshal(body, &rows); err == nil {
		return rows, nil
	}
	var wrapped struct {
		Workspaces []cmuxWorkspaceRow `json:"workspaces"`
	}
	if err := json.Unmarshal(body, &wrapped); err != nil {
		return nil, fmt.Errorf("read cmux workspace list: %w", err)
	}
	return wrapped.Workspaces, nil
}

// parseCmuxWindows reads `cmux list-windows --json`: an array of windows.
func parseCmuxWindows(body []byte) ([]string, error) {
	var rows []struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(body, &rows); err != nil {
		var wrapped struct {
			Windows []struct {
				ID string `json:"id"`
			} `json:"windows"`
		}
		if err := json.Unmarshal(body, &wrapped); err != nil {
			return nil, fmt.Errorf("read cmux window list: %w", err)
		}
		rows = wrapped.Windows
	}
	var ids []string
	for _, row := range rows {
		if row.ID != "" {
			ids = append(ids, row.ID)
		}
	}
	return ids, nil
}

// sameSSHHost treats "big-red" and "leo@big-red" as the same destination.
func sameSSHHost(a, b string) bool {
	strip := func(s string) string {
		s = strings.ToLower(strings.TrimSpace(s))
		if i := strings.LastIndexByte(s, '@'); i >= 0 {
			s = s[i+1:]
		}
		return s
	}
	return a != "" && b != "" && strip(a) == strip(b)
}

func (p *hostCmuxPills) cmux(ctx context.Context, args ...string) error {
	return p.r.commandRunner().Run(ctx, "cmux", args, nil, io.Discard, io.Discard)
}

func (p *hostCmuxPills) update(ctx context.Context, checks []hostCheck) error {
	body, err := p.r.commandRunner().Output(ctx, "cmux", []string{"list-windows", "--json"})
	if err != nil {
		return fmt.Errorf("list windows: %w", err)
	}
	windows, err := parseCmuxWindows(body)
	if err != nil {
		return err
	}
	want := map[cmuxWorkspaceKey]hostCheck{}
	for _, window := range windows {
		body, err := p.r.commandRunner().Output(ctx, "cmux", []string{"workspace", "list", "--json", "--window", window})
		if err != nil {
			return fmt.Errorf("list workspaces in window %s: %w", window, err)
		}
		rows, err := parseCmuxWorkspaces(body)
		if err != nil {
			return err
		}
		for _, row := range rows {
			if row.Remote == nil || row.id() == "" {
				continue
			}
			for _, c := range checks {
				if sameSSHHost(row.Remote.Destination, c.host.SSHHost) {
					want[cmuxWorkspaceKey{window, row.id()}] = c
					break
				}
			}
		}
	}
	keys := make([]cmuxWorkspaceKey, 0, len(want))
	for key := range want {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].window != keys[j].window {
			return keys[i].window < keys[j].window
		}
		return keys[i].workspace < keys[j].workspace
	})
	for _, key := range keys {
		text, color := want[key].pill()
		if p.last[key] == text {
			continue
		}
		if err := p.cmux(ctx, "set-status", hostCmuxStatusKey, text, "--workspace", key.workspace, "--window", key.window, "--color", color); err != nil {
			return fmt.Errorf("set status on %s: %w", key.workspace, err)
		}
		p.last[key] = text
	}
	for key := range p.last {
		if _, ok := want[key]; !ok {
			p.clear(ctx, key)
		}
	}
	return nil
}

func (p *hostCmuxPills) clear(ctx context.Context, key cmuxWorkspaceKey) {
	_ = p.cmux(ctx, "clear-status", hostCmuxStatusKey, "--workspace", key.workspace, "--window", key.window)
	delete(p.last, key)
}

func (p *hostCmuxPills) clearAll(ctx context.Context) {
	for key := range p.last {
		p.clear(ctx, key)
	}
}
