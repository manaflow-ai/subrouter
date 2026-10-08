package proxy

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
)

func TestFetchUsageWindowsSharedRechecksActiveThrottleBeforeFlight(t *testing.T) {
	transport := &countingTransport{responses: claudeUsageOK}
	ref := &AccountRef{}
	client := &http.Client{Transport: transport}
	account := accounts.Account{ID: "claude-a", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "token-a"}
	cacheKey := account.ID + "\x00" + string(account.Provider)
	flightKey := usageWindowsFailureKey(cacheKey, account.Token)
	throttleErr := errors.New("usage fetch failed: 429 Too Many Requests")
	ref.usageWindowsMu.Lock()
	ref.usageWindowsFailures = map[string]usageWindowsFailure{
		flightKey: {err: throttleErr, retryAt: time.Now().Add(time.Hour)},
	}
	ref.usageWindowsMu.Unlock()

	windows, _, _, err := ref.fetchUsageWindowsShared(context.Background(), client, account, cacheKey)
	if !errors.Is(err, throttleErr) || windows != nil {
		t.Fatalf("active throttle was not reused: windows=%v err=%v", windows, err)
	}
	if transport.calls != 0 {
		t.Fatalf("active throttle started an upstream request: calls=%d", transport.calls)
	}
}

func TestUsageThrottleAvoidsRepeatedUpstreamRequests(t *testing.T) {
	transport := &countingTransport{responses: claudeUsage429}
	ref := &AccountRef{}
	client := &http.Client{Transport: transport}
	account := accounts.Account{ID: "claude-a", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "token-a"}

	// The first miss may make the ordinary usage request and a missing-window
	// probe. Further status/score readers should make zero upstream calls.
	_, _, err := ref.FetchUsageWindowsCached(context.Background(), client, account)
	if err == nil {
		t.Fatal("expected throttle error on first live usage fetch")
	}
	initial := transport.calls
	if initial != 1 {
		t.Fatalf("usage throttle triggered %d requests, want 1 usage call and no synthetic Messages probe", initial)
	}
	for i := 0; i < 5; i++ {
		_, _, gotErr := ref.FetchUsageWindowsCached(context.Background(), client, account)
		if gotErr == nil {
			t.Fatalf("attempt %d silently discarded first error", i)
		}
	}
	if transport.calls != initial {
		t.Fatalf("repeated throttled usage polling: calls=%d, want %d", transport.calls, initial)
	}
	// A manual status refresh must not bypass a provider's throttle.
	ref.InvalidateUsageWindowsCache()
	_, _, _ = ref.FetchUsageWindowsCached(context.Background(), client, account)
	if transport.calls != initial {
		t.Fatal("cache invalidation bypassed the upstream throttle deadline")
	}
}

func TestUsageThrottleKeepsLastGoodStaleWithoutExtraRequests(t *testing.T) {
	transport := &countingTransport{responses: claudeUsageOK}
	ref := &AccountRef{}
	client := &http.Client{Transport: transport}
	account := accounts.Account{ID: "claude-a", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "token-a"}
	windows, _, err := ref.FetchUsageWindowsCached(context.Background(), client, account)
	if err != nil || len(windows) == 0 {
		t.Fatalf("prime cache: windows=%v err=%v", windows, err)
	}
	key := account.ID + "\x00" + string(account.Provider)
	ref.usageWindowsMu.Lock()
	entry := ref.usageWindows[key]
	entry.at = time.Now().Add(-usageWindowsTTL - time.Second)
	ref.usageWindows[key] = entry
	ref.usageWindowsMu.Unlock()

	transport.responses = claudeUsage429
	windows, fresh, err := ref.FetchUsageWindowsCached(context.Background(), client, account)
	if err != nil || fresh || len(windows) == 0 {
		t.Fatalf("stale fallback: windows=%v fresh=%t err=%v", windows, fresh, err)
	}
	calls := transport.calls
	for i := 0; i < 3; i++ {
		windows, fresh, err = ref.FetchUsageWindowsCached(context.Background(), client, account)
		if err != nil || fresh || len(windows) == 0 {
			t.Fatalf("cached stale fallback %d: windows=%v fresh=%t err=%v", i, windows, fresh, err)
		}
	}
	if transport.calls != calls {
		t.Fatalf("extra upstream probes while throttled: %d after %d", transport.calls, calls)
	}
}

func TestUsageThrottleKeepsProviderResetWindowsAcrossFailureCache(t *testing.T) {
	reset := time.Now().Add(2 * time.Hour).Unix()
	transport := &countingTransport{responses: func() *http.Response {
		return &http.Response{
			StatusCode: http.StatusTooManyRequests,
			Status:     "429 Too Many Requests",
			Header: http.Header{
				"Retry-After":                                []string{"1800"},
				"Anthropic-Ratelimit-Unified-5h-Status":      []string{"allowed"},
				"Anthropic-Ratelimit-Unified-5h-Utilization": []string{"0.42"},
				"Anthropic-Ratelimit-Unified-5h-Reset":       []string{strconv.FormatInt(reset, 10)},
			},
			Body: io.NopCloser(strings.NewReader("{}")),
		}
	}}
	ref := &AccountRef{}
	client := &http.Client{Transport: transport}
	account := accounts.Account{ID: "claude-a", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "token-a"}

	windows, fresh, observedAt, throttled, err := ref.FetchUsageWindowsCachedWithObservation(context.Background(), client, account)
	if err != nil || fresh || !throttled || observedAt.IsZero() || len(windows) != 1 {
		t.Fatalf("first throttled read: windows=%v fresh=%t observed=%v throttled=%t err=%v", windows, fresh, observedAt, throttled, err)
	}
	windows, fresh, observedAt, throttled, err = ref.FetchUsageWindowsCachedWithObservation(context.Background(), client, account)
	if err != nil || fresh || !throttled || observedAt.IsZero() || len(windows) != 1 {
		t.Fatalf("cached throttled read: windows=%v fresh=%t observed=%v throttled=%t err=%v", windows, fresh, observedAt, throttled, err)
	}
	if transport.calls != 1 {
		t.Fatalf("cached throttled read made another upstream call: %d", transport.calls)
	}
}

func TestUsageThrottleCacheIsCredentialScopedAndExpires(t *testing.T) {
	transport := &countingTransport{responses: claudeUsage429}
	ref := &AccountRef{}
	client := &http.Client{Transport: transport}
	account := accounts.Account{ID: "claude-a", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "old-token"}
	_, _, _ = ref.FetchUsageWindowsCached(context.Background(), client, account)
	first := transport.calls
	if first == 0 {
		t.Fatal("expected initial fetch")
	}

	repaired := account
	repaired.Token = "new-token"
	_, _, _ = ref.FetchUsageWindowsCached(context.Background(), client, repaired)
	if transport.calls <= first {
		t.Fatal("repaired credential must bypass old throttle cache")
	}
	second := transport.calls

	// Simulate expiry without sleeping: the next read can retry the upstream.
	key := account.ID + "\x00" + string(account.Provider)
	cacheKey := usageWindowsFailureKey(key, repaired.Token)
	ref.usageWindowsMu.Lock()
	failure := ref.usageWindowsFailures[cacheKey]
	failure.at = time.Now().Add(-usageWindowsThrottleTTL - time.Second)
	failure.retryAt = time.Now().Add(-time.Second)
	ref.usageWindowsFailures[cacheKey] = failure
	ref.usageWindowsMu.Unlock()
	_, _, _ = ref.FetchUsageWindowsCached(context.Background(), client, repaired)
	if transport.calls <= second {
		t.Fatalf("expired failure cache must allow a new fetch: %d after %d", transport.calls, second)
	}
}

func TestUsageThrottleDoesNotCacheAuthFailure(t *testing.T) {
	transport := &countingTransport{responses: func() *http.Response {
		return &http.Response{
			StatusCode: http.StatusUnauthorized, Status: "401 Unauthorized",
			Header: http.Header{}, Body: io.NopCloser(strings.NewReader("{}")),
		}
	}}
	ref := &AccountRef{}
	client := &http.Client{Transport: transport}
	account := accounts.Account{ID: "claude-a", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "token-a"}
	_, _, err := ref.FetchUsageWindowsCached(context.Background(), client, account)
	if err == nil {
		t.Fatal("expected auth error")
	}
	first := transport.calls
	_, _, err = ref.FetchUsageWindowsCached(context.Background(), client, account)
	if err == nil || transport.calls <= first {
		t.Fatalf("auth failure should propagate outside the transient throttle cache: calls=%d, first=%d, err=%v", transport.calls, first, err)
	}
}

func TestUsageThrottleHonorsLongRetryAfterAndDoesNotProbe(t *testing.T) {
	transport := &countingTransport{responses: func() *http.Response {
		return &http.Response{
			StatusCode: http.StatusTooManyRequests,
			Status:     "429 Too Many Requests",
			Header:     http.Header{"Retry-After": []string{"1800"}},
			Body:       io.NopCloser(strings.NewReader("{}")),
		}
	}}
	ref := &AccountRef{}
	client := &http.Client{Transport: transport}
	account := accounts.Account{ID: "claude-a", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "token-a"}
	_, _, err := ref.FetchUsageWindowsCached(context.Background(), client, account)
	if err == nil {
		t.Fatal("want 429 from usage")
	}
	initial := transport.calls
	key := usageWindowsFailureKey(account.ID+"\x00"+string(account.Provider), account.Token)
	ref.usageWindowsMu.Lock()
	failure, ok := ref.usageWindowsFailures[key]
	ref.usageWindowsMu.Unlock()
	if !ok || failure.retryAt.Before(time.Now().Add(29*time.Minute)) {
		t.Fatalf("retryAt = %v, want provider's 30m deadline", failure.retryAt)
	}
	// Many status clients and an explicit cache refresh cannot shorten it.
	ref.InvalidateUsageWindowsCache()
	for i := 0; i < 20; i++ {
		_, _, _ = ref.FetchUsageWindowsCached(context.Background(), client, account)
	}
	if transport.calls != initial {
		t.Fatalf("provider throttle triggered repeated calls: got %d, want %d", transport.calls, initial)
	}
	ref.usageWindowsMu.Lock()
	failure = ref.usageWindowsFailures[key]
	failure.retryAt = time.Now().Add(-time.Second)
	ref.usageWindowsFailures[key] = failure
	ref.usageWindowsMu.Unlock()
	_, _, _ = ref.FetchUsageWindowsCached(context.Background(), client, account)
	if transport.calls <= initial {
		t.Fatal("no verification after the provider deadline passed")
	}
}
