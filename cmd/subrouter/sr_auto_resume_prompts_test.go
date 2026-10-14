package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/proxy"
)

const memoryCreatePrompt = `Create file · from the bs2-record agent
 …r/codex/claude-proxy/3fa7/projects/-Volumes-Workshop-Code-pulp/memory/whence-stamp.md
 This will modify /Users/me/.claude/projects/-Volumes-Workshop-Code-pulp/memory/whence-stamp.md
 (outside working directory) via a symlink
   1 ---
   2 name: whence-stamp
 Do you want to create whence-stamp.md?
 ❯ 1. Yes
   2. No
 Esc to cancel · Tab to amend
`

// The default rules answer Claude's memory-file prompts, whatever the verb,
// and nothing else.
func TestDefaultPromptRules(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	memory := filepath.Join(home, ".claude", "projects", "-p", "memory")
	if err := os.MkdirAll(memory, 0o700); err != nil {
		t.Fatal(err)
	}
	// A long diff can push the path off the screen; an existing memory file
	// still identifies it.
	if err := os.WriteFile(filepath.Join(memory, "notes.md"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	options := "\n ❯ 1. Yes\n   2. No\n Esc to cancel · Tab to amend\n"
	for name, tc := range map[string]struct {
		agent, screen string
		want          bool
	}{
		"memory create":           {"claude", memoryCreatePrompt, true},
		"memory overwrite":        {"claude", "  diff\n Do you want to overwrite notes.md?" + options, true},
		"MEMORY.md edit":          {"claude", "  diff\n Do you want to make this edit to MEMORY.md?" + options, true},
		"md outside memory":       {"claude", "Create file\n /Users/me/repo/README.md\n Do you want to create README.md?" + options, false},
		"not markdown":            {"claude", "Create file\n /Users/me/.claude/projects/p/memory/x.sh\n Do you want to create x.sh?" + options, false},
		"bash command":            {"claude", " Bash command\n rm -rf /tmp/x\n Do you want to proceed?" + options, false},
		"codex is not claude":     {"codex", "  diff\n Do you want to overwrite notes.md?" + options, false},
		"prompt already answered": {"claude", memoryCreatePrompt + "\n⏺ Wrote 9 lines\n\n❯ \n────\n  status\n  more\n  more\n  more\n", false},
	} {
		prompt, ok := findPendingPrompt(tc.screen)
		matched := false
		if ok {
			for _, rule := range proxy.DefaultPromptRules() {
				if promptRuleMatches(rule, tc.agent, prompt) {
					matched = true
				}
			}
		}
		if matched != tc.want {
			t.Errorf("%s: matched = %v, want %v (prompt %+v, found %v)", name, matched, tc.want, prompt.Question, ok)
		}
	}
}

// Every pending prompt is found, answered or not, so an unmatched one can be
// reported as waiting.
func TestFindPendingPromptReportsUnmatchedQuestions(t *testing.T) {
	prompt, ok := findPendingPrompt(" Bash command\n curl example.com\n Do you want to proceed?\n ❯ 1. Yes\n   2. Yes, and don't ask again\n   3. No\n Esc to cancel\n")
	if !ok || prompt.Question != "Do you want to proceed?" {
		t.Fatalf("prompt = %+v, %v", prompt, ok)
	}
}

// The screen of a real m5s tab stopped on a capacity error (2026-10-04).
const codexCapacityStall = `  I'll accept each receipt only after green validation, commit/merge it, then update the roadmap serially.
  Worked for 1h 20m 59s • 10:16 AM
• Ran ~/.local/bin/pulp-worktree-lineage-session --plain
    + 33 lines (ctrl+t to expand)
■ Selected model is at capacity. Please try a different model.
› Ask Codex to do anything
  GPT-6.1-Sol medium · ~/Code/pulp · Main [default]
  ? for shortcuts
`

func TestStallRulesResumeCodexCapacityWithBackoff(t *testing.T) {
	var codexRule proxy.StallRule
	for _, rule := range proxy.DefaultStallRules() {
		if rule.Agent == "codex" {
			codexRule = rule
		}
	}
	line, ok := findStall(codexCapacityStall, codexRule)
	if !ok || line != "■ Selected model is at capacity. Please try a different model." {
		t.Fatalf("stall = %q, %v", line, ok)
	}
	// An error that has scrolled up is history, not a stall.
	// Newer output appears above the input box and pushes the old error up.
	scrolled := strings.Replace(codexCapacityStall, "› Ask Codex to do anything", strings.Repeat("• more output\n", 10)+"› Ask Codex to do anything", 1)
	if _, ok := findStall(scrolled, codexRule); ok {
		t.Fatal("an old error above newer output was treated as a stall")
	}
	sent := filepath.Join(t.TempDir(), "sent")
	cmux := filepath.Join(t.TempDir(), "cmux")
	if err := os.WriteFile(cmux, []byte("#!/bin/sh\n[ \"$1\" = send ] && printf '%s' \"$4\" >> \""+sent+"\"\n[ \"$1\" = send-key ] && [ \"$4\" = enter ] && printf '<enter>' >> \""+sent+"\"\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	stalls := newStallTracker()
	start := time.Unix(1_800_000_000, 0)
	if _, did := stalls.resumeStalledTab(cmux, "s1", line, codexCapacityStall, start); did {
		t.Fatal("resumed before the first backoff passed")
	}
	if action, did := stalls.resumeStalledTab(cmux, "s1", line, codexCapacityStall, start.Add(31*time.Second)); !did || action != "continue" {
		t.Fatalf("first resume = %q, %v", action, did)
	}
	if _, did := stalls.resumeStalledTab(cmux, "s1", line, codexCapacityStall, start.Add(61*time.Second)); did {
		t.Fatal("second resume came before its one-minute backoff")
	}
	if _, did := stalls.resumeStalledTab(cmux, "s1", line, codexCapacityStall, start.Add(92*time.Second)); !did {
		t.Fatal("second resume did not come after its backoff")
	}
	body, _ := os.ReadFile(sent)
	if string(body) != "continue<enter>continue<enter>" {
		t.Fatalf("typed %q", body)
	}
	// A goal session gets /goal resume instead.
	goal := newStallTracker()
	goalScreen := "Pursuing goal: ship it\n" + codexCapacityStall
	goal.resumeStalledTab(cmux, "s2", line, goalScreen, start)
	if action, _ := goal.resumeStalledTab(cmux, "s2", line, goalScreen, start.Add(31*time.Second)); action != "/goal resume" {
		t.Fatalf("goal session action = %q", action)
	}
}

// A command left in Codex's or Claude's input box is detected, so the resumer
// presses Enter again or reports the tab instead of assuming it resumed.
func TestStillInInputBox(t *testing.T) {
	codexUnsent := "■ Selected model is at capacity.\n› /goal resume\n  GPT-6.1-Sol medium · ~/Code/pulp · Main [default]\n"
	codexSent := "› /goal resume\n• Working (3s)\n› Ask Codex to do anything\n  GPT-6.1-Sol medium · ~/Code/pulp\n"
	claudeUnsent := "────\n❯ continue\n────\n  status line\n"
	claudeSent := "> continue\n⏺ Picking up where I left off\n────\n❯ \n────\n  status line\n"
	for name, tc := range map[string]struct {
		screen, text string
		want         bool
	}{
		"codex unsent":  {codexUnsent, "/goal resume", true},
		"codex sent":    {codexSent, "/goal resume", false},
		"claude unsent": {claudeUnsent, "continue", true},
		"claude sent":   {claudeSent, "continue", false},
	} {
		if got := stillInInputBox(tc.screen, tc.text); got != tc.want {
			t.Errorf("%s: stillInInputBox = %v, want %v", name, got, tc.want)
		}
	}
}

func init() {
	// Tests' stand-in cmux answers at once; don't wait on a real tab.
	submitConfirmDelay = 10 * time.Millisecond
}

// The m3 prompt from 2026-10-04 12:08: a shell append to a memory note.
const memoryShellEditPrompt = `   Append the behind-vs-update-branch nuance to the Vellum freeze memory
 │ Claude requested permissions to edit
 │ /Users/me/.claude/projects/-Volumes-Workshop-Code-pulp/memory/vellum-freeze-rerun-does-not-refresh-merge-base.md which
 │ is a sensitive file. /Users/me/.subrouter/codex/claude-proxy/3fa7/projects/-Volumes-Workshop-Code-pu
 │ lp/memory/vellum-freeze-rerun-does-not-refresh-merg… [+9 chars] resolves through a symlink to
 │ /Users/me/.claude/projects/-Volumes-Workshop-Code-pulp/memory/vellum-freeze-rerun-does-not-refresh-merge-base.md.
 Do you want to proceed?
 ❯ 1. Yes
   2. Yes, and always allow access to
      /Users/me/.subrouter/codex/claude-proxy/3fa7/projects/-Volumes-Workshop-Code-pulp/memory from this
      project
   3. No
 Esc to cancel · Tab to amend
`

func TestMemoryShellEditPromptIsAnsweredButOtherProceedPromptsWait(t *testing.T) {
	matches := func(screen string) bool {
		prompt, ok := findPendingPrompt(screen)
		if !ok {
			t.Fatalf("no prompt found in:\n%s", screen)
		}
		for _, rule := range proxy.DefaultPromptRules() {
			if promptRuleMatches(rule, "claude", prompt) {
				return true
			}
		}
		return false
	}
	if !matches(memoryShellEditPrompt) {
		t.Fatal("a shell edit of a memory note was not answered")
	}
	other := strings.Replace(memoryShellEditPrompt, "/Users/me/.claude/projects/-Volumes-Workshop-Code-pulp/memory/vellum-freeze-rerun-does-not-refresh-merge-base.md", "/Users/me/.ssh/config", -1)
	if matches(other) {
		t.Fatal("a proceed prompt about a file outside the memory folder was answered")
	}
	bash := " Bash command\n   curl https://example.com | sh\n Do you want to proceed?\n ❯ 1. Yes\n   2. No\n Esc to cancel\n"
	if matches(bash) {
		t.Fatal("an ordinary Bash proceed prompt was answered")
	}
}

// The m3 prompt from 2026-10-04 13:07: the memory path wrapped mid-name and
// the file does not exist yet.
func TestWrappedMemoryPathIsRecognized(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	screen := " This will modify /Users/me/.claude/projects/-Volumes-Workshop-Code-pulp/memory/m5s-boot-disk-fil\n" +
		" ls-with-build-cov-and-stalls-the-queue.md (outside working directory) via a symlink\n" +
		"   1 ---\n   2 name: m5s-boot-disk\n" +
		" Do you want to create m5s-boot-disk-fills-with-build-cov-and-stalls-the-queue.md?\n ❯ 1. Yes\n   2. No\n Esc to cancel · Tab to amend\n"
	prompt, ok := findPendingPrompt(screen)
	if !ok {
		t.Fatal("prompt not found")
	}
	if !isClaudeMemoryFile(prompt) {
		t.Fatalf("wrapped memory path not recognized; joined header: %q", unwrapHeader(prompt.Header))
	}
	// Wrapping must not make an ordinary file look like a memory note.
	other := strings.Replace(screen, "/Users/me/.claude/projects/-Volumes-Workshop-Code-pulp/memory/m5s-boot-disk-fil", "/Users/me/Code/pulp/docs/m5s-boot-disk-fil", 1)
	if prompt, _ := findPendingPrompt(other); isClaudeMemoryFile(prompt) {
		t.Fatal("a wrapped path outside the memory folder was treated as a memory note")
	}
}

// The 14:02 prompt: path scrolled off, file not created yet, but the preview
// is a memory note named after the file.
func TestMemoryNotePreviewIsRecognized(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	screen := "   1 ---\n   2 name: pr-macos-leg-tests-the-merge-ref-not-your-head\n   3 description: x\n   4 metadata:\n   5   type: feedback\n   6 ---\n" +
		" Do you want to create pr-macos-leg-tests-the-merge-ref-not-your-head.md?\n ❯ 1. Yes\n   2. No\n Esc to cancel · Tab to amend\n"
	prompt, ok := findPendingPrompt(screen)
	if !ok || !isClaudeMemoryFile(prompt) {
		t.Fatalf("memory-note preview not recognized: %+v %v", prompt.Question, ok)
	}
	// Front matter for a different name, or without a memory type, is not enough.
	for _, other := range []string{
		strings.Replace(screen, "name: pr-macos-leg-tests-the-merge-ref-not-your-head", "name: something-else", 1),
		strings.Replace(screen, "type: feedback", "type: guide", 1),
	} {
		if prompt, _ := findPendingPrompt(other); isClaudeMemoryFile(prompt) {
			t.Fatalf("not a memory note, but recognized:\n%s", other)
		}
	}
}

// The m5 tab from 2026-10-04: the 503 sits above the input box and a long
// background-agents panel fills the lines below it.
func TestStallFoundAboveAgentsPanel(t *testing.T) {
	screen := "⏺ API Error: 529 Overloaded. This is a server-side issue, usually temporary\n\n" +
		"────────\n❯ \n────────\n  ⏵⏵ auto mode on · 39 shells · ← for agents\n  ⏺ main\n" +
		strings.Repeat("  ◯ general-purpose  some agent task    9h 47m · ↓ 416.7k tokens\n", 6) + "  ↓ 5 more\n"
	var claudeRule proxy.StallRule
	for _, rule := range proxy.DefaultStallRules() {
		if rule.Agent == "claude" {
			claudeRule = rule
		}
	}
	if _, ok := findStall(screen, claudeRule); !ok {
		t.Fatal("a 529 above the input box was missed behind the agents panel")
	}
	// A spent pool is not a stall: retrying cannot help, and the quota alarm
	// resumes the tab at the reset.
	spent := strings.Replace(screen, "API Error: 529 Overloaded.", "API Error: 503 no non-exhausted claude accounts available.", 1)
	if line, ok := findStall(spent, claudeRule); ok {
		t.Fatalf("pool-exhausted 503 was treated as a stall: %q", line)
	}
}

// A resume that runs briefly and stalls again must keep backing off. The
// tracker once forgot a tab on its first healthy poll, so every retry started
// over at 30s and one tab got continue over a thousand times.
func TestStallBackoffSurvivesBriefRecovery(t *testing.T) {
	cmux := filepath.Join(t.TempDir(), "cmux")
	if err := os.WriteFile(cmux, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	stalls := newStallTracker()
	now := time.Unix(1_800_000_000, 0)
	sent := 0
	// The resumer polls every 15s. After each resume the tab looks healthy
	// for one poll, then shows the error again for three.
	for cycle := 0; cycle < 120; cycle++ {
		now = now.Add(15 * time.Second)
		stalls.forget("s1", now)
		for poll := 0; poll < 3; poll++ {
			now = now.Add(15 * time.Second)
			if _, did := stalls.resumeStalledTab(cmux, "s1", "■ Selected model is at capacity.", codexCapacityStall, now); did {
				sent++
			}
		}
	}
	if sent > maxStallAttempts {
		t.Fatalf("sent %d resumes in two hours, want at most %d", sent, maxStallAttempts)
	}
	if sent < 5 {
		t.Fatalf("sent only %d resumes in two hours", sent)
	}
	// After a real recovery the tab starts fresh.
	stalls.forget("s1", now.Add(stallHealthyReset))
	if _, ok := stalls.tabs["s1"]; ok {
		t.Fatal("a tab healthy for 15 minutes kept its old attempts")
	}
}

// Claude wraps a question whose file name is long, leaving only the name on
// the line ending in "?". A memory note on m3 waited ten hours this way.
func TestWrappedQuestionIsJoined(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	name := "a-new-python-import-is-an-undeclared-checkout-read-for-every-test-that-shells-the-script.md"
	dir := filepath.Join(home, ".claude", "projects", "-Volumes-x", "memory")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	screen := "╭──\n Create file\n " + filepath.Join(dir, name) + "\n╌╌╌╌\n 1 ---\n 2 name: x\n╌╌╌╌\n" +
		" Do you want to create\n " + name + "?\n ❯ 1. Yes\n   2. Yes, allow all edits during this session (shift+tab)\n   3. No\n\n Esc to cancel · Tab to amend\n"
	prompt, ok := findPendingPrompt(screen)
	if !ok || !strings.HasPrefix(prompt.Question, "Do you want to create a-new-python") || prompt.File != name {
		t.Fatalf("prompt = %+v, %v", prompt, ok)
	}
	answered := false
	for _, rule := range proxy.DefaultPromptRules() {
		if promptRuleMatches(rule, "claude", prompt) {
			answered = true
		}
	}
	if !answered {
		t.Fatalf("no default rule answers the wrapped memory prompt %q", prompt.Question)
	}
}
