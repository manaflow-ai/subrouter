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
	config, recovered, err := loadCloudConfigForLogin(path)
	if err != nil {
		return err
	}
	if recovered {
		fmt.Fprintf(
			r.errOut,
			"warning: replacing an unreadable cmux.com config after login succeeds\n",
		)
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
				if poll.Client != "subrouter" {
					return fmt.Errorf(
						"cmux.com approved login for unexpected client %q",
						poll.Client,
					)
				}
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
	config.CredentialSource = broker.CredentialSourceTeam
	if err := broker.SaveConfig(path, config); err != nil {
		return err
	}
	fmt.Fprintf(r.out, "Logged in to cmux.com team %s (%s).\n", team.Name, team.ID)
	return restartInstalledDaemon()
}

func cloudModeConfig() (broker.Config, error) {
	path, err := broker.DefaultConfigPath()
	if err != nil {
		return broker.Config{}, err
	}
	return broker.LoadConfig(path)
}

func loadCloudConfigForLogin(path string) (broker.Config, bool, error) {
	config, err := broker.LoadConfig(path)
	if err == nil {
		return config, false, nil
	}
	info, statErr := os.Stat(path)
	if statErr != nil {
		return broker.Config{}, false, err
	}
	if !info.Mode().IsRegular() {
		return broker.Config{}, false, err
	}
	return broker.Config{
		Version: 1,
		BaseURL: broker.DefaultBaseURL,
	}, true, nil
}

func cloudServerProxyToken(config broker.Config) string {
	if !config.TeamModeReady() {
		return ""
	}
	return strings.TrimSpace(config.LocalProxyToken)
}

func cloudClientProxyToken(config broker.Config, targetBaseURL string) string {
	if !config.TeamModeReady() ||
		!loopbackEndpoint(targetBaseURL) ||
		!sameEndpoint(targetBaseURL, localBaseURL()) {
		return ""
	}
	return strings.TrimSpace(config.LocalProxyToken)
}

func (r srRunner) cloudSetup(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("setup", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	noLogin := flags.Bool("no-login", false, "install the daemon without signing in to cmux.com")
	noInstall := flags.Bool("no-install", false, "start and verify the existing daemon")
	storage := flags.String("storage", "", "credential storage: team or local")
	if err := flags.Parse(args); err != nil {
		return err
	}

	path, err := broker.DefaultConfigPath()
	if err != nil {
		return err
	}
	config, recovered, err := loadCloudConfigForLogin(path)
	if err != nil {
		return err
	}
	if recovered {
		fmt.Fprintln(
			r.errOut,
			"warning: replacing an unreadable cmux.com config during setup",
		)
	}
	source := config.EffectiveCredentialSource()
	if strings.TrimSpace(*storage) != "" {
		source, err = parseCredentialSource(*storage, false)
		if err != nil {
			return err
		}
	} else if *noLogin && config.CredentialSource == "" && !config.Ready() {
		source = broker.CredentialSourceLocal
	} else if config.CredentialSource == "" && !config.Ready() {
		source = broker.CredentialSourceTeam
	}

	if source == broker.CredentialSourceTeam && !*noLogin && !config.LoggedIn() {
		fmt.Fprintln(r.out, "First, sign in to cmux.com.")
		if err := r.cloudLogin(ctx, nil); err != nil {
			return err
		}
		config, err = cloudModeConfig()
		if err != nil {
			return err
		}
	}
	if source == broker.CredentialSourceTeam && !*noLogin && config.LoggedIn() && config.TeamID == "" {
		return fmt.Errorf("no team selected; run 'sr team use <team>'")
	}
	if source == broker.CredentialSourceTeam && !config.Ready() {
		return fmt.Errorf("team credential storage requires login and a selected team; run 'sr login'")
	}
	config.CredentialSource = source
	if err := broker.SaveConfig(path, config); err != nil {
		return err
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
	config, recovered, err := loadCloudConfigForLogin(path)
	if err != nil {
		return err
	}
	if recovered {
		fmt.Fprintln(
			r.errOut,
			"warning: the unreadable cmux.com session could not be revoked and will be replaced locally",
		)
	}
	if config.LoggedIn() {
		if err := broker.NewClient(config).Logout(ctx); err != nil {
			return fmt.Errorf(
				"could not revoke the cmux.com session; local credentials were kept so logout can be retried: %w",
				err,
			)
		}
	}
	config.AccessToken = ""
	config.RefreshToken = ""
	config.TeamID = ""
	config.TeamName = ""
	config.CredentialSource = broker.CredentialSourceLocal
	if err := broker.SaveConfig(path, config); err != nil {
		return err
	}
	fmt.Fprintln(r.out, "Logged out of cmux.com. Credential storage is now local.")
	return restartInstalledDaemon()
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
		config.CredentialSource = broker.CredentialSourceTeam
		if err := broker.SaveConfig(path, config); err != nil {
			return err
		}
		fmt.Fprintf(r.out, "Using %s (%s).\n", team.Name, team.ID)
		return restartInstalledDaemon()
	default:
		return fmt.Errorf("unknown team command %q; use list, current, or use", args[0])
	}
}

func parseCredentialSource(raw string, allowLegacy bool) (broker.CredentialSource, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "team", "shared", "cloud":
		return broker.CredentialSourceTeam, nil
	case "local", "device", "machine":
		return broker.CredentialSourceLocal, nil
	case "legacy", "server", "remote":
		if allowLegacy {
			return broker.CredentialSourceLegacy, nil
		}
	}
	if allowLegacy {
		return "", fmt.Errorf("credential storage must be team, local, or legacy")
	}
	return "", fmt.Errorf("credential storage must be team or local")
}

func (r srRunner) cloudStorage(args []string) error {
	if len(args) > 0 && args[0] == "use" {
		args = args[1:]
	}
	if len(args) == 0 || args[0] == "current" || args[0] == "status" {
		config, err := cloudModeConfig()
		if err != nil {
			return err
		}
		return r.printCredentialSource(config)
	}
	if len(args) != 1 {
		return fmt.Errorf("usage: sr storage [team|local|legacy]")
	}
	source, err := parseCredentialSource(args[0], true)
	if err != nil {
		return err
	}
	path, err := broker.DefaultConfigPath()
	if err != nil {
		return err
	}
	config, err := cloudModeConfig()
	if err != nil {
		if source == broker.CredentialSourceTeam {
			return err
		}
		var recovered bool
		config, recovered, err = loadCloudConfigForLogin(path)
		if err != nil {
			return err
		}
		if recovered {
			fmt.Fprintln(
				r.errOut,
				"warning: replacing an unreadable cmux.com config with local credential storage",
			)
		}
	}
	if source == broker.CredentialSourceTeam && !config.Ready() {
		return fmt.Errorf("team credential storage requires login and a selected team; run 'sr login'")
	}
	config.CredentialSource = source
	if err := broker.SaveConfig(path, config); err != nil {
		return err
	}
	if err := r.printCredentialSource(config); err != nil {
		return err
	}
	return restartInstalledDaemon()
}

func (r srRunner) printCredentialSource(config broker.Config) error {
	switch config.EffectiveCredentialSource() {
	case broker.CredentialSourceTeam:
		if !config.Ready() {
			return fmt.Errorf("team credential storage requires login and a selected team; run 'sr login'")
		}
		fmt.Fprintf(
			r.out,
			"Credential storage: team (%s, %s)\n",
			config.TeamName,
			config.TeamID,
		)
	case broker.CredentialSourceLocal:
		fmt.Fprintf(r.out, "Credential storage: local (%s)\n", r.store.StoreDir())
	case broker.CredentialSourceLegacy:
		fmt.Fprintln(r.out, "Credential storage: legacy remote server")
	default:
		return fmt.Errorf("unknown credential storage %q", config.CredentialSource)
	}
	return nil
}

func (r srRunner) cloudStatus(ctx context.Context) error {
	config, _, client, err := loadCloudClient(true)
	if err != nil {
		return err
	}
	if !config.TeamModeReady() {
		return fmt.Errorf("credential storage is %s; run 'sr storage team' to use the team vault", config.EffectiveCredentialSource())
	}
	if err := r.printCredentialSource(config); err != nil {
		return err
	}
	items, err := client.ListAccounts(ctx)
	if err != nil {
		return err
	}
	if len(items) == 0 {
		fmt.Fprintln(r.out, "No shared accounts.")
		return nil
	}
	fmt.Fprintln(r.out, "\nShared accounts")
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
		key, err := promptSecret(
			r.out,
			reader,
			r.in,
			"Anthropic API key (sk-ant-...): ",
		)
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
		return restartInstalledDaemon()
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
	replacement, err := replacementUploadForSharedAccount(
		target,
		matches,
		r.in,
		r.out,
	)
	if err != nil {
		return err
	}
	if _, err := client.RepairAccount(
		ctx,
		accountID,
		replacement,
	); err != nil {
		return err
	}
	fmt.Fprintf(r.out, "Repaired shared account %s (%s).\n", target.Label, accountID)
	return restartInstalledDaemon()
}

func replacementUploadForSharedAccount(
	target *broker.SharedAccount,
	matches []localAccountUpload,
	in io.Reader,
	out io.Writer,
) (broker.AccountUpload, error) {
	if len(matches) > 1 {
		return nil, fmt.Errorf(
			"multiple local credentials match %s; remove the duplicate before repair",
			target.Label,
		)
	}
	if len(matches) == 1 {
		return matches[0].body, nil
	}

	prefix := ""
	switch target.Kind {
	case "anthropic-apikey":
		prefix = "sk-ant-"
	case "openai-apikey":
		prefix = "sk-"
	default:
		return nil, fmt.Errorf(
			"no matching local credential for %s; authenticate it locally first",
			target.Label,
		)
	}
	reader := bufio.NewReader(in)
	key, err := promptSecret(
		out,
		reader,
		in,
		fmt.Sprintf("Replacement API key for %s: ", target.Label),
	)
	if err != nil {
		return nil, err
	}
	key = strings.TrimSpace(key)
	if !strings.HasPrefix(key, prefix) {
		return nil, fmt.Errorf("%s API key must start with %s", target.Kind, prefix)
	}
	return broker.AccountUpload{
		"provider": target.Kind,
		"label":    target.Label,
		"apiKey":   key,
	}, nil
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
	return restartInstalledDaemon()
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
			var cloudProvider string
			switch item.ProviderOrDefault() {
			case accounts.ProviderCodex:
				cloudProvider = "openai-apikey"
			case accounts.ProviderClaude:
				cloudProvider = "anthropic-apikey"
			default:
				// The team vault does not proxy Kimi or Z.AI yet. Keeping
				// those credentials local is safer than mislabeling them.
				continue
			}
			out = append(out, localAccountUpload{
				kind:  cloudProvider,
				label: label,
				body: broker.AccountUpload{
					"provider": cloudProvider,
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
		var usable []broker.Team
		for _, team := range teams {
			if team.Use {
				usable = append(usable, team)
			}
		}
		if len(usable) == 1 {
			return usable[0], nil
		}
		return broker.Team{}, fmt.Errorf("choose a team with 'sr team use <team>'")
	}
	for _, team := range teams {
		if team.ID != selector {
			continue
		}
		if !team.Use {
			return broker.Team{}, fmt.Errorf(
				"team %q does not grant Subrouter use permission",
				selector,
			)
		}
		return team, nil
	}
	lower := strings.ToLower(selector)
	var exactNames, partial []broker.Team
	matchedWithoutPermission := false
	for _, team := range teams {
		exactName := strings.EqualFold(team.Name, selector)
		contains := strings.Contains(strings.ToLower(team.Name), lower) ||
			strings.Contains(strings.ToLower(team.ID), lower)
		if !team.Use {
			matchedWithoutPermission = matchedWithoutPermission ||
				exactName ||
				contains
			continue
		}
		if exactName {
			exactNames = append(exactNames, team)
		} else if contains {
			partial = append(partial, team)
		}
	}
	matches := exactNames
	if len(matches) == 0 {
		matches = partial
	}
	if len(matches) == 1 {
		return matches[0], nil
	}
	if len(matches) == 0 {
		if matchedWithoutPermission {
			return broker.Team{}, fmt.Errorf(
				"team %q does not grant Subrouter use permission",
				selector,
			)
		}
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

func restartInstalledDaemon() error {
	controller, err := newServiceController()
	return restartInstalledDaemonWith(controller, err)
}

func restartInstalledDaemonWith(
	controller serviceController,
	controllerErr error,
) error {
	if controllerErr != nil || controller == nil || !controller.installed() {
		return nil
	}
	if err := controller.restart(); err != nil {
		return fmt.Errorf("restart local daemon: %w", err)
	}
	return nil
}
