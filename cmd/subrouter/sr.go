package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	agentclaude "github.com/manaflow-ai/subrouter/internal/agents/claude"
	"github.com/manaflow-ai/subrouter/internal/selectacct"
)

const srUsageCacheTTL = time.Hour

const (
	ansiReset    = "\x1b[0m"
	ansiBold     = "\x1b[1m"
	ansiDim      = "\x1b[2m"
	ansiGreen    = "\x1b[32m"
	ansiYellow   = "\x1b[33m"
	ansiRed      = "\x1b[31m"
	ansiCyan     = "\x1b[36m"
	ansiMagenta  = "\x1b[35m"
	ansiWhite    = "\x1b[37m"
	ansiBGGreen  = "\x1b[42m"
	ansiBGYellow = "\x1b[43m"
	ansiBGRed    = "\x1b[41m"
	ansiBGGray   = "\x1b[100m"
	ansiBGRowAlt = "\x1b[48;5;236m"
)

const srHelp = `sr - Manage Subrouter accounts

Usage:
  sr                    Show Codex and Claude usage, grouped by provider
  sr add                Add a new Codex account (opens OAuth login)
  sr add-key            Add an API key account
  sr import             Import current ~/.codex/auth.json account
  sr list               List all Codex accounts
  sr switch [email]     Switch active Codex account and sync OpenCode/pi
  sr g [email]          Switch active account, sync OpenCode/pi, and restart Codex.app
  sr gui [email]        Switch active account, sync OpenCode/pi, and restart Codex.app
  sr gui-switch [email] Switch active account, sync OpenCode/pi, and restart Codex.app
  sr remove <email>     Remove a Codex account
  sr status             Show Codex and Claude usage (non-interactive)
  sr pick               Switch to the recommended account, failing if none has quota
  sr reset [email]      Redeem a rate-limit reset credit (pick best, or --all, or --dry-run)
  sr usage [days]       Refresh and show API-key spend
  sr trace <email>      Show OAuth refresh breadcrumbs for an account

  sr server             Manage Subrouter servers
  sr server add <name> --url <url> [--default]
  sr server use <name|local> [--no-codex-config]
  sr server rename <old> <new>
  sr server install <name>
  sr server login <name> [--device-auth]
  sr server sync <name> [--device-auth] [--yes]

  sr admin-keys         List stored OpenAI admin keys
  sr add-admin-key      Add an sk-admin-* key
  sr remove-admin-key <label>
  sr attach-project <api-key-label> [--project-id <id-or-name>]

  sr claude             Manage Claude Code profiles
  sr claude-aws [--model fable] [claude args...]
                        Launch Claude Code on AWS Bedrock via the server (Fable 5)
  sr claude-direct [claude args...]
                        Launch Claude Code directly on Anthropic (bypass subrouter)
  sr spend              Show AWS Bedrock spend tracked by the server
  sr gemini             Manage Gemini profiles

These account commands also work at top level as subrouter <command> and sr <command>.
The subrouter cx <command> form is kept as a compatibility alias.
`

type srRunner struct {
	program string
	store   accounts.CodexStore
	in      io.Reader
	out     io.Writer
	errOut  io.Writer
	client  *http.Client
	cmd     srCommandRunner
}

type srSwitchOptions struct {
	restartCodexGUI bool
}

type srUsageRow struct {
	email              string
	active             bool
	planType           string
	windows            []accounts.UsageWindow
	credits            *accounts.CreditsInfo
	complimentaryReset *accounts.ComplimentaryResetInfo
	apiKeySpend        *accounts.APIKeyUsageSnapshot
	apiKeyHint         string
	err                error
	score              selectacct.Score
	gtoReason          string
	gtoRecommended     bool
	cooked             bool
	cookedReason       string
	tempCooked         bool
	tempCookedReason   string
	authMode           accounts.AuthMode
	provider           accounts.Provider
}

func cxAlias(args []string) error {
	return srForProgram("cx", args)
}

func srForProgram(program string, args []string) error {
	runner := srRunner{
		program: program,
		store:   accounts.DefaultCodexStore(),
		in:      os.Stdin,
		out:     os.Stdout,
		errOut:  os.Stderr,
		client:  &http.Client{Timeout: 120 * time.Second},
	}
	return runner.run(context.Background(), args)
}

func (r srRunner) run(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return r.defaultInteractive(ctx, srSwitchOptions{})
	}
	if shouldRouteSRCommand(args[0]) {
		if server, ok, err := r.selectedRemoteServer(); err != nil {
			return err
		} else if ok {
			return r.runRemoteAccountCommand(ctx, server, args)
		}
	}
	switch args[0] {
	case "add", "login":
		return r.add(ctx)
	case "add-key", "add-api-key":
		return r.addKey()
	case "import":
		return r.importActive()
	case "list", "ls":
		return r.list()
	case "switch", "use":
		selector, opts, err := parseSRSwitchArgs(args[1:], srSwitchOptions{})
		if err != nil {
			return err
		}
		if selector == "" {
			return r.defaultInteractive(ctx, opts)
		}
		return r.switchAccount(ctx, selector, opts)
	case "g", "gui", "gui-switch", "gui-use":
		selector, opts, err := parseSRSwitchArgs(args[1:], srSwitchOptions{restartCodexGUI: true})
		if err != nil {
			return err
		}
		if selector == "" {
			return r.defaultInteractive(ctx, opts)
		}
		return r.switchAccount(ctx, selector, opts)
	case "remove", "rm":
		if len(args) < 2 {
			return fmt.Errorf("usage: subrouter remove <email>")
		}
		return r.remove(args[1])
	case "status":
		return r.status(ctx)
	case "pick":
		return r.pick(ctx, srSwitchOptions{})
	case "reset":
		return r.reset(ctx, args[1:])
	case "usage":
		days := 30
		if len(args) > 1 {
			parsed, err := strconv.Atoi(args[1])
			if err != nil || parsed < 1 || parsed > 30 {
				return fmt.Errorf("days must be 1..30")
			}
			days = parsed
		}
		return r.usage(ctx, days)
	case "trace", "breadcrumbs", "why":
		if len(args) < 2 {
			return fmt.Errorf("usage: subrouter trace <email>")
		}
		return r.trace(args[1])
	case "add-admin-key":
		return r.addAdminKey(ctx)
	case "list-admin-keys", "admin-keys":
		return r.listAdminKeys()
	case "remove-admin-key":
		if len(args) < 2 {
			return fmt.Errorf("usage: subrouter remove-admin-key <label>")
		}
		return r.removeAdminKey(args[1])
	case "attach-project":
		if len(args) < 2 {
			return fmt.Errorf("usage: subrouter attach-project <api-key-label> [--project-id <id-or-name>]")
		}
		projectID := ""
		if idx := indexOf(args, "--project-id"); idx >= 0 && idx+1 < len(args) {
			projectID = args[idx+1]
		}
		return r.attachProject(ctx, args[1], projectID)
	case "server", "servers":
		return r.server(ctx, args[1:])
	case "help", "-h", "--help":
		fmt.Fprint(r.out, srHelp)
		return nil
	case "claude":
		return r.claude(ctx, args[1:])
	case "claude-aws":
		return r.claudeAWS(ctx, args[1:])
	case "claude-direct":
		return r.claudeDirect(ctx, args[1:])
	case "spend", "cost":
		return r.spend(ctx)
	case "gemini":
		return r.gemini(args[1:])
	default:
		if strings.Contains(args[0], "@") {
			return r.statusOne(ctx, args[0])
		}
		return fmt.Errorf("unknown account command %q\n%s", args[0], srHelp)
	}
}

func shouldRouteSRCommand(command string) bool {
	switch command {
	case "server", "servers", "claude", "claude-aws", "claude-direct", "spend", "cost", "gemini", "help", "-h", "--help":
		return false
	default:
		return true
	}
}

func (r srRunner) runRemoteAccountCommand(ctx context.Context, server srServerConfig, args []string) error {
	command := args[0]
	switch command {
	case "add", "login":
		deviceAuth, err := parseRemoteAddArgs(command, args[1:])
		if err != nil {
			return err
		}
		return r.serverLoginOne(ctx, server, deviceAuth, "")
	case "add-key", "add-api-key":
		return r.addKeyToServer(ctx, server, args[1:])
	case "list", "ls":
		return r.listServerAccounts(ctx, server)
	case "status":
		return r.serverStatus(ctx, defaultSRServerStore(r.store), server.Name)
	case "usage":
		if len(args) > 1 {
			return fmt.Errorf("remote usage does not accept a day count; use %s server status %s", r.programOrSubrouter(), server.Name)
		}
		return r.serverStatus(ctx, defaultSRServerStore(r.store), server.Name)
	case "pick":
		return r.pickRemoteAccount(ctx, server)
	case "reset":
		return r.reset(ctx, args[1:])
	case "switch", "use", "g", "gui", "gui-switch", "gui-use":
		selector, _, err := parseSRSwitchArgs(args[1:], srSwitchOptions{})
		if err != nil {
			return err
		}
		if selector == "" {
			return r.serverStatus(ctx, defaultSRServerStore(r.store), server.Name)
		}
		return r.unsupportedRemoteCommand(command, server, "remote servers select accounts per session; use SUBROUTER_CODEX_ACCOUNT_ID for a one-off forced account")
	case "import":
		return r.unsupportedRemoteCommand(command, server, "copying a local refresh-token chain to a server is unsafe; use "+r.programOrSubrouter()+" add or "+r.programOrSubrouter()+" server sync "+server.Name)
	case "remove", "rm", "trace", "breadcrumbs", "why", "add-admin-key", "list-admin-keys", "admin-keys", "remove-admin-key", "attach-project":
		return r.unsupportedRemoteCommand(command, server, "this command has no remote-safe implementation yet")
	default:
		if strings.Contains(command, "@") {
			return r.statusOneRemote(ctx, server, command)
		}
		return fmt.Errorf("unknown account command %q\n%s", command, srHelp)
	}
}

func parseRemoteAddArgs(command string, args []string) (bool, error) {
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	deviceAuth := flags.Bool("device-auth", false, "use codex login --device-auth")
	if err := flags.Parse(args); err != nil {
		return false, err
	}
	if flags.NArg() != 0 {
		return false, fmt.Errorf("unexpected arguments: %s", strings.Join(flags.Args(), " "))
	}
	return *deviceAuth, nil
}

func (r srRunner) unsupportedRemoteCommand(command string, server srServerConfig, detail string) error {
	return fmt.Errorf("%s is configured to use server %s (%s), so %s will not edit local Codex state; %s", r.programOrSubrouter(), server.Name, server.URL, r.programOrSubrouter()+" "+command, detail)
}

func (r srRunner) add(ctx context.Context) error {
	previousActive, err := r.store.DetectActiveAccount()
	if err != nil {
		return err
	}
	if err := r.store.SyncActiveToStore(); err != nil {
		return err
	}
	fmt.Fprintln(r.out, "Opening Codex OAuth login...")
	if err := r.commandRunner().Run(ctx, "codex", []string{"login"}, r.in, r.out, r.errOut); err != nil {
		return fmt.Errorf("codex login failed: %w", err)
	}
	account, existed, err := r.store.ImportActive()
	if err != nil {
		return err
	}
	if existed {
		fmt.Fprintf(r.out, "\nUpdated account: %s\n", account.Email)
	} else {
		fmt.Fprintf(r.out, "\nAdded account: %s\n", account.Email)
	}
	if previousActive != "" && previousActive != account.Email {
		if err := r.store.SwitchActive(previousActive); err != nil {
			return fmt.Errorf("restore active account %s: %w", previousActive, err)
		}
		previous, ok, err := r.store.FindStored(previousActive)
		if err != nil {
			return err
		}
		if ok {
			for _, result := range syncCodexCompatibleAuth(previous) {
				if result.Err != nil {
					fmt.Fprintf(r.errOut, "Warning: %s auth sync failed: %s\n", result.Tool, result.Err)
					continue
				}
				fmt.Fprintf(r.out, "Synced %s auth: %s\n", result.Tool, result.Path)
			}
		}
		fmt.Fprintf(r.out, "Restored active account: %s\n", previousActive)
	}
	return nil
}

func (r srRunner) addKey() error {
	if err := r.store.SyncActiveToStore(); err != nil {
		return err
	}
	reader := bufio.NewReader(r.in)
	label, err := promptLine(r.out, reader, "Label (e.g. work, personal): ")
	if err != nil {
		return err
	}
	key, err := promptLine(r.out, reader, "API key (sk-...): ")
	if err != nil {
		return err
	}
	account, existed, err := r.store.AddAPIKey(label, key)
	if err != nil {
		return err
	}
	if existed {
		fmt.Fprintf(r.out, "Updated API key account: %s\n", account.APIKeyLabel())
	} else {
		fmt.Fprintf(r.out, "Added API key account: %s\n", account.APIKeyLabel())
	}
	return nil
}

func (r srRunner) importActive() error {
	account, existed, err := r.store.ImportActive()
	if err != nil {
		return err
	}
	if existed {
		fmt.Fprintf(r.out, "Updated existing account: %s\n", account.Email)
	} else {
		fmt.Fprintf(r.out, "Imported account: %s\n", account.Email)
	}
	return nil
}

func (r srRunner) autoImportIfEmpty() error {
	all, err := r.store.ListStored()
	if err != nil || len(all) > 0 {
		return err
	}
	account, _, err := r.store.ImportActive()
	if err == nil {
		fmt.Fprintf(r.out, "Auto-imported active account: %s\n\n", account.Email)
	}
	return nil
}

func (r srRunner) list() error {
	all, err := r.store.ListStored()
	if err != nil {
		return err
	}
	active, err := r.store.DetectActiveAccount()
	if err != nil {
		return err
	}
	if len(all) == 0 {
		fmt.Fprintln(r.out, "No accounts configured. Run 'subrouter add' to add one.")
		return nil
	}
	fmt.Fprintln(r.out)
	for _, account := range all {
		marker := ""
		if account.Email == active {
			marker = " *"
		}
		fmt.Fprintf(r.out, "  %s%s (added %s)\n", displayAccountName(account.Email), marker, formatDate(account.AddedAt))
	}
	fmt.Fprintln(r.out)
	fmt.Fprintln(r.out, "* = currently active in ~/.codex/auth.json")
	return nil
}

func (r srRunner) trace(selector string) error {
	account, ok, err := r.store.FindStored(selector)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("no account found matching %q", selector)
	}
	fmt.Fprintf(r.out, "\nOAuth breadcrumbs for %s\n\n", displayAccountName(account.Email))
	if len(account.Breadcrumbs) == 0 {
		fmt.Fprintln(r.out, "  none")
		return nil
	}
	for _, crumb := range account.Breadcrumbs {
		fmt.Fprintf(r.out, "  %s  %s\n", crumb.At, crumb.Event)
		fmt.Fprintf(r.out, "    where: %s\n", formatBreadcrumbWhere(crumb))
		fmt.Fprintf(r.out, "    why: %s\n", formatBreadcrumbWhy(crumb))
		fmt.Fprintf(r.out, "    token: %s\n", formatBreadcrumbToken(crumb))
		if errLine := formatBreadcrumbError(crumb); errLine != "" {
			fmt.Fprintf(r.out, "    error: %s\n", errLine)
		}
		if crumb.SourcePath != "" {
			fmt.Fprintf(r.out, "    file: %s\n", crumb.SourcePath)
		}
	}
	return nil
}

func formatBreadcrumbWhere(crumb accounts.CodexAuthBreadcrumb) string {
	parts := []string{}
	appendKV(&parts, "host", crumb.Host)
	if crumb.PID != 0 {
		appendKV(&parts, "pid", strconv.Itoa(crumb.PID))
	}
	if crumb.PPID != 0 {
		appendKV(&parts, "ppid", strconv.Itoa(crumb.PPID))
	}
	appendKV(&parts, "exe", crumb.Executable)
	appendKV(&parts, "cwd", crumb.WorkingDir)
	if len(parts) == 0 {
		return "unknown"
	}
	return strings.Join(parts, " ")
}

func formatBreadcrumbWhy(crumb accounts.CodexAuthBreadcrumb) string {
	parts := []string{}
	appendKV(&parts, "source", crumb.Source)
	appendKV(&parts, "reason", crumb.Reason)
	appendKV(&parts, "force", strconv.FormatBool(crumb.Force))
	appendKV(&parts, "last_refresh", crumb.LastRefresh)
	appendKV(&parts, "access_exp", crumb.AccessExp)
	appendKV(&parts, "access_expired", strconv.FormatBool(crumb.AccessExpired))
	if len(parts) == 0 {
		return "unknown"
	}
	return strings.Join(parts, " ")
}

func formatBreadcrumbToken(crumb accounts.CodexAuthBreadcrumb) string {
	parts := []string{}
	appendKV(&parts, "refresh", crumb.RefreshFP)
	appendKV(&parts, "old_refresh", crumb.OldRefreshFP)
	appendKV(&parts, "new_refresh", crumb.NewRefreshFP)
	appendKV(&parts, "recovered_refresh", crumb.RecoveredRefreshFP)
	appendKV(&parts, "account_id", crumb.AccountID)
	appendKV(&parts, "old_account_id", crumb.OldAccountID)
	appendKV(&parts, "new_account_id", crumb.NewAccountID)
	appendKV(&parts, "recovered_account_id", crumb.RecoveredAccountID)
	if len(parts) == 0 {
		return "none"
	}
	return strings.Join(parts, " ")
}

func formatBreadcrumbError(crumb accounts.CodexAuthBreadcrumb) string {
	parts := []string{}
	if crumb.StatusCode != 0 {
		appendKV(&parts, "status", strconv.Itoa(crumb.StatusCode))
	}
	appendKV(&parts, "provider_type", crumb.ProviderType)
	appendKV(&parts, "provider_code", crumb.ProviderCode)
	appendKV(&parts, "provider_message", crumb.ProviderMessage)
	return strings.Join(parts, " ")
}

func appendKV(parts *[]string, key, value string) {
	if strings.TrimSpace(value) == "" {
		return
	}
	*parts = append(*parts, key+"="+strconv.Quote(value))
}

func (r srRunner) status(ctx context.Context) error {
	if server, ok, err := r.defaultRemoteServer(); err != nil {
		return err
	} else if ok {
		return r.serverStatus(ctx, defaultSRServerStore(r.store), server.Name)
	}
	if err := r.autoImportIfEmpty(); err != nil {
		return err
	}
	rows, err := r.fetchUsageRows(ctx)
	if err != nil {
		return err
	}
	displayUsageRows(r.out, rows, false)
	return nil
}

func (r srRunner) statusOne(ctx context.Context, selector string) error {
	if err := r.autoImportIfEmpty(); err != nil {
		return err
	}
	all, err := r.fetchUsageRows(ctx)
	if err != nil {
		return err
	}
	var matches []srUsageRow
	lower := strings.ToLower(selector)
	for _, row := range all {
		if strings.Contains(strings.ToLower(row.email), lower) {
			matches = append(matches, row)
		}
	}
	if len(matches) == 0 {
		return fmt.Errorf("no account found for %s", selector)
	}
	displayUsageRows(r.out, matches, false)
	return nil
}

func (r srRunner) pick(ctx context.Context, opts srSwitchOptions) error {
	if err := r.autoImportIfEmpty(); err != nil {
		return err
	}
	rows, err := r.fetchUsageRows(ctx)
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		return fmt.Errorf("no accounts configured. Run 'subrouter add' to add one")
	}
	target := recommendedUsableUsageRow(rows)
	if target == nil {
		displayUsageRows(r.out, rows, false)
		return fmt.Errorf("no recommended account has quota for a new session")
	}
	if target.active {
		displayUsageRows(r.out, []srUsageRow{*target}, false)
		fmt.Fprintf(r.out, "Already using recommended account: %s\n", target.email)
		return nil
	}
	if err := ensureUsageRowSwitchable(*target); err != nil {
		return err
	}
	displayUsageRows(r.out, []srUsageRow{*target}, false)
	if err := r.switchAccount(ctx, target.email, opts); err != nil {
		return err
	}
	fmt.Fprintf(r.out, "Picked recommended account: %s\n", target.email)
	return nil
}

func (r srRunner) defaultInteractive(ctx context.Context, opts srSwitchOptions) error {
	if server, ok, err := r.defaultRemoteServer(); err != nil {
		return err
	} else if ok {
		return r.serverStatus(ctx, defaultSRServerStore(r.store), server.Name)
	}
	if err := r.autoImportIfEmpty(); err != nil {
		return err
	}
	rows, err := r.fetchUsageRows(ctx)
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		fmt.Fprintln(r.out, "No accounts configured. Run 'subrouter add' to add one.")
		return nil
	}
	switched, err := r.autoSwitchExhaustedActive(ctx, rows, opts)
	if err != nil {
		return err
	}
	if switched {
		rows, err = r.fetchUsageRows(ctx)
		if err != nil {
			return err
		}
	}
	allCooked := allOAuthRowsCooked(rows)
	allBlocked := allOAuthRowsBlockedForNewSession(rows)
	if allCooked {
		fmt.Fprintln(r.out)
		fmt.Fprintln(r.out, "All of your OAuth accounts are cooked. Every usable 7d window is fully consumed, so sr will keep the current active account and will not switch.")
	} else if allBlocked {
		fmt.Fprintln(r.out)
		fmt.Fprintln(r.out, "All of your OAuth accounts are unavailable for new sessions. Usable windows are exhausted, so sr will keep the current active account and will not switch.")
	}
	displayUsageRows(r.out, rows, true)
	if switched {
		return nil
	}
	if allCooked || allBlocked || !hasSwitchableUsageRows(rows) {
		return nil
	}

	reader := bufio.NewReader(r.in)
	answer, err := promptLine(r.out, reader, "Switch to (#): ")
	if err != nil {
		return err
	}
	answer = strings.TrimSpace(answer)
	if answer == "" {
		return nil
	}
	if idx, err := strconv.Atoi(answer); err == nil && idx >= 1 && idx <= len(rows) {
		row := rows[idx-1]
		if err := ensureUsageRowSwitchable(row); err != nil {
			return err
		}
		return r.switchAccount(ctx, row.email, opts)
	}
	return r.switchAccount(ctx, answer, opts)
}

func (r srRunner) autoSwitchExhaustedActive(ctx context.Context, rows []srUsageRow, opts srSwitchOptions) (bool, error) {
	active := activeUsageRow(rows)
	if active == nil || active.err != nil || active.authMode != accounts.AuthModeOAuth || !exhaustedForNewSession(active.score) {
		return false, nil
	}
	target := recommendedUsableOAuthRow(rows)
	if target == nil || target.email == active.email {
		return false, nil
	}
	if err := r.switchAccount(ctx, target.email, opts); err != nil {
		return false, err
	}
	fmt.Fprintf(r.out, "Auto-switched to %s because active account %s is exhausted.\n", target.email, active.email)
	return true, nil
}

func activeUsageRow(rows []srUsageRow) *srUsageRow {
	for i := range rows {
		if rows[i].active {
			return &rows[i]
		}
	}
	return nil
}

func recommendedUsableOAuthRow(rows []srUsageRow) *srUsageRow {
	for i := range rows {
		if rows[i].gtoRecommended && recommendedForNewSession(rows[i]) && rows[i].authMode == accounts.AuthModeOAuth {
			return &rows[i]
		}
	}
	return nil
}

func recommendedUsableUsageRow(rows []srUsageRow) *srUsageRow {
	for i := range rows {
		if rows[i].gtoRecommended && recommendedForNewSession(rows[i]) {
			return &rows[i]
		}
	}
	return nil
}

func (r srRunner) fetchUsageRows(ctx context.Context) ([]srUsageRow, error) {
	all, err := r.store.ListStored()
	if err != nil {
		return nil, err
	}
	active, err := r.store.DetectActiveAccount()
	if err != nil {
		return nil, err
	}
	admins, err := r.store.ListAdminKeys()
	if err != nil {
		return nil, err
	}

	rows := make([]srUsageRow, len(all))
	var wg sync.WaitGroup
	for i, account := range all {
		i, account := i, account
		rows[i] = srUsageRow{email: account.Email, active: account.Email == active, provider: accounts.ProviderCodex}
		if account.IsAPIKey() {
			rows[i].authMode = accounts.AuthModeAPIKey
			rows[i].score = selectacct.Score{AccountID: account.Email, Headroom: 0.01, ShortHeadroom: 0.01}
			rows[i].planType = "api key"
			rows[i].apiKeyHint = r.apiKeyHint(account, admins)
			if admin, ok, err := r.store.PickAdminKeyFor(account); err == nil && ok {
				if fresh, ok, err := r.store.ReadUsageCache(admin.Label, account.ProjectID, srUsageCacheTTL); err == nil && ok {
					rows[i].apiKeySpend = &fresh
					rows[i].apiKeyHint = ""
				} else if stale, ok, err := r.store.ReadUsageCacheStale(admin.Label, account.ProjectID); err == nil && ok {
					rows[i].apiKeySpend = &stale
					rows[i].apiKeyHint = ""
				}
			}
			continue
		}

		rows[i].authMode = accounts.AuthModeOAuth
		wg.Add(1)
		go func() {
			defer wg.Done()
			refreshCtx := accounts.WithCodexRefreshReason(ctx, "sr.status")
			refreshed, _, refreshErr := r.store.RefreshStoredIfExpired(refreshCtx, r.client, account)
			if refreshErr != nil {
				rows[i].err = refreshErr
				rows[i].score = selectacct.Score{AccountID: account.Email, Headroom: 0, ShortHeadroom: 0}
				return
			}
			acct := accountFromStored(refreshed)
			details, err := accounts.FetchCodexUsageDetails(ctx, r.client, acct)
			if err != nil {
				rows[i].err = err
				rows[i].score = selectacct.Score{AccountID: account.Email, Headroom: 0, ShortHeadroom: 0}
				return
			}
			rows[i].planType = details.PlanType
			rows[i].windows = details.Windows
			rows[i].credits = details.Credits
			rows[i].complimentaryReset = details.ComplimentaryReset
			rows[i].score = scoreFromWindows(account.Email, details.Windows)
			rows[i].cooked, rows[i].cookedReason = cookedFromWindows(details.Windows)
			rows[i].tempCooked, rows[i].tempCookedReason = tempCookedFromWindows(details.Windows)
		}()
	}
	wg.Wait()
	claudeStore := agentclaude.DefaultStore()
	claudeProfiles := claudeStore.ListProfiles()
	claudeOffset := len(rows)
	rows = append(rows, make([]srUsageRow, len(claudeProfiles))...)
	activeClaude := claudeStore.ActiveProfile()
	for i, profile := range claudeProfiles {
		i, profile := claudeOffset+i, profile
		rows[i] = srUsageRow{
			email:    profile.Name,
			active:   profile.Name == activeClaude,
			planType: "claude",
			authMode: accounts.AuthModeOAuth,
			provider: accounts.ProviderClaude,
			score:    selectacct.Score{AccountID: profile.Name, Headroom: 1, ShortHeadroom: 1},
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			account, _, err := claudeStore.RefreshCredentialIfExpired(ctx, r.client, profile)
			if err != nil {
				rows[i].err = err
				rows[i].score = selectacct.Score{AccountID: profile.Name, Headroom: 0, ShortHeadroom: 0}
				return
			}
			windows, err := fetchClaudeUsageWindows(ctx, r.client, account.Token)
			if err != nil {
				rows[i].err = err
				rows[i].score = selectacct.Score{AccountID: profile.Name, Headroom: 0, ShortHeadroom: 0}
				return
			}
			rows[i].windows = windows
			rows[i].score = scoreFromWindows(profile.Name, rows[i].windows)
		}()
	}
	wg.Wait()
	rankUsageRows(rows)
	return rows, nil
}

func (r srRunner) apiKeyHint(account accounts.StoredCodexAccount, admins []accounts.AdminKeyEntry) string {
	if len(admins) == 0 {
		return "no admin key, run 'subrouter add-admin-key' to enable spend display"
	}
	admin, ok, err := r.store.PickAdminKeyFor(account)
	if err != nil || !ok {
		return "no admin key linked"
	}
	return fmt.Sprintf("no cached usage, run 'subrouter usage' (admin: %s)", admin.Label)
}

func (r srRunner) switchAccount(ctx context.Context, selector string, opts srSwitchOptions) error {
	account, ok, err := r.store.FindStored(selector)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("no account found matching %q", selector)
	}
	if err := r.store.SyncActiveToStore(); err != nil {
		return err
	}
	refreshCtx := accounts.WithCodexRefreshReason(ctx, "sr.switch")
	refreshed, didRefresh, err := r.store.RefreshStoredIfExpired(refreshCtx, r.client, account)
	if err != nil {
		fmt.Fprintf(r.errOut, "Warning: token refresh failed, using cached tokens: %s\n", err)
		refreshed = account
	} else if didRefresh {
		account = refreshed
	}
	if err := r.ensureSwitchableForFreshUsage(ctx, account); err != nil {
		return err
	}
	if err := accounts.WriteActiveCodexAuth(account.Auth); err != nil {
		return err
	}
	fmt.Fprintf(r.out, "Switched to %s\n", account.Email)
	for _, result := range syncCodexCompatibleAuth(account) {
		if result.Err != nil {
			fmt.Fprintf(r.errOut, "Warning: %s auth sync failed: %s\n", result.Tool, result.Err)
			continue
		}
		fmt.Fprintf(r.out, "Synced %s auth: %s\n", result.Tool, result.Path)
	}
	if opts.restartCodexGUI {
		return r.reportCodexGUIRestart(ctx)
	}
	fmt.Fprintln(r.out, "Restart any running Codex, OpenCode, or pi sessions to use the new account.")
	return nil
}

func (r srRunner) reportCodexGUIRestart(ctx context.Context) error {
	status, err := restartCodexGUI(ctx)
	if err != nil {
		fmt.Fprintf(r.errOut, "Warning: %s\n", err)
		fmt.Fprintln(r.errOut, "Restart Codex.app manually to use the new account in the GUI.")
		return nil
	}
	switch status {
	case "restarted":
		fmt.Fprintln(r.out, "Restarted Codex.app so the GUI uses the new account.")
	case "not-running":
		fmt.Fprintln(r.out, "Codex.app is not running; it will use the new account on next launch.")
	case "unsupported":
		fmt.Fprintln(r.out, "Codex.app restart is only supported on macOS.")
	}
	return nil
}

func (r srRunner) remove(selector string) error {
	account, ok, err := r.store.RemoveStored(selector)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("no account found matching %q", selector)
	}
	fmt.Fprintf(r.out, "Removed account: %s\n", account.Email)
	return nil
}

func (r srRunner) addAdminKey(ctx context.Context) error {
	reader := bufio.NewReader(r.in)
	label, err := promptLine(r.out, reader, "Label (e.g. work): ")
	if err != nil {
		return err
	}
	key, err := promptLine(r.out, reader, "Admin key (sk-admin-...): ")
	if err != nil {
		return err
	}
	label = strings.TrimSpace(label)
	key = strings.TrimSpace(key)
	if label == "" {
		return fmt.Errorf("label is required")
	}
	if !strings.HasPrefix(key, "sk-admin-") {
		return fmt.Errorf("invalid admin key format, expected sk-admin-...")
	}
	fmt.Fprint(r.out, "Validating with OpenAI... ")
	if err := accounts.ValidateAdminKey(ctx, r.client, key); err != nil {
		fmt.Fprintln(r.out, "failed")
		return err
	}
	fmt.Fprintln(r.out, "ok")
	entry := accounts.AdminKeyEntry{Label: label, Key: key, AddedAt: time.Now().UTC().Format(time.RFC3339)}
	if err := r.store.SaveAdminKey(entry); err != nil {
		return err
	}
	fmt.Fprintf(r.out, "Added admin key: %s\n", label)
	return nil
}

func (r srRunner) listAdminKeys() error {
	all, err := r.store.ListAdminKeys()
	if err != nil {
		return err
	}
	if len(all) == 0 {
		fmt.Fprintln(r.out, "No admin keys. Run 'subrouter add-admin-key' to add one.")
		return nil
	}
	fmt.Fprintln(r.out)
	for _, key := range all {
		fmt.Fprintf(r.out, "  %s  %s  added %s\n", key.Label, maskSecret(key.Key), formatDate(key.AddedAt))
	}
	fmt.Fprintln(r.out)
	return nil
}

func (r srRunner) removeAdminKey(label string) error {
	removed, err := r.store.RemoveAdminKey(label)
	if err != nil {
		return err
	}
	if !removed {
		return fmt.Errorf("no admin key labeled %q", label)
	}
	fmt.Fprintf(r.out, "Removed admin key: %s\n", label)
	return nil
}

func (r srRunner) attachProject(ctx context.Context, apiKeyLabel, projectID string) error {
	selector := apiKeyLabel
	if !strings.HasPrefix(selector, "apikey:") {
		selector = "apikey:" + selector
	}
	account, ok, err := r.store.FindStored(selector)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("no API-key account named %q", apiKeyLabel)
	}
	if !account.IsAPIKey() {
		return fmt.Errorf("%q is not an API-key account", apiKeyLabel)
	}
	admin, ok, err := r.store.PickAdminKeyFor(account)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("no admin key configured, run 'subrouter add-admin-key' first")
	}
	projects, err := accounts.ListOpenAIProjects(ctx, r.client, admin.Key)
	if err != nil {
		return err
	}
	chosen := -1
	if projectID != "" {
		if projectID == "none" || projectID == "org" {
			chosen = -1
		} else {
			for i, project := range projects {
				if project.ID == projectID || project.Name == projectID {
					chosen = i
					break
				}
			}
			if chosen == -1 {
				return fmt.Errorf("no project matching %q", projectID)
			}
		}
	} else {
		fmt.Fprintln(r.out)
		for i, project := range projects {
			fmt.Fprintf(r.out, "  %d) %s  %s\n", i+1, project.Name, project.ID)
		}
		fmt.Fprintln(r.out, "  0) <none / org-wide>")
		reader := bufio.NewReader(r.in)
		answer, err := promptLine(r.out, reader, "Pick project (#): ")
		if err != nil {
			return err
		}
		idx, err := strconv.Atoi(strings.TrimSpace(answer))
		if err != nil || idx < 0 || idx > len(projects) {
			return fmt.Errorf("invalid selection")
		}
		chosen = idx - 1
	}
	if chosen < 0 {
		account.ProjectID = ""
		account.ProjectName = ""
	} else {
		account.ProjectID = projects[chosen].ID
		account.ProjectName = projects[chosen].Name
	}
	account.AdminKeyLabel = admin.Label
	if err := r.store.SaveStored(account); err != nil {
		return err
	}
	scope := "<org-wide>"
	if account.ProjectName != "" {
		scope = account.ProjectName
	}
	fmt.Fprintf(r.out, "Updated %s: project=%s via admin=%s\n", account.APIKeyLabel(), scope, admin.Label)
	return nil
}

func (r srRunner) usage(ctx context.Context, days int) error {
	all, err := r.store.ListStored()
	if err != nil {
		return err
	}
	var apiAccounts []accounts.StoredCodexAccount
	for _, account := range all {
		if account.IsAPIKey() {
			apiAccounts = append(apiAccounts, account)
		}
	}
	if len(apiAccounts) == 0 {
		fmt.Fprintln(r.out, "No API-key accounts. Run 'subrouter add-key' first.")
		return nil
	}
	admins, err := r.store.ListAdminKeys()
	if err != nil {
		return err
	}
	if len(admins) == 0 {
		return fmt.Errorf("no admin keys. Run 'subrouter add-admin-key' first")
	}

	fmt.Fprintf(r.out, "Fetching %d-day usage for %d API-key account(s)...\n\n", days, len(apiAccounts))
	active, _ := r.store.DetectActiveAccount()
	rows := make([]srUsageRow, len(apiAccounts))
	var wg sync.WaitGroup
	for i, account := range apiAccounts {
		i, account := i, account
		rows[i] = srUsageRow{email: account.Email, active: account.Email == active, planType: "api key", authMode: accounts.AuthModeAPIKey}
		wg.Add(1)
		go func() {
			defer wg.Done()
			admin, ok, err := r.store.PickAdminKeyFor(account)
			if err != nil || !ok {
				rows[i].err = fmt.Errorf("no admin key linked")
				return
			}
			started := time.Now()
			snapshot, err := accounts.FetchAPIKeyUsageSnapshot(ctx, r.client, admin, account, days)
			if err != nil {
				rows[i].err = err
				fmt.Fprintf(r.out, "  %s via %s: failed (%s)\n", account.APIKeyLabel(), admin.Label, time.Since(started).Round(time.Second))
				return
			}
			if err := r.store.WriteUsageCache(snapshot); err != nil {
				rows[i].err = err
				return
			}
			rows[i].apiKeySpend = &snapshot
			rows[i].score = selectacct.Score{AccountID: account.Email, Headroom: 0.01, ShortHeadroom: 0.01}
			fmt.Fprintf(r.out, "  %s via %s: ok (%s)\n", account.APIKeyLabel(), admin.Label, time.Since(started).Round(time.Second))
		}()
	}
	wg.Wait()
	rankUsageRows(rows)
	displayUsageRows(r.out, rows, false)
	return nil
}

func accountFromStored(account accounts.StoredCodexAccount) accounts.Account {
	out := accounts.Account{
		ID:       account.Email,
		Provider: accounts.ProviderCodex,
		Label:    account.Email,
		Email:    account.Email,
		Source:   account.Email,
	}
	if account.IsAPIKey() {
		out.AuthMode = accounts.AuthModeAPIKey
		out.Token = account.Auth.OpenAIAPIKey
		return out
	}
	out.AuthMode = accounts.AuthModeOAuth
	if account.Auth.Tokens != nil {
		out.Token = account.Auth.Tokens.AccessToken
		out.AccountID = account.Auth.Tokens.AccountID
	}
	return out
}

func scoreFromWindows(accountID string, windows []accounts.UsageWindow) selectacct.Score {
	limitWindows := make([]selectacct.LimitWindow, 0, len(windows)*2)
	hasFableWindow := false
	for _, window := range windows {
		if window.Feature == agentclaude.FableFeature || window.Feature == agentclaude.FableModel || window.Name == agentclaude.FableWindowName {
			hasFableWindow = true
			break
		}
	}
	for _, window := range windows {
		limitWindows = append(limitWindows, selectacct.LimitWindow{
			Name:               window.Name,
			UsedPercent:        window.UsedPercent,
			LimitWindowSeconds: window.LimitWindowSeconds,
			ResetAfterSeconds:  window.ResetAfterSeconds,
			Feature:            window.Feature,
		})
		if hasFableWindow && window.Feature == "" {
			limitWindows = append(limitWindows, selectacct.LimitWindow{
				Name:               window.Name,
				UsedPercent:        window.UsedPercent,
				LimitWindowSeconds: window.LimitWindowSeconds,
				ResetAfterSeconds:  window.ResetAfterSeconds,
				Feature:            agentclaude.FableFeature,
			})
		}
	}
	return selectacct.ScoreFromLimitWindows(accountID, 0, limitWindows)
}

// isModelScopedWindow reports whether a window is a per-model quota pool (e.g.
// Fable), identified by a non-empty Feature. Such a pool being exhausted must
// not cook the whole account: the scheduler already scores it as its own pool,
// and the account stays usable for other models (Opus/Sonnet).
func isModelScopedWindow(window accounts.UsageWindow) bool {
	return strings.TrimSpace(window.Feature) != ""
}

func cookedFromWindows(windows []accounts.UsageWindow) (bool, string) {
	for _, window := range windows {
		if isModelScopedWindow(window) || !isLongQuotaWindow(window) || clampUsagePercent(window.UsedPercent) < 100 {
			continue
		}
		if window.ResetAfterSeconds > 0 {
			return true, fmt.Sprintf("%s fully consumed, resets in %s", windowLabel(window), formatDuration(window.ResetAfterSeconds))
		}
		return true, fmt.Sprintf("%s fully consumed", windowLabel(window))
	}
	return false, ""
}

func tempCookedFromWindows(windows []accounts.UsageWindow) (bool, string) {
	if longQuotaSaturated(windows) {
		return false, ""
	}
	for _, window := range windows {
		if !isShortQuotaWindow(window) || clampUsagePercent(window.UsedPercent) < 100 {
			continue
		}
		if window.ResetAfterSeconds > 0 {
			return true, fmt.Sprintf("%s fully consumed, resets in %s", windowLabel(window), formatDuration(window.ResetAfterSeconds))
		}
		return true, fmt.Sprintf("%s fully consumed", windowLabel(window))
	}
	return false, ""
}

func longQuotaSaturated(windows []accounts.UsageWindow) bool {
	cooked, _ := cookedFromWindows(windows)
	return cooked
}

func isShortQuotaWindow(window accounts.UsageWindow) bool {
	if window.LimitWindowSeconds > 0 {
		return window.LimitWindowSeconds <= int64((6*time.Hour)/time.Second)
	}
	name := strings.ToLower(window.Name)
	return strings.Contains(name, "5h") || strings.Contains(name, "primary")
}

func isLongQuotaWindow(window accounts.UsageWindow) bool {
	if window.LimitWindowSeconds > 0 {
		return window.LimitWindowSeconds >= int64((6*24*time.Hour)/time.Second)
	}
	name := strings.ToLower(window.Name)
	return strings.Contains(name, "7d") || strings.Contains(name, "weekly")
}

func isClaudeSessionWindow(window accounts.UsageWindow) bool {
	name := strings.ToLower(window.Name)
	return name == "5h" || strings.Contains(name, "session")
}

func isClaudeWeeklyWindow(window accounts.UsageWindow) bool {
	name := strings.ToLower(window.Name)
	return name == "7d" || name == "weekly"
}

func isClaudeOAuthAppsWeeklyWindow(window accounts.UsageWindow) bool {
	name := strings.ToLower(window.Name)
	return strings.Contains(name, "oauth-apps") || strings.Contains(name, "7d_oi")
}

func isClaudeOpusWeeklyWindow(window accounts.UsageWindow) bool {
	return strings.Contains(strings.ToLower(window.Name), "opus")
}

func isClaudeSonnetWeeklyWindow(window accounts.UsageWindow) bool {
	return strings.Contains(strings.ToLower(window.Name), "sonnet")
}

func isClaudeExtraWindow(window accounts.UsageWindow) bool {
	return strings.Contains(strings.ToLower(window.Name), "extra")
}

func clampUsagePercent(value float64) float64 {
	switch {
	case value < 0:
		return 0
	case value > 100:
		return 100
	default:
		return value
	}
}

func allOAuthRowsCooked(rows []srUsageRow) bool {
	seenOAuth := false
	for _, row := range rows {
		if row.provider != "" && row.provider != accounts.ProviderCodex {
			continue
		}
		if row.err != nil || row.authMode == accounts.AuthModeAPIKey {
			return false
		}
		if row.authMode != accounts.AuthModeOAuth {
			continue
		}
		seenOAuth = true
		if !row.cooked {
			return false
		}
	}
	return seenOAuth
}

func allOAuthRowsBlockedForNewSession(rows []srUsageRow) bool {
	seenOAuth := false
	for _, row := range rows {
		if row.provider != "" && row.provider != accounts.ProviderCodex {
			continue
		}
		if row.err != nil || row.authMode == accounts.AuthModeAPIKey {
			return false
		}
		if row.authMode != accounts.AuthModeOAuth {
			continue
		}
		seenOAuth = true
		if !row.cooked && !row.tempCooked && !exhaustedForNewSession(row.score) {
			return false
		}
	}
	return seenOAuth
}

func hasSwitchableUsageRows(rows []srUsageRow) bool {
	for _, row := range rows {
		if row.provider != "" && row.provider != accounts.ProviderCodex {
			continue
		}
		if row.err != nil {
			continue
		}
		if row.authMode == accounts.AuthModeAPIKey {
			return true
		}
		if row.authMode == accounts.AuthModeOAuth && !row.cooked && !row.tempCooked {
			return true
		}
	}
	return false
}

func ensureUsageRowSwitchable(row srUsageRow) error {
	if row.provider != "" && row.provider != accounts.ProviderCodex {
		return fmt.Errorf("cannot switch to %s with sr; use sr claude switch %s", row.email, row.email)
	}
	if row.cooked {
		return fmt.Errorf("cannot switch to %s: account is cooked (%s)", row.email, row.cookedReason)
	}
	if row.tempCooked {
		return fmt.Errorf("cannot switch to %s: account is temporarily cooked (%s)", row.email, row.tempCookedReason)
	}
	return nil
}

func claudeUsageWindows(usage *agentclaude.UsageResponse) []accounts.UsageWindow {
	if usage == nil {
		return nil
	}
	var windows []accounts.UsageWindow
	add := func(name string, windowSeconds int64, limit *agentclaude.RateLimit) {
		if limit == nil || limit.Utilization == nil {
			return
		}
		window := accounts.UsageWindow{Name: name, UsedPercent: *limit.Utilization, LimitWindowSeconds: windowSeconds}
		if name == agentclaude.FableWindowName {
			window.Feature = agentclaude.FableFeature
		}
		if reset, err := time.Parse(time.RFC3339, limit.ResetsAt); err == nil {
			seconds := int64(time.Until(reset).Seconds())
			if seconds < 0 {
				seconds = 0
			}
			window.ResetAfterSeconds = seconds
		}
		windows = append(windows, window)
	}
	const (
		fiveHourSeconds = int64(5 * 60 * 60)
		sevenDaySeconds = int64(7 * 24 * 60 * 60)
	)
	add("5h", fiveHourSeconds, usage.FiveHour)
	add("7d", sevenDaySeconds, usage.SevenDay)
	add(agentclaude.FableWindowName, sevenDaySeconds, usage.SevenDayOAuthApps)
	add("opus-weekly", sevenDaySeconds, usage.SevenDayOpus)
	add("sonnet-weekly", sevenDaySeconds, usage.SevenDaySonnet)
	if usage.ExtraUsage != nil && usage.ExtraUsage.IsEnabled && usage.ExtraUsage.Utilization != nil {
		windows = append(windows, accounts.UsageWindow{Name: "extra", UsedPercent: *usage.ExtraUsage.Utilization})
	}
	return windows
}

func fetchClaudeUsageWindows(ctx context.Context, client *http.Client, accessToken string) ([]accounts.UsageWindow, error) {
	usage, err := agentclaude.FetchUsage(ctx, client, accessToken)
	windows := claudeUsageWindows(usage)
	if !usageWindowNamed(windows, agentclaude.FableWindowName) {
		fableWindows, probeErr := agentclaude.FetchFableUsageWindows(ctx, client, accessToken)
		if probeErr == nil && len(fableWindows) > 0 {
			if err == nil || fableProbeHasPrimaryWindows(fableWindows) {
				windows = mergeUsageWindows(windows, fableWindows)
			}
		} else if err != nil {
			if probeErr != nil {
				return nil, probeErr
			}
			return nil, err
		}
	}
	if err != nil && len(windows) == 0 {
		return nil, err
	}
	return windows, nil
}

func fableProbeHasPrimaryWindows(windows []accounts.UsageWindow) bool {
	for _, window := range windows {
		if window.Name == "5h" || window.Name == "7d" {
			return true
		}
	}
	return false
}

func mergeUsageWindows(base, extra []accounts.UsageWindow) []accounts.UsageWindow {
	out := append([]accounts.UsageWindow(nil), base...)
	index := make(map[string]int, len(out))
	for i, window := range out {
		index[usageWindowMergeKey(window)] = i
	}
	for _, window := range extra {
		key := usageWindowMergeKey(window)
		if i, ok := index[key]; ok {
			out[i] = window
			continue
		}
		index[key] = len(out)
		out = append(out, window)
	}
	return out
}

func usageWindowNamed(windows []accounts.UsageWindow, name string) bool {
	for _, window := range windows {
		if window.Name == name {
			return true
		}
	}
	return false
}

func usageWindowMergeKey(window accounts.UsageWindow) string {
	return strings.ToLower(window.Name) + "\x00" + strings.ToLower(window.Feature)
}

func (r srRunner) ensureSwitchableForFreshUsage(ctx context.Context, account accounts.StoredCodexAccount) error {
	if account.IsAPIKey() {
		return nil
	}
	if r.client == nil {
		return nil
	}
	details, err := accounts.FetchCodexUsageDetails(ctx, r.client, accountFromStored(account))
	if err != nil {
		return nil
	}
	cooked, reason := cookedFromWindows(details.Windows)
	if cooked {
		return fmt.Errorf("cannot switch to %s: account is cooked (%s)", account.Email, reason)
	}
	tempCooked, reason := tempCookedFromWindows(details.Windows)
	if tempCooked {
		return fmt.Errorf("cannot switch to %s: account is temporarily cooked (%s)", account.Email, reason)
	}
	return nil
}

func rankUsageRows(rows []srUsageRow) {
	sort.SliceStable(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		ao, bo := usageProviderOrder(a), usageProviderOrder(b)
		if ao != bo {
			return ao < bo
		}
		if usageProvider(a) == accounts.ProviderClaude {
			return claudeUsageRowLess(a, b)
		}
		return codexUsageRowLess(a, b)
	})
	recommended := false
	for i := range rows {
		rows[i].gtoRecommended = false
		if !recommended && recommendedForNewSession(rows[i]) {
			rows[i].gtoRecommended = true
			recommended = true
		}
		rows[i].gtoReason = gtoReason(rows[i])
	}
}

func codexUsageRowLess(a, b srUsageRow) bool {
	if (a.err != nil) != (b.err != nil) {
		return a.err == nil
	}
	at, bt := usageRowTier(a), usageRowTier(b)
	if at != bt {
		return at < bt
	}
	au, bu := usableForNewSession(a.score), usableForNewSession(b.score)
	if au != bu {
		return au
	}
	if au && bu && a.score.ExpiryPressure != b.score.ExpiryPressure {
		return a.score.ExpiryPressure > b.score.ExpiryPressure
	}
	if a.score.Headroom != b.score.Headroom {
		return a.score.Headroom > b.score.Headroom
	}
	if a.score.ShortResetAfterSeconds != b.score.ShortResetAfterSeconds {
		if a.score.ShortResetAfterSeconds == 0 {
			return false
		}
		if b.score.ShortResetAfterSeconds == 0 {
			return true
		}
		return a.score.ShortResetAfterSeconds < b.score.ShortResetAfterSeconds
	}
	return a.email < b.email
}

func claudeUsageRowLess(a, b srUsageRow) bool {
	if (a.err != nil) != (b.err != nil) {
		return a.err == nil
	}
	if a.active != b.active {
		return a.active
	}
	au, bu := usableForNewSession(a.score), usableForNewSession(b.score)
	if au != bu {
		return au
	}
	if a.score.Headroom != b.score.Headroom {
		return a.score.Headroom > b.score.Headroom
	}
	if a.score.ShortResetAfterSeconds != b.score.ShortResetAfterSeconds {
		if a.score.ShortResetAfterSeconds == 0 {
			return false
		}
		if b.score.ShortResetAfterSeconds == 0 {
			return true
		}
		return a.score.ShortResetAfterSeconds < b.score.ShortResetAfterSeconds
	}
	return a.email < b.email
}

func usageProvider(row srUsageRow) accounts.Provider {
	if row.provider == "" {
		return accounts.ProviderCodex
	}
	return row.provider
}

func usageProviderOrder(row srUsageRow) int {
	switch usageProvider(row) {
	case accounts.ProviderCodex:
		return 0
	case accounts.ProviderClaude:
		return 1
	default:
		return 2
	}
}

func usageProviderLabel(row srUsageRow) string {
	switch usageProvider(row) {
	case accounts.ProviderCodex:
		return "Codex accounts"
	case accounts.ProviderClaude:
		return "Claude profiles"
	default:
		return strings.Title(string(usageProvider(row))) + " accounts"
	}
}

func usageRowTier(row srUsageRow) int {
	if row.provider != "" && row.provider != accounts.ProviderCodex {
		return 5
	}
	if row.authMode == accounts.AuthModeOAuth && usableForNewSession(row.score) {
		return 0
	}
	if row.authMode == accounts.AuthModeAPIKey {
		return 1
	}
	if row.authMode == accounts.AuthModeOAuth && !row.tempCooked && !row.cooked {
		return 2
	}
	if row.authMode == accounts.AuthModeOAuth && row.tempCooked {
		return 3
	}
	if row.authMode == accounts.AuthModeOAuth && row.cooked {
		return 4
	}
	return 5
}

func recommendedForNewSession(row srUsageRow) bool {
	if row.provider != "" && row.provider != accounts.ProviderCodex {
		return false
	}
	if row.err != nil || row.cooked || row.tempCooked {
		return false
	}
	if row.authMode == accounts.AuthModeAPIKey {
		return true
	}
	return row.authMode == accounts.AuthModeOAuth && usableForNewSession(row.score)
}

func usableForNewSession(score selectacct.Score) bool {
	return score.Headroom >= selectacct.MinNewSessionHeadroom && score.ShortHeadroom >= selectacct.MinNewSessionHeadroom
}

func exhaustedForNewSession(score selectacct.Score) bool {
	return score.Headroom <= 0 || score.ShortHeadroom <= 0
}

func gtoReason(row srUsageRow) string {
	if row.err != nil {
		return "usage unavailable"
	}
	if row.cooked {
		return "cooked, cannot switch"
	}
	if row.tempCooked {
		return "temporarily cooked, cannot start new session"
	}
	if row.authMode == accounts.AuthModeAPIKey {
		return "API key fallback"
	}
	left := fmt.Sprintf("%d%% bottleneck left", int(row.score.Headroom*100+0.5))
	if !usableForNewSession(row.score) {
		return fmt.Sprintf("%s, protected below %d%%", left, int(selectacct.MinNewSessionHeadroom*100))
	}
	if row.score.ShortResetAfterSeconds > 0 {
		return fmt.Sprintf("%s, 5h resets in %s", left, formatDuration(row.score.ShortResetAfterSeconds))
	}
	return left
}

func displayUsageRows(out io.Writer, rows []srUsageRow, numbered bool) {
	if len(rows) == 0 {
		fmt.Fprintln(out, "No accounts configured. Run 'subrouter add' to add one.")
		return
	}
	colored := colorEnabled(out)
	displayUsageRowsGrid(out, rows, numbered, colored)
}

func displayUsageRowsGrid(out io.Writer, rows []srUsageRow, numbered bool, colored bool) {
	fmt.Fprintln(out)
	currentGroup := ""
	accountRowIndex := 0
	for i, row := range rows {
		group := usageProviderLabel(row)
		columns := usageGridColumns(out, numbered, usageProvider(row))
		if group != currentGroup {
			if currentGroup != "" {
				fmt.Fprintln(out)
			}
			printUsageGridGroup(out, columns, group, colored)
			printUsageGridLine(out, columns, func(col usageGridColumn) usageGridCell {
				return usageGridCell{Text: col.Title, Style: ansiDim}
			}, colored, "")
			printUsageGridSeparator(out, columns, colored)
			currentGroup = group
		}
		rowIndex := ""
		if numbered {
			rowIndex = strconv.Itoa(i + 1)
		}
		values := usageGridValues(row, rowIndex)
		printUsageGridLine(out, columns, func(col usageGridColumn) usageGridCell {
			return values[col.Key]
		}, colored, usageGridRowStyle(accountRowIndex))
		accountRowIndex++
	}
	fmt.Fprintln(out)
	if usageRowsHaveErrors(rows) {
		for _, row := range rows {
			if row.err != nil {
				fmt.Fprintf(out, "  %s %s: %s%s\n",
					style(colored, ansiBold+ansiWhite, displayAccountName(row.email)),
					style(colored, ansiDim, "["+string(usageProvider(row))+"]"),
					style(colored, ansiRed, row.err.Error()),
					style(colored, ansiDim, usageRowErrorHint(row)))
			}
		}
		fmt.Fprintln(out)
	}
}

// printAccountCountSummary prints a one-line total across providers so the user
// can see how many accounts are configured at a glance, e.g.
// "Total: 23 Codex accounts, 2 Claude profiles (25 accounts)".
func printAccountCountSummary(out io.Writer, rows []srUsageRow) {
	if len(rows) <= 1 {
		return
	}
	order := []accounts.Provider{}
	counts := map[accounts.Provider]int{}
	for _, row := range rows {
		p := usageProvider(row)
		if _, seen := counts[p]; !seen {
			order = append(order, p)
		}
		counts[p]++
	}
	parts := make([]string, 0, len(order))
	for _, p := range order {
		parts = append(parts, fmt.Sprintf("%d %s", counts[p], providerCountNoun(p, counts[p])))
	}
	colored := colorEnabled(out)
	summary := fmt.Sprintf("Total: %s (%d accounts)", strings.Join(parts, ", "), len(rows))
	fmt.Fprintln(out, style(colored, ansiDim, summary))
}

func providerCountNoun(provider accounts.Provider, n int) string {
	switch provider {
	case accounts.ProviderCodex:
		return "Codex"
	case accounts.ProviderClaude:
		if n == 1 {
			return "Claude profile"
		}
		return "Claude profiles"
	default:
		return strings.Title(string(provider))
	}
}

func usageRowErrorHint(row srUsageRow) string {
	if row.err == nil || !authErrorNeedsReadd(row.err) {
		return ""
	}
	return " (re-add with: " + providerReaddCommand(usageProvider(row)) + ")"
}

func providerReaddCommand(provider accounts.Provider) string {
	switch provider {
	case accounts.ProviderClaude:
		return "sr claude add"
	case "gemini":
		return "sr gemini add"
	default:
		return "sr add"
	}
}

func authErrorNeedsReadd(err error) bool {
	msg := strings.ToLower(err.Error())
	for _, marker := range []string{"401", "unauthorized", "invalid_grant", "reauth", "refresh failed", "access_expired"} {
		if strings.Contains(msg, marker) {
			return true
		}
	}
	return false
}

func printUsageGridGroup(out io.Writer, columns []usageGridColumn, label string, colored bool) {
	fmt.Fprintln(out, style(colored, ansiBold+ansiDim, fitCell(label, usageGridWidth(columns))))
}

func usageGridColumns(out io.Writer, numbered bool, provider accounts.Provider) []usageGridColumn {
	termWidth := terminalColumns(out)
	accountWidth := 22
	planWidth := 6
	stateWidth := 10
	pickWidth := 22
	windowWidth := 9
	creditsWidth := 7
	sparkWidth := 8
	if termWidth < 100 {
		accountWidth = 20
		planWidth = 4
		stateWidth = 14
		pickWidth = 16
		windowWidth = 7
	}

	columns := []usageGridColumn{}
	if numbered {
		columns = append(columns, usageGridColumn{Key: "#", Title: "#", Width: 3})
	}
	columns = append(columns,
		usageGridColumn{Key: "Account", Title: "Account", Width: accountWidth},
		usageGridColumn{Key: "Plan", Title: "Plan", Width: planWidth},
		usageGridColumn{Key: "State", Title: "State", Width: stateWidth},
		usageGridColumn{Key: "Pick", Title: "Use", Width: pickWidth},
	)
	if provider == accounts.ProviderClaude {
		columns = append(columns,
			usageGridColumn{Key: "Session", Title: "Session", Width: windowWidth},
			usageGridColumn{Key: "Weekly", Title: "Weekly", Width: windowWidth},
		)
		columns = appendUsageGridColumnIfFits(columns, usageGridColumn{Key: "Fable wk", Title: "Fable wk", Width: sparkWidth}, termWidth)
		columns = appendUsageGridColumnIfFits(columns, usageGridColumn{Key: "Opus wk", Title: "Opus wk", Width: sparkWidth}, termWidth)
		columns = appendUsageGridColumnIfFits(columns, usageGridColumn{Key: "Sonnet wk", Title: "Sonnet wk", Width: 9}, termWidth)
		columns = appendUsageGridColumnIfFits(columns, usageGridColumn{Key: "Extra", Title: "Extra", Width: sparkWidth}, termWidth)
	} else {
		columns = append(columns,
			usageGridColumn{Key: "5h", Title: "5h", Width: windowWidth},
			usageGridColumn{Key: "7d", Title: "7d", Width: windowWidth},
		)
		columns = appendUsageGridColumnIfFits(columns, usageGridColumn{Key: "Reset", Title: "1x reset", Width: 8}, termWidth)
		columns = appendUsageGridColumnIfFits(columns, usageGridColumn{Key: "Credits", Title: "$", Width: creditsWidth}, termWidth)
		columns = appendUsageGridColumnIfFits(columns, usageGridColumn{Key: "Spark", Title: "Spark", Width: sparkWidth}, termWidth)
		columns = appendUsageGridColumnIfFits(columns, usageGridColumn{Key: "Spark wk", Title: "Spark wk", Width: sparkWidth}, termWidth)
	}

	extra := termWidth - usageGridWidth(columns)
	if extra <= 0 {
		return columns
	}
	extra = widenUsageGridColumn(columns, "Account", extra, 36)
	extra = widenUsageGridColumn(columns, "Pick", extra, 34)
	extra = widenUsageGridColumn(columns, "State", extra, 14)
	extra = widenUsageGridColumn(columns, "Fable wk", extra, 10)
	extra = widenUsageGridColumn(columns, "Sonnet wk", extra, 10)
	extra = widenUsageGridColumn(columns, "Spark wk", extra, 10)
	extra = widenUsageGridColumn(columns, "Weekly", extra, 12)
	_ = widenUsageGridColumn(columns, "7d", extra, 12)
	return columns
}

func usageGridValues(row srUsageRow, rowIndex string) map[string]usageGridCell {
	return map[string]usageGridCell{
		"#":         {Text: rowIndex, Style: ansiDim},
		"Account":   {Text: displayAccountName(row.email), Style: ansiBold + ansiWhite},
		"Plan":      {Text: row.planType, Style: ansiDim},
		"State":     {Text: usageGridState(row), Style: usageGridStateColor(row)},
		"Pick":      {Text: compactPickReason(row), Style: usageGridPickColor(row)},
		"5h":        usageGridShortWindowCell(row),
		"7d":        usageGridWindowCell(row.windows, isLongQuotaWindow),
		"Reset":     usageGridResetCell(row),
		"Spark":     usageGridShortNamedWindowCell(row),
		"Spark wk":  usageGridNamedWindowCell(row.windows, true),
		"Credits":   usageGridCreditsCell(row),
		"Session":   usageGridWindowCell(row.windows, isClaudeSessionWindow),
		"Weekly":    usageGridWindowCell(row.windows, isClaudeWeeklyWindow),
		"Fable wk":  usageGridWindowCell(row.windows, isClaudeOAuthAppsWeeklyWindow),
		"Opus wk":   usageGridWindowCell(row.windows, isClaudeOpusWeeklyWindow),
		"Sonnet wk": usageGridWindowCell(row.windows, isClaudeSonnetWeeklyWindow),
		"Extra":     usageGridWindowCell(row.windows, isClaudeExtraWindow),
	}
}

func usageGridResetCell(row srUsageRow) usageGridCell {
	if usageProvider(row) != accounts.ProviderCodex || row.authMode != accounts.AuthModeOAuth {
		return usageGridCell{}
	}
	info := row.complimentaryReset
	if info == nil || !info.Known {
		return usageGridCell{Text: "unknown", Style: ansiDim}
	}
	if info.Eligible != nil && !*info.Eligible {
		return usageGridCell{Text: "not elig", Style: ansiDim}
	}
	// Prefer the concrete count over a bare "avail" so >1 credits are visible.
	if info.Remaining != nil {
		if *info.Remaining > 0 {
			return usageGridCell{Text: fmt.Sprintf("%d left", *info.Remaining), Style: ansiGreen}
		}
		return usageGridCell{Text: "used", Style: ansiYellow}
	}
	if info.Available {
		return usageGridCell{Text: "avail", Style: ansiGreen}
	}
	if info.Consumed {
		return usageGridCell{Text: "used", Style: ansiYellow}
	}
	if strings.TrimSpace(info.Status) != "" {
		return usageGridCell{Text: compactResetStatus(info.Status), Style: ansiDim}
	}
	return usageGridCell{Text: "unknown", Style: ansiDim}
}

func compactResetStatus(status string) string {
	status = strings.ToLower(strings.TrimSpace(status))
	status = strings.ReplaceAll(status, "_", " ")
	if len(status) <= 8 {
		return status
	}
	return status[:8]
}

type usageGridColumn struct {
	Key   string
	Title string
	Width int
}

type usageGridCell struct {
	Text  string
	Style string
}

func usageGridWidth(columns []usageGridColumn) int {
	if len(columns) == 0 {
		return 0
	}
	width := 2 * (len(columns) - 1)
	for _, col := range columns {
		width += col.Width
	}
	return width
}

func appendUsageGridColumnIfFits(columns []usageGridColumn, column usageGridColumn, termWidth int) []usageGridColumn {
	next := append(append([]usageGridColumn{}, columns...), column)
	if usageGridWidth(next) <= termWidth {
		return next
	}
	return columns
}

func widenUsageGridColumn(columns []usageGridColumn, title string, extra int, maxWidth int) int {
	if extra <= 0 {
		return 0
	}
	for i := range columns {
		if columns[i].Key != title {
			continue
		}
		add := min(extra, maxWidth-columns[i].Width)
		if add > 0 {
			columns[i].Width += add
			extra -= add
		}
		return extra
	}
	return extra
}

func printUsageGridLine(out io.Writer, columns []usageGridColumn, value func(usageGridColumn) usageGridCell, colored bool, rowStyle string) {
	for i, col := range columns {
		if i > 0 {
			fmt.Fprint(out, style(colored, rowStyle, "  "))
		}
		cell := value(col)
		text := fitCell(cell.Text, col.Width)
		if cell.Style != "" || rowStyle != "" {
			text = style(colored, rowStyle+cell.Style, text)
		}
		fmt.Fprint(out, text)
	}
	fmt.Fprintln(out)
}

func usageGridRowStyle(rowIndex int) string {
	if rowIndex%2 == 1 {
		return ansiBGRowAlt
	}
	return ""
}

func printUsageGridSeparator(out io.Writer, columns []usageGridColumn, colored bool) {
	for i, col := range columns {
		if i > 0 {
			fmt.Fprint(out, "  ")
		}
		fmt.Fprint(out, style(colored, ansiDim, strings.Repeat("─", col.Width)))
	}
	fmt.Fprintln(out)
}

func usageGridState(row srUsageRow) string {
	var states []string
	if row.active && row.gtoRecommended {
		states = append(states, "active rec")
	} else {
		if row.active {
			states = append(states, "active")
		}
		if row.gtoRecommended {
			states = append(states, "rec")
		}
	}
	if row.cooked {
		states = append(states, "cooked")
	} else if row.tempCooked {
		states = append(states, "temp")
	}
	if row.err != nil {
		states = append(states, "error")
	}
	return strings.Join(states, ", ")
}

func usageGridStateColor(row srUsageRow) string {
	switch {
	case row.err != nil || row.cooked:
		return ansiRed
	case row.tempCooked:
		return ansiYellow
	case row.gtoRecommended:
		return ansiGreen
	case row.active:
		return ansiCyan
	default:
		return ""
	}
}

func usageGridPickColor(row srUsageRow) string {
	switch {
	case row.err != nil || row.cooked:
		return ansiRed
	case row.tempCooked || !recommendedForNewSession(row):
		if usageProvider(row) == accounts.ProviderClaude {
			return ""
		}
		return ansiYellow
	case row.gtoRecommended:
		return ansiGreen
	default:
		return ""
	}
}

func usageGridError(row srUsageRow) string {
	if row.err == nil {
		return ""
	}
	return "error"
}

func compactPickReason(row srUsageRow) string {
	if row.err != nil {
		return "usage unavailable"
	}
	if row.cooked {
		return "cooked, cannot switch"
	}
	if row.tempCooked {
		return "temp cooked, cannot start"
	}
	if row.authMode == accounts.AuthModeAPIKey {
		return "API key fallback"
	}
	if usageProvider(row) == accounts.ProviderClaude && len(row.windows) == 0 {
		return "Claude profile"
	}
	left := fmt.Sprintf("%d%% left", int(row.score.Headroom*100+0.5))
	suffix := exhaustedModelSuffix(row.windows)
	if !usableForNewSession(row.score) {
		return fmt.Sprintf("%s, protected < %d%%%s", left, int(selectacct.MinNewSessionHeadroom*100), suffix)
	}
	if row.score.ShortResetAfterSeconds > 0 {
		if usageProvider(row) == accounts.ProviderClaude {
			return fmt.Sprintf("%s, session reset %s%s", left, formatDuration(row.score.ShortResetAfterSeconds), suffix)
		}
		return fmt.Sprintf("%s, 5h reset %s%s", left, formatDuration(row.score.ShortResetAfterSeconds), suffix)
	}
	return left + suffix
}

// exhaustedModelSuffix names any per-model quota pools that are fully consumed
// (e.g. Fable) so the Use column makes clear the account still works for other
// models even when one model's weekly pool is out.
func exhaustedModelSuffix(windows []accounts.UsageWindow) string {
	var out []string
	seen := map[string]bool{}
	for _, window := range windows {
		if !isModelScopedWindow(window) || clampUsagePercent(window.UsedPercent) < 100 {
			continue
		}
		label := modelScopedWindowLabel(window)
		if label == "" || seen[label] {
			continue
		}
		seen[label] = true
		out = append(out, label)
	}
	if len(out) == 0 {
		return ""
	}
	return " (" + strings.Join(out, "/") + " out)"
}

func modelScopedWindowLabel(window accounts.UsageWindow) string {
	f := strings.ToLower(window.Feature)
	switch {
	case strings.Contains(f, "fable"):
		return "Fable"
	case strings.Contains(f, "opus"):
		return "Opus"
	case strings.Contains(f, "sonnet"):
		return "Sonnet"
	case strings.Contains(f, "haiku"):
		return "Haiku"
	default:
		return window.Feature
	}
}

func usageGridShortWindowCell(row srUsageRow) usageGridCell {
	if longQuotaSaturatedMatching(row.windows, func(window accounts.UsageWindow) bool {
		return !isSparkWindow(window)
	}) {
		return usageGridCell{}
	}
	return usageGridWindowCell(row.windows, isShortQuotaWindow)
}

func usageGridShortNamedWindowCell(row srUsageRow) usageGridCell {
	if longQuotaSaturatedMatching(row.windows, isSparkWindow) {
		return usageGridCell{}
	}
	return usageGridNamedWindowCell(row.windows, false)
}

func longQuotaSaturatedMatching(windows []accounts.UsageWindow, match func(accounts.UsageWindow) bool) bool {
	for _, window := range windows {
		if isModelScopedWindow(window) {
			continue
		}
		if match(window) && isLongQuotaWindow(window) && clampUsagePercent(window.UsedPercent) >= 100 {
			return true
		}
	}
	return false
}

func usageGridWindowCell(windows []accounts.UsageWindow, match func(accounts.UsageWindow) bool) usageGridCell {
	for _, window := range windows {
		if match(window) && !isSparkWindow(window) {
			return usageGridWindowStatusCell(window)
		}
	}
	return usageGridCell{}
}

func usageGridNamedWindowCell(windows []accounts.UsageWindow, weekly bool) usageGridCell {
	for _, window := range windows {
		name := strings.ToLower(windowLabel(window))
		if !isSparkWindow(window) {
			continue
		}
		isWeekly := strings.Contains(name, "weekly")
		if isWeekly == weekly {
			return usageGridWindowStatusCell(window)
		}
	}
	return usageGridCell{}
}

func isSparkWindow(window accounts.UsageWindow) bool {
	return strings.Contains(strings.ToLower(windowLabel(window)), "codex-spark")
}

func usageGridWindowStatusCell(window accounts.UsageWindow) usageGridCell {
	return usageGridCell{Text: compactWindowStatus(window), Style: usageColor(window.UsedPercent)}
}

func compactWindowStatus(window accounts.UsageWindow) string {
	left := compactPercentLeft(window.UsedPercent)
	if window.ResetAfterSeconds <= 0 {
		return left
	}
	return left + "/" + strings.ReplaceAll(formatDuration(window.ResetAfterSeconds), " ", "")
}

func compactPercentLeft(used float64) string {
	left := 100 - used
	if left < 0 {
		left = 0
	}
	return fmt.Sprintf("%.0f%%", left)
}

func usageGridCreditsCell(row srUsageRow) usageGridCell {
	if row.credits != nil {
		if row.credits.Unlimited {
			return usageGridCell{Text: "unlimited", Style: ansiGreen}
		}
		if row.credits.Balance != "" {
			return usageGridCell{Text: "$" + row.credits.Balance}
		}
	}
	if row.apiKeySpend != nil {
		return usageGridCell{Text: fmtUSD(row.apiKeySpend.WeekUSD) + "/7d", Style: ansiDim}
	}
	return usageGridCell{}
}

func usageRowsHaveErrors(rows []srUsageRow) bool {
	for _, row := range rows {
		if row.err != nil {
			return true
		}
	}
	return false
}

func displayAPIKeySpend(out io.Writer, snapshot *accounts.APIKeyUsageSnapshot, width int, colored bool) {
	scope := "org-wide"
	if snapshot.ProjectID != "" {
		scope = "proj " + firstNonEmpty(snapshot.ProjectName, snapshot.ProjectID)
	}
	fmt.Fprintf(out, "  %s: %s %s\n", style(colored, ansiDim, pad("scope", width)), scope, style(colored, ansiDim, "via "+snapshot.AdminKeyLabel+", "+formatAge(snapshot.FetchedAt)))
	today := fmtUSD(snapshot.TodayUSD)
	if snapshot.TodayCostEstimated {
		today = "~" + today + " est"
	}
	fmt.Fprintf(out, "  %s: %s %s\n", style(colored, ansiDim, pad("today", width)), today, style(colored, ansiDim, "("+fmtTokens(snapshot.TodayTokens)+" tok)"))
	fmt.Fprintf(out, "  %s: %s %s\n", style(colored, ansiDim, pad("7d", width)), fmtUSD(snapshot.WeekUSD), style(colored, ansiDim, "("+fmtTokens(snapshot.WeekTokens)+" tok)"))
	fmt.Fprintf(out, "  %s: %s %s\n", style(colored, ansiDim, pad("30d", width)), fmtUSD(snapshot.MonthUSD), style(colored, ansiDim, "("+fmtTokens(snapshot.MonthTokens)+" tok)"))
	if snapshot.TopModel != nil && snapshot.TopModel.Tokens > 0 {
		fmt.Fprintf(out, "  %s: %s %s\n", style(colored, ansiDim, pad("top model", width)), snapshot.TopModel.Model, style(colored, ansiDim, "("+fmtTokens(snapshot.TopModel.Tokens)+" tok 30d)"))
	}
}

func parseSRSwitchArgs(args []string, defaults srSwitchOptions) (string, srSwitchOptions, error) {
	opts := defaults
	selector := ""
	for _, arg := range args {
		switch arg {
		case "--restart-codex-gui", "--restart-gui":
			opts.restartCodexGUI = true
		case "--no-restart-codex-gui", "--no-restart-gui":
			opts.restartCodexGUI = false
		default:
			if strings.HasPrefix(arg, "-") {
				return "", opts, fmt.Errorf("unknown switch option: %s", arg)
			}
			if selector != "" {
				return "", opts, fmt.Errorf("unexpected extra account selector: %s", arg)
			}
			selector = arg
		}
	}
	return selector, opts, nil
}

func promptLine(out io.Writer, reader *bufio.Reader, prompt string) (string, error) {
	fmt.Fprint(out, prompt)
	line, err := reader.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	return strings.TrimSpace(line), nil
}

func restartCodexGUI(ctx context.Context) (string, error) {
	if runtime.GOOS != "darwin" {
		return "unsupported", nil
	}
	pids, err := commandOutput(ctx, "pgrep", "-x", "Codex")
	if err != nil {
		if exitError(err) == 1 {
			return "not-running", nil
		}
		return "", fmt.Errorf("pgrep Codex failed: %w", err)
	}
	if strings.TrimSpace(pids) == "" {
		return "not-running", nil
	}
	if _, err := commandOutput(ctx, "pkill", "-x", "Codex"); err != nil && exitError(err) != 1 {
		return "", fmt.Errorf("pkill Codex failed: %w", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		out, err := commandOutput(ctx, "pgrep", "-x", "Codex")
		if exitError(err) == 1 || strings.TrimSpace(out) == "" {
			break
		}
		timer := time.NewTimer(100 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return "", ctx.Err()
		case <-timer.C:
		}
	}
	if _, err := commandOutput(ctx, "open", "-b", "com.openai.codex"); err == nil {
		return "restarted", nil
	}
	if _, err := commandOutput(ctx, "open", "/Applications/Codex.app"); err == nil {
		return "restarted", nil
	}
	return "", fmt.Errorf("failed to reopen Codex.app")
}

func commandOutput(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	body, err := cmd.CombinedOutput()
	return string(body), err
}

func exitError(err error) int {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	return -1
}

func windowLabel(window accounts.UsageWindow) string {
	name := window.Name
	switch name {
	case "primary":
		if window.LimitWindowSeconds >= 3600 {
			return fmt.Sprintf("%dh limit", int((window.LimitWindowSeconds+1800)/3600))
		}
		return fmt.Sprintf("%dm limit", int((window.LimitWindowSeconds+30)/60))
	case "secondary":
		if window.LimitWindowSeconds >= 86400 {
			return fmt.Sprintf("%dd limit", int((window.LimitWindowSeconds+43200)/86400))
		}
		return "Weekly limit"
	}
	if strings.HasSuffix(name, "/primary") {
		return strings.TrimSuffix(name, "/primary")
	}
	if strings.HasSuffix(name, "/secondary") {
		return strings.TrimSuffix(name, "/secondary") + " (weekly)"
	}
	if name == agentclaude.FableWindowName {
		return "Fable (weekly)"
	}
	return name
}

func colorEnabled(out io.Writer) bool {
	if os.Getenv("SR_NO_COLOR") != "" {
		return false
	}
	if os.Getenv("CX_NO_COLOR") != "" {
		return false
	}
	if force := os.Getenv("FORCE_COLOR"); force != "" && force != "0" {
		return true
	}
	if os.Getenv("TERM") == "dumb" {
		return false
	}
	file, ok := out.(*os.File)
	if !ok {
		return false
	}
	info, err := file.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

func style(enabled bool, code, value string) string {
	if !enabled || value == "" {
		return value
	}
	return code + value + ansiReset
}

func usageColor(usedPercent float64) string {
	switch {
	case usedPercent >= 90:
		return ansiRed
	case usedPercent >= 70:
		return ansiYellow
	default:
		return ansiGreen
	}
}

func barBGColor(usedPercent float64) string {
	switch {
	case usedPercent >= 90:
		return ansiBGRed
	case usedPercent >= 70:
		return ansiBGYellow
	default:
		return ansiBGGreen
	}
}

func renderBar(usedPercent float64, colored bool) string {
	width := 22
	filled := int(usedPercent/100*float64(width) + 0.5)
	if filled < 0 {
		filled = 0
	}
	if filled > width {
		filled = width
	}
	if colored {
		return barBGColor(usedPercent) + strings.Repeat(" ", filled) + ansiBGGray + strings.Repeat(" ", width-filled) + ansiReset
	}
	return "[" + strings.Repeat("=", filled) + strings.Repeat(" ", width-filled) + "]"
}

func formatPercentLeft(used float64) string {
	left := 100 - used
	if left < 0 {
		left = 0
	}
	return fmt.Sprintf("%.1f%%", left)
}

func formatDuration(seconds int64) string {
	if seconds <= 0 {
		return "now"
	}
	days := seconds / 86400
	hours := (seconds % 86400) / 3600
	mins := (seconds % 3600) / 60
	var parts []string
	if days > 0 {
		parts = append(parts, fmt.Sprintf("%dd", days))
	}
	if hours > 0 {
		parts = append(parts, fmt.Sprintf("%dh", hours))
	}
	if mins > 0 && days == 0 {
		parts = append(parts, fmt.Sprintf("%dm", mins))
	}
	if len(parts) == 0 {
		return "<1m"
	}
	return strings.Join(parts, " ")
}

func displayAccountName(email string) string {
	if strings.HasPrefix(email, "apikey:") {
		return strings.TrimPrefix(email, "apikey:") + " (api key)"
	}
	return email
}

func formatDate(value string) string {
	t, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return value
	}
	return t.Format("2006-01-02")
}

func formatAge(value string) string {
	t, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return value
	}
	age := time.Since(t)
	switch {
	case age < time.Minute:
		return fmt.Sprintf("%ds ago", int(age.Seconds()))
	case age < time.Hour:
		return fmt.Sprintf("%dm ago", int(age.Minutes()))
	case age < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(age.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(age.Hours()/24))
	}
}

func fmtUSD(n float64) string {
	switch {
	case n == 0:
		return "$0"
	case n < 0.01:
		return fmt.Sprintf("$%.4f", n)
	case n < 1:
		return fmt.Sprintf("$%.3f", n)
	default:
		return fmt.Sprintf("$%.2f", n)
	}
}

func fmtTokens(n int64) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	case n >= 1_000:
		return fmt.Sprintf("%.1fK", float64(n)/1_000)
	default:
		return strconv.FormatInt(n, 10)
	}
}

func maskSecret(value string) string {
	if len(value) <= 18 {
		return value
	}
	return value[:12] + "***" + value[len(value)-6:]
}

func pad(value string, width int) string {
	if width <= len(value) {
		return value
	}
	return value + strings.Repeat(" ", width-len(value))
}

func fitCell(value string, width int) string {
	value = strings.ReplaceAll(strings.TrimSpace(value), "\n", " ")
	if width <= 0 {
		return ""
	}
	if len(value) > width {
		if width <= 3 {
			return value[:width]
		}
		return value[:width-3] + "..."
	}
	return value + strings.Repeat(" ", width-len(value))
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func indexOf(values []string, needle string) int {
	for i, value := range values {
		if value == needle {
			return i
		}
	}
	return -1
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
