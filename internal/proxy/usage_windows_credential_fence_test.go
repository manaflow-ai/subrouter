package proxy

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
)

func usageCredentialForTest(token, version string) accounts.Account {
	return accounts.Account{
		ID:                "same-profile",
		Provider:          accounts.ProviderCodex,
		AuthMode:          accounts.AuthModeOAuth,
		Token:             token,
		CredentialVersion: version,
	}
}

func TestUsageWindowCacheDoesNotReusePreviousCredential(t *testing.T) {
	transport := &countingTransport{}
	transport.responses = func() *http.Response {
		return codexUsageResponseForTest(float64(transport.calls * 10))
	}
	ref := &AccountRef{}
	client := &http.Client{Transport: transport}
	old := usageCredentialForTest("old-access", "old-grant")
	newer := usageCredentialForTest("new-access", "new-grant")

	first, _, err := ref.FetchUsageWindowsCached(context.Background(), client, old)
	if err != nil || len(first) != 1 || first[0].UsedPercent != 10 {
		t.Fatalf("initial quota = %+v err=%v", first, err)
	}
	second, _, err := ref.FetchUsageWindowsCached(context.Background(), client, newer)
	if err != nil || len(second) != 1 || second[0].UsedPercent != 20 {
		t.Fatalf("new login inherited old quota: %+v err=%v", second, err)
	}
	third, _, err := ref.FetchUsageWindowsCached(context.Background(), client, newer)
	if err != nil || len(third) != 1 || third[0].UsedPercent != 20 {
		t.Fatalf("new credential cache was not reused: %+v err=%v", third, err)
	}
	if transport.calls != 2 {
		t.Fatalf("upstream calls = %d, want one per credential", transport.calls)
	}
}

func TestUsageWindowCacheOldFlightCannotReplaceNewCredential(t *testing.T) {
	transport := &invalidationRaceTransport{
		firstStarted:  make(chan struct{}),
		releaseFirst:  make(chan struct{}),
		secondStarted: make(chan struct{}),
	}
	ref := &AccountRef{}
	client := &http.Client{Transport: transport}
	old := usageCredentialForTest("old-access", "old-grant")
	newer := usageCredentialForTest("new-access", "new-grant")
	type result struct {
		windows []accounts.UsageWindow
		err     error
	}
	oldResult := make(chan result, 1)
	go func() {
		w, _, err := ref.FetchUsageWindowsCached(context.Background(), client, old)
		oldResult <- result{windows: w, err: err}
	}()
	<-transport.firstStarted

	newResult := make(chan result, 1)
	go func() {
		w, _, err := ref.FetchUsageWindowsCached(context.Background(), client, newer)
		newResult <- result{windows: w, err: err}
	}()
	<-transport.secondStarted
	latest := <-newResult
	close(transport.releaseFirst)
	previous := <-oldResult
	if previous.err != nil || latest.err != nil {
		t.Fatalf("fetch errors: old=%v new=%v", previous.err, latest.err)
	}
	if len(previous.windows) != 1 || previous.windows[0].UsedPercent != 10 ||
		len(latest.windows) != 1 || latest.windows[0].UsedPercent != 20 {
		t.Fatalf("in-flight request results old=%+v new=%+v", previous.windows, latest.windows)
	}
	key := newer.ID + "\x00" + string(newer.Provider)
	ref.usageWindowsMu.Lock()
	entry := ref.usageWindows[key]
	ref.usageWindowsMu.Unlock()
	if len(entry.windows) != 1 || entry.windows[0].UsedPercent != 20 ||
		entry.credentialKey != usageWindowsCredentialKey(newer) {
		t.Fatalf("superseded credential replaced latest cache: %+v", entry)
	}
	// The successful newer fetch is reused; a late older completion cannot
	// force another upstream quota read.
	again, _, err := ref.FetchUsageWindowsCached(context.Background(), client, newer)
	if err != nil || len(again) != 1 || again[0].UsedPercent != 20 {
		t.Fatalf("newer credential not cached after race: %+v err=%v", again, err)
	}
}

func TestUsageWindowCacheDoesNotFallbackToPreviousCredentialOnThrottle(t *testing.T) {
	transport := &countingTransport{}
	transport.responses = func() *http.Response {
		if transport.calls == 1 {
			return codexUsageResponseForTest(10)
		}
		return &http.Response{
			StatusCode: http.StatusTooManyRequests,
			Status:     "429 Too Many Requests",
			Header:     http.Header{},
			Body:       io.NopCloser(strings.NewReader("{}")),
		}
	}
	ref := &AccountRef{}
	client := &http.Client{Transport: transport}
	old := usageCredentialForTest("old-access", "old-grant")
	newer := usageCredentialForTest("new-access", "new-grant")
	if _, _, err := ref.FetchUsageWindowsCached(context.Background(), client, old); err != nil {
		t.Fatal(err)
	}
	result, fresh, err := ref.FetchUsageWindowsCached(context.Background(), client, newer)
	if err == nil || fresh || len(result) != 0 {
		t.Fatalf("new credential inherited stale previous-account quota: %+v fresh=%t err=%v", result, fresh, err)
	}
	if transport.calls != 2 {
		t.Fatalf("upstream calls=%d, want one fresh check for new credential", transport.calls)
	}
}

func TestUsageWindowCacheSeparatesCredentialVersionWithSameAccessToken(t *testing.T) {
	transport := &countingTransport{}
	transport.responses = func() *http.Response {
		return codexUsageResponseForTest(float64(transport.calls * 10))
	}
	ref := &AccountRef{}
	client := &http.Client{Transport: transport}
	old := usageCredentialForTest("same-access", "old-refresh-grant")
	newer := usageCredentialForTest("same-access", "repaired-refresh-grant")
	if _, _, err := ref.FetchUsageWindowsCached(context.Background(), client, old); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ref.FetchUsageWindowsCached(context.Background(), client, newer); err != nil {
		t.Fatal(err)
	}
	if transport.calls != 2 {
		t.Fatalf("a repaired credential was treated as the prior refresh grant: %d calls", transport.calls)
	}
}

func TestSupersededUsage429CannotThrottleRepairedCredential(t *testing.T) {
	var calls atomic.Int32
	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	transport := roundTripFunc(func(*http.Request) (*http.Response, error) {
		n := calls.Add(1)
		if n == 1 {
			close(firstStarted)
			<-releaseFirst
			return claudeUsage429(), nil
		}
		return codexUsageResponseForTest(float64(n * 10)), nil
	})
	ref := &AccountRef{}
	client := &http.Client{Transport: transport}
	// Two refresh-grant versions may briefly share the same access token.
	// Their provider throttle cache keys are equal, but the older flight
	// must never overwrite the newer credential's throttle state.
	old := usageCredentialForTest("same-access", "old-grant")
	newer := usageCredentialForTest("same-access", "new-grant")
	oldDone := make(chan error, 1)
	go func() {
		_, _, err := ref.FetchUsageWindowsCached(context.Background(), client, old)
		oldDone <- err
	}()
	<-firstStarted
	result, fresh, err := ref.FetchUsageWindowsCached(context.Background(), client, newer)
	if err != nil || !fresh || len(result) != 1 || result[0].UsedPercent != 20 {
		t.Fatalf("newer credential fetch: windows=%+v fresh=%t err=%v", result, fresh, err)
	}
	close(releaseFirst)
	if err := <-oldDone; err == nil {
		t.Fatal("older 429 request unexpectedly succeeded")
	}
	// Expire the successful cache reading. The old flight's 429 must not
	// suppress another live fetch for the repaired credential.
	key := newer.ID + "\x00" + string(newer.Provider)
	ref.usageWindowsMu.Lock()
	entry := ref.usageWindows[key]
	entry.at = time.Now().Add(-usageWindowsTTL - time.Second)
	ref.usageWindows[key] = entry
	ref.usageWindowsMu.Unlock()
	result, fresh, err = ref.FetchUsageWindowsCached(context.Background(), client, newer)
	if err != nil || !fresh || len(result) != 1 || result[0].UsedPercent != 30 {
		t.Fatalf("older 429 poisoned repaired credential: windows=%+v fresh=%t err=%v", result, fresh, err)
	}
	if got := calls.Load(); got != 3 {
		t.Fatalf("provider calls=%d, want 3 without spurious retries", got)
	}
}

func TestUsageThrottleSameAccessTokenWithRepairedGrant(t *testing.T) {
	// A failed quota check belongs to the complete OAuth grant. Repairing
	// only the refresh grant must allow one fresh check immediately.
	transport := &countingTransport{}
	transport.responses = func() *http.Response {
		if transport.calls == 1 {
			return claudeUsage429()
		}
		return codexUsageResponseForTest(30)
	}
	ref := &AccountRef{}
	client := &http.Client{Transport: transport}
	old := usageCredentialForTest("same-access-token", "rejected-grant")
	if _, _, err := ref.FetchUsageWindowsCached(context.Background(), client, old); err == nil {
		t.Fatal("first credential should receive the upstream throttle")
	}
	repaired := usageCredentialForTest("same-access-token", "repaired-grant")
	windows, fresh, err := ref.FetchUsageWindowsCached(context.Background(), client, repaired)
	if err != nil || !fresh || len(windows) != 1 || windows[0].UsedPercent != 30 {
		t.Fatalf("new grant inherited old throttle: windows=%+v fresh=%t err=%v", windows, fresh, err)
	}
	if transport.calls != 2 {
		t.Fatalf("expected one quota check per distinct grant, got %d", transport.calls)
	}
}
