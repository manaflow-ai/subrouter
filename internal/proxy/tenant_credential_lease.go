package proxy

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
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
	"github.com/manaflow-ai/subrouter/selectacct"
	"github.com/manaflow-ai/subrouter/session"
)

const (
	tenantCredentialLeaseTTL              = 5 * time.Minute
	tenantCredentialLeaseSessionTTL       = time.Hour
	tenantCredentialLeaseMax              = 4096
	tenantCredentialLeaseIssueMaxAttempts = 2
	tenantCredentialLeaseMaxBytes         = 64 << 10
	// Tenant reports are useful for immediate failover but are not attested.
	// Their effect is session-local and time-bounded; central usage refresh
	// remains authoritative for shared routing state.
	tenantCredentialLeaseReportDefaultCooldown = 5 * time.Minute
	tenantCredentialLeaseReportMaxCooldown     = 15 * time.Minute
	// Model-less requests (for example, provider model-catalog calls) need a
	// real pool key: the session-local avoidance map reserves an empty pool for
	// account-wide avoidance within that one session.
	tenantCredentialLeaseUnspecifiedModelPool = "subroutertenantunspecifiedmodel"
)

type tenantCredentialLeaseStore struct {
	mu         sync.Mutex
	leases     map[string]tenantCredentialLease
	avoidances map[tenantCredentialLeaseAvoidanceKey]tenantCredentialLeaseAvoidance
	sessions   map[string]tenantCredentialLeaseSession
}

type tenantCredentialLease struct {
	accountID          string
	provider           accounts.Provider
	authMode           accounts.AuthMode
	credentialIdentity string
	agentType          string
	sessionID          string
	sessionToken       string
	model              string
	expiresAt          time.Time
}

type tenantCredentialLeaseAvoidanceKey struct {
	agentType    string
	sessionID    string
	sessionToken string
	provider     accounts.Provider
	accountID    string
	poolModel    string
	credential   string
}

type tenantCredentialLeaseAvoidance struct {
	expiresAt time.Time
}

type tenantCredentialLeaseSession struct {
	agentType string
	sessionID string
	provider  accounts.Provider
	expiresAt time.Time
}

type tenantCredentialLeaseAllAvoidedError struct {
	retryAt time.Time
}

func (e *tenantCredentialLeaseAllAvoidedError) Error() string {
	return "all credential lease accounts are cooling down"
}

var errTenantCredentialLeaseNotFound = errors.New("credential lease not found")

type tenantCredentialLeaseRequest struct {
	Provider         string `json:"provider"`
	RequiredAuthMode string `json:"requiredAuthMode,omitempty"`
	AgentType        string `json:"agentType,omitempty"`
	SessionID        string `json:"sessionId"`
	UserEmail        string `json:"userEmail,omitempty"`
	PreferAccountID  string `json:"preferAccountId,omitempty"`
	ForceAccountID   string `json:"forceAccountId,omitempty"`
	Model            string `json:"model,omitempty"`
	SessionToken     string `json:"sessionToken,omitempty"`
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
	return &tenantCredentialLeaseStore{
		leases:     map[string]tenantCredentialLease{},
		avoidances: map[tenantCredentialLeaseAvoidanceKey]tenantCredentialLeaseAvoidance{},
		sessions:   map[string]tenantCredentialLeaseSession{},
	}
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
	forceAccountRequested := strings.TrimSpace(input.ForceAccountID) != ""
	input.normalize()
	provider := accounts.Provider(input.Provider)
	if keyedProvider, ok := APIKeyProviderForName(input.Provider); ok {
		provider = keyedProvider
		input.Provider = string(keyedProvider)
	}
	if provider != accounts.ProviderCodex && provider != accounts.ProviderClaude &&
		provider != accounts.ProviderQwenToken && provider != accounts.ProviderQwenAnthropic {
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
		len(input.PreferAccountID) > 512 || len(input.ForceAccountID) > 512 || len(input.Model) > 256 ||
		len(input.SessionToken) > 128 || (forceAccountRequested && input.ForceAccountID == "") {
		http.Error(w, "invalid credential lease request", http.StatusBadRequest)
		return
	}
	sessionToken, err := s.resolveSessionToken(input, provider, time.Now())
	if err != nil {
		http.Error(w, "credential lease unavailable", http.StatusInternalServerError)
		return
	}
	input.SessionToken = sessionToken

	var account accounts.Account
	var generation int
	var issuedAt, expiresAt time.Time
	var leaseID string
	var retryAt time.Time
	for attempts := 0; attempts < tenantCredentialLeaseIssueMaxAttempts; attempts++ {
		var err error
		account, generation, err = selectTenantCredentialLeaseAccount(
			r.Context(), s, server, provider, authMode, input,
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
			var allAvoided *tenantCredentialLeaseAllAvoidedError
			if errors.As(err, &allAvoided) {
				setTenantCredentialLeaseRetryAfter(w, allAvoided.retryAt)
			}
			http.Error(w, "credential lease unavailable", http.StatusServiceUnavailable)
			return
		}
		issuedAt = time.Now().UTC()
		expiresAt = issuedAt.Add(tenantCredentialLeaseTTL)
		leaseID, err = newTenantCredentialLeaseID()
		if err != nil {
			http.Error(w, "credential lease unavailable", http.StatusInternalServerError)
			return
		}
		lease := tenantCredentialLease{
			accountID: account.ID, provider: schedulerAccountProvider(provider), authMode: account.AuthMode,
			credentialIdentity: account.CredentialIdentity(),
			agentType:          tenantCredentialLeaseAgentType(input, provider),
			sessionID:          input.SessionID,
			sessionToken:       input.SessionToken,
			model:              tenantCredentialLeasePoolModel(provider, input.Model, server.scheduler()),
			expiresAt:          expiresAt,
		}
		if until, avoided := s.putIfEligible(
			leaseID, lease, issuedAt, server.SchedulerRef,
		); avoided {
			if retryAt.IsZero() || until.Before(retryAt) {
				retryAt = until
			}
			leaseID = ""
			continue
		}
		break
	}
	if leaseID == "" {
		if retryAt.IsZero() {
			retryAt = time.Now().Add(tenantCredentialLeaseReportDefaultCooldown)
		}
		setTenantCredentialLeaseRetryAfter(w, retryAt)
		http.Error(w, "credential lease unavailable", http.StatusServiceUnavailable)
		return
	}
	writeJSON(w, map[string]any{
		"teamId": t.ID,
		"lease": map[string]any{
			"leaseId": leaseID, "accountId": account.ID,
			"provider": provider, "authMode": account.AuthMode,
			"token": account.Token, "providerAccountId": account.AccountID,
			"label": account.Label, "email": account.Email,
			"credentialGeneration": generation,
			"sessionToken":         input.SessionToken,
			"issuedAt":             issuedAt.Format(time.RFC3339Nano),
			"expiresAt":            expiresAt.Format(time.RFC3339Nano),
		},
	})
}

func setTenantCredentialLeaseRetryAfter(w http.ResponseWriter, retryAt time.Time) {
	delay := time.Until(retryAt)
	seconds := int64((delay + time.Second - 1) / time.Second)
	if seconds < 1 {
		seconds = 1
	}
	w.Header().Set("Retry-After", fmt.Sprintf("%d", seconds))
}

func (s *tenantCredentialLeaseStore) handleReport(
	_ *Server,
	w http.ResponseWriter,
	r *http.Request,
) {
	const prefix = "/_subrouter/leases/"
	leaseID := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, prefix), "/events")
	if leaseID == "" || strings.Contains(leaseID, "/") {
		http.NotFound(w, r)
		return
	}
	var report tenantCredentialLeaseReport
	if err := decodeTenantCredentialLeaseJSON(w, r, &report); err != nil {
		http.Error(w, "invalid credential lease report", http.StatusBadRequest)
		return
	}
	if _, err := s.consumeReport(leaseID, report, time.Now()); err != nil {
		if errors.Is(err, errTenantCredentialLeaseNotFound) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, "invalid credential lease report", http.StatusBadRequest)
		return
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
	input.ForceAccountID = session.NormalizeAccountID(input.ForceAccountID)
	input.Model = session.NormalizeModel(input.Model)
	input.SessionToken = strings.TrimSpace(input.SessionToken)
}

func selectTenantCredentialLeaseAccount(
	ctx context.Context,
	store *tenantCredentialLeaseStore,
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
	var retryAt time.Time
	for len(tried) < len(available) {
		account, err := pickTenantCredentialLeaseAccount(store, server, available, tried, input)
		if err != nil {
			var allAvoided *tenantCredentialLeaseAllAvoidedError
			if errors.As(err, &allAvoided) && !retryAt.IsZero() {
				if allAvoided.retryAt.Before(retryAt) {
					retryAt = allAvoided.retryAt
				}
				return accounts.Account{}, 0, &tenantCredentialLeaseAllAvoidedError{retryAt: retryAt}
			}
			return accounts.Account{}, 0, err
		}
		tried[account.ID] = struct{}{}
		refreshed, err := server.refreshAccount(ctx, account)
		if err != nil {
			server.markAccountExhaustedRefreshFailure(account, err)
			if until, blocked := tenantCredentialLeaseTrustedBlockedUntil(
				server, account, tenantCredentialLeasePoolModel(provider, input.Model, server.scheduler()), time.Now(),
			); blocked && (retryAt.IsZero() || until.Before(retryAt)) {
				retryAt = until
			}
			continue
		}
		if strings.TrimSpace(refreshed.Token) == "" {
			server.markAccountExhaustedCredentialForAccount(account)
			if until, blocked := tenantCredentialLeaseTrustedBlockedUntil(
				server, account, tenantCredentialLeasePoolModel(provider, input.Model, server.scheduler()), time.Now(),
			); blocked && (retryAt.IsZero() || until.Before(retryAt)) {
				retryAt = until
			}
			continue
		}
		// A 401 avoidance is bound to the rejected credential identity, not the
		// account. Let OAuth refresh run, then reject the candidate only when it
		// still carries that same credential; token repair becomes immediately
		// eligible without weakening quota/account avoidance.
		if store != nil {
			model := tenantCredentialLeasePoolModel(provider, input.Model, server.scheduler())
			if until, avoided := store.avoidanceUntil(input, refreshed, model, time.Now()); avoided {
				if retryAt.IsZero() || until.Before(retryAt) {
					retryAt = until
				}
				continue
			}
		}
		if server.Sessions != nil {
			agentType := tenantCredentialLeaseAgentType(input, provider)
			if _, err := server.Sessions.Put(agentType, input.SessionID, refreshed.ID, input.UserEmail); err != nil {
				return accounts.Account{}, 0, &tenantCredentialLeasePersistenceError{err: err}
			}
		}
		return refreshed, int(generation), nil
	}
	if !retryAt.IsZero() {
		return accounts.Account{}, 0, &tenantCredentialLeaseAllAvoidedError{retryAt: retryAt}
	}
	return accounts.Account{}, 0, fmt.Errorf("no usable %s credential", provider)
}

func pickTenantCredentialLeaseAccount(
	store *tenantCredentialLeaseStore,
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
	provider := accounts.Provider(input.Provider)
	baseScheduler := server.scheduler()
	model := tenantCredentialLeasePoolModel(provider, input.Model, baseScheduler)
	scheduler := baseScheduler.ForModel(model)
	if server.Sessions != nil {
		scheduler = scheduler.WithSessionCounts(SchedulerSessionCounts(server.Sessions))
	}
	// Forced, sticky, and preferred routing never grant permission to reuse a
	// cooled credential. Filtering must happen first; if every candidate is
	// blocked, return a retryable outage rather than defeating the cooldown.
	now := time.Now()
	eligible := candidates[:0]
	var retryAt time.Time
	for _, candidate := range candidates {
		blockedUntil := time.Time{}
		if until, blocked := tenantCredentialLeaseTrustedBlockedUntil(
			server, candidate, model, now,
		); blocked {
			blockedUntil = until
		}
		if store != nil {
			if avoidedUntil, avoided := store.selectionAvoidanceUntil(input, candidate, model, now); avoided && avoidedUntil.After(blockedUntil) {
				blockedUntil = avoidedUntil
			}
		}
		if blockedUntil.IsZero() {
			eligible = append(eligible, candidate)
			continue
		}
		if retryAt.IsZero() || blockedUntil.Before(retryAt) {
			retryAt = blockedUntil
		}
	}
	if len(eligible) == 0 {
		if retryAt.IsZero() {
			retryAt = now.Add(tenantCredentialLeaseReportDefaultCooldown)
		}
		return accounts.Account{}, &tenantCredentialLeaseAllAvoidedError{retryAt: retryAt}
	}
	candidates = eligible
	if input.ForceAccountID != "" {
		if forced, ok := findAccount(candidates, input.ForceAccountID); ok {
			return forced, nil
		}
		return accounts.Account{}, fmt.Errorf("forced account %q is unavailable", input.ForceAccountID)
	}
	if server.Sessions != nil {
		agentType := tenantCredentialLeaseAgentType(input, provider)
		if assignment, ok := server.Sessions.Get(agentType, input.SessionID); ok {
			if sticky, found := findAccount(candidates, assignment.AccountID); found {
				return sticky, nil
			}
		}
	}
	if input.PreferAccountID != "" {
		if preferred, ok := findAccount(candidates, input.PreferAccountID); ok {
			return preferred, nil
		}
	}
	return pickRoutingAccount(scheduler, candidates)
}

func tenantCredentialLeaseTrustedBlockedUntil(
	server *Server,
	account accounts.Account,
	model string,
	now time.Time,
) (time.Time, bool) {
	if server.SchedulerRef == nil {
		return time.Time{}, false
	}
	schedulerProvider := schedulerAccountProvider(account.Provider)
	if account.AuthMode == accounts.AuthModeAPIKey {
		return server.SchedulerRef.ExplicitBlockedUntilFor(
			schedulerProvider, account.ID, model, now,
		)
	}
	return server.SchedulerRef.BlockedUntilFor(
		schedulerProvider, account.ID, model, now,
	)
}

func tenantCredentialLeaseAgentType(
	input tenantCredentialLeaseRequest,
	provider accounts.Provider,
) string {
	if agentType := session.NormalizeAgentType(input.AgentType); agentType != "" {
		return agentType
	}
	return string(schedulerAccountProvider(provider))
}

func tenantCredentialLeasePoolModel(provider accounts.Provider, model string, schedulers ...selectacct.Scheduler) string {
	if strings.TrimSpace(model) == "" {
		return tenantCredentialLeaseUnspecifiedModelPool
	}
	if provider == accounts.ProviderClaude {
		model = claudePoolModel(model)
	} else if provider == accounts.ProviderAntigravity {
		if len(schedulers) > 0 {
			model = antigravityPoolModel(schedulers[0], model)
		} else {
			model = antigravityFamilyPoolModel(model)
		}
	}
	if key := selectacct.ModelKey(model); key != "" {
		return key
	}
	return tenantCredentialLeaseUnspecifiedModelPool
}

func tenantCredentialLeaseCredentialKey(identity string) string {
	sum := sha256.Sum256([]byte(identity))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// normalizeTenantCredentialLeaseReport validates the claimed provider result
// and derives its session-local blast radius. Caller scope is accepted only as
// a compatibility hint; it never controls shared scheduler state.
func normalizeTenantCredentialLeaseReport(
	lease tenantCredentialLease,
	report tenantCredentialLeaseReport,
	now time.Time,
) (tenantCredentialLeaseReport, error) {
	if !validTenantCredentialLeaseOutcome(report.Outcome) ||
		report.StatusCode < 100 || report.StatusCode > 599 ||
		report.RetryAt < 0 {
		return tenantCredentialLeaseReport{}, errors.New("invalid outcome metadata")
	}

	validStatus := false
	switch report.Outcome {
	case broker.LeaseSuccess:
		validStatus = report.StatusCode < http.StatusBadRequest
	case broker.LeaseUnauthorized:
		validStatus = report.StatusCode == http.StatusUnauthorized
	case broker.LeaseForbidden:
		validStatus = report.StatusCode == http.StatusForbidden
	case broker.LeaseRateLimited:
		// Claude can report quota rejection headers on a non-429 error response.
		// Hosted reports carry no provider headers, so require an HTTP error before
		// honoring this untrusted signal and constrain it to the issued model pool.
		validStatus = report.StatusCode == http.StatusTooManyRequests ||
			(lease.provider == accounts.ProviderClaude &&
				report.StatusCode >= http.StatusBadRequest &&
				report.StatusCode != http.StatusUnauthorized)
	case broker.LeaseProviderError:
		validStatus = report.StatusCode >= http.StatusBadRequest &&
			report.StatusCode != http.StatusUnauthorized &&
			report.StatusCode != http.StatusTooManyRequests
	}
	if !validStatus {
		return tenantCredentialLeaseReport{}, errors.New("outcome does not match status")
	}

	derivedScope := broker.LeaseCooldownScope("")
	switch report.Outcome {
	case broker.LeaseUnauthorized:
		derivedScope = broker.LeaseCooldownAccount
	case broker.LeaseForbidden:
		derivedScope = broker.LeaseCooldownQuota
		if lease.provider == accounts.ProviderClaude &&
			lease.authMode == accounts.AuthModeOAuth {
			derivedScope = broker.LeaseCooldownAccount
		}
	case broker.LeaseRateLimited:
		derivedScope = broker.LeaseCooldownAccount
		if lease.provider == accounts.ProviderClaude {
			// Header-derived Claude rejections, including 403, are accepted but
			// default to the issued model pool. The trusted proxy client can
			// preserve an account-wide header verdict explicitly; an absent scope
			// remains conservative because hosted reports carry no headers.
			derivedScope = broker.LeaseCooldownQuota
			if report.Scope == broker.LeaseCooldownAccount {
				derivedScope = broker.LeaseCooldownAccount
			}
		}
	}
	if report.Scope != "" && report.Scope != broker.LeaseCooldownQuota &&
		report.Scope != broker.LeaseCooldownAccount {
		return tenantCredentialLeaseReport{}, errors.New("cooldown scope does not match lease metadata")
	}
	if report.Outcome != broker.LeaseRateLimited && report.RetryAt != 0 {
		return tenantCredentialLeaseReport{}, errors.New("retry deadline without rate limit")
	}
	if derivedScope != "" {
		requested := now.Add(tenantCredentialLeaseReportDefaultCooldown)
		if report.Outcome == broker.LeaseRateLimited {
			if candidate := time.UnixMilli(report.RetryAt); candidate.After(now) {
				requested = candidate
			}
		}
		if maximum := now.Add(tenantCredentialLeaseReportMaxCooldown); requested.After(maximum) {
			requested = maximum
		}
		report.RetryAt = requested.UnixMilli()
	}
	report.Scope = derivedScope
	return report, nil
}

func (s *tenantCredentialLeaseStore) consumeReport(
	leaseID string,
	report tenantCredentialLeaseReport,
	now time.Time,
) (tenantCredentialLeaseReport, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked(now)
	lease, ok := s.leases[leaseID]
	if !ok {
		return tenantCredentialLeaseReport{}, errTenantCredentialLeaseNotFound
	}
	normalized, err := normalizeTenantCredentialLeaseReport(lease, report, now)
	if err != nil {
		return tenantCredentialLeaseReport{}, err
	}
	if normalized.Outcome != broker.LeaseUnauthorized &&
		normalized.Outcome != broker.LeaseForbidden &&
		normalized.Outcome != broker.LeaseRateLimited {
		return normalized, nil
	}

	key := tenantCredentialLeaseAvoidanceKey{
		agentType: lease.agentType, sessionID: lease.sessionID,
		sessionToken: lease.sessionToken,
		provider:     schedulerAccountProvider(lease.provider), accountID: lease.accountID,
	}
	if normalized.Scope == broker.LeaseCooldownQuota {
		// lease.model is canonicalized once, when the lease is issued. Re-running
		// provider normalization here is not idempotent for already-canonical
		// Claude pool keys (for example, "opus" becomes "claudeopus").
		key.poolModel = lease.model
	}
	if normalized.Outcome == broker.LeaseUnauthorized {
		key.credential = tenantCredentialLeaseCredentialKey(lease.credentialIdentity)
	}
	expiresAt := time.UnixMilli(normalized.RetryAt)
	if _, exists := s.avoidances[key]; !exists && len(s.avoidances) >= tenantCredentialLeaseMax {
		// Avoidance is untrusted, process-local telemetry. At capacity, discard
		// the oldest such hint instead of amplifying this report into a broader
		// cooldown or denying every newly issued capability.
		s.evictEarliestAvoidanceLocked()
	}
	// Consume the reported lease and every sibling for the same originating
	// session/account before publishing avoidance. One concurrent report wins;
	// replays observe not-found and cannot extend the cooldown.
	for candidateID, candidate := range s.leases {
		if candidate.agentType == lease.agentType &&
			candidate.sessionID == lease.sessionID &&
			candidate.sessionToken == lease.sessionToken &&
			schedulerAccountProvider(candidate.provider) == schedulerAccountProvider(lease.provider) &&
			candidate.accountID == lease.accountID &&
			(normalized.Scope != broker.LeaseCooldownQuota || candidate.model == lease.model) {
			delete(s.leases, candidateID)
		}
	}
	if existing, exists := s.avoidances[key]; exists {
		if expiresAt.After(existing.expiresAt) {
			existing.expiresAt = expiresAt
			s.avoidances[key] = existing
		}
	} else {
		s.avoidances[key] = tenantCredentialLeaseAvoidance{expiresAt: expiresAt}
	}
	return normalized, nil
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
	return newTenantCredentialLeaseOpaqueID("lease_")
}

func newTenantCredentialLeaseSessionToken() (string, error) {
	return newTenantCredentialLeaseOpaqueID("session_")
}

func newTenantCredentialLeaseOpaqueID(prefix string) (string, error) {
	raw := make([]byte, 18)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return prefix + base64.RawURLEncoding.EncodeToString(raw), nil
}

func (s *tenantCredentialLeaseStore) resolveSessionToken(
	input tenantCredentialLeaseRequest,
	provider accounts.Provider,
	now time.Time,
) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked(now)
	agentType := tenantCredentialLeaseAgentType(input, provider)
	provider = schedulerAccountProvider(provider)
	if existing, ok := s.sessions[input.SessionToken]; ok &&
		existing.agentType == agentType && existing.sessionID == input.SessionID &&
		existing.provider == provider {
		if renewed := now.Add(tenantCredentialLeaseSessionTTL); renewed.After(existing.expiresAt) {
			existing.expiresAt = renewed
		}
		s.sessions[input.SessionToken] = existing
		return input.SessionToken, nil
	}
	token, err := newTenantCredentialLeaseSessionToken()
	if err != nil {
		return "", err
	}
	if len(s.sessions) >= tenantCredentialLeaseMax {
		// Session capabilities are process-local optimization state. Evict one
		// entry from this table only; an in-flight publication can recreate it.
		s.evictEarliestSessionLocked()
	}
	s.sessions[token] = tenantCredentialLeaseSession{
		agentType: agentType, sessionID: input.SessionID, provider: provider,
		expiresAt: now.Add(tenantCredentialLeaseSessionTTL),
	}
	return token, nil
}

func (s *tenantCredentialLeaseStore) put(id string, lease tenantCredentialLease, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked(now)
	s.putLocked(id, lease)
}

// putIfEligible publishes a freshly selected lease only if no terminal report
// has made that account ineligible for the originating session. Holding the
// same mutex as consumeReport makes selection publication and sibling
// invalidation linearizable: either the report wins and this insert is
// rejected, or this insert wins and the report consumes it as a sibling.
func (s *tenantCredentialLeaseStore) putIfEligible(
	id string,
	lease tenantCredentialLease,
	now time.Time,
	scheduler *selectacct.SchedulerRef,
) (time.Time, bool) {
	// Lock order is lease store then SchedulerRef. No scheduler path acquires
	// this store, so an authoritative mark cannot deadlock with publication.
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked(now)
	if until, avoided := s.avoidanceUntilLeaseLocked(lease, now); avoided {
		return until, true
	}
	if scheduler != nil {
		publish := func() { s.putLocked(id, lease) }
		schedulerProvider := schedulerAccountProvider(lease.provider)
		var until time.Time
		var published bool
		if lease.authMode == accounts.AuthModeAPIKey {
			until, published = scheduler.RunIfAccountNotExplicitlyBlocked(
				schedulerProvider, lease.accountID, lease.model, now, publish,
			)
		} else {
			until, published = scheduler.RunIfAccountNotBlocked(
				schedulerProvider, lease.accountID, lease.model, now, publish,
			)
		}
		if !published {
			return until, true
		}
		// A mark can arrive after the read guard is released and before the
		// response is written. That ordinary routing race is bounded to one
		// leased probe; its terminal report then prevents reuse by this capability.
		return time.Time{}, false
	}
	s.putLocked(id, lease)
	return time.Time{}, false
}

func (s *tenantCredentialLeaseStore) putLocked(id string, lease tenantCredentialLease) {
	delete(s.leases, id)
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
	// Exactly one slot was made available above; preserve the hard global cap.
	s.leases[id] = lease
	session, ok := s.sessions[lease.sessionToken]
	if !ok && lease.sessionToken != "" {
		if len(s.sessions) >= tenantCredentialLeaseMax {
			s.evictEarliestSessionLocked()
		}
		session = tenantCredentialLeaseSession{
			agentType: lease.agentType, sessionID: lease.sessionID,
			provider: schedulerAccountProvider(lease.provider),
		}
	}
	if lease.sessionToken != "" {
		if lease.expiresAt.After(session.expiresAt) {
			session.expiresAt = lease.expiresAt
		}
		s.sessions[lease.sessionToken] = session
	}
}

func (s *tenantCredentialLeaseStore) evictEarliestSessionLocked() {
	var evictionToken string
	var evictionExpiry time.Time
	for candidateToken, candidate := range s.sessions {
		if evictionToken == "" || candidate.expiresAt.Before(evictionExpiry) {
			evictionToken, evictionExpiry = candidateToken, candidate.expiresAt
		}
	}
	if evictionToken != "" {
		delete(s.sessions, evictionToken)
	}
}

func (s *tenantCredentialLeaseStore) get(id string, now time.Time) (tenantCredentialLease, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked(now)
	lease, ok := s.leases[id]
	return lease, ok
}

func (s *tenantCredentialLeaseStore) avoidanceUntil(
	input tenantCredentialLeaseRequest,
	account accounts.Account,
	poolModel string,
	now time.Time,
) (time.Time, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked(now)
	lease := tenantCredentialLease{
		agentType: tenantCredentialLeaseAgentType(input, account.Provider),
		sessionID: input.SessionID, provider: account.Provider, accountID: account.ID,
		sessionToken: input.SessionToken,
		model:        poolModel, credentialIdentity: account.CredentialIdentity(),
	}
	return s.avoidanceUntilLeaseLocked(lease, now)
}

func (s *tenantCredentialLeaseStore) selectionAvoidanceUntil(
	input tenantCredentialLeaseRequest,
	account accounts.Account,
	poolModel string,
	now time.Time,
) (time.Time, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked(now)
	lease := tenantCredentialLease{
		agentType: tenantCredentialLeaseAgentType(input, account.Provider),
		sessionID: input.SessionID, sessionToken: input.SessionToken,
		provider: account.Provider, accountID: account.ID, model: poolModel,
	}
	return s.avoidanceUntilLeaseKeysLocked(lease, now, false)
}

func (s *tenantCredentialLeaseStore) avoidanceUntilLeaseLocked(
	lease tenantCredentialLease,
	now time.Time,
) (time.Time, bool) {
	return s.avoidanceUntilLeaseKeysLocked(lease, now, true)
}

func (s *tenantCredentialLeaseStore) avoidanceUntilLeaseKeysLocked(
	lease tenantCredentialLease,
	now time.Time,
	includeCredential bool,
) (time.Time, bool) {
	base := tenantCredentialLeaseAvoidanceKey{
		agentType: lease.agentType, sessionID: lease.sessionID,
		sessionToken: lease.sessionToken,
		provider:     schedulerAccountProvider(lease.provider), accountID: lease.accountID,
	}
	keys := []tenantCredentialLeaseAvoidanceKey{
		base,
		{
			agentType: base.agentType, sessionID: base.sessionID,
			sessionToken: base.sessionToken,
			provider:     base.provider, accountID: base.accountID,
			poolModel: lease.model,
		},
	}
	if credential := lease.credentialIdentity; includeCredential && credential != "" {
		credentialKey := base
		credentialKey.credential = tenantCredentialLeaseCredentialKey(credential)
		keys = append(keys, credentialKey)
	}
	var until time.Time
	for _, key := range keys {
		if avoidance, ok := s.avoidances[key]; ok && avoidance.expiresAt.After(until) {
			until = avoidance.expiresAt
		}
	}
	return until, until.After(now)
}

func (s *tenantCredentialLeaseStore) pruneLocked(now time.Time) {
	for id, lease := range s.leases {
		if !now.Before(lease.expiresAt) {
			delete(s.leases, id)
		}
	}
	for key, avoidance := range s.avoidances {
		if !now.Before(avoidance.expiresAt) {
			delete(s.avoidances, key)
		}
	}
	for token, session := range s.sessions {
		if !now.Before(session.expiresAt) {
			delete(s.sessions, token)
		}
	}
}

func (s *tenantCredentialLeaseStore) evictEarliestAvoidanceLocked() {
	var oldestKey tenantCredentialLeaseAvoidanceKey
	var oldest tenantCredentialLeaseAvoidance
	found := false
	for key, candidate := range s.avoidances {
		if !found || candidate.expiresAt.Before(oldest.expiresAt) {
			oldestKey, oldest, found = key, candidate, true
		}
	}
	if found {
		delete(s.avoidances, oldestKey)
	}
}
