package proxy

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/internal/broker"
	"github.com/manaflow-ai/subrouter/internal/tenant"
	"github.com/manaflow-ai/subrouter/session"
)

const (
	tenantCredentialLeaseTTL      = 5 * time.Minute
	tenantCredentialLeaseMax      = 4096
	tenantCredentialLeaseMaxBytes = 64 << 10
)

type tenantCredentialLeaseStore struct {
	mu     sync.Mutex
	leases map[string]tenantCredentialLease
}

type tenantCredentialLease struct {
	accountID            string
	provider             accounts.Provider
	authMode             accounts.AuthMode
	credentialGeneration int
	model                string
	expiresAt            time.Time
}

type tenantCredentialLeaseRequest struct {
	Provider         string `json:"provider"`
	RequiredAuthMode string `json:"requiredAuthMode,omitempty"`
	AgentType        string `json:"agentType,omitempty"`
	SessionID        string `json:"sessionId"`
	UserEmail        string `json:"userEmail,omitempty"`
	PreferAccountID  string `json:"preferAccountId,omitempty"`
	Model            string `json:"model,omitempty"`
}

type tenantCredentialLeaseReport struct {
	Outcome    broker.LeaseOutcome       `json:"outcome"`
	StatusCode int                       `json:"statusCode,omitempty"`
	Scope      broker.LeaseCooldownScope `json:"scope,omitempty"`
	RetryAt    int64                     `json:"retryAt,omitempty"`
}

type tenantCredentialLeasePersistenceError struct {
	err error
}

func (e *tenantCredentialLeasePersistenceError) Error() string { return e.err.Error() }
func (e *tenantCredentialLeasePersistenceError) Unwrap() error { return e.err }

func newTenantCredentialLeaseStore() *tenantCredentialLeaseStore {
	return &tenantCredentialLeaseStore{leases: map[string]tenantCredentialLease{}}
}

func (s *tenantCredentialLeaseStore) handleIssue(
	server *Server,
	t tenant.Tenant,
	w http.ResponseWriter,
	r *http.Request,
) {
	var input tenantCredentialLeaseRequest
	if err := decodeTenantCredentialLeaseJSON(w, r, &input); err != nil {
		http.Error(w, "invalid credential lease request", http.StatusBadRequest)
		return
	}
	input.normalize()
	provider := accounts.Provider(input.Provider)
	if provider != accounts.ProviderCodex && provider != accounts.ProviderClaude {
		http.Error(w, "unsupported credential lease provider", http.StatusBadRequest)
		return
	}
	authMode := accounts.AuthMode(input.RequiredAuthMode)
	if authMode != "" && authMode != accounts.AuthModeOAuth && authMode != accounts.AuthModeAPIKey {
		http.Error(w, "unsupported credential lease auth mode", http.StatusBadRequest)
		return
	}
	if input.SessionID == "" || len(input.SessionID) > 512 ||
		len(input.AgentType) > 64 || len(input.UserEmail) > 320 ||
		len(input.PreferAccountID) > 512 || len(input.Model) > 256 {
		http.Error(w, "invalid credential lease request", http.StatusBadRequest)
		return
	}

	account, generation, err := selectTenantCredentialLeaseAccount(
		r.Context(), server, provider, authMode, input,
	)
	if err != nil {
		var persistenceError *tenantCredentialLeasePersistenceError
		if errors.As(err, &persistenceError) {
			if server.Logger != nil {
				server.Logger.Error("credential lease session persistence failed", "error", err)
			}
			http.Error(w, "credential lease unavailable", http.StatusInternalServerError)
			return
		}
		http.Error(w, "credential lease unavailable", http.StatusServiceUnavailable)
		return
	}
	issuedAt := time.Now().UTC()
	expiresAt := issuedAt.Add(tenantCredentialLeaseTTL)
	leaseID, err := newTenantCredentialLeaseID()
	if err != nil {
		http.Error(w, "credential lease unavailable", http.StatusInternalServerError)
		return
	}
	lease := tenantCredentialLease{
		accountID: account.ID, provider: provider, authMode: account.AuthMode,
		credentialGeneration: generation, model: input.Model, expiresAt: expiresAt,
	}
	s.put(leaseID, lease, issuedAt)
	writeJSON(w, map[string]any{
		"teamId": t.ID,
		"lease": map[string]any{
			"leaseId": leaseID, "accountId": account.ID,
			"provider": provider, "authMode": account.AuthMode,
			"token": account.Token, "providerAccountId": account.AccountID,
			"label": account.Label, "email": account.Email,
			"credentialGeneration": generation,
			"issuedAt":             issuedAt.Format(time.RFC3339Nano),
			"expiresAt":            expiresAt.Format(time.RFC3339Nano),
		},
	})
}

func (s *tenantCredentialLeaseStore) handleReport(
	server *Server,
	w http.ResponseWriter,
	r *http.Request,
) {
	const prefix = "/_subrouter/leases/"
	leaseID := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, prefix), "/events")
	if leaseID == "" || strings.Contains(leaseID, "/") {
		http.NotFound(w, r)
		return
	}
	lease, ok := s.get(leaseID, time.Now())
	if !ok {
		http.NotFound(w, r)
		return
	}
	var report tenantCredentialLeaseReport
	if err := decodeTenantCredentialLeaseJSON(w, r, &report); err != nil ||
		!validTenantCredentialLeaseOutcome(report.Outcome) ||
		report.StatusCode < 0 || report.StatusCode > 999 {
		http.Error(w, "invalid credential lease report", http.StatusBadRequest)
		return
	}
	applyTenantCredentialLeaseReport(server, lease, report)
	if report.Outcome == broker.LeaseUnauthorized ||
		report.Outcome == broker.LeaseForbidden ||
		report.Outcome == broker.LeaseRateLimited {
		s.delete(leaseID)
	}
	w.WriteHeader(http.StatusNoContent)
}

func (input *tenantCredentialLeaseRequest) normalize() {
	input.Provider = strings.ToLower(strings.TrimSpace(input.Provider))
	input.RequiredAuthMode = strings.ToLower(strings.TrimSpace(input.RequiredAuthMode))
	input.AgentType = session.NormalizeAgentType(input.AgentType)
	input.SessionID = strings.TrimSpace(input.SessionID)
	input.UserEmail = session.NormalizeUserEmail(input.UserEmail)
	input.PreferAccountID = session.NormalizeAccountID(input.PreferAccountID)
	input.Model = session.NormalizeModel(input.Model)
}

func selectTenantCredentialLeaseAccount(
	ctx context.Context,
	server *Server,
	provider accounts.Provider,
	requiredAuthMode accounts.AuthMode,
	input tenantCredentialLeaseRequest,
) (accounts.Account, int, error) {
	available, generation := server.accountListSnapshotContext(ctx)
	if generation == 0 {
		generation = 1
	}
	available = filterAccountsForProvider(available, provider)
	if requiredAuthMode != "" {
		filtered := available[:0]
		for _, candidate := range available {
			if candidate.AuthMode == requiredAuthMode {
				filtered = append(filtered, candidate)
			}
		}
		available = filtered
	}
	if len(available) == 0 {
		return accounts.Account{}, 0, fmt.Errorf("no %s accounts available", provider)
	}

	tried := make(map[string]struct{}, len(available))
	for len(tried) < len(available) {
		account, err := pickTenantCredentialLeaseAccount(server, available, tried, input)
		if err != nil {
			return accounts.Account{}, 0, err
		}
		tried[account.ID] = struct{}{}
		refreshed, err := server.refreshAccount(ctx, account)
		if err != nil {
			server.markAccountExhaustedRefreshFailure(provider, account.ID, "", err)
			continue
		}
		if strings.TrimSpace(refreshed.Token) == "" {
			server.markAccountExhaustedCredential(provider, account.ID, "")
			continue
		}
		if server.Sessions != nil {
			agentType := input.AgentType
			if agentType == "" {
				agentType = string(provider)
			}
			if _, err := server.Sessions.Put(agentType, input.SessionID, refreshed.ID, input.UserEmail); err != nil {
				return accounts.Account{}, 0, &tenantCredentialLeasePersistenceError{err: err}
			}
		}
		return refreshed, int(generation), nil
	}
	return accounts.Account{}, 0, fmt.Errorf("no usable %s credential", provider)
}

func pickTenantCredentialLeaseAccount(
	server *Server,
	available []accounts.Account,
	tried map[string]struct{},
	input tenantCredentialLeaseRequest,
) (accounts.Account, error) {
	candidates := make([]accounts.Account, 0, len(available))
	for _, candidate := range available {
		if _, seen := tried[candidate.ID]; !seen {
			candidates = append(candidates, candidate)
		}
	}
	if len(candidates) == 0 {
		return accounts.Account{}, errors.New("no untried credential accounts")
	}
	if input.PreferAccountID != "" {
		if preferred, ok := findAccount(candidates, input.PreferAccountID); ok {
			return preferred, nil
		}
	}
	if server.Sessions != nil {
		agentType := input.AgentType
		if agentType == "" {
			agentType = input.Provider
		}
		if assignment, ok := server.Sessions.Get(agentType, input.SessionID); ok {
			if sticky, found := findAccount(candidates, assignment.AccountID); found {
				return sticky, nil
			}
		}
	}
	model := input.Model
	if accounts.Provider(input.Provider) == accounts.ProviderClaude {
		model = claudePoolModel(model)
	}
	scheduler := server.scheduler().ForModel(model)
	if server.Sessions != nil {
		scheduler = scheduler.WithSessionCounts(server.Sessions.CountByAccount())
	}
	return scheduler.Pick(candidates)
}

func applyTenantCredentialLeaseReport(
	server *Server,
	lease tenantCredentialLease,
	report tenantCredentialLeaseReport,
) {
	if server.SchedulerRef == nil {
		return
	}
	switch report.Outcome {
	case broker.LeaseUnauthorized, broker.LeaseForbidden:
		server.markAccountExhaustedCredential(lease.provider, lease.accountID, "")
	case broker.LeaseRateLimited:
		poolKey := ""
		if report.Scope == broker.LeaseCooldownQuota {
			poolKey = lease.model
			if lease.provider == accounts.ProviderClaude {
				poolKey = claudePoolModel(poolKey)
			}
		}
		until := time.Time{}
		if report.RetryAt > 0 {
			until = time.UnixMilli(report.RetryAt)
		}
		now := time.Now()
		if !until.After(now) {
			until = now.Add(5 * time.Minute)
		}
		if maximum := now.Add(8 * 24 * time.Hour); until.After(maximum) {
			until = maximum
		}
		server.SchedulerRef.MarkExhaustedUntil(lease.provider, lease.accountID, poolKey, until)
	}
}

func decodeTenantCredentialLeaseJSON(w http.ResponseWriter, r *http.Request, out any) error {
	r.Body = http.MaxBytesReader(w, r.Body, tenantCredentialLeaseMaxBytes)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(out); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("credential lease body must contain one JSON value")
	}
	return nil
}

func validTenantCredentialLeaseOutcome(outcome broker.LeaseOutcome) bool {
	switch outcome {
	case broker.LeaseSuccess, broker.LeaseUnauthorized, broker.LeaseForbidden,
		broker.LeaseRateLimited, broker.LeaseProviderError:
		return true
	default:
		return false
	}
}

func newTenantCredentialLeaseID() (string, error) {
	raw := make([]byte, 18)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return "lease_" + base64.RawURLEncoding.EncodeToString(raw), nil
}

func (s *tenantCredentialLeaseStore) put(id string, lease tenantCredentialLease, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked(now)
	if len(s.leases) >= tenantCredentialLeaseMax {
		oldestID := ""
		var oldestExpiry time.Time
		for candidateID, candidate := range s.leases {
			if oldestID == "" || candidate.expiresAt.Before(oldestExpiry) {
				oldestID = candidateID
				oldestExpiry = candidate.expiresAt
			}
		}
		delete(s.leases, oldestID)
	}
	s.leases[id] = lease
}

func (s *tenantCredentialLeaseStore) get(id string, now time.Time) (tenantCredentialLease, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked(now)
	lease, ok := s.leases[id]
	return lease, ok
}

func (s *tenantCredentialLeaseStore) delete(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.leases, id)
}

func (s *tenantCredentialLeaseStore) pruneLocked(now time.Time) {
	for id, lease := range s.leases {
		if !now.Before(lease.expiresAt) {
			delete(s.leases, id)
		}
	}
}
