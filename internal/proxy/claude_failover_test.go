package proxy

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	agentclaude "github.com/manaflow-ai/subrouter/internal/agents/claude"
	"github.com/manaflow-ai/subrouter/selectacct"
	"github.com/manaflow-ai/subrouter/session"
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
	if _, err := io.Copy(io.Discard, response.Body); err != nil {
		t.Fatal(err)
	}
	routed, ok := routedResponseAccount(response)
	if !ok || routed.ID != "fresh@example.com" {
		t.Fatalf("final response account = %+v, %t; want fresh account", routed, ok)
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
	if _, err := io.Copy(io.Discard, response.Body); err != nil {
		t.Fatal(err)
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
	if _, err := io.Copy(io.Discard, response.Body); err != nil {
		t.Fatal(err)
	}
	if !sawAPIKey {
		t.Fatal("Claude API-key fallback was not used")
	}
	assignment, ok := store.Get("claude", "session-1")
	if !ok || assignment.AccountID != "claude:team-key" {
		t.Fatalf("sticky assignment = %+v, want moved to claude:team-key", assignment)
	}
}

func TestAccountForSessionProviderClaudeRoutesWhenUsageScoreSaysExhausted(t *testing.T) {
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
	if _, _, _, err := server.accountForSessionProvider(accounts.ProviderClaude, "claude", "session-1", req); err != nil {
		t.Fatalf("stale exhausted Claude OAuth score should route optimistically: %v", err)
	}
	if assignment, ok := store.Get("claude", "session-1"); !ok || assignment.AccountID != "cooked@example.com" {
		t.Fatalf("Claude account should be assigned for request-time validation: %+v", assignment)
	}
}

func TestAccountForSessionProviderClaudeRejectsFreshExhaustedScore(t *testing.T) {
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
			{AccountID: "cooked@example.com", Provider: accounts.ProviderClaude, Headroom: 0, ShortHeadroom: 0, Fresh: true},
		})),
		MaxBodyBytes: 1024,
	}
	req, err := http.NewRequest(http.MethodPost, "https://subrouter.test/v1/messages", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Subrouter-Agent", "claude")
	req.Header.Set("X-Subrouter-Session", "session-fresh")
	if _, _, _, err := server.accountForSessionProvider(accounts.ProviderClaude, "claude", "session-fresh", req); err == nil {
		t.Fatal("freshly exhausted Claude account should not be routed")
	}
	if _, ok := store.Get("claude", "session-fresh"); ok {
		t.Fatal("freshly exhausted account should not be persisted")
	}
}

// A Max account whose score lacks the Opus pool that other (cooked) accounts
// expose must not be refused: the zero-filled pool score is unknown, not
// measured exhaustion. Regression for the 503 "no non-exhausted claude
// accounts" served while the only uncooked account still had quota.
func TestAccountForSessionProviderClaudeRoutesAccountMissingModelPool(t *testing.T) {
	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	opusKey := selectacct.ModelKey(claudePoolModel("claude-opus-5-5"))
	server := Server{
		Accounts: []accounts.Account{
			{ID: "cooked@example.com", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "tok-cooked"},
			{ID: "fresh@example.com", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "tok-fresh"},
		},
		Sessions: store,
		SchedulerRef: selectacct.NewSchedulerRef(selectacct.NewScheduler([]selectacct.Score{
			{AccountID: "cooked@example.com", Provider: accounts.ProviderClaude, Fresh: true,
				ModelScores: map[string]selectacct.Score{opusKey: {AccountID: "cooked@example.com", Provider: accounts.ProviderClaude, Fresh: true}}},
			{AccountID: "fresh@example.com", Provider: accounts.ProviderClaude, Headroom: 0.5, ShortHeadroom: 0.9, Fresh: true},
		})),
		MaxBodyBytes: 1024,
	}
	req, err := http.NewRequest(http.MethodPost, "https://subrouter.test/v1/messages", strings.NewReader(`{"model":"claude-opus-5-5"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Subrouter-Agent", "claude")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Subrouter-Session", "session-missing-pool")
	account, _, _, err := server.accountForSessionProvider(accounts.ProviderClaude, "claude", "session-missing-pool", req)
	if err != nil {
		t.Fatalf("account missing a model-pool score should route optimistically: %v", err)
	}
	if account.ID != "fresh@example.com" {
		t.Fatalf("routed to %q, want fresh@example.com", account.ID)
	}
}

// Live shape from 2026-09-25: a setup-token Max account (user:inference only)
// cannot read /api/oauth/usage, so its windows come from the rate-limit header
// probe and carry session, weekly, and Fable buckets but no Opus/Sonnet bucket.
// Every browser-OAuth account is weekly-cooked. Opus requests must route to the
// setup-token account on its session/weekly headroom, and must still refuse it
// once its measured weekly window is spent.
func TestClaudeOpusRoutesSetupTokenAccountFromLiveUsageShape(t *testing.T) {
	week := int64(7 * 24 * 60 * 60)
	cooked := scoreFromUsageWindows(accounts.ProviderClaude, "cooked@example.com", []accounts.UsageWindow{
		{Name: "5h", UsedPercent: 0, LimitWindowSeconds: 18000},
		{Name: "7d", UsedPercent: 100, LimitWindowSeconds: week},
		{Name: agentclaude.FableWindowName, UsedPercent: 13, LimitWindowSeconds: week, Feature: agentclaude.FableFeature},
		{Name: "opus-weekly", UsedPercent: 0, LimitWindowSeconds: week, Feature: agentclaude.OpusFeature},
		{Name: "sonnet-weekly", UsedPercent: 0, LimitWindowSeconds: week, Feature: agentclaude.SonnetFeature},
	})
	cooked.Fresh = true
	setupTokenWindows := func(weeklyUsed float64) selectacct.Score {
		score := scoreFromUsageWindows(accounts.ProviderClaude, "setup@example.com", []accounts.UsageWindow{
			{Name: "5h", UsedPercent: 4, LimitWindowSeconds: 18000},
			{Name: "7d", UsedPercent: weeklyUsed, LimitWindowSeconds: week},
			{Name: agentclaude.FableWindowName, UsedPercent: 16, LimitWindowSeconds: week, Feature: agentclaude.FableFeature},
		})
		score.Fresh = true
		return score
	}
	route := func(t *testing.T, setup selectacct.Score) (accounts.Account, error) {
		t.Helper()
		store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
		if err != nil {
			t.Fatal(err)
		}
		server := Server{
			Accounts: []accounts.Account{
				{ID: "cooked@example.com", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "tok-cooked"},
				{ID: "setup@example.com", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "tok-setup"},
			},
			Sessions:     store,
			SchedulerRef: selectacct.NewSchedulerRef(selectacct.NewScheduler([]selectacct.Score{cooked, setup})),
			MaxBodyBytes: 1024,
		}
		req, err := http.NewRequest(http.MethodPost, "https://subrouter.test/v1/messages", strings.NewReader(`{"model":"claude-opus-5-5"}`))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Subrouter-Agent", "claude")
		req.Header.Set("X-Subrouter-Session", "session-live")
		account, _, _, err := server.accountForSessionProvider(accounts.ProviderClaude, "claude", "session-live", req)
		return account, err
	}
	t.Run("weekly headroom left routes", func(t *testing.T) {
		account, err := route(t, setupTokenWindows(53))
		if err != nil {
			t.Fatalf("setup-token account with 47%% weekly left was refused: %v", err)
		}
		if account.ID != "setup@example.com" {
			t.Fatalf("routed to %q, want setup@example.com", account.ID)
		}
	})
	t.Run("weekly spent refuses", func(t *testing.T) {
		if account, err := route(t, setupTokenWindows(100)); err == nil {
			t.Fatalf("weekly-spent pool routed to %q; want no non-exhausted accounts", account.ID)
		}
	})
}

func TestAccountForSessionProviderClaudeReassignsRemovedSessionAccount(t *testing.T) {
	server, store := claudeFailoverServer(t)
	server.SchedulerRef = selectacct.NewSchedulerRef(selectacct.NewScheduler([]selectacct.Score{
		{AccountID: "cooked@example.com", Provider: accounts.ProviderClaude, Headroom: 0, ShortHeadroom: 0},
		{AccountID: "fresh@example.com", Provider: accounts.ProviderClaude, Headroom: 1, ShortHeadroom: 1},
	}))
	if _, err := store.Put("claude", "session-removed", "removed@example.com", ""); err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, "https://subrouter.test/v1/messages", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Subrouter-Agent", "claude")
	req.Header.Set("X-Subrouter-Session", "session-removed")
	if _, _, _, err := server.accountForSessionProvider(accounts.ProviderClaude, "claude", "session-removed", req); err != nil {
		t.Fatalf("removed Claude account should be replaced automatically: %v", err)
	}
	assignment, ok := store.Get("claude", "session-removed")
	if !ok || assignment.AccountID != "fresh@example.com" {
		t.Fatalf("removed account assignment = %+v, want fresh@example.com", assignment)
	}
}

func TestAccountForSessionProviderClaudeReassignsStickySessionWithStaleScores(t *testing.T) {
	server, store := claudeFailoverServer(t)
	server.SchedulerRef = selectacct.NewSchedulerRef(selectacct.NewScheduler([]selectacct.Score{
		{AccountID: "cooked@example.com", Provider: accounts.ProviderClaude, Headroom: 0, ShortHeadroom: 0},
		{AccountID: "fresh@example.com", Provider: accounts.ProviderClaude, Headroom: 0, ShortHeadroom: 0},
	}))
	if _, err := store.Put("claude", "session-stale", "cooked@example.com", ""); err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, "https://subrouter.test/v1/messages", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Subrouter-Agent", "claude")
	req.Header.Set("X-Subrouter-Session", "session-stale")
	if _, _, _, err := server.accountForSessionProvider(accounts.ProviderClaude, "claude", "session-stale", req); err != nil {
		t.Fatalf("stale Claude scores should still route: %v", err)
	}
	assignment, ok := store.Get("claude", "session-stale")
	if !ok || assignment.AccountID == "cooked@example.com" {
		t.Fatalf("stale sticky assignment = %+v, want reassignment", assignment)
	}
}

func TestAccountForSessionProviderClaudeRejectsExplicitlyUnavailableAccount(t *testing.T) {
	server, store := claudeFailoverServer(t)
	server.Accounts = server.Accounts[:1]
	server.SchedulerRef = selectacct.NewSchedulerRef(selectacct.NewScheduler([]selectacct.Score{
		{AccountID: "cooked@example.com", Provider: accounts.ProviderClaude, Headroom: 0, ShortHeadroom: 0},
	}))
	server.SchedulerRef.MarkAccountUnavailableUntil(accounts.ProviderClaude, "cooked@example.com", time.Now().Add(time.Hour))
	req, err := http.NewRequest(http.MethodPost, "https://subrouter.test/v1/messages", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Subrouter-Agent", "claude")
	req.Header.Set("X-Subrouter-Session", "session-unavailable")
	if _, _, _, err := server.accountForSessionProvider(accounts.ProviderClaude, "claude", "session-unavailable", req); err == nil {
		t.Fatal("explicitly unavailable Claude account should not be routed")
	}
	if _, ok := store.Get("claude", "session-unavailable"); ok {
		t.Fatal("explicitly unavailable account should not be persisted")
	}
}

func TestAccountForSessionProviderClaudeRejectsAuthoritativeExhaustionMark(t *testing.T) {
	server, store := claudeFailoverServer(t)
	server.Accounts = server.Accounts[:1]
	server.SchedulerRef = selectacct.NewSchedulerRef(selectacct.NewScheduler([]selectacct.Score{
		{AccountID: "cooked@example.com", Provider: accounts.ProviderClaude, Headroom: 0, ShortHeadroom: 0},
	}))
	server.SchedulerRef.MarkExhaustedUntil(accounts.ProviderClaude, "cooked@example.com", "", time.Now().Add(time.Hour))
	req, err := http.NewRequest(http.MethodPost, "https://subrouter.test/v1/messages", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Subrouter-Agent", "claude")
	req.Header.Set("X-Subrouter-Session", "session-marked")
	if _, _, _, err := server.accountForSessionProvider(accounts.ProviderClaude, "claude", "session-marked", req); err == nil {
		t.Fatal("authoritatively exhausted Claude account should not be routed")
	}
	if _, ok := store.Get("claude", "session-marked"); ok {
		t.Fatal("authoritatively exhausted account should not be persisted")
	}
}

func TestClaudeExtraUsageFallbackSkipsExplicitlyUnavailableAccount(t *testing.T) {
	server, _ := claudeFailoverServer(t)
	server.SchedulerRef = selectacct.NewSchedulerRef(selectacct.NewScheduler([]selectacct.Score{
		{
			AccountID: "cooked@example.com", Provider: accounts.ProviderClaude,
			Headroom: 0, ShortHeadroom: 0, WeeklyHeadroom: 0, WeeklyHeadroomKnown: true,
			ClaudeExtraUsageEnabled: true, ClaudeExtraUsageKnown: true, ClaudeExtraUsageRemaining: 0.95,
		},
		{
			AccountID: "fresh@example.com", Provider: accounts.ProviderClaude,
			Headroom: 0, ShortHeadroom: 0, WeeklyHeadroom: 0, WeeklyHeadroomKnown: true,
			ClaudeExtraUsageEnabled: true, ClaudeExtraUsageKnown: true, ClaudeExtraUsageRemaining: 0.40,
		},
	}))
	server.SchedulerRef.MarkAccountUnavailableUntil(accounts.ProviderClaude, "cooked@example.com", time.Now().Add(time.Hour))

	fallback, ok := server.pickClaudeExtraUsageFallbackForServer(
		server.SchedulerRef.Get(), server.Accounts, "",
	)
	if !ok {
		t.Fatal("expected an eligible paid-usage fallback")
	}
	if fallback.ID != "fresh@example.com" {
		t.Fatalf("fallback account = %q, want fresh@example.com", fallback.ID)
	}
}

func TestCaptureResponseBodyClaude401MarksExhausted(t *testing.T) {
	server, _ := claudeFailoverServer(t)
	response := &http.Response{
		StatusCode: http.StatusUnauthorized,
		Body:       io.NopCloser(strings.NewReader(`{"type":"error","error":{"type":"authentication_error"}}`)),
		Header:     http.Header{},
	}
	server.captureResponseBody(response, context.Background(), "claude", "session-1", "cooked@example.com", accounts.ProviderClaude, "", "", "/v1/messages")
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
	server.captureResponseBody(response, context.Background(), "claude", "session-1", "cooked@example.com", accounts.ProviderClaude, "", "", "/v1/messages")
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
	server.captureResponseBody(response, context.Background(), "claude", "session-1", "healthy@example.com", accounts.ProviderClaude, "", "", "/v1/messages")
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
	if _, err := io.Copy(io.Discard, response.Body); err != nil {
		t.Fatal(err)
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

// refreshSelectedAccount must only fail over when the refresh error says the
// credential is dead. A transient failure (cancelled context, timeout, upstream
// 5xx) previously fell straight through to markAccountExhaustedRefreshFailure
// for Claude, because the non-terminal carve-out was gated on Codex, so a blip
// held a healthy Claude account out of selection.
func TestRefreshSelectedAccountClaudeKeepsAccountOnTransientRefreshError(t *testing.T) {
	transient := []struct {
		name string
		err  error
	}{
		{name: "context canceled", err: context.Canceled},
		{name: "deadline exceeded", err: context.DeadlineExceeded},
		{name: "upstream 5xx", err: errors.New("Claude OAuth refresh failed: 503 Service Unavailable")},
	}
	for _, tc := range transient {
		t.Run(tc.name, func(t *testing.T) {
			server, _ := claudeFailoverServer(t)
			cooked := server.Accounts[0]
			refreshed := make([]string, 0, 2)
			server.RefreshAccountFn = func(_ context.Context, account accounts.Account) (accounts.Account, error) {
				refreshed = append(refreshed, account.ID)
				return account, tc.err
			}

			got, pending, err := server.refreshSelectedAccount(context.Background(), accounts.ProviderClaude, "claude", "session-1", "", claudeRefreshRequest(t), cooked)
			if err == nil {
				t.Fatal("a transient refresh failure must still be reported to the caller")
			}
			if !errors.Is(err, tc.err) {
				t.Fatalf("expected the transient error back, got %v", err)
			}
			if got.ID != cooked.ID {
				t.Fatalf("expected the originally selected account back, got %q", got.ID)
			}
			if pending {
				t.Fatal("a transient refresh failure created a pending reassignment")
			}
			if len(refreshed) != 1 || refreshed[0] != cooked.ID {
				t.Fatalf("expected exactly one refresh of the selected account, got %v", refreshed)
			}
			if server.SchedulerRef.Get().Exhausted(accounts.ProviderClaude, cooked.ID) {
				t.Fatal("a transient refresh failure must not mark a healthy Claude account exhausted")
			}
		})
	}
}

// The carve-out must not swallow a real logout: Claude's refresh error text
// carries invalid_grant, which isTerminalCredentialError classifies as terminal,
// so the account is still marked and failover still runs.
func TestRefreshSelectedAccountClaudeFailsOverOnTerminalRefreshError(t *testing.T) {
	server, _ := claudeFailoverServer(t)
	cooked := server.Accounts[0]
	fresh := server.Accounts[1]
	refreshed := make([]string, 0, 2)
	server.RefreshAccountFn = func(_ context.Context, account accounts.Account) (accounts.Account, error) {
		refreshed = append(refreshed, account.ID)
		if account.ID == cooked.ID {
			return account, errors.New(`Claude OAuth refresh failed: 400 Bad Request: {"error": "invalid_grant", "error_description": "Refresh token not found or invalid"}`)
		}
		return account, nil
	}

	got, pending, err := server.refreshSelectedAccount(context.Background(), accounts.ProviderClaude, "claude", "session-1", "", claudeRefreshRequest(t), cooked)
	if err != nil {
		t.Fatalf("failover to the healthy account should succeed: %v", err)
	}
	if got.ID != fresh.ID {
		t.Fatalf("expected failover to %q, got %q", fresh.ID, got.ID)
	}
	if !pending {
		t.Fatal("the refreshed alternate should remain provisional until upstream success")
	}
	if len(refreshed) != 2 || refreshed[0] != cooked.ID || refreshed[1] != fresh.ID {
		t.Fatalf("expected a refresh of the dead account then the healthy one, got %v", refreshed)
	}
	if !server.SchedulerRef.Get().Exhausted(accounts.ProviderClaude, cooked.ID) {
		t.Fatal("a terminal Claude credential error must still mark the account exhausted")
	}
}

func claudeRefreshRequest(t *testing.T) *http.Request {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, "https://subrouter.test/v1/messages", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Subrouter-Agent", "claude")
	req.Header.Set("X-Subrouter-Session", "session-1")
	return req
}

// A stored credential that will not decode cannot be refreshed, so it needs
// re-auth exactly like a rejected refresh token does. Classifying it as
// transient would retry the same unparseable blob forever instead of failing
// over to an account that still works.
func TestIsTerminalCredentialErrorClassifiesUnreadableCredential(t *testing.T) {
	unreadable := errors.New(`unreadable credential from keychain (bytes=132 offset=125 trailing_bytes=8 trailing_kind=binary-plist): invalid character 'b' after top-level value`)
	if !isTerminalCredentialError(unreadable) {
		t.Fatal("an unreadable stored credential must be terminal so selection fails over")
	}
	if isTerminalCredentialError(errors.New("Claude OAuth refresh failed: 503 Service Unavailable")) {
		t.Fatal("an upstream 5xx must stay transient")
	}
	if isTerminalCredentialError(context.Canceled) {
		t.Fatal("a cancelled context must stay transient")
	}
}

const claudeSSEOverloadedEvent = "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"Overloaded\"}}\n\n"

const claudeSSEMessageStart = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_sse\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[]}}\n\n" +
	"event: ping\ndata: {\"type\":\"ping\"}\n\n"

const claudeSSEContent = "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
	"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hello\"}}\n\n"

const claudeSSETail = "event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
	"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"

func claudeSSEResponse(body string) *http.Response {
	h := http.Header{}
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	return &http.Response{StatusCode: http.StatusOK, Header: h, Body: io.NopCloser(strings.NewReader(body))}
}

// TestClaudeSSEOverloadBeforeContentRetried: Anthropic can answer 200 and then
// send an overloaded_error SSE event before any content. Nothing reached the
// client yet, so it must be retried like a 529 and the client must receive the
// retried, successful stream.
func TestClaudeSSEOverloadBeforeContentRetried(t *testing.T) {
	server, store := claudeFailoverServer(t)
	if _, err := store.Put("claude", "session-sse", "fresh@example.com", ""); err != nil {
		t.Fatal(err)
	}
	var calls int
	stub := &stubRoundTripper{responses: func(req *http.Request) *http.Response {
		calls++
		if calls == 1 {
			return claudeSSEResponse(claudeSSEMessageStart + claudeSSEOverloadedEvent)
		}
		return claudeSSEResponse(claudeSSEMessageStart + claudeSSEContent + claudeSSETail)
	}}
	var waits []time.Duration
	transport := usageLimitRetryTransport{
		base: stub, server: &server, provider: accounts.ProviderClaude,
		agent: "claude", session: "session-sse", account: "fresh@example.com",
		method: http.MethodPost, path: "/v1/messages", maxAttempts: 6,
		sleep: recordSleep(&waits),
	}
	req, _ := http.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages", bytes.NewReader([]byte(`{"stream":true}`)))
	req.Header.Set("Authorization", "Bearer tok-fresh")
	response, err := transport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.StatusCode)
	}
	if calls != 2 {
		t.Fatalf("upstream calls = %d, want 2 (overloaded stream, then retry)", calls)
	}
	if string(body) != claudeSSEMessageStart+claudeSSEContent+claudeSSETail {
		t.Fatalf("client body = %q, want only the retried successful stream", string(body))
	}
	if len(waits) != 1 || waits[0] != time.Second {
		t.Fatalf("backoff waits = %v, want [1s]", waits)
	}
	if server.SchedulerRef.Get().Exhausted(accounts.ProviderClaude, "fresh@example.com") {
		t.Fatal("an in-stream overload must NOT mark the account exhausted")
	}
}

// TestClaudeSSEOverloadAfterContentPassedThrough: once content has streamed,
// replaying would duplicate output, so a later overloaded_error must be passed
// through untouched with no retry.
func TestClaudeSSEOverloadAfterContentPassedThrough(t *testing.T) {
	server, _ := claudeFailoverServer(t)
	stream := claudeSSEMessageStart + claudeSSEContent + claudeSSEOverloadedEvent
	var calls int
	stub := &stubRoundTripper{responses: func(req *http.Request) *http.Response {
		calls++
		return claudeSSEResponse(stream)
	}}
	var waits []time.Duration
	transport := usageLimitRetryTransport{
		base: stub, server: &server, provider: accounts.ProviderClaude,
		agent: "claude", session: "s", account: "fresh@example.com",
		method: http.MethodPost, path: "/v1/messages", maxAttempts: 6,
		sleep: recordSleep(&waits),
	}
	req, _ := http.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages", bytes.NewReader([]byte(`{"stream":true}`)))
	req.Header.Set("Authorization", "Bearer tok-fresh")
	response, err := transport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	if calls != 1 || len(waits) != 0 {
		t.Fatalf("calls=%d waits=%v, want a single attempt with no retry", calls, waits)
	}
	if response.StatusCode != http.StatusOK || string(body) != stream {
		t.Fatalf("status=%d body=%q, want the original stream passed through", response.StatusCode, string(body))
	}
}

// TestClaudeOverloadReroutesOnceAfterSameAccountRetries: after the bounded
// same-account 529 retries, one other account with headroom gets exactly one
// attempt. Overload is not quota, so the first account is not marked and the
// session is not moved.
func TestClaudeOverloadReroutesOnceAfterSameAccountRetries(t *testing.T) {
	server, store := claudeFailoverServer(t)
	if _, err := store.Put("claude", "session-reroute", "cooked@example.com", ""); err != nil {
		t.Fatal(err)
	}
	var cookedCalls, freshCalls int
	stub := &stubRoundTripper{responses: func(req *http.Request) *http.Response {
		if strings.Contains(req.Header.Get("Authorization"), "tok-cooked") {
			cookedCalls++
			return &http.Response{StatusCode: 529, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"type":"error","error":{"type":"overloaded_error"}}`))}
		}
		freshCalls++
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"id":"msg_fresh"}`))}
	}}
	var waits []time.Duration
	transport := usageLimitRetryTransport{
		base: stub, server: &server, provider: accounts.ProviderClaude,
		agent: "claude", session: "session-reroute", account: "cooked@example.com",
		method: http.MethodPost, path: "/v1/messages", maxAttempts: 6,
		sleep: recordSleep(&waits),
	}
	req, _ := http.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages", bytes.NewReader([]byte(`{}`)))
	req.Header.Set("Authorization", "Bearer tok-cooked")
	response, err := transport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	if response.StatusCode != http.StatusOK || !strings.Contains(string(body), "msg_fresh") {
		t.Fatalf("status=%d body=%s, want the alternate account's 200", response.StatusCode, string(body))
	}
	if cookedCalls != 1+providerOverloadMaxRetries || freshCalls != 1 {
		t.Fatalf("calls cooked=%d fresh=%d, want %d/1", cookedCalls, freshCalls, 1+providerOverloadMaxRetries)
	}
	if server.SchedulerRef.Get().Exhausted(accounts.ProviderClaude, "cooked@example.com") {
		t.Fatal("overload must NOT mark the first account exhausted")
	}
	if got, ok := store.Get("claude", "session-reroute"); !ok || got.AccountID != "cooked@example.com" {
		t.Fatalf("session assignment = %+v, want it to stay on cooked@example.com", got)
	}
}

// TestClaudeOverloadNoRerouteWithoutHeadroom: the one-shot overload reroute
// only targets an account with new-session headroom; otherwise the 529 passes
// through after the same-account retries.
func TestClaudeOverloadNoRerouteWithoutHeadroom(t *testing.T) {
	server, _ := claudeFailoverServer(t)
	// Both accounts are low but not exhausted: below new-session headroom.
	server.SchedulerRef = selectacct.NewSchedulerRef(selectacct.NewScheduler([]selectacct.Score{
		{AccountID: "cooked@example.com", Provider: accounts.ProviderClaude, Headroom: 0.1, ShortHeadroom: 0.1},
		{AccountID: "fresh@example.com", Provider: accounts.ProviderClaude, Headroom: 0.1, ShortHeadroom: 0.1},
	}))
	var calls int
	stub := &stubRoundTripper{responses: func(req *http.Request) *http.Response {
		calls++
		return &http.Response{StatusCode: 529, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"type":"error","error":{"type":"overloaded_error"}}`))}
	}}
	var waits []time.Duration
	transport := usageLimitRetryTransport{
		base: stub, server: &server, provider: accounts.ProviderClaude,
		agent: "claude", session: "s", account: "fresh@example.com",
		method: http.MethodPost, path: "/v1/messages", maxAttempts: 6,
		sleep: recordSleep(&waits),
	}
	req, _ := http.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages", bytes.NewReader([]byte(`{}`)))
	req.Header.Set("Authorization", "Bearer tok-fresh")
	response, err := transport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != 529 || calls != 1+providerOverloadMaxRetries {
		t.Fatalf("status=%d calls=%d, want 529 after %d same-account attempts", response.StatusCode, calls, 1+providerOverloadMaxRetries)
	}
}

// TestClaudeStreamOverloadedPeekTimeoutKeepsBytes: when the first decisive
// event is slower than the peek bound, the stream is handed over undecided
// and every byte (peeked and later) still reaches the client in order.
func TestClaudeStreamOverloadedPeekTimeoutKeepsBytes(t *testing.T) {
	previous := claudeSSEOverloadPeekTimeout
	claudeSSEOverloadPeekTimeout = 20 * time.Millisecond
	t.Cleanup(func() { claudeSSEOverloadPeekTimeout = previous })
	pr, pw := io.Pipe()
	h := http.Header{}
	h.Set("Content-Type", "text/event-stream")
	response := &http.Response{StatusCode: http.StatusOK, Header: h, Body: pr}
	go func() {
		_, _ = pw.Write([]byte(claudeSSEMessageStart))
		time.Sleep(100 * time.Millisecond)
		_, _ = pw.Write([]byte(claudeSSEContent + claudeSSETail))
		_ = pw.Close()
	}()
	if claudeStreamOverloaded(response) {
		t.Fatal("an undecided stream must not be classified as overloaded")
	}
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != claudeSSEMessageStart+claudeSSEContent+claudeSSETail {
		t.Fatalf("body = %q, want the full stream in order", string(body))
	}
	_ = response.Body.Close()
}
