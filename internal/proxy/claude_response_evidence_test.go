package proxy

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	agentclaude "github.com/manaflow-ai/subrouter/internal/agents/claude"
	"github.com/manaflow-ai/subrouter/selectacct"
	"github.com/manaflow-ai/subrouter/session"
)

func passiveClaudeAccount(token, version string) accounts.Account {
	return accounts.Account{
		ID:                "claude-test",
		Provider:          accounts.ProviderClaude,
		AuthMode:          accounts.AuthModeOAuth,
		Token:             token,
		CredentialVersion: version,
	}
}

func passiveClaudeHeader(prefix, utilization string) http.Header {
	h := make(http.Header)
	h.Set("anthropic-ratelimit-unified-"+prefix+"-utilization", utilization)
	h.Set("anthropic-ratelimit-unified-"+prefix+"-status", "allowed")
	h.Set("anthropic-ratelimit-unified-"+prefix+"-reset", strconv.FormatInt(time.Now().Add(72*time.Hour).Unix(), 10))
	return h
}

// passiveClaudeAccountQuotaHeaders builds a response carrying both account
// quota windows and the Fable model bucket.
func passiveClaudeAccountQuotaHeaders() http.Header {
	h := passiveClaudeHeader("7d_oi", "0.4")
	h.Set("Anthropic-Ratelimit-Unified-5h-Utilization", "1")
	h.Set("Anthropic-Ratelimit-Unified-5h-Status", "allowed")
	h.Set("Anthropic-Ratelimit-Unified-5h-Reset", strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10))
	h.Set("Anthropic-Ratelimit-Unified-7d-Utilization", "0.2")
	h.Set("Anthropic-Ratelimit-Unified-7d-Status", "allowed")
	h.Set("Anthropic-Ratelimit-Unified-7d-Reset", strconv.FormatInt(time.Now().Add(72*time.Hour).Unix(), 10))
	return h
}

func findFeatureWindow(windows []accounts.UsageWindow, feature string) (accounts.UsageWindow, bool) {
	for _, window := range windows {
		if window.Feature == feature {
			return window, true
		}
	}
	return accounts.UsageWindow{}, false
}

type partialSupplementalTransport struct {
	usageCalls int
	fableCalls int
}

func (t *partialSupplementalTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	switch req.URL.Path {
	case "/api/oauth/usage":
		t.usageCalls++
		return claudeUsageOK(), nil
	case "/v1/messages":
		t.fableCalls++
		header := passiveClaudeHeader("7d_oi", "0.4")
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     header,
			Body:       io.NopCloser(strings.NewReader(`{"type":"message"}`)),
		}, nil
	default:
		return nil, fmt.Errorf("unexpected Claude quota path %s", req.URL.Path)
	}
}

func TestPassiveClaudeReplyEliminatesSupplementaryQuotaProbe(t *testing.T) {
	account := passiveClaudeAccount("access-token", "grant-v1")
	ref := &AccountRef{accounts: []accounts.Account{account}}
	ref.observeClaudeQuotaHeaders(account, passiveClaudeHeader("7d_oi", "0.4"), time.Now())
	transport := &countingTransport{responses: claudeUsageOK}
	client := &http.Client{Transport: transport}
	windows, fresh, err := ref.FetchUsageWindowsCached(context.Background(), client, account)
	if err != nil || !fresh {
		t.Fatalf("quota fetch failed: fresh=%t err=%v", fresh, err)
	}
	fable, ok := findFeatureWindow(windows, agentclaude.FableFeature)
	if !ok || fable.UsedPercent != 40 {
		t.Fatalf("cached quota header missing or incorrect: %+v", windows)
	}
	if transport.calls != 1 {
		t.Fatalf("expected only the normal usage fetch, got %d upstream calls", transport.calls)
	}
}

// TestObservedClaudeResponseQuotaIncludesAccountAndModelWindows verifies that
// response parsing preserves account-wide and model-specific quota windows.
func TestObservedClaudeResponseQuotaIncludesAccountAndModelWindows(t *testing.T) {
	windows := agentclaude.ObservedUsageWindows(passiveClaudeAccountQuotaHeaders(), time.Now())
	var accountWindows, modelWindows int
	for _, window := range windows {
		if window.Feature == "" {
			accountWindows++
		} else {
			modelWindows++
		}
	}
	if accountWindows != 2 || modelWindows != 1 {
		t.Fatalf("observed windows = %+v, want two account-wide and one model window", windows)
	}
}

func TestPassiveOpusEvidenceStillProbesMissingFableQuota(t *testing.T) {
	account := passiveClaudeAccount("access-token", "grant-v1")
	ref := &AccountRef{accounts: []accounts.Account{account}}
	ref.observeClaudeQuotaHeaders(account, passiveClaudeHeader("7d_opus", "0.6"), time.Now())
	transport := &partialSupplementalTransport{}
	windows, fresh, err := ref.FetchUsageWindowsCached(context.Background(), &http.Client{Transport: transport}, account)
	if err != nil || !fresh {
		t.Fatalf("quota fetch failed: fresh=%t err=%v", fresh, err)
	}
	if transport.usageCalls != 1 || transport.fableCalls != 1 {
		t.Fatalf("quota calls = usage %d fable %d, want one of each", transport.usageCalls, transport.fableCalls)
	}
	opus, ok := findFeatureWindow(windows, agentclaude.OpusFeature)
	if !ok || opus.UsedPercent != 60 {
		t.Fatalf("passive Opus evidence was lost: %+v", windows)
	}
	fable, ok := findFeatureWindow(windows, agentclaude.FableFeature)
	if !ok || fable.UsedPercent != 40 {
		t.Fatalf("Fable probe evidence missing: %+v", windows)
	}
}

func TestPassiveClaudeReplyCannotPoisonReauthenticatedProfile(t *testing.T) {
	current := passiveClaudeAccount("new-token", "grant-v2")
	old := passiveClaudeAccount("old-token", "grant-v1")
	ref := &AccountRef{accounts: []accounts.Account{current}}
	ref.observeClaudeQuotaHeaders(old, passiveClaudeHeader("7d_oi", "0.9"), time.Now())
	if _, ok := ref.cachedClaudeSupplemental(current, time.Now()); ok {
		t.Fatal("stale credential's quota was adopted after login replacement")
	}
	ref.observeClaudeQuotaHeaders(current, passiveClaudeHeader("7d_oi", "0.4"), time.Now())
	if windows, ok := ref.cachedClaudeSupplemental(current, time.Now()); !ok {
		t.Fatal("new credential's quota observation missing")
	} else if found, ok := findFeatureWindow(windows, agentclaude.FableFeature); !ok || found.UsedPercent != 40 {
		t.Fatalf("new credential quota: %+v", windows)
	}
}

func TestPassiveClaudeOlderReplyDoesNotOverwriteNewerQuota(t *testing.T) {
	account := passiveClaudeAccount("token", "grant-v1")
	ref := &AccountRef{accounts: []accounts.Account{account}}
	later := time.Now().Add(-time.Second)
	earlier := later.Add(-time.Second)
	ref.observeClaudeQuotaHeaders(account, passiveClaudeHeader("7d_oi", "0.8"), later)
	ref.observeClaudeQuotaHeaders(account, passiveClaudeHeader("7d_oi", "0.1"), earlier)
	windows, ok := ref.cachedClaudeSupplemental(account, time.Now())
	if !ok {
		t.Fatal("latest observed quota missing")
	}
	fable, ok := findFeatureWindow(windows, agentclaude.FableFeature)
	if !ok || fable.UsedPercent != 80 {
		t.Fatalf("out-of-order request overwrote newer quota: %+v", windows)
	}
}

func TestPassiveClaudePartialFeatureHeadersKeepOlderBucketsAged(t *testing.T) {
	account := passiveClaudeAccount("token", "grant-v1")
	ref := &AccountRef{accounts: []accounts.Account{account}}
	earlier := time.Now().Add(-2 * time.Minute)
	later := earlier.Add(time.Minute)
	ref.observeClaudeQuotaHeaders(account, passiveClaudeHeader("7d_opus", "0.6"), earlier)
	_, firstObservedAt, ok := ref.cachedClaudeSupplementalWithObservation(account, time.Now())
	if !ok {
		t.Fatal("first model window was not observed")
	}
	ref.observeClaudeQuotaHeaders(account, passiveClaudeHeader("7d_oi", "0.4"), later)
	windows, observedAt, ok := ref.cachedClaudeSupplementalWithObservation(account, time.Now())
	if !ok || len(windows) != 2 {
		t.Fatalf("partial headers were not merged: %+v observed=%s ok=%t", windows, observedAt, ok)
	}
	if observedAt.After(firstObservedAt) {
		t.Fatalf("partial header refreshed older Opus evidence without a new observation: %s > %s", observedAt, firstObservedAt)
	}
	opus, okOpus := findFeatureWindow(windows, agentclaude.OpusFeature)
	fable, okFable := findFeatureWindow(windows, agentclaude.FableFeature)
	if !okOpus || !okFable || opus.UsedPercent != 60 || fable.UsedPercent != 40 {
		t.Fatalf("unexpected merged model windows: %+v", windows)
	}
}

func TestPassiveClaudeUncertainExhaustionNotCached(t *testing.T) {
	account := passiveClaudeAccount("token", "grant-v1")
	ref := &AccountRef{accounts: []accounts.Account{account}}
	ref.observeClaudeQuotaHeaders(account, passiveClaudeHeader("7d_oi", "0.4"), time.Now().Add(-time.Second))
	h := make(http.Header)
	h.Set("anthropic-ratelimit-unified-7d_oi-status", "rejected")
	ref.observeClaudeQuotaHeaders(account, h, time.Now())
	if windows, ok := ref.cachedClaudeSupplemental(account, time.Now()); ok {
		t.Fatalf("exhaustion without reset was incorrectly reused: %+v", windows)
	}
}

func TestPassiveClaudeLongRequestStillPublishesFreshArrivingHeaders(t *testing.T) {
	account := passiveClaudeAccount("token", "grant-v1")
	ref := &AccountRef{accounts: []accounts.Account{account}}
	requestStarted := time.Now().Add(-20 * time.Minute)
	ref.observeClaudeQuotaHeaders(account, passiveClaudeHeader("7d_oi", "0.4"), requestStarted)
	_, observedAt, ok := ref.cachedClaudeSupplementalWithObservation(account, time.Now())
	if !ok || time.Since(observedAt) > time.Second {
		t.Fatalf("long model request produced incorrectly stale quota observation: ok=%t at=%s", ok, observedAt)
	}
}

// TestRealClaudeProxyResponseSeedsLaterModelQuotaCache verifies that a real
// response updates reset scheduling and suppresses redundant quota polling.
func TestRealClaudeProxyResponseSeedsLaterModelQuotaCache(t *testing.T) {
	account := passiveClaudeAccount("real-response-token", "grant-v1")
	ref := &AccountRef{accounts: []accounts.Account{account}, store: accounts.CodexStore{Dir: t.TempDir()}}
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		_, _ = io.Copy(io.Discard, req.Body)
		for key, values := range passiveClaudeAccountQuotaHeaders() {
			for _, value := range values {
				w.Header().Add(key, value)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_real","type":"message"}`)
	}))
	defer provider.Close()
	upstream, err := url.Parse(provider.URL)
	if err != nil {
		t.Fatal(err)
	}
	sessionStore, err := session.NewStore(t.TempDir() + "/sessions.json")
	if err != nil {
		t.Fatal(err)
	}
	s := Server{
		AccountRef:     ref,
		Sessions:       sessionStore,
		ClaudeUpstream: upstream,
		SchedulerRef:   selectacct.NewSchedulerRef(selectacct.NewScheduler(nil)),
		UsageScoreTTL:  0,
		MaxBodyBytes:   1 << 20,
		RefreshAccountFn: func(_ context.Context, candidate accounts.Account) (accounts.Account, error) {
			return candidate, nil
		},
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"claude-opus-4-8","messages":[]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Subrouter-Agent", "claude")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("routed Claude reply status=%d body=%s", rec.Code, rec.Body.String())
	}
	if observed, ok := ref.cachedClaudeSupplemental(account, time.Now()); !ok {
		t.Fatal("real upstream response did not seed supplementary quota")
	} else if fable, ok := findFeatureWindow(observed, agentclaude.FableFeature); !ok || fable.UsedPercent != 40 {
		t.Fatalf("incorrect passive quota evidence: %+v", observed)
	}
	score := s.SchedulerRef.RefreshSeed().ScoreFor(accounts.ProviderClaude, account.ID)
	if score.Headroom != 0 || score.ExhaustedResetAt.Before(time.Now().Add(59*time.Minute)) {
		t.Fatalf("response quota did not publish the one-hour account reset: %+v", score)
	}
	transport := &countingTransport{responses: claudeUsageOK}
	windows, _, err := ref.FetchUsageWindowsCached(context.Background(), &http.Client{Transport: transport}, account)
	if err != nil || len(windows) == 0 || transport.calls != 0 {
		t.Fatalf("real response did not prevent quota polling: windows=%+v calls=%d err=%v", windows, transport.calls, err)
	}
	if fable, ok := findFeatureWindow(windows, agentclaude.FableFeature); !ok || fable.UsedPercent != 40 {
		t.Fatalf("real response did not retain model quota alongside account windows: %+v", windows)
	}
	var account7d bool
	for _, window := range windows {
		if window.Feature == "" && window.LimitWindowSeconds == 7*24*60*60 && window.UsedPercent == 20 {
			account7d = true
			break
		}
	}
	if !account7d {
		t.Fatalf("real response did not retain the account-wide 7d quota window: %+v", windows)
	}
	for range 3 {
		if _, scored := s.scoreAccounts(context.Background(), []accounts.Account{account}); scored != 0 {
			t.Fatalf("future account reset triggered a quota poll on repeated score refresh: scored=%d", scored)
		}
	}
	if transport.calls != 0 {
		t.Fatalf("future reset triggered %d provider quota calls", transport.calls)
	}
}

func TestPassiveClaudeRepairedGrantWithSameAccessTokenGetsFreshEvidence(t *testing.T) {
	old := passiveClaudeAccount("shared-access", "grant-v1")
	ref := &AccountRef{accounts: []accounts.Account{old}}
	ref.observeClaudeQuotaHeaders(old, passiveClaudeHeader("7d_oi", "0.9"), time.Now().Add(-time.Second))
	repaired := passiveClaudeAccount("shared-access", "grant-v2")
	ref.replace(repaired)
	if _, ok := ref.cachedClaudeSupplemental(repaired, time.Now()); ok {
		t.Fatal("new refresh grant inherited old model quota because access token matched")
	}
	ref.observeClaudeQuotaHeaders(repaired, passiveClaudeHeader("7d_oi", "0.3"), time.Now())
	windows, ok := ref.cachedClaudeSupplemental(repaired, time.Now())
	if !ok {
		t.Fatal("repaired OAuth grant did not acquire fresh model quota")
	}
	observed, found := findFeatureWindow(windows, agentclaude.FableFeature)
	if !found || observed.UsedPercent != 30 {
		t.Fatalf("repaired grant quota=%+v; want 30%% Fable utilization", windows)
	}
}
