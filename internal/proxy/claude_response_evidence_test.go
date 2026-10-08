package proxy

import (
	"context"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	agentclaude "github.com/manaflow-ai/subrouter/internal/agents/claude"
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

func findFeatureWindow(windows []accounts.UsageWindow, feature string) (accounts.UsageWindow, bool) {
	for _, window := range windows {
		if window.Feature == feature {
			return window, true
		}
	}
	return accounts.UsageWindow{}, false
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
	earlier := time.Now().Add(-2*time.Minute)
	later := earlier.Add(time.Minute)
	ref.observeClaudeQuotaHeaders(account, passiveClaudeHeader("7d_opus", "0.6"), earlier)
	ref.observeClaudeQuotaHeaders(account, passiveClaudeHeader("7d_oi", "0.4"), later)
	windows, observedAt, ok := ref.cachedClaudeSupplementalWithObservation(account, time.Now())
	if !ok || len(windows) != 2 {
		t.Fatalf("partial headers were not merged: %+v observed=%s ok=%t", windows, observedAt, ok)
	}
	if observedAt.After(earlier) {
		t.Fatalf("partial header refreshed older Opus evidence without a new observation: %s > %s", observedAt, earlier)
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
