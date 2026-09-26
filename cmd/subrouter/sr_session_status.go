package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	agentclaude "github.com/manaflow-ai/subrouter/internal/agents/claude"
	"github.com/manaflow-ai/subrouter/session"
)

const (
	// sessionStatusLineCommand and sessionNotifyCommand are hidden entry
	// points that sr injects into pooled launches: Claude Code's statusLine
	// hook and Codex's notify hook. They work under any program name.
	sessionStatusLineCommand = "__session-statusline"
	sessionNotifyCommand     = "__session-notify"

	// sessionStatusDisableEnv turns off the injected Claude status line.
	sessionStatusDisableEnv = "SUBROUTER_CLAUDE_STATUSLINE"

	// sessionUsageCacheTTL lets every running session share one usage-status
	// read. The server itself caches usage for 30s and refreshes it on its own
	// --fetch-usage schedule, so this never adds provider polling.
	sessionUsageCacheTTL = 20 * time.Second
	// sessionStatusTimeout bounds one status line render end to end.
	sessionStatusTimeout = 2500 * time.Millisecond
	// sessionUserStatusLineTimeout bounds a chained user status line command.
	sessionUserStatusLineTimeout = 2 * time.Second
)

// serverSessionAssignment mirrors the server's /_subrouter/sessions rows.
type serverSessionAssignment struct {
	AgentType string    `json:"agent_type"`
	SessionID string    `json:"session_id"`
	AccountID string    `json:"account_id"`
	UpdatedAt time.Time `json:"updated_at"`
	Active    bool      `json:"active"`
}

type sessionUsageCache struct {
	FetchedAt time.Time                 `json:"fetched_at"`
	Statuses  []remoteServerUsageStatus `json:"statuses"`
}

// ledgerServer resolves the server a launch was routed through. An empty
// name means whatever the launcher would pick now.
func (r srRunner) ledgerServer(ctx context.Context, name string) (srServerConfig, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		server, remote, err := r.selectedRemoteServer()
		if err != nil {
			return srServerConfig{}, err
		}
		if remote {
			return server, nil
		}
		name = "local"
	}
	if isLocalServerName(name) {
		return r.localServingServer()
	}
	return r.namedRemoteServer(ctx, defaultSRServerStore(r.store), name)
}

// launchServerName is the name recorded for a launch: the selected remote,
// or "local".
func (r srRunner) launchServerName() string {
	server, remote, err := r.selectedRemoteServer()
	if err != nil || !remote {
		return "local"
	}
	return server.Name
}

func (r srRunner) fetchServerSessionAssignments(ctx context.Context, server srServerConfig, agent, sessionID string) ([]serverSessionAssignment, error) {
	baseURL, err := serverControlBaseURL(server)
	if err != nil {
		return nil, err
	}
	query := url.Values{}
	if agent != "" {
		query.Set("agent_type", agent)
	}
	if sessionID != "" {
		query.Set("session_id", sessionID)
	}
	endpoint := baseURL + "/_subrouter/sessions"
	if len(query) > 0 {
		endpoint += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, redactServerRequestError(err, server)
	}
	addServerAdminAuth(req, server)
	client, err := r.securedRequestClientForServer(server, baseURL, sessionStatusTimeout)
	if err != nil {
		return nil, err
	}
	res, err := client.Do(req)
	if err != nil {
		return nil, redactServerRequestError(err, server)
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, res.Body)
		return nil, fmt.Errorf("server sessions failed: %s", res.Status)
	}
	var out []serverSessionAssignment
	if err := json.NewDecoder(io.LimitReader(res.Body, 8<<20)).Decode(&out); err != nil {
		return nil, err
	}
	return out, nil
}

// assignmentForSession finds the server's current assignment for one agent
// session. Older servers ignore the query filter, so the client filters too.
func assignmentForSession(assignments []serverSessionAssignment, agent, sessionID string) (serverSessionAssignment, bool) {
	agent = session.NormalizeAgentType(agent)
	want := session.StickySessionID(agent, sessionID)
	var best serverSessionAssignment
	found := false
	for _, assignment := range assignments {
		if session.NormalizeAgentType(assignment.AgentType) != agent {
			continue
		}
		if session.StickySessionID(agent, assignment.SessionID) != want {
			continue
		}
		if !found || assignment.UpdatedAt.After(best.UpdatedAt) {
			best = assignment
			found = true
		}
	}
	return best, found
}

func sessionUsageCachePath(ledger sessionLedger, server srServerConfig) string {
	key := strings.TrimSpace(server.Name) + "\x00" + strings.TrimSpace(server.URL)
	return filepath.Join(ledger.dir, "cache", "usage-"+strings.TrimSuffix(sessionFileName(key), ".json")+".json")
}

// cachedServerUsage returns the server's usage-status rows, shared across
// every status line render for sessionUsageCacheTTL. On a failed fetch it
// falls back to the last good copy and reports it as stale.
func (r srRunner) cachedServerUsage(ctx context.Context, ledger sessionLedger, server srServerConfig) ([]remoteServerUsageStatus, time.Time, bool) {
	path := sessionUsageCachePath(ledger, server)
	var cache sessionUsageCache
	haveCache, _ := readLedgerJSON(path, &cache)
	now := ledger.clock()
	if haveCache && now.Sub(cache.FetchedAt) < sessionUsageCacheTTL && now.Sub(cache.FetchedAt) >= 0 {
		return cache.Statuses, cache.FetchedAt, true
	}
	statuses, supported, err := r.fetchServerUsageStatuses(ctx, server)
	if err != nil || !supported {
		if haveCache {
			return cache.Statuses, cache.FetchedAt, false
		}
		return nil, time.Time{}, false
	}
	cache = sessionUsageCache{FetchedAt: now, Statuses: statuses}
	_ = ledger.writeJSON(path, cache)
	return statuses, now, true
}

func usageStatusForAccount(statuses []remoteServerUsageStatus, provider accounts.Provider, accountID string) (remoteServerUsageStatus, bool) {
	for _, status := range statuses {
		if status.ID == accountID && (provider == "" || status.Provider == provider) {
			return status, true
		}
	}
	for _, status := range statuses {
		if status.ID == accountID {
			return status, true
		}
	}
	return remoteServerUsageStatus{}, false
}

func accountDisplayLabel(status remoteServerUsageStatus, accountID string) string {
	if label := serverUsageDisplayAccount(status); label != "" {
		return label
	}
	for _, candidate := range []string{status.Label, status.Email} {
		if candidate = strings.TrimSpace(candidate); candidate != "" {
			return candidate
		}
	}
	return accountID
}

// accountQuotaWindows picks the account-wide short (5h) and weekly windows,
// skipping per-model pools and the paid extra-usage window.
func accountQuotaWindows(windows []accounts.UsageWindow) (short, weekly *accounts.UsageWindow) {
	for i := range windows {
		window := &windows[i]
		if window.Feature != "" || window.ExtraUsage != nil || strings.Contains(window.Name, "/") || window.Name == agentclaude.FableWindowName {
			continue
		}
		if accounts.IsLongQuotaWindow(*window) {
			if weekly == nil {
				weekly = window
			}
		} else if isShortQuotaWindow(*window) && short == nil {
			short = window
		}
	}
	return short, weekly
}

// sessionStatusView is everything one status line or session row renders.
type sessionStatusView struct {
	AccountID      string
	Label          string
	Plan           string
	Pinned         bool
	Windows        []accounts.UsageWindow
	UsageFetchedAt time.Time
	// Stale marks data from the ledger or cache because the server did not
	// answer this time. Denied means it answered but refused the lookup.
	Stale  bool
	Denied bool
	// PreviousLabel and SwitchedAt describe the most recent account switch.
	PreviousLabel string
	SwitchedAt    time.Time
}

func formatSessionResetTime(reset, now time.Time) string {
	local := reset.Local()
	if reset.Sub(now) < 20*time.Hour {
		return local.Format("15:04")
	}
	return local.Format("Mon 15:04")
}

func formatQuotaWindow(name string, window *accounts.UsageWindow, fetchedAt, now time.Time) string {
	if window == nil {
		return ""
	}
	text := fmt.Sprintf("%s %.0f%%", name, window.UsedPercent)
	if window.ResetAfterSeconds > 0 && !fetchedAt.IsZero() {
		reset := fetchedAt.Add(time.Duration(window.ResetAfterSeconds) * time.Second)
		if reset.After(now) {
			text += " resets " + formatSessionResetTime(reset, now)
		}
	}
	return text
}

func formatAgo(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d/time.Minute))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d/time.Hour))
	default:
		return fmt.Sprintf("%dd ago", int(d/(24*time.Hour)))
	}
}

// renderSessionStatus is the single-line account summary shared by the
// Claude status line and sr sessions.
func renderSessionStatus(view sessionStatusView, now time.Time) string {
	if view.AccountID == "" {
		if view.Denied {
			return "sr: account unknown (server refused the session lookup)"
		}
		if view.Stale {
			return "sr: account unknown (server unreachable)"
		}
		return "sr: waiting for first request"
	}
	label := view.Label
	if label == "" {
		label = view.AccountID
	}
	parts := []string{"sr: " + label}
	if plan := strings.TrimSpace(view.Plan); plan != "" {
		parts[0] += " [" + plan + "]"
	}
	if view.Pinned {
		parts[0] += " (pinned)"
	}
	short, weekly := accountQuotaWindows(view.Windows)
	if text := formatQuotaWindow("5h", short, view.UsageFetchedAt, now); text != "" {
		parts = append(parts, text)
	}
	if text := formatQuotaWindow("wk", weekly, view.UsageFetchedAt, now); text != "" {
		parts = append(parts, text)
	}
	if view.PreviousLabel != "" && !view.SwitchedAt.IsZero() && now.Sub(view.SwitchedAt) < sessionLedgerSwitchNoticeWindow {
		parts = append(parts, "switched from "+view.PreviousLabel+" "+formatAgo(now.Sub(view.SwitchedAt)))
	}
	if view.Stale {
		parts = append(parts, "(stale)")
	}
	return strings.Join(parts, " · ")
}

func viewFromRecord(record sessionRecord) sessionStatusView {
	view := sessionStatusView{}
	if span, ok := record.lastAccount(); ok {
		view.AccountID = span.AccountID
		view.Label = span.Label
		if len(record.Accounts) > 1 {
			previous := record.Accounts[len(record.Accounts)-2]
			view.PreviousLabel = previous.Label
			if view.PreviousLabel == "" {
				view.PreviousLabel = previous.AccountID
			}
			view.SwitchedAt = span.From
		}
	}
	return view
}

func ledgerProvider(agent string) accounts.Provider {
	if sanitizeLedgerAgent(agent) == "codex" {
		return accounts.ProviderCodex
	}
	return accounts.ProviderClaude
}

// observeSession asks the server which account serves the session, records
// the sighting, and returns the view to render. A server failure still
// records usage counters and renders the last known account as stale.
func (r srRunner) observeSession(ctx context.Context, ledger sessionLedger, launch sessionLaunchRecord, agent, sessionID string, counters *sessionUsageCounters) (sessionStatusView, *sessionSwitchEvent) {
	observation := sessionObservation{
		Agent:     agent,
		SessionID: sessionID,
		LaunchID:  launch.ID,
		Server:    launch.Server,
		Counters:  counters,
	}
	stale, denied := false, false
	var statuses []remoteServerUsageStatus
	var fetchedAt time.Time
	server, err := r.ledgerServer(ctx, launch.Server)
	if err != nil {
		stale = true
	} else {
		assignments, fetchErr := r.fetchServerSessionAssignments(ctx, server, session.NormalizeAgentType(agent), sessionID)
		if fetchErr != nil {
			stale = true
			denied = strings.Contains(fetchErr.Error(), "401") || strings.Contains(fetchErr.Error(), "403")
		} else if assignment, ok := assignmentForSession(assignments, agent, sessionID); ok {
			observation.AccountID = assignment.AccountID
		}
		var fresh bool
		statuses, fetchedAt, fresh = r.cachedServerUsage(ctx, ledger, server)
		if !fresh && len(statuses) == 0 {
			stale = true
		}
	}
	if observation.AccountID == "" && launch.Pinned && launch.AccountID != "" {
		// A pinned launch has exactly one possible account.
		observation.AccountID = launch.AccountID
	}
	provider := ledgerProvider(agent)
	if observation.AccountID != "" {
		status, _ := usageStatusForAccount(statuses, provider, observation.AccountID)
		observation.Label = accountDisplayLabel(status, observation.AccountID)
	}
	record, event, _ := ledger.observe(observation)
	if event != nil {
		slog.Info("pooled session switched account",
			"agent", event.Agent, "session", event.SessionID, "launch", event.LaunchID,
			"from", event.From, "to", event.To)
	}
	view := viewFromRecord(record)
	view.Pinned = launch.Pinned
	view.Stale = stale
	view.Denied = denied
	if view.AccountID != "" {
		if status, ok := usageStatusForAccount(statuses, provider, view.AccountID); ok {
			view.Plan = status.PlanType
			view.Windows = status.Windows
			view.UsageFetchedAt = fetchedAt
		}
	}
	return view, event
}

// claudeStatusLineInput is the subset of Claude Code's status line JSON that
// sr reads.
type claudeStatusLineInput struct {
	SessionID string `json:"session_id"`
	Cost      struct {
		TotalCostUSD float64 `json:"total_cost_usd"`
	} `json:"cost"`
	ContextWindow struct {
		TotalInputTokens  int64 `json:"total_input_tokens"`
		TotalOutputTokens int64 `json:"total_output_tokens"`
	} `json:"context_window"`
}

// sessionHookArgs are the arguments sr bakes into a hook command. The agent
// runs hooks with SUBROUTER_* stripped from the environment, so everything a
// hook needs to find its ledger and server travels here or in the launch
// record.
type sessionHookArgs struct {
	launchID string
	storeDir string
	rest     []string
}

func parseSessionHookArgs(args []string, name string) (sessionHookArgs, error) {
	flags := flag.NewFlagSet(name, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	launch := flags.String("launch", "", "launch id")
	storeDir := flags.String("store-dir", "", "subrouter store directory")
	if err := flags.Parse(args); err != nil {
		return sessionHookArgs{}, err
	}
	return sessionHookArgs{launchID: strings.TrimSpace(*launch), storeDir: strings.TrimSpace(*storeDir), rest: flags.Args()}, nil
}

// hookRunner builds the runner a hook uses: the launch's store, and the
// launch's local daemon override.
func (h sessionHookArgs) runner(program string, out io.Writer) (srRunner, sessionLedger) {
	store := accounts.DefaultCodexStore()
	if h.storeDir != "" && filepath.IsAbs(h.storeDir) {
		store = accounts.CodexStore{Dir: filepath.Join(h.storeDir, "accounts")}
	}
	ledger := newSessionLedger(store.StoreDir())
	if launch, ok, _ := ledger.loadLaunch(h.launchID); ok && launch.LocalBaseURL != "" {
		_ = os.Setenv("SUBROUTER_LOCAL_BASE_URL", launch.LocalBaseURL)
	}
	return srRunner{program: program, store: store, useServingAPI: true, in: os.Stdin, out: out, errOut: io.Discard}, ledger
}

// runHiddenSessionCommand dispatches the hook entry points. It runs before
// any program-name routing (including cx), so a hook works however sr was
// invoked.
func runHiddenSessionCommand(program string, args []string) (bool, error) {
	if len(args) == 0 {
		return false, nil
	}
	switch args[0] {
	case sessionStatusLineCommand:
		return true, runSessionStatusLine(program, args[1:], os.Stdin, os.Stdout)
	case sessionNotifyCommand:
		return true, runSessionNotify(program, args[1:])
	}
	return false, nil
}

// runSessionStatusLine is Claude Code's statusLine command for pooled
// launches. It must always print something and exit 0 quickly.
func runSessionStatusLine(program string, args []string, in io.Reader, out io.Writer) error {
	hook, _ := parseSessionHookArgs(args, sessionStatusLineCommand)
	r, ledger := hook.runner(program, out)
	body, _ := io.ReadAll(io.LimitReader(in, 1<<20))
	// The user's own status line runs alongside sr's lookup, so it is never
	// delayed by sr's network calls.
	userLine := make(chan string, 1)
	go func() { userLine <- runUserClaudeStatusLine(body) }()
	ctx, cancel := context.WithTimeout(context.Background(), sessionStatusTimeout)
	defer cancel()
	line := r.sessionStatusLine(ctx, ledger, hook.launchID, body)
	if user := <-userLine; user != "" {
		fmt.Fprintln(out, user)
	}
	fmt.Fprintln(out, line)
	return nil
}

func (r srRunner) sessionStatusLine(ctx context.Context, ledger sessionLedger, launchID string, body []byte) string {
	var input claudeStatusLineInput
	_ = json.Unmarshal(body, &input)
	sessionID := strings.TrimSpace(input.SessionID)
	if sessionID == "" {
		return "sr: no session yet"
	}
	launch, ok, _ := ledger.loadLaunch(launchID)
	if !ok {
		launch = sessionLaunchRecord{ID: launchID}
		if !validSessionLaunchID(launchID) {
			launch.ID = ""
		}
	}
	counters := &sessionUsageCounters{
		InputTokens:  input.ContextWindow.TotalInputTokens,
		OutputTokens: input.ContextWindow.TotalOutputTokens,
		CostUSD:      input.Cost.TotalCostUSD,
	}
	view, _ := r.observeSession(ctx, ledger, launch, "claude", sessionID, counters)
	return renderSessionStatus(view, ledger.clock())
}

// userClaudeStatusLineSettings lists the settings files that can define the
// status line sr's injected hook would otherwise hide, highest precedence
// first: project local, project, then the launch's config directory.
func userClaudeStatusLineSettings(input []byte) []string {
	var workspace struct {
		Cwd       string `json:"cwd"`
		Workspace struct {
			ProjectDir string `json:"project_dir"`
			CurrentDir string `json:"current_dir"`
		} `json:"workspace"`
	}
	_ = json.Unmarshal(input, &workspace)
	var paths []string
	for _, dir := range []string{workspace.Workspace.ProjectDir, workspace.Workspace.CurrentDir, workspace.Cwd} {
		if dir = strings.TrimSpace(dir); dir != "" && filepath.IsAbs(dir) {
			paths = append(paths, filepath.Join(dir, ".claude", "settings.local.json"), filepath.Join(dir, ".claude", "settings.json"))
			break
		}
	}
	configDir := strings.TrimSpace(os.Getenv("CLAUDE_CONFIG_DIR"))
	if configDir == "" {
		if home, err := os.UserHomeDir(); err == nil {
			configDir = filepath.Join(home, ".claude")
		}
	}
	if configDir != "" {
		paths = append(paths, filepath.Join(configDir, "settings.json"))
	}
	// A pooled launch runs with CLAUDE_CONFIG_DIR set to its proxy directory,
	// whose settings never hold the user's status line. Fall back to the
	// user's own Claude settings, as a plain `claude` would read them.
	if shared := strings.TrimSpace(agentclaude.DefaultStore().SharedStateDir); shared != "" {
		if userSettings := filepath.Join(shared, "settings.json"); len(paths) == 0 || paths[len(paths)-1] != userSettings {
			paths = append(paths, userSettings)
		}
	}
	return paths
}

// runUserClaudeStatusLine chains the user's own status line command.
func runUserClaudeStatusLine(input []byte) string {
	command := ""
	for _, path := range userClaudeStatusLineSettings(input) {
		if command = userClaudeStatusLineCommand(path); command != "" {
			break
		}
	}
	if command == "" {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), sessionUserStatusLineTimeout)
	defer cancel()
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.CommandContext(ctx, "cmd", "/C", command)
	} else {
		cmd = exec.CommandContext(ctx, "/bin/sh", "-c", command)
	}
	cmd.Stdin = bytes.NewReader(input)
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	if err := cmd.Run(); err != nil && stdout.Len() == 0 {
		return ""
	}
	return strings.TrimRight(stdout.String(), "\r\n")
}

func userClaudeStatusLineCommand(settingsPath string) string {
	body, err := os.ReadFile(settingsPath)
	if err != nil {
		return ""
	}
	var settings struct {
		StatusLine *struct {
			Type    string `json:"type"`
			Command string `json:"command"`
		} `json:"statusLine"`
	}
	if json.Unmarshal(body, &settings) != nil || settings.StatusLine == nil {
		return ""
	}
	command := strings.TrimSpace(settings.StatusLine.Command)
	if settings.StatusLine.Type != "" && settings.StatusLine.Type != "command" {
		return ""
	}
	if strings.Contains(command, sessionStatusLineCommand) {
		return ""
	}
	return command
}

// sessionHookExecutable is the absolute path hooks use to call back into sr.
func sessionHookExecutable() (string, error) {
	path, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.Abs(path)
}

func hookCommandQuote(value string) string {
	if runtime.GOOS == "windows" {
		return `"` + strings.ReplaceAll(value, `"`, `""`) + `"`
	}
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}

// claudeStatusLineSetting is the statusLine block sr adds to a pooled
// launch's private settings, or nil when disabled.
func claudeStatusLineSetting(launchID, storeDir string) map[string]any {
	if disabled := strings.TrimSpace(os.Getenv(sessionStatusDisableEnv)); disabled == "0" || strings.EqualFold(disabled, "false") || strings.EqualFold(disabled, "off") {
		return nil
	}
	executable, err := sessionHookExecutable()
	if err != nil || !validSessionLaunchID(launchID) {
		return nil
	}
	return map[string]any{
		"type":    "command",
		"command": hookCommandQuote(executable) + " " + sessionStatusLineCommand + " --launch " + launchID + " --store-dir " + hookCommandQuote(storeDir),
		"padding": 0,
	}
}

// runSessionNotify is Codex's notify hook for pooled launches. Codex appends
// one JSON payload argument per event.
func runSessionNotify(program string, args []string) error {
	hook, err := parseSessionHookArgs(args, sessionNotifyCommand)
	rest := hook.rest
	if err != nil || len(rest) == 0 {
		return nil
	}
	var payload struct {
		Type     string `json:"type"`
		ThreadID string `json:"thread-id"`
	}
	if json.Unmarshal([]byte(rest[len(rest)-1]), &payload) != nil {
		return nil
	}
	threadID := strings.TrimSpace(payload.ThreadID)
	if threadID == "" {
		return nil
	}
	r, ledger := hook.runner(program, io.Discard)
	launch, ok, _ := ledger.loadLaunch(hook.launchID)
	if !ok {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	r.observeSession(ctx, ledger, launch, "codex", threadID, nil)
	return nil
}

// printLaunchSessionSummary tells the user, after the agent exits, which
// accounts served each session of this launch and how to resume it.
func printLaunchSessionSummary(out io.Writer, ledger sessionLedger, launchID, program, resumeCommand string) {
	launch, ok, err := ledger.loadLaunch(launchID)
	if err != nil || !ok || len(launch.Sessions) == 0 {
		return
	}
	for _, sessionID := range launch.Sessions {
		record, ok, err := ledger.loadSession(launch.Agent, sessionID)
		if err != nil || !ok {
			continue
		}
		fmt.Fprintf(out, "%s: session %s served by %s\n", program, sessionID, formatSessionAccountSpans(record.Accounts))
		if resumeCommand != "" {
			fmt.Fprintf(out, "%s: resume with: %s %s\n", program, resumeCommand, sessionID)
		}
	}
}

func formatSessionAccountSpans(spans []sessionAccountSpan) string {
	if len(spans) == 0 {
		return "an unrecorded account"
	}
	parts := make([]string, 0, len(spans))
	for _, span := range spans {
		label := span.Label
		if label == "" {
			label = span.AccountID
		}
		text := fmt.Sprintf("%s (%s-%s", label, span.From.Local().Format("15:04"), span.To.Local().Format("15:04"))
		if tokens := span.InputTokens + span.OutputTokens; tokens > 0 {
			text += ", " + formatTokenCount(tokens) + " tokens"
		}
		if span.CostUSD > 0 {
			text += fmt.Sprintf(", $%.2f", span.CostUSD)
		}
		parts = append(parts, text+")")
	}
	return strings.Join(parts, " -> ")
}

func formatTokenCount(tokens int64) string {
	switch {
	case tokens >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(tokens)/1_000_000)
	case tokens >= 1_000:
		return fmt.Sprintf("%.1fk", float64(tokens)/1_000)
	default:
		return fmt.Sprintf("%d", tokens)
	}
}

// preferredAccountForResume returns the account that last served a Claude
// session, so a resumed session can go back to it (and its prompt cache).
func preferredAccountForResume(ledger sessionLedger, agent, sessionID string) (sessionAccountSpan, bool) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return sessionAccountSpan{}, false
	}
	record, ok, err := ledger.loadSession(agent, sessionID)
	if err != nil || !ok {
		return sessionAccountSpan{}, false
	}
	return record.lastAccount()
}

// claudeResumeSessionID returns the session named by --resume/-r, if any.
// A bare --resume opens Claude's picker, which has no ID yet.
func claudeResumeSessionID(args []string) string {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			return ""
		}
		switch {
		case arg == "--resume" || arg == "-r":
			if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				return strings.TrimSpace(args[i+1])
			}
			return ""
		case strings.HasPrefix(arg, "--resume="):
			return strings.TrimSpace(strings.TrimPrefix(arg, "--resume="))
		}
	}
	return ""
}

// sessions implements sr sessions / sr whoami: local sessions, the account
// serving each one now, and that account's limits.
func (r srRunner) sessions(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("sr sessions", flag.ContinueOnError)
	flags.SetOutput(r.errOut)
	all := flags.Bool("all", false, "include sessions older than a day")
	jsonOutput := flags.Bool("json", false, "print machine-readable JSON")
	if err := flags.Parse(args); err != nil {
		return err
	}
	ledger := newSessionLedger(r.store.StoreDir())
	records, err := ledger.listSessions()
	if err != nil {
		return err
	}
	now := ledger.clock()
	type row struct {
		Record  sessionRecord        `json:"session"`
		Running bool                 `json:"running"`
		Crashed bool                 `json:"crashed,omitempty"`
		Launch  *sessionLaunchRecord `json:"launch,omitempty"`
		View    sessionStatusView    `json:"-"`
	}
	rows := make([]row, 0, len(records))
	for _, record := range records {
		item := row{Record: record}
		if launch, ok, _ := ledger.loadLaunch(record.LaunchID); ok {
			launchCopy := launch
			item.Launch = &launchCopy
			item.Running = launch.running()
			item.Crashed = launch.EndedAt == nil && !item.Running
		}
		if !*all && !item.Running && now.Sub(record.LastSeen) > 24*time.Hour {
			continue
		}
		rows = append(rows, item)
	}
	for i := range rows {
		if rows[i].Running {
			fetchCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			view, _ := r.observeSession(fetchCtx, ledger, *rows[i].Launch, rows[i].Record.Agent, rows[i].Record.SessionID, nil)
			cancel()
			if record, ok, _ := ledger.loadSession(rows[i].Record.Agent, rows[i].Record.SessionID); ok {
				rows[i].Record = record
			}
			rows[i].View = view
		} else {
			rows[i].View = viewFromRecord(rows[i].Record)
		}
	}
	if *jsonOutput {
		encoder := json.NewEncoder(r.out)
		encoder.SetIndent("", "  ")
		return encoder.Encode(rows)
	}
	if len(rows) == 0 {
		fmt.Fprintf(r.out, "No pooled sessions recorded in the last day. Sessions are recorded by '%s claude' and '%s codex' launches; use --all for older ones.\n", r.programOrSubrouter(), r.programOrSubrouter())
		return nil
	}
	for _, item := range rows {
		record := item.Record
		state := "ended"
		switch {
		case item.Running:
			state = fmt.Sprintf("running (pid %d)", item.Launch.PID)
		case item.Crashed:
			state = "launcher exited without cleanup (crashed?)"
		}
		fmt.Fprintf(r.out, "%s %s  %s  last seen %s\n", record.Agent, record.SessionID, state, formatAgo(now.Sub(record.LastSeen)))
		if item.Running {
			fmt.Fprintf(r.out, "  now: %s\n", strings.TrimPrefix(renderSessionStatus(item.View, now), "sr: "))
		}
		fmt.Fprintf(r.out, "  accounts: %s\n", formatSessionAccountSpans(record.Accounts))
		if total := record.totals(); total.InputTokens+total.OutputTokens > 0 || total.CostUSD > 0 {
			fmt.Fprintf(r.out, "  total: %s in / %s out", formatTokenCount(total.InputTokens), formatTokenCount(total.OutputTokens))
			if total.CostUSD > 0 {
				fmt.Fprintf(r.out, ", $%.2f", total.CostUSD)
			}
			fmt.Fprintln(r.out)
		}
		if !item.Running {
			switch record.Agent {
			case "claude":
				fmt.Fprintf(r.out, "  resume: %s claude proxy --resume %s\n", r.programOrSubrouter(), record.SessionID)
			case "codex":
				fmt.Fprintf(r.out, "  resume: %s codex resume %s\n", r.programOrSubrouter(), record.SessionID)
			}
		}
	}
	return nil
}
