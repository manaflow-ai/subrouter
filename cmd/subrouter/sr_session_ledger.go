package main

import (
	"bufio"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/manaflow-ai/subrouter/internal/fsutil"
)

// The session ledger is the client's own record of which pooled account served
// each agent session. The server keeps only the current sticky assignment (and
// forgets it on restart or eviction), so a crashed or resumed session could
// not be attributed to an account before this existed. Everything here is
// local metadata: account IDs, labels, timestamps and token totals. It never
// holds credentials, prompts, or request bodies.
//
// Layout under <store dir>/session-accounts:
//
//	launches/<launch id>.json      one per pooled launch (sr claude, sr codex)
//	sessions/<agent>/<id>.json     one per agent session, with account spans
//	events.jsonl                   append-only account switch log
const sessionLedgerDirName = "session-accounts"

// sessionLedgerSwitchNoticeWindow is how long the status line keeps showing
// the previous account after a switch.
const sessionLedgerSwitchNoticeWindow = 10 * time.Minute

type sessionLedger struct {
	dir string
	now func() time.Time
}

func newSessionLedger(storeDir string) sessionLedger {
	return sessionLedger{dir: filepath.Join(storeDir, sessionLedgerDirName), now: time.Now}
}

type sessionLaunchRecord struct {
	ID               string     `json:"id"`
	Agent            string     `json:"agent"`
	PID              int        `json:"pid"`
	Server           string     `json:"server,omitempty"`
	LocalBaseURL     string     `json:"local_base_url,omitempty"`
	Pinned           bool       `json:"pinned,omitempty"`
	AccountID        string     `json:"account_id,omitempty"`
	PreferredAccount string     `json:"preferred_account_id,omitempty"`
	ResumeSession    string     `json:"resume_session_id,omitempty"`
	WorkingDir       string     `json:"cwd,omitempty"`
	StartedAt        time.Time  `json:"started_at"`
	EndedAt          *time.Time `json:"ended_at,omitempty"`
	ExitError        string     `json:"exit_error,omitempty"`
	Sessions         []string   `json:"sessions,omitempty"`
}

type sessionAccountSpan struct {
	AccountID    string    `json:"account_id"`
	Label        string    `json:"label,omitempty"`
	From         time.Time `json:"from"`
	To           time.Time `json:"to"`
	InputTokens  int64     `json:"input_tokens,omitempty"`
	OutputTokens int64     `json:"output_tokens,omitempty"`
	CostUSD      float64   `json:"cost_usd,omitempty"`
}

// sessionUsageCounters are the cumulative per-process counters an agent
// reports (Claude's status line input). Deltas between observations are
// attributed to the account serving at the time.
type sessionUsageCounters struct {
	InputTokens  int64   `json:"input_tokens,omitempty"`
	OutputTokens int64   `json:"output_tokens,omitempty"`
	CostUSD      float64 `json:"cost_usd,omitempty"`
}

type sessionRecord struct {
	Agent     string               `json:"agent"`
	SessionID string               `json:"session_id"`
	LaunchID  string               `json:"launch_id,omitempty"`
	Server    string               `json:"server,omitempty"`
	FirstSeen time.Time            `json:"first_seen"`
	LastSeen  time.Time            `json:"last_seen"`
	Accounts  []sessionAccountSpan `json:"accounts,omitempty"`
	// LastCounters is the most recent cumulative report, used to compute the
	// next delta. A smaller value than last time means the agent process
	// restarted (for example a resume), so the new value is itself the delta.
	LastCounters sessionUsageCounters `json:"last_counters,omitempty"`
	// LastLaunchCounters remembers which launch reported LastCounters, so a
	// new process that has not yet caught up is still treated as a restart.
	LastCountersLaunch string `json:"last_counters_launch,omitempty"`
	// Unattributed holds usage reported before any account was seen (for
	// example while the server was unreachable); the first span absorbs it.
	Unattributed sessionUsageCounters `json:"unattributed,omitempty"`
}

type sessionSwitchEvent struct {
	Time      time.Time `json:"time"`
	Agent     string    `json:"agent"`
	SessionID string    `json:"session_id"`
	LaunchID  string    `json:"launch_id,omitempty"`
	From      string    `json:"from,omitempty"`
	FromLabel string    `json:"from_label,omitempty"`
	To        string    `json:"to"`
	ToLabel   string    `json:"to_label,omitempty"`
}

// sessionObservation is one sighting of the account serving a session.
type sessionObservation struct {
	Agent     string
	SessionID string
	LaunchID  string
	Server    string
	AccountID string
	Label     string
	// Counters is nil when the reporter has no usage numbers (Codex notify).
	Counters *sessionUsageCounters
}

func (s sessionLedger) clock() time.Time {
	if s.now != nil {
		return s.now().UTC()
	}
	return time.Now().UTC()
}

func newSessionLaunchID() string {
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return fmt.Sprintf("%x", time.Now().UnixNano())
	}
	return hex.EncodeToString(buf[:])
}

func validSessionLaunchID(id string) bool {
	if len(id) == 0 || len(id) > 64 {
		return false
	}
	for _, char := range id {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return false
		}
	}
	return true
}

// sessionFileName keeps readable IDs (Claude and Codex use UUIDs) and hashes
// anything that could escape the directory.
func sessionFileName(sessionID string) string {
	safe := len(sessionID) > 0 && len(sessionID) <= 128
	for _, char := range sessionID {
		if !(char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' || char == '-' || char == '_' || char == '.') {
			safe = false
			break
		}
	}
	if safe && sessionID != "." && sessionID != ".." {
		return sessionID + ".json"
	}
	sum := sha256.Sum256([]byte(sessionID))
	return "h-" + hex.EncodeToString(sum[:16]) + ".json"
}

func sanitizeLedgerAgent(agent string) string {
	agent = strings.ToLower(strings.TrimSpace(agent))
	switch agent {
	case "claude", "codex":
		return agent
	default:
		return "other"
	}
}

func (s sessionLedger) launchPath(id string) string {
	return filepath.Join(s.dir, "launches", id+".json")
}

func (s sessionLedger) sessionPath(agent, sessionID string) string {
	return filepath.Join(s.dir, "sessions", sanitizeLedgerAgent(agent), sessionFileName(sessionID))
}

func (s sessionLedger) writeJSON(path string, value any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	body, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return fsutil.WriteFileAtomic(path, append(body, '\n'), 0o600)
}

func readLedgerJSON(path string, value any) (bool, error) {
	body, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err := json.Unmarshal(body, value); err != nil {
		return false, fmt.Errorf("parse %s: %w", path, err)
	}
	return true, nil
}

func (s sessionLedger) startLaunch(record sessionLaunchRecord) (sessionLaunchRecord, error) {
	if record.ID == "" {
		record.ID = newSessionLaunchID()
	}
	if record.PID == 0 {
		record.PID = os.Getpid()
	}
	if record.StartedAt.IsZero() {
		record.StartedAt = s.clock()
	}
	if record.WorkingDir == "" {
		record.WorkingDir, _ = os.Getwd()
	}
	if record.LocalBaseURL == "" {
		// Hooks run under the agent, whose environment has SUBROUTER_*
		// stripped; the record carries the daemon override for them.
		record.LocalBaseURL = strings.TrimSpace(os.Getenv("SUBROUTER_LOCAL_BASE_URL"))
	}
	if err := s.writeJSON(s.launchPath(record.ID), record); err != nil {
		return record, err
	}
	slog.Info("pooled launch started", "agent", record.Agent, "launch", record.ID,
		"server", record.Server, "pid", record.PID, "pinned_account", record.AccountID,
		"preferred_account", record.PreferredAccount, "resume_session", record.ResumeSession)
	return record, nil
}

func (s sessionLedger) loadLaunch(id string) (sessionLaunchRecord, bool, error) {
	var record sessionLaunchRecord
	if !validSessionLaunchID(id) {
		return record, false, nil
	}
	ok, err := readLedgerJSON(s.launchPath(id), &record)
	return record, ok, err
}

func (s sessionLedger) finishLaunch(id string, runErr error) (sessionLaunchRecord, error) {
	if !validSessionLaunchID(id) {
		return sessionLaunchRecord{}, nil
	}
	defer s.lock(s.launchPath(id))()
	record, ok, err := s.loadLaunch(id)
	if err != nil || !ok {
		return record, err
	}
	ended := s.clock()
	record.EndedAt = &ended
	if runErr != nil {
		record.ExitError = runErr.Error()
	}
	return record, s.writeJSON(s.launchPath(id), record)
}

func (s sessionLedger) loadSession(agent, sessionID string) (sessionRecord, bool, error) {
	var record sessionRecord
	ok, err := readLedgerJSON(s.sessionPath(agent, sessionID), &record)
	if ok && record.SessionID != sessionID {
		// A hashed file name collision or a hand-edited file; never attribute
		// another session's accounts.
		return sessionRecord{}, false, nil
	}
	return record, ok, err
}

// observe records one sighting and reports the switch it represents, if any.
func (s sessionLedger) observe(observation sessionObservation) (sessionRecord, *sessionSwitchEvent, error) {
	sessionID := strings.TrimSpace(observation.SessionID)
	if sessionID == "" {
		return sessionRecord{}, nil, fmt.Errorf("session id is required")
	}
	agent := sanitizeLedgerAgent(observation.Agent)
	defer s.lock(s.sessionPath(agent, sessionID))()
	now := s.clock()
	record, ok, err := s.loadSession(agent, sessionID)
	if err != nil {
		// A corrupt record must not take the status line down; start over.
		ok = false
	}
	if !ok {
		record = sessionRecord{Agent: agent, SessionID: sessionID, FirstSeen: now}
	}
	record.LastSeen = now
	if observation.LaunchID != "" {
		record.LaunchID = observation.LaunchID
	}
	if observation.Server != "" {
		record.Server = observation.Server
	}

	var delta sessionUsageCounters
	if observation.Counters != nil {
		current := *observation.Counters
		previous := record.LastCounters
		restarted := record.LastCountersLaunch != observation.LaunchID ||
			current.InputTokens < previous.InputTokens ||
			current.OutputTokens < previous.OutputTokens ||
			current.CostUSD < previous.CostUSD
		if restarted {
			delta = current
		} else {
			delta = sessionUsageCounters{
				InputTokens:  current.InputTokens - previous.InputTokens,
				OutputTokens: current.OutputTokens - previous.OutputTokens,
				CostUSD:      current.CostUSD - previous.CostUSD,
			}
		}
		record.LastCounters = current
		record.LastCountersLaunch = observation.LaunchID
	}

	var event *sessionSwitchEvent
	accountID := strings.TrimSpace(observation.AccountID)
	if accountID != "" {
		last := len(record.Accounts) - 1
		if last >= 0 && record.Accounts[last].AccountID == accountID {
			record.Accounts[last].To = now
			if observation.Label != "" {
				record.Accounts[last].Label = observation.Label
			}
		} else {
			if last >= 0 {
				event = &sessionSwitchEvent{
					Time:      now,
					Agent:     agent,
					SessionID: sessionID,
					LaunchID:  observation.LaunchID,
					From:      record.Accounts[last].AccountID,
					FromLabel: record.Accounts[last].Label,
					To:        accountID,
					ToLabel:   observation.Label,
				}
			}
			span := sessionAccountSpan{
				AccountID: accountID,
				Label:     observation.Label,
				From:      now,
				To:        now,
			}
			if last < 0 {
				// Usage reported before the first account sighting belongs
				// to the account that then turns out to serve the session.
				span.InputTokens = record.Unattributed.InputTokens
				span.OutputTokens = record.Unattributed.OutputTokens
				span.CostUSD = record.Unattributed.CostUSD
				record.Unattributed = sessionUsageCounters{}
			}
			record.Accounts = append(record.Accounts, span)
		}
	}
	if last := len(record.Accounts) - 1; last >= 0 {
		record.Accounts[last].InputTokens += delta.InputTokens
		record.Accounts[last].OutputTokens += delta.OutputTokens
		record.Accounts[last].CostUSD += delta.CostUSD
	} else {
		record.Unattributed.InputTokens += delta.InputTokens
		record.Unattributed.OutputTokens += delta.OutputTokens
		record.Unattributed.CostUSD += delta.CostUSD
	}
	if err := s.writeJSON(s.sessionPath(agent, sessionID), record); err != nil {
		return record, event, err
	}
	if event != nil {
		_ = s.appendEvent(*event)
	}
	if observation.LaunchID != "" {
		_ = s.linkLaunchSession(observation.LaunchID, sessionID)
	}
	return record, event, nil
}

func (s sessionLedger) appendEvent(event sessionSwitchEvent) error {
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return err
	}
	file, err := os.OpenFile(filepath.Join(s.dir, "events.jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer file.Close()
	body, err := json.Marshal(event)
	if err != nil {
		return err
	}
	_, err = file.Write(append(body, '\n'))
	return err
}

// recentEvents returns switch events for one session, oldest first.
func (s sessionLedger) sessionEvents(agent, sessionID string) []sessionSwitchEvent {
	file, err := os.Open(filepath.Join(s.dir, "events.jsonl"))
	if err != nil {
		return nil
	}
	defer file.Close()
	agent = sanitizeLedgerAgent(agent)
	var out []sessionSwitchEvent
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 4096), 1<<20)
	for scanner.Scan() {
		var event sessionSwitchEvent
		if json.Unmarshal(scanner.Bytes(), &event) != nil {
			continue
		}
		if event.Agent == agent && event.SessionID == sessionID {
			out = append(out, event)
		}
	}
	return out
}

func (s sessionLedger) linkLaunchSession(launchID, sessionID string) error {
	if !validSessionLaunchID(launchID) {
		return nil
	}
	defer s.lock(s.launchPath(launchID))()
	record, ok, err := s.loadLaunch(launchID)
	if err != nil || !ok {
		return err
	}
	for _, existing := range record.Sessions {
		if existing == sessionID {
			return nil
		}
	}
	record.Sessions = append(record.Sessions, sessionID)
	return s.writeJSON(s.launchPath(launchID), record)
}

// listSessions returns every session record, most recently seen first.
func (s sessionLedger) listSessions() ([]sessionRecord, error) {
	var out []sessionRecord
	root := filepath.Join(s.dir, "sessions")
	agents, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	for _, agent := range agents {
		if !agent.IsDir() {
			continue
		}
		entries, err := os.ReadDir(filepath.Join(root, agent.Name()))
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
				continue
			}
			var record sessionRecord
			if ok, err := readLedgerJSON(filepath.Join(root, agent.Name(), entry.Name()), &record); err != nil || !ok || record.SessionID == "" {
				continue
			}
			out = append(out, record)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].LastSeen.After(out[j].LastSeen) })
	return out, nil
}

// sessionLedgerLockWait bounds how long a writer waits for another process's
// read-modify-write of the same record. Status lines render concurrently.
const sessionLedgerLockWait = time.Second

// sessionLedgerStaleLock is when a lock left by a killed process is broken.
const sessionLedgerStaleLock = 10 * time.Second

// lock serializes read-modify-write of one ledger file across processes. It
// never blocks a caller for long: after sessionLedgerLockWait it proceeds
// unlocked, since a lost update is better than a hung status line.
func (s sessionLedger) lock(path string) func() {
	lockPath := path + ".lock"
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o700); err != nil {
		return func() {}
	}
	deadline := time.Now().Add(sessionLedgerLockWait)
	for {
		file, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			_ = file.Close()
			return func() { _ = os.Remove(lockPath) }
		}
		if info, statErr := os.Stat(lockPath); statErr == nil && time.Since(info.ModTime()) > sessionLedgerStaleLock {
			_ = os.Remove(lockPath)
			continue
		}
		if time.Now().After(deadline) {
			return func() {}
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// lastAccount is the account that most recently served the session.
func (r sessionRecord) lastAccount() (sessionAccountSpan, bool) {
	if len(r.Accounts) == 0 {
		return sessionAccountSpan{}, false
	}
	return r.Accounts[len(r.Accounts)-1], true
}

func (r sessionRecord) totals() sessionUsageCounters {
	total := r.Unattributed
	for _, span := range r.Accounts {
		total.InputTokens += span.InputTokens
		total.OutputTokens += span.OutputTokens
		total.CostUSD += span.CostUSD
	}
	return total
}

// launchRunning reports whether the launcher that owns a record is still
// alive. A record without an end time whose process is gone crashed.
func (l sessionLaunchRecord) running() bool {
	return l.EndedAt == nil && l.PID > 0 && pidAlive(l.PID)
}
