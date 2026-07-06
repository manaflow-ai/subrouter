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
	"strconv"
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
	scheduler := schedulerRef.Get()
	if !scheduler.ForModel(agentclaude.OpusFeature).Exhausted(accounts.ProviderClaude, "cooked@example.com") {
		t.Fatal("cooked account should be marked exhausted for the Opus pool by the 429")
	}
	if scheduler.Exhausted(accounts.ProviderClaude, "cooked@example.com") {
		t.Fatal("Opus 429 should not mark the base account exhausted")
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

func TestHTTPProxyDoesNotForwardClientIPHeaders(t *testing.T) {
	var upstreamHeaders http.Header
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		upstreamHeaders = r.Header.Clone()
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"msg_ok","type":"message"}`))
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
	schedulerRef := selectacct.NewSchedulerRef(selectacct.NewScheduler([]selectacct.Score{
		{AccountID: "claude@example.com", Provider: accounts.ProviderClaude, Headroom: 1, ShortHeadroom: 1},
	}))
	handler := Server{
		ClaudeUpstream: upstreamURL,
		Accounts: []accounts.Account{
			{ID: "claude@example.com", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "tok-claude"},
		},
		Sessions:      store,
		SchedulerRef:  schedulerRef,
		UsageScoreTTL: 0,
		MaxBodyBytes:  1 << 20,
	}.Handler()
	subrouter := httptest.NewServer(handler)
	defer subrouter.Close()

	req, err := http.NewRequest(http.MethodPost, subrouter.URL+"/v1/messages", strings.NewReader(`{"model":"claude-opus-4-8","messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Subrouter-Agent", "claude")
	req.Header.Set("X-Subrouter-Session", "session-forwarded")
	req.Header.Set("Forwarded", "for=203.0.113.7")
	req.Header.Set("X-Forwarded-For", "203.0.113.7")
	req.Header.Set("X-Forwarded-Host", "attacker.example")
	req.Header.Set("X-Forwarded-Proto", "http")
	req.Header.Set("X-Real-IP", "203.0.113.8")
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("status=%d body=%s, want 200", response.StatusCode, string(body))
	}
	for _, header := range []string{"Forwarded", "X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto", "X-Forwarded-Ssl", "X-Real-IP"} {
		if got := upstreamHeaders.Get(header); got != "" {
			t.Fatalf("upstream %s = %q, want stripped; headers=%v", header, got, upstreamHeaders)
		}
	}
}

func TestClaude429FailoverTriesPastDefaultAttemptBudget(t *testing.T) {
	var hits []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		account := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer tok-")
		hits = append(hits, account)
		if account == "fresh-7@example.com" {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"id":"msg_fresh","type":"message"}`))
			return
		}
		w.Header().Set("Anthropic-Ratelimit-Unified-Status", "rejected")
		w.Header().Set("Anthropic-Ratelimit-Unified-Reset", strconv.FormatInt(time.Now().Add(24*time.Hour).Unix(), 10))
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(realisticAnthropic429Body))
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
	if _, err := store.Put("claude", "session-many", "cooked-0@example.com", ""); err != nil {
		t.Fatal(err)
	}
	accountsList := make([]accounts.Account, 0, 8)
	scores := make([]selectacct.Score, 0, 8)
	for i := 0; i < 8; i++ {
		id := fmt.Sprintf("cooked-%d@example.com", i)
		if i == 7 {
			id = "fresh-7@example.com"
		}
		accountsList = append(accountsList, accounts.Account{ID: id, Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "tok-" + id})
		headroom := 0.90 - float64(i)*0.01
		scores = append(scores, selectacct.Score{AccountID: id, Provider: accounts.ProviderClaude, Headroom: headroom, ShortHeadroom: headroom})
	}

	handler := Server{
		ClaudeUpstream: upstreamURL,
		Accounts:       accountsList,
		Sessions:       store,
		SchedulerRef:   selectacct.NewSchedulerRef(selectacct.NewScheduler(scores)),
		UsageScoreTTL:  0,
		MaxBodyBytes:   1 << 20,
	}.Handler()
	subrouter := httptest.NewServer(handler)
	defer subrouter.Close()

	req, err := http.NewRequest(http.MethodPost, subrouter.URL+"/v1/messages", strings.NewReader(`{"model":"claude-fable-5","messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Subrouter-Agent", "claude")
	req.Header.Set("X-Subrouter-Session", "session-many")
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	if response.StatusCode != http.StatusOK || !strings.Contains(string(body), "msg_fresh") {
		t.Fatalf("status=%d body=%s hits=%v, want failover to reach the eighth account", response.StatusCode, string(body), hits)
	}
	if len(hits) != 8 {
		t.Fatalf("hits=%v, want all 8 accounts tried before success", hits)
	}
	if hits[len(hits)-1] != "fresh-7@example.com" {
		t.Fatalf("last hit=%q, want fresh-7@example.com; hits=%v", hits[len(hits)-1], hits)
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

func TestClaudeUsageWindowsIncludeOAuthAppsWeekly(t *testing.T) {
	usage := &agentclaude.UsageResponse{
		FiveHour:          &agentclaude.RateLimit{Utilization: floatPtr(0), ResetsAt: time.Now().Add(time.Hour).Format(time.RFC3339)},
		SevenDay:          &agentclaude.RateLimit{Utilization: floatPtr(60), ResetsAt: time.Now().Add(5 * 24 * time.Hour).Format(time.RFC3339)},
		SevenDayOAuthApps: &agentclaude.RateLimit{Utilization: floatPtr(100), ResetsAt: time.Now().Add(5 * 24 * time.Hour).Format(time.RFC3339)},
	}
	windows := claudeUsageWindows(usage)

	var oauthApps *accounts.UsageWindow
	for i := range windows {
		if windows[i].Name == "oauth-apps-weekly" {
			oauthApps = &windows[i]
			break
		}
	}
	if oauthApps == nil {
		t.Fatalf("missing oauth-apps-weekly window: %+v", windows)
	}
	if oauthApps.LimitWindowSeconds != 7*24*60*60 {
		t.Fatalf("oauth apps LimitWindowSeconds = %d, want %d", oauthApps.LimitWindowSeconds, 7*24*60*60)
	}
	if oauthApps.Feature != agentclaude.FableFeature {
		t.Fatalf("oauth apps Feature = %q, want %q", oauthApps.Feature, agentclaude.FableFeature)
	}
	score := scoreFromUsageWindows(accounts.ProviderClaude, "acct", windows)
	if score.Headroom == 0 {
		t.Fatalf("base Headroom = %.3f, hidden Fable bucket should not exhaust non-Fable Claude models", score.Headroom)
	}
	fableScore, ok := score.ModelScores[selectacct.ModelKey(agentclaude.FableFeature)]
	if !ok {
		t.Fatalf("missing Fable model score: %+v", score.ModelScores)
	}
	if fableScore.Headroom != 0 {
		t.Fatalf("Fable Headroom = %.3f, want 0 from saturated oauth app weekly bucket", fableScore.Headroom)
	}
	if fableScore.ShortHeadroom != 1 {
		t.Fatalf("Fable ShortHeadroom = %.3f, want 1 from healthy 5h bucket", fableScore.ShortHeadroom)
	}
}

func TestClaudeFableProbeAddsHiddenOAuthAppsWindow(t *testing.T) {
	usageBody := `{"five_hour":{"utilization":10.0,"resets_at":"2030-01-01T00:00:00+00:00"},"seven_day":{"utilization":60.0,"resets_at":"2030-01-02T00:00:00+00:00"}}`
	probeHeader := http.Header{}
	probeHeader.Set("Anthropic-Ratelimit-Unified-5h-Status", "allowed")
	probeHeader.Set("Anthropic-Ratelimit-Unified-5h-Utilization", "0.1")
	probeHeader.Set("Anthropic-Ratelimit-Unified-5h-Reset", fmt.Sprintf("%d", time.Now().Add(time.Hour).Unix()))
	probeHeader.Set("Anthropic-Ratelimit-Unified-7d-Status", "allowed")
	probeHeader.Set("Anthropic-Ratelimit-Unified-7d-Utilization", "0.6")
	probeHeader.Set("Anthropic-Ratelimit-Unified-7d-Reset", fmt.Sprintf("%d", time.Now().Add(5*24*time.Hour).Unix()))
	probeHeader.Set("Anthropic-Ratelimit-Unified-7d_Oi-Status", "rejected")
	probeHeader.Set("Anthropic-Ratelimit-Unified-7d_Oi-Utilization", "1.0")
	probeHeader.Set("Anthropic-Ratelimit-Unified-7d_Oi-Reset", fmt.Sprintf("%d", time.Now().Add(5*24*time.Hour).Unix()))
	calls := 0
	client := &http.Client{Transport: proxyRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		switch req.URL.Path {
		case "/api/oauth/usage":
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(usageBody)), Header: http.Header{}}, nil
		case "/v1/messages":
			return &http.Response{StatusCode: http.StatusTooManyRequests, Body: io.NopCloser(strings.NewReader(`{"type":"error"}`)), Header: probeHeader}, nil
		default:
			t.Fatalf("unexpected path %s", req.URL.Path)
			return nil, nil
		}
	})}
	windows, err := fetchAccountUsageWindowsLive(context.Background(), client, accounts.Account{
		ID:       "acct",
		Provider: accounts.ProviderClaude,
		AuthMode: accounts.AuthModeOAuth,
		Token:    "tok",
	})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("calls = %d, want usage fetch plus Fable probe", calls)
	}
	var fable *accounts.UsageWindow
	for i := range windows {
		if windows[i].Name == "oauth-apps-weekly" {
			fable = &windows[i]
			break
		}
	}
	if fable == nil {
		t.Fatalf("missing Fable window: %+v", windows)
	}
	if fable.UsedPercent != 100 {
		t.Fatalf("Fable UsedPercent = %.1f, want 100", fable.UsedPercent)
	}
	if fable.Feature != agentclaude.FableFeature {
		t.Fatalf("Fable Feature = %q, want %q", fable.Feature, agentclaude.FableFeature)
	}
}

// A headerless 429 from the probe is a transient burst or bot-shape
// rejection, never authoritative quota evidence: a fresh Max 20x account with
// 0.0 utilization 429'd the bare probe live while answering 200 (with 7d_oi
// headers) once the request looked like Claude Code. It must NOT synthesize a
// cooked fable pool.
func TestClaudeFableProbeIgnoresHeaderless429(t *testing.T) {
	usageBody := `{"five_hour":{"utilization":10.0,"resets_at":"2030-01-01T00:00:00+00:00"},"seven_day":{"utilization":60.0,"resets_at":"2030-01-02T00:00:00+00:00"}}`
	client := &http.Client{Transport: proxyRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		switch req.URL.Path {
		case "/api/oauth/usage":
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(usageBody)), Header: http.Header{}}, nil
		case "/v1/messages":
			return &http.Response{StatusCode: http.StatusTooManyRequests, Body: io.NopCloser(strings.NewReader(`{"type":"error","error":{"type":"rate_limit_error","message":"Error"}}`)), Header: http.Header{}}, nil
		default:
			t.Fatalf("unexpected path %s", req.URL.Path)
			return nil, nil
		}
	})}
	windows, err := fetchAccountUsageWindowsLive(context.Background(), client, accounts.Account{
		ID:       "acct",
		Provider: accounts.ProviderClaude,
		AuthMode: accounts.AuthModeOAuth,
		Token:    "tok",
	})
	if err != nil {
		t.Fatal(err)
	}
	score := scoreFromUsageWindows(accounts.ProviderClaude, "acct", windows)
	if score.Headroom == 0 {
		t.Fatalf("base Headroom = %.3f, a headerless probe 429 must not exhaust the account", score.Headroom)
	}
	if _, ok := score.ModelScores[selectacct.ModelKey(agentclaude.FableFeature)]; ok {
		t.Fatalf("headerless probe 429 must not synthesize a fable pool: %+v", score.ModelScores)
	}
}

func TestUsageStatusFreshFableWindowUpdatesSchedulerModelPool(t *testing.T) {
	schedulerRef := selectacct.NewSchedulerRef(selectacct.NewScheduler([]selectacct.Score{
		{AccountID: "acct", Provider: accounts.ProviderClaude, Headroom: 1, ShortHeadroom: 1},
	}))
	server := Server{
		Accounts: []accounts.Account{{
			ID:       "acct",
			Provider: accounts.ProviderClaude,
			AuthMode: accounts.AuthModeOAuth,
		}},
		SchedulerRef: schedulerRef,
	}
	server.updateSchedulerFromUsageStatuses([]AccountUsageStatus{{
		AccountStatus: AccountStatus{
			ID:       "acct",
			Provider: accounts.ProviderClaude,
			AuthMode: accounts.AuthModeOAuth,
		},
		UsageFresh: true,
		Windows: []accounts.UsageWindow{
			{Name: "5h", UsedPercent: 10, LimitWindowSeconds: 5 * 60 * 60},
			{Name: "7d", UsedPercent: 40, LimitWindowSeconds: 7 * 24 * 60 * 60},
			{Name: agentclaude.FableWindowName, Feature: agentclaude.FableFeature, UsedPercent: 100, LimitWindowSeconds: 7 * 24 * 60 * 60},
		},
	}})
	base := schedulerRef.Get()
	if base.Exhausted(accounts.ProviderClaude, "acct") {
		t.Fatalf("base Claude score should remain usable when only Fable is exhausted")
	}
	// The request path canonicalizes the model to its pool family first.
	fable := base.ForModel(claudePoolModel(agentclaude.FableModel))
	if !fable.Exhausted(accounts.ProviderClaude, "acct") {
		t.Fatalf("Fable model score should be exhausted after fresh status update")
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

func TestClaudeBareRejectedFableMarksOnlyFablePool(t *testing.T) {
	const (
		fableAccount = "a@example.com"
		otherAccount = "z@example.com"
	)
	fableKey := selectacct.ModelKey(agentclaude.FableFeature)
	opusKey := selectacct.ModelKey(agentclaude.OpusFeature)
	server := Server{
		Accounts: []accounts.Account{
			{ID: fableAccount, Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "tok-a"},
			{ID: otherAccount, Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "tok-z"},
		},
		SchedulerRef: selectacct.NewSchedulerRef(selectacct.NewScheduler([]selectacct.Score{
			{
				AccountID: fableAccount, Provider: accounts.ProviderClaude, Headroom: 1, ShortHeadroom: 1,
				ModelScores: map[string]selectacct.Score{
					fableKey: {AccountID: fableAccount, Provider: accounts.ProviderClaude, Headroom: 1, ShortHeadroom: 1},
					opusKey:  {AccountID: fableAccount, Provider: accounts.ProviderClaude, Headroom: 1, ShortHeadroom: 1},
				},
			},
			{
				AccountID: otherAccount, Provider: accounts.ProviderClaude, Headroom: 1, ShortHeadroom: 1,
				ModelScores: map[string]selectacct.Score{
					fableKey: {AccountID: otherAccount, Provider: accounts.ProviderClaude, Headroom: 1, ShortHeadroom: 1},
					opusKey:  {AccountID: otherAccount, Provider: accounts.ProviderClaude, Headroom: 1, ShortHeadroom: 1},
				},
			},
		})),
	}
	stub := &stubRoundTripper{responses: func(req *http.Request) *http.Response {
		h := http.Header{}
		h.Set("Anthropic-Ratelimit-Unified-Status", "rejected")
		return &http.Response{
			StatusCode: http.StatusTooManyRequests,
			Header:     h,
			Body:       io.NopCloser(strings.NewReader(realisticAnthropic429Body)),
		}
	}}
	transport := usageLimitRetryTransport{
		base: stub, server: &server, provider: accounts.ProviderClaude,
		agent: "claude", session: "session-fable", account: fableAccount,
		method: http.MethodPost, path: "/v1/messages", maxAttempts: 1,
		poolModel: agentclaude.FableFeature,
	}
	req, err := http.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages", bytes.NewReader([]byte(`{"model":"claude-fable-5"}`)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer tok-a")
	response, err := transport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status=%d, want original fable rejection", response.StatusCode)
	}

	scheduler := server.SchedulerRef.Get()
	if !scheduler.ForModel(agentclaude.FableFeature).Exhausted(accounts.ProviderClaude, fableAccount) {
		t.Fatal("bare rejected fable response should mark only the fable pool")
	}
	if scheduler.Exhausted(accounts.ProviderClaude, fableAccount) {
		t.Fatal("bare rejected fable response must not mark the base account exhausted")
	}
	if scheduler.ForModel(agentclaude.OpusFeature).Exhausted(accounts.ProviderClaude, fableAccount) {
		t.Fatal("bare rejected fable response must not mark opus exhausted")
	}
	picked, err := scheduler.ForModel(agentclaude.OpusFeature).Pick(server.Accounts)
	if err != nil {
		t.Fatal(err)
	}
	if picked.ID != fableAccount {
		t.Fatalf("opus pick after fable rejection = %q, want same account %q", picked.ID, fableAccount)
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

func TestClaudeFailoverSkipsMarkedExhaustedAlternates(t *testing.T) {
	server, store := claudeFailoverServer(t)
	if _, err := store.Put("claude", "session-exhausted-alt", "cooked@example.com", ""); err != nil {
		t.Fatal(err)
	}
	server.SchedulerRef.MarkExhaustedUntil(accounts.ProviderClaude, "fresh@example.com", "", time.Now().Add(time.Hour))

	var calls int
	stub := &stubRoundTripper{responses: func(req *http.Request) *http.Response {
		calls++
		if strings.Contains(req.Header.Get("Authorization"), "tok-fresh") {
			t.Fatal("fresh account is marked exhausted and must not be retried")
		}
		h := http.Header{}
		h.Set("Anthropic-Ratelimit-Unified-Status", "rejected")
		h.Set("Anthropic-Ratelimit-Unified-Reset", strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10))
		return &http.Response{StatusCode: http.StatusTooManyRequests, Header: h, Body: io.NopCloser(strings.NewReader(`{"type":"error","error":{"type":"rate_limit_error","message":"Error"}}`))}
	}}
	transport := usageLimitRetryTransport{
		base: stub, server: &server, provider: accounts.ProviderClaude,
		agent: "claude", session: "session-exhausted-alt", account: "cooked@example.com",
		method: http.MethodPost, path: "/v1/messages", maxAttempts: 6,
	}
	req, err := http.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages", bytes.NewReader([]byte(`{}`)))
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
		t.Fatalf("status=%d, want original 429", response.StatusCode)
	}
	if calls != 1 {
		t.Fatalf("upstream calls=%d, want 1 because marked-exhausted alternates are skipped", calls)
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
	server.captureResponseBody(response, "claude", "session-1", "acct@example.com", accounts.ProviderClaude, "", "/v1/messages")
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

func TestClaudeExhaustionExpiry(t *testing.T) {
	now := time.Unix(1_750_000_000, 0)
	h := http.Header{}
	// No headers: scheduler default TTL.
	if got := claudeExhaustionExpiry(h, now); !got.Equal(now.Add(selectacct.DefaultExhaustedTTL)) {
		t.Fatalf("default = %v, want now+%v", got, selectacct.DefaultExhaustedTTL)
	}
	// Authoritative unified-reset epoch wins.
	h.Set("Anthropic-Ratelimit-Unified-Reset", "1750003600")
	if got := claudeExhaustionExpiry(h, now); !got.Equal(time.Unix(1_750_003_600, 0)) {
		t.Fatalf("unified-reset = %v, want epoch 1750003600", got)
	}
	// A reset already in the past floors to now+1m (clock skew guard).
	h.Set("Anthropic-Ratelimit-Unified-Reset", "1749990000")
	if got := claudeExhaustionExpiry(h, now); !got.Equal(now.Add(time.Minute)) {
		t.Fatalf("past reset = %v, want floor now+1m", got)
	}
	// Far-future reset is capped at 8d.
	h.Set("Anthropic-Ratelimit-Unified-Reset", "1999999999")
	if got := claudeExhaustionExpiry(h, now); !got.Equal(now.Add(8 * 24 * time.Hour)) {
		t.Fatalf("far reset = %v, want cap now+8d", got)
	}
	// Retry-After honored when unified-reset absent.
	h = http.Header{}
	h.Set("Retry-After", "300")
	if got := claudeExhaustionExpiry(h, now); !got.Equal(now.Add(5 * time.Minute)) {
		t.Fatalf("retry-after = %v, want now+5m", got)
	}
}

// TestClaudeFailoverTriesRecoveredAccount is the live regression: an account
// whose exhaustion mark has lapsed (window reset) must be reachable by failover
// again. Before the fix its zero score persisted until a successful usage
// refresh, so failover burned its attempts on genuinely-cooked accounts and
// never reached the recovered one.
func TestClaudeFailoverTriesRecoveredAccount(t *testing.T) {
	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put("claude", "session-rec", "cooked@example.com", ""); err != nil {
		t.Fatal(err)
	}
	ref := selectacct.NewSchedulerRef(selectacct.NewScheduler(nil))
	// recovered@ was marked exhausted while cooked, but its window has reset.
	ref.MarkExhaustedUntil(accounts.ProviderClaude, "recovered@example.com", "", time.Now().Add(-time.Second))
	server := Server{
		Accounts: []accounts.Account{
			{ID: "cooked@example.com", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "tok-cooked"},
			{ID: "recovered@example.com", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "tok-recovered"},
		},
		Sessions:     store,
		SchedulerRef: ref,
	}
	stub := &stubRoundTripper{responses: func(req *http.Request) *http.Response {
		if strings.Contains(req.Header.Get("Authorization"), "tok-recovered") {
			return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"id":"msg_recovered"}`))}
		}
		h := http.Header{}
		h.Set("Anthropic-Ratelimit-Unified-Status", "rejected")
		h.Set("Anthropic-Ratelimit-Unified-Reset", "1999999999")
		return &http.Response{StatusCode: http.StatusTooManyRequests, Header: h, Body: io.NopCloser(strings.NewReader(`{"type":"error","error":{"type":"rate_limit_error","message":"Error"}}`))}
	}}
	transport := usageLimitRetryTransport{
		base: stub, server: &server, provider: accounts.ProviderClaude,
		agent: "claude", session: "session-rec", account: "cooked@example.com",
		method: http.MethodPost, path: "/v1/messages", maxAttempts: 6,
	}
	req, _ := http.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages", bytes.NewReader([]byte(`{}`)))
	req.Header.Set("Authorization", "Bearer tok-cooked")
	response, err := transport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	if response.StatusCode != http.StatusOK || !strings.Contains(string(body), "msg_recovered") {
		t.Fatalf("status=%d body=%s, want the recovered account to serve after its mark lapsed", response.StatusCode, string(body))
	}
}

// TestMarkTTLSelection pins the TTL class per failure kind: rate-limit marks
// expire at the upstream reset, 401 dead-credential marks get the long
// credential TTL, and re-marking through the passive body-inspect path must not
// shorten a header-derived expiry.
func TestMarkTTLSelection(t *testing.T) {
	server, _ := claudeFailoverServer(t)
	resetEpoch := time.Now().Add(48 * time.Hour).Unix()
	h := http.Header{}
	h.Set("Anthropic-Ratelimit-Unified-Status", "rejected")
	h.Set("Anthropic-Ratelimit-Unified-Reset", strconv.FormatInt(resetEpoch, 10))

	// Rate-limit mark: expiry = upstream reset.
	server.markAccountExhaustedFromResponse(accounts.ProviderClaude, "rl@example.com", "", http.StatusTooManyRequests, h)
	until, ok := server.SchedulerRef.ExhaustedUntilFor(accounts.ProviderClaude, "rl@example.com", "")
	if !ok || !until.Equal(time.Unix(resetEpoch, 0)) {
		t.Fatalf("rate-limit mark until=%v ok=%v, want upstream reset %v", until, ok, time.Unix(resetEpoch, 0))
	}
	// Re-marking with the same headers (the passive body-inspect path) must not
	// shorten the expiry to the default TTL.
	server.markAccountExhaustedFromResponse(accounts.ProviderClaude, "rl@example.com", "", http.StatusTooManyRequests, h)
	until2, _ := server.SchedulerRef.ExhaustedUntilFor(accounts.ProviderClaude, "rl@example.com", "")
	if !until2.Equal(time.Unix(resetEpoch, 0)) {
		t.Fatalf("re-mark shortened expiry to %v, want %v", until2, time.Unix(resetEpoch, 0))
	}

	// 401 dead credential: long credential TTL, not the 10m default.
	server.markAccountExhaustedFromResponse(accounts.ProviderClaude, "dead@example.com", "", http.StatusUnauthorized, http.Header{})
	deadUntil, ok := server.SchedulerRef.ExhaustedUntilFor(accounts.ProviderClaude, "dead@example.com", "")
	if !ok || time.Until(deadUntil) < 50*time.Minute {
		t.Fatalf("401 mark until=%v (in %v), want ~1h credential TTL", deadUntil, time.Until(deadUntil))
	}

	// Terminal refresh failure: credential TTL; transient: short default.
	server.markAccountExhaustedRefreshFailure(accounts.ProviderClaude, "invalid@example.com", "", fmt.Errorf("400 Bad Request: invalid_grant"))
	invUntil, _ := server.SchedulerRef.ExhaustedUntilFor(accounts.ProviderClaude, "invalid@example.com", "")
	if time.Until(invUntil) < 50*time.Minute {
		t.Fatalf("terminal refresh mark in %v, want ~1h", time.Until(invUntil))
	}
	server.markAccountExhaustedRefreshFailure(accounts.ProviderClaude, "blip@example.com", "", fmt.Errorf("dial tcp: connection refused"))
	blipUntil, _ := server.SchedulerRef.ExhaustedUntilFor(accounts.ProviderClaude, "blip@example.com", "")
	if time.Until(blipUntil) > 15*time.Minute {
		t.Fatalf("transient refresh mark in %v, want ~10m default", time.Until(blipUntil))
	}
}

func TestClaudePoolModelAliasesVersionedModels(t *testing.T) {
	cases := map[string]string{
		"claude-fable-5":      agentclaude.FableFeature,
		"claude-fable-5[1m]":  agentclaude.FableFeature,
		"claude-opus-4-8":     agentclaude.OpusFeature,
		"claude-opus-4-8[1m]": agentclaude.OpusFeature,
		"claude-sonnet-5":     agentclaude.SonnetFeature,
		"claude-haiku-4-5":    "claude-haiku-4-5",
		"gpt-5.3-codex-spark": "gpt-5.3-codex-spark",
	}
	for model, want := range cases {
		if got := claudePoolModel(model); got != want {
			t.Fatalf("claudePoolModel(%q) = %q, want %q", model, got, want)
		}
	}
}

// A Fable-only rejection (7d_oi rejected, account windows allowed) must not
// cook the whole account: opus/sonnet can still use it. Before this a wave of
// Fable traffic marked every account exhausted and starved opus routing.
func TestClaudeExhaustedByResponseIgnoresPoolScopedRejection(t *testing.T) {
	poolOnly := http.Header{}
	poolOnly.Set("Anthropic-Ratelimit-Unified-Status", "rejected")
	poolOnly.Set("Anthropic-Ratelimit-Unified-5h-Status", "allowed")
	poolOnly.Set("Anthropic-Ratelimit-Unified-7d-Status", "allowed")
	poolOnly.Set("Anthropic-Ratelimit-Unified-7d_Oi-Status", "rejected")
	if claudeAccountExhaustedByResponse(http.StatusOK, poolOnly) {
		t.Fatal("fable-pool-only rejection must not exhaust the account")
	}

	accountWide := http.Header{}
	accountWide.Set("Anthropic-Ratelimit-Unified-Status", "rejected")
	accountWide.Set("Anthropic-Ratelimit-Unified-7d-Status", "rejected")
	accountWide.Set("Anthropic-Ratelimit-Unified-7d_Oi-Status", "rejected")
	if !claudeAccountExhaustedByResponse(http.StatusOK, accountWide) {
		t.Fatal("7d rejection must exhaust the account")
	}

	bare := http.Header{}
	bare.Set("Anthropic-Ratelimit-Unified-Status", "rejected")
	if !claudeAccountExhaustedByResponse(http.StatusTooManyRequests, bare) {
		t.Fatal("rejected with no per-window detail stays account-wide (conservative)")
	}
}

// Fresh accounts report opus/sonnet weekly buckets as null (unused). They must
// still carry 0%-used pool windows so ForModel never zero-fills them out of
// opus routing once another account grows a real opus window.
func TestClaudeUsageWindowsSynthesizeUnusedOpusSonnetPools(t *testing.T) {
	usage := &agentclaude.UsageResponse{
		FiveHour:          &agentclaude.RateLimit{Utilization: floatPtr(10), ResetsAt: time.Now().Add(time.Hour).Format(time.RFC3339)},
		SevenDay:          &agentclaude.RateLimit{Utilization: floatPtr(20), ResetsAt: time.Now().Add(5 * 24 * time.Hour).Format(time.RFC3339)},
		SevenDayOAuthApps: &agentclaude.RateLimit{Utilization: floatPtr(100), ResetsAt: time.Now().Add(5 * 24 * time.Hour).Format(time.RFC3339)},
	}
	windows := claudeUsageWindows(usage)
	score := scoreFromUsageWindows(accounts.ProviderClaude, "fresh", windows)

	opusScore, ok := score.ModelScores[selectacct.ModelKey(agentclaude.OpusFeature)]
	if !ok {
		t.Fatalf("missing synthesized opus pool: %+v", score.ModelScores)
	}
	// Pool score includes the base windows, so 7d at 20%% caps it at 0.8.
	if opusScore.Headroom < 0.79 || opusScore.Headroom > 0.81 {
		t.Fatalf("opus pool Headroom = %.3f, want ~0.8 (base 7d), fable exhaustion must not leak in", opusScore.Headroom)
	}
	if _, ok := score.ModelScores[selectacct.ModelKey(agentclaude.SonnetFeature)]; !ok {
		t.Fatalf("missing synthesized sonnet pool: %+v", score.ModelScores)
	}
}

// A model pool with quota left is still unusable when the account-wide windows
// are cooked: the pool score must include base windows.
func TestClaudeModelPoolScoresIncludeBaseWindows(t *testing.T) {
	windows := []accounts.UsageWindow{
		{Name: "5h", UsedPercent: 100, LimitWindowSeconds: 5 * 60 * 60},
		{Name: "7d", UsedPercent: 10, LimitWindowSeconds: 7 * 24 * 60 * 60},
		{Name: agentclaude.FableWindowName, Feature: agentclaude.FableFeature, UsedPercent: 0, LimitWindowSeconds: 7 * 24 * 60 * 60},
	}
	score := scoreFromUsageWindows(accounts.ProviderClaude, "acct", windows)
	fableScore, ok := score.ModelScores[selectacct.ModelKey(agentclaude.FableFeature)]
	if !ok {
		t.Fatalf("missing fable pool: %+v", score.ModelScores)
	}
	if fableScore.Headroom != 0 {
		t.Fatalf("fable pool Headroom = %.3f, want 0: the cooked 5h window binds every pool", fableScore.Headroom)
	}
}
