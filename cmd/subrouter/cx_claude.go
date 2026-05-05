package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/manaflow-ai/subrouter/internal/agents/claude"
)

const cxClaudeHelp = `cx claude - Manage multiple Claude Code profiles

Usage:
  cx claude                     Show profiles and switch interactively
  cx claude add [name]          Add account (opens OAuth login, infers email)
  cx claude list                List all profiles with auth status
  cx claude switch [name]       Switch active profile
  cx claude remove <name>       Remove a profile
  cx claude env                 Print export CLAUDE_CONFIG_DIR=...
  cx claude run [name] [...]    Launch Claude with a specific profile
  cx claude <name> [...]        Shorthand for 'cx claude run <name>'
  cx claude help                Show this help
`

type claudeRunner struct {
	store  claude.Store
	in     io.Reader
	out    io.Writer
	errOut io.Writer
	client *http.Client
}

func (r cxRunner) claude(ctx context.Context, args []string) error {
	cr := claudeRunner{
		store:  claude.DefaultStore(),
		in:     r.in,
		out:    r.out,
		errOut: r.errOut,
		client: r.client,
	}
	return cr.run(ctx, args)
}

func (r claudeRunner) run(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return r.defaultInteractive(ctx)
	}
	switch args[0] {
	case "add", "login":
		name := ""
		if len(args) > 1 {
			name = args[1]
		}
		return r.add(ctx, name)
	case "list", "ls", "status":
		return r.list(ctx, false)
	case "switch", "use":
		if len(args) < 2 {
			return r.defaultInteractive(ctx)
		}
		return r.switchProfile(args[1])
	case "remove", "rm":
		if len(args) < 2 {
			return fmt.Errorf("usage: cx claude remove <name>")
		}
		return r.remove(args[1])
	case "env":
		return r.env()
	case "run":
		name := ""
		extra := []string{}
		if len(args) > 1 {
			if strings.HasPrefix(args[1], "-") {
				extra = args[1:]
			} else {
				name = args[1]
				extra = args[2:]
			}
		}
		return r.runClaude(ctx, name, extra)
	case "help", "-h", "--help":
		fmt.Fprint(r.out, cxClaudeHelp)
		return nil
	default:
		if _, ok := r.store.FindProfile(args[0]); ok {
			return r.runClaude(ctx, args[0], args[1:])
		}
		return fmt.Errorf("unknown command: cx claude %s\n%s", args[0], cxClaudeHelp)
	}
}

func (r claudeRunner) add(ctx context.Context, name string) error {
	claudePath, ok := claude.DetectCLI()
	if !ok {
		return fmt.Errorf("Claude CLI not found. Install from https://claude.ai/download")
	}

	var instancePath string
	var tempDir string
	var err error
	if name != "" {
		instancePath, err = r.store.CreateProfile(name)
		if err != nil {
			return err
		}
	} else {
		instancePath, tempDir, err = r.store.CreateTempInstance()
		if err != nil {
			return err
		}
	}
	claudeConfigDir := r.store.PreferredInstancePath(instancePath)

	fmt.Fprintln(r.out, "Starting Claude Code...")
	fmt.Fprintln(r.out, "Complete the OAuth login in your browser, then exit Claude to finish setup.")
	fmt.Fprintln(r.out)

	cmd := exec.CommandContext(ctx, claudePath)
	cmd.Stdin = r.in
	cmd.Stdout = r.out
	cmd.Stderr = r.errOut
	cmd.Env = append(os.Environ(), "CLAUDE_CONFIG_DIR="+claudeConfigDir)
	if err := cmd.Run(); err != nil {
		if name != "" {
			_, _ = r.store.RemoveProfile(name)
		} else {
			_ = r.store.CleanupInstance(tempDir)
		}
		return fmt.Errorf("Claude login did not complete: %w", err)
	}

	status, err := claude.AuthStatusForPath(ctx, claudePath, claudeConfigDir)
	if err != nil || status == nil || !status.LoggedIn {
		if name != "" {
			_, _ = r.store.RemoveProfile(name)
		} else {
			_ = r.store.CleanupInstance(tempDir)
		}
		return fmt.Errorf("login was not completed")
	}

	profileName := name
	if profileName == "" {
		profileName = status.Email
		if profileName == "" {
			profileName = "default"
		}
		if _, ok := r.store.FindProfile(profileName); ok {
			_, _ = r.store.RemoveProfile(profileName)
		}
		if err := r.store.RegisterProfile(profileName, tempDir); err != nil {
			return err
		}
	}

	plan := ""
	if status.SubscriptionType != "" {
		plan = " [" + status.SubscriptionType + "]"
	}
	email := ""
	if status.Email != "" {
		email = " (" + status.Email + ")"
	}
	fmt.Fprintf(r.out, "\nAdded Claude profile %q.%s%s\n", profileName, email, plan)
	fmt.Fprintf(r.out, "\n  cx claude switch %s\n", profileName)
	fmt.Fprintf(r.out, "  cx claude run %s\n", profileName)
	return nil
}

func (r claudeRunner) list(ctx context.Context, numbered bool) error {
	infos := r.fetchInfos(ctx)
	displayClaudeProfiles(r.out, infos, numbered)
	return nil
}

func (r claudeRunner) defaultInteractive(ctx context.Context) error {
	profiles := r.store.ListProfiles()
	if len(profiles) == 0 {
		fmt.Fprintln(r.out, "No Claude profiles. Run 'cx claude add' to create one.")
		return nil
	}
	infos := r.fetchInfos(ctx)
	displayClaudeProfiles(r.out, infos, true)
	reader := bufio.NewReader(r.in)
	answer, err := promptLine(r.out, reader, "Switch to (#): ")
	if err != nil {
		return err
	}
	answer = strings.TrimSpace(answer)
	if answer == "" {
		return nil
	}
	if idx, err := strconv.Atoi(answer); err == nil && idx >= 1 && idx <= len(infos) {
		return r.switchProfile(infos[idx-1].Name)
	}
	return r.switchProfile(answer)
}

func (r claudeRunner) fetchInfos(ctx context.Context) []claude.ProfileInfo {
	profiles := r.store.ListProfiles()
	active := r.store.ActiveProfile()
	claudePath, _ := claude.DetectCLI()
	infos := make([]claude.ProfileInfo, len(profiles))
	var wg sync.WaitGroup
	for i, profile := range profiles {
		i, profile := i, profile
		infos[i] = claude.ProfileInfo{Name: profile.Name, Active: profile.Name == active, CreatedAt: profile.CreatedAt}
		wg.Add(1)
		go func() {
			defer wg.Done()
			claudeConfigDir := r.store.ClaudeConfigDir(profile.Name)
			var auth *claude.AuthStatus
			var credential *claude.CredentialInfo
			var authErr, credentialErr error
			var inner sync.WaitGroup
			inner.Add(2)
			go func() {
				defer inner.Done()
				auth, authErr = claude.AuthStatusForPath(ctx, claudePath, claudeConfigDir)
			}()
			go func() {
				defer inner.Done()
				credential, credentialErr = r.store.ReadCredential(ctx, claudeConfigDir)
			}()
			inner.Wait()
			if authErr != nil {
				infos[i].Error = authErr
				return
			}
			if credentialErr != nil {
				infos[i].Error = credentialErr
				return
			}
			infos[i].Auth = auth
			infos[i].Credential = credential
			if credential != nil && credential.AccessToken != "" {
				usage, err := claude.FetchUsage(ctx, r.client, credential.AccessToken)
				if err == nil {
					infos[i].Usage = usage
				}
			}
		}()
	}
	wg.Wait()
	return infos
}

func (r claudeRunner) switchProfile(selector string) error {
	profile, ok, err := r.store.MatchProfile(selector)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("no profile matching %q", selector)
	}
	if err := r.store.SetActiveProfile(profile.Name); err != nil {
		return err
	}
	fmt.Fprintf(r.out, "Active Claude profile: %s\n", profile.Name)
	fmt.Fprintf(r.out, "\n  export CLAUDE_CONFIG_DIR=%s\n", r.store.ClaudeConfigDir(profile.Name))
	fmt.Fprintln(r.out, "\nOr add to shell rc: eval \"$(cx claude env)\"")
	return nil
}

func (r claudeRunner) remove(selector string) error {
	profile, ok, err := r.store.MatchProfile(selector)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("profile %q not found", selector)
	}
	removed, err := r.store.RemoveProfile(profile.Name)
	if err != nil {
		return err
	}
	if !removed {
		return fmt.Errorf("profile %q not found", selector)
	}
	fmt.Fprintf(r.out, "Removed Claude profile: %s\n", profile.Name)
	return nil
}

func (r claudeRunner) env() error {
	active := r.store.ActiveProfile()
	if active == "" {
		return nil
	}
	fmt.Fprintf(r.out, "export CLAUDE_CONFIG_DIR=%s\n", r.store.ClaudeConfigDir(active))
	return nil
}

func (r claudeRunner) runClaude(ctx context.Context, name string, extra []string) error {
	if name == "" {
		name = r.store.ActiveProfile()
	}
	if name == "" {
		return fmt.Errorf("no profile specified and no active profile set")
	}
	profile, ok, err := r.store.MatchProfile(name)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("profile %q not found", name)
	}
	claudePath, ok := claude.DetectCLI()
	if !ok {
		return fmt.Errorf("Claude CLI not found. Install from https://claude.ai/download")
	}
	if err := r.store.SetActiveProfile(profile.Name); err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, claudePath, extra...)
	cmd.Stdin = r.in
	cmd.Stdout = r.out
	cmd.Stderr = r.errOut
	cmd.Env = append(os.Environ(), "CLAUDE_CONFIG_DIR="+r.store.ClaudeConfigDir(profile.Name))
	return cmd.Run()
}

type claudeRow struct {
	label    string
	used     float64
	resetsIn string
}

func displayClaudeProfiles(out io.Writer, infos []claude.ProfileInfo, numbered bool) {
	if len(infos) == 0 {
		fmt.Fprintln(out, "No Claude profiles. Run 'cx claude add' to create one.")
		return
	}
	colored := colorEnabled(out)
	width := 0
	for _, info := range infos {
		for _, row := range collectClaudeRows(info) {
			width = max(width, len(row.label))
		}
	}
	fmt.Fprintln(out)
	for i, info := range infos {
		prefix := ""
		if numbered {
			prefix = fmt.Sprintf("%d) ", i+1)
		}
		active := ""
		if info.Active {
			active = " " + style(colored, ansiCyan, "(active)")
		}
		if info.Error != nil {
			fmt.Fprintf(out, "%s%s%s\n", style(colored, ansiDim, prefix), style(colored, ansiBold+ansiWhite, info.Name), active)
			fmt.Fprintf(out, "  %s\n\n", style(colored, ansiRed, "Error: "+info.Error.Error()))
			continue
		}
		if info.Auth == nil || !info.Auth.LoggedIn {
			fmt.Fprintf(out, "%s%s%s\n", style(colored, ansiDim, prefix), style(colored, ansiBold+ansiWhite, info.Name), active)
			fmt.Fprintln(out, "  "+style(colored, ansiDim, "not logged in"))
			fmt.Fprintln(out)
			continue
		}
		plan := ""
		if info.Auth.SubscriptionType != "" {
			plan = " " + style(colored, ansiDim, "["+info.Auth.SubscriptionType+"]")
		}
		fmt.Fprintf(out, "%s%s%s%s\n", style(colored, ansiDim, prefix), style(colored, ansiBold+ansiWhite, info.Name), plan, active)
		rows := collectClaudeRows(info)
		if len(rows) == 0 {
			detail := info.Auth.Email
			if detail == "" {
				detail = "unknown"
			}
			if info.Auth.OrgName != "" {
				detail += " (" + info.Auth.OrgName + ")"
			}
			fmt.Fprintf(out, "  %s\n\n", style(colored, ansiDim, detail))
			continue
		}
		for _, row := range rows {
			fmt.Fprintf(out, "  %s: %s %s", style(colored, ansiDim, pad(row.label, width)), renderBar(row.used, colored), style(colored, usageColor(row.used), formatPercentLeft(row.used)+" left"))
			if row.resetsIn != "" {
				fmt.Fprintf(out, " %s", style(colored, ansiDim, "resets in "+row.resetsIn))
			}
			fmt.Fprintln(out)
		}
		fmt.Fprintln(out)
	}
}

func collectClaudeRows(info claude.ProfileInfo) []claudeRow {
	if info.Usage == nil {
		return nil
	}
	var rows []claudeRow
	add := func(label string, limit *claude.RateLimit) {
		if limit == nil || limit.Utilization == nil {
			return
		}
		rows = append(rows, claudeRow{label: label, used: *limit.Utilization, resetsIn: formatResetTime(limit.ResetsAt)})
	}
	add("5h limit", info.Usage.FiveHour)
	add("7d limit", info.Usage.SevenDay)
	add("Opus (weekly)", info.Usage.SevenDayOpus)
	add("Sonnet (weekly)", info.Usage.SevenDaySonnet)
	if info.Usage.ExtraUsage != nil && info.Usage.ExtraUsage.IsEnabled && info.Usage.ExtraUsage.Utilization != nil {
		rows = append(rows, claudeRow{label: "Extra usage", used: *info.Usage.ExtraUsage.Utilization})
	}
	return rows
}

func formatResetTime(value string) string {
	if value == "" {
		return ""
	}
	t, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return ""
	}
	seconds := int64(time.Until(t).Seconds())
	if seconds < 0 {
		seconds = 0
	}
	return formatDuration(seconds)
}
