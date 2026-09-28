package proxy

import (
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/session"
)

// RetryStatus is the client-visible state of one request waiting before its
// next upstream attempt. It omits the session identity because the
// authenticated /_subrouter/sessions row already supplies it.
type RetryStatus struct {
	Provider    accounts.Provider `json:"provider"`
	Model       string            `json:"model,omitempty"`
	AccountID   string            `json:"account_id,omitempty"`
	Attempt     int               `json:"attempt"`
	Reason      string            `json:"reason"`
	NextRetryAt time.Time         `json:"next_retry_at"`
}

// retryHealthStatus is the deliberately redacted form returned by the public
// health endpoint. Model, account, and session identifiers remain on the
// authenticated sessions endpoint.
type retryHealthStatus struct {
	Provider    accounts.Provider `json:"provider"`
	Attempt     int               `json:"attempt"`
	Reason      string            `json:"reason"`
	NextRetryAt time.Time         `json:"next_retry_at"`
}

type activeRetryStatus struct {
	RetryStatus
	agentType string
	sessionID string
	id        uint64
}

type retryStatusRegistry struct {
	mu     sync.Mutex
	nextID uint64
	active map[uint64]activeRetryStatus
}

func newRetryStatusRegistry() *retryStatusRegistry {
	return &retryStatusRegistry{active: make(map[uint64]activeRetryStatus)}
}

// begin registers one backoff wait and returns its idempotent release. The
// registry describes waits, not in-flight upstream attempts, so callers release
// immediately after the sleep ends as well as on cancellation.
func (r *retryStatusRegistry) begin(agentType, sessionID string, status RetryStatus) func() {
	if r == nil {
		return func() {}
	}
	status.Model = strings.TrimSpace(status.Model)
	status.AccountID = strings.TrimSpace(status.AccountID)
	status.Reason = strings.TrimSpace(status.Reason)
	status.NextRetryAt = status.NextRetryAt.UTC()
	if status.Attempt < 1 {
		status.Attempt = 1
	}
	r.mu.Lock()
	r.nextID++
	id := r.nextID
	r.active[id] = activeRetryStatus{
		RetryStatus: status,
		agentType:   session.NormalizeAgentType(agentType),
		sessionID:   session.StickySessionID(agentType, strings.TrimSpace(sessionID)),
		id:          id,
	}
	r.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			r.mu.Lock()
			delete(r.active, id)
			r.mu.Unlock()
		})
	}
}

func (r *retryStatusRegistry) forSession(agentType, sessionID string) *RetryStatus {
	if r == nil {
		return nil
	}
	agentType = session.NormalizeAgentType(agentType)
	sessionID = session.StickySessionID(agentType, strings.TrimSpace(sessionID))
	r.mu.Lock()
	defer r.mu.Unlock()
	var best *activeRetryStatus
	for id := range r.active {
		candidate := r.active[id]
		if candidate.agentType != agentType || candidate.sessionID != sessionID {
			continue
		}
		if best == nil || candidate.NextRetryAt.After(best.NextRetryAt) ||
			(candidate.NextRetryAt.Equal(best.NextRetryAt) && candidate.id > best.id) {
			copy := candidate
			best = &copy
		}
	}
	if best == nil {
		return nil
	}
	status := best.RetryStatus
	return &status
}

func (r *retryStatusRegistry) healthSnapshot() []retryHealthStatus {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	statuses := make([]activeRetryStatus, 0, len(r.active))
	for _, status := range r.active {
		statuses = append(statuses, status)
	}
	r.mu.Unlock()
	sort.Slice(statuses, func(i, j int) bool {
		if !statuses[i].NextRetryAt.Equal(statuses[j].NextRetryAt) {
			return statuses[i].NextRetryAt.Before(statuses[j].NextRetryAt)
		}
		return statuses[i].id < statuses[j].id
	})
	out := make([]retryHealthStatus, 0, len(statuses))
	for _, status := range statuses {
		out = append(out, retryHealthStatus{
			Provider:    status.Provider,
			Attempt:     status.Attempt,
			Reason:      status.Reason,
			NextRetryAt: status.NextRetryAt,
		})
	}
	return out
}

func (s *Server) beginRetryWait(agentType, sessionID string, status RetryStatus) func() {
	if s == nil || s.retryStatuses == nil {
		return func() {}
	}
	return s.retryStatuses.begin(agentType, sessionID, status)
}
