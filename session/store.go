package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/manaflow-ai/subrouter/internal/fsutil"
)

// MaxRetainedAssignments bounds the on-disk and HTTP-visible routing history.
// The client may further cap its snapshot, but the daemon must never
// materialize an unbounded session map first.
const MaxRetainedAssignments = 512

// UserEmailRetention limits how long inactive self-reported identity metadata
// remains attached to a sticky routing assignment. The assignment itself is
// retained so a resumed session still reaches the same account.
const UserEmailRetention = 30 * 24 * time.Hour

// sessionActivityWriteInterval keeps active-session retention accurate without
// rewriting the shared store for every proxied request.
const sessionActivityWriteInterval = 24 * time.Hour

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
	s.pruneLocked()
	return assignment, s.saveLocked()
}

// Touch records recent use of an existing sticky assignment. Updates are
// coalesced because this path runs for ordinary requests and the store may be
// shared by several processes.
func (s *Store) Touch(agentType, sessionID string) (Assignment, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	lock, err := lockSessionStore(s.path)
	if err != nil {
		return Assignment{}, false, err
	}
	defer lock.Close()
	if err := s.loadLocked(); err != nil {
		return Assignment{}, false, err
	}

	key := ScopedSessionKey(agentType, sessionID)
	assignment, ok := s.data[key]
	if !ok {
		return Assignment{}, false, nil
	}
	now := time.Now().UTC()
	if now.Sub(assignment.UpdatedAt) < sessionActivityWriteInterval {
		return assignment, true, nil
	}
	assignment.UpdatedAt = now
	s.data[key] = assignment
	if err := s.saveLocked(); err != nil {
		return Assignment{}, false, err
	}
	return assignment, true, nil
}

// CompareAndPut replaces one sticky assignment only while it still points at
// expectedAccountID. Deferred proxy responses use it so an older stream cannot
// overwrite a newer forced/admin move that completed while the stream was in
// flight. swapped is false, without error, when another writer won the race.
func (s *Store) CompareAndPut(agentType, sessionID, expectedAccountID, accountID, userEmail string) (assignment Assignment, swapped bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	lock, err := lockSessionStore(s.path)
	if err != nil {
		return Assignment{}, false, err
	}
	defer lock.Close()
	if err := s.loadLocked(); err != nil {
		return Assignment{}, false, err
	}

	now := time.Now().UTC()
	normalizedAgent := NormalizeAgentType(agentType)
	if normalizedAgent == "" {
		normalizedAgent = "codex"
	}
	stickySessionID := StickySessionID(normalizedAgent, sessionID)
	key := ScopedSessionKey(normalizedAgent, stickySessionID)
	existing, ok := s.data[key]
	if !ok || existing.AccountID != expectedAccountID {
		return existing, false, nil
	}
	assignment = Assignment{
		AgentType: normalizedAgent,
		SessionID: stickySessionID,
		AccountID: accountID,
		UserEmail: NormalizeUserEmail(userEmail),
		CreatedAt: existing.CreatedAt,
		UpdatedAt: now,
	}
	if assignment.UserEmail == "" {
		assignment.UserEmail = existing.UserEmail
	}
	s.data[key] = assignment
	if err := s.saveLocked(); err != nil {
		s.data[key] = existing
		return Assignment{}, false, err
	}
	return assignment, true, nil
}

// Delete removes one scoped sticky assignment and its self-reported identity
// metadata. It is used by the admin-only sessions endpoint.
func (s *Store) Delete(agentType, sessionID string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	lock, err := lockSessionStore(s.path)
	if err != nil {
		return false, err
	}
	defer lock.Close()
	if err := s.loadLocked(); err != nil {
		return false, err
	}
	key := ScopedSessionKey(agentType, sessionID)
	if _, ok := s.data[key]; !ok {
		return false, nil
	}
	delete(s.data, key)
	return true, s.saveLocked()
}

func (s *Store) All() []Assignment {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reloadBestEffortLocked()

	s.pruneLocked()
	assignments := make([]Assignment, 0, len(s.data))
	for _, assignment := range s.data {
		assignments = append(assignments, assignment)
	}
	sort.Slice(assignments, func(i, j int) bool {
		if assignments[i].UpdatedAt.Equal(assignments[j].UpdatedAt) {
			return assignments[i].SessionID < assignments[j].SessionID
		}
		return assignments[i].UpdatedAt.After(assignments[j].UpdatedAt)
	})
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
		// Assignments are routing stickiness only: losing them sends the
		// next request of each session through normal account selection.
		// Refusing to load would instead fail every Put/Touch/CompareAndPut
		// forever, so keep the bytes for inspection and start empty.
		if quarantineErr := s.quarantineCorruptLocked(err); quarantineErr != nil {
			return quarantineErr
		}
		s.data = map[string]Assignment{}
		return nil
	}
	s.data = data
	s.migrateLoadedAssignments()
	changed := s.expireUserEmailsLocked(time.Now().UTC())
	if s.pruneLocked() {
		// Rewrite legacy/unbounded files while the session lock is held so
		// every subsequent reader starts from the bounded representation.
		changed = true
	}
	if changed {
		return s.saveLocked()
	}
	return nil
}

func (s *Store) pruneLocked() bool {
	if len(s.data) <= MaxRetainedAssignments {
		return false
	}
	type entry struct {
		key       string
		updatedAt time.Time
	}
	entries := make([]entry, 0, len(s.data))
	for key, assignment := range s.data {
		entries = append(entries, entry{key: key, updatedAt: assignment.UpdatedAt})
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].updatedAt.Equal(entries[j].updatedAt) {
			return entries[i].key < entries[j].key
		}
		return entries[i].updatedAt.After(entries[j].updatedAt)
	})
	for _, stale := range entries[MaxRetainedAssignments:] {
		delete(s.data, stale.key)
	}
	return true
}

func (s *Store) expireUserEmailsLocked(now time.Time) bool {
	changed := false
	for key, assignment := range s.data {
		if assignment.UserEmail == "" || now.Sub(assignment.UpdatedAt) < UserEmailRetention {
			continue
		}
		assignment.UserEmail = ""
		s.data[key] = assignment
		changed = true
	}
	return changed
}

func (s *Store) saveLocked() error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	body, err := json.MarshalIndent(s.data, "", "  ")
	if err != nil {
		return err
	}
	return fsutil.WriteFileAtomic(s.path, body, 0o600)
}

// quarantineCorruptLocked moves an unparseable store aside. The caller holds
// the session store lock, so no other process is writing s.path meanwhile.
func (s *Store) quarantineCorruptLocked(parseErr error) error {
	aside := fmt.Sprintf("%s.corrupt-%d", s.path, time.Now().UnixNano())
	if err := os.Rename(s.path, aside); err != nil {
		return fmt.Errorf("parse %s: %w (quarantine failed: %v)", s.path, parseErr, err)
	}
	if err := fsutil.SyncDir(filepath.Dir(s.path)); err != nil {
		return fmt.Errorf("sync %s after quarantine: %w", filepath.Dir(s.path), err)
	}
	log.Printf("session store: %s is corrupt (%v); moved it to %s and starting with no sticky assignments", s.path, parseErr, aside)
	return nil
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
