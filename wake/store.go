// Package wake stores durable, cancellable agent wake alarms.
package wake

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Status string

const (
	StatusScheduled Status = "scheduled"
	StatusFired     Status = "fired"
	StatusCompleted Status = "completed"
	StatusStale     Status = "stale"
	StatusCancelled Status = "cancelled"
	StatusExpired   Status = "expired"
	StatusFailed    Status = "failed"
)

const (
	KindCodexProvider = "codex-provider"
	KindCodexQuota    = "codex-quota"
	KindClaudeQuota   = "claude-quota"
)

type Alarm struct {
	ID                  string    `json:"id"`
	Kind                string    `json:"kind"`
	Agent               string    `json:"agent"`
	SessionID           string    `json:"session_id"`
	SurfaceID           string    `json:"surface_id"`
	Machine             string    `json:"machine,omitempty"`
	Pool                string    `json:"pool,omitempty"`
	Action              string    `json:"action"`
	WakeAt              time.Time `json:"wake_at"`
	ExpiresAt           time.Time `json:"expires_at"`
	CreatedAt           time.Time `json:"created_at"`
	UpdatedAt           time.Time `json:"updated_at"`
	Status              Status    `json:"status"`
	LastError           string    `json:"last_error,omitempty"`
	Attempt             int       `json:"attempt,omitempty"`
	JitterSeconds       int       `json:"jitter_seconds,omitempty"`
	SessionLastActiveAt time.Time `json:"session_last_active_at,omitempty"`
	ObservedAt          time.Time `json:"observed_at,omitempty"`
	Automatic           bool      `json:"automatic,omitempty"`
	AcceleratedAt       time.Time `json:"accelerated_at,omitempty"`
}

const InitialSessionMaxAge = 8 * time.Hour

// EligibleForAutomatic rejects stale sessions during the initial scan. Later
// monitor passes require a fresh confirmed provider/quota event instead of
// rediscovering an old tab from a broad screen scrape.
func EligibleForAutomatic(lastActivity, now time.Time, initialScan, confirmedEvent bool) bool {
	if lastActivity.IsZero() || now.Before(lastActivity) {
		return false
	}
	if initialScan {
		return now.Sub(lastActivity) <= InitialSessionMaxAge
	}
	return confirmedEvent
}

type fileState struct {
	Version int              `json:"version"`
	Alarms  map[string]Alarm `json:"alarms"`
}

type Store struct {
	path string
	mu   sync.Mutex
}

func NewStore(path string) *Store { return &Store{path: path} }

func DefaultPath(home string) string {
	return filepath.Join(home, ".subrouter", "wake.json")
}

func (s *Store) List(now time.Time) ([]Alarm, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	unlock, err := s.lockFile()
	if err != nil {
		return nil, err
	}
	defer unlock()
	state, err := s.load()
	if err != nil {
		return nil, err
	}
	changed := false
	for id, alarm := range state.Alarms {
		if alarm.Status == StatusScheduled && !alarm.ExpiresAt.IsZero() && !alarm.ExpiresAt.After(now) {
			alarm.Status = StatusExpired
			alarm.UpdatedAt = now
			state.Alarms[id] = alarm
			changed = true
		}
	}
	if changed {
		if err := s.save(state); err != nil {
			return nil, err
		}
	}
	alarms := make([]Alarm, 0, len(state.Alarms))
	for _, alarm := range state.Alarms {
		alarms = append(alarms, alarm)
	}
	sort.Slice(alarms, func(i, j int) bool {
		if alarms[i].WakeAt.Equal(alarms[j].WakeAt) {
			return alarms[i].ID < alarms[j].ID
		}
		return alarms[i].WakeAt.Before(alarms[j].WakeAt)
	})
	return alarms, nil
}

func (s *Store) Get(id string, now time.Time) (Alarm, bool, error) {
	alarms, err := s.List(now)
	if err != nil {
		return Alarm{}, false, err
	}
	for _, alarm := range alarms {
		if alarm.ID == id {
			return alarm, true, nil
		}
	}
	return Alarm{}, false, nil
}

func (s *Store) Put(alarm Alarm, now time.Time) (Alarm, error) {
	if err := validate(alarm); err != nil {
		return Alarm{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	unlock, err := s.lockFile()
	if err != nil {
		return Alarm{}, err
	}
	defer unlock()
	state, err := s.load()
	if err != nil {
		return Alarm{}, err
	}
	for _, existing := range state.Alarms {
		if existing.Status == StatusScheduled && existing.Agent == alarm.Agent && existing.SessionID == alarm.SessionID && existing.Kind == alarm.Kind {
			return existing, nil
		}
	}
	if alarm.ID == "" {
		seed := alarm.Agent + "\x00" + alarm.SessionID + "\x00" + alarm.Kind + "\x00" + now.UTC().Format(time.RFC3339Nano)
		hash := sha256.Sum256([]byte(seed))
		alarm.ID = "w_" + hex.EncodeToString(hash[:])[:12]
	}
	alarm.CreatedAt = now.UTC()
	alarm.UpdatedAt = alarm.CreatedAt
	alarm.Status = StatusScheduled
	if state.Alarms == nil {
		state.Alarms = make(map[string]Alarm)
	}
	state.Alarms[alarm.ID] = alarm
	if err := s.save(state); err != nil {
		return Alarm{}, err
	}
	return alarm, nil
}

func (s *Store) Update(id string, now time.Time, update func(*Alarm) error) (Alarm, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	unlock, err := s.lockFile()
	if err != nil {
		return Alarm{}, err
	}
	defer unlock()
	state, err := s.load()
	if err != nil {
		return Alarm{}, err
	}
	alarm, ok := state.Alarms[id]
	if !ok {
		return Alarm{}, fmt.Errorf("wake alarm %q not found", id)
	}
	if err := update(&alarm); err != nil {
		return Alarm{}, err
	}
	if err := validate(alarm); err != nil {
		return Alarm{}, err
	}
	alarm.UpdatedAt = now.UTC()
	state.Alarms[id] = alarm
	if err := s.save(state); err != nil {
		return Alarm{}, err
	}
	return alarm, nil
}

func (s *Store) Cancel(id string, now time.Time) (Alarm, error) {
	return s.Update(id, now, func(alarm *Alarm) error {
		if alarm.Status == StatusCancelled {
			return nil
		}
		alarm.Status = StatusCancelled
		return nil
	})
}

func (s *Store) CancelAgent(agent string, now time.Time) (int, error) {
	alarms, err := s.List(now)
	if err != nil {
		return 0, err
	}
	count := 0
	for _, alarm := range alarms {
		if alarm.Agent == agent && alarm.Status == StatusScheduled {
			if _, err := s.Cancel(alarm.ID, now); err != nil {
				return count, err
			}
			count++
		}
	}
	return count, nil
}

func (s *Store) CancelAll(now time.Time) (int, error) {
	alarms, err := s.List(now)
	if err != nil {
		return 0, err
	}
	count := 0
	for _, alarm := range alarms {
		if alarm.Status == StatusScheduled {
			if _, err := s.Cancel(alarm.ID, now); err != nil {
				return count, err
			}
			count++
		}
	}
	return count, nil
}

var durationPart = regexp.MustCompile(`(?i)([0-9]+)([dhms])`)

// ParseDuration accepts Go-style hours/minutes/seconds plus days, e.g. 2d4h15m.
func ParseDuration(value string) (time.Duration, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, errors.New("duration is empty")
	}
	if value[0] == '-' {
		return 0, errors.New("duration must not be negative")
	}
	parts := durationPart.FindAllStringSubmatch(value, -1)
	if len(parts) == 0 {
		return 0, fmt.Errorf("invalid duration %q; use values such as 2d4h15m", value)
	}
	consumed := 0
	var total time.Duration
	for _, part := range parts {
		consumed += len(part[0])
		n, err := strconv.ParseInt(part[1], 10, 64)
		if err != nil {
			return 0, fmt.Errorf("invalid duration %q", value)
		}
		unit := strings.ToLower(part[2])
		multiplier := time.Second
		switch unit {
		case "d":
			multiplier = 24 * time.Hour
		case "h":
			multiplier = time.Hour
		case "m":
			multiplier = time.Minute
		}
		piece := time.Duration(n) * multiplier
		if piece < 0 || total > time.Duration(1<<63-1)-piece {
			return 0, errors.New("duration is too large")
		}
		total += piece
	}
	if consumed != len(value) {
		return 0, fmt.Errorf("invalid duration %q; use values such as 2d4h15m", value)
	}
	return total, nil
}

func validate(alarm Alarm) error {
	if alarm.Agent != "codex" && alarm.Agent != "claude" {
		return fmt.Errorf("wake agent must be codex or claude")
	}
	if strings.TrimSpace(alarm.SessionID) == "" || strings.TrimSpace(alarm.SurfaceID) == "" {
		return errors.New("wake alarm requires session_id and surface_id")
	}
	if alarm.WakeAt.IsZero() || alarm.ExpiresAt.IsZero() {
		return errors.New("wake alarm requires wake_at and expires_at")
	}
	if alarm.ExpiresAt.Before(alarm.WakeAt) {
		return errors.New("wake alarm expires before wake_at")
	}
	switch alarm.Kind {
	case KindCodexProvider, KindCodexQuota, KindClaudeQuota:
	default:
		return fmt.Errorf("unknown wake kind %q", alarm.Kind)
	}
	return nil
}

func (s *Store) load() (fileState, error) {
	body, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return fileState{Version: 1, Alarms: make(map[string]Alarm)}, nil
	}
	if err != nil {
		return fileState{}, err
	}
	state := fileState{Version: 1, Alarms: make(map[string]Alarm)}
	if len(strings.TrimSpace(string(body))) != 0 {
		if err := json.Unmarshal(body, &state); err != nil {
			return fileState{}, fmt.Errorf("decode wake state: %w", err)
		}
	}
	if state.Alarms == nil {
		state.Alarms = make(map[string]Alarm)
	}
	return state, nil
}

func (s *Store) save(state fileState) error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	body, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	body = append(body, '\n')
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".wake-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(body); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, s.path)
}

func (s *Store) lockFile() (func(), error) {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return nil, err
	}
	return acquireFileLock(s.path + ".lock")
}
