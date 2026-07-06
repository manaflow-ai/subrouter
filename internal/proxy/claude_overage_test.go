package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	agentclaude "github.com/manaflow-ai/subrouter/internal/agents/claude"
	"github.com/manaflow-ai/subrouter/internal/selectacct"
	"github.com/manaflow-ai/subrouter/internal/session"
)

func TestNormalizeClaudeOverageAccounts(t *testing.T) {
	accounts := NormalizeClaudeOverageAccounts(" Lawrence@Manaflow.AI , other-profile ,,LAWRENCE@MANAFLOW.AI ")
	if !accounts["lawrence@manaflow.ai"] {
		t.Fatalf("missing normalized opt-in account: %+v", accounts)
	}
	if !accounts["other-profile"] {
		t.Fatalf("missing second opt-in account: %+v", accounts)
	}
	if len(accounts) != 2 {
		t.Fatalf("accounts = %+v, want two deduped entries", accounts)
	}

	t.Setenv("SUBROUTER_CLAUDE_OVERAGE_ACCOUNTS", " Lawrence@Manaflow.AI ")
	server := Server{ClaudeOverageAccounts: NormalizeClaudeOverageAccounts(os.Getenv("SUBROUTER_CLAUDE_OVERAGE_ACCOUNTS"))}
	if !server.ClaudeOverageAccounts["lawrence@manaflow.ai"] || !server.claudeOverageOptIn(" LAWRENCE@MANAFLOW.AI ") {
		t.Fatalf("env-derived Server.ClaudeOverageAccounts = %+v", server.ClaudeOverageAccounts)
	}
}

func TestClaudeOverageOptInPassesThroughAndLogsCost(t *testing.T) {
	var cookedHits, freshHits int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		auth := r.Header.Get("Authorization")
		switch {
		case strings.Contains(auth, "tok-cooked"):
			cookedHits++
			w.Header().Set("Retry-After", "3600")
			w.Header().Set("Anthropic-Ratelimit-Unified-Status", "rejected")
			w.Header().Set("Anthropic-Ratelimit-Unified-5h-Status", "rejected")
			w.Header().Set("Anthropic-Ratelimit-Unified-7d-Status", "allowed")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"id":"msg_overage","type":"message","model":"claude-fable-5","usage":{"input_tokens":11,"output_tokens":22,"cache_creation_input_tokens":3,"cache_read_input_tokens":4}}`))
		case strings.Contains(auth, "tok-fresh"):
			freshHits++
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"id":"unexpected_failover"}`))
		default:
			w.WriteHeader(http.StatusBadRequest)
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
	if _, err := store.Put("claude", "session-overage", "cooked@example.com", ""); err != nil {
		t.Fatal(err)
	}
	schedulerRef := selectacct.NewSchedulerRef(selectacct.NewScheduler([]selectacct.Score{
		{AccountID: "cooked@example.com", Provider: accounts.ProviderClaude, Headroom: 1, ShortHeadroom: 1},
		{AccountID: "fresh@example.com", Provider: accounts.ProviderClaude, Headroom: 1, ShortHeadroom: 1},
	}))
	var logBuf bytes.Buffer
	costLog := filepath.Join(t.TempDir(), "claude-overage.jsonl")
	handler := Server{
		ClaudeUpstream: upstreamURL,
		Accounts: []accounts.Account{
			{ID: "cooked@example.com", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "tok-cooked"},
			{ID: "fresh@example.com", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "tok-fresh"},
		},
		Sessions:                 store,
		SchedulerRef:             schedulerRef,
		UsageScoreTTL:            0,
		MaxBodyBytes:             1 << 20,
		Logger:                   slog.New(slog.NewTextHandler(&logBuf, nil)),
		ClaudeOverageAccounts:    NormalizeClaudeOverageAccounts(" cooked@example.com "),
		ClaudeOverageCostLogPath: costLog,
	}.Handler()
	subrouter := httptest.NewServer(handler)
	defer subrouter.Close()

	req, err := http.NewRequest(http.MethodPost, subrouter.URL+"/v1/messages", strings.NewReader(`{"model":"claude-fable-5","messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Subrouter-Agent", "claude")
	req.Header.Set("X-Subrouter-Session", "session-overage")
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()

	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d body=%s, want overage 200", response.StatusCode, body)
	}
	if cookedHits != 1 || freshHits != 0 {
		t.Fatalf("upstream hits cooked=%d fresh=%d, want 1/0", cookedHits, freshHits)
	}
	for _, header := range []string{
		"Retry-After",
		"Anthropic-Ratelimit-Unified-Status",
		"Anthropic-Ratelimit-Unified-5h-Status",
		"Anthropic-Ratelimit-Unified-7d-Status",
		claudeOverageAccountHeader,
	} {
		if got := response.Header.Get(header); got != "" {
			t.Fatalf("client header %s = %q, want stripped", header, got)
		}
	}
	if schedulerRef.Get().Exhausted(accounts.ProviderClaude, "cooked@example.com") {
		t.Fatal("overage-served opt-in account should remain routable")
	}
	if !strings.Contains(logBuf.String(), "claude overage served") {
		t.Fatalf("missing overage served log; logs=\n%s", logBuf.String())
	}
	data, err := os.ReadFile(costLog)
	if err != nil {
		t.Fatal(err)
	}
	var record map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(data), &record); err != nil {
		t.Fatalf("cost log JSON = %q: %v", data, err)
	}
	if record["account"] != "cooked@example.com" || record["status"].(float64) != 200 {
		t.Fatalf("cost record account/status = %+v", record)
	}
	if record["input_tokens"].(float64) != 11 || record["output_tokens"].(float64) != 22 ||
		record["cache_creation_input_tokens"].(float64) != 3 || record["cache_read_input_tokens"].(float64) != 4 {
		t.Fatalf("cost record tokens = %+v", record)
	}
}

func TestClaudeOverageFailoverUsesServingAccountIdentity(t *testing.T) {
	server, store := claudeFailoverServer(t)
	server.ClaudeOverageAccounts = NormalizeClaudeOverageAccounts("fresh@example.com")
	if _, err := store.Put("claude", "session-overage-failover", "cooked@example.com", ""); err != nil {
		t.Fatal(err)
	}
	var cookedCalls, freshCalls int
	stub := &stubRoundTripper{responses: func(req *http.Request) *http.Response {
		switch req.Header.Get("Authorization") {
		case "Bearer tok-cooked":
			cookedCalls++
			h := http.Header{}
			h.Set("Anthropic-Ratelimit-Unified-Status", "rejected")
			return &http.Response{StatusCode: http.StatusTooManyRequests, Header: h, Body: io.NopCloser(strings.NewReader(realisticAnthropic429Body))}
		case "Bearer tok-fresh":
			freshCalls++
			h := http.Header{}
			h.Set("Retry-After", "3600")
			h.Set("Anthropic-Ratelimit-Unified-Status", "rejected")
			h.Set("Anthropic-Ratelimit-Unified-5h-Status", "rejected")
			return &http.Response{StatusCode: http.StatusOK, Header: h, Body: io.NopCloser(strings.NewReader(`{"id":"msg_overage"}`))}
		default:
			return &http.Response{StatusCode: http.StatusBadRequest, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`bad auth`))}
		}
	}}
	body := []byte(`{"model":"claude-opus-4-8","messages":[]}`)
	req, err := http.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(body)), nil }
	req.Header.Set("Authorization", "Bearer tok-cooked")
	transport := usageLimitRetryTransport{
		base:        stub,
		server:      &server,
		provider:    accounts.ProviderClaude,
		agent:       "claude",
		session:     "session-overage-failover",
		account:     "cooked@example.com",
		method:      http.MethodPost,
		path:        "/v1/messages",
		maxAttempts: 4,
	}
	response, err := transport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want opt-in overage 200", response.StatusCode)
	}
	if cookedCalls != 1 || freshCalls != 1 {
		t.Fatalf("calls cooked=%d fresh=%d, want 1/1", cookedCalls, freshCalls)
	}
	if got := response.Header.Get("Anthropic-Ratelimit-Unified-Status"); got != "" {
		t.Fatalf("unified status leaked from active overage path: %q", got)
	}
	if got := response.Header.Get(claudeOverageAccountHeader); got != "fresh@example.com" {
		t.Fatalf("marker account = %q, want fresh@example.com", got)
	}
	if server.SchedulerRef.Get().Exhausted(accounts.ProviderClaude, "fresh@example.com") {
		t.Fatal("serving opt-in account should not be exhausted")
	}
}

func TestClaudeOverageOptIn429FallsThroughToFableFallback(t *testing.T) {
	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	server := Server{
		Accounts: []accounts.Account{
			{ID: "overage@example.com", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "tok-overage"},
		},
		Sessions:              store,
		SchedulerRef:          selectacct.NewSchedulerRef(selectacct.NewScheduler([]selectacct.Score{{AccountID: "overage@example.com", Provider: accounts.ProviderClaude, Headroom: 1, ShortHeadroom: 1}})),
		ClaudeOverageAccounts: NormalizeClaudeOverageAccounts("overage@example.com"),
	}
	stub := &stubRoundTripper{responses: func(req *http.Request) *http.Response {
		h := http.Header{}
		h.Set("Anthropic-Ratelimit-Unified-Status", "rejected")
		return &http.Response{StatusCode: http.StatusTooManyRequests, Header: h, Body: io.NopCloser(strings.NewReader(realisticAnthropic429Body))}
	}}
	body := []byte(`{"model":"claude-fable-5","messages":[]}`)
	req, err := http.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(body)), nil }
	req.Header.Set("Authorization", "Bearer tok-overage")
	fallbackCalled := false
	transport := usageLimitRetryTransport{
		base:        stub,
		server:      &server,
		provider:    accounts.ProviderClaude,
		agent:       "claude",
		session:     "session-fable-overage",
		account:     "overage@example.com",
		method:      http.MethodPost,
		path:        "/v1/messages",
		maxAttempts: 4,
		poolModel:   agentclaude.FableFeature,
		fableFallback: func() (*http.Response, bool) {
			fallbackCalled = true
			return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"type":"message","source":"fallback"}`))}, true
		},
	}
	response, err := transport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK || !fallbackCalled {
		t.Fatalf("status=%d fallbackCalled=%v, want fable fallback 200", response.StatusCode, fallbackCalled)
	}
	if !server.SchedulerRef.Get().ForModel(agentclaude.FableFeature).Exhausted(accounts.ProviderClaude, "overage@example.com") {
		t.Fatal("429 rejected should still mark the account exhausted for the request pool")
	}
}

func TestClaudeOverageNonReplayableRequestStillStripsAndReturns(t *testing.T) {
	server := Server{ClaudeOverageAccounts: NormalizeClaudeOverageAccounts("overage@example.com")}
	stub := &stubRoundTripper{responses: func(req *http.Request) *http.Response {
		h := http.Header{}
		h.Set("Retry-After", "3600")
		h.Set("Anthropic-Ratelimit-Unified-Status", "rejected")
		return &http.Response{StatusCode: http.StatusOK, Header: h, Body: io.NopCloser(strings.NewReader(`{"id":"msg_overage"}`))}
	}}
	req, err := http.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages", io.NopCloser(strings.NewReader(`{}`)))
	if err != nil {
		t.Fatal(err)
	}
	req.GetBody = nil
	transport := usageLimitRetryTransport{
		base:     stub,
		server:   &server,
		provider: accounts.ProviderClaude,
		account:  "overage@example.com",
	}
	response, err := transport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if stub.calls != 1 || response.StatusCode != http.StatusOK {
		t.Fatalf("calls=%d status=%d, want one accepted overage response", stub.calls, response.StatusCode)
	}
	if got := response.Header.Get("Retry-After"); got != "" {
		t.Fatalf("Retry-After = %q, want stripped", got)
	}
	if got := response.Header.Get(claudeOverageAccountHeader); got != "overage@example.com" {
		t.Fatalf("marker = %q, want overage account", got)
	}
}

func TestClaudeOverageFloorKeepsOptInScoresEligible(t *testing.T) {
	windows := []accounts.UsageWindow{
		{Name: "5h", UsedPercent: 100, LimitWindowSeconds: 5 * 60 * 60},
		{Name: "7d", UsedPercent: 100, LimitWindowSeconds: 7 * 24 * 60 * 60},
		{Name: agentclaude.FableWindowName, Feature: agentclaude.FableFeature, UsedPercent: 100, LimitWindowSeconds: 7 * 24 * 60 * 60},
		{Name: "extra", UsedPercent: 25},
	}
	base := scoreFromUsageWindows(accounts.ProviderClaude, "acct@example.com", windows)
	if !selectacct.NewScheduler([]selectacct.Score{base}).Exhausted(accounts.ProviderClaude, "acct@example.com") {
		t.Fatal("non-opt-in base score should stay exhausted")
	}
	optIn := applyClaudeOverageFloor(base, windows)
	if optIn.Headroom < claudeOverageFloorHeadroom || optIn.ShortHeadroom < claudeOverageFloorHeadroom {
		t.Fatalf("base floor not applied: %+v", optIn)
	}
	fableScore, ok := optIn.ModelScores[selectacct.ModelKey(agentclaude.FableFeature)]
	if !ok {
		t.Fatalf("missing fable score: %+v", optIn.ModelScores)
	}
	if fableScore.Headroom < claudeOverageFloorHeadroom || fableScore.ShortHeadroom < claudeOverageFloorHeadroom {
		t.Fatalf("model floor not applied: %+v", fableScore)
	}
	if selectacct.NewScheduler([]selectacct.Score{optIn}).ForModel(agentclaude.FableFeature).Exhausted(accounts.ProviderClaude, "acct@example.com") {
		t.Fatal("opt-in score with extra usage headroom should not be exhausted")
	}

	usedExtra := append([]accounts.UsageWindow(nil), windows...)
	usedExtra[len(usedExtra)-1].UsedPercent = 100
	cooked := applyClaudeOverageFloor(scoreFromUsageWindows(accounts.ProviderClaude, "acct@example.com", usedExtra), usedExtra)
	if !selectacct.NewScheduler([]selectacct.Score{cooked}).ForModel(agentclaude.FableFeature).Exhausted(accounts.ProviderClaude, "acct@example.com") {
		t.Fatal("extra usage at 100% should not get the floor")
	}
}

func TestServerAppliesClaudeOverageFloorOnlyForOptInRoutingScores(t *testing.T) {
	windows := []accounts.UsageWindow{
		{Name: "5h", UsedPercent: 100, LimitWindowSeconds: 5 * 60 * 60},
		{Name: "7d", UsedPercent: 100, LimitWindowSeconds: 7 * 24 * 60 * 60},
		{Name: agentclaude.FableWindowName, Feature: agentclaude.FableFeature, UsedPercent: 100, LimitWindowSeconds: 7 * 24 * 60 * 60},
		{Name: "extra", UsedPercent: 10},
	}
	server := Server{
		Accounts: []accounts.Account{
			{ID: "optin@example.com", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth},
			{ID: "plain@example.com", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth},
		},
		SchedulerRef:          selectacct.NewSchedulerRef(selectacct.NewScheduler(nil)),
		ClaudeOverageAccounts: NormalizeClaudeOverageAccounts(" optin@example.com "),
	}
	server.updateSchedulerFromUsageStatuses([]AccountUsageStatus{
		{
			AccountStatus: AccountStatus{ID: "optin@example.com", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth},
			UsageFresh:    true,
			Windows:       windows,
		},
		{
			AccountStatus: AccountStatus{ID: "plain@example.com", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth},
			UsageFresh:    true,
			Windows:       windows,
		},
	})
	scheduler := server.SchedulerRef.Get()
	if scheduler.ForModel(agentclaude.FableFeature).Exhausted(accounts.ProviderClaude, "optin@example.com") {
		t.Fatal("opt-in routing score should receive the overage floor")
	}
	if !scheduler.ForModel(agentclaude.FableFeature).Exhausted(accounts.ProviderClaude, "plain@example.com") {
		t.Fatal("non-opt-in routing score should remain exhausted")
	}
}

func TestParseClaudeOverageSSEUsage(t *testing.T) {
	body := "" +
		"event: message_start\n" +
		"data: {\"type\":\"message_start\",\"message\":{\"model\":\"claude-opus-4-8\",\"usage\":{\"input_tokens\":7,\"cache_read_input_tokens\":2}}}\n\n" +
		"event: message_delta\n" +
		"data: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":9}}\n\n"
	model, usage, ok := parseClaudeOverageUsage([]byte(body))
	if !ok {
		t.Fatal("expected SSE usage")
	}
	if model != "claude-opus-4-8" || usage.InputTokens != 7 || usage.OutputTokens != 9 || usage.CacheReadInputTokens != 2 {
		t.Fatalf("model=%q usage=%+v", model, usage)
	}
}

func TestCaptureResponseBodyPassiveClaudeOverageStripsAndSkipsExhaustion(t *testing.T) {
	server, _ := claudeFailoverServer(t)
	server.ClaudeOverageAccounts = NormalizeClaudeOverageAccounts("cooked@example.com")
	response := &http.Response{
		StatusCode: http.StatusOK,
		Header: http.Header{
			"Anthropic-Ratelimit-Unified-Status": []string{"rejected"},
			"Retry-After":                        []string{"3600"},
		},
		Body: io.NopCloser(strings.NewReader(`{"id":"msg_overage"}`)),
	}
	server.captureResponseBody(response, "claude", "session-1", "cooked@example.com", accounts.ProviderClaude, "", "/v1/messages")
	_, _ = io.Copy(io.Discard, response.Body)
	_ = response.Body.Close()
	if got := response.Header.Get("Anthropic-Ratelimit-Unified-Status"); got != "" {
		t.Fatalf("unified status = %q, want stripped", got)
	}
	if server.SchedulerRef.Get().Exhausted(accounts.ProviderClaude, "cooked@example.com") {
		t.Fatal("passive overage handling should not exhaust opted-in account")
	}
}

func TestClaudeOverageCostLogWithoutUsageStillAppends(t *testing.T) {
	path := filepath.Join(t.TempDir(), "claude-overage.jsonl")
	server := Server{ClaudeOverageCostLogPath: path}
	server.appendClaudeOverageCostRecord("acct@example.com", "claude-opus", http.StatusOK, claudeOverageUsage{}, false)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"account":"acct@example.com"`) || strings.Contains(string(data), "input_tokens") {
		t.Fatalf("unexpected cost log line: %s", data)
	}
}

func TestScoreAccountsAppliesClaudeOverageFloorForOptInFreshUsage(t *testing.T) {
	account := accounts.Account{ID: "optin@example.com", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "tok"}
	ref := &AccountRef{
		accounts: []accounts.Account{account},
		client:   &http.Client{Transport: &countingTransport{responses: claudeUsageOK}},
		usageWindows: map[string]usageWindowsEntry{
			account.ID + "\x00" + string(account.Provider): {
				windows: []accounts.UsageWindow{
					{Name: "5h", UsedPercent: 100, LimitWindowSeconds: 5 * 60 * 60},
					{Name: "7d", UsedPercent: 100, LimitWindowSeconds: 7 * 24 * 60 * 60},
					{Name: "extra", UsedPercent: 1},
				},
				at: time.Now(),
			},
		},
	}
	server := Server{
		AccountRef:            ref,
		SchedulerRef:          selectacct.NewSchedulerRef(selectacct.NewScheduler(nil)),
		ClaudeOverageAccounts: NormalizeClaudeOverageAccounts("optin@example.com"),
		RefreshAccountFn: func(ctx context.Context, account accounts.Account) (accounts.Account, error) {
			return account, nil
		},
	}
	scores, scored := server.scoreAccounts(context.Background(), []accounts.Account{account})
	if scored != 1 || len(scores) != 1 {
		t.Fatalf("scored=%d scores=%d", scored, len(scores))
	}
	if selectacct.NewScheduler(scores).Exhausted(accounts.ProviderClaude, "optin@example.com") {
		t.Fatalf("opt-in score should not be exhausted: %+v", scores[0])
	}
}
