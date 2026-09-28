package transcript

import (
	"bufio"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type Summary struct {
	AgentType    string `json:"agent_type"`
	SessionID    string `json:"session_id"`
	EventCount   int    `json:"event_count"`
	TotalBytes   int64  `json:"total_bytes"`
	Usage        Usage  `json:"usage"`
	FirstEventAt string `json:"first_event_at,omitempty"`
	LastEventAt  string `json:"last_event_at,omitempty"`
	User         string `json:"user,omitempty"`
	Account      string `json:"account,omitempty"`
	HasBodies    bool   `json:"has_bodies"`
	SizeBytes    int64  `json:"size_bytes"`
}

type SanitizedEvent struct {
	Timestamp string         `json:"timestamp"`
	Type      string         `json:"type"`
	Payload   map[string]any `json:"payload"`
}

func (r *Recorder) Dir() string {
	if r == nil {
		return ""
	}
	return r.dir
}

func ListSummaries(dir string) ([]Summary, error) {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return nil, nil
	}

	root := filepath.Join(dir, "by-agent")
	var summaries []Summary
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			return err
		}
		if entry.IsDir() || filepath.Ext(path) != ".jsonl" {
			return nil
		}

		summary, err := summarizeFile(root, path)
		if err != nil {
			return err
		}
		summaries = append(summaries, summary)
		return nil
	})
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	sort.Slice(summaries, func(i, j int) bool {
		left := summaries[i].LastEventAt
		right := summaries[j].LastEventAt
		if left == right {
			return summaries[i].SessionID < summaries[j].SessionID
		}
		return left > right
	})
	return summaries, nil
}

func ReadSanitizedSession(dir, agentType, sessionID string) ([]SanitizedEvent, error) {
	path := SessionPath(dir, agentType, sessionID)
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	var events []SanitizedEvent
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 32*1024*1024)
	for scanner.Scan() {
		var event Event
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			return nil, err
		}
		events = append(events, SanitizedEvent{
			Timestamp: event.Timestamp,
			Type:      event.Type,
			Payload:   sanitizePayload(event.Payload),
		})
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return events, nil
}

func SessionPath(dir, agentType, sessionID string) string {
	agent := safeFilename(normalizeAgentType(agentType))
	base := BaseSessionID(sessionID)
	return filepath.Join(dir, "by-agent", agent, "by-session", safeFilename(base)+".jsonl")
}

func summarizeFile(root, path string) (Summary, error) {
	info, err := os.Stat(path)
	if err != nil {
		return Summary{}, err
	}

	rel, err := filepath.Rel(root, path)
	if err != nil {
		return Summary{}, err
	}
	parts := strings.Split(rel, string(os.PathSeparator))
	summary := Summary{SizeBytes: info.Size()}
	if len(parts) >= 3 {
		summary.AgentType = parts[0]
	}
	summary.SessionID = strings.TrimSuffix(filepath.Base(path), ".jsonl")

	file, err := os.Open(path)
	if err != nil {
		return Summary{}, err
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 32*1024*1024)
	chunks := bodyChunkAccumulator{}
	for scanner.Scan() {
		var event Event
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			return Summary{}, err
		}
		applyEvent(&summary, event)
		if body, ok := chunks.bodyFromEvent(event); ok {
			for _, record := range extractUsageRecords(body) {
				summary.Usage.add(record.Usage)
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return Summary{}, err
	}
	return summary, nil
}

func applyEvent(summary *Summary, event Event) {
	summary.EventCount++
	if event.Timestamp != "" {
		if summary.FirstEventAt == "" || timestampLess(event.Timestamp, summary.FirstEventAt) {
			summary.FirstEventAt = event.Timestamp
		}
		if summary.LastEventAt == "" || timestampLess(summary.LastEventAt, event.Timestamp) {
			summary.LastEventAt = event.Timestamp
		}
	}

	if agent, ok := stringValue(event.Payload["agent_type"]); ok && summary.AgentType == "" {
		summary.AgentType = agent
	}
	if session, ok := stringValue(event.Payload["agent_session_id"]); ok && summary.SessionID == "" {
		summary.SessionID = session
	}
	if user, ok := eventUser(event.Payload); ok && summary.User == "" {
		summary.User = user
	}
	if account, ok := stringValue(event.Payload["account"]); ok && summary.Account == "" {
		summary.Account = account
	}
	if bytesValue, ok := numericValue(event.Payload["bytes"]); ok {
		summary.TotalBytes += bytesValue
	}
	if _, ok := event.Payload["body_base64"]; ok {
		summary.HasBodies = true
	}
	if boolField(event.Payload, "body_chunked") || boolField(event.Payload, "body_chunk") {
		summary.HasBodies = true
	}
}

func sanitizePayload(payload map[string]any) map[string]any {
	out := make(map[string]any, len(payload)+1)
	for key, value := range payload {
		if key == "body_base64" {
			out["body_base64_redacted"] = true
			continue
		}
		out[key] = value
	}
	return out
}

func timestampLess(left, right string) bool {
	leftTime, leftErr := time.Parse(time.RFC3339Nano, left)
	rightTime, rightErr := time.Parse(time.RFC3339Nano, right)
	if leftErr == nil && rightErr == nil {
		return leftTime.Before(rightTime)
	}
	return left < right
}

func stringValue(value any) (string, bool) {
	text, ok := value.(string)
	return text, ok && text != ""
}

func numericValue(value any) (int64, bool) {
	switch v := value.(type) {
	case float64:
		return int64(v), true
	case int64:
		return v, true
	case int:
		return int64(v), true
	default:
		return 0, false
	}
}

// eventUser is the user a meta event names. Transcripts written before 2026-08-27 carry the email as
// "user"; later ones carry only "user_hash" (the proxy's userEmailHash), shown as "user:<hash>" so
// analytics can still group by person without the address.
func eventUser(payload map[string]any) (string, bool) {
	if user, ok := stringValue(payload["user"]); ok {
		return user, true
	}
	if hash, ok := stringValue(payload["user_hash"]); ok {
		return "user:" + hash, true
	}
	return "", false
}
