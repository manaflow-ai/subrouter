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
	usageCalls           int
	probeCalls           int
	usageThrottle        bool
	includeSupplemental  bool
	primaryUsed          float64
	primaryIncludesFable bool
	primaryFableUsed     float64
}

func (c *supplementalProbeCounter) RoundTrip(req *http.Request) (*http.Response, error) {
	switch {
	case strings.HasSuffix(req.URL.Path, "/api/oauth/usage"):
		c.usageCalls++
		if c.usageThrottle {
			return &http.Response{
				StatusCode: http.StatusTooManyRequests,
				Status:     "429 Too Many Requests",
				Header:     http.Header{"Retry-After": []string{"1800"}},
				Body:       io.NopCloser(strings.NewReader("{}")),
			}, nil
		}
		extra := ""
		if c.primaryIncludesFable {
			extra = fmt.Sprintf(`,"seven_day_oauth_apps":{"utilization":%.1f,"resets_at":"2030-01-03T00:00:00+00:00"}`, c.primaryFableUsed)
		}
		body := fmt.Sprintf(`{"five_hour":{"utilization":%.1f,"resets_at":"2030-01-01T00:00:00+00:00"},"seven_day":{"utilization":12.0,"resets_at":"2030-01-02T00:00:00+00:00"}%s}`, c.primaryUsed, extra)
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

func TestClaudeUsageThrottleDoesNotLaunchSupplementalProbe(t *testing.T) {
	transport := &supplementalProbeCounter{usageThrottle: true}
	ref := &AccountRef{}
	client := &http.Client{Transport: transport}
	account := probeAccount()

	_, _, err := ref.FetchUsageWindowsCached(context.Background(), client, account)
	if err == nil {
		t.Fatal("expected usage throttle error")
	}
	if transport.usageCalls != 1 || transport.probeCalls != 0 {
		t.Fatalf("throttle launched an unnecessary probe: usage=%d probe=%d", transport.usageCalls, transport.probeCalls)
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

func TestClaudeExhaustedSupplementalWithoutResetMustReprobe(t *testing.T) {
	ref := &AccountRef{}
	account := probeAccount()
	// A response carrying "rejected" but no reset timestamp can become
	// obsolete at any point. Do not present its 100% usage as fresh for 10m.
	unknownReset := []accounts.UsageWindow{{
		Name:        agentclaude.FableWindowName,
		Feature:     agentclaude.FableFeature,
		UsedPercent: 100,
	}}
	ref.rememberClaudeSupplemental(account, unknownReset)
	if _, ok := ref.cachedClaudeSupplemental(account, time.Now()); ok {
		t.Fatal("100%-used model bucket without a reset must be refreshed")
	}
	knownReset := []accounts.UsageWindow{{
		Name:        agentclaude.FableWindowName,
		Feature:     agentclaude.FableFeature,
		UsedPercent: 100,
		ResetAt:     time.Now().Add(time.Hour),
	}}
	ref.rememberClaudeSupplemental(account, knownReset)
	if _, ok := ref.cachedClaudeSupplemental(account, time.Now()); !ok {
		t.Fatal("an explicit future reset allows reuse until the reset")
	}
}

func TestClaudePrimaryModelWindowSupersedesOlderProbe(t *testing.T) {
	transport := &supplementalProbeCounter{includeSupplemental: true, primaryUsed: 30}
	ref := &AccountRef{}
	client := &http.Client{Transport: transport}
	account := probeAccount()
	if _, _, err := ref.FetchUsageWindowsCached(context.Background(), client, account); err != nil {
		t.Fatal(err)
	}
	if transport.probeCalls != 1 {
		t.Fatalf("expected initial supplementary probe; calls=%d", transport.probeCalls)
	}
	// The ordinary usage endpoint starts reporting a newer Fable utilization.
	expirePrimaryClaudeUsageForTest(t, ref, account)
	transport.primaryIncludesFable = true
	transport.primaryFableUsed = 85
	windows, _, err := ref.FetchUsageWindowsCached(context.Background(), client, account)
	if err != nil {
		t.Fatal(err)
	}
	observed, ok := fableUsageWindow(windows)
	if !ok || observed.UsedPercent != 85 {
		t.Fatalf("fresh primary Fable data was ignored: %+v", windows)
	}
	// The next ordinary response omits the optional model window. It should
	// reuse the latest 85%, not resurrect the older probe's 30%.
	expirePrimaryClaudeUsageForTest(t, ref, account)
	transport.primaryIncludesFable = false
	windows, _, err = ref.FetchUsageWindowsCached(context.Background(), client, account)
	if err != nil {
		t.Fatal(err)
	}
	observed, ok = fableUsageWindow(windows)
	if !ok || observed.UsedPercent != 85 {
		t.Fatalf("old Fable evidence resurrected after new primary status: %+v", windows)
	}
	if transport.probeCalls != 1 {
		t.Fatalf("latest quota evidence triggered unnecessary model probe: %d", transport.probeCalls)
	}
}

func TestClaudeSupplementalCacheExcludesGlobalAndExtraWindows(t *testing.T) {
	items := []accounts.UsageWindow{
		{Name: "5h", UsedPercent: 45},
		{Name: "7d", UsedPercent: 30},
		{Name: "extra", UsedPercent: 75},
		{Name: agentclaude.FableWindowName, Feature: agentclaude.FableFeature, UsedPercent: 20},
	}
	got := supplementalClaudeWindows(items)
	if len(got) != 1 || got[0].Name != agentclaude.FableWindowName {
		t.Fatalf("supplemental cache captured account-wide or extra-spend status: %+v", got)
	}
}

func TestClaudeExhaustedSupplementalSkipsProbesUntilKnownReset(t *testing.T) {
	transport := &supplementalProbeCounter{includeSupplemental: true, primaryUsed: 30}
	ref := &AccountRef{}
	client := &http.Client{Transport: transport}
	account := probeAccount()
	if _, _, err := ref.FetchUsageWindowsCached(context.Background(), client, account); err != nil {
		t.Fatal(err)
	}
	if transport.probeCalls != 1 {
		t.Fatalf("initial synthetic probe count = %d, want 1", transport.probeCalls)
	}
	key := claudeSupplementalCacheKey(account)
	ref.usageWindowsMu.Lock()
	cached := ref.claudeSupplemental[key]
	if len(cached.windows) != 1 {
		ref.usageWindowsMu.Unlock()
		t.Fatalf("expected one supplementary window, got %+v", cached.windows)
	}
	cached.windows[0].UsedPercent = 100
	cached.windows[0].ResetAt = time.Now().Add(2 * time.Hour)
	cached.at = time.Now().Add(-claudeSupplementalProbeTTL - time.Minute)
	ref.claudeSupplemental[key] = cached
	ref.usageWindowsMu.Unlock()

	// Simulate repeated ordinary status refreshes beyond the original 10m TTL.
	// None should issue another synthetic Messages request while the model
	// window is exhausted and its authoritative reset is in the future.
	for i := 0; i < 4; i++ {
		expirePrimaryClaudeUsageForTest(t, ref, account)
		windows, _, err := ref.FetchUsageWindowsCached(context.Background(), client, account)
		if err != nil {
			t.Fatal(err)
		}
		w, ok := fableUsageWindow(windows)
		if !ok || w.UsedPercent != 100 {
			t.Fatalf("refresh %d: expected retained exhaustion until reset; got %+v", i, windows)
		}
	}
	if transport.usageCalls != 5 || transport.probeCalls != 1 {
		t.Fatalf("usage=%d probes=%d; want fresh primary on each pass and only one supplemental probe",
			transport.usageCalls, transport.probeCalls)
	}

	ref.usageWindowsMu.Lock()
	cached = ref.claudeSupplemental[key]
	cached.windows[0].ResetAt = time.Now().Add(-time.Second)
	ref.claudeSupplemental[key] = cached
	ref.usageWindowsMu.Unlock()
	expirePrimaryClaudeUsageForTest(t, ref, account)
	if _, _, err := ref.FetchUsageWindowsCached(context.Background(), client, account); err != nil {
		t.Fatal(err)
	}
	if transport.probeCalls != 2 {
		t.Fatalf("model bucket reset did not trigger one new verification: probes=%d", transport.probeCalls)
	}
}

func TestClaudeHealthySupplementalStillExpiresAfterTTL(t *testing.T) {
	ref := &AccountRef{}
	account := probeAccount()
	ref.rememberClaudeSupplemental(account, []accounts.UsageWindow{{
		Name: agentclaude.FableWindowName, Feature: agentclaude.FableFeature,
		UsedPercent: 40, ResetAt: time.Now().Add(time.Hour),
	}})
	key := claudeSupplementalCacheKey(account)
	ref.usageWindowsMu.Lock()
	value := ref.claudeSupplemental[key]
	value.at = time.Now().Add(-claudeSupplementalProbeTTL - time.Second)
	ref.claudeSupplemental[key] = value
	ref.usageWindowsMu.Unlock()
	if _, ok := ref.cachedClaudeSupplemental(account, time.Now()); ok {
		t.Fatal("healthy model evidence older than TTL must not be treated as fresh")
	}
}

func TestClaudeSupplementalOlderThanPrimaryCadenceIsStale(t *testing.T) {
	transport := &supplementalProbeCounter{includeSupplemental: true, primaryUsed: 30}
	ref := &AccountRef{}
	client := &http.Client{Transport: transport}
	account := probeAccount()
	if _, _, err := ref.FetchUsageWindowsCached(context.Background(), client, account); err != nil {
		t.Fatal(err)
	}
	if transport.probeCalls != 1 {
		t.Fatalf("initial synthetic probe count = %d, want 1", transport.probeCalls)
	}
	key := claudeSupplementalCacheKey(account)
	ref.usageWindowsMu.Lock()
	cached := ref.claudeSupplemental[key]
	cached.at = time.Now().Add(-usageWindowsTTL - time.Second)
	ref.claudeSupplemental[key] = cached
	ref.usageWindowsMu.Unlock()

	// The primary endpoint is refreshed, but the reusable model bucket is older
	// than the normal primary cadence. Keep its values for display while
	// withholding a fresh scheduler score until a new model probe observes it.
	expirePrimaryClaudeUsageForTest(t, ref, account)
	_, fresh, err := ref.FetchUsageWindowsCached(context.Background(), client, account)
	if err != nil {
		t.Fatal(err)
	}
	if fresh {
		t.Fatal("stale supplemental model evidence was reported as a fresh usage score")
	}
	if transport.probeCalls != 1 {
		t.Fatalf("stale supplemental evidence triggered an immediate probe: %d", transport.probeCalls)
	}
}
