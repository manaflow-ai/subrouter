package proxy

import (
	"sync"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
)

const (
	codexSessionBanThreshold = 2
	codexSessionBanRetention = 24 * time.Hour
)

type codexSessionBans struct {
	mu       sync.Mutex
	sessions map[string]*codexSessionBanEntry
	now      func() time.Time
}

type codexSessionBanEntry struct {
	failures map[string]int
	banned   map[string]struct{}
	touched  time.Time
}

func newCodexSessionBans() *codexSessionBans {
	return &codexSessionBans{sessions: map[string]*codexSessionBanEntry{}, now: time.Now}
}

func (b *codexSessionBans) noteFailure(key, account string) bool {
	if b == nil || key == "" || account == "" {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	b.sweepLocked(now)
	e := b.entryLocked(key, now)
	if _, ok := e.banned[account]; ok {
		return false
	}
	e.failures[account]++
	if e.failures[account] < codexSessionBanThreshold {
		return false
	}
	delete(e.failures, account)
	e.banned[account] = struct{}{}
	return true
}

func (b *codexSessionBans) noteSuccess(key, account string) {
	if b == nil || key == "" || account == "" {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if e := b.sessions[key]; e != nil {
		e.touched = b.now()
		delete(e.failures, account)
	}
}

func (b *codexSessionBans) banned(key, account string) bool {
	if b == nil || key == "" || account == "" {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	e := b.sessions[key]
	if e == nil {
		return false
	}
	e.touched = b.now()
	_, ok := e.banned[account]
	return ok
}

func (b *codexSessionBans) entryLocked(key string, now time.Time) *codexSessionBanEntry {
	e := b.sessions[key]
	if e == nil {
		e = &codexSessionBanEntry{failures: map[string]int{}, banned: map[string]struct{}{}}
		b.sessions[key] = e
	}
	e.touched = now
	return e
}

func (b *codexSessionBans) sweepLocked(now time.Time) {
	for key, e := range b.sessions {
		if now.Sub(e.touched) > codexSessionBanRetention {
			delete(b.sessions, key)
		}
	}
}

func (s Server) codexSessionBanned(agent, session, account string) bool {
	return s.codexSessionBans != nil && s.codexSessionBans.banned(azureCodexSessionKeyFor(agent, session), account)
}

func (s Server) withoutCodexSessionBans(agent, session string, candidates []accounts.Account) []accounts.Account {
	if s.codexSessionBans == nil {
		return candidates
	}
	out := make([]accounts.Account, 0, len(candidates))
	for _, account := range candidates {
		if !s.codexSessionBanned(agent, session, account.ID) {
			out = append(out, account)
		}
	}
	return out
}

func (s Server) noteCodexSessionCapacityFailure(agent, session, account string) {
	if s.codexSessionBans == nil || s.CodexOverloadFailover == nil || !s.CodexOverloadFailover.enabled() {
		return
	}
	if s.codexSessionBans.noteFailure(azureCodexSessionKeyFor(agent, session), account) && s.Logger != nil {
		s.Logger.Warn("codex session banned from account after repeated capacity failures; rotating session", "agent", agent, "session", session, "account", account, "threshold", codexSessionBanThreshold)
	}
}

func (s Server) noteCodexSessionCapacitySuccess(agent, session, account string) {
	if s.codexSessionBans != nil {
		s.codexSessionBans.noteSuccess(azureCodexSessionKeyFor(agent, session), account)
	}
}
