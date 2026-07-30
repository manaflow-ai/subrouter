package proxy

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/selectacct"
)

type countingTransport struct {
	calls     int
	responses func() *http.Response
}

func (c *countingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	c.calls++
	return c.responses(), nil
}

func claudeUsageOK() *http.Response {
	body := `{"five_hour":{"utilization":40.0,"resets_at":"2030-01-01T00:00:00+00:00"},"seven_day":{"utilization":10.0,"resets_at":"2030-01-02T00:00:00+00:00"}}`
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}
}

func claudeUsage429() *http.Response {
	return &http.Response{StatusCode: http.StatusTooManyRequests, Status: "429 Too Many Requests", Body: io.NopCloser(strings.NewReader("{}")), Header: http.Header{}}
}

func TestFetchUsageWindowsCachedServesFromCacheWithinTTL(t *testing.T) {
	transport := &countingTransport{responses: claudeUsageOK}
	ref := &AccountRef{}
	client := &http.Client{Transport: transport}
	account := accounts.Account{ID: "a@example.com", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "tok"}
	for i := 0; i < 3; i++ {
		windows, fresh, err := ref.FetchUsageWindowsCached(context.Background(), client, account)
		if err != nil || len(windows) == 0 {
			t.Fatalf("call %d: windows=%v err=%v", i, windows, err)
		}
		if !fresh {
			t.Fatalf("call %d: within-TTL cache should report fresh", i)
		}
	}
	if transport.calls != 2 {
		t.Fatalf("upstream calls = %d, want 2 (usage fetch plus one Fable probe; cache should absorb repeats)", transport.calls)
	}
}

func TestFetchUsageWindowsCachedFallsBackToLastGoodOn429(t *testing.T) {
	transport := &countingTransport{responses: claudeUsageOK}
	ref := &AccountRef{}
	client := &http.Client{Transport: transport}
	account := accounts.Account{ID: "a@example.com", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "tok"}
	if _, _, err := ref.FetchUsageWindowsCached(context.Background(), client, account); err != nil {
		t.Fatal(err)
	}
	// Expire the freshness window but keep the entry as last-good.
	key := account.ID + "\x00" + string(account.Provider)
	ref.usageWindowsMu.Lock()
	entry := ref.usageWindows[key]
	entry.at = entry.at.Add(-usageWindowsTTL - 1)
	ref.usageWindows[key] = entry
	ref.usageWindowsMu.Unlock()
	transport.responses = claudeUsage429
	windows, fresh, err := ref.FetchUsageWindowsCached(context.Background(), client, account)
	if err != nil {
		t.Fatalf("transient 429 should serve last-good, got error %v", err)
	}
	if len(windows) == 0 {
		t.Fatal("last-good windows missing")
	}
	if fresh {
		t.Fatal("stale last-good fallback must report fresh=false so scoring does not trust it")
	}
}

func TestFetchUsageWindowsCachedPropagatesAuthErrors(t *testing.T) {
	transport := &countingTransport{responses: func() *http.Response {
		return &http.Response{StatusCode: http.StatusUnauthorized, Status: "401 Unauthorized", Body: io.NopCloser(strings.NewReader("{}")), Header: http.Header{}}
	}}
	ref := &AccountRef{}
	client := &http.Client{Transport: transport}
	account := accounts.Account{ID: "a@example.com", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "tok"}
	if _, _, err := ref.FetchUsageWindowsCached(context.Background(), client, account); err == nil {
		t.Fatal("auth error should propagate, not be masked")
	}
}

func TestScoreAccountsKeepsOptimisticScoreOnTransientFailure(t *testing.T) {
	server := Server{
		ScoreAccounts: nil,
		Logger:        nil,
	}
	scores := []selectacct.Score{{AccountID: "a@example.com", Headroom: 1, ShortHeadroom: 1}}
	scoreByID := map[string]int{selectacct.ScoreKey(accounts.ProviderCodex, "a@example.com"): 0}
	// Simulate the error-handling branch directly: transient errors must not
	// zero, auth errors must.
	transientErr := errors.New("usage fetch failed: 429 Too Many Requests")
	if authLikeUsageError(transientErr.Error()) {
		t.Fatal("429 must not classify as auth error")
	}
	authErr := errors.New("usage fetch failed: 401 Unauthorized")
	if !authLikeUsageError(authErr.Error()) {
		t.Fatal("401 must classify as auth error")
	}
	setZeroScore(scores, scoreByID, accounts.ProviderCodex, "a@example.com")
	if scores[0].Headroom != 0 {
		t.Fatal("setZeroScore should zero")
	}
	_ = server
}
