package proxy

import (
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
)

// Codex "Selected model is at capacity" (server_is_overloaded) is a property
// of the account, not the request: the same turn on another account usually
// completes. The failover in codex_overload.go acts on one failure at a time
// and forgets it after MarkTTL. This file keeps the history per account so
// the team can see which accounts fail how often, and so the failover can
// hold a persistently constrained account out of routing for longer than a
// single mark.
//
// Retries are the trap in both. cmux-codex retries a failed turn without
// limit, so one hung session on one bad account produces hundreds of
// failures that say nothing new. Every failure is recorded, but the numbers
// that drive rate and rotation are "episodes": a failure whose session did
// not already fail on this account inside codexCapacityRetryWindow without a
// success in between. A burst of retries from one session is one episode.

const (
	// codexCapacityRetryWindow is how long a session's repeat failure on the
	// same account counts as a retry of the last one rather than a new episode.
	codexCapacityRetryWindow = 5 * time.Minute
	// codexCapacityRetention bounds the per-account history. Snapshots
	// summarise 15m, 1h and 24h windows, so nothing older is needed.
	codexCapacityRetention = 24 * time.Hour
	// codexCapacityMaxOutcomes caps one account's history in memory.
	codexCapacityMaxOutcomes = 4096
	// codexCapacityPersistentStreak is the streak (consecutive capacity
	// failures with no success) at which an account counts as persistently
	// at capacity, provided the streak also spans two sessions or
	// codexCapacityPersistentAge. Two conditions because three retries of one
	// turn fired inside ten seconds is one unlucky turn, not a bad account.
	codexCapacityPersistentStreak = 3
	codexCapacityPersistentAge    = time.Minute
	// codexCapacityMaxMarkTTL caps the escalated mark. Overload is a
	// probability that changes minute to minute, so no account is held out
	// for longer than this on capacity evidence alone.
	codexCapacityMaxMarkTTL = 15 * time.Minute
	// codexCapacityPersistentRerouteInterval is how often a websocket session
	// that has spent its reroute budget may still leave a persistently
	// constrained account. Without it the fork's retries ride the bound
	// websocket to the same account forever; with it the session moves at
	// most once a minute, which cannot storm.
	codexCapacityPersistentRerouteInterval = time.Minute
)

type codexCapacityOutcome struct {
	at      time.Time
	failed  bool
	retry   bool
	session string
}

type codexCapacityAccount struct {
	outcomes []codexCapacityOutcome
	// streak counts consecutive capacity failures since the last success.
	streak         int
	streakEpisodes int
	streakStart    time.Time
	streakSessions map[string]struct{}
	lastFailure    time.Time
	lastSuccess    time.Time
	// lastFailureBySession decides whether the next failure is a retry.
	lastFailureBySession map[string]time.Time
	byReason             map[string]uint64
	totalFailures        uint64
	totalEpisodes        uint64
	totalSuccesses       uint64
	markedUntil          time.Time
	markTTL              time.Duration
}

// codexCapacityStats is the per-account capacity history. One per Server,
// shared by the HTTP failover transport and the websocket copier.
type codexCapacityStats struct {
	mu       sync.Mutex
	accounts map[string]*codexCapacityAccount
	now      func() time.Time
}

func newCodexCapacityStats() *codexCapacityStats {
	return &codexCapacityStats{accounts: map[string]*codexCapacityAccount{}, now: time.Now}
}

// codexCapacityVerdict is what one failure means for routing.
type codexCapacityVerdict struct {
	// Retry: the same session already failed on this account inside the
	// retry window. Counted, but not a new episode.
	Retry bool
	// Persistent: the account has failed repeatedly across sessions or time
	// with no success in between.
	Persistent bool
	// Streak is the consecutive failure count including this one.
	Streak int
	// MarkTTL is how long the account should stay out of routing, escalated
	// from base when the account is persistent.
	MarkTTL time.Duration
}

func (s *codexCapacityStats) account(accountID string) *codexCapacityAccount {
	entry := s.accounts[accountID]
	if entry == nil {
		entry = &codexCapacityAccount{
			streakSessions:       map[string]struct{}{},
			lastFailureBySession: map[string]time.Time{},
			byReason:             map[string]uint64{},
		}
		s.accounts[accountID] = entry
	}
	return entry
}

// noteFailure records one capacity failure and returns how routing should
// treat the account. baseTTL is the configured single-failure mark.
func (s *codexCapacityStats) noteFailure(accountID, sessionKey, reason string, baseTTL time.Duration) codexCapacityVerdict {
	if s == nil || accountID == "" {
		return codexCapacityVerdict{MarkTTL: baseTTL}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	entry := s.account(accountID)
	entry.prune(now)
	retry := false
	if last, ok := entry.lastFailureBySession[sessionKey]; ok && sessionKey != "" &&
		now.Sub(last) < codexCapacityRetryWindow && !entry.lastSuccess.After(last) {
		retry = true
	}
	if sessionKey != "" {
		entry.lastFailureBySession[sessionKey] = now
	}
	if entry.streak == 0 {
		entry.streakStart = now
		entry.streakSessions = map[string]struct{}{}
		entry.streakEpisodes = 0
	}
	entry.streak++
	if !retry {
		entry.streakEpisodes++
		entry.totalEpisodes++
	}
	if sessionKey != "" {
		entry.streakSessions[sessionKey] = struct{}{}
	}
	entry.lastFailure = now
	entry.totalFailures++
	if reason == "" {
		reason = "unknown"
	}
	entry.byReason[reason]++
	entry.append(codexCapacityOutcome{at: now, failed: true, retry: retry, session: sessionKey})
	persistent := entry.persistent(now)
	ttl := codexCapacityMarkTTL(baseTTL, entry.streakEpisodes, persistent)
	entry.markTTL = ttl
	entry.markedUntil = now.Add(ttl)
	return codexCapacityVerdict{Retry: retry, Persistent: persistent, Streak: entry.streak, MarkTTL: ttl}
}

// noteSuccess records a completed turn. It ends the account's streak.
func (s *codexCapacityStats) noteSuccess(accountID, sessionKey string) {
	if s == nil || accountID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	entry := s.account(accountID)
	entry.prune(now)
	entry.streak = 0
	entry.streakEpisodes = 0
	entry.streakStart = time.Time{}
	entry.streakSessions = map[string]struct{}{}
	entry.lastSuccess = now
	entry.totalSuccesses++
	if sessionKey != "" {
		delete(entry.lastFailureBySession, sessionKey)
	}
	entry.append(codexCapacityOutcome{at: now, session: sessionKey})
}

// persistent reports whether the account is in a persistent capacity state.
// It requires the last outcome to be a failure (a success ends the state) and
// streak evidence beyond one turn's retries: enough failures, and either more
// than one session or a streak older than codexCapacityPersistentAge.
func (s *codexCapacityStats) persistent(accountID string) bool {
	if s == nil || accountID == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	entry := s.accounts[accountID]
	if entry == nil {
		return false
	}
	return entry.persistent(s.now())
}

func (a *codexCapacityAccount) persistent(now time.Time) bool {
	if a.streak < codexCapacityPersistentStreak {
		return false
	}
	if len(a.streakSessions) >= 2 {
		return true
	}
	return now.Sub(a.streakStart) >= codexCapacityPersistentAge
}

// codexCapacityMarkTTL escalates the mark once an account is persistent:
// base for the first two episodes, then doubling per episode up to the cap.
func codexCapacityMarkTTL(base time.Duration, episodes int, persistent bool) time.Duration {
	if base <= 0 {
		base = codexOverloadDefaultMarkTTL
	}
	if !persistent {
		return base
	}
	ttl := base
	for i := 2; i < episodes && ttl < codexCapacityMaxMarkTTL; i++ {
		ttl *= 2
	}
	if ttl > codexCapacityMaxMarkTTL {
		ttl = codexCapacityMaxMarkTTL
	}
	return ttl
}

func (a *codexCapacityAccount) append(outcome codexCapacityOutcome) {
	a.outcomes = append(a.outcomes, outcome)
	if len(a.outcomes) > codexCapacityMaxOutcomes {
		a.outcomes = append([]codexCapacityOutcome(nil), a.outcomes[len(a.outcomes)-codexCapacityMaxOutcomes:]...)
	}
}

func (a *codexCapacityAccount) prune(now time.Time) {
	cutoff := now.Add(-codexCapacityRetention)
	first := 0
	for first < len(a.outcomes) && a.outcomes[first].at.Before(cutoff) {
		first++
	}
	if first > 0 {
		a.outcomes = append([]codexCapacityOutcome(nil), a.outcomes[first:]...)
	}
	retryCutoff := now.Add(-codexCapacityRetryWindow)
	for session, at := range a.lastFailureBySession {
		if at.Before(retryCutoff) {
			delete(a.lastFailureBySession, session)
		}
	}
}

// CodexCapacityWindow summarises one account over one trailing window.
type CodexCapacityWindow struct {
	// Failures is every capacity failure, retries included.
	Failures int `json:"failures"`
	// Episodes is failures with retries collapsed: the count that reflects
	// how often a fresh turn hit capacity.
	Episodes int `json:"episodes"`
	// Sessions is the distinct sessions that hit capacity.
	Sessions int `json:"sessions"`
	// Successes is completed turns.
	Successes int `json:"successes"`
	// Rate is Episodes / (Episodes + Successes), the share of fresh turns
	// that hit capacity. -1 when the window has no turns.
	Rate float64 `json:"rate"`
}

// CodexCapacityAccountStats is one account's row in the capacity report.
type CodexCapacityAccountStats struct {
	ID    string `json:"id"`
	Label string `json:"label,omitempty"`
	Email string `json:"email,omitempty"`

	Last15m CodexCapacityWindow `json:"last_15m"`
	Last1h  CodexCapacityWindow `json:"last_1h"`
	Last24h CodexCapacityWindow `json:"last_24h"`

	// Streak is consecutive capacity failures with no success in between.
	Streak int `json:"streak"`
	// StreakEpisodes is the streak with retries collapsed.
	StreakEpisodes int `json:"streak_episodes"`
	// StreakSince is when the streak began; empty when there is none.
	StreakSince string `json:"streak_since,omitempty"`
	// Persistent is the rotation verdict: the account is held out of
	// routing on an escalated mark.
	Persistent bool `json:"persistent"`
	// MarkedUntil is when the latest capacity mark expires, if in the future.
	MarkedUntil string `json:"marked_until,omitempty"`
	// MarkTTL is the length of the latest mark.
	MarkTTL string `json:"mark_ttl,omitempty"`

	LastFailure string            `json:"last_failure,omitempty"`
	LastSuccess string            `json:"last_success,omitempty"`
	ByReason    map[string]uint64 `json:"by_reason,omitempty"`

	TotalFailures  uint64 `json:"total_failures"`
	TotalEpisodes  uint64 `json:"total_episodes"`
	TotalSuccesses uint64 `json:"total_successes"`
}

// CodexCapacityReport is the payload of /_subrouter/codex-capacity.
type CodexCapacityReport struct {
	GeneratedAt string `json:"generated_at"`
	// RetryWindow is how long a repeat failure from one session counts as a
	// retry rather than a new episode.
	RetryWindow string `json:"retry_window"`
	// Persistent states the rotation rule in words so a reader of the JSON
	// does not need the source.
	PersistentRule string                      `json:"persistent_rule"`
	Accounts       []CodexCapacityAccountStats `json:"accounts"`
}

func (s *codexCapacityStats) snapshot(labels map[string]accounts.Account) CodexCapacityReport {
	report := CodexCapacityReport{
		RetryWindow: codexCapacityRetryWindow.String(),
		PersistentRule: "streak >= " + strconv.Itoa(codexCapacityPersistentStreak) +
			" consecutive capacity failures and (>= 2 sessions or streak age >= " +
			codexCapacityPersistentAge.String() + "); mark doubles per episode up to " +
			codexCapacityMaxMarkTTL.String() + "; any success resets",
	}
	if s == nil {
		report.GeneratedAt = time.Now().UTC().Format(time.RFC3339)
		return report
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	report.GeneratedAt = now.UTC().Format(time.RFC3339)
	for id, entry := range s.accounts {
		entry.prune(now)
		row := CodexCapacityAccountStats{
			ID:             id,
			Last15m:        entry.window(now, 15*time.Minute),
			Last1h:         entry.window(now, time.Hour),
			Last24h:        entry.window(now, codexCapacityRetention),
			Streak:         entry.streak,
			StreakEpisodes: entry.streakEpisodes,
			Persistent:     entry.persistent(now),
			TotalFailures:  entry.totalFailures,
			TotalEpisodes:  entry.totalEpisodes,
			TotalSuccesses: entry.totalSuccesses,
		}
		if account, ok := labels[id]; ok {
			row.Label = account.Label
			row.Email = account.Email
		}
		if entry.streak > 0 {
			row.StreakSince = entry.streakStart.UTC().Format(time.RFC3339)
		}
		if entry.markedUntil.After(now) {
			row.MarkedUntil = entry.markedUntil.UTC().Format(time.RFC3339)
			row.MarkTTL = entry.markTTL.String()
		}
		if !entry.lastFailure.IsZero() {
			row.LastFailure = entry.lastFailure.UTC().Format(time.RFC3339)
		}
		if !entry.lastSuccess.IsZero() {
			row.LastSuccess = entry.lastSuccess.UTC().Format(time.RFC3339)
		}
		if len(entry.byReason) > 0 {
			row.ByReason = make(map[string]uint64, len(entry.byReason))
			for reason, count := range entry.byReason {
				row.ByReason[reason] = count
			}
		}
		report.Accounts = append(report.Accounts, row)
	}
	sort.Slice(report.Accounts, func(i, j int) bool {
		a, b := report.Accounts[i], report.Accounts[j]
		if a.Last24h.Episodes != b.Last24h.Episodes {
			return a.Last24h.Episodes > b.Last24h.Episodes
		}
		if a.Last24h.Rate != b.Last24h.Rate {
			return a.Last24h.Rate > b.Last24h.Rate
		}
		return a.ID < b.ID
	})
	return report
}

func (a *codexCapacityAccount) window(now time.Time, span time.Duration) CodexCapacityWindow {
	cutoff := now.Add(-span)
	var out CodexCapacityWindow
	sessions := map[string]struct{}{}
	for _, outcome := range a.outcomes {
		if outcome.at.Before(cutoff) {
			continue
		}
		if !outcome.failed {
			out.Successes++
			continue
		}
		out.Failures++
		if !outcome.retry {
			out.Episodes++
		}
		if outcome.session != "" {
			sessions[outcome.session] = struct{}{}
		}
	}
	out.Sessions = len(sessions)
	if total := out.Episodes + out.Successes; total > 0 {
		out.Rate = float64(out.Episodes) / float64(total)
	} else {
		out.Rate = -1
	}
	return out
}

// codexCapacityReason names the failure for the by-reason breakdown:
// "server_is_overloaded" for the capacity code, otherwise the upstream's own
// code or the transport reason.
func codexCapacityReason(body []byte, fallback string) string {
	var event map[string]any
	if err := json.Unmarshal(body, &event); err == nil {
		code, message := codexFailureCodeAndMessage(event)
		if code != "" {
			return code
		}
		if strings.Contains(strings.ToLower(message), "at capacity") {
			return "server_is_overloaded"
		}
	}
	if fallback == "" {
		return "unknown"
	}
	return fallback
}

// codexWebSocketResponseCompleted reports a turn that finished with output,
// the success signal for capacity accounting. response.incomplete and
// response.failed are not successes; response.done is treated as one.
func codexWebSocketResponseCompleted(body []byte) bool {
	var event map[string]any
	if err := json.Unmarshal(body, &event); err != nil {
		return false
	}
	switch strings.ToLower(stringField(event, "type")) {
	case "response.completed", "response.done":
		return true
	default:
		return false
	}
}

// noteCodexCapacityFailure records the failure and marks the account out of
// this pool for the verdict's TTL. It replaces the flat markAccountOverloaded
// on both the HTTP and websocket paths.
func (s *Server) noteCodexCapacityFailure(accountID, sessionKey, poolModel, reason string) codexCapacityVerdict {
	base := s.CodexOverloadFailover.markTTL()
	verdict := s.codexCapacity.noteFailure(accountID, sessionKey, reason, base)
	s.markAccountOverloaded(accountID, poolModel, verdict.MarkTTL)
	if verdict.Persistent && s.Logger != nil {
		s.Logger.Warn("codex account persistently at capacity; held out on escalated mark",
			"account", accountID, "pool", poolModel, "streak", verdict.Streak, "retry", verdict.Retry, "ttl", verdict.MarkTTL.String())
	}
	return verdict
}

func (s *Server) noteCodexCapacitySuccess(accountID, sessionKey string) {
	s.codexCapacity.noteSuccess(accountID, sessionKey)
}

// handleCodexCapacity serves the per-account capacity report.
func (s Server) handleCodexCapacity(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	labels := map[string]accounts.Account{}
	for _, account := range s.accountListContext(r.Context()) {
		labels[account.ID] = account
	}
	writeJSON(w, s.codexCapacity.snapshot(labels))
}
