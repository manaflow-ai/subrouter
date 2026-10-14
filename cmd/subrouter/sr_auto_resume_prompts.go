package main

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode"

	"github.com/manaflow-ai/subrouter/internal/proxy"
)

// Prompt rules answer a permission prompt an agent tab is waiting on. The
// rules are data on the pool's proxy (proxy.PromptRule; see
// `sr auto-resume rules`), so adding one needs no new sr. The resumer only
// reads the tab, finds the pending prompt, applies the first matching rule,
// and reports what it saw, answered or not, to the proxy's dashboard. A
// prompt no rule matches keeps waiting for a person.

// pendingPrompt is a permission prompt a tab is waiting on.
type pendingPrompt struct {
	Question string
	File     string
	// Header holds the lines above the question, where Claude names the
	// file's full path.
	Header []string
}

var (
	promptOptionLine = regexp.MustCompile(`^[❯›>]?\s*1\.\s+\S`)
	promptFileToken  = regexp.MustCompile(`(\S+\.[A-Za-z0-9]+)\?$`)
)

// findPendingPrompt returns the prompt the screen ends on, if any: a question
// line followed by numbered options and a cancel hint, at the bottom.
func findPendingPrompt(screen string) (pendingPrompt, bool) {
	lines := strings.Split(strings.TrimRight(screen, "\n"), "\n")
	end := len(lines)
	for end > 0 && strings.TrimSpace(lines[end-1]) == "" {
		end--
	}
	tail := strings.Join(lines[max(0, end-8):end], "\n")
	if !strings.Contains(tail, "Esc to cancel") && !strings.Contains(tail, "esc to cancel") {
		return pendingPrompt{}, false
	}
	option := -1
	for i := end - 1; i >= 0 && i >= end-10; i-- {
		if promptOptionLine.MatchString(strings.TrimSpace(lines[i])) {
			option = i
			break
		}
	}
	if option < 0 {
		return pendingPrompt{}, false
	}
	for i := option - 1; i >= 0 && i >= option-4; i-- {
		question := strings.TrimSpace(lines[i])
		if strings.HasSuffix(question, "?") {
			// A long file name wraps the question, leaving only the name on
			// the last line; join the lines it continues. A question starts
			// with a capital ("Do you want…"), a wrapped remainder does not.
			start := i
			for start > 0 && i-start < 2 && !startsWithUpper(question) {
				prev := strings.TrimSpace(lines[start-1])
				if prev == "" || strings.Trim(prev, "─━-╌") == "" || strings.HasSuffix(prev, "?") {
					break
				}
				start--
				question = prev + " " + question
			}
			prompt := pendingPrompt{Question: question, Header: lines[max(0, start-60):start]}
			if m := promptFileToken.FindStringSubmatch(question); m != nil {
				prompt.File = filepath.Base(m[1])
			}
			return prompt, true
		}
	}
	return pendingPrompt{}, false
}

// promptRuleMatches reports whether rule answers prompt in an agent's tab.
func promptRuleMatches(rule proxy.PromptRule, agent string, prompt pendingPrompt) bool {
	if rule.Agent != agent {
		return false
	}
	question, err := regexp.Compile(rule.Question)
	if err != nil || !question.MatchString(prompt.Question) {
		return false
	}
	switch rule.Files {
	case "":
		return true
	case "claude-memory":
		return isClaudeMemoryFile(prompt)
	default:
		for _, line := range prompt.Header {
			for _, field := range strings.Fields(line) {
				if ok, _ := filepath.Match(rule.Files, field); ok && strings.HasSuffix(field, prompt.File) {
					return true
				}
			}
		}
		return false
	}
}

// isClaudeMemoryFile reports whether the prompt's file is in Claude's own
// memory folder, from the path Claude shows or, when a long diff pushed that
// off the screen, from the file existing in a memory folder.
func isClaudeMemoryFile(prompt pendingPrompt) bool {
	if prompt.File == "" {
		// A shell command that writes a memory note asks a generic "Do you
		// want to proceed?"; the header names the file instead.
		return headerRequestsMemoryEdit(prompt.Header)
	}
	if !strings.HasSuffix(prompt.File, ".md") {
		return false
	}
	if prompt.File == "MEMORY.md" {
		return true
	}
	// A long path wraps across screen lines, so look at the header joined.
	joined := unwrapHeader(prompt.Header)
	if strings.Contains(joined, "/.claude/projects/") && strings.Contains(joined, "/memory/"+prompt.File) {
		return true
	}
	home, err := os.UserHomeDir()
	if err == nil {
		matches, _ := filepath.Glob(filepath.Join(home, ".claude", "projects", "*", "memory", prompt.File))
		if len(matches) > 0 {
			return true
		}
	}
	// A long preview can push the path off the screen, and a new note does
	// not exist yet. Claude's memory notes carry front matter whose name is
	// the file name and whose type is one of four memory types.
	return previewIsMemoryNote(prompt.Header, strings.TrimSuffix(prompt.File, ".md"))
}

var (
	memoryNameLine = regexp.MustCompile(`^\s*\d+\s+name:\s*(\S+)\s*$`)
	memoryTypeLine = regexp.MustCompile(`^\s*\d+\s+type:\s*(user|feedback|project|reference)\s*$`)
)

// previewIsMemoryNote reports whether the file preview in a prompt is a
// Claude memory note named stem.
func previewIsMemoryNote(header []string, stem string) bool {
	named, typed := false, false
	for _, line := range header {
		line = strings.Trim(strings.TrimSpace(line), "│|")
		if m := memoryNameLine.FindStringSubmatch(line); m != nil && m[1] == stem {
			named = true
		}
		if memoryTypeLine.MatchString(line) {
			typed = true
		}
	}
	return named && typed
}

// promptQuietFor is how long a tab must be unchanged before it is read: a
// tab waiting on a person is quiet, and a working one is skipped cheaply.
const promptQuietFor = 5 * time.Second

// answerPrompts applies the pool's prompt rules to every quiet agent tab.
func answerPrompts(serverURL, cmuxPath string, setting proxy.AutoResumeSetting, stalls *stallTracker, out io.Writer) {
	sessions, err := readCMUXSessions(cmuxPath)
	if err != nil {
		return
	}
	host, _ := os.Hostname()
	host = strings.Split(host, ".")[0]
	rules := setting.EffectivePromptRules()
	now := time.Now()
	seen := map[string]bool{}
	for _, session := range sessions {
		if (session.Agent != "claude" && session.Agent != "codex") || !setting.Enabled(session.Agent) ||
			session.SurfaceID == "" || seen[session.SurfaceID] {
			continue
		}
		seen[session.SurfaceID] = true
		if updated, err := time.Parse(time.RFC3339Nano, session.UpdatedAt); err == nil && now.Sub(updated) < promptQuietFor {
			continue
		}
		screen, err := readSurface(cmuxPath, session.SurfaceID)
		if err != nil {
			continue
		}
		prompt, ok := findPendingPrompt(screen)
		if !ok {
			resumeIfStalled(serverURL, cmuxPath, host, session, screen, setting, stalls, now, out)
			continue
		}
		report := proxy.PromptReport{Host: host, Agent: session.Agent, SurfaceID: session.SurfaceID, Question: prompt.Question, File: prompt.File}
		for _, rule := range rules {
			if !promptRuleMatches(rule, session.Agent, prompt) {
				continue
			}
			if answerPromptAndConfirm(cmuxPath, session.SurfaceID, rule.Answer, prompt) == nil {
				report.AnsweredBy = rule.Note
				if report.AnsweredBy == "" {
					report.AnsweredBy = rule.Question
				}
				io.WriteString(out, now.Format("15:04:05")+" answered "+session.Agent+" prompt: "+prompt.Question+"\n")
			}
			break
		}
		if report.AnsweredBy == "" {
			io.WriteString(out, now.Format("15:04:05")+" waiting on you: "+session.Agent+" in "+session.SurfaceID+": "+prompt.Question+"\n")
		}
		_ = postAutoResume(serverURL, "/_subrouter/auto-resume/prompts", report, nil)
	}
}

// stallTracker remembers, per tab, how often a stall rule has resumed it, so
// a tab that keeps stopping is retried with backoff rather than hammered.
type stallTracker struct {
	tabs map[string]*stalledTab
}

type stalledTab struct {
	firstSeen time.Time
	// lastStalled is the last poll that saw the tab stalled.
	lastStalled time.Time
	lastSent    time.Time
	attempts    int
	line        string
	// failed is set when the last resume never left the input box.
	failed bool
}

func newStallTracker() *stallTracker { return &stallTracker{tabs: map[string]*stalledTab{}} }

// stallBackoff is how long to wait before each resume of a stalled tab.
var stallBackoff = []time.Duration{30 * time.Second, time.Minute, 2 * time.Minute, 5 * time.Minute, 10 * time.Minute}

// maxStallAttempts stops retrying a tab that never recovers (about 2 hours).
const maxStallAttempts = 14

// stallHealthyReset is how long a tab must go without stalling before its
// attempts start over. A resume that runs for a moment and fails again is
// still the same outage: resetting on the first healthy poll once made the
// backoff restart at 30s every time, typing continue into one tab over a
// thousand times while the pool stayed exhausted.
const stallHealthyReset = 15 * time.Minute

// notAStall matches errors a stall rule may catch but a retry cannot fix.
// "no non-exhausted accounts" means the whole pool is out of quota; the
// proxy records it and the quota alarm resumes the tab at the reset.
var notAStall = regexp.MustCompile(`no non-exhausted`)

// findStall returns the screen line a stall rule matches, when it is among
// the last lines of the tab, so an error that has scrolled up is ignored.
func findStall(screen string, rule proxy.StallRule) (string, bool) {
	pattern, err := regexp.Compile(rule.Screen)
	if err != nil {
		return "", false
	}
	lines := strings.Split(strings.TrimRight(screen, "\n"), "\n")
	var recent []string
	for i := len(lines) - 1; i >= 0 && len(recent) < 8; i-- {
		if line := strings.TrimSpace(lines[i]); line != "" {
			recent = append(recent, line)
		}
	}
	// Claude can draw a long background-agents panel below its input box,
	// pushing the error out of the last lines; also read what sits just
	// above the input box, which is the last thing the agent printed.
	recent = append(recent, linesAboveInputBox(lines, 8)...)
	for _, line := range recent {
		if pattern.MatchString(line) && !notAStall.MatchString(line) {
			return line, true
		}
	}
	return "", false
}

// resumeStalledTab types continue or /goal resume into a tab stopped on a
// temporary failure, once its backoff has passed. It reports whether it sent.
func (t *stallTracker) resumeStalledTab(cmuxPath, surface, line, screen string, now time.Time) (string, bool) {
	tab := t.tabs[surface]
	if tab == nil {
		tab = &stalledTab{firstSeen: now}
		t.tabs[surface] = tab
	}
	tab.line, tab.lastStalled = line, now
	if tab.attempts >= maxStallAttempts {
		return "", false
	}
	wait := stallBackoff[min(tab.attempts, len(stallBackoff)-1)]
	since := tab.firstSeen
	if !tab.lastSent.IsZero() {
		since = tab.lastSent
	}
	if now.Sub(since) < wait {
		return "", false
	}
	action := resumeActionForSurface(screen)
	err := typeIntoTab(cmuxPath, surface, action)
	if err != nil && !errors.Is(err, errNotSubmitted) {
		return "", false
	}
	tab.attempts++
	tab.lastSent = now
	tab.failed = err != nil
	return action, true
}

// forget drops a tab once it has gone stallHealthyReset without stalling.
func (t *stallTracker) forget(surface string, now time.Time) {
	if tab := t.tabs[surface]; tab != nil && now.Sub(tab.lastStalled) >= stallHealthyReset {
		delete(t.tabs, surface)
	}
}

// resumeIfStalled applies the stall rules to a quiet tab with no prompt.
func resumeIfStalled(serverURL, cmuxPath, host string, session cmuxSessionWire, screen string, setting proxy.AutoResumeSetting, stalls *stallTracker, now time.Time, out io.Writer) {
	if stalls == nil {
		return
	}
	for _, rule := range setting.EffectiveStallRules() {
		if rule.Agent != session.Agent {
			continue
		}
		line, stalled := findStall(screen, rule)
		if !stalled {
			continue
		}
		action, sent := stalls.resumeStalledTab(cmuxPath, session.SurfaceID, line, screen, now)
		if !sent {
			return
		}
		report := proxy.PromptReport{
			Host: host, Agent: session.Agent, SurfaceID: session.SurfaceID,
			Question: "stalled on: " + line, AnsweredBy: "typed " + action + " — " + rule.Note,
		}
		if tab := stalls.tabs[session.SurfaceID]; tab != nil && tab.failed {
			// Reported as waiting, so why and the dashboard flag it.
			report.Question, report.AnsweredBy = "could not submit "+action+" after: "+line, ""
			io.WriteString(out, now.Format("15:04:05")+" FAILED to submit "+action+" in "+session.Agent+" tab "+session.SurfaceID+"\n")
		} else {
			io.WriteString(out, now.Format("15:04:05")+" typed "+action+" into "+session.Agent+" tab "+session.SurfaceID+" after: "+line+"\n")
		}
		_ = postAutoResume(serverURL, "/_subrouter/auto-resume/prompts", report, nil)
		return
	}
	stalls.forget(session.SurfaceID, now)
}

// typeIntoTab types text into a cmux tab, presses Enter, and confirms the
// command left the input box. A trailing newline in the text is not enough:
// Codex's composer treats it as a new line rather than a submit, so the
// command once sat unsent. If it is still in the input box after a second
// Enter, typeIntoTab returns errNotSubmitted for the caller to report.
func typeIntoTab(cmuxPath, surface, text string) error {
	text = strings.TrimRight(text, "\n")
	if err := exec.Command(cmuxPath, "send", "--surface", surface, text).Run(); err != nil {
		return err
	}
	for attempt := 0; attempt < 2; attempt++ {
		if err := exec.Command(cmuxPath, "send-key", "--surface", surface, "enter").Run(); err != nil {
			return err
		}
		time.Sleep(submitConfirmDelay)
		screen, err := readSurface(cmuxPath, surface)
		if err != nil || !stillInInputBox(screen, text) {
			return nil
		}
	}
	return errNotSubmitted
}

// submitConfirmDelay is how long a tab gets to take a submitted command.
var submitConfirmDelay = 2 * time.Second

var errNotSubmitted = errors.New("typed into the tab, but it is still in the input box after two Enter presses")

// stillInInputBox reports whether text is still waiting in the tab's input
// box: the lowest line starting with a prompt marker (Codex ›, Claude ❯).
func stillInInputBox(screen, text string) bool {
	lines := strings.Split(strings.TrimRight(screen, "\n"), "\n")
	for i := len(lines) - 1; i >= 0 && i >= len(lines)-12; i-- {
		line := strings.TrimSpace(lines[i])
		for _, marker := range []string{"›", "❯", ">"} {
			if strings.HasPrefix(line, marker) {
				return strings.TrimSpace(strings.TrimPrefix(line, marker)) == text
			}
		}
	}
	return false
}

// answerPromptAndConfirm sends a rule's answer and confirms the prompt is
// gone, trying once more before giving up.
func answerPromptAndConfirm(cmuxPath, surface, answer string, prompt pendingPrompt) error {
	for attempt := 0; attempt < 2; attempt++ {
		if err := exec.Command(cmuxPath, "send", "--surface", surface, answer).Run(); err != nil {
			return err
		}
		time.Sleep(submitConfirmDelay)
		screen, err := readSurface(cmuxPath, surface)
		if err != nil {
			return nil
		}
		if still, ok := findPendingPrompt(screen); !ok || still.Question != prompt.Question {
			return nil
		}
	}
	return errNotSubmitted
}

// headerRequestsMemoryEdit reports whether Claude's prompt header asks to edit
// or write a markdown file in a memory folder, and nothing else.
func headerRequestsMemoryEdit(header []string) bool {
	text := unwrapHeader(header)
	at := strings.LastIndex(text, "requested permissions to ")
	if at < 0 {
		return false
	}
	request := text[at:]
	if !strings.HasPrefix(request, "requested permissions to edit") && !strings.HasPrefix(request, "requested permissions to write") {
		return false
	}
	for _, field := range strings.Fields(request) {
		if strings.Contains(field, "/.claude/projects/") && strings.Contains(field, "/memory/") && strings.HasSuffix(strings.TrimRight(field, ".,"), ".md") {
			return true
		}
	}
	return false
}

// unwrapHeader rejoins a prompt header that the terminal wrapped: a line
// ending in the middle of a path continues on the next. Box-drawing borders
// and indentation are dropped, words stay separated by spaces.
func unwrapHeader(header []string) string {
	var b strings.Builder
	for _, raw := range header {
		line := strings.TrimSpace(strings.Trim(strings.TrimSpace(raw), "│|"))
		if line == "" {
			continue
		}
		if b.Len() > 0 {
			prev := b.String()
			// A wrapped path breaks mid-token: no space at the break.
			if strings.ContainsAny(prev[len(prev)-1:], "/-_.") || strings.HasPrefix(line, "/") && !strings.HasSuffix(prev, " ") && strings.Count(prev[max(0, strings.LastIndex(prev, " ")):], "/") > 0 {
				b.WriteString(line)
				continue
			}
			lastSpace := strings.LastIndex(prev, " ")
			if lastSpace < len(prev)-1 && strings.Contains(prev[lastSpace+1:], "/") && !strings.HasSuffix(prev, ".md") {
				b.WriteString(line)
				continue
			}
			b.WriteString(" ")
		}
		b.WriteString(line)
	}
	return b.String()
}

// linesAboveInputBox returns up to n content lines just above the tab's
// input box (the lowest line starting with ❯ or ›), skipping separators.
func linesAboveInputBox(lines []string, n int) []string {
	box := -1
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if strings.HasPrefix(line, "❯") || strings.HasPrefix(line, "›") {
			box = i
			break
		}
	}
	var out []string
	for i := box - 1; box > 0 && i >= 0 && len(out) < n; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" || strings.Trim(line, "─━-") == "" {
			continue
		}
		out = append(out, line)
	}
	return out
}

func startsWithUpper(s string) bool {
	for _, r := range s {
		return unicode.IsUpper(r)
	}
	return false
}
