package session

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type Assignment struct {
	AgentType string    `json:"agent_type"`
	SessionID string    `json:"session_id"`
	AccountID string    `json:"account_id"`
	UserEmail string    `json:"user_email,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type Store struct {
	path string
	mu   sync.Mutex
	data map[string]Assignment
}

func NewStore(path string) (*Store, error) {
	store := &Store{path: path, data: map[string]Assignment{}}
	lock, err := lockSessionStore(path)
	if err != nil {
		return nil, err
	}
	defer lock.Close()
	if err := store.loadLocked(); err != nil {
		return nil, err
	}
	return store, nil
}

func (s *Store) Get(agentType, sessionID string) (Assignment, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reloadBestEffortLocked()
	assignment, ok := s.data[ScopedSessionKey(agentType, sessionID)]
	return assignment, ok
}

func (s *Store) Put(agentType, sessionID, accountID, userEmail string) (Assignment, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	lock, err := lockSessionStore(s.path)
	if err != nil {
		return Assignment{}, err
	}
	defer lock.Close()
	if err := s.loadLocked(); err != nil {
		return Assignment{}, err
	}

	now := time.Now().UTC()
	normalizedAgent := NormalizeAgentType(agentType)
	if normalizedAgent == "" {
		normalizedAgent = "codex"
	}
	stickySessionID := StickySessionID(normalizedAgent, sessionID)
	key := ScopedSessionKey(normalizedAgent, stickySessionID)
	assignment := Assignment{
		AgentType: normalizedAgent,
		SessionID: stickySessionID,
		AccountID: accountID,
		UserEmail: NormalizeUserEmail(userEmail),
		CreatedAt: now,
		UpdatedAt: now,
	}
	if existing, ok := s.data[key]; ok {
		assignment.CreatedAt = existing.CreatedAt
		if assignment.UserEmail == "" {
			assignment.UserEmail = existing.UserEmail
		}
	}
	s.data[key] = assignment
	return assignment, s.saveLocked()
}

func (s *Store) All() []Assignment {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reloadBestEffortLocked()

	assignments := make([]Assignment, 0, len(s.data))
	for _, assignment := range s.data {
		assignments = append(assignments, assignment)
	}
	return assignments
}

func (s *Store) CountByAccount() map[string]int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reloadBestEffortLocked()

	counts := make(map[string]int)
	for _, assignment := range s.data {
		counts[assignment.AccountID]++
	}
	return counts
}

func (s *Store) reloadBestEffortLocked() {
	lock, err := lockSessionStore(s.path)
	if err != nil {
		return
	}
	defer lock.Close()
	_ = s.loadLocked()
}

func (s *Store) loadLocked() error {
	body, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		s.data = map[string]Assignment{}
		return nil
	}
	if err != nil {
		return err
	}
	data := map[string]Assignment{}
	if err := json.Unmarshal(body, &data); err != nil {
		return err
	}
	s.data = data
	s.migrateLoadedAssignments()
	return nil
}

func (s *Store) saveLocked() error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	body, err := json.MarshalIndent(s.data, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, body, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

func DefaultStorePath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ".subrouter/sessions.json"
	}
	return filepath.Join(home, ".subrouter", "sessions.json")
}

func ScopedSessionKey(agentType, sessionID string) string {
	normalizedAgent := NormalizeAgentType(agentType)
	if normalizedAgent == "" {
		normalizedAgent = "codex"
	}
	return normalizedAgent + ":" + StickySessionID(normalizedAgent, sessionID)
}

func StickySessionID(agentType, sessionID string) string {
	if NormalizeAgentType(agentType) == "codex" {
		return BaseSessionID(sessionID)
	}
	return sessionID
}

func BaseSessionID(sessionID string) string {
	if before, _, ok := strings.Cut(sessionID, ":"); ok {
		return before
	}
	return sessionID
}

func (s *Store) migrateLoadedAssignments() {
	migrated := make(map[string]Assignment, len(s.data))
	for key, assignment := range s.data {
		if assignment.SessionID == "" {
			assignment.SessionID = key
		}
		if assignment.AgentType == "" {
			assignment.AgentType = "codex"
		} else if normalized := NormalizeAgentType(assignment.AgentType); normalized != "" {
			assignment.AgentType = normalized
		} else {
			assignment.AgentType = "codex"
		}
		assignment.SessionID = StickySessionID(assignment.AgentType, assignment.SessionID)
		nextKey := ScopedSessionKey(assignment.AgentType, assignment.SessionID)
		if existing, ok := migrated[nextKey]; ok && existing.UpdatedAt.After(assignment.UpdatedAt) {
			continue
		}
		migrated[nextKey] = assignment
	}
	s.data = migrated
}
