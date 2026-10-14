package proxy

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/manaflow-ai/subrouter/internal/fsutil"
	"github.com/manaflow-ai/subrouter/wake"
)

// AutoResumeSetting is the pool-wide auto-resume switch for each agent. It
// lives on the proxy so one change applies to every machine using the pool.
type AutoResumeSetting struct {
	Claude bool `json:"claude"`
	Codex  bool `json:"codex"`
	// PromptRules answer permission prompts an agent would otherwise wait on.
	// Nil means DefaultPromptRules; an empty list means none.
	PromptRules []PromptRule `json:"prompt_rules,omitempty"`
	// StallRules resume a tab whose agent stopped on a temporary failure the
	// proxy cannot see, such as a capacity error inside a successful stream.
	// Nil means DefaultStallRules; an empty list means none.
	StallRules []StallRule `json:"stall_rules,omitempty"`
}

// StallRule recognizes an agent tab stopped on a temporary failure: Screen,
// a regular expression, matches one of the last lines of a quiet tab. The
// resumer then types continue (or /goal resume for a goal session), with
// backoff, until the tab moves on.
type StallRule struct {
	Agent  string `json:"agent"`
	Screen string `json:"screen"`
	Note   string `json:"note,omitempty"`
}

// DefaultStallRules are the stall rules a pool has before anyone changes them.
func DefaultStallRules() []StallRule {
	return []StallRule{
		{Agent: "codex", Screen: `(?i)(model is at capacity|server_is_overloaded|servers are currently overloaded|try a different model)`, Note: "Codex stopped on a temporary model-provider capacity error."},
		{Agent: "claude", Screen: `(?i)API Error: (5\d\d|Overloaded)|overloaded_error`, Note: "Claude stopped on a temporary provider overload."},
	}
}

// EffectiveStallRules returns the stall rules in force.
func (s AutoResumeSetting) EffectiveStallRules() []StallRule {
	if s.StallRules == nil {
		return DefaultStallRules()
	}
	return s.StallRules
}

// PromptRule answers one kind of permission prompt. A prompt that no rule
// matches keeps waiting for a person, and is reported to the dashboard.
type PromptRule struct {
	// Agent is "claude" or "codex".
	Agent string `json:"agent"`
	// Question is a regular expression matched against the prompt's question
	// line, for example `^Do you want to (create|overwrite|make this edit to) .+\.md\?$`.
	Question string `json:"question"`
	// Files, when set, also requires the prompt's file to be in that place:
	// "claude-memory" for ~/.claude/projects/<project>/memory/ (or MEMORY.md),
	// or a glob matched against the file's full path.
	Files string `json:"files,omitempty"`
	// Answer is the key sent to the tab, such as "1" for the first option.
	Answer string `json:"answer"`
	// Note says why the rule exists.
	Note string `json:"note,omitempty"`
}

// DefaultPromptRules are the rules a pool has before anyone changes them.
func DefaultPromptRules() []PromptRule {
	return []PromptRule{{
		Agent:    "claude",
		Question: `^Do you want to (create|overwrite|make this edit to|make these edits to) \S+\?$`,
		Files:    "claude-memory",
		Answer:   "1",
		Note:     "Claude saving its own memory notes; pooled sessions reach that folder through a symlink, so Claude asks every time.",
	}, {
		Agent:    "claude",
		Question: `^Do you want to proceed\?$`,
		Files:    "claude-memory",
		Answer:   "1",
		Note:     "Claude editing its own memory notes with a shell command; the prompt names a memory file as the only thing it touches.",
	}}
}

// EffectivePromptRules returns the rules in force.
func (s AutoResumeSetting) EffectivePromptRules() []PromptRule {
	if s.PromptRules == nil {
		return DefaultPromptRules()
	}
	return s.PromptRules
}

// Enabled reports the switch for an agent ("claude" or "codex").
func (s AutoResumeSetting) Enabled(agent string) bool {
	switch agent {
	case "claude":
		return s.Claude
	case "codex":
		return s.Codex
	}
	return false
}

// autoResumeSettingMu serializes writes to the setting file.
var autoResumeSettingMu sync.Mutex

// ReadAutoResumeSetting returns the saved setting; a missing file is off.
func ReadAutoResumeSetting(path string) (AutoResumeSetting, error) {
	var setting AutoResumeSetting
	if strings.TrimSpace(path) == "" {
		return setting, nil
	}
	body, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return setting, nil
	}
	if err != nil {
		return setting, err
	}
	err = json.Unmarshal(body, &setting)
	return setting, err
}

func writeAutoResumeSetting(path, agent string, enabled bool) (AutoResumeSetting, error) {
	autoResumeSettingMu.Lock()
	defer autoResumeSettingMu.Unlock()
	setting, err := ReadAutoResumeSetting(path)
	if err != nil {
		return setting, err
	}
	switch agent {
	case "claude":
		setting.Claude = enabled
	case "codex":
		setting.Codex = enabled
	default:
		return setting, errors.New("agent must be claude or codex")
	}
	body, err := json.MarshalIndent(setting, "", "  ")
	if err != nil {
		return setting, err
	}
	return setting, fsutil.WriteFileAtomic(path, append(body, '\n'), 0o600)
}

// handleAutoResume serves the pool-wide switch. GET returns it. POST with a
// JSON body {"agent":"claude","enabled":true} changes one agent and returns
// the result; a form POST from the dashboard redirects back to it.
func (s Server) handleAutoResume(w http.ResponseWriter, r *http.Request) {
	if strings.TrimSpace(s.AutoResumeSettingPath) == "" {
		http.Error(w, "auto-resume is not configured on this server", http.StatusNotFound)
		return
	}
	switch r.Method {
	case http.MethodGet:
		setting, err := ReadAutoResumeSetting(s.AutoResumeSettingPath)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		setting.PromptRules = setting.EffectivePromptRules()
		setting.StallRules = setting.EffectiveStallRules()
		writeJSON(w, setting)
	case http.MethodPost:
		agent, enabled, form := "", false, false
		if strings.HasPrefix(r.Header.Get("Content-Type"), "application/x-www-form-urlencoded") {
			if err := r.ParseForm(); err != nil {
				http.Error(w, "invalid form", http.StatusBadRequest)
				return
			}
			agent, enabled, form = r.PostForm.Get("agent"), r.PostForm.Get("enabled") == "true", true
		} else {
			var body struct {
				Agent   string `json:"agent"`
				Enabled bool   `json:"enabled"`
			}
			if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err != nil {
				http.Error(w, "invalid auto-resume update", http.StatusBadRequest)
				return
			}
			agent, enabled = body.Agent, body.Enabled
		}
		setting, err := writeAutoResumeSetting(s.AutoResumeSettingPath, agent, enabled)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if s.Logger != nil {
			s.Logger.Info("auto-resume setting changed", "agent", agent, "enabled", enabled)
		}
		if form {
			http.Redirect(w, r, "/_subrouter/dashboard#auto-resume", http.StatusSeeOther)
			return
		}
		writeJSON(w, setting)
	default:
		w.Header().Set("Allow", "GET, POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// handleAutoResumeTest records a quota failure, already past its reset, for
// a test session. `sr auto-resume test` binds that session to its own tab and
// checks that the resumer types into it, without any model request.
func (s Server) handleAutoResumeTest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		Agent     string `json:"agent"`
		SessionID string `json:"session_id"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err != nil ||
		(body.Agent != "claude" && body.Agent != "codex") ||
		!strings.HasPrefix(body.SessionID, "sr-auto-resume-test-") {
		http.Error(w, "agent must be claude or codex and session_id must start with sr-auto-resume-test-", http.StatusBadRequest)
		return
	}
	kind := wake.KindClaudeQuota
	if body.Agent == "codex" {
		kind = wake.KindCodexQuota
	}
	now := time.Now().UTC()
	// Failed two minutes ago and already reset, so the resume is due now.
	s.Recovery.RecordQuotaFailure(body.Agent, body.SessionID, kind, "test", now.Add(-2*time.Minute), now.Add(-3*time.Minute))
	writeJSON(w, map[string]string{"agent": body.Agent, "session_id": body.SessionID, "kind": kind})
}

func writeAutoResumeRules(path string, rules []PromptRule) (AutoResumeSetting, error) {
	autoResumeSettingMu.Lock()
	defer autoResumeSettingMu.Unlock()
	setting, err := ReadAutoResumeSetting(path)
	if err != nil {
		return setting, err
	}
	for _, rule := range rules {
		if rule.Agent != "claude" && rule.Agent != "codex" {
			return setting, errors.New("each rule needs agent claude or codex")
		}
		if _, err := regexp.Compile(rule.Question); err != nil || rule.Question == "" {
			return setting, fmt.Errorf("rule question %q is not a valid regular expression", rule.Question)
		}
		if rule.Answer == "" {
			return setting, errors.New("each rule needs an answer")
		}
	}
	if rules == nil {
		rules = []PromptRule{}
	}
	setting.PromptRules = rules
	body, err := json.MarshalIndent(setting, "", "  ")
	if err != nil {
		return setting, err
	}
	return setting, fsutil.WriteFileAtomic(path, append(body, '\n'), 0o600)
}

// handleAutoResumeRules replaces the prompt rules (PUT, JSON list), or
// restores the defaults (DELETE).
func (s Server) handleAutoResumeRules(w http.ResponseWriter, r *http.Request) {
	if strings.TrimSpace(s.AutoResumeSettingPath) == "" {
		http.Error(w, "auto-resume is not configured on this server", http.StatusNotFound)
		return
	}
	var rules []PromptRule
	switch r.Method {
	case http.MethodPut:
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&rules); err != nil {
			http.Error(w, "invalid rules", http.StatusBadRequest)
			return
		}
	case http.MethodDelete:
		rules = DefaultPromptRules()
	default:
		w.Header().Set("Allow", "PUT, DELETE")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	setting, err := writeAutoResumeRules(s.AutoResumeSettingPath, rules)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, setting.EffectivePromptRules())
}

// PromptReport is one permission prompt a resumer saw in an agent tab.
type PromptReport struct {
	Host      string `json:"host"`
	Agent     string `json:"agent"`
	SurfaceID string `json:"surface_id"`
	Question  string `json:"question"`
	File      string `json:"file,omitempty"`
	// AnsweredBy is the note of the rule that answered it; empty while the
	// prompt is waiting for a person.
	AnsweredBy string    `json:"answered_by,omitempty"`
	FirstSeen  time.Time `json:"first_seen"`
	LastSeen   time.Time `json:"last_seen"`
	Count      int       `json:"count"`
}

// promptReports keeps the most recent reports for the dashboard.
type promptReports struct {
	mu      sync.Mutex
	reports []PromptReport
	// path, when set, keeps the reports across a proxy restart or upgrade.
	path   string
	loaded bool
}

var recentPrompts = &promptReports{}

// usePath points the reports at a file next to the session store and loads
// what an earlier run saved.
func (p *promptReports) usePath(path string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.loaded && p.path == path {
		return
	}
	p.path, p.loaded = path, true
	if body, err := os.ReadFile(path); err == nil {
		var saved []PromptReport
		if json.Unmarshal(body, &saved) == nil {
			p.reports = saved
		}
	}
}

func (p *promptReports) saveLocked() {
	if p.path == "" {
		return
	}
	if body, err := json.Marshal(p.reports); err == nil {
		_ = fsutil.WriteFileAtomic(p.path, body, 0o600)
	}
}

const maxPromptReports = 200

func (p *promptReports) add(report PromptReport) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now().UTC()
	for i := range p.reports {
		r := &p.reports[i]
		if r.Host == report.Host && r.SurfaceID == report.SurfaceID && r.Question == report.Question && r.AnsweredBy == report.AnsweredBy {
			r.LastSeen, r.Count = now, r.Count+1
			p.saveLocked()
			return
		}
	}
	report.FirstSeen, report.LastSeen, report.Count = now, now, 1
	p.reports = append(p.reports, report)
	if len(p.reports) > maxPromptReports {
		p.reports = p.reports[len(p.reports)-maxPromptReports:]
	}
	p.saveLocked()
}

func (p *promptReports) list() []PromptReport {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := append([]PromptReport(nil), p.reports...)
	sort.Slice(out, func(i, j int) bool { return out[i].LastSeen.After(out[j].LastSeen) })
	return out
}

// handleAutoResumePrompts takes a resumer's report (POST) or lists recent
// reports (GET), so prompts blocking any tab on any machine show up in one
// place.
func (s Server) handleAutoResumePrompts(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, recentPrompts.list())
	case http.MethodPost:
		var report PromptReport
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&report); err != nil || report.Question == "" {
			http.Error(w, "invalid prompt report", http.StatusBadRequest)
			return
		}
		recentPrompts.add(report)
		if s.Logger != nil {
			if report.AnsweredBy == "" {
				s.Logger.Warn("agent waiting on a prompt no rule answers", "host", report.Host, "agent", report.Agent, "surface", report.SurfaceID, "question", report.Question)
			} else {
				s.Logger.Info("prompt answered by rule", "host", report.Host, "agent", report.Agent, "question", report.Question, "rule", report.AnsweredBy)
			}
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		w.Header().Set("Allow", "GET, POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}
