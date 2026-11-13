package proxy

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"sync"
	"time"
)

// Codex capacity event log.
//
// Every "Selected model is at capacity" (and every other pre-output pool
// failure) the router sees is one event: each failed physical attempt, with
// the region and transport it went out on, and, once per request or
// websocket turn that hit one, whether the client still got served
// (absorbed) or got the failure (client_visible). Events append to a JSONL
// file next to the session store, rotated by size, and feed the per-minute
// summary in /_subrouter/codex-capacity.

const (
	codexCapacityOutcomeAttempt       = "attempt_failed"
	codexCapacityOutcomeAbsorbed      = "absorbed"
	codexCapacityOutcomeClientVisible = "client_visible"
	// codexCapacityOutcomeFailedOther is a request that hit capacity and then
	// ended in some other error (quota, auth, transport).
	codexCapacityOutcomeFailedOther = "failed_other"

	codexCapacityEventsMaxBytes = 32 << 20
	codexCapacityEventsKeep     = 4
	codexCapacityEventsMinutes  = 24 * 60
	codexCapacityEventsRecent   = 200
)

// CodexCapacityEvent is one line of codex-capacity-events.jsonl.
type CodexCapacityEvent struct {
	Time               time.Time `json:"time"`
	Transport          string    `json:"transport"`
	Region             string    `json:"region,omitempty"`
	Account            string    `json:"account,omitempty"`
	Model              string    `json:"model,omitempty"`
	Tier               string    `json:"tier,omitempty"`
	Reason             string    `json:"reason,omitempty"`
	Agent              string    `json:"agent,omitempty"`
	Session            string    `json:"session,omitempty"`
	Outcome            string    `json:"outcome"`
	ClientRetryAdvised bool      `json:"client_retry_advised,omitempty"`
	// Attempts is the request's capacity failures, on absorbed and
	// client_visible events.
	Attempts int `json:"attempts,omitempty"`
}

type codexCapacityMinute struct {
	minute int64
	counts map[string]int // outcome|region
}

type codexCapacityEventLog struct {
	mu       sync.Mutex
	path     string
	file     *os.File
	size     int64
	now      func() time.Time
	minutes  []codexCapacityMinute // newest last
	recent   []CodexCapacityEvent  // client_visible only, newest last
	writeErr string
}

func newCodexCapacityEventLog(path string) *codexCapacityEventLog {
	return &codexCapacityEventLog{path: path, now: time.Now}
}

func (s *Server) recordCodexCapacityEvent(event CodexCapacityEvent) {
	if s == nil || s.codexCapacityEvents == nil {
		return
	}
	s.codexCapacityEvents.record(event)
}

func (l *codexCapacityEventLog) record(event CodexCapacityEvent) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if event.Time.IsZero() {
		event.Time = l.now().UTC()
	}
	minute := event.Time.Unix() / 60
	if n := len(l.minutes); n == 0 || l.minutes[n-1].minute != minute {
		l.minutes = append(l.minutes, codexCapacityMinute{minute: minute, counts: map[string]int{}})
		if len(l.minutes) > codexCapacityEventsMinutes {
			l.minutes = l.minutes[len(l.minutes)-codexCapacityEventsMinutes:]
		}
	}
	l.minutes[len(l.minutes)-1].counts[event.Outcome+"|"+event.Region]++
	if event.Outcome == codexCapacityOutcomeClientVisible {
		l.recent = append(l.recent, event)
		if len(l.recent) > codexCapacityEventsRecent {
			l.recent = l.recent[len(l.recent)-codexCapacityEventsRecent:]
		}
	}
	l.appendLocked(event)
}

func (l *codexCapacityEventLog) appendLocked(event CodexCapacityEvent) {
	if l.path == "" {
		return
	}
	line, err := json.Marshal(event)
	if err != nil {
		return
	}
	line = append(line, '\n')
	if l.file == nil {
		file, err := os.OpenFile(l.path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
		if err != nil {
			l.writeErr = err.Error()
			return
		}
		info, err := file.Stat()
		if err == nil {
			l.size = info.Size()
		}
		l.file = file
	}
	if l.size+int64(len(line)) > codexCapacityEventsMaxBytes {
		l.rotateLocked()
		if l.file == nil {
			return
		}
	}
	n, err := l.file.Write(line)
	l.size += int64(n)
	if err != nil {
		l.writeErr = err.Error()
	}
}

func (l *codexCapacityEventLog) rotateLocked() {
	_ = l.file.Close()
	l.file = nil
	for index := codexCapacityEventsKeep - 1; index >= 1; index-- {
		_ = os.Rename(fmt.Sprintf("%s.%d", l.path, index), fmt.Sprintf("%s.%d", l.path, index+1))
	}
	_ = os.Rename(l.path, l.path+".1")
	_ = os.Remove(fmt.Sprintf("%s.%d", l.path, codexCapacityEventsKeep+1))
	file, err := os.OpenFile(l.path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		l.writeErr = err.Error()
		return
	}
	l.file, l.size = file, 0
}

// CodexCapacityEventCount is one (outcome, region) total for a window.
type CodexCapacityEventCount struct {
	Outcome string `json:"outcome"`
	Region  string `json:"region,omitempty"`
	Last15m int    `json:"last_15m"`
	Last1h  int    `json:"last_1h"`
	Last24h int    `json:"last_24h"`
}

// CodexCapacityEventsReport is the events section of /_subrouter/codex-capacity.
type CodexCapacityEventsReport struct {
	Path          string                    `json:"path,omitempty"`
	WriteError    string                    `json:"write_error,omitempty"`
	Counts        []CodexCapacityEventCount `json:"counts"`
	ClientVisible []CodexCapacityEvent      `json:"recent_client_visible"`
}

func (l *codexCapacityEventLog) snapshot() *CodexCapacityEventsReport {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now().Unix() / 60
	totals := map[string]*CodexCapacityEventCount{}
	for _, bucket := range l.minutes {
		age := now - bucket.minute
		for key, n := range bucket.counts {
			row := totals[key]
			if row == nil {
				outcome, region := key, ""
				for i := range len(key) {
					if key[i] == '|' {
						outcome, region = key[:i], key[i+1:]
						break
					}
				}
				row = &CodexCapacityEventCount{Outcome: outcome, Region: region}
				totals[key] = row
			}
			if age < 15 {
				row.Last15m += n
			}
			if age < 60 {
				row.Last1h += n
			}
			if age < codexCapacityEventsMinutes {
				row.Last24h += n
			}
		}
	}
	report := &CodexCapacityEventsReport{Path: l.path, WriteError: l.writeErr, Counts: []CodexCapacityEventCount{}}
	for _, row := range totals {
		report.Counts = append(report.Counts, *row)
	}
	sort.Slice(report.Counts, func(i, j int) bool {
		if report.Counts[i].Outcome != report.Counts[j].Outcome {
			return report.Counts[i].Outcome < report.Counts[j].Outcome
		}
		return report.Counts[i].Region < report.Counts[j].Region
	})
	report.ClientVisible = append([]CodexCapacityEvent{}, l.recent...)
	return report
}
