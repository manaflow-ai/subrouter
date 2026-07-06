package proxy

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/internal/selectacct"
	"github.com/manaflow-ai/subrouter/internal/session"
)

func TestRetryableUpstreamPostRequestClaudeMessages(t *testing.T) {
	post := func(path string) *http.Request {
		req, err := http.NewRequest(http.MethodPost, "http://example.com"+path, strings.NewReader("{}"))
		if err != nil {
			t.Fatal(err)
		}
		return req
	}
	if !retryableUpstreamPostRequest(accounts.ProviderClaude, post("/v1/messages")) {
		t.Fatal("claude POST /v1/messages should be retryable")
	}
	if !retryableUpstreamPostRequest(accounts.ProviderClaude, post("/messages")) {
		t.Fatal("claude POST /messages should be retryable")
	}
	if retryableUpstreamPostRequest(accounts.ProviderClaude, post("/v1/messages/count_tokens")) {
		t.Fatal("count_tokens should not be retryable")
	}
	if !retryableUpstreamPostRequest(accounts.ProviderCodex, post("/v1/responses")) {
		t.Fatal("codex POST /v1/responses should stay retryable")
	}
	if retryableUpstreamPostRequest(accounts.ProviderCodex, post("/v1/messages")) {
		t.Fatal("codex POST /v1/messages should not be retryable")
	}
}

type stubRoundTripper struct {
	calls     int
	responses func(req *http.Request) *http.Response
}

func (s *stubRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	s.calls++
	return s.responses(req), nil
}

func claudeFailoverServer(t *testing.T) (Server, *session.Store) {
	t.Helper()
	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	server := Server{
		Accounts: []accounts.Account{
			{ID: "cooked@example.com", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "tok-cooked"},
			{ID: "fresh@example.com", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "tok-fresh"},
		},
		Sessions:     store,
		SchedulerRef: selectacct.NewSchedulerRef(selectacct.NewScheduler(nil)),
		ScoreAccounts: func(ctx context.Context, available []accounts.Account) ([]selectacct.Score, int) {
			scores := make([]selectacct.Score, 0, len(available))
			for _, account := range available {
				headroom := 1.0
				if account.ID == "cooked@example.com" {
					headroom = 0
				}
				scores = append(scores, selectacct.Score{AccountID: account.ID, Headroom: headroom, ShortHeadroom: headroom})
			}
			return scores, len(scores)
		},
	}
	return server, store
}

func TestUsageLimitRetryTransportClaudeFailsOverOn429(t *testing.T) {
	server, store := claudeFailoverServer(t)
	if _, err := store.Put("claude", "session-1", "cooked@example.com", ""); err != nil {
		t.Fatal(err)
	}
	stub := &stubRoundTripper{responses: func(req *http.Request) *http.Response {
		auth := req.Header.Get("Authorization")
		if strings.Contains(auth, "tok-cooked") {
			// Genuine quota exhaustion: Anthropic stamps the unified-status
			// rejected header, which is what marks the account exhausted.
			h := http.Header{}
			h.Set("Anthropic-Ratelimit-Unified-Status", "rejected")
			return &http.Response{
				StatusCode: http.StatusTooManyRequests,
				Body:       io.NopCloser(strings.NewReader(`{"type":"error","error":{"type":"rate_limit_error","message":"You've hit your session limit"}}`)),
				Header:     h,
			}
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(`{"id":"msg_1"}`)),
			Header:     http.Header{},
		}
	}}
	transport := usageLimitRetryTransport{
		base:        stub,
		server:      &server,
		provider:    accounts.ProviderClaude,
		agent:       "claude",
		session:     "session-1",
		account:     "cooked@example.com",
		method:      http.MethodPost,
		path:        "/v1/messages",
		maxAttempts: 3,
	}
	req, err := http.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages", bytes.NewReader([]byte(`{"model":"claude-opus-4-8"}`)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer tok-cooked")
	response, err := transport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 after failover", response.StatusCode)
	}
	if stub.calls != 2 {
		t.Fatalf("upstream calls = %d, want 2 (429 then retry)", stub.calls)
	}
	if !server.SchedulerRef.Get().Exhausted(accounts.ProviderClaude, "cooked@example.com") {
		t.Fatal("429 should mark the cooked account exhausted")
	}
	assignment, ok := store.Get("claude", "session-1")
	if !ok || assignment.AccountID != "fresh@example.com" {
		t.Fatalf("sticky assignment = %+v, want moved to fresh@example.com", assignment)
	}
}

func TestUsageLimitRetryTransportClaudeFailsOverOn401(t *testing.T) {
	server, store := claudeFailoverServer(t)
	if _, err := store.Put("claude", "session-1", "cooked@example.com", ""); err != nil {
		t.Fatal(err)
	}
	// A dead/expired Claude OAuth token yields a plain 401 authentication_error.
	// subrouter must fail over to a healthy account instead of returning 401.
	stub := &stubRoundTripper{responses: func(req *http.Request) *http.Response {
		auth := req.Header.Get("Authorization")
		if strings.Contains(auth, "tok-cooked") {
			return &http.Response{
				StatusCode: http.StatusUnauthorized,
				Body:       io.NopCloser(strings.NewReader(`{"type":"error","error":{"type":"authentication_error","message":"Invalid authentication credentials"}}`)),
				Header:     http.Header{},
			}
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(`{"id":"msg_1"}`)),
			Header:     http.Header{},
		}
	}}
	transport := usageLimitRetryTransport{
		base:        stub,
		server:      &server,
		provider:    accounts.ProviderClaude,
		agent:       "claude",
		session:     "session-1",
		account:     "cooked@example.com",
		method:      http.MethodPost,
		path:        "/v1/messages",
		maxAttempts: 3,
	}
	req, err := http.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages", bytes.NewReader([]byte(`{"model":"claude-opus-4-8"}`)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer tok-cooked")
	response, err := transport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 after 401 failover", response.StatusCode)
	}
	if stub.calls != 2 {
		t.Fatalf("upstream calls = %d, want 2 (401 then retry)", stub.calls)
	}
	if !server.SchedulerRef.Get().Exhausted(accounts.ProviderClaude, "cooked@example.com") {
		t.Fatal("401 should drop the dead-token account from selection")
	}
	assignment, ok := store.Get("claude", "session-1")
	if !ok || assignment.AccountID != "fresh@example.com" {
		t.Fatalf("sticky assignment = %+v, want moved to fresh@example.com", assignment)
	}
}

func TestUsageLimitRetryTransportClaudeFallsBackToAPIKey(t *testing.T) {
	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	server := Server{
		Accounts: []accounts.Account{
			{ID: "cooked@example.com", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "tok-cooked"},
			{ID: "claude:team-key", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeAPIKey, Token: "api-key"},
		},
		Sessions: store,
		SchedulerRef: selectacct.NewSchedulerRef(selectacct.NewScheduler([]selectacct.Score{
			{AccountID: "cooked@example.com", Provider: accounts.ProviderClaude, Headroom: 0, ShortHeadroom: 0},
		})),
	}
	if _, err := store.Put("claude", "session-1", "cooked@example.com", ""); err != nil {
		t.Fatal(err)
	}
	var sawAPIKey bool
	stub := &stubRoundTripper{responses: func(req *http.Request) *http.Response {
		if req.Header.Get("X-Api-Key") == "api-key" {
			sawAPIKey = true
			if got := req.Header.Get("Authorization"); got != "" {
				t.Fatalf("Authorization leaked on API-key retry: %q", got)
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader(`{"id":"msg_1"}`)),
				Header:     http.Header{},
			}
		}
		h := http.Header{}
		h.Set("Anthropic-Ratelimit-Unified-Status", "rejected")
		return &http.Response{
			StatusCode: http.StatusTooManyRequests,
			Body:       io.NopCloser(strings.NewReader(`{"type":"error","error":{"type":"rate_limit_error"}}`)),
			Header:     h,
		}
	}}
	transport := usageLimitRetryTransport{
		base:        stub,
		server:      &server,
		provider:    accounts.ProviderClaude,
		agent:       "claude",
		session:     "session-1",
		account:     "cooked@example.com",
		method:      http.MethodPost,
		path:        "/v1/messages",
		maxAttempts: 3,
	}
	req, err := http.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages", bytes.NewReader([]byte(`{"model":"claude-opus-4-8"}`)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer tok-cooked")
	response, err := transport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 after API-key fallback", response.StatusCode)
	}
	if !sawAPIKey {
		t.Fatal("Claude API-key fallback was not used")
	}
	assignment, ok := store.Get("claude", "session-1")
	if !ok || assignment.AccountID != "claude:team-key" {
		t.Fatalf("sticky assignment = %+v, want moved to claude:team-key", assignment)
	}
}

func TestAccountForSessionProviderClaudeRefusesExhaustedOAuthWithoutFallback(t *testing.T) {
	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	server := Server{
		Accounts: []accounts.Account{{
			ID:       "cooked@example.com",
			Provider: accounts.ProviderClaude,
			AuthMode: accounts.AuthModeOAuth,
			Token:    "tok-cooked",
		}},
		Sessions: store,
		SchedulerRef: selectacct.NewSchedulerRef(selectacct.NewScheduler([]selectacct.Score{
			{AccountID: "cooked@example.com", Provider: accounts.ProviderClaude, Headroom: 0, ShortHeadroom: 0},
		})),
		MaxBodyBytes: 1024,
	}
	req, err := http.NewRequest(http.MethodPost, "https://subrouter.test/v1/messages", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Subrouter-Agent", "claude")
	req.Header.Set("X-Subrouter-Session", "session-1")
	if _, _, _, err := server.accountForSessionProvider(accounts.ProviderClaude, "claude", "session-1", req); err == nil {
		t.Fatal("expected exhausted Claude OAuth selection to be refused")
	}
	if _, ok := store.Get("claude", "session-1"); ok {
		t.Fatal("exhausted account should not be assigned to a fresh Claude session")
	}
}

func TestCaptureResponseBodyClaude401MarksExhausted(t *testing.T) {
	server, _ := claudeFailoverServer(t)
	response := &http.Response{
		StatusCode: http.StatusUnauthorized,
		Body:       io.NopCloser(strings.NewReader(`{"type":"error","error":{"type":"authentication_error"}}`)),
		Header:     http.Header{},
	}
	server.captureResponseBody(response, "claude", "session-1", "cooked@example.com", accounts.ProviderClaude, "", "/v1/messages")
	if !server.SchedulerRef.Get().Exhausted(accounts.ProviderClaude, "cooked@example.com") {
		t.Fatal("claude 401 should mark the account exhausted via passive inspection")
	}
}

func TestReuseStickyAssignmentClaudeExhausted(t *testing.T) {
	server, _ := claudeFailoverServer(t)
	cooked := accounts.Account{ID: "cooked@example.com", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth}
	scheduler := selectacct.NewScheduler([]selectacct.Score{
		{AccountID: "cooked@example.com", Provider: accounts.ProviderClaude, Headroom: 0, ShortHeadroom: 0},
		{AccountID: "fresh@example.com", Provider: accounts.ProviderClaude, Headroom: 1, ShortHeadroom: 1},
	})
	if server.reuseStickyAssignment("claude", "session-1", cooked, scheduler) {
		t.Fatal("exhausted claude account should not keep its sticky session")
	}
	fresh := accounts.Account{ID: "fresh@example.com", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth}
	if !server.reuseStickyAssignment("claude", "session-1", fresh, scheduler) {
		t.Fatal("healthy claude account should keep its sticky session")
	}
}

func TestCaptureResponseBodyClaude429MarksExhausted(t *testing.T) {
	server, _ := claudeFailoverServer(t)
	// A quota 429 carries the authoritative rejected header; that is what marks
	// the account exhausted (a bare 429 does not, see
	// TestCaptureResponseBodyClaudeBare429DoesNotPoison).
	h := http.Header{}
	h.Set("Anthropic-Ratelimit-Unified-Status", "rejected")
	response := &http.Response{
		StatusCode: http.StatusTooManyRequests,
		Body:       io.NopCloser(strings.NewReader(`{"type":"error","error":{"type":"rate_limit_error"}}`)),
		Header:     h,
	}
	server.captureResponseBody(response, "claude", "session-1", "cooked@example.com", accounts.ProviderClaude, "", "/v1/messages")
	if !server.SchedulerRef.Get().Exhausted(accounts.ProviderClaude, "cooked@example.com") {
		t.Fatal("claude rejected-header 429 should mark the account exhausted via passive inspection")
	}
}

// TestCaptureResponseBodyClaudeBare429DoesNotPoison is the production
// regression: healthy accounts return a headerless rate_limit_error 429 under a
// short-window burst; that must NOT mark them exhausted (poisoning the scheduler).
func TestCaptureResponseBodyClaudeBare429DoesNotPoison(t *testing.T) {
	server, _ := claudeFailoverServer(t)
	response := &http.Response{
		StatusCode: http.StatusTooManyRequests,
		Body:       io.NopCloser(strings.NewReader(`{"type":"error","error":{"type":"rate_limit_error","message":"Error"}}`)),
		Header:     http.Header{},
	}
	server.captureResponseBody(response, "claude", "session-1", "healthy@example.com", accounts.ProviderClaude, "", "/v1/messages")
	if server.SchedulerRef.Get().Exhausted(accounts.ProviderClaude, "healthy@example.com") {
		t.Fatal("a bare (headerless) 429 must NOT mark a healthy account exhausted")
	}
}

func TestRefreshUsageScoresCoversAllProviders(t *testing.T) {
	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	var captured []accounts.Account
	server := Server{
		Accounts: []accounts.Account{
			{ID: "codex@example.com", Provider: accounts.ProviderCodex, AuthMode: accounts.AuthModeOAuth},
			{ID: "claude@example.com", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth},
			{ID: "apikey", Provider: accounts.ProviderCodex, AuthMode: accounts.AuthModeAPIKey},
		},
		Sessions:      store,
		SchedulerRef:  selectacct.NewSchedulerRef(selectacct.NewScheduler(nil)),
		UsageScoreTTL: time.Minute,
		ScoreAccounts: func(ctx context.Context, available []accounts.Account) ([]selectacct.Score, int) {
			captured = available
			return []selectacct.Score{{AccountID: "claude@example.com", Provider: accounts.ProviderClaude, Headroom: 0, ShortHeadroom: 0}}, 1
		},
	}
	server.SchedulerRef.SetUpdatedAt(time.Now().Add(-time.Hour))
	server.refreshUsageScoresIfStale(context.Background())
	ids := make(map[string]bool, len(captured))
	for _, account := range captured {
		ids[account.ID] = true
	}
	if !ids["codex@example.com"] || !ids["claude@example.com"] {
		t.Fatalf("refresh scored %v, want both codex and claude OAuth accounts", ids)
	}
	if ids["apikey"] {
		t.Fatal("API-key accounts should not be usage-scored")
	}
	if !server.SchedulerRef.Get().Exhausted(accounts.ProviderClaude, "claude@example.com") {
		t.Fatal("claude usage scores should make Exhausted() real")
	}
}

// Anthropic can disable OAuth for one account's org mid-flight (403
// permission_error "OAuth authentication is currently not allowed for this
// organization", observed live 2026-07-04): the account must fail over and be
// marked exhausted with the credential TTL, not black-hole its sticky sessions.
func TestUsageLimitRetryTransportClaudeFailsOverOn403OrgDisabled(t *testing.T) {
	server, store := claudeFailoverServer(t)
	if _, err := store.Put("claude", "session-403", "cooked@example.com", ""); err != nil {
		t.Fatal(err)
	}
	stub := &stubRoundTripper{responses: func(req *http.Request) *http.Response {
		auth := req.Header.Get("Authorization")
		if strings.Contains(auth, "tok-cooked") {
			return &http.Response{
				StatusCode: http.StatusForbidden,
				Body:       io.NopCloser(strings.NewReader(`{"type":"error","error":{"type":"permission_error","message":"OAuth authentication is currently not allowed for this organization."}}`)),
				Header:     http.Header{},
			}
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(`{"id":"msg_1"}`)),
			Header:     http.Header{},
		}
	}}
	transport := usageLimitRetryTransport{
		base:        stub,
		server:      &server,
		provider:    accounts.ProviderClaude,
		agent:       "claude",
		session:     "session-403",
		account:     "cooked@example.com",
		method:      http.MethodPost,
		path:        "/v1/messages",
		maxAttempts: 3,
	}
	req, err := http.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages", bytes.NewReader([]byte(`{"model":"claude-opus-4-8"}`)))
	if err != nil {
		t.Fatal(err)
	}
	req.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader([]byte(`{"model":"claude-opus-4-8"}`))), nil
	}
	req.Header.Set("Authorization", "Bearer tok-cooked")
	response, err := transport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 after failing over the org-disabled account", response.StatusCode)
	}
	if stub.calls != 2 {
		t.Fatalf("upstream calls = %d, want 2 (403 then retry on healthy account)", stub.calls)
	}
	if !server.SchedulerRef.Get().Exhausted(accounts.ProviderClaude, "cooked@example.com") {
		t.Fatal("an org-disabled 403 should mark the account exhausted (credential TTL)")
	}
	assignment, ok := store.Get("claude", "session-403")
	if !ok || assignment.AccountID != "fresh@example.com" {
		t.Fatalf("sticky assignment = %+v, want moved off the org-disabled account", assignment)
	}
}
