package proxy

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	agentclaude "github.com/manaflow-ai/subrouter/internal/agents/claude"
)

type supplementalProbeCounter struct {
	usageCalls          int
	probeCalls          int
	includeSupplemental bool
	primaryUsed         float64
}

func (c *supplementalProbeCounter) RoundTrip(req *http.Request) (*http.Response, error) {
	switch {
	case strings.HasSuffix(req.URL.Path, "/api/oauth/usage"):
		c.usageCalls++
		body := fmt.Sprintf(`{"five_hour":{"utilization":%.1f,"resets_at":"2030-01-01T00:00:00+00:00"},"seven_day":{"utilization":12.0,"resets_at":"2030-01-02T00:00:00+00:00"}}`, c.primaryUsed)
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}, nil
	case strings.HasSuffix(req.URL.Path, "/v1/messages"):
		c.probeCalls++
		h := make(http.Header)
		if c.includeSupplemental {
			h.Set("anthropic-ratelimit-unified-7d_oi-status", "allowed")
			h.Set("anthropic-ratelimit-unified-7d_oi-utilization", "0.3")
			h.Set("anthropic-ratelimit-unified-7d_oi-reset", fmt.Sprint(time.Now().Add(72*time.Hour).Unix()))
		}
		return &http.Response{StatusCode: http.StatusOK, Header: h, Body: io.NopCloser(strings.NewReader("{}"))}, nil
	default:
		return nil, fmt.Errorf("unexpected upstream request %s", req.URL.Path)
	}
}

func expirePrimaryClaudeUsageForTest(t *testing.T, ref *AccountRef, account accounts.Account) {
	t.Helper()
	key := account.ID + "\x00" + string(account.Provider)
	ref.usageWindowsMu.Lock()
	defer ref.usageWindowsMu.Unlock()
	entry, ok := ref.usageWindows[key]
	if !ok {
		t.Fatal("primary usage cache was never filled")
	}
	entry.at = time.Now().Add(-usageWindowsTTL - time.Second)
	ref.usageWindows[key] = entry
}

func probeAccount() accounts.Account {
	return accounts.Account{ID: "claude-test", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "token-a"}
}

func fableUsageWindow(windows []accounts.UsageWindow) (accounts.UsageWindow, bool) {
	for _, w := range windows {
		if w.Name == agentclaude.FableWindowName {
			return w, true
		}
	}
	return accounts.UsageWindow{}, false
}

func TestClaudeSupplementalProbeReusedAcrossPrimaryUsageRefreshes(t *testing.T) {
	transport := &supplementalProbeCounter{includeSupplemental: true, primaryUsed: 30}
	ref := &AccountRef{}
	client := &http.Client{Transport: transport}
	account := probeAccount()

	windows, fresh, err := ref.FetchUsageWindowsCached(context.Background(), client, account)
	if err != nil || !fresh {
		t.Fatalf("first fetch fresh=%v err=%v", fresh, err)
	}
	if _, ok := fableUsageWindow(windows); !ok {
		t.Fatal("supplementary usage window missing from first fetch")
	}
	if transport.usageCalls != 1 || transport.probeCalls != 1 {
		t.Fatalf("first fetch usage=%d probe=%d; want 1+1", transport.usageCalls, transport.probeCalls)
	}

	// The primary usage cache expires normally, but only the primary
	// endpoint needs a new request: supplementary evidence is still valid.
	expirePrimaryClaudeUsageForTest(t, ref, account)
	transport.primaryUsed = 65
	windows, fresh, err = ref.FetchUsageWindowsCached(context.Background(), client, account)
	if err != nil || !fresh {
		t.Fatalf("second fetch fresh=%v err=%v", fresh, err)
	}
	if transport.usageCalls != 2 || transport.probeCalls != 1 {
		t.Fatalf("second fetch usage=%d probe=%d; want 2+1", transport.usageCalls, transport.probeCalls)
	}
	var primaryFresh bool
	for _, window := range windows {
		if window.Name == "5h" && window.UsedPercent == 65 {
			primaryFresh = true
		}
	}
	if !primaryFresh {
		t.Fatalf("older probe overwrote the new primary 5h usage: %+v", windows)
	}
	if _, ok := fableUsageWindow(windows); !ok {
		t.Fatal("supplementary window was lost on second fetch")
	}
}

func TestClaudeEmptySupplementalProbeIsNotRepeated(t *testing.T) {
	transport := &supplementalProbeCounter{primaryUsed: 30}
	ref := &AccountRef{}
	client := &http.Client{Transport: transport}
	account := probeAccount()
	if _, _, err := ref.FetchUsageWindowsCached(context.Background(), client, account); err != nil {
		t.Fatal(err)
	}
	expirePrimaryClaudeUsageForTest(t, ref, account)
	if _, _, err := ref.FetchUsageWindowsCached(context.Background(), client, account); err != nil {
		t.Fatal(err)
	}
	if transport.usageCalls != 2 || transport.probeCalls != 1 {
		t.Fatalf("empty supplementary evidence triggered a repeated probe: usage=%d probe=%d", transport.usageCalls, transport.probeCalls)
	}
}

func TestClaudeSupplementalReprobesForNewTokenAndAfterReset(t *testing.T) {
	transport := &supplementalProbeCounter{includeSupplemental: true, primaryUsed: 10}
	ref := &AccountRef{}
	client := &http.Client{Transport: transport}
	account := probeAccount()
	if _, _, err := ref.FetchUsageWindowsCached(context.Background(), client, account); err != nil {
		t.Fatal(err)
	}
	expirePrimaryClaudeUsageForTest(t, ref, account)
	account.Token = "repaired-token"
	if _, _, err := ref.FetchUsageWindowsCached(context.Background(), client, account); err != nil {
		t.Fatal(err)
	}
	if transport.probeCalls != 2 {
		t.Fatalf("new credential inherited stale supplementary usage: probes=%d", transport.probeCalls)
	}

	// A reset invalidates only that stale supplementary evidence; the next
	// ordinary primary refresh can request fresh supplementary data.
	cacheKey := claudeSupplementalCacheKey(account)
	ref.usageWindowsMu.Lock()
	entry := ref.claudeSupplemental[cacheKey]
	if len(entry.windows) == 0 {
		ref.usageWindowsMu.Unlock()
		t.Fatal("expected Fable supplementary window")
	}
	entry.windows[0].ResetAt = time.Now().Add(-time.Second)
	ref.claudeSupplemental[cacheKey] = entry
	ref.usageWindowsMu.Unlock()
	expirePrimaryClaudeUsageForTest(t, ref, account)
	if _, _, err := ref.FetchUsageWindowsCached(context.Background(), client, account); err != nil {
		t.Fatal(err)
	}
	if transport.probeCalls != 3 {
		t.Fatalf("expired supplementary window was reused: probes=%d", transport.probeCalls)
	}
	ref.InvalidateUsageWindowsCache()
	if _, _, err := ref.FetchUsageWindowsCached(context.Background(), client, account); err != nil {
		t.Fatal(err)
	}
	if transport.probeCalls != 4 {
		t.Fatalf("explicit cache invalidation ignored: probes=%d", transport.probeCalls)
	}
}
