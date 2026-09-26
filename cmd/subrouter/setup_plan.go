package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"golang.org/x/term"

	"github.com/manaflow-ai/subrouter/internal/accounts"
)

// Setup asks for one confirmation covering the whole reviewed change set rather
// than a yes/no per item. Per-item prompts produce half-installed machines: a
// user who declines background startup gets a daemon that stops working at the
// next reboot and no longer resembles any state we test. Enter-to-apply is
// already opt-in because nothing changes until the key is pressed, and the
// uncommon choices stay reachable behind one keystroke.

// setupAction identifies a unit of work in the plan.
type setupAction string

const (
	actionBackground   setupAction = "background"
	actionConfigCodex  setupAction = "config-codex"
	actionConfigClaude setupAction = "config-claude"
	actionAdoptLocal   setupAction = "adopt-local"
)

// setupItem is one line of the review screen.
type setupItem struct {
	Action setupAction
	// Summary is the imperative description shown on the review screen.
	Summary string
	// Detail is the exact change, shown by `d`.
	Detail string
	// Change marks new work (+), a modification of existing state (~), or
	// something already true (=), which is listed but never applied.
	Change string
	// Selected reports whether applying the plan performs this item.
	Selected bool
	// Optional reports whether `e` may toggle it. Required work is not
	// togglable, because a machine without it is not set up.
	Optional bool
}

type setupPlan struct {
	Items []setupItem
}

func (p setupPlan) find(action setupAction) (setupItem, bool) {
	for _, item := range p.Items {
		if item.Action == action {
			return item, true
		}
	}
	return setupItem{}, false
}

func (p setupPlan) selected(action setupAction) bool {
	item, ok := p.find(action)
	return ok && item.Selected && item.Change != "="
}

// pending reports whether applying the plan would change anything.
func (p setupPlan) pending() bool {
	for _, item := range p.Items {
		if item.Selected && item.Change != "=" {
			return true
		}
	}
	return false
}

func (p *setupPlan) toggle(action setupAction) bool {
	for i := range p.Items {
		if p.Items[i].Action != action || !p.Items[i].Optional {
			continue
		}
		p.Items[i].Selected = !p.Items[i].Selected
		return true
	}
	return false
}

// backgroundSummary names the platform mechanism, because "run in the
// background" means a different object to inspect on each OS.
func backgroundSummary(goos string) string {
	switch goos {
	case "darwin":
		return "Run Subrouter in the background after you sign in (LaunchAgent)"
	case "linux":
		return "Run Subrouter in the background after you sign in (systemd user service)"
	case "windows":
		return "Run Subrouter in the background after you sign in (scheduled task)"
	default:
		return "Run Subrouter in the background after you sign in"
	}
}

// setupPlanInput is the observed state the plan is computed from.
type setupPlanInput struct {
	GOOS             string
	DaemonInstalled  bool
	CodexConfigured  bool
	ClaudeConfigured bool
	LocalAccounts    int
	TeamModeReady    bool
	// WantBackground and WantConfig carry --no-background and --no-config.
	WantBackground bool
	WantConfig     bool
}

func buildSetupPlan(in setupPlanInput) setupPlan {
	change := func(done bool) string {
		if done {
			return "="
		}
		return "+"
	}
	plan := setupPlan{Items: []setupItem{
		{
			Action:   actionBackground,
			Summary:  backgroundSummary(in.GOOS),
			Detail:   backgroundDetail(in.GOOS),
			Change:   change(in.DaemonInstalled),
			Selected: in.WantBackground,
			Optional: true,
		},
		{
			Action:   actionConfigCodex,
			Summary:  "Configure Codex to route through Subrouter",
			Detail:   "sets openai_base_url and chatgpt_base_url in ~/.codex/config.toml to the local daemon",
			Change:   change(in.CodexConfigured),
			Selected: in.WantConfig,
			Optional: true,
		},
		{
			Action:   actionConfigClaude,
			Summary:  "Configure Claude Code to route through Subrouter",
			Detail:   "points Claude Code's base URL at the local daemon",
			Change:   change(in.ClaudeConfigured),
			Selected: in.WantConfig,
			Optional: true,
		},
	}}
	// Only offer a credential decision when there is somewhere to hand them to.
	// Without a vault the accounts simply stay local, and rendering a
	// deselectable "keep" line implies setup might remove them.
	if in.LocalAccounts > 0 && in.TeamModeReady {
		// Say plainly that handing accounts to the vault stops this machine
		// refreshing them. The provider rotates refresh tokens on use, so two
		// refreshers invalidate each other; a user who does not know that will
		// read this line as a harmless copy.
		summary := fmt.Sprintf("Hand %d existing account(s) to the team vault", in.LocalAccounts)
		detail := "uploads them, then stops refreshing them here so only the vault holds their token chains"
		plan.Items = append(plan.Items, setupItem{
			Action:   actionAdoptLocal,
			Summary:  summary,
			Detail:   detail,
			Change:   "~",
			Selected: in.TeamModeReady,
			Optional: true,
		})
	}
	return plan
}

func backgroundDetail(goos string) string {
	switch goos {
	case "darwin":
		return "writes ~/Library/LaunchAgents/" + defaultDaemonLabel + ".plist and loads it"
	case "linux":
		return "writes ~/.config/systemd/user/" + defaultSystemdServiceName + ".service and enables it"
	case "windows":
		return `registers the scheduled task \\Subrouter\\Daemon to start at logon`
	default:
		return "installs a per-user service that starts at login"
	}
}

// renderSetupPlan writes the review screen. Selected items are marked with their
// change type; deselected ones are dimmed to a dash so the screen always shows
// every choice that exists rather than hiding what was turned off.
func renderSetupPlan(plan setupPlan, out io.Writer) {
	fmt.Fprintln(out, "Subrouter is ready to set up this computer.")
	fmt.Fprintln(out)
	for _, item := range plan.Items {
		marker := item.Change
		if !item.Selected {
			marker = "-"
		}
		if item.Change == "=" && item.Selected {
			fmt.Fprintf(out, "  %s %s (already done)\n", marker, item.Summary)
			continue
		}
		fmt.Fprintf(out, "  %s %s\n", marker, item.Summary)
	}
	fmt.Fprintln(out)
	if !plan.pending() {
		fmt.Fprintln(out, "Nothing to do. Press Enter to exit.")
	} else {
		fmt.Fprintln(out, "Press Enter to apply.")
	}
	fmt.Fprintln(out, "[d] details   [e] edit options   [Ctrl-C] cancel")
}

func renderSetupDetails(plan setupPlan, out io.Writer) {
	fmt.Fprintln(out)
	for _, item := range plan.Items {
		state := "will apply"
		switch {
		case item.Change == "=":
			state = "already done"
		case !item.Selected:
			state = "skipped"
		}
		fmt.Fprintf(out, "  %s [%s]\n      %s\n", item.Summary, state, item.Detail)
	}
	fmt.Fprintln(out)
}

func renderSetupOptions(plan setupPlan, out io.Writer) {
	fmt.Fprintln(out)
	index := 0
	for _, item := range plan.Items {
		if !item.Optional {
			continue
		}
		index++
		mark := " "
		if item.Selected {
			mark = "x"
		}
		fmt.Fprintf(out, "  %d. [%s] %s\n", index, mark, item.Summary)
	}
	fmt.Fprintln(out, "\nType a number to toggle, or press Enter to go back.")
}

// optionalActions lists togglable actions in display order, so a typed number on
// the options screen maps to the same item the user is looking at.
func (p setupPlan) optionalActions() []setupAction {
	var out []setupAction
	for _, item := range p.Items {
		if item.Optional {
			out = append(out, item.Action)
		}
	}
	return out
}

// reviewSetupPlan runs the interactive review. It returns the plan to apply and
// whether the user confirmed. EOF counts as cancellation: an unattended terminal
// must never be treated as approval.
func reviewSetupPlan(plan setupPlan, in io.Reader, out io.Writer) (setupPlan, bool) {
	reader := bufio.NewReader(in)
	for {
		renderSetupPlan(plan, out)
		line, err := reader.ReadString('\n')
		if err != nil && strings.TrimSpace(line) == "" {
			fmt.Fprintln(out, "\ncancelled; nothing was changed")
			return plan, false
		}
		switch strings.ToLower(strings.TrimSpace(line)) {
		case "":
			return plan, true
		case "d":
			renderSetupDetails(plan, out)
		case "e":
			plan = editSetupOptions(plan, reader, out)
		case "q":
			fmt.Fprintln(out, "cancelled; nothing was changed")
			return plan, false
		default:
			fmt.Fprintln(out, "unrecognised key; press Enter to apply, d for details, e to edit")
		}
	}
}

func editSetupOptions(plan setupPlan, reader *bufio.Reader, out io.Writer) setupPlan {
	for {
		renderSetupOptions(plan, out)
		line, err := reader.ReadString('\n')
		choice := strings.TrimSpace(line)
		if choice == "" {
			return plan
		}
		actions := plan.optionalActions()
		var index int
		if _, scanErr := fmt.Sscanf(choice, "%d", &index); scanErr != nil ||
			index < 1 || index > len(actions) {
			fmt.Fprintln(out, "no such option")
			if err != nil {
				return plan
			}
			continue
		}
		plan.toggle(actions[index-1])
		if err != nil {
			return plan
		}
	}
}

// currentGOOS exists so tests can render every platform's wording.
func currentGOOS() string { return runtime.GOOS }

// setupPlanOptions carries the flag-derived preferences into plan construction.
type setupPlanOptions struct {
	wantBackground bool
	wantConfig     bool
}

// clientRoutesToLocal reports whether a client config file already points at the
// local daemon. Reading the file beats trusting a flag: the plan must describe
// what is actually on disk, or "already done" is a guess.
func clientRoutesToLocal(path, localURL string) bool {
	body, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	target := strings.TrimSuffix(localURL, "/")
	return strings.Contains(string(body), target)
}

// setupStdin returns stdin when a human is present, and nil otherwise. A pipe or
// a cron job cannot approve a change set, and treating its silence as approval is
// how unattended runs surprise people.
func setupStdin() io.Reader {
	// A character-device check is not enough: /dev/null is one, and reading a
	// change-set approval from /dev/null is exactly the mistake to avoid. Only a
	// real terminal has a human behind it.
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return nil
	}
	return os.Stdin
}

// planForSetup observes the machine and builds the reviewable change set.
func planForSetup(_ context.Context, store accounts.CodexStore, opts setupPlanOptions) (setupPlan, error) {
	controller, err := newServiceController()
	installed := err == nil && controller.installed()

	local := localBaseURL()
	codexPath, codexErr := defaultCodexConfigPath()
	codexConfigured := codexErr == nil && clientRoutesToLocal(codexPath, local)

	claudeConfigured := false
	if home, homeErr := os.UserHomeDir(); homeErr == nil {
		claudeConfigured = clientRoutesToLocal(
			filepath.Join(home, ".claude", "settings.json"), local)
	}

	localAccounts := 0
	if list, listErr := store.ListStored(); listErr == nil {
		localAccounts = len(list)
	}

	teamReady := false
	if cloudConfig, cloudErr := cloudModeConfig(); cloudErr == nil {
		teamReady = cloudConfig.TeamModeReady()
	}

	return buildSetupPlan(setupPlanInput{
		GOOS:             currentGOOS(),
		DaemonInstalled:  installed,
		CodexConfigured:  codexConfigured,
		ClaudeConfigured: claudeConfigured,
		LocalAccounts:    localAccounts,
		TeamModeReady:    teamReady,
		WantBackground:   opts.wantBackground,
		WantConfig:       opts.wantConfig,
	}), nil
}
