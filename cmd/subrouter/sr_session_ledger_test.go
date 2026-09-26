package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
)

type ledgerClock struct{ now time.Time }

func (c *ledgerClock) Now() time.Time { return c.now }

func testLedger(t *testing.T) (sessionLedger, *ledgerClock) {
	t.Helper()
	clock := &ledgerClock{now: time.Date(2026, 9, 26, 9, 0, 0, 0, time.UTC)}
	ledger := newSessionLedger(t.TempDir())
	ledger.now = clock.Now
	return ledger, clock
}

func TestSessionLedgerRecordsAccountSpansSwitchesAndUsage(t *testing.T) {
	ledger, clock := testLedger(t)
	launch, err := ledger.startLaunch(sessionLaunchRecord{Agent: "claude", Server: "team"})
	if err != nil {
		t.Fatal(err)
	}
	observe := func(account string, in, out int64, cost float64) (sessionRecord, *sessionSwitchEvent) {
		t.Helper()
		record, event, err := ledger.observe(sessionObservation{
			Agent: "claude", SessionID: "sess-1", LaunchID: launch.ID, Server: "team",
			AccountID: account, Label: account + "@example.com",
			Counters: &sessionUsageCounters{InputTokens: in, OutputTokens: out, CostUSD: cost},
		})
		if err != nil {
			t.Fatal(err)
		}
		return record, event
	}

	// Usage before the first request has no account yet.
	record, event := observe("", 0, 0, 0)
	if len(record.Accounts) != 0 || event != nil {
		t.Fatalf("pre-request observation recorded %+v, event %+v", record.Accounts, event)
	}
	clock.now = clock.now.Add(time.Minute)
	record, event = observe("a", 100, 10, 0.5)
	if event != nil || len(record.Accounts) != 1 || record.Accounts[0].InputTokens != 100 {
		t.Fatalf("first account: %+v event %+v", record.Accounts, event)
	}
	clock.now = clock.now.Add(time.Minute)
	record, _ = observe("a", 150, 20, 0.75)
	if len(record.Accounts) != 1 || record.Accounts[0].InputTokens != 150 || record.Accounts[0].OutputTokens != 20 {
		t.Fatalf("same account should extend the span: %+v", record.Accounts)
	}
	if !record.Accounts[0].To.Equal(clock.now) {
		t.Fatalf("span end = %v, want %v", record.Accounts[0].To, clock.now)
	}
	clock.now = clock.now.Add(time.Minute)
	record, event = observe("b", 400, 30, 1.25)
	if event == nil || event.From != "a" || event.To != "b" || event.FromLabel != "a@example.com" {
		t.Fatalf("switch event = %+v", event)
	}
	if len(record.Accounts) != 2 || record.Accounts[1].InputTokens != 250 || record.Accounts[1].OutputTokens != 10 {
		t.Fatalf("delta after the switch belongs to the new account: %+v", record.Accounts)
	}
	if total := record.totals(); total.InputTokens != 400 || total.OutputTokens != 30 || total.CostUSD != 1.25 {
		t.Fatalf("totals = %+v", total)
	}
	events := ledger.sessionEvents("claude", "sess-1")
	if len(events) != 1 || events[0].To != "b" {
		t.Fatalf("events = %+v", events)
	}

	// A resumed process restarts Claude's counters; the new values are the delta.
	resumed, err := ledger.startLaunch(sessionLaunchRecord{Agent: "claude"})
	if err != nil {
		t.Fatal(err)
	}
	record, _, err = ledger.observe(sessionObservation{
		Agent: "claude", SessionID: "sess-1", LaunchID: resumed.ID, AccountID: "b",
		Counters: &sessionUsageCounters{InputTokens: 5, OutputTokens: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := record.Accounts[1].InputTokens; got != 255 {
		t.Fatalf("restart delta: input tokens = %d, want 255", got)
	}
	if record.LaunchID != resumed.ID {
		t.Fatalf("session launch = %q, want the resumed launch", record.LaunchID)
	}

	stored, ok, err := ledger.loadLaunch(launch.ID)
	if err != nil || !ok {
		t.Fatalf("loadLaunch: ok=%v err=%v", ok, err)
	}
	if len(stored.Sessions) != 1 || stored.Sessions[0] != "sess-1" {
		t.Fatalf("launch sessions = %v", stored.Sessions)
	}
}

func TestSessionLedgerFinishLaunchAndSummary(t *testing.T) {
	ledger, clock := testLedger(t)
	launch, err := ledger.startLaunch(sessionLaunchRecord{Agent: "claude"})
	if err != nil {
		t.Fatal(err)
	}
	if !launch.running() {
		t.Fatal("a launch owned by this process should read as running")
	}
	if _, _, err := ledger.observe(sessionObservation{Agent: "claude", SessionID: "s", LaunchID: launch.ID, AccountID: "a", Label: "alice"}); err != nil {
		t.Fatal(err)
	}
	clock.now = clock.now.Add(time.Hour)
	if _, _, err := ledger.observe(sessionObservation{Agent: "claude", SessionID: "s", LaunchID: launch.ID, AccountID: "b", Label: "bob"}); err != nil {
		t.Fatal(err)
	}
	finished, err := ledger.finishLaunch(launch.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if finished.EndedAt == nil || finished.running() {
		t.Fatalf("finished launch = %+v", finished)
	}
	var out bytes.Buffer
	printLaunchSessionSummary(&out, ledger, launch.ID, "sr", "sr claude proxy --resume")
	text := out.String()
	for _, want := range []string{"session s served by alice", " -> bob", "resume with: sr claude proxy --resume s"} {
		if !strings.Contains(text, want) {
			t.Fatalf("summary missing %q:\n%s", want, text)
		}
	}
}

func TestSessionLedgerCrashedLaunchIsNotRunning(t *testing.T) {
	ledger, _ := testLedger(t)
	launch, err := ledger.startLaunch(sessionLaunchRecord{Agent: "claude", PID: 1 << 30})
	if err != nil {
		t.Fatal(err)
	}
	if launch.running() {
		t.Fatal("a launch whose process is gone must not read as running")
	}
}

func TestSessionFileNameRejectsTraversal(t *testing.T) {
	if got := sessionFileName("3f2a9c1e-1111-2222-3333-444455556666"); got != "3f2a9c1e-1111-2222-3333-444455556666.json" {
		t.Fatalf("uuid file name = %q", got)
	}
	for _, id := range []string{"../../etc/passwd", "..", "a/b", strings.Repeat("x", 200)} {
		name := sessionFileName(id)
		if !strings.HasPrefix(name, "h-") || strings.ContainsAny(name, `/\`) {
			t.Fatalf("sessionFileName(%q) = %q, want a hashed name", id, name)
		}
	}
	if validSessionLaunchID("../x") || !validSessionLaunchID(newSessionLaunchID()) {
		t.Fatal("launch id validation is wrong")
	}
}

func TestSessionLedgerListsMostRecentFirst(t *testing.T) {
	ledger, clock := testLedger(t)
	for _, id := range []string{"old", "new"} {
		if _, _, err := ledger.observe(sessionObservation{Agent: "codex", SessionID: id, AccountID: "a"}); err != nil {
			t.Fatal(err)
		}
		clock.now = clock.now.Add(time.Minute)
	}
	records, err := ledger.listSessions()
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 2 || records[0].SessionID != "new" || records[0].Agent != "codex" {
		t.Fatalf("records = %+v", records)
	}
}

func TestAssignmentForSessionMatchesAgentAndCodexThreadBase(t *testing.T) {
	older := time.Date(2026, 9, 26, 8, 0, 0, 0, time.UTC)
	newer := older.Add(time.Hour)
	assignments := []serverSessionAssignment{
		{AgentType: "codex", SessionID: "sess", AccountID: "codex-acct", UpdatedAt: newer},
		{AgentType: "claude", SessionID: "sess", AccountID: "claude-old", UpdatedAt: older},
		{AgentType: "claude", SessionID: "sess", AccountID: "claude-new", UpdatedAt: newer},
		{AgentType: "codex", SessionID: "thread-1", AccountID: "codex-thread", UpdatedAt: older},
	}
	if got, ok := assignmentForSession(assignments, "claude", "sess"); !ok || got.AccountID != "claude-new" {
		t.Fatalf("claude assignment = %+v ok=%v", got, ok)
	}
	if got, ok := assignmentForSession(assignments, "codex", "thread-1:3"); !ok || got.AccountID != "codex-thread" {
		t.Fatalf("codex window id should match its thread: %+v ok=%v", got, ok)
	}
	if _, ok := assignmentForSession(assignments, "claude", "missing"); ok {
		t.Fatal("unknown session matched")
	}
}

func TestRenderSessionStatus(t *testing.T) {
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	view := sessionStatusView{
		AccountID: "acct-b",
		Label:     "bob@example.com",
		Plan:      "max",
		Windows: []accounts.UsageWindow{
			{Name: "five_hour", UsedPercent: 42, LimitWindowSeconds: 5 * 3600, ResetAfterSeconds: 3600},
			{Name: "seven_day", UsedPercent: 18, LimitWindowSeconds: 7 * 86400, ResetAfterSeconds: 3 * 86400},
			{Name: "seven_day_opus/secondary", UsedPercent: 99, LimitWindowSeconds: 7 * 86400},
			{Name: "extra", UsedPercent: 77, ExtraUsage: &accounts.ExtraUsageInfo{}},
		},
		UsageFetchedAt: now,
		PreviousLabel:  "alice@example.com",
		SwitchedAt:     now.Add(-3 * time.Minute),
	}
	got := renderSessionStatus(view, now)
	for _, want := range []string{
		"sr: bob@example.com [max]",
		"5h 42% resets " + now.Add(time.Hour).Local().Format("15:04"),
		"wk 18% resets " + now.Add(72*time.Hour).Local().Format("Mon 15:04"),
		"switched from alice@example.com 3m ago",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("status %q missing %q", got, want)
		}
	}
	if strings.Contains(got, "99%") || strings.Contains(got, "77%") {
		t.Fatalf("status shows a per-model or extra-usage window: %q", got)
	}
	if strings.Contains(got, "\u2014") {
		t.Fatalf("status contains an em dash: %q", got)
	}

	view.SwitchedAt = now.Add(-time.Hour)
	view.Pinned = true
	view.Stale = true
	got = renderSessionStatus(view, now)
	if strings.Contains(got, "switched from") || !strings.Contains(got, "(pinned)") || !strings.Contains(got, "(stale)") {
		t.Fatalf("status = %q", got)
	}
	if got := renderSessionStatus(sessionStatusView{}, now); got != "sr: waiting for first request" {
		t.Fatalf("empty status = %q", got)
	}
}

func TestClaudeResumeSessionID(t *testing.T) {
	cases := map[string][]string{
		"abc": {"--resume", "abc"},
		"def": {"--model", "opus", "-r", "def"},
		"ghi": {"--resume=ghi"},
		"":    {"--resume"},
	}
	for want, args := range cases {
		if got := claudeResumeSessionID(args); got != want {
			t.Errorf("claudeResumeSessionID(%v) = %q, want %q", args, got, want)
		}
	}
	if got := claudeResumeSessionID([]string{"--", "--resume", "x"}); got != "" {
		t.Errorf("args after -- are literal, got %q", got)
	}
	if got := claudeResumeSessionID([]string{"--resume", "--model"}); got != "" {
		t.Errorf("bare --resume followed by a flag = %q", got)
	}
}

func TestClaudeFlagsLaunchPooled(t *testing.T) {
	if !claudeFlagsLaunchPooled([]string{"--resume", "abc"}, "") {
		t.Fatal("--resume without an active profile should launch pooled")
	}
	if claudeFlagsLaunchPooled([]string{"--resume", "abc"}, "work") {
		t.Fatal("an active profile keeps the legacy profile launch")
	}
	for _, args := range [][]string{nil, {"proxy"}, {"list"}, {"--help"}, {"-h"}} {
		if claudeFlagsLaunchPooled(args, "") {
			t.Fatalf("claudeFlagsLaunchPooled(%v) = true", args)
		}
	}
}

func TestSRClaudeResumeWithoutProfileRoutesToPooledLauncher(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	// Point the local daemon at a closed port so the pooled path stops at its
	// health probe instead of reaching a developer's daemon or launching Claude.
	closed := httptest.NewServer(http.NotFoundHandler())
	closedURL := closed.URL
	closed.Close()
	t.Setenv("SUBROUTER_LOCAL_BASE_URL", closedURL)
	var out, errOut bytes.Buffer
	runner := srRunner{store: accounts.DefaultCodexStore(), out: &out, errOut: &errOut}
	err := runner.claude(context.Background(), []string{"--resume", "abc"})
	if err == nil || !strings.Contains(err.Error(), "local proxy is unavailable") {
		t.Fatalf("sr claude --resume should reach the pooled launcher's daemon check, got %v", err)
	}
	if !strings.Contains(errOut.String(), "launching pooled Claude") {
		t.Fatalf("missing pooled launch notice: %q", errOut.String())
	}
}

func TestWithClaudeSessionStatusLine(t *testing.T) {
	body, err := claudeLaunchSettingsJSON("/tmp/config", map[string]string{"ANTHROPIC_BASE_URL": "http://x"})
	if err != nil {
		t.Fatal(err)
	}
	launchID := newSessionLaunchID()
	withStatus, err := withClaudeSessionStatusLine(body, launchID, "/tmp/store dir")
	if err != nil {
		t.Fatal(err)
	}
	var settings struct {
		Env        map[string]string `json:"env"`
		StatusLine struct {
			Type    string `json:"type"`
			Command string `json:"command"`
		} `json:"statusLine"`
	}
	if err := json.Unmarshal(withStatus, &settings); err != nil {
		t.Fatal(err)
	}
	if settings.Env["ANTHROPIC_BASE_URL"] != "http://x" {
		t.Fatalf("env lost: %+v", settings.Env)
	}
	if settings.StatusLine.Type != "command" || !strings.HasSuffix(settings.StatusLine.Command, " "+sessionStatusLineCommand+" --launch "+launchID+" --store-dir '/tmp/store dir'") {
		t.Fatalf("statusLine = %+v", settings.StatusLine)
	}

	t.Setenv(sessionStatusDisableEnv, "0")
	disabled, err := withClaudeSessionStatusLine(body, launchID, "/tmp/store dir")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(disabled, body) {
		t.Fatalf("disabled status line still changed settings: %s", disabled)
	}
	t.Setenv(sessionStatusDisableEnv, "")
	if unchanged, _ := withClaudeSessionStatusLine(body, "", "/tmp/store dir"); !bytes.Equal(unchanged, body) {
		t.Fatal("an unrecorded launch must not get a status line")
	}
}

func TestUserClaudeStatusLineCommandChainsButNeverRecurses(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	write := func(body string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(`{"statusLine":{"type":"command","command":"echo mine"}}`)
	if got := userClaudeStatusLineCommand(path); got != "echo mine" {
		t.Fatalf("user command = %q", got)
	}
	write(`{"statusLine":{"type":"command","command":"/bin/sr __session-statusline --launch ab"}}`)
	if got := userClaudeStatusLineCommand(path); got != "" {
		t.Fatalf("recursive command = %q", got)
	}
	write(`{}`)
	if got := userClaudeStatusLineCommand(path); got != "" {
		t.Fatalf("missing command = %q", got)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	write(`{"statusLine":{"type":"command","command":"cat >/dev/null; echo user-line"}}`)
	if got := runUserClaudeStatusLine([]byte(`{}`)); got != "user-line" {
		t.Fatalf("chained output = %q", got)
	}
}

func TestCodexSessionNotifyConfig(t *testing.T) {
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	args := codexSessionNotifyConfigArgs(nil, "abcd", "/st")
	if len(args) != 2 || args[0] != "-c" || !strings.HasPrefix(args[1], "notify=[") || !strings.Contains(args[1], `"`+sessionNotifyCommand+`","--launch","abcd","--store-dir","/st"]`) {
		t.Fatalf("notify args = %v", args)
	}
	if got := codexSessionNotifyConfigArgs([]string{"-c", "notify=[\"x\"]"}, "abcd", "/st"); got != nil {
		t.Fatalf("user -c notify must win, got %v", got)
	}
	if err := os.WriteFile(filepath.Join(codexHome, "config.toml"), []byte("model = \"gpt\"\nnotify = [\"say\"]\n[profiles.x]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := codexSessionNotifyConfigArgs(nil, "abcd", "/st"); got != nil {
		t.Fatalf("config.toml notify must win, got %v", got)
	}
	if tomlTopLevelKeyPresent("[tui]\nnotify = true\n", "notify") {
		t.Fatal("a key inside a table is not top level")
	}
	for args, want := range map[string]bool{"": true, "exec hi": true, "resume abc": true, "review": false, "login": false} {
		if got := codexInvocationRecordsSession(strings.Fields(args)); got != want {
			t.Errorf("codexInvocationRecordsSession(%q) = %v, want %v", args, got, want)
		}
	}
}

// TestSessionStatusLineEndToEnd drives the status line against a fake server:
// it must name the serving account with its limits, record the ledger, and
// show a failover as it happens.
func TestSessionStatusLineEndToEnd(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	store := accounts.DefaultCodexStore()
	serving := "acct-a"
	var sessionQueries []string
	serverHTTP := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch req.URL.Path {
		case "/_subrouter/sessions":
			sessionQueries = append(sessionQueries, req.URL.RawQuery)
			_ = json.NewEncoder(w).Encode([]serverSessionAssignment{
				{AgentType: "claude", SessionID: "sess-9", AccountID: serving, UpdatedAt: time.Now()},
				{AgentType: "claude", SessionID: "other", AccountID: "acct-z", UpdatedAt: time.Now()},
			})
		case "/_subrouter/usage-status":
			_ = json.NewEncoder(w).Encode([]remoteServerUsageStatus{
				{ID: "acct-a", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Label: "alice@example.com", PlanType: "max",
					Windows: []accounts.UsageWindow{{Name: "five_hour", UsedPercent: 91, LimitWindowSeconds: 5 * 3600, ResetAfterSeconds: 600}}},
				{ID: "acct-b", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Label: "bob@example.com", PlanType: "max",
					Windows: []accounts.UsageWindow{{Name: "seven_day", UsedPercent: 12, LimitWindowSeconds: 7 * 86400}}},
			})
		default:
			http.NotFound(w, req)
		}
	}))
	defer serverHTTP.Close()
	if err := defaultSRServerStore(store).save(srServerFile{Servers: []srServerConfig{{Name: "team", URL: serverHTTP.URL}}, Default: "team"}); err != nil {
		t.Fatal(err)
	}
	runner := srRunner{store: store, out: &bytes.Buffer{}, errOut: &bytes.Buffer{}, client: serverHTTP.Client()}
	ledger := newSessionLedger(store.StoreDir())
	launch, err := ledger.startLaunch(sessionLaunchRecord{Agent: "claude", Server: "team"})
	if err != nil {
		t.Fatal(err)
	}
	input := []byte(`{"session_id":"sess-9","cost":{"total_cost_usd":0.4},"context_window":{"total_input_tokens":1200,"total_output_tokens":300}}`)

	line := runner.sessionStatusLine(context.Background(), ledger, launch.ID, input)
	if !strings.Contains(line, "alice@example.com [max]") || !strings.Contains(line, "5h 91%") {
		t.Fatalf("status line = %q", line)
	}
	if len(sessionQueries) == 0 || !strings.Contains(sessionQueries[0], "session_id=sess-9") || !strings.Contains(sessionQueries[0], "agent_type=claude") {
		t.Fatalf("session query = %v", sessionQueries)
	}

	serving = "acct-b"
	line = runner.sessionStatusLine(context.Background(), ledger, launch.ID, input)
	if !strings.Contains(line, "bob@example.com") || !strings.Contains(line, "switched from alice@example.com") || !strings.Contains(line, "wk 12%") {
		t.Fatalf("status line after failover = %q", line)
	}
	record, ok, err := ledger.loadSession("claude", "sess-9")
	if err != nil || !ok {
		t.Fatalf("session record: ok=%v err=%v", ok, err)
	}
	if len(record.Accounts) != 2 || record.Accounts[0].AccountID != "acct-a" || record.Accounts[1].AccountID != "acct-b" {
		t.Fatalf("spans = %+v", record.Accounts)
	}
	if record.Accounts[0].InputTokens != 1200 || record.Server != "team" || record.LaunchID != launch.ID {
		t.Fatalf("record = %+v", record)
	}

	// A resume of the crashed session prefers the account that served it last.
	var errOut bytes.Buffer
	runner.errOut = &errOut
	if got := runner.resumePreferredClaudeAccount([]string{"--resume", "sess-9"}, "", ""); got != "acct-b" {
		t.Fatalf("resume preference = %q", got)
	}
	if !strings.Contains(errOut.String(), "preferring bob@example.com") {
		t.Fatalf("resume notice = %q", errOut.String())
	}
	if got := runner.resumePreferredClaudeAccount([]string{"--resume", "sess-9"}, "pinned", ""); got != "" {
		t.Fatalf("a pinned launch must not gain a preference, got %q", got)
	}

	// sr sessions lists the running session with its current account.
	var out bytes.Buffer
	runner.out = &out
	if err := runner.sessions(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	for _, want := range []string{"claude sess-9", "running", "now: bob@example.com", "alice@example.com (", "-> bob@example.com"} {
		if !strings.Contains(text, want) {
			t.Fatalf("sr sessions missing %q:\n%s", want, text)
		}
	}

	// A server outage renders the last known account as stale.
	serverHTTP.Close()
	line = runner.sessionStatusLine(context.Background(), ledger, launch.ID, input)
	if !strings.Contains(line, "bob@example.com") || !strings.Contains(line, "(stale)") {
		t.Fatalf("offline status line = %q", line)
	}
}

func TestSessionLedgerKeepsUsageSeenBeforeTheFirstAccount(t *testing.T) {
	ledger, _ := testLedger(t)
	for _, step := range []struct {
		account string
		input   int64
	}{{"", 100}, {"", 300}, {"a", 350}} {
		if _, _, err := ledger.observe(sessionObservation{Agent: "claude", SessionID: "s", LaunchID: "ab", AccountID: step.account,
			Counters: &sessionUsageCounters{InputTokens: step.input}}); err != nil {
			t.Fatal(err)
		}
	}
	record, _, _ := ledger.loadSession("claude", "s")
	if len(record.Accounts) != 1 || record.Accounts[0].InputTokens != 350 || record.Unattributed.InputTokens != 0 {
		t.Fatalf("record = %+v", record)
	}
}

func TestTomlBasicStringEscapesForToml(t *testing.T) {
	got := tomlBasicString("a\"b\\c\x01\u00e9")
	if want := `"a\"b\\c\u0001` + "\u00e9" + `"`; got != want {
		t.Fatalf("tomlBasicString = %s", got)
	}
	if strings.Contains(got, `\x`) {
		t.Fatalf("Go-only escape leaked: %s", got)
	}
}

// Hooks run under the agent with SUBROUTER_* stripped, so the store dir in
// the hook argv and the launch record must be enough to find everything.
func TestSessionHookUsesLaunchStoreAndDaemonOverride(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	storeDir := t.TempDir()
	ledger := newSessionLedger(storeDir)
	launch, err := ledger.startLaunch(sessionLaunchRecord{Agent: "claude", Server: "local", LocalBaseURL: "http://127.0.0.1:9"})
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("SUBROUTER_LOCAL_BASE_URL", "")
	var out bytes.Buffer
	handled, err := runHiddenSessionCommand("cx", []string{sessionNotifyCommand, "--launch", launch.ID, "--store-dir", storeDir, `{"type":"agent-turn-complete","thread-id":"t-1"}`})
	if !handled || err != nil {
		t.Fatalf("notify under cx: handled=%v err=%v", handled, err)
	}
	_ = out
	if got := os.Getenv("SUBROUTER_LOCAL_BASE_URL"); got != "http://127.0.0.1:9" {
		t.Fatalf("daemon override not restored: %q", got)
	}
	if _, ok, _ := ledger.loadSession("codex", "t-1"); !ok {
		t.Fatal("notify did not record the session in the launch's store")
	}
	stored, _, _ := ledger.loadLaunch(launch.ID)
	if len(stored.Sessions) != 1 || stored.Sessions[0] != "t-1" {
		t.Fatalf("launch sessions = %v", stored.Sessions)
	}
}

func TestRenderSessionStatusDistinguishesRefusedLookup(t *testing.T) {
	got := renderSessionStatus(sessionStatusView{Stale: true, Denied: true}, time.Now())
	if !strings.Contains(got, "refused") {
		t.Fatalf("status = %q", got)
	}
}
