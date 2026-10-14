package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/internal/proxy"
	"github.com/manaflow-ai/subrouter/wake"
)

// Auto-resume runs in two places:
//
//   - The pool's proxy decides. It records quota and provider failures per
//     session (internal/proxy/recovery_tracker.go), serves them at
//     /_subrouter/recovery-status, and holds the pool-wide on/off switch at
//     /_subrouter/auto-resume.
//   - One resumer per Mac acts. It runs inside any `sr claude`, `sr codex` or
//     `sr auto-resume watch` process, finds each session's cmux tab, and types
//     `continue` or `/goal resume` when the proxy's state says the session can
//     go on (the timing rules are in syncRecoveryAlarms in sr_wake.go).
//
// No LaunchAgent is involved: a resumer lives exactly as long as some sr
// process on that Mac does.

// resumerProtocolVersion identifies what this resumer can do. Version 3
// answers permission prompts and resumes stalled tabs by the proxy's rules
// (sr_auto_resume_prompts.go). Bump it when
// the resumer gains an ability rules depend on; a newer resumer then takes a
// Mac over from an older one still running inside a long-lived session.
const resumerProtocolVersion = 9

// resumerInterval is how often a resumer checks the proxy and cmux.
const resumerInterval = 15 * time.Second

// wakeScope supplies what a resume pass needs beyond the alarm store.
type wakeScope struct {
	// enabled reports the pool-wide switch for an agent.
	enabled func(agent string) (bool, error)
	// testBinding, when set, ties one test session to a cmux tab directly,
	// so `sr auto-resume test` needs no live agent.
	testBinding *cmuxSessionWire
	// onDecision, when set, hears why a stopped session on this Mac was or
	// was not scheduled for a resume.
	onDecision func(state recoveryWireState, surface, decision string)
}

func (s wakeScope) decide(state recoveryWireState, surface, decision string) {
	if s.onDecision != nil {
		s.onDecision(state, surface, decision)
	}
}

func (s wakeScope) boundSession(agent, sessionID string) (cmuxSessionWire, bool) {
	if s.testBinding != nil && s.testBinding.Agent == agent && s.testBinding.SessionID == sessionID {
		return *s.testBinding, true
	}
	return cmuxSessionWire{}, false
}

// fleetWakeScope reads the pool-wide switch from the proxy once per pass.
func fleetWakeScope(serverURL string) wakeScope {
	var (
		loaded  bool
		setting proxy.AutoResumeSetting
		loadErr error
	)
	return wakeScope{enabled: func(agent string) (bool, error) {
		if !loaded {
			setting, loadErr = fetchAutoResumeSetting(serverURL)
			loaded = true
		}
		return setting.Enabled(agent), loadErr
	}}
}

func autoResumeHTTPClient() *http.Client { return &http.Client{Timeout: 5 * time.Second} }

func fetchAutoResumeSetting(serverURL string) (proxy.AutoResumeSetting, error) {
	var setting proxy.AutoResumeSetting
	resp, err := autoResumeHTTPClient().Get(strings.TrimRight(serverURL, "/") + "/_subrouter/auto-resume")
	if err != nil {
		return setting, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return setting, fmt.Errorf("the pool server at %s has no auto-resume support; update it", serverURL)
	}
	if resp.StatusCode != http.StatusOK {
		return setting, fmt.Errorf("auto-resume setting: %s", resp.Status)
	}
	return setting, json.NewDecoder(resp.Body).Decode(&setting)
}

func postAutoResume(serverURL, path string, body any, out any) error {
	payload, err := json.Marshal(body)
	if err != nil {
		return err
	}
	resp, err := autoResumeHTTPClient().Post(strings.TrimRight(serverURL, "/")+path, "application/json", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		message, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("%s: %s %s", path, resp.Status, strings.TrimSpace(string(message)))
	}
	if out == nil || resp.StatusCode == http.StatusNoContent {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// autoResumeDir holds the per-Mac resumer's lock and alarms. It is fixed
// under the home directory, so every sr on the Mac agrees on it whatever
// SUBROUTER_STATE_DIR each process was given.
func autoResumeDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(home, ".subrouter", "auto-resume")
	return dir, os.MkdirAll(dir, 0o700)
}

// runResumer acts as this Mac's resumer while ctx lasts. Only one process
// per Mac acts at a time: the holder of an exclusive lock. A process with a
// newer resumerProtocolVersion records itself as wanted, and an older holder
// then steps aside.
func runResumer(ctx context.Context, serverURL string, out io.Writer) {
	dir, err := autoResumeDir()
	if err != nil {
		slog.Warn("auto-resume: no state directory", "error", err)
		return
	}
	cmuxPath, err := wakeCmuxPath()
	if err != nil {
		slog.Debug("auto-resume: cmux not found; this process will not resume sessions", "error", err)
		return
	}
	store := wake.NewStore(filepath.Join(dir, "alarms.json"))
	lockPath := filepath.Join(dir, "resumer.lock")
	wantPath := filepath.Join(dir, "resumer.want")
	var release func()
	defer func() {
		if release != nil {
			release()
		}
	}()
	startedAt := time.Now().UTC()
	initial := true
	lastReadiness := time.Time{}
	stalls := newStallTracker()
	decisions := newDecisionReporter(serverURL, out)
	ticker := time.NewTicker(resumerInterval)
	defer ticker.Stop()
	for {
		claimResumerWant(wantPath)
		if release != nil && newerResumerWanted(wantPath) {
			release()
			release = nil
		}
		if release == nil && !newerResumerWanted(wantPath) {
			if unlock, err := wake.AcquireWorkerLock(lockPath); err == nil {
				release = unlock
				initial = true
			}
		}
		if release != nil {
			scope := fleetWakeScope(serverURL)
			scope.onDecision = decisions.report
			if err := syncRecoveryAlarms(store, serverURL, cmuxPath, startedAt, initial, scope); err != nil {
				slog.Debug("auto-resume pass", "error", err)
			} else {
				initial = false
			}
			if time.Since(lastReadiness) >= time.Minute {
				lastReadiness = time.Now()
				_ = accelerateRecoveredQuotaAlarms(store, serverURL, lastReadiness, scope)
			}
			if err := dispatchDueWakeAlarms(store, serverURL, cmuxPath, 5*time.Second, startedAt, out, scope); err != nil {
				slog.Debug("auto-resume dispatch", "error", err)
			}
			if setting, err := fetchAutoResumeSetting(serverURL); err == nil {
				answerPrompts(serverURL, cmuxPath, setting, stalls, out)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// claimResumerWant records this process when it is newer than the one
// recorded, or when the recorded process has exited.
func claimResumerWant(path string) {
	version, pid := readResumerWant(path)
	if version > resumerProtocolVersion && processAlive(pid) {
		return
	}
	if version == resumerProtocolVersion && processAlive(pid) {
		return
	}
	_ = os.WriteFile(path, []byte(fmt.Sprintf("%d %d\n", resumerProtocolVersion, os.Getpid())), 0o600)
}

func newerResumerWanted(path string) bool {
	version, pid := readResumerWant(path)
	return version > resumerProtocolVersion && pid != os.Getpid() && processAlive(pid)
}

func readResumerWant(path string) (version, pid int) {
	body, err := os.ReadFile(path)
	if err != nil {
		return 0, 0
	}
	fields := strings.Fields(string(body))
	if len(fields) != 2 {
		return 0, 0
	}
	version, _ = strconv.Atoi(fields[0])
	pid, _ = strconv.Atoi(fields[1])
	return version, pid
}

func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// startSessionResumer runs this Mac's resumer alongside an sr agent session.
// An empty serverURL means this Mac's selected pool server. It does nothing
// outside cmux, and stops when the session ends.
func startSessionResumer(serverURL string) func() {
	if strings.TrimSpace(os.Getenv("CMUX_SURFACE_ID")) == "" {
		return func() {}
	}
	if strings.TrimSpace(serverURL) == "" {
		resolved, err := srRunner{program: "sr", store: accounts.DefaultCodexStore()}.wakeServerURL()
		if err != nil {
			slog.Debug("auto-resume: no pool server", "error", err)
			return func() {}
		}
		serverURL = resolved
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		runResumer(ctx, strings.TrimSuffix(strings.TrimRight(serverURL, "/"), "/v1"), io.Discard)
	}()
	return func() {
		cancel()
		<-done
	}
}

// autoResumeWatch runs the resumer in the foreground until Ctrl-C.
func (r srRunner) autoResumeWatch() error {
	serverURL, err := r.wakeServerURL()
	if err != nil {
		return err
	}
	if _, err := wakeCmuxPath(); err != nil {
		return err
	}
	// A pool that is briefly unreachable must not end the watch: on m1 one
	// startup timeout left the Mac with no resumer. The loop retries.
	if _, err := fetchAutoResumeSetting(serverURL); err != nil {
		fmt.Fprintf(r.out, "Cannot reach the pool yet (%v); retrying every 15s.\n", err)
	}
	fmt.Fprintf(r.out, "Watching every cmux tab on this Mac for auto-resume (pool %s).\nLeave this open while sessions started before the sr update are still running. Ctrl-C stops it.\n", serverURL)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	runResumer(ctx, serverURL, r.out)
	fmt.Fprintln(r.out, "Stopped.")
	return nil
}

// autoResumeStatus prints the pool-wide switch.
func (r srRunner) autoResumeFleetStatus() error {
	serverURL, err := r.wakeServerURL()
	if err != nil {
		return err
	}
	setting, err := fetchAutoResumeSetting(serverURL)
	if err != nil {
		return err
	}
	onOff := map[bool]string{true: "enabled", false: "disabled"}
	fmt.Fprintf(r.out, "claude auto-resume: %s\ncodex auto-resume: %s\npool: %s (applies to every machine using it)\n", onOff[setting.Claude], onOff[setting.Codex], serverURL)
	return nil
}

func (r srRunner) autoResumeSet(agent string, enabled bool) error {
	if agent != "claude" && agent != "codex" {
		return fmt.Errorf("usage: sr auto-resume %s claude|codex", map[bool]string{true: "enable", false: "disable"}[enabled])
	}
	serverURL, err := r.wakeServerURL()
	if err != nil {
		return err
	}
	if err := postAutoResume(serverURL, "/_subrouter/auto-resume", map[string]any{"agent": agent, "enabled": enabled}, nil); err != nil {
		return err
	}
	fmt.Fprintf(r.out, "%s auto-resume %s for every machine using %s\n", agent, map[bool]string{true: "enabled", false: "disabled"}[enabled], serverURL)
	return nil
}

// autoResumeTest checks this Mac end to end without a model request: the
// proxy records a test session as out of quota and already reset, the
// resumer binds it to this tab, and this command reads what gets typed.
func (r srRunner) autoResumeTest(args []string) error {
	agent := "claude"
	if len(args) > 0 {
		agent = args[0]
	}
	if agent != "claude" && agent != "codex" {
		return errors.New("usage: sr auto-resume test [claude|codex]")
	}
	surface := strings.TrimSpace(os.Getenv("CMUX_SURFACE_ID"))
	if surface == "" {
		return errors.New("run sr auto-resume test inside a cmux tab")
	}
	serverURL, err := r.wakeServerURL()
	if err != nil {
		return err
	}
	cmuxPath, err := wakeCmuxPath()
	if err != nil {
		return err
	}
	return r.runAutoResumeTest(serverURL, cmuxPath, surface, agent)
}

func (r srRunner) runAutoResumeTest(serverURL, cmuxPath, surface, agent string) error {
	setting, err := fetchAutoResumeSetting(serverURL)
	if err != nil {
		return err
	}
	if !setting.Enabled(agent) {
		return fmt.Errorf("%s auto-resume is off for this pool; turn it on first (sr auto-resume enable %s)", agent, agent)
	}
	var suffix [6]byte
	_, _ = rand.Read(suffix[:])
	session := "sr-auto-resume-test-" + hex.EncodeToString(suffix[:])
	if err := postAutoResume(serverURL, "/_subrouter/auto-resume/test", map[string]string{"agent": agent, "session_id": session}, nil); err != nil {
		return err
	}
	dir, err := os.MkdirTemp("", "sr-auto-resume-test-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	store := wake.NewStore(filepath.Join(dir, "alarms.json"))
	scope := fleetWakeScope(serverURL)
	scope.testBinding = &cmuxSessionWire{Agent: agent, SessionID: session, SurfaceID: surface}
	fmt.Fprintf(r.out, "Testing %s auto-resume in this tab against %s.\nThe proxy now reports a test session that ran out of quota and has recovered;\nthe resumer should type into this tab within a few seconds.\n", agent, serverURL)
	if err := syncRecoveryAlarms(store, serverURL, cmuxPath, time.Now().UTC(), true, scope); err != nil {
		return err
	}
	if err := dispatchDueWakeAlarms(store, serverURL, cmuxPath, 0, time.Now().UTC(), io.Discard, scope); err != nil {
		return err
	}
	line := make(chan string, 1)
	go func() {
		text, _ := bufio.NewReader(r.in).ReadString('\n')
		line <- strings.TrimSpace(text)
	}()
	select {
	case got := <-line:
		if got == "continue" || got == "/goal resume" {
			fmt.Fprintf(r.out, "PASS: this tab received %q.\n", got)
			return nil
		}
		return fmt.Errorf("FAIL: this tab received %q, want continue", got)
	case <-time.After(30 * time.Second):
		return errors.New("FAIL: nothing was typed into this tab within 30 seconds")
	}
}

// autoResumeRules lists, adds, removes or resets the pool's prompt rules:
//
//	sr auto-resume rules
//	sr auto-resume rules add --agent claude --question REGEX --answer 1 [--files claude-memory|GLOB] --note TEXT
//	sr auto-resume rules remove N
//	sr auto-resume rules reset
func (r srRunner) autoResumeRules(args []string) error {
	serverURL, err := r.wakeServerURL()
	if err != nil {
		return err
	}
	return r.autoResumeRulesAt(serverURL, args)
}

func (r srRunner) autoResumeRulesAt(serverURL string, args []string) error {
	setting, err := fetchAutoResumeSetting(serverURL)
	if err != nil {
		return err
	}
	rules := setting.EffectivePromptRules()
	put := func(next []proxy.PromptRule) error {
		payload, err := json.Marshal(next)
		if err != nil {
			return err
		}
		req, err := http.NewRequest(http.MethodPut, strings.TrimRight(serverURL, "/")+"/_subrouter/auto-resume/rules", bytes.NewReader(payload))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := autoResumeHTTPClient().Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			message, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
			return fmt.Errorf("rules: %s %s", resp.Status, strings.TrimSpace(string(message)))
		}
		return nil
	}
	if len(args) == 0 {
		if len(rules) == 0 {
			fmt.Fprintln(r.out, "No prompt rules: every permission prompt waits for you.")
			return nil
		}
		fmt.Fprintf(r.out, "Prompt rules for the pool at %s (first match wins; anything else waits for you):\n", serverURL)
		for i, rule := range rules {
			files := rule.Files
			if files == "" {
				files = "any file"
			}
			fmt.Fprintf(r.out, "%d. %s: when asked %q about %s, answer %q\n   why: %s\n", i+1, rule.Agent, rule.Question, files, rule.Answer, rule.Note)
		}
		fmt.Fprintln(r.out, "\nStall rules (a quiet tab whose last lines match is resumed with continue or /goal resume, with backoff):")
		for _, rule := range setting.EffectiveStallRules() {
			fmt.Fprintf(r.out, "- %s: screen matches %q\n   why: %s\n", rule.Agent, rule.Screen, rule.Note)
		}
		return nil
	}
	switch args[0] {
	case "add":
		var rule proxy.PromptRule
		for i := 1; i < len(args); i++ {
			if i+1 >= len(args) {
				return fmt.Errorf("%s needs a value", args[i])
			}
			value := args[i+1]
			switch args[i] {
			case "--agent":
				rule.Agent = value
			case "--question":
				rule.Question = value
			case "--files":
				rule.Files = value
			case "--answer":
				rule.Answer = value
			case "--note":
				rule.Note = value
			default:
				return fmt.Errorf("unknown option %q", args[i])
			}
			i++
		}
		if rule.Note == "" {
			return errors.New("give the rule a --note saying why, so 'sr auto-resume why' can explain what it did")
		}
		if err := put(append(rules, rule)); err != nil {
			return err
		}
		fmt.Fprintf(r.out, "added rule %d for every machine using %s\n", len(rules)+1, serverURL)
	case "remove":
		n := 0
		if len(args) == 2 {
			n, _ = strconv.Atoi(args[1])
		}
		if n < 1 || n > len(rules) {
			return fmt.Errorf("usage: sr auto-resume rules remove N (1-%d)", len(rules))
		}
		if err := put(append(append([]proxy.PromptRule{}, rules[:n-1]...), rules[n:]...)); err != nil {
			return err
		}
		fmt.Fprintf(r.out, "removed rule %d\n", n)
	case "reset":
		req, err := http.NewRequest(http.MethodDelete, strings.TrimRight(serverURL, "/")+"/_subrouter/auto-resume/rules", nil)
		if err != nil {
			return err
		}
		resp, err := autoResumeHTTPClient().Do(req)
		if err != nil {
			return err
		}
		resp.Body.Close()
		fmt.Fprintln(r.out, "prompt rules reset to the defaults")
	default:
		return errors.New("usage: sr auto-resume rules [add ...|remove N|reset]")
	}
	return nil
}

// autoResumeWhy explains recent automatic actions across the pool, newest
// first: each prompt answered and the rule that answered it, each session
// resumed and the failure that caused it, and each prompt still waiting.
func (r srRunner) autoResumeWhy() error {
	serverURL, err := r.wakeServerURL()
	if err != nil {
		return err
	}
	return r.autoResumeWhyAt(serverURL)
}

func (r srRunner) autoResumeWhyAt(serverURL string) error {
	type event struct {
		at   time.Time
		text string
	}
	var events []event
	var reports []proxy.PromptReport
	if err := getAutoResumeJSON(serverURL, "/_subrouter/auto-resume/prompts", &reports); err != nil {
		return err
	}
	waiting := 0
	for _, p := range reports {
		where := p.Host + " " + p.Agent + " tab " + p.SurfaceID
		if strings.HasPrefix(p.Question, "resume decision for session ") {
			events = append(events, event{p.LastSeen, fmt.Sprintf("%s %s: %s", where, strings.TrimPrefix(p.Question, "resume decision for "), p.AnsweredBy)})
		} else if p.AnsweredBy != "" {
			events = append(events, event{p.LastSeen, fmt.Sprintf("answered %q in %s\n    because: %s", p.Question, where, p.AnsweredBy)})
		} else if time.Since(p.LastSeen) < 2*time.Minute {
			waiting++
			events = append(events, event{p.LastSeen, fmt.Sprintf("WAITING ON YOU: %q in %s\n    no rule matches; answer it, or add one with sr auto-resume rules add", p.Question, where)})
		}
	}
	var states []recoveryWireState
	if err := getAutoResumeJSON(serverURL, "/_subrouter/recovery-status", &states); err == nil {
		for _, s := range states {
			if s.LastReplayAt.IsZero() || strings.HasPrefix(s.SessionID, "sr-auto-resume-test-") {
				continue
			}
			cause := "a quota failure"
			if s.Kind == wake.KindCodexProvider {
				cause = "a temporary provider failure"
			}
			reset := ""
			if !s.ResetAt.IsZero() {
				reset = ", quota reset " + s.ResetAt.Local().Format("Jan 2 15:04")
			}
			events = append(events, event{s.LastReplayAt, fmt.Sprintf("typed %q into %s session %s\n    because: %s at %s%s, and the session had not run since",
				s.LastReplayAction, s.Agent, s.SessionID, cause, s.LastFailureAt.Local().Format("Jan 2 15:04"), reset)})
		}
	}
	if len(events) == 0 {
		fmt.Fprintln(r.out, "Nothing automatic has happened recently, and nothing is waiting on you.")
		return nil
	}
	// A scorecard first, so how well this is working shows at a glance.
	answered, resumed, failed := 0, 0, 0
	for _, p := range reports {
		switch {
		case strings.HasPrefix(p.Question, "resume decision for session "):
		case strings.HasPrefix(p.Question, "could not submit"):
			failed += p.Count
		case strings.HasPrefix(p.AnsweredBy, "typed "):
			resumed += p.Count
		case p.AnsweredBy != "":
			answered += p.Count
		}
	}
	for _, s := range states {
		if !s.LastReplayAt.IsZero() && !strings.HasPrefix(s.SessionID, "sr-auto-resume-test-") {
			resumed++
		}
	}
	fmt.Fprintf(r.out, "Since the pool started: %d prompts answered, %d sessions resumed, %d resumes that failed to submit, %d prompts waiting on you now.\n\n", answered, resumed, failed, waiting)
	sort.Slice(events, func(i, j int) bool { return events[i].at.After(events[j].at) })
	if len(events) > 20 {
		events = events[:20]
	}
	for _, e := range events {
		fmt.Fprintf(r.out, "%s  %s\n", e.at.Local().Format("Jan 2 15:04:05"), e.text)
	}
	if waiting == 0 {
		fmt.Fprintln(r.out, "\nNothing is waiting on you right now.")
	}
	fmt.Fprintln(r.out, "Rules: sr auto-resume rules")
	return nil
}

func getAutoResumeJSON(serverURL, path string, out any) error {
	resp, err := autoResumeHTTPClient().Get(strings.TrimRight(serverURL, "/") + path)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: %s", path, resp.Status)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// decisionReporter tells the proxy, once per session and decision, why a
// stopped session on this Mac was or was not scheduled, so
// `sr auto-resume why` can answer "why didn't it resume?".
type decisionReporter struct {
	serverURL string
	out       io.Writer
	host      string
	last      map[string]string
}

func newDecisionReporter(serverURL string, out io.Writer) *decisionReporter {
	host, _ := os.Hostname()
	return &decisionReporter{serverURL: serverURL, out: out, host: strings.Split(host, ".")[0], last: map[string]string{}}
}

func (d *decisionReporter) report(state recoveryWireState, surface, decision string) {
	key := state.Agent + "\x00" + state.SessionID
	if d.last[key] == decision {
		return
	}
	d.last[key] = decision
	fmt.Fprintf(d.out, "%s %s session %s: %s\n", time.Now().Format("15:04:05"), state.Agent, state.SessionID, decision)
	_ = postAutoResume(d.serverURL, "/_subrouter/auto-resume/prompts", proxy.PromptReport{
		Host: d.host, Agent: state.Agent, SurfaceID: surface,
		Question:   "resume decision for session " + state.SessionID,
		AnsweredBy: decision,
	}, nil)
}
