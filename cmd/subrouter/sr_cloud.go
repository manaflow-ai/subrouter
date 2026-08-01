package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
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
	"github.com/manaflow-ai/subrouter/internal/stackauth"
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
	httpClient := r.client
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	publicConfig, err := stackauth.FetchPublicConfig(ctx, httpClient, config.BaseURL)
	if err != nil {
		return fmt.Errorf("load hosted login configuration: %w", err)
	}
	stackClient := stackauth.Client{
		APIURL: publicConfig.Auth.APIURL, ProjectID: publicConfig.Auth.ProjectID,
		PublishableClientKey: publicConfig.Auth.PublishableClientKey,
		HTTPClient:           httpClient,
	}
	start, err := stackClient.StartCLI(ctx, 15*time.Minute)
	if err != nil {
		return fmt.Errorf("start cmux.com login: %w", err)
	}

	verificationURL, err := cliVerificationURL(publicConfig.Auth.ConfirmURL, start.LoginCode)
	if err != nil {
		return err
	}
	fmt.Fprintf(r.out, "Approve Subrouter at:\n  %s\n", verificationURL)
	if !*noBrowser {
		openBrowser(verificationURL)
	}
	expires := time.Until(start.ExpiresAt)
	if expires <= 0 {
		return fmt.Errorf("login expired before approval")
	}
	deadline := time.NewTimer(expires)
	defer deadline.Stop()

	var refreshToken string
	for {
		poll, pollErr := stackClient.PollCLI(ctx, start.PollingCode)
		if pollErr != nil && !stackauth.Retryable(pollErr) {
			return fmt.Errorf("poll cmux.com login: %w", pollErr)
		}
		if pollErr == nil {
			switch poll.Status {
			case "waiting":
			case "success":
				if poll.RefreshToken == "" {
					return fmt.Errorf("cmux.com approved login without a refresh token")
				}
				refreshToken = poll.RefreshToken
			case "expired", "used":
				return fmt.Errorf("login %s", poll.Status)
			default:
				return fmt.Errorf("login returned unexpected status %q", poll.Status)
			}
		}
		if refreshToken != "" {
			break
		}
		timer := time.NewTimer(2 * time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-deadline.C:
			timer.Stop()
			return fmt.Errorf("login expired before approval")
		case <-timer.C:
		}
	}

	tokens, err := stackClient.Refresh(ctx, refreshToken)
	if err != nil {
		return fmt.Errorf("open Stack session: %w", err)
	}
	claims, err := stackauth.ParseClaimsUnverified(tokens.AccessToken)
	if err != nil {
		return err
	}
	stackTeams, err := stackClient.ListTeams(ctx, tokens.AccessToken)
	if err != nil {
		return fmt.Errorf("list Stack teams: %w", err)
	}
	selector := strings.TrimSpace(*teamSelector)
	if selector == "" {
		selector = claims.SelectedTeamID
	}
	team, err := matchNativeStackTeam(stackTeams, selector)
	if err != nil {
		return err
	}
	if claims.SelectedTeamID != team.ID {
		if err := stackClient.SelectTeam(ctx, tokens.AccessToken, team.ID); err != nil {
			return fmt.Errorf("select Stack team: %w", err)
		}
		tokens, err = stackClient.Refresh(ctx, tokens.RefreshToken)
		if err != nil {
			return fmt.Errorf("refresh selected Stack team: %w", err)
		}
		claims, err = stackauth.ParseClaimsUnverified(tokens.AccessToken)
		if err != nil {
			return err
		}
		if claims.SelectedTeamID != team.ID {
			return fmt.Errorf("Stack Auth did not select team %s", team.ID)
		}
	}
	exchange, err := stackauth.ExchangeTenant(
		ctx,
		httpClient,
		publicConfig.Subrouter.URL,
		tokens.AccessToken,
		team.ID,
		team.DisplayName,
	)
	if err != nil {
		return err
	}
	config.AccessToken = tokens.AccessToken
	config.RefreshToken = tokens.RefreshToken
	config.TeamID = team.ID
	config.TeamName = team.DisplayName
	config.CredentialSource = broker.CredentialSourceHosted
	config.HostedURL = publicConfig.Subrouter.URL
	config.TenantKey = exchange.TenantKey
	config.StackAPIURL = publicConfig.Auth.APIURL
	config.StackProjectID = publicConfig.Auth.ProjectID
	config.StackPublishable = publicConfig.Auth.PublishableClientKey
	if err := broker.SaveConfig(path, config); err != nil {
		return err
	}
	if err := r.configureHostedCMUX(config); err != nil {
		return err
	}
	fmt.Fprintf(r.out, "Logged in to cmux.com team %s (%s).\n", team.DisplayName, team.ID)
	fmt.Fprintln(r.out, "Remote: cmux (hosted)")
	return nil
}

func cliVerificationURL(baseURL, loginCode string) (string, error) {
	parsed, err := url.Parse(baseURL)
	if err != nil {
		return "", fmt.Errorf("invalid CLI confirmation URL: %w", err)
	}
	query := parsed.Query()
	query.Set("login_code", loginCode)
	parsed.RawQuery = query.Encode()
	return parsed.String(), nil
}

func matchNativeStackTeam(teams []stackauth.Team, selector string) (stackauth.Team, error) {
	if len(teams) == 0 {
		return stackauth.Team{}, fmt.Errorf("your Stack account has no teams")
	}
	selector = strings.TrimSpace(selector)
	if selector == "" {
		if len(teams) == 1 {
			return teams[0], nil
		}
		available := make([]string, 0, len(teams))
		for _, team := range teams {
			available = append(
				available,
				fmt.Sprintf("%s (%s)", team.DisplayName, team.ID),
			)
		}
		sort.Strings(available)
		return stackauth.Team{}, fmt.Errorf(
			"multiple Stack teams are available: %s; rerun 'sr login --team <id-or-name>'",
			strings.Join(available, ", "),
		)
	}
	for _, team := range teams {
		if team.ID == selector || strings.EqualFold(team.DisplayName, selector) {
			return team, nil
		}
	}
	lower := strings.ToLower(selector)
	var matches []stackauth.Team
	for _, team := range teams {
		if strings.Contains(strings.ToLower(team.ID), lower) ||
			strings.Contains(strings.ToLower(team.DisplayName), lower) {
			matches = append(matches, team)
		}
	}
	if len(matches) == 1 {
		return matches[0], nil
	}
	if len(matches) == 0 {
		return stackauth.Team{}, fmt.Errorf("team %q not found", selector)
	}
	return stackauth.Team{}, fmt.Errorf("team %q is ambiguous", selector)
}

func (r srRunner) configureHostedCMUX(config broker.Config) error {
	store := defaultSRServerStore(r.store)
	file, err := store.load()
	if err != nil {
		return err
	}
	hosted := srServerConfig{
		Name: "cmux", URL: strings.TrimRight(config.HostedURL, "/"),
		TenantKey: config.TenantKey,
	}
	replaced := false
	for i := range file.Servers {
		if file.Servers[i].Name == hosted.Name {
			file.Servers[i] = hosted
			replaced = true
			break
		}
	}
	if !replaced {
		file.Servers = append(file.Servers, hosted)
	}
	file.Default = hosted.Name
	if err := store.save(file); err != nil {
		return err
	}
	path, err := writeCodexConfigForServer(hosted)
	if err != nil {
		return err
	}
	fmt.Fprintf(r.out, "Codex config: %s\n", path)
	return nil
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
	source := config.EffectiveCredentialSource()
	if source != broker.CredentialSourceLocal && !config.TeamModeReady() {
		return ""
	}
	return strings.TrimSpace(config.LocalProxyToken)
}

func cloudClientProxyToken(config broker.Config, targetBaseURL string) string {
	source := config.EffectiveCredentialSource()
	if (source != broker.CredentialSourceLocal && !config.TeamModeReady()) ||
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
	storage := flags.String("storage", "", "credential storage: hosted or local")
	// Forwarded to runSetup, which owns the review screen. Declared here too
	// because this flag set parses first and rejects anything it does not know.
	planOnly := flags.Bool("plan", false, "print the change set and exit without modifying anything")
	assumeYes := flags.Bool("yes", false, "apply the plan without the review screen")
	noBackground := flags.Bool("no-background", false, "do not start Subrouter after login")
	noConfig := flags.Bool("no-config", false, "do not configure Codex or Claude Code")
	if err := flags.Parse(args); err != nil {
		return err
	}
	forwarded := []string{}
	for flagName, enabled := range map[string]*bool{
		"--plan": planOnly, "--yes": assumeYes,
		"--no-background": noBackground, "--no-config": noConfig,
	} {
		if *enabled {
			forwarded = append(forwarded, flagName)
		}
	}
	sort.Strings(forwarded)
	// `--plan` promises to change nothing, so it must not fall through to the
	// login and storage writes below.
	if *planOnly {
		return runSetup(ctx, r.store, forwarded, r.out)
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
		source = broker.CredentialSourceHosted
	}

	if source == broker.CredentialSourceHosted && !*noLogin && !config.HostedReady() {
		fmt.Fprintln(r.out, "First, sign in to cmux.com.")
		if err := r.cloudLogin(ctx, nil); err != nil {
			return err
		}
		config, err = cloudModeConfig()
		if err != nil {
			return err
		}
	}
	if source == broker.CredentialSourceHosted && !config.HostedReady() {
		return fmt.Errorf("hosted cmux requires login and a selected team; run 'sr login'")
	}
	config.CredentialSource = source
	if err := broker.SaveConfig(path, config); err != nil {
		return err
	}
	setupArgs := append([]string{}, forwarded...)
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
		if config.StackProjectID != "" && config.StackPublishable != "" {
			client := nativeStackClient(config, r.client)
			if err := client.SignOut(ctx, config.AccessToken, config.RefreshToken); err != nil {
				return fmt.Errorf(
					"could not revoke the Stack session; local credentials were kept so logout can be retried: %w",
					err,
				)
			}
		} else {
			fmt.Fprintln(
				r.errOut,
				"warning: this legacy session cannot be revoked because its retired auth endpoint no longer exists",
			)
		}
	}
	config.AccessToken = ""
	config.RefreshToken = ""
	config.TeamID = ""
	config.TeamName = ""
	config.HostedURL = ""
	config.TenantKey = ""
	config.CredentialSource = broker.CredentialSourceLocal
	if err := broker.SaveConfig(path, config); err != nil {
		return err
	}
	if err := r.clearDefaultServer(defaultSRServerStore(r.store), true); err != nil {
		return fmt.Errorf(
			"logout incomplete: the cmux.com session was cleared, but the hosted remote and tenant key remain; rerun 'sr logout': %w",
			err,
		)
	}
	fmt.Fprintln(r.out, "Logged out of cmux.com. Credential storage is now local.")
	return nil
}

func (r srRunner) cloudTeam(ctx context.Context, args []string) error {
	if len(args) == 0 {
		args = []string{"current"}
	}
	config, path, _, err := loadCloudClient(false)
	if err != nil {
		return err
	}
	if args[0] == "current" {
		if config.TeamID == "" {
			return fmt.Errorf("no team selected; run 'sr team list' then 'sr team use <team>'")
		}
		fmt.Fprintf(r.out, "%s (%s)\n", config.TeamName, config.TeamID)
		return nil
	}
	if config.StackProjectID == "" || config.StackPublishable == "" {
		return fmt.Errorf("this login predates native Stack Auth; run 'sr login' again")
	}
	client := nativeStackClient(config, r.client)
	tokens, err := client.Refresh(ctx, config.RefreshToken)
	if err != nil {
		return fmt.Errorf("refresh Stack session: %w", err)
	}
	config.AccessToken = tokens.AccessToken
	config.RefreshToken = tokens.RefreshToken
	if err := broker.SaveConfig(path, config); err != nil {
		return fmt.Errorf("persist refreshed Stack session: %w", err)
	}
	stackTeams, err := client.ListTeams(ctx, tokens.AccessToken)
	if err != nil {
		return err
	}
	switch args[0] {
	case "list", "ls":
		for _, team := range stackTeams {
			marker := " "
			if team.ID == config.TeamID {
				marker = "*"
			}
			fmt.Fprintf(r.out, "%s %-28s %s\n", marker, team.DisplayName, team.ID)
		}
		return nil
	case "use":
		if len(args) != 2 {
			return fmt.Errorf("usage: sr team use <team-id-or-name>")
		}
		team, err := matchNativeStackTeam(stackTeams, args[1])
		if err != nil {
			return err
		}
		if err := client.SelectTeam(ctx, tokens.AccessToken, team.ID); err != nil {
			return err
		}
		tokens, err = client.Refresh(ctx, tokens.RefreshToken)
		if err != nil {
			return err
		}
		exchange, err := stackauth.ExchangeTenant(
			ctx, r.client, config.HostedURL, tokens.AccessToken,
			team.ID, team.DisplayName,
		)
		if err != nil {
			return err
		}
		config.AccessToken = tokens.AccessToken
		config.RefreshToken = tokens.RefreshToken
		config.TeamID = team.ID
		config.TeamName = team.DisplayName
		config.TenantKey = exchange.TenantKey
		config.CredentialSource = broker.CredentialSourceHosted
		if err := broker.SaveConfig(path, config); err != nil {
			return err
		}
		if err := r.configureHostedCMUX(config); err != nil {
			return err
		}
		fmt.Fprintf(r.out, "Using %s (%s).\n", team.DisplayName, team.ID)
		return nil
	default:
		return fmt.Errorf("unknown team command %q; use list, current, or use", args[0])
	}
}

func nativeStackClient(config broker.Config, httpClient *http.Client) stackauth.Client {
	return stackauth.Client{
		APIURL: config.StackAPIURL, ProjectID: config.StackProjectID,
		PublishableClientKey: config.StackPublishable, HTTPClient: httpClient,
	}
}

func parseCredentialSource(raw string, allowLegacy bool) (broker.CredentialSource, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "hosted", "cmux", "team", "shared", "cloud":
		return broker.CredentialSourceHosted, nil
	case "local", "device", "machine":
		return broker.CredentialSourceLocal, nil
	case "legacy", "server", "remote":
		if allowLegacy {
			return broker.CredentialSourceLegacy, nil
		}
	}
	if allowLegacy {
		return "", fmt.Errorf("credential storage must be hosted, local, or legacy")
	}
	return "", fmt.Errorf("credential storage must be hosted or local")
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
		return fmt.Errorf("usage: sr storage [hosted|team|local|legacy]")
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
		if source == broker.CredentialSourceHosted {
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
	if source == broker.CredentialSourceHosted {
		candidate := config
		candidate.CredentialSource = source
		if !candidate.HostedReady() {
			return fmt.Errorf("hosted cmux requires login and a selected team; run 'sr login'")
		}
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
	case broker.CredentialSourceHosted:
		if !config.HostedReady() {
			return fmt.Errorf("hosted Subrouter requires login; run 'sr login'")
		}
		fmt.Fprintf(
			r.out,
			"Credential storage: hosted cmux (%s, %s)\n",
			config.TeamName,
			config.TeamID,
		)
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
	if !config.TeamModeReady() && !config.HostedReady() {
		return fmt.Errorf("credential storage is %s; run 'sr login' to use hosted cmux", config.EffectiveCredentialSource())
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
	if client.Config.HostedReady() {
		return r.hostedAccountAdd(ctx, client, args)
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

func (r srRunner) hostedAccountAdd(
	ctx context.Context,
	client *broker.Client,
	args []string,
) error {
	switch args[0] {
	case "codex":
		deviceAuth := false
		for _, arg := range args[1:] {
			if arg != "--device-auth" {
				return fmt.Errorf("usage: sr add codex [--device-auth]")
			}
			deviceAuth = true
		}
		return r.hostedCodexAdd(ctx, client, deviceAuth)
	case "claude":
		if len(args) > 2 {
			return fmt.Errorf("usage: sr add claude [name]")
		}
		name := ""
		if len(args) == 2 {
			name = strings.TrimSpace(args[1])
		}
		return r.hostedClaudeAdd(ctx, client, name)
	case "openai-key", "anthropic-key":
		if len(args) != 1 {
			return fmt.Errorf("usage: sr add %s", args[0])
		}
		return r.hostedAPIKeyAdd(ctx, client, args[0])
	default:
		return fmt.Errorf(
			"unknown provider %q; use codex, claude, openai-key, or anthropic-key",
			args[0],
		)
	}
}

func (r srRunner) hostedCodexAdd(
	ctx context.Context,
	client *broker.Client,
	deviceAuth bool,
) error {
	lock, err := accounts.AcquireActiveCodexAuthLock(func() {
		fmt.Fprintln(r.out, "Another sr add/login is in progress; waiting...")
	})
	if err != nil {
		return fmt.Errorf("lock hosted Codex login: %w", err)
	}
	defer func() { _ = lock.Close() }()

	loginHome, err := os.MkdirTemp("", "sr-hosted-codex-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(loginHome)

	loginArgs := []string{"login"}
	if deviceAuth {
		loginArgs = append(loginArgs, "--device-auth")
	}
	fmt.Fprintln(r.out, "Opening Codex OAuth login for hosted cmux...")
	if err := r.commandRunner().RunWithEnv(
		ctx,
		"codex",
		loginArgs,
		[]string{"CODEX_HOME=" + loginHome},
		r.in,
		r.out,
		r.errOut,
	); err != nil {
		return fmt.Errorf("codex login failed: %w", err)
	}
	auth, ok, err := accounts.ReadCodexAuthFile(filepath.Join(loginHome, "auth.json"))
	if err != nil {
		return err
	}
	if !ok || auth.Tokens == nil || auth.Tokens.AccessToken == "" ||
		auth.Tokens.RefreshToken == "" || auth.Tokens.IDToken == "" {
		return fmt.Errorf("codex login did not write complete OAuth auth")
	}
	email, err := accounts.ExtractEmailFromJWT(auth.Tokens.IDToken)
	if err != nil || strings.TrimSpace(email) == "" {
		return fmt.Errorf("could not extract email from logged-in auth")
	}
	if err := lock.Close(); err != nil {
		return err
	}
	if _, err := client.UploadAccount(ctx, broker.AccountUpload{
		"provider": "codex",
		"label":    email,
		"tokens": map[string]any{
			"accessToken":  auth.Tokens.AccessToken,
			"refreshToken": auth.Tokens.RefreshToken,
			"idToken":      auth.Tokens.IDToken,
			"accountID":    auth.Tokens.AccountID,
		},
	}); err != nil {
		return err
	}
	fmt.Fprintf(r.out, "Added Codex account %s to hosted cmux.\n", email)
	fmt.Fprintln(r.out, "Local Codex auth was left unchanged.")
	return nil
}

func (r srRunner) hostedClaudeAdd(
	ctx context.Context,
	client *broker.Client,
	name string,
) error {
	root, err := os.MkdirTemp("", "sr-hosted-claude-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(root)
	store := agentclaude.Store{Dir: filepath.Join(root, "store")}
	runner := claudeRunner{
		store:     store,
		in:        r.in,
		out:       r.out,
		errOut:    r.errOut,
		client:    r.client,
		ephemeral: true,
		pushAfterAdd: func(ctx context.Context, profileName string) error {
			profile, ok := store.FindProfile(profileName)
			if !ok {
				return fmt.Errorf("temporary Claude profile %q was not found", profileName)
			}
			credential, err := store.ReadCredential(ctx, store.ClaudeConfigDir(profile.Name))
			if err != nil {
				return err
			}
			if credential == nil {
				return fmt.Errorf("Claude login wrote no credential")
			}
			upload, ok := claudeAccountUpload(profileName, credential)
			if !ok {
				return fmt.Errorf("Claude login wrote an incomplete credential")
			}
			_, err = client.UploadAccount(ctx, upload.body)
			return err
		},
	}
	return runner.add(ctx, name)
}

func (r srRunner) hostedAPIKeyAdd(
	ctx context.Context,
	client *broker.Client,
	provider string,
) error {
	reader := bufio.NewReader(r.in)
	label, err := promptLine(r.out, reader, "Label (e.g. work, personal): ")
	if err != nil {
		return err
	}
	prompt := "OpenAI API key (sk-...): "
	prefix := "sk-"
	wireProvider := "openai-apikey"
	displayProvider := "OpenAI"
	if provider == "anthropic-key" {
		prompt = "Anthropic API key (sk-ant-...): "
		prefix = "sk-ant-"
		wireProvider = "anthropic-apikey"
		displayProvider = "Anthropic"
	}
	key, err := promptSecret(r.out, reader, r.in, prompt)
	if err != nil {
		return err
	}
	key = strings.TrimSpace(key)
	if !strings.HasPrefix(key, prefix) {
		return fmt.Errorf("%s API key must start with %s", displayProvider, prefix)
	}
	if _, err := client.UploadAccount(ctx, broker.AccountUpload{
		"provider": wireProvider,
		"label":    label,
		"apiKey":   key,
	}); err != nil {
		return err
	}
	fmt.Fprintf(r.out, "Added %s API key %s to hosted cmux.\n", displayProvider, label)
	return nil
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
	migrated := 0
	for _, upload := range pending {
		if _, err := client.UploadAccount(ctx, upload.body); err != nil {
			return fmt.Errorf("upload %s %s: %w", upload.kind, upload.label, err)
		}
		uploaded++
		fmt.Fprintf(r.out, "uploaded %-20s %s\n", upload.kind, upload.label)
		// Hand over ownership as well as the bytes. The provider rotates the
		// refresh token on every use, so leaving a refreshable local copy makes
		// this machine and the vault invalidate each other's chain. The record
		// is kept for rollback, just where the daemon will not refresh it.
		if upload.kind != "codex" {
			if upload.kind == "claude" {
				if err := r.routeClaudeProfileThroughHosted(upload.label); err != nil {
					return fmt.Errorf("Claude credential uploaded, but local proxy routing failed: %w", err)
				}
			}
			continue
		}
		path, ok, err := r.store.MigrateStoredAway(upload.label)
		if err != nil {
			return fmt.Errorf("hand over %s: %w", upload.label, err)
		}
		if ok {
			migrated++
			fmt.Fprintf(r.out, "  local copy kept as a record at %s\n", path)
		}
	}
	fmt.Fprintf(
		r.out,
		"\nUploaded %d shared account(s); %d local credential(s) handed over. This machine no longer refreshes them, so the vault owns their token chains. Rollback records remain in %s.\n",
		uploaded,
		migrated,
		filepath.Join(r.store.StoreDir(), accounts.MigratedDirName),
	)
	return restartInstalledDaemon()
}

func (r srRunner) routeClaudeProfileThroughHosted(label string) error {
	server, ok, err := r.selectedRemoteServer()
	if err != nil {
		return err
	}
	if !ok || server.Name != "cmux" || strings.TrimSpace(server.TenantKey) == "" {
		return fmt.Errorf("hosted cmux remote is not selected")
	}
	store := agentclaude.DefaultStore()
	profile, ok, err := store.MatchProfile(label)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("Claude profile %q not found", label)
	}
	return writeClaudeProxyEnv(
		store.ClaudeConfigDir(profile.Name),
		serverProxyRootURL(server),
		server.TenantKey,
	)
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
