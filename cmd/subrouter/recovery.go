package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type recoveryTask struct {
	ID      string `json:"id"`
	Path    string `json:"path"`
	Status  string `json:"status,omitempty"`
	Summary string `json:"summary,omitempty"`
}
type recoverySession struct {
	ID             string         `json:"id"`
	Agent          string         `json:"agent"`
	CWD            string         `json:"cwd,omitempty"`
	TranscriptPath string         `json:"transcript_path"`
	UpdatedAt      time.Time      `json:"updated_at"`
	Lifecycle      string         `json:"lifecycle"`
	LastMessage    string         `json:"last_message,omitempty"`
	Tasks          []recoveryTask `json:"tasks,omitempty"`
}

func runRecoveryCommand(args []string, out io.Writer) error {
	if len(args) == 0 {
		args = []string{"list"}
	}
	if args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		_, _ = io.WriteString(out, "Usage: sr recover list|show|prompt [--session ID] [--task ID] [--query TEXT] [--limit N] [--json]\n")
		return nil
	}
	jsonOut, query, sessionID, taskID, limit := false, "", "", "", 25
	fs := flag.NewFlagSet("sr recover", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.BoolVar(&jsonOut, "json", false, "JSON")
	fs.StringVar(&query, "query", "", "query")
	fs.StringVar(&sessionID, "session", "", "session")
	fs.StringVar(&taskID, "task", "", "task")
	fs.IntVar(&limit, "limit", 25, "limit")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected recovery argument %q", fs.Arg(0))
	}
	if limit < 1 || limit > 500 {
		return errors.New("recover limit must be 1..500")
	}
	if args[0] != "list" && args[0] != "show" && args[0] != "prompt" {
		return fmt.Errorf("unknown recover command %q", args[0])
	}
	if args[0] != "list" && sessionID == "" {
		return errors.New("recover show/prompt requires --session ID")
	}
	rows, err := discoverRecoverySessions(limit, sessionID, query)
	if err != nil {
		return err
	}
	if args[0] == "list" {
		return writeRecoverySessions(out, rows, jsonOut)
	}
	if len(rows) == 0 {
		return fmt.Errorf("recovery session %q not found", sessionID)
	}
	if args[0] == "show" {
		return writeRecoverySessions(out, rows[:1], jsonOut)
	}
	_, _ = io.WriteString(out, recoveryPrompt(rows[0], taskID))
	return nil
}

func recoveryRoots() []string {
	root := os.Getenv("SUBROUTER_CLAUDE_SESSION_ROOT")
	if root == "" {
		if h, e := os.UserHomeDir(); e == nil {
			root = filepath.Join(h, ".subrouter", "codex", "claude-proxy")
		}
	}
	roots := []string{root}
	if r, e := filepath.EvalSymlinks(root); e == nil && r != root {
		roots = append(roots, r)
	}
	if os.Getenv("SUBROUTER_CLAUDE_SESSION_ROOT") == "" {
		if h, e := os.UserHomeDir(); e == nil {
			if r, e := filepath.EvalSymlinks(filepath.Join(h, ".claude", "projects")); e == nil {
				roots = append(roots, r)
			}
		}
	}
	return roots
}
func recoveryTaskRoots() []string {
	v := os.Getenv("SUBROUTER_RECOVERY_TASK_ROOTS")
	if v != "" {
		return strings.Split(v, string(os.PathListSeparator))
	}
	r := []string{os.TempDir()}
	if _, e := os.Stat("/private/tmp"); e == nil && filepath.Clean(os.TempDir()) != "/private/tmp" {
		r = append(r, "/private/tmp")
	}
	return r
}
func discoverRecoverySessions(limit int, exact, query string) ([]recoverySession, error) {
	paths := []string{}
	for _, root := range recoveryRoots() {
		for _, pat := range []string{filepath.Join(root, "*.jsonl"), filepath.Join(root, "*", "*.jsonl"), filepath.Join(root, "*", "*", "*.jsonl")} {
			m, e := filepath.Glob(pat)
			if e != nil {
				return nil, e
			}
			paths = append(paths, m...)
		}
	}
	sort.Slice(paths, func(i, j int) bool {
		a, _ := os.Stat(paths[i])
		b, _ := os.Stat(paths[j])
		return a != nil && b != nil && a.ModTime().After(b.ModTime())
	})
	if exact == "" && len(paths) > limit*8 {
		paths = paths[:limit*8]
	}
	seen := map[string]bool{}
	rows := []recoverySession{}
	for _, p := range paths {
		if exact != "" && strings.TrimSuffix(filepath.Base(p), ".jsonl") != exact {
			continue
		}
		s, ok := readRecoveryTranscript(p)
		if !ok || seen[s.ID] {
			continue
		}
		seen[s.ID] = true
		s.Tasks = findRecoveryTasks(s.ID)
		if query != "" && !strings.Contains(strings.ToLower(s.ID+" "+s.CWD+" "+s.LastMessage+" "+s.TranscriptPath+" "+taskText(s.Tasks)), strings.ToLower(query)) {
			continue
		}
		rows = append(rows, s)
		if len(rows) >= limit {
			break
		}
	}
	return rows, nil
}
func taskText(ts []recoveryTask) string {
	var b strings.Builder
	for _, t := range ts {
		b.WriteString(" " + t.ID + " " + t.Summary)
	}
	return b.String()
}
func readRecoveryTranscript(path string) (recoverySession, bool) {
	s := recoverySession{ID: strings.TrimSuffix(filepath.Base(path), ".jsonl"), Agent: "claude", TranscriptPath: path, Lifecycle: "unknown"}
	f, e := os.Open(path)
	if e != nil {
		return s, false
	}
	defer f.Close()
	b, e := recoveryTail(f, 256<<10)
	if e != nil {
		return s, false
	}
	if i, e := f.Stat(); e == nil {
		s.UpdatedAt = i.ModTime()
	}
	for _, line := range strings.Split(string(b), "\n") {
		var r map[string]any
		if json.Unmarshal([]byte(line), &r) != nil {
			continue
		}
		if v, ok := r["cwd"].(string); ok {
			s.CWD = v
		}
		if v, ok := r["timestamp"].(string); ok {
			if t, e := time.Parse(time.RFC3339Nano, v); e == nil && t.After(s.UpdatedAt) {
				s.UpdatedAt = t
			}
		}
		if v := recoveryText(r["message"]); v != "" {
			s.LastMessage = v
		}
		if strings.Contains(strings.ToLower(line), "api error") {
			s.Lifecycle = "errored"
		}
	}
	if s.LastMessage == "" {
		s.LastMessage = "transcript available"
	}
	return s, true
}
func recoveryText(v any) string {
	m, ok := v.(map[string]any)
	if !ok {
		return ""
	}
	if s, ok := m["content"].(string); ok {
		return trimRecoveryText(s)
	}
	if a, ok := m["content"].([]any); ok {
		for i := len(a) - 1; i >= 0; i-- {
			if p, ok := a[i].(map[string]any); ok {
				if s, ok := p["text"].(string); ok {
					return trimRecoveryText(s)
				}
			}
		}
	}
	return ""
}
func trimRecoveryText(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) > 280 {
		return string(r[:280]) + "..."
	}
	return s
}
func findRecoveryTasks(id string) []recoveryTask {
	out := []recoveryTask{}
	for _, root := range recoveryTaskRoots() {
		for _, pat := range []string{filepath.Join(root, "tasks", "*.output"), filepath.Join(root, "*", "tasks", "*.output"), filepath.Join(root, "*", "*", "tasks", "*.output"), filepath.Join(root, "*", "*", id, "tasks", "*.output"), filepath.Join(root, "claude-*", "*", id, "tasks", "*.output")} {
			ms, _ := filepath.Glob(pat)
			for _, p := range ms {
				b, e := os.ReadFile(p)
				if e != nil {
					continue
				}
				status := "unknown"
				for _, l := range strings.Split(string(b), "\n") {
					if strings.HasPrefix(strings.TrimSpace(l), "result:") {
						status = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(l), "result:"))
					}
				}
				out = append(out, recoveryTask{ID: strings.TrimSuffix(filepath.Base(p), ".output"), Path: p, Status: status, Summary: recoveryOutputSummary(b)})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}
func recoveryOutputSummary(b []byte) string {
	best := ""
	for _, l := range strings.Split(string(b), "\n") {
		var r map[string]any
		if json.Unmarshal([]byte(l), &r) == nil {
			if s := recoveryText(r["message"]); len(s) > len(best) {
				best = s
			}
		} else if strings.TrimSpace(l) != "" && len(l) > len(best) {
			best = trimRecoveryText(l)
		}
	}
	if best == "" {
		best = trimRecoveryText(string(b))
	}
	return best
}
func writeRecoverySessions(w io.Writer, rows []recoverySession, jsonOut bool) error {
	if jsonOut {
		e := json.NewEncoder(w)
		e.SetIndent("", "  ")
		return e.Encode(rows)
	}
	for _, s := range rows {
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%d tasks\n", s.ID, s.Lifecycle, s.CWD, s.LastMessage, len(s.Tasks))
	}
	if len(rows) == 0 {
		_, _ = io.WriteString(w, "No recoverable Claude sessions found.\n")
	}
	return nil
}
func recoveryPrompt(s recoverySession, taskID string) string {
	b := &strings.Builder{}
	fmt.Fprintf(b, "Recover the interrupted Claude session %s through sr codex.\nWorking directory: %s\nTranscript: %s\nLast observed context: %s\n", s.ID, s.CWD, s.TranscriptPath, s.LastMessage)
	for _, t := range s.Tasks {
		if taskID == "" || taskID == t.ID {
			fmt.Fprintf(b, "Task %s (%s): %s\nOutput: %s\n", t.ID, t.Status, t.Summary, t.Path)
		}
	}
	b.WriteString("Continue the existing work, preserve user intent, test changes, and leave a resumable state if blocked.\n")
	r := b.String()
	if len(r) > 6000 {
		r = r[:6000] + "\n[recovery context truncated]\n"
	}
	return r
}
func recoveryTail(f *os.File, max int64) ([]byte, error) {
	i, e := f.Stat()
	if e != nil {
		return nil, e
	}
	start := i.Size() - max
	if start < 0 {
		start = 0
	}
	b := make([]byte, int(i.Size()-start))
	n, e := f.ReadAt(b, start)
	if e != nil && e != io.EOF {
		return nil, e
	}
	b = b[:n]
	if start > 0 {
		if i := bytes.IndexByte(b, '\n'); i >= 0 {
			b = b[i+1:]
		}
	}
	return b, nil
}
