package proxy

import (
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/manaflow-ai/subrouter/internal/transcript"
)

func (s Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	summaries, err := transcript.ListSummaries(s.transcriptDir())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	analytics, err := transcript.Analyze(s.transcriptDir())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	var sessions any
	if s.Sessions != nil {
		sessions = s.Sessions.All()
	}

	data := dashboardData{
		Sessions:    sessions,
		Transcripts: summaries,
		Analytics:   analytics,
		Enabled:     s.Transcripts != nil && s.Transcripts.Enabled(),
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := dashboardTemplate.Execute(w, data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (s Server) handleTranscriptList(w http.ResponseWriter, _ *http.Request) {
	summaries, err := transcript.ListSummaries(s.transcriptDir())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, summaries)
}

func (s Server) handleTranscriptDetail(w http.ResponseWriter, r *http.Request) {
	agentType, sessionID, raw, ok := parseTranscriptPath(r.URL.Path)
	if !ok {
		http.NotFound(w, r)
		return
	}

	if raw {
		events, err := transcript.ReadRawSession(s.transcriptDir(), agentType, sessionID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		writeJSON(w, map[string]any{
			"agent_type": agentType,
			"session_id": sessionID,
			"events":     events,
		})
		return
	}

	events, err := transcript.ReadSanitizedSession(s.transcriptDir(), agentType, sessionID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	writeJSON(w, map[string]any{
		"agent_type": agentType,
		"session_id": sessionID,
		"events":     events,
	})
}

func parseTranscriptPath(path string) (string, string, bool, bool) {
	agentType, rest, ok := strings.Cut(strings.TrimPrefix(path, "/_subrouter/transcripts/"), "/")
	if !ok || agentType == "" || rest == "" {
		return "", "", false, false
	}
	raw := false
	if before, ok := strings.CutSuffix(rest, "/raw"); ok {
		rest = before
		raw = true
	}
	agentType, err := url.PathUnescape(agentType)
	if err != nil {
		return "", "", false, false
	}
	sessionID, err := url.PathUnescape(rest)
	if err != nil || sessionID == "" {
		return "", "", false, false
	}
	return agentType, sessionID, raw, true
}

// transcriptDir returns the transcript directory after flushing buffered
// events, so the dashboard and transcript endpoints read everything recorded
// before the request.
func (s Server) transcriptDir() string {
	if s.Transcripts == nil {
		return ""
	}
	_ = s.Transcripts.Flush()
	return s.Transcripts.Dir()
}

func transcriptURL(agentType, sessionID string) string {
	return "/_subrouter/transcripts/" + url.PathEscape(agentType) + "/" + url.PathEscape(sessionID)
}

func rawTranscriptURL(agentType, sessionID string) string {
	return transcriptURL(agentType, sessionID) + "/raw"
}

type dashboardData struct {
	Sessions    any
	Transcripts []transcript.Summary
	Analytics   transcript.Analytics
	Enabled     bool
}

var dashboardTemplate = template.Must(template.New("dashboard").Funcs(template.FuncMap{
	"json": func(value any) string {
		body, err := json.MarshalIndent(value, "", "  ")
		if err != nil {
			return "{}"
		}
		return string(body)
	},
	"transcriptURL":    transcriptURL,
	"rawTranscriptURL": rawTranscriptURL,
	"bar": func(value, max int64) int {
		if max <= 0 || value <= 0 {
			return 0
		}
		percent := int((value * 100) / max)
		if percent < 1 {
			return 1
		}
		return percent
	},
	"fmtInt": func(value int64) string {
		return strconv.FormatInt(value, 10)
	},
	"tokens": func(value int64) string {
		switch {
		case value >= 1000000:
			return fmt.Sprintf("%.1fM", float64(value)/1000000)
		case value >= 1000:
			return fmt.Sprintf("%.1fk", float64(value)/1000)
		default:
			return strconv.FormatInt(value, 10)
		}
	},
}).Parse(`<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>Subrouter Trajectories</title>
  <style>
    :root { color-scheme: light dark; --border: #74747455; --muted: #777; --fill: #4f7cff; --fill-soft: #4f7cff33; }
    body { font-family: ui-sans-serif, system-ui, -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif; margin: 24px; line-height: 1.35; }
    header { display: flex; justify-content: space-between; gap: 24px; align-items: baseline; margin-bottom: 20px; }
    h1 { margin: 0; font-size: 24px; }
    h2 { margin-top: 28px; font-size: 18px; }
    h3 { margin: 0 0 10px; font-size: 14px; color: var(--muted); }
    .grid { display: grid; grid-template-columns: repeat(4, minmax(0, 1fr)); gap: 12px; }
    .panel { border: 1px solid var(--border); border-radius: 8px; padding: 12px; }
    .metric { font-size: 24px; font-weight: 700; }
    .label { color: var(--muted); font-size: 12px; }
    .charts { display: grid; grid-template-columns: minmax(0, 1.5fr) minmax(320px, 1fr); gap: 16px; align-items: start; }
    .timeline { display: flex; align-items: end; gap: 2px; height: 180px; border-bottom: 1px solid var(--border); padding-top: 12px; }
    .timeline .bar { flex: 1; min-width: 3px; background: var(--fill); border-radius: 3px 3px 0 0; }
    .hbar { display: grid; grid-template-columns: minmax(160px, 1fr) minmax(180px, 2fr) 92px; gap: 10px; align-items: center; margin: 8px 0; font-size: 13px; }
    .track { height: 10px; background: var(--fill-soft); border-radius: 999px; overflow: hidden; }
    .track span { display: block; height: 100%; background: var(--fill); }
    .split { display: grid; grid-template-columns: repeat(2, minmax(0, 1fr)); gap: 16px; }
    table { width: 100%; border-collapse: collapse; font-size: 13px; }
    th, td { border-bottom: 1px solid var(--border); padding: 8px 10px; text-align: left; vertical-align: top; }
    th { font-weight: 650; color: var(--muted); }
    code { font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace; font-size: 12px; }
    a { color: inherit; }
    .muted { color: var(--muted); }
    .pill { display: inline-block; border: 1px solid var(--border); border-radius: 999px; padding: 2px 8px; }
    pre { border: 1px solid var(--border); border-radius: 8px; padding: 12px; overflow: auto; max-height: 520px; }
    @media (max-width: 900px) { .grid, .charts, .split { grid-template-columns: 1fr; } }
  </style>
</head>
<body>
  <header>
    <h1>Subrouter Trajectories</h1>
    <span class="muted">Internal dashboard. Raw trajectory links include decoded bodies.</span>
  </header>
  {{if not .Enabled}}
    <p class="pill">Transcript recording is disabled.</p>
  {{end}}
  <section class="grid">
    <div class="panel">
      <div class="label">Total Tokens</div>
      <div class="metric">{{tokens .Analytics.Totals.TotalTokens}}</div>
    </div>
    <div class="panel">
      <div class="label">Input Tokens</div>
      <div class="metric">{{tokens .Analytics.Totals.InputTokens}}</div>
      <div class="label">{{tokens .Analytics.Totals.CachedInputTokens}} cached</div>
    </div>
    <div class="panel">
      <div class="label">Output Tokens</div>
      <div class="metric">{{tokens .Analytics.Totals.OutputTokens}}</div>
      <div class="label">{{tokens .Analytics.Totals.ReasoningTokens}} reasoning</div>
    </div>
    <div class="panel">
      <div class="label">Model Calls</div>
      <div class="metric">{{fmtInt .Analytics.Totals.Requests}}</div>
    </div>
  </section>
  <h2>Usage</h2>
  <section class="charts">
    <div class="panel">
      <h3>Tokens Over Time</h3>
      <div class="timeline">
        {{range .Analytics.Timeline}}
          <div class="bar" style="height: {{bar .Usage.TotalTokens $.Analytics.MaxTimelineTokens}}%" title="{{.Label}}: {{fmtInt .Usage.TotalTokens}} tokens"></div>
        {{else}}
          <span class="muted">No token usage found in transcripts.</span>
        {{end}}
      </div>
    </div>
    <div class="panel">
      <h3>Models</h3>
      {{range .Analytics.ByModel}}
        <div class="hbar">
          <code>{{.Key}}</code>
          <div class="track"><span style="width: {{bar .Usage.TotalTokens $.Analytics.Totals.TotalTokens}}%"></span></div>
          <span>{{tokens .Usage.TotalTokens}}</span>
        </div>
      {{else}}
        <p class="muted">No model usage found.</p>
      {{end}}
    </div>
  </section>
  <section class="split">
    <div>
      <h2>Users</h2>
      <div class="panel">
        {{range .Analytics.ByUser}}
          <div class="hbar">
            <code>{{.Key}}</code>
            <div class="track"><span style="width: {{bar .Usage.TotalTokens $.Analytics.MaxUserTokens}}%"></span></div>
            <span>{{tokens .Usage.TotalTokens}}</span>
          </div>
        {{else}}
          <p class="muted">No user usage found.</p>
        {{end}}
      </div>
    </div>
    <div>
      <h2>Accounts</h2>
      <div class="panel">
        {{range .Analytics.ByAccount}}
          <div class="hbar">
            <code>{{.Key}}</code>
            <div class="track"><span style="width: {{bar .Usage.TotalTokens $.Analytics.MaxAccountTokens}}%"></span></div>
            <span>{{tokens .Usage.TotalTokens}}</span>
          </div>
        {{else}}
          <p class="muted">No account usage found.</p>
        {{end}}
      </div>
    </div>
  </section>
  <h2>Transcripts</h2>
  <table>
    <thead>
      <tr>
        <th>Session</th>
        <th>Agent</th>
        <th>User</th>
        <th>Account</th>
        <th>Tokens</th>
        <th>Events</th>
        <th>Payload</th>
        <th>Last Event</th>
        <th>Contents</th>
      </tr>
    </thead>
    <tbody>
      {{range .Transcripts}}
        <tr>
          <td><a href="{{transcriptURL .AgentType .SessionID}}"><code>{{.SessionID}}</code></a></td>
          <td>{{.AgentType}}</td>
          <td>{{.User}}</td>
          <td>{{.Account}}</td>
          <td>{{tokens .Usage.TotalTokens}}</td>
          <td>{{.EventCount}}</td>
          <td>{{.TotalBytes}} bytes{{if .HasBodies}} (bodies stored){{end}}</td>
          <td>{{.LastEventAt}}</td>
          <td><a href="{{rawTranscriptURL .AgentType .SessionID}}">raw</a></td>
        </tr>
      {{else}}
        <tr><td colspan="9" class="muted">No transcripts found.</td></tr>
      {{end}}
    </tbody>
  </table>
  <h2>Sessions</h2>
  <pre>{{json .Sessions}}</pre>
</body>
</html>`))
