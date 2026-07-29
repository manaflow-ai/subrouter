package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	agentclaude "github.com/manaflow-ai/subrouter/internal/agents/claude"
	"github.com/manaflow-ai/subrouter/internal/broker"
)

func (r srRunner) cloudLogin(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("login", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	baseURL := flags.String("base-url", "", "cmux.com base URL")
	teamSelector := flags.String("team", "", "team ID or name to select after login")
	noBrowser := flags.Bool("no-browser", false, "print the approval URL without opening it")
	if err := flags.Parse(args); err != nil {
		return err
	}

	path, err := broker.DefaultConfigPath()
	if err != nil {
		return err
	}
	config, err := broker.LoadConfig(path)
	if err != nil {
		return err
	}
	if strings.TrimSpace(*baseURL) != "" {
		config.BaseURL = *baseURL
	}
	client := broker.NewClient(config)
	start, err := client.StartAuth(ctx)
	if err != nil {
		return fmt.Errorf("start cmux.com login: %w", err)
	}

	fmt.Fprintf(r.out, "Approve Subrouter at:\n  %s\n\nCode: %s\n", start.VerificationURL, start.UserCode)
	if !*noBrowser {
		openBrowser(start.VerificationURL)
	}
	interval := time.Duration(start.IntervalSeconds) * time.Second
	if interval <= 0 {
		interval = 3 * time.Second
	}
	expires := time.Duration(start.ExpiresInSeconds) * time.Second
	if expires <= 0 {
		expires = 15 * time.Minute
	}
	deadline := time.NewTimer(expires)
	defer deadline.Stop()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	var poll broker.AuthPoll
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return fmt.Errorf("login expired before approval")
		case <-ticker.C:
			poll, err = client.PollAuth(ctx, start.DeviceCode)
			if err != nil {
				return fmt.Errorf("poll cmux.com login: %w", err)
			}
			switch poll.Status {
			case "pending":
				continue
			case "approved":
				if poll.AccessToken == "" || poll.RefreshToken == "" {
					return fmt.Errorf("cmux.com approved login without session tokens")
				}
			default:
				return fmt.Errorf("login %s", poll.Status)
			}
		}
		break
	}

	config.AccessToken = poll.AccessToken
	config.RefreshToken = poll.RefreshToken
	client = broker.NewClient(config)
	teams, selectedTeamID, err := client.ListTeams(ctx)
	if err != nil {
		return fmt.Errorf("list Stack teams: %w", err)
	}
	selector := strings.TrimSpace(*teamSelector)
	if selector == "" {
		selector = selectedTeamID
	}
	team, err := matchCloudTeam(teams, selector)
	if err != nil {
		return err
	}
	config.TeamID = team.ID
	config.TeamName = team.Name
	if err := broker.SaveConfig(path, config); err != nil {
		return err
	}
	fmt.Fprintf(r.out, "Logged in to cmux.com team %s (%s).\n", team.Name, team.ID)
	restartInstalledDaemon(r.errOut)
	return nil
}

func cloudModeConfig() (broker.Config, error) {
	path, err := broker.DefaultConfigPath()
	if err != nil {
		return broker.Config{}, err
	}
	return broker.LoadConfig(path)
}

func cloudLocalProxyToken(config broker.Config) string {
	if !config.Ready() {
		return ""
	}
	return config.AccessToken
}

func (r srRunner) cloudSetup(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("setup", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	noLogin := flags.Bool("no-login", false, "install the daemon without signing in to cmux.com")
	noInstall := flags.Bool("no-install", false, "start and verify the existing daemon")
	if err := flags.Parse(args); err != nil {
		return err
	}

	config, err := cloudModeConfig()
	if err != nil {
		return err
	}
	if !*noLogin && !config.LoggedIn() {
		fmt.Fprintln(r.out, "First, sign in to cmux.com.")
		if err := r.cloudLogin(ctx, nil); err != nil {
			return err
		}
		config, err = cloudModeConfig()
		if err != nil {
			return err
		}
	}
	if !*noLogin && config.LoggedIn() && config.TeamID == "" {
		return fmt.Errorf("no team selected; run 'sr team use <team>'")
	}
	setupArgs := []string{}
	if *noInstall {
		setupArgs = append(setupArgs, "--no-install")
	}
	return runSetup(ctx, r.store, setupArgs, r.out)
}

func (r srRunner) cloudLogout(ctx context.Context) error {
	path, err := broker.DefaultConfigPath()
	if err != nil {
		return err
	}
	config, err := broker.LoadConfig(path)
	if err != nil {
		return err
	}
	if config.LoggedIn() {
		if err := broker.NewClient(config).Logout(ctx); err != nil {
			return fmt.Errorf(
				"could not revoke the cmux.com session; local credentials were kept so logout can be retried: %w",
				err,
			)
		}
	}
	if err := broker.DeleteConfig(path); err != nil {
		return err
	}
	fmt.Fprintln(r.out, "Logged out of cmux.com.")
	restartInstalledDaemon(r.errOut)
	return nil
}

func (r srRunner) cloudTeam(ctx context.Context, args []string) error {
	if len(args) == 0 {
		args = []string{"current"}
	}
	config, path, client, err := loadCloudClient(false)
	if err != nil {
		return err
	}
	switch args[0] {
	case "list", "ls":
		teams, _, err := client.ListTeams(ctx)
		if err != nil {
			return err
		}
		for _, team := range teams {
			marker := " "
			if team.ID == config.TeamID {
				marker = "*"
			}
			fmt.Fprintf(r.out, "%s %-28s %s\n", marker, team.Name, team.ID)
		}
		return nil
	case "current":
		if config.TeamID == "" {
			return fmt.Errorf("no team selected; run 'sr team list' then 'sr team use <team>'")
		}
		fmt.Fprintf(r.out, "%s (%s)\n", config.TeamName, config.TeamID)
		return nil
	case "use":
		if len(args) != 2 {
			return fmt.Errorf("usage: sr team use <team-id-or-name>")
		}
		teams, _, err := client.ListTeams(ctx)
		if err != nil {
			return err
		}
		team, err := matchCloudTeam(teams, args[1])
		if err != nil {
			return err
		}
		config.TeamID = team.ID
		config.TeamName = team.Name
		if err := broker.SaveConfig(path, config); err != nil {
			return err
		}
		fmt.Fprintf(r.out, "Using %s (%s).\n", team.Name, team.ID)
		restartInstalledDaemon(r.errOut)
		return nil
	default:
		return fmt.Errorf("unknown team command %q; use list, current, or use", args[0])
	}
}

func (r srRunner) cloudAccount(ctx context.Context, args []string) error {
	if len(args) == 0 {
		args = []string{"list"}
	}
	_, _, client, err := loadCloudClient(true)
	if err != nil {
		return err
	}
	switch args[0] {
	case "list", "ls":
		items, err := client.ListAccounts(ctx)
		if err != nil {
			return err
		}
		if len(items) == 0 {
			fmt.Fprintln(r.out, "No shared accounts.")
			return nil
		}
		for _, item := range items {
			status := "ready"
			if item.Health != nil && !item.Health.OK {
				status = "NEEDS REPAIR"
			}
			fmt.Fprintf(
				r.out,
				"%-20s %-32s %-14s %s\n",
				item.Kind,
				item.Label,
				status,
				item.ID,
			)
		}
		return nil
	case "import", "push", "upload":
		return r.cloudAccountImport(ctx, client, args[1:])
	case "remove", "rm":
		if len(args) != 2 {
			return fmt.Errorf("usage: sr account remove <account-id>")
		}
		if err := client.DeleteAccount(ctx, args[1]); err != nil {
			return err
		}
		fmt.Fprintf(r.out, "Removed shared account %s.\n", args[1])
		return nil
	case "add":
		return r.cloudAccountAdd(ctx, client, args[1:])
	case "repair":
		return r.cloudAccountRepair(ctx, client, args[1:])
	default:
		return fmt.Errorf("unknown account command %q; use list, import, remove, or repair", args[0])
	}
}

func (r srRunner) cloudAccountAdd(
	ctx context.Context,
	client *broker.Client,
	args []string,
) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: sr account add <codex|claude|openai-key|anthropic-key>")
	}
	if args[0] == "anthropic-key" {
		reader := bufio.NewReader(r.in)
		label, err := promptLine(r.out, reader, "Label (e.g. work, personal): ")
		if err != nil {
			return err
		}
		key, err := promptLine(r.out, reader, "Anthropic API key (sk-ant-...): ")
		if err != nil {
			return err
		}
		if !strings.HasPrefix(strings.TrimSpace(key), "sk-ant-") {
			return fmt.Errorf("Anthropic API key must start with sk-ant-")
		}
		if _, err := client.UploadAccount(ctx, broker.AccountUpload{
			"provider": "anthropic-apikey",
			"label":    label,
			"apiKey":   strings.TrimSpace(key),
		}); err != nil {
			return err
		}
		fmt.Fprintf(r.out, "Added shared Anthropic API key: %s\n", label)
		restartInstalledDaemon(r.errOut)
		return nil
	}
	before, err := localAccountUploads(ctx, r.store)
	if err != nil {
		return err
	}
	beforeKeys := make(map[string]bool, len(before))
	for _, upload := range before {
		beforeKeys[sharedAccountKey(upload.kind, upload.label)] = true
	}
	switch args[0] {
	case "codex":
		if err := r.add(ctx); err != nil {
			return err
		}
	case "claude":
		name := ""
		if len(args) > 1 {
			name = args[1]
		}
		claudeArgs := []string{"add"}
		if name != "" {
			claudeArgs = append(claudeArgs, name)
		}
		if err := r.claude(ctx, claudeArgs); err != nil {
			return err
		}
	case "openai-key":
		if err := r.addKey(); err != nil {
			return err
		}
	default:
		return fmt.Errorf(
			"unknown provider %q; use codex, claude, openai-key, or anthropic-key",
			args[0],
		)
	}
	after, err := localAccountUploads(ctx, r.store)
	if err != nil {
		return err
	}
	var added []localAccountUpload
	for _, upload := range after {
		if !beforeKeys[sharedAccountKey(upload.kind, upload.label)] {
			added = append(added, upload)
		}
	}
	if len(added) == 0 {
		return fmt.Errorf(
			"authentication completed but no new local credential was found; use 'sr account repair' for an existing shared account",
		)
	}
	if len(added) != 1 {
		return fmt.Errorf(
			"authentication created %d credentials; upload one canary with 'sr account import --only <label>'",
			len(added),
		)
	}
	selector := added[0].kind + ":" + added[0].label
	return r.cloudAccountImport(ctx, client, []string{"--only", selector})
}

func (r srRunner) cloudAccountRepair(
	ctx context.Context,
	client *broker.Client,
	args []string,
) error {
	flags := flag.NewFlagSet("account repair", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 1 {
		return fmt.Errorf("usage: sr account repair <account-id>")
	}
	accountID := flags.Arg(0)
	shared, err := client.ListAccounts(ctx)
	if err != nil {
		return err
	}
	var target *broker.SharedAccount
	for i := range shared {
		if shared[i].ID == accountID {
			target = &shared[i]
			break
		}
	}
	if target == nil {
		return fmt.Errorf("shared account %q not found", accountID)
	}
	local, err := localAccountUploads(ctx, r.store)
	if err != nil {
		return err
	}
	var matches []localAccountUpload
	for _, candidate := range local {
		if sharedAccountKey(candidate.kind, candidate.label) ==
			sharedAccountKey(target.Kind, target.Label) {
			matches = append(matches, candidate)
		}
	}
	if len(matches) == 0 {
		return fmt.Errorf(
			"no matching local credential for %s; authenticate it locally first",
			target.Label,
		)
	}
	if len(matches) > 1 {
		return fmt.Errorf(
			"multiple local credentials match %s; remove the duplicate before repair",
			target.Label,
		)
	}
	if _, err := client.RepairAccount(
		ctx,
		accountID,
		matches[0].body,
	); err != nil {
		return err
	}
	fmt.Fprintf(r.out, "Repaired shared account %s (%s).\n", target.Label, accountID)
	restartInstalledDaemon(r.errOut)
	return nil
}

func (r srRunner) cloudAccountImport(
	ctx context.Context,
	client *broker.Client,
	args []string,
) error {
	flags := flag.NewFlagSet("account import", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	all := flags.Bool("all", false, "import every local Codex and Claude credential")
	only := flags.String("only", "", "import one local credential by label or kind:label")
	dryRun := flags.Bool("dry-run", false, "show what would be uploaded")
	yes := flags.Bool("yes", false, "confirm a bulk upload after reviewing a dry run")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *all == (strings.TrimSpace(*only) != "") {
		return fmt.Errorf(
			"usage: sr account import (--only <label|kind:label> | --all) [--dry-run] [--yes]",
		)
	}

	uploads, err := localAccountUploads(ctx, r.store)
	if err != nil {
		return err
	}
	if !*all {
		uploads, err = selectLocalAccountUpload(uploads, *only)
		if err != nil {
			return err
		}
	}
	existing, err := client.ListAccounts(ctx)
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, item := range existing {
		seen[sharedAccountKey(item.Kind, item.Label)] = true
	}
	pending := make([]localAccountUpload, 0, len(uploads))
	for _, upload := range uploads {
		if !seen[sharedAccountKey(upload.kind, upload.label)] {
			pending = append(pending, upload)
		}
	}
	sort.Slice(pending, func(i, j int) bool {
		if pending[i].kind == pending[j].kind {
			return pending[i].label < pending[j].label
		}
		return pending[i].kind < pending[j].kind
	})
	if len(pending) == 0 {
		fmt.Fprintln(r.out, "All local accounts are already shared.")
		return nil
	}
	for _, upload := range pending {
		fmt.Fprintf(r.out, "%-20s %s\n", upload.kind, upload.label)
	}
	if *dryRun {
		fmt.Fprintf(r.out, "\n%d account(s) would be uploaded.\n", len(pending))
		return nil
	}
	if *all && !*yes {
		return fmt.Errorf(
			"bulk import requires --yes after reviewing 'sr account import --all --dry-run'",
		)
	}
	uploaded := 0
	for _, upload := range pending {
		if _, err := client.UploadAccount(ctx, upload.body); err != nil {
			return fmt.Errorf("upload %s %s: %w", upload.kind, upload.label, err)
		}
		uploaded++
		fmt.Fprintf(r.out, "uploaded %-20s %s\n", upload.kind, upload.label)
	}
	fmt.Fprintf(
		r.out,
		"\nUploaded %d shared account(s). Central refresh adoption may rotate OAuth chains; local files remain only as migration records and may require re-authentication for rollback.\n",
		uploaded,
	)
	restartInstalledDaemon(r.errOut)
	return nil
}

type localAccountUpload struct {
	kind  string
	label string
	body  broker.AccountUpload
}

func selectLocalAccountUpload(
	uploads []localAccountUpload,
	selector string,
) ([]localAccountUpload, error) {
	selector = strings.ToLower(strings.TrimSpace(selector))
	if selector == "" {
		return nil, fmt.Errorf("account selector cannot be empty")
	}
	var exact []localAccountUpload
	var partial []localAccountUpload
	for _, upload := range uploads {
		label := strings.ToLower(strings.TrimSpace(upload.label))
		composite := strings.ToLower(strings.TrimSpace(upload.kind)) + ":" + label
		switch {
		case selector == label || selector == composite:
			exact = append(exact, upload)
		case strings.Contains(label, selector) || strings.Contains(composite, selector):
			partial = append(partial, upload)
		}
	}
	matches := exact
	if len(matches) == 0 {
		matches = partial
	}
	if len(matches) == 0 {
		return nil, fmt.Errorf("local account %q not found", selector)
	}
	if len(matches) != 1 {
		return nil, fmt.Errorf(
			"local account %q is ambiguous; use kind:label to select exactly one credential",
			selector,
		)
	}
	return matches, nil
}

func localAccountUploads(
	ctx context.Context,
	codexStore accounts.CodexStore,
) ([]localAccountUpload, error) {
	var out []localAccountUpload
	stored, err := codexStore.ListStored()
	if err != nil {
		return nil, err
	}
	for _, item := range stored {
		label := item.Email
		if item.IsAPIKey() {
			if item.Auth.OpenAIAPIKey == "" {
				continue
			}
			out = append(out, localAccountUpload{
				kind:  "openai-apikey",
				label: label,
				body: broker.AccountUpload{
					"provider": "openai-apikey",
					"label":    label,
					"apiKey":   item.Auth.OpenAIAPIKey,
				},
			})
			continue
		}
		if item.Auth.Tokens == nil ||
			item.Auth.Tokens.AccessToken == "" ||
			item.Auth.Tokens.RefreshToken == "" ||
			item.Auth.Tokens.IDToken == "" {
			continue
		}
		out = append(out, localAccountUpload{
			kind:  "codex",
			label: label,
			body: broker.AccountUpload{
				"provider": "codex",
				"label":    label,
				"tokens": map[string]any{
					"accessToken":  item.Auth.Tokens.AccessToken,
					"refreshToken": item.Auth.Tokens.RefreshToken,
					"idToken":      item.Auth.Tokens.IDToken,
					"accountID":    item.Auth.Tokens.AccountID,
				},
			},
		})
	}

	claudeStore := agentclaude.DefaultStore()
	seenClaude := map[string]bool{}
	for _, profile := range claudeStore.ListProfiles() {
		credential, err := claudeStore.ReadCredential(
			ctx,
			claudeStore.ClaudeConfigDir(profile.Name),
		)
		if err != nil || credential == nil {
			continue
		}
		if upload, ok := claudeAccountUpload(profile.Name, credential); ok {
			out = append(out, upload)
			seenClaude[credential.RefreshToken] = true
		}
	}
	home, err := os.UserHomeDir()
	if err == nil {
		credential, readErr := claudeStore.ReadCredential(ctx, filepath.Join(home, ".claude"))
		if readErr == nil && credential != nil && !seenClaude[credential.RefreshToken] {
			if upload, ok := claudeAccountUpload("default", credential); ok {
				out = append(out, upload)
			}
		}
	}
	return out, nil
}

func claudeAccountUpload(
	label string,
	credential *agentclaude.CredentialInfo,
) (localAccountUpload, bool) {
	if credential.AccessToken == "" || credential.RefreshToken == "" {
		return localAccountUpload{}, false
	}
	expiresAt := credential.ExpiresAt
	if expiresAt <= 0 {
		// Unknown expiry is treated as immediately stale so the central broker
		// refreshes before issuing its first lease.
		expiresAt = time.Now().UnixMilli()
	}
	return localAccountUpload{
		kind:  "claude",
		label: label,
		body: broker.AccountUpload{
			"provider": "claude",
			"label":    label,
			"claudeAiOauth": map[string]any{
				"accessToken":      credential.AccessToken,
				"refreshToken":     credential.RefreshToken,
				"expiresAt":        expiresAt,
				"subscriptionType": credential.SubscriptionType,
				"rateLimitTier":    credential.RateLimitTier,
			},
		},
	}, true
}

func sharedAccountKey(kind, label string) string {
	return strings.ToLower(strings.TrimSpace(kind)) + "\x00" +
		strings.ToLower(strings.TrimSpace(label))
}

func loadCloudClient(requireTeam bool) (broker.Config, string, *broker.Client, error) {
	path, err := broker.DefaultConfigPath()
	if err != nil {
		return broker.Config{}, "", nil, err
	}
	config, err := broker.LoadConfig(path)
	if err != nil {
		return broker.Config{}, "", nil, err
	}
	if !config.LoggedIn() {
		return broker.Config{}, "", nil, fmt.Errorf("not logged in; run 'sr login'")
	}
	if requireTeam && config.TeamID == "" {
		return broker.Config{}, "", nil, fmt.Errorf("no team selected; run 'sr team use <team>'")
	}
	return config, path, broker.NewClient(config), nil
}

func matchCloudTeam(teams []broker.Team, selector string) (broker.Team, error) {
	selector = strings.TrimSpace(selector)
	if selector == "" {
		if len(teams) == 1 {
			return teams[0], nil
		}
		return broker.Team{}, fmt.Errorf("choose a team with 'sr team use <team>'")
	}
	lower := strings.ToLower(selector)
	var matches []broker.Team
	for _, team := range teams {
		if team.ID == selector || strings.EqualFold(team.Name, selector) {
			return team, nil
		}
		if strings.Contains(strings.ToLower(team.Name), lower) ||
			strings.Contains(strings.ToLower(team.ID), lower) {
			matches = append(matches, team)
		}
	}
	if len(matches) == 1 {
		return matches[0], nil
	}
	if len(matches) == 0 {
		return broker.Team{}, fmt.Errorf("team %q not found", selector)
	}
	return broker.Team{}, fmt.Errorf("team %q is ambiguous", selector)
}

func openBrowser(target string) {
	var command *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		command = exec.Command("open", target)
	case "linux":
		command = exec.Command("xdg-open", target)
	default:
		return
	}
	_ = command.Start()
}

func restartInstalledDaemon(warn io.Writer) {
	controller, err := newServiceController()
	if err != nil || !controller.installed() {
		return
	}
	if err := controller.restart(); err != nil && warn != nil {
		fmt.Fprintf(warn, "warning: restart local daemon: %v\n", err)
	}
}
