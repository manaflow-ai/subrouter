package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
)

// claudeWebBalanceTTL bounds how long a pushed prepaid balance is served.
// This is display-only data, so staleness is tolerated generously: hours, not
// minutes.
const claudeWebBalanceTTL = 24 * time.Hour

// claudeWebBalanceRecord is one pushed prepaid-balance reading.
type claudeWebBalanceRecord struct {
	BalanceCents float64   `json:"balance_cents"`
	FetchedAt    time.Time `json:"fetched_at"`
	Source       string    `json:"source,omitempty"`
}

type claudeWebBalanceFile struct {
	Balances map[string]claudeWebBalanceRecord `json:"balances"`
}

// claudeWebBalanceStore is the server's JSON-backed store of Claude prepaid
// extra-usage balances pushed by CLI machines that hold a claude.ai web
// session, so clients without one still see the balance in usage-status. The
// balance is display-only metadata; it never feeds routing decisions.
type claudeWebBalanceStore struct {
	path string
	mu   sync.Mutex
	now  func() time.Time
}

func newClaudeWebBalanceStore(path string) *claudeWebBalanceStore {
	return &claudeWebBalanceStore{path: path, now: time.Now}
}

func (s *claudeWebBalanceStore) load() claudeWebBalanceFile {
	data, err := os.ReadFile(s.path)
	if err != nil {
		return claudeWebBalanceFile{Balances: map[string]claudeWebBalanceRecord{}}
	}
	var file claudeWebBalanceFile
	if err := json.Unmarshal(data, &file); err != nil || file.Balances == nil {
		return claudeWebBalanceFile{Balances: map[string]claudeWebBalanceRecord{}}
	}
	return file
}

// record returns the stored balance when it exists and is fresher than the
// TTL; stale entries are treated as absent.
func (s *claudeWebBalanceStore) record(email string) (claudeWebBalanceRecord, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	record, ok := s.load().Balances[email]
	if !ok || s.now().Sub(record.FetchedAt) >= claudeWebBalanceTTL {
		return claudeWebBalanceRecord{}, false
	}
	return record, true
}

func (s *claudeWebBalanceStore) set(email string, balanceCents float64, source string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	file := s.load()
	file.Balances[email] = claudeWebBalanceRecord{
		BalanceCents: balanceCents,
		FetchedAt:    s.now(),
		Source:       source,
	}
	data, err := json.Marshal(file)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0700); err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return err
	}
	if err := os.Rename(tmp, s.path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// handleClaudeWebBalance accepts a pushed prepaid balance reading. It is an
// admin-gated mutating endpoint, same as the other mutating /_subrouter APIs.
func (s Server) handleClaudeWebBalance(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.claudeWebBalances == nil {
		http.Error(w, "balance store unavailable", http.StatusServiceUnavailable)
		return
	}
	var payload struct {
		Email        string  `json:"email"`
		BalanceCents float64 `json:"balance_cents"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&payload); err != nil {
		http.Error(w, "invalid payload", http.StatusBadRequest)
		return
	}
	email := strings.ToLower(strings.TrimSpace(payload.Email))
	if !strings.Contains(email, "@") || len(email) > 254 {
		http.Error(w, "invalid email", http.StatusBadRequest)
		return
	}
	if payload.BalanceCents < 0 {
		http.Error(w, "invalid balance", http.StatusBadRequest)
		return
	}
	if err := s.claudeWebBalances.set(email, payload.BalanceCents, "push"); err != nil {
		http.Error(w, "balance store write failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

// withClaudeWebBalances overlays pushed prepaid balances onto Claude usage
// statuses by account email. It only ever adds CreditsBalance — whatever the
// OAuth pipeline produced is otherwise untouched, and when it produced no
// ExtraUsage a minimal one carrying only the balance is attached.
func (s Server) withClaudeWebBalances(statuses []AccountUsageStatus) []AccountUsageStatus {
	if s.claudeWebBalances == nil {
		return statuses
	}
	for i := range statuses {
		if statuses[i].Provider != accounts.ProviderClaude {
			continue
		}
		email := strings.ToLower(strings.TrimSpace(statuses[i].Email))
		if email == "" {
			continue
		}
		record, ok := s.claudeWebBalances.record(email)
		if !ok {
			continue
		}
		balance := record.BalanceCents
		if statuses[i].ExtraUsage != nil {
			merged := *statuses[i].ExtraUsage
			merged.CreditsBalance = &balance
			statuses[i].ExtraUsage = &merged
		} else {
			statuses[i].ExtraUsage = &accounts.ExtraUsageInfo{CreditsBalance: &balance}
		}
	}
	return statuses
}
