package proxy

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/wake"
)

// A failure recorded before a proxy restart is still there afterwards, so the
// session it stopped is still resumed.
func TestRecoveryTrackerSurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "recovery-state.json")
	now := time.Now().UTC()
	NewRecoveryTrackerAt(path).RecordQuotaFailure("claude", "s1", wake.KindClaudeQuota, "pool", now, now.Add(time.Hour))
	states := NewRecoveryTrackerAt(path).List(now)
	if len(states) != 1 || states[0].SessionID != "s1" || !states[0].ResetAt.Equal(now.Add(time.Hour)) {
		t.Fatalf("after restart: %+v", states)
	}
}

// The dashboard's form switches one agent and returns to the dashboard.
func TestAutoResumeDashboardFormSwitchesOneAgent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auto-resume.json")
	handler := Server{AutoResumeSettingPath: path, Recovery: NewRecoveryTracker()}.Handler()
	form := url.Values{"agent": {"claude"}, "enabled": {"true"}}
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/_subrouter/auto-resume", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.RemoteAddr = "127.0.0.1:5000"
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, req)
	if resp.Code != http.StatusSeeOther {
		t.Fatalf("status = %d: %s", resp.Code, resp.Body.String())
	}
	setting, err := ReadAutoResumeSetting(path)
	if err != nil || !setting.Claude || setting.Codex {
		t.Fatalf("setting = %+v, %v", setting, err)
	}
}

// Prompt reports survive a proxy restart, so why still explains what
// happened before an upgrade.
func TestPromptReportsSurviveRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auto-resume-prompts.json")
	first := &promptReports{}
	first.usePath(path)
	first.add(PromptReport{Host: "m5s", Agent: "codex", SurfaceID: "s", Question: "stalled on: at capacity", AnsweredBy: "typed continue"})
	second := &promptReports{}
	second.usePath(path)
	if got := second.list(); len(got) != 1 || got[0].Host != "m5s" {
		t.Fatalf("after restart: %+v", got)
	}
}
