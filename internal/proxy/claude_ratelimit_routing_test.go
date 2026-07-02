package proxy

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	agentclaude "github.com/manaflow-ai/subrouter/internal/agents/claude"
	"github.com/manaflow-ai/subrouter/internal/selectacct"
	"github.com/manaflow-ai/subrouter/internal/session"
)

// realisticAnthropic429Body mirrors the body Anthropic returns when a Claude
// subscription account hits its rolling rate limit. The exact wording is what we
// want to surface in logs so operators can see "what the message is like".
const realisticAnthropic429Body = `{"type":"error","error":{"type":"rate_limit_error","message":"This request would exceed your account's rate limit. Please try again later."}}`

// TestClaude429FailoverEndToEndAndCaptured drives a real request through
// Server.Handler() with a mock Anthropic upstream that rate-limits the first
// account. It proves two things the user asked for: (1) the genuine 429 message
// and rate-limit headers are captured in logs, and (2) traffic fails over to a
// second Claude account and succeeds.
func TestClaude429FailoverEndToEndAndCaptured(t *testing.T) {
	var cookedHits, freshHits int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		auth := r.Header.Get("Authorization")
		switch {
		case strings.Contains(auth, "tok-cooked"):
			cookedHits++
			w.Header().Set("Retry-After", "3600")
			w.Header().Set("Anthropic-Ratelimit-Unified-Status", "rejected")
			w.Header().Set("Anthropic-Ratelimit-Unified-Reset", "1750000000")
			w.Header().Set("Anthropic-Ratelimit-Unified-Remaining", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(realisticAnthropic429Body))
		case strings.Contains(auth, "tok-fresh"):
			freshHits++
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"id":"msg_ok","type":"message"}`))
		default:
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"unexpected account"}`))
		}
	}))
	defer upstream.Close()
	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}

	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	// Pin the session to the cooked account so it is selected first; the 429
	// then forces failover. Both accounts start healthy so the sticky
	// assignment is honored until the upstream rejects it.
	if _, err := store.Put("claude", "session-rl", "cooked@example.com", ""); err != nil {
		t.Fatal(err)
	}

	schedulerRef := selectacct.NewSchedulerRef(selectacct.NewScheduler([]selectacct.Score{
		{AccountID: "cooked@example.com", Provider: accounts.ProviderClaude, Headroom: 1, ShortHeadroom: 1},
		{AccountID: "fresh@example.com", Provider: accounts.ProviderClaude, Headroom: 1, ShortHeadroom: 1},
	}))

	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	handler := Server{
		ClaudeUpstream: upstreamURL,
		Accounts: []accounts.Account{
			{ID: "cooked@example.com", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "tok-cooked"},
			{ID: "fresh@example.com", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "tok-fresh"},
		},
		Sessions:      store,
		SchedulerRef:  schedulerRef,
		UsageScoreTTL: 0,
		MaxBodyBytes:  1 << 20,
		Logger:        logger,
	}.Handler()
	subrouter := httptest.NewServer(handler)
	defer subrouter.Close()

	req, err := http.NewRequest(http.MethodPost, subrouter.URL+"/v1/messages", strings.NewReader(`{"model":"claude-opus-4-8","messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Subrouter-Agent", "claude")
	req.Header.Set("X-Subrouter-Session", "session-rl")
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)

	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 after failover; body=%s", response.StatusCode, string(body))
	}
	if cookedHits != 1 {
		t.Fatalf("cooked upstream hits = %d, want 1 (one 429 then failover)", cookedHits)
	}
	if freshHits != 1 {
		t.Fatalf("fresh upstream hits = %d, want 1 (served after failover)", freshHits)
	}
	if !schedulerRef.Get().Exhausted(accounts.ProviderClaude, "cooked@example.com") {
		t.Fatal("cooked account should be marked exhausted by the 429")
	}
	assignment, ok := store.Get("claude", "session-rl")
	if !ok || assignment.AccountID != "fresh@example.com" {
		t.Fatalf("sticky assignment = %+v, want moved to fresh@example.com", assignment)
	}

	logs := logBuf.String()
	// The genuine upstream message must be captured so operators can see it.
	if !strings.Contains(logs, "This request would exceed your account") {
		t.Fatalf("expected the 429 body to be logged; logs=\n%s", logs)
	}
	// The rate-limit headers must be captured too.
	for _, want := range []string{"retry-after", "anthropic-ratelimit-unified-reset"} {
		if !strings.Contains(logs, want) {
			t.Fatalf("expected rate-limit header %q in logs; logs=\n%s", want, logs)
		}
	}
}

func floatPtr(v float64) *float64 { return &v }

// TestClaudeUsageWindowsClassifyFiveHourAsShort guards the GTO routing fix:
// the 5h window must be tagged with its window length so the scheduler treats it
// as the short window. Without LimitWindowSeconds the scheduler could not tell
// the 5h rate limit apart from the 7d one, so ShortHeadroom, the 5h reset, and
// ExpiryPressure were all dead and the routing calculation ignored the 5h limit.
func TestClaudeUsageWindowsClassifyFiveHourAsShort(t *testing.T) {
	usage := &agentclaude.UsageResponse{
		// 5h window is nearly drained and resets soon; 7d window is healthy.
		FiveHour: &agentclaude.RateLimit{Utilization: floatPtr(95), ResetsAt: time.Now().Add(30 * time.Minute).Format(time.RFC3339)},
		SevenDay: &agentclaude.RateLimit{Utilization: floatPtr(10), ResetsAt: time.Now().Add(5 * 24 * time.Hour).Format(time.RFC3339)},
	}
	windows := claudeUsageWindows(usage)

	var fiveHour, sevenDay *accounts.UsageWindow
	for i := range windows {
		switch windows[i].Name {
		case "5h":
			fiveHour = &windows[i]
		case "7d":
			sevenDay = &windows[i]
		}
	}
	if fiveHour == nil || sevenDay == nil {
		t.Fatalf("missing windows: %+v", windows)
	}
	if fiveHour.LimitWindowSeconds != 5*60*60 {
		t.Fatalf("5h LimitWindowSeconds = %d, want %d", fiveHour.LimitWindowSeconds, 5*60*60)
	}
	if sevenDay.LimitWindowSeconds != 7*24*60*60 {
		t.Fatalf("7d LimitWindowSeconds = %d, want %d", sevenDay.LimitWindowSeconds, 7*24*60*60)
	}

	score := scoreFromUsageWindows(accounts.ProviderClaude, "acct", windows)
	// Overall headroom is the min remaining (driven by the cooked 5h window).
	if score.Headroom > 0.06 {
		t.Fatalf("Headroom = %.3f, want ~0.05 (driven by 5h window)", score.Headroom)
	}
	// Short headroom must come from the 5h window specifically.
	if score.ShortHeadroom > 0.06 {
		t.Fatalf("ShortHeadroom = %.3f, want ~0.05 from the 5h window", score.ShortHeadroom)
	}
	// The 5h reset must be carried so the scheduler can compute expiry pressure.
	if score.ShortResetAfterSeconds <= 0 {
		t.Fatalf("ShortResetAfterSeconds = %d, want > 0 (5h reset)", score.ShortResetAfterSeconds)
	}
	if score.ExpiryPressure <= 0 {
		t.Fatal("ExpiryPressure should be > 0 once the 5h window is classified as short")
	}
}

// TestClaudeShortWindowDrivesRouting proves the routing calculation now prefers
// the account with more 5h headroom even when both accounts have identical
// account-wide (7d) headroom. Before the fix both scored equal short headroom.
func TestClaudeShortWindowDrivesRouting(t *testing.T) {
	drained := scoreFromUsageWindows(accounts.ProviderClaude, "drained", claudeUsageWindows(&agentclaude.UsageResponse{
		FiveHour: &agentclaude.RateLimit{Utilization: floatPtr(90), ResetsAt: time.Now().Add(time.Hour).Format(time.RFC3339)},
		SevenDay: &agentclaude.RateLimit{Utilization: floatPtr(10), ResetsAt: time.Now().Add(5 * 24 * time.Hour).Format(time.RFC3339)},
	}))
	fresh := scoreFromUsageWindows(accounts.ProviderClaude, "fresh", claudeUsageWindows(&agentclaude.UsageResponse{
		FiveHour: &agentclaude.RateLimit{Utilization: floatPtr(10), ResetsAt: time.Now().Add(time.Hour).Format(time.RFC3339)},
		SevenDay: &agentclaude.RateLimit{Utilization: floatPtr(10), ResetsAt: time.Now().Add(5 * 24 * time.Hour).Format(time.RFC3339)},
	}))

	scheduler := selectacct.NewScheduler([]selectacct.Score{drained, fresh})
	picked, err := scheduler.Pick([]accounts.Account{
		{ID: "drained", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth},
		{ID: "fresh", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth},
	})
	if err != nil {
		t.Fatal(err)
	}
	if picked.ID != "fresh" {
		t.Fatalf("scheduler picked %q, want fresh (more 5h headroom)", picked.ID)
	}
}

// TestClaudeAccountExhaustedByResponse pins the authoritative-signal logic:
// 401 and a "rejected" 429 mean the account is out of quota, while an
// "allowed"/"allowed_warning" 429 is a transient throttle that must not poison
// the routing score. A 429 with no unified-status header keeps the conservative
// legacy behavior (treated as exhausted).
func TestClaudeAccountExhaustedByResponse(t *testing.T) {
	withStatus := func(v string) http.Header {
		h := http.Header{}
		if v != "" {
			h.Set("Anthropic-Ratelimit-Unified-Status", v)
		}
		return h
	}
	cases := []struct {
		name   string
		status int
		header http.Header
		want   bool
	}{
		{"401 dead token", http.StatusUnauthorized, http.Header{}, true},
		{"429 rejected", http.StatusTooManyRequests, withStatus("rejected"), true},
		{"429 allowed_warning", http.StatusTooManyRequests, withStatus("allowed_warning"), false},
		{"429 allowed", http.StatusTooManyRequests, withStatus("allowed"), false},
		{"429 no header (transient burst, not quota)", http.StatusTooManyRequests, http.Header{}, false},
		{"200 healthy", http.StatusOK, withStatus("allowed"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := claudeAccountExhaustedByResponse(tc.status, tc.header); got != tc.want {
				t.Fatalf("claudeAccountExhaustedByResponse(%d, %v) = %v, want %v", tc.status, tc.header, got, tc.want)
			}
		})
	}
}

func TestClaudeResponseExhaustedUntil(t *testing.T) {
	now := time.Unix(1000, 0)
	header := http.Header{}
	header.Set("Anthropic-Ratelimit-Unified-Reset", "1500")
	if got := claudeResponseExhaustedUntil(header, now); !got.Equal(time.Unix(1500, 0)) {
		t.Fatalf("reset header parsed as %s, want %s", got, time.Unix(1500, 0))
	}

	header = http.Header{}
	header.Set("Retry-After", "60")
	if got := claudeResponseExhaustedUntil(header, now); !got.Equal(now.Add(time.Minute)) {
		t.Fatalf("retry-after parsed as %s, want %s", got, now.Add(time.Minute))
	}
}

// TestClaudeTransient429FailsOverWithoutPoisoningScore proves a transient
// (allowed_warning) 429 still fails the request over to another account but does
// NOT mark the first account exhausted, so the GTO scheduler keeps routing to it.
func TestClaudeTransient429FailsOverWithoutPoisoningScore(t *testing.T) {
	server, store := claudeFailoverServer(t)
	if _, err := store.Put("claude", "session-t", "cooked@example.com", ""); err != nil {
		t.Fatal(err)
	}
	stub := &stubRoundTripper{responses: func(req *http.Request) *http.Response {
		if strings.Contains(req.Header.Get("Authorization"), "tok-cooked") {
			h := http.Header{}
			h.Set("Anthropic-Ratelimit-Unified-Status", "allowed_warning")
			return &http.Response{StatusCode: http.StatusTooManyRequests, Header: h, Body: io.NopCloser(strings.NewReader(`{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`))}
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"id":"msg_ok"}`))}
	}}
	transport := usageLimitRetryTransport{
		base: stub, server: &server, provider: accounts.ProviderClaude,
		agent: "claude", session: "session-t", account: "cooked@example.com",
		method: http.MethodPost, path: "/v1/messages", maxAttempts: 3,
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
		t.Fatalf("status = %d, want 200 after transient failover", response.StatusCode)
	}
	if server.SchedulerRef.Get().Exhausted(accounts.ProviderClaude, "cooked@example.com") {
		t.Fatal("a transient allowed_warning 429 must NOT mark the account exhausted")
	}
}

// TestClaudeFailoverExhaustionIsLogged guards the monitoring contract: when
// every failover attempt is rate-limited and the client ends up with a 429, the
// single authoritative failure signal must be logged even though no
// "no alternate account" error occurred (the maxAttempts cap was hit first).
func TestClaudeFailoverExhaustionIsLogged(t *testing.T) {
	server, store := claudeFailoverServer(t)
	if _, err := store.Put("claude", "session-x", "cooked@example.com", ""); err != nil {
		t.Fatal(err)
	}
	// Every account the server can pick returns a rejected 429, so failover
	// never finds a healthy account and the client gets a 429.
	stub := &stubRoundTripper{responses: func(req *http.Request) *http.Response {
		h := http.Header{}
		h.Set("Anthropic-Ratelimit-Unified-Status", "rejected")
		return &http.Response{StatusCode: http.StatusTooManyRequests, Header: h, Body: io.NopCloser(strings.NewReader(`{"type":"error","error":{"type":"rate_limit_error","message":"Error"}}`))}
	}}
	var logBuf bytes.Buffer
	server.Logger = slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	// maxAttempts=2 with the 2-account server forces the max_attempts give-up
	// path specifically: attempt 1 (cooked) fails over to fresh, attempt 2
	// (fresh) hits attempt==maxAttempts and returns via the max_attempts branch
	// BEFORE oauthRetryAccount could report no_alternate_account. This is the
	// path that previously failed silently.
	transport := usageLimitRetryTransport{
		base: stub, server: &server, logger: server.Logger, provider: accounts.ProviderClaude,
		agent: "claude", session: "session-x", account: "cooked@example.com",
		method: http.MethodPost, path: "/v1/messages", maxAttempts: 2,
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
	if response.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429 (all accounts rate-limited)", response.StatusCode)
	}
	logs := logBuf.String()
	if !strings.Contains(logs, "claude rate-limit returned to client after failover exhausted") {
		t.Fatalf("expected the client-facing failover-exhaustion signal; logs=\n%s", logs)
	}
	// Assert the path is specifically max_attempts (not no_alternate_account), so
	// removing the new max_attempts logging call would fail this test.
	if !strings.Contains(logs, "reason=max_attempts") {
		t.Fatalf("expected reason=max_attempts in the failover-exhaustion log; logs=\n%s", logs)
	}
}

// TestClaudeRejectedHeaderOn200FailsOver is the Aziz regression: a depleted
// account answers 200 via overage but sets anthropic-ratelimit-unified-status:
// rejected, which Claude Code hard-blocks on. subrouter must treat that 200 as
// unusable, fail over to a healthy account, return the healthy (allowed)
// response to the client, and mark the depleted account exhausted.
func TestClaudeRejectedHeaderOn200FailsOver(t *testing.T) {
	server, store := claudeFailoverServer(t)
	if _, err := store.Put("claude", "session-rej", "cooked@example.com", ""); err != nil {
		t.Fatal(err)
	}
	var cookedHits, freshHits int
	const secretCompletion = "SECRET_USER_COMPLETION_CONTENT"
	stub := &stubRoundTripper{responses: func(req *http.Request) *http.Response {
		if strings.Contains(req.Header.Get("Authorization"), "tok-cooked") {
			cookedHits++
			h := http.Header{}
			// 200 OK, but Anthropic flags the account out of quota (served via overage).
			h.Set("Anthropic-Ratelimit-Unified-Status", "rejected")
			h.Set("Anthropic-Ratelimit-Unified-Overage-In-Use", "true")
			h.Set("Anthropic-Ratelimit-Unified-Reset", fmt.Sprintf("%d", time.Now().Add(time.Hour).Unix()))
			// A rejected 200 carries a real completion; it must never be logged.
			return &http.Response{StatusCode: http.StatusOK, Header: h, Body: io.NopCloser(strings.NewReader(`{"id":"msg_overage","text":"` + secretCompletion + `"}`))}
		}
		freshHits++
		h := http.Header{}
		h.Set("Anthropic-Ratelimit-Unified-Status", "allowed")
		return &http.Response{StatusCode: http.StatusOK, Header: h, Body: io.NopCloser(strings.NewReader(`{"id":"msg_fresh"}`))}
	}}
	var logBuf bytes.Buffer
	server.Logger = slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	transport := usageLimitRetryTransport{
		base: stub, server: &server, logger: server.Logger, provider: accounts.ProviderClaude,
		agent: "claude", session: "session-rej", account: "cooked@example.com",
		method: http.MethodPost, path: "/v1/messages", maxAttempts: 3,
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
	body, _ := io.ReadAll(response.Body)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.StatusCode)
	}
	// The client must receive the healthy account's response, not the rejected one.
	if !strings.Contains(string(body), "msg_fresh") {
		t.Fatalf("client got %q, want the healthy account's response", string(body))
	}
	if response.Header.Get("Anthropic-Ratelimit-Unified-Status") != "allowed" {
		t.Fatalf("client header status = %q, want allowed", response.Header.Get("Anthropic-Ratelimit-Unified-Status"))
	}
	if cookedHits != 1 || freshHits != 1 {
		t.Fatalf("hits cooked=%d fresh=%d, want 1/1 (rejected 200 then failover)", cookedHits, freshHits)
	}
	if !server.SchedulerRef.Get().Exhausted(accounts.ProviderClaude, "cooked@example.com") {
		t.Fatal("a rejected-header 200 must mark the depleted account exhausted")
	}
	server.SchedulerRef.FinishRefresh(selectacct.NewScheduler([]selectacct.Score{
		{AccountID: "cooked@example.com", Provider: accounts.ProviderClaude, Headroom: 1, ShortHeadroom: 1},
		{AccountID: "fresh@example.com", Provider: accounts.ProviderClaude, Headroom: 1, ShortHeadroom: 1},
	}), true)
	if !server.SchedulerRef.Get().Exhausted(accounts.ProviderClaude, "cooked@example.com") {
		t.Fatal("fresh usage refresh clobbered the live rejected-header exhaustion")
	}
	assignment, ok := store.Get("claude", "session-rej")
	if !ok || assignment.AccountID != "fresh@example.com" {
		t.Fatalf("sticky assignment = %+v, want moved to fresh@example.com", assignment)
	}
	// Privacy: the rejected 200's completion body must never be logged.
	logs := logBuf.String()
	if strings.Contains(logs, secretCompletion) {
		t.Fatalf("leaked Claude completion content into logs; logs=\n%s", logs)
	}
	if strings.Contains(logs, "claude account unusable upstream body") {
		t.Fatalf("must not log a body for a 2xx rejected response; logs=\n%s", logs)
	}
	// But the rejected signal itself (headers) should still be captured.
	if !strings.Contains(logs, "claude account unusable upstream response") {
		t.Fatalf("expected the rejected-header signal to be logged; logs=\n%s", logs)
	}
}

// TestClaudeAllowedHeaderOn200DoesNotFailOver guards against over-rerouting: a
// normal 200 with allowed/allowed_warning must pass straight through.
func TestClaudeAllowedHeaderOn200DoesNotFailOver(t *testing.T) {
	server, store := claudeFailoverServer(t)
	if _, err := store.Put("claude", "session-ok", "cooked@example.com", ""); err != nil {
		t.Fatal(err)
	}
	var hits int
	stub := &stubRoundTripper{responses: func(req *http.Request) *http.Response {
		hits++
		h := http.Header{}
		h.Set("Anthropic-Ratelimit-Unified-Status", "allowed_warning")
		return &http.Response{StatusCode: http.StatusOK, Header: h, Body: io.NopCloser(strings.NewReader(`{"id":"msg_ok"}`))}
	}}
	transport := usageLimitRetryTransport{
		base: stub, server: &server, provider: accounts.ProviderClaude,
		agent: "claude", session: "session-ok", account: "cooked@example.com",
		method: http.MethodPost, path: "/v1/messages", maxAttempts: 3,
	}
	req, _ := http.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages", bytes.NewReader([]byte(`{"model":"claude-opus-4-8"}`)))
	req.Header.Set("Authorization", "Bearer tok-cooked")
	response, err := transport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if hits != 1 {
		t.Fatalf("upstream hits = %d, want 1 (no failover on allowed_warning)", hits)
	}
	if server.SchedulerRef.Get().Exhausted(accounts.ProviderClaude, "cooked@example.com") {
		t.Fatal("allowed_warning must not mark the account exhausted")
	}
}

// TestClaudeRejectedNon429StatusDoesNotLogBody locks the logging scope: a
// rejected-header response on a status other than 429/401 (here a 500) must fail
// over but never have its body logged, since that body is not a known rate-limit
// envelope and may contain request/error detail.
func TestClaudeRejectedNon429StatusDoesNotLogBody(t *testing.T) {
	server, store := claudeFailoverServer(t)
	if _, err := store.Put("claude", "session-5xx", "cooked@example.com", ""); err != nil {
		t.Fatal(err)
	}
	const secret = "SENSITIVE_5XX_BODY"
	stub := &stubRoundTripper{responses: func(req *http.Request) *http.Response {
		if strings.Contains(req.Header.Get("Authorization"), "tok-cooked") {
			h := http.Header{}
			h.Set("Anthropic-Ratelimit-Unified-Status", "rejected")
			return &http.Response{StatusCode: http.StatusInternalServerError, Header: h, Body: io.NopCloser(strings.NewReader(secret))}
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"id":"ok"}`))}
	}}
	var logBuf bytes.Buffer
	server.Logger = slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	transport := usageLimitRetryTransport{
		base: stub, server: &server, logger: server.Logger, provider: accounts.ProviderClaude,
		agent: "claude", session: "session-5xx", account: "cooked@example.com",
		method: http.MethodPost, path: "/v1/messages", maxAttempts: 3,
	}
	req, _ := http.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages", bytes.NewReader([]byte(`{"model":"claude-opus-4-8"}`)))
	req.Header.Set("Authorization", "Bearer tok-cooked")
	response, err := transport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if strings.Contains(logBuf.String(), secret) {
		t.Fatalf("leaked a non-rate-limit 5xx body into logs; logs=\n%s", logBuf.String())
	}
}

// TestClaudeFailoverSkipsDeadTokenAccount is the live regression caught by
// monitoring: failover picked an account whose OAuth refresh returned
// invalid_grant and aborted the whole retry ("no alternate account" -> 429 to
// the client) even though healthy accounts remained untried. Failover must skip
// the dead-token account and continue to a healthy one.
func TestClaudeFailoverSkipsDeadTokenAccount(t *testing.T) {
	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put("claude", "session-dead", "cooked@example.com", ""); err != nil {
		t.Fatal(err)
	}
	server := Server{
		Accounts: []accounts.Account{
			{ID: "cooked@example.com", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "tok-cooked"},
			{ID: "dead@example.com", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "tok-dead"},
			{ID: "fresh@example.com", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "tok-fresh"},
		},
		Sessions:     store,
		SchedulerRef: selectacct.NewSchedulerRef(selectacct.NewScheduler(nil)),
		// Pick order among untried candidates: dead (highest) before fresh, so
		// failover tries the dead-token account first and must skip it.
		ScoreAccounts: func(ctx context.Context, available []accounts.Account) ([]selectacct.Score, int) {
			h := map[string]float64{"cooked@example.com": 0, "dead@example.com": 1.0, "fresh@example.com": 0.9}
			scores := make([]selectacct.Score, 0, len(available))
			for _, a := range available {
				scores = append(scores, selectacct.Score{AccountID: a.ID, Provider: a.Provider, Headroom: h[a.ID], ShortHeadroom: h[a.ID]})
			}
			return scores, len(scores)
		},
		// dead@example.com has a dead refresh token; everything else refreshes fine.
		RefreshAccountFn: func(ctx context.Context, a accounts.Account) (accounts.Account, error) {
			if a.ID == "dead@example.com" {
				return accounts.Account{}, fmt.Errorf("Claude OAuth refresh failed: 400 Bad Request: invalid_grant")
			}
			return a, nil
		},
	}
	stub := &stubRoundTripper{responses: func(req *http.Request) *http.Response {
		if strings.Contains(req.Header.Get("Authorization"), "tok-fresh") {
			return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"id":"msg_fresh"}`))}
		}
		// cooked (and any other) -> 429 rejected
		h := http.Header{}
		h.Set("Anthropic-Ratelimit-Unified-Status", "rejected")
		return &http.Response{StatusCode: http.StatusTooManyRequests, Header: h, Body: io.NopCloser(strings.NewReader(`{"type":"error","error":{"type":"rate_limit_error","message":"Error"}}`))}
	}}
	transport := usageLimitRetryTransport{
		base: stub, server: &server, provider: accounts.ProviderClaude,
		agent: "claude", session: "session-dead", account: "cooked@example.com",
		method: http.MethodPost, path: "/v1/messages", maxAttempts: 6,
	}
	req, _ := http.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages", bytes.NewReader([]byte(`{"model":"claude-opus-4-8"}`)))
	req.Header.Set("Authorization", "Bearer tok-cooked")
	response, err := transport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (failover should skip dead token and reach fresh); body=%s", response.StatusCode, string(body))
	}
	if !strings.Contains(string(body), "msg_fresh") {
		t.Fatalf("client got %q, want the healthy account's response", string(body))
	}
	if !server.SchedulerRef.Get().Exhausted(accounts.ProviderClaude, "dead@example.com") {
		t.Fatal("the dead-token account should be marked exhausted (dropped from selection)")
	}
	assignment, ok := store.Get("claude", "session-dead")
	if !ok || assignment.AccountID != "fresh@example.com" {
		t.Fatalf("sticky assignment = %+v, want fresh@example.com", assignment)
	}
}

// TestClaudeFailoverTransientRefreshErrorNotPoisoned guards the refinement: a
// transient refresh failure (not a dead token) must skip the account for this
// request but must NOT mark it exhausted, so it stays eligible for future
// routing once the transient condition clears.
func TestClaudeFailoverTransientRefreshErrorNotPoisoned(t *testing.T) {
	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put("claude", "session-tr", "cooked@example.com", ""); err != nil {
		t.Fatal(err)
	}
	server := Server{
		Accounts: []accounts.Account{
			{ID: "cooked@example.com", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "tok-cooked"},
			{ID: "blip@example.com", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "tok-blip"},
			{ID: "fresh@example.com", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "tok-fresh"},
		},
		Sessions:     store,
		SchedulerRef: selectacct.NewSchedulerRef(selectacct.NewScheduler(nil)),
		ScoreAccounts: func(ctx context.Context, available []accounts.Account) ([]selectacct.Score, int) {
			h := map[string]float64{"cooked@example.com": 0, "blip@example.com": 1.0, "fresh@example.com": 0.9}
			scores := make([]selectacct.Score, 0, len(available))
			for _, a := range available {
				scores = append(scores, selectacct.Score{AccountID: a.ID, Provider: a.Provider, Headroom: h[a.ID], ShortHeadroom: h[a.ID]})
			}
			return scores, len(scores)
		},
		// blip@ fails with a transient (non-credential) error, not a dead token.
		RefreshAccountFn: func(ctx context.Context, a accounts.Account) (accounts.Account, error) {
			if a.ID == "blip@example.com" {
				return accounts.Account{}, fmt.Errorf("dial tcp: connection refused")
			}
			return a, nil
		},
	}
	stub := &stubRoundTripper{responses: func(req *http.Request) *http.Response {
		if strings.Contains(req.Header.Get("Authorization"), "tok-fresh") {
			return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"id":"msg_fresh"}`))}
		}
		h := http.Header{}
		h.Set("Anthropic-Ratelimit-Unified-Status", "rejected")
		return &http.Response{StatusCode: http.StatusTooManyRequests, Header: h, Body: io.NopCloser(strings.NewReader(`{"type":"error","error":{"type":"rate_limit_error","message":"Error"}}`))}
	}}
	transport := usageLimitRetryTransport{
		base: stub, server: &server, provider: accounts.ProviderClaude,
		agent: "claude", session: "session-tr", account: "cooked@example.com",
		method: http.MethodPost, path: "/v1/messages", maxAttempts: 6,
	}
	req, _ := http.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages", bytes.NewReader([]byte(`{"model":"claude-opus-4-8"}`)))
	req.Header.Set("Authorization", "Bearer tok-cooked")
	response, err := transport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	if response.StatusCode != http.StatusOK || !strings.Contains(string(body), "msg_fresh") {
		t.Fatalf("status=%d body=%s, want 200 from fresh (transient blip skipped)", response.StatusCode, string(body))
	}
	if server.SchedulerRef.Get().Exhausted(accounts.ProviderClaude, "blip@example.com") {
		t.Fatal("a transient refresh error must NOT mark the account exhausted")
	}
}

func TestIsTerminalCredentialError(t *testing.T) {
	cases := []struct {
		err  error
		want bool
	}{
		{fmt.Errorf(`Claude OAuth refresh failed: 400 Bad Request: {"error": "invalid_grant"}`), true},
		{fmt.Errorf("profile has no refresh token"), true},
		{fmt.Errorf("dial tcp: connection refused"), false},
		{context.Canceled, false},
		{context.DeadlineExceeded, false},
		{nil, false},
	}
	for _, tc := range cases {
		if got := isTerminalCredentialError(tc.err); got != tc.want {
			t.Fatalf("isTerminalCredentialError(%v) = %v, want %v", tc.err, got, tc.want)
		}
	}
}

// TestClaudeUpstreamServerErrorLoggedNotExhausted verifies a 529 overload is
// logged for observability but does NOT mark the account exhausted (overload is
// Anthropic-side capacity, not account-specific; rerouting can't help).
func TestClaudeUpstreamServerErrorLoggedNotExhausted(t *testing.T) {
	server, _ := claudeFailoverServer(t)
	var logBuf bytes.Buffer
	server.Logger = slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	response := &http.Response{
		StatusCode: 529,
		Header:     http.Header{},
		Body:       io.NopCloser(strings.NewReader(`{"type":"error","error":{"type":"overloaded_error"}}`)),
	}
	server.captureResponseBody(response, "claude", "session-1", "acct@example.com", accounts.ProviderClaude, "/v1/messages")
	logs := logBuf.String()
	if !strings.Contains(logs, "claude upstream server error") || !strings.Contains(logs, "status=529") {
		t.Fatalf("expected a 529 upstream-server-error log; logs=\n%s", logs)
	}
	if server.SchedulerRef.Get().Exhausted(accounts.ProviderClaude, "acct@example.com") {
		t.Fatal("a 529 overload must NOT mark the account exhausted")
	}
}

func TestClaudeRateLimitHeaderFields(t *testing.T) {
	header := http.Header{}
	header.Set("Retry-After", "3600")
	header.Set("Anthropic-Ratelimit-Unified-Reset", "1750000000")
	header.Set("Anthropic-Ratelimit-Unified-Status", "rejected")
	header.Set("Content-Type", "application/json")
	header.Set("X-Should-Not-Appear", "secret")

	fields := claudeRateLimitHeaderFields(header)
	got := map[string]string{}
	for i := 0; i+1 < len(fields); i += 2 {
		got[fields[i].(string)] = fields[i+1].(string)
	}
	if got["retry-after"] != "3600" {
		t.Fatalf("retry-after = %q, want 3600", got["retry-after"])
	}
	if got["anthropic-ratelimit-unified-reset"] != "1750000000" {
		t.Fatalf("reset = %q, want 1750000000", got["anthropic-ratelimit-unified-reset"])
	}
	if _, ok := got["content-type"]; ok {
		t.Fatal("content-type should not be captured as a rate-limit field")
	}
	if _, ok := got["x-should-not-appear"]; ok {
		t.Fatal("unrelated headers must not be captured")
	}
}

func TestClientRemoteIP(t *testing.T) {
	mk := func(remote, xff string) *http.Request {
		r, _ := http.NewRequest(http.MethodPost, "http://x/v1/messages", nil)
		r.RemoteAddr = remote
		if xff != "" {
			r.Header.Set("X-Forwarded-For", xff)
		}
		return r
	}
	cases := []struct{ remote, xff, want string }{
		{"100.94.126.75:41562", "", "100.94.126.75"},
		{"[fd7a:115c:a1e0::1]:443", "", "fd7a:115c:a1e0::1"},
		{"noport", "", "noport"},
		// X-Forwarded-For is spoofable and must be ignored; the socket peer wins.
		{"100.94.126.75:41562", "100.1.2.3", "100.94.126.75"},
	}
	for _, tc := range cases {
		if got := clientRemoteIP(mk(tc.remote, tc.xff)); got != tc.want {
			t.Fatalf("clientRemoteIP(remote=%q xff=%q) = %q, want %q", tc.remote, tc.xff, got, tc.want)
		}
	}
}

// recordSleep returns a sleep fn that records waits without actually sleeping.
func recordSleep(waits *[]time.Duration) func(context.Context, time.Duration) error {
	return func(ctx context.Context, d time.Duration) error {
		*waits = append(*waits, d)
		return ctx.Err()
	}
}

// TestClaudeOverloadRetrySucceeds: a brief 529 blip is absorbed on the SAME
// account with backoff; the client sees the eventual 200 and the account is
// never marked exhausted.
func TestClaudeOverloadRetrySucceeds(t *testing.T) {
	server, store := claudeFailoverServer(t)
	if _, err := store.Put("claude", "session-529", "cooked@example.com", ""); err != nil {
		t.Fatal(err)
	}
	var calls int
	stub := &stubRoundTripper{responses: func(req *http.Request) *http.Response {
		calls++
		if calls <= 2 {
			return &http.Response{StatusCode: 529, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"type":"error","error":{"type":"overloaded_error"}}`))}
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"id":"msg_ok"}`))}
	}}
	var waits []time.Duration
	transport := usageLimitRetryTransport{
		base: stub, server: &server, provider: accounts.ProviderClaude,
		agent: "claude", session: "session-529", account: "cooked@example.com",
		method: http.MethodPost, path: "/v1/messages", maxAttempts: 6,
		sleep: recordSleep(&waits),
	}
	req, _ := http.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages", bytes.NewReader([]byte(`{"model":"claude-opus-4-8"}`)))
	req.Header.Set("Authorization", "Bearer tok-cooked")
	response, err := transport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 after overload retries", response.StatusCode)
	}
	if calls != 3 {
		t.Fatalf("upstream calls = %d, want 3 (529, 529, 200)", calls)
	}
	if len(waits) != 2 || waits[0] != time.Second || waits[1] != 2*time.Second {
		t.Fatalf("backoff waits = %v, want [1s 2s]", waits)
	}
	if server.SchedulerRef.Get().Exhausted(accounts.ProviderClaude, "cooked@example.com") {
		t.Fatal("an overload must NOT mark the account exhausted")
	}
}

// TestClaudeOverloadRetryGivesUpAfterBudget: a sustained outage passes the 5xx
// through after the small retry budget so the client's own backoff takes over.
func TestClaudeOverloadRetryGivesUpAfterBudget(t *testing.T) {
	server, _ := claudeFailoverServer(t)
	var calls int
	stub := &stubRoundTripper{responses: func(req *http.Request) *http.Response {
		calls++
		return &http.Response{StatusCode: 529, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"type":"error","error":{"type":"overloaded_error"}}`))}
	}}
	var waits []time.Duration
	transport := usageLimitRetryTransport{
		base: stub, server: &server, provider: accounts.ProviderClaude,
		agent: "claude", session: "s", account: "cooked@example.com",
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
	if response.StatusCode != 529 {
		t.Fatalf("status = %d, want 529 passed through after budget", response.StatusCode)
	}
	if calls != 1+claudeOverloadMaxRetries {
		t.Fatalf("upstream calls = %d, want %d", calls, 1+claudeOverloadMaxRetries)
	}
}

// TestClaudeOverloadRetryPreservesFailoverAccount is the header-revert
// regression: after failover moved the request to a second account, an overload
// retry must keep that account's auth, not silently revert to the first.
func TestClaudeOverloadRetryPreservesFailoverAccount(t *testing.T) {
	server, store := claudeFailoverServer(t)
	if _, err := store.Put("claude", "session-mix", "cooked@example.com", ""); err != nil {
		t.Fatal(err)
	}
	var freshCalls int
	var authsSeen []string
	stub := &stubRoundTripper{responses: func(req *http.Request) *http.Response {
		auth := req.Header.Get("Authorization")
		authsSeen = append(authsSeen, auth)
		if strings.Contains(auth, "tok-cooked") {
			h := http.Header{}
			h.Set("Anthropic-Ratelimit-Unified-Status", "rejected")
			return &http.Response{StatusCode: http.StatusTooManyRequests, Header: h, Body: io.NopCloser(strings.NewReader(`{"type":"error","error":{"type":"rate_limit_error"}}`))}
		}
		freshCalls++
		if freshCalls == 1 {
			return &http.Response{StatusCode: 529, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"type":"error","error":{"type":"overloaded_error"}}`))}
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"id":"msg_fresh"}`))}
	}}
	var waits []time.Duration
	transport := usageLimitRetryTransport{
		base: stub, server: &server, provider: accounts.ProviderClaude,
		agent: "claude", session: "session-mix", account: "cooked@example.com",
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
		t.Fatalf("status=%d body=%s, want fresh 200 (429 -> failover -> 529 -> retry same fresh account)", response.StatusCode, string(body))
	}
	// Sequence must be cooked, fresh (529), fresh (200): the overload retry
	// stays on the failover account.
	if len(authsSeen) != 3 || !strings.Contains(authsSeen[1], "tok-fresh") || !strings.Contains(authsSeen[2], "tok-fresh") {
		t.Fatalf("auth sequence = %v, want cooked then fresh twice", authsSeen)
	}
}

func TestClaudeOverloadBackoff(t *testing.T) {
	h := http.Header{}
	if d := claudeOverloadBackoff(h, 0); d != time.Second {
		t.Fatalf("retry0 = %v, want 1s", d)
	}
	if d := claudeOverloadBackoff(h, 1); d != 2*time.Second {
		t.Fatalf("retry1 = %v, want 2s", d)
	}
	h.Set("Retry-After", "3")
	if d := claudeOverloadBackoff(h, 0); d != 3*time.Second {
		t.Fatalf("retry-after 3 = %v, want 3s", d)
	}
	h.Set("Retry-After", "9999")
	if d := claudeOverloadBackoff(h, 0); d != claudeOverloadMaxWait {
		t.Fatalf("retry-after 9999 = %v, want cap %v", d, claudeOverloadMaxWait)
	}
	if !claudeOverloadStatus(529) || !claudeOverloadStatus(500) || claudeOverloadStatus(429) || claudeOverloadStatus(200) {
		t.Fatal("claudeOverloadStatus classification wrong")
	}
}

// TestClaudeRejected5xxFailsOverNotOverloadRetried: a 5xx that carries the
// rejected unified-status header is a depleted account, not API overload, so it
// must fail over to a healthy account instead of burning same-account retries.
func TestClaudeRejected5xxFailsOverNotOverloadRetried(t *testing.T) {
	server, store := claudeFailoverServer(t)
	if _, err := store.Put("claude", "session-r5", "cooked@example.com", ""); err != nil {
		t.Fatal(err)
	}
	var cookedCalls, freshCalls int
	stub := &stubRoundTripper{responses: func(req *http.Request) *http.Response {
		if strings.Contains(req.Header.Get("Authorization"), "tok-cooked") {
			cookedCalls++
			h := http.Header{}
			h.Set("Anthropic-Ratelimit-Unified-Status", "rejected")
			return &http.Response{StatusCode: http.StatusInternalServerError, Header: h, Body: io.NopCloser(strings.NewReader(`{"type":"error"}`))}
		}
		freshCalls++
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"id":"msg_fresh"}`))}
	}}
	var waits []time.Duration
	transport := usageLimitRetryTransport{
		base: stub, server: &server, provider: accounts.ProviderClaude,
		agent: "claude", session: "session-r5", account: "cooked@example.com",
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
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 via failover", response.StatusCode)
	}
	if cookedCalls != 1 || freshCalls != 1 {
		t.Fatalf("calls cooked=%d fresh=%d, want 1/1 (failover, not same-account overload retry)", cookedCalls, freshCalls)
	}
	if len(waits) != 0 {
		t.Fatalf("overload backoff waits = %v, want none for a rejected 5xx", waits)
	}
	if !server.SchedulerRef.Get().Exhausted(accounts.ProviderClaude, "cooked@example.com") {
		t.Fatal("a rejected 5xx must mark the depleted account exhausted")
	}
}
