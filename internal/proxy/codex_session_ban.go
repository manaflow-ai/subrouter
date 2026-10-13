package proxy

import (
	"sync"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
)

// A Codex session that fails on one account twice in a row stops using that
// account for the rest of the session. The pool-wide mark in codex_overload.go
// and codex_capacity.go is shared by every session and lasts about two
// minutes; this ban is private to the session and lasts as long as the
// session is in use. Without it the retried request is pinned by the session
// assignment and keeps landing on the account that just failed.
const (
	// codexSessionBanThreshold is the number of consecutive capacity failures
	// one session may take on one account before the account is banned for
	// that session.
	codexSessionBanThreshold = 2
	// codexSessionBanRetention bounds idle session state in memory. A session
	// that has not failed or succeeded for this long is forgotten, which means
	// its ban lifts; the session is then routed afresh.
	codexSessionBanRetention = 24 * time.Hour
)

// codexSessionBans holds, per session, the failure streak and the banned
// accounts. It lives in memory only and is lost on restart.
type codexSessionBans struct {
	mu       sync.Mutex
	sessions map[string]*codexSessionBanEntry
	now      func() time.Time
}

type codexSessionBanEntry struct {
	// failures counts consecutive capacity failures per account, reset by a
	// success on that account.
	failures map[string]int
	banned   map[string]struct{}
	touched  time.Time
}

func newCodexSessionBans() *codexSessionBans {
	return &codexSessionBans{sessions: map[string]*codexSessionBanEntry{}, now: time.Now}
}

// noteFailure records one capacity failure of the session on the account and
// reports whether this failure bans the account for the session.
func (b *codexSessionBans) noteFailure(sessionKey, accountID string) bool {
	if b == nil || sessionKey == "" || accountID == "" {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.sweepLocked(b.now())
	entry := b.entryLocked(sessionKey)
	if _, ok := entry.banned[accountID]; ok {
		return false
	}
	entry.failures[accountID]++
	if entry.failures[accountID] < codexSessionBanThreshold {
		return false
	}
	delete(entry.failures, accountID)
	entry.banned[accountID] = struct{}{}
	return true
}

// noteSuccess ends the account's failure streak for the session.
func (b *codexSessionBans) noteSuccess(sessionKey, accountID string) {
	if b == nil || sessionKey == "" || accountID == "" {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if entry, ok := b.sessions[sessionKey]; ok {
		delete(entry.failures, accountID)
	}
}

// banned reports whether the account is banned for the session.
func (b *codexSessionBans) banned(sessionKey, accountID string) bool {
	if b == nil || sessionKey == "" || accountID == "" {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	entry, ok := b.sessions[sessionKey]
	if !ok {
		return false
	}
	_, ok = entry.banned[accountID]
	return ok
}

// entryLocked returns the session's entry, creating it when absent, and marks
// it as in use now. Caller holds mu.
func (b *codexSessionBans) entryLocked(sessionKey string) *codexSessionBanEntry {
	now := b.now()
	entry, ok := b.sessions[sessionKey]
	if !ok {
		entry = &codexSessionBanEntry{failures: map[string]int{}, banned: map[string]struct{}{}}
		b.sessions[sessionKey] = entry
	}
	entry.touched = now
	return entry
}

// sweepLocked forgets sessions idle past the retention window. It runs on the
// failure path only, so the per-turn success path stays constant time.
// Caller holds mu.
func (b *codexSessionBans) sweepLocked(now time.Time) {
	for key, entry := range b.sessions {
		if now.Sub(entry.touched) > codexSessionBanRetention {
			delete(b.sessions, key)
		}
	}
}

// codexSessionBanned reports whether the Codex session must not use the
// account. Only Codex records bans, so other providers never match.
func (s Server) codexSessionBanned(agentType, sessionID, accountID string) bool {
	return s.codexSessionBans.banned(azureCodexSessionKeyFor(agentType, sessionID), accountID)
}

// withoutCodexSessionBans drops the accounts this Codex session is banned from.
func (s Server) withoutCodexSessionBans(agentType, sessionID string, candidates []accounts.Account) []accounts.Account {
	kept := make([]accounts.Account, 0, len(candidates))
	for _, account := range candidates {
		if s.codexSessionBanned(agentType, sessionID, account.ID) {
			continue
		}
		kept = append(kept, account)
	}
	return kept
}
