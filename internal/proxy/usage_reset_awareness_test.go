package proxy

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/selectacct"
)

type quotaResetCountingTransport struct {
	mu    sync.Mutex
	calls map[string]int
}

func (c *quotaResetCountingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	c.mu.Lock()
	if c.calls == nil {
		c.calls = make(map[string]int)
	}
	c.calls[req.Header.Get("Authorization")]++
	c.mu.Unlock()
	return claudeUsageOK(), nil
}

func (c *quotaResetCountingTransport) count(token string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls["Bearer "+token]
}

func futureExhaustedQuota(account accounts.Account, until time.Time) selectacct.Score {
	return selectacct.Score{
		AccountID: account.ID, Provider: account.Provider,
		Headroom: 0, ShortHeadroom: 0, WeeklyHeadroom: 0,
		WeeklyHeadroomKnown: true, ExhaustedResetAt: until, Fresh: true,
	}
}

func TestKnownExhaustedQuotaSuppressesBackgroundUsageFetch(t *testing.T) {
	reset := time.Now().Add(3 * time.Hour)
	account := accounts.Account{
		ID: "exhausted@example.com", Provider: accounts.ProviderClaude,
		AuthMode: accounts.AuthModeOAuth, Token: "unused-token",
	}
	transport := &quotaResetCountingTransport{}
	ref := &AccountRef{
		accounts: []accounts.Account{account},
		client:   &http.Client{Transport: transport},
	}
	server := Server{
		AccountRef:    ref,
		SchedulerRef:  selectacct.NewSchedulerRef(selectacct.NewScheduler([]selectacct.Score{futureExhaustedQuota(account, reset)})),
		UsageScoreTTL: 30 * time.Second,
	}
	got, blocked := server.allOAuthAccountsWaitingForReset(time.Now())
	if !blocked || !got.Equal(reset) {
		t.Fatalf("all-account reset=(%v,%t), want (%v,true)", got, blocked, reset)
	}
	delay := server.nextUsageScoreRefreshDelay()
	if delay < 2*time.Hour+59*time.Minute || delay > 3*time.Hour+time.Minute {
		t.Fatalf("background wake=%v, want near the provider's 3h quota reset", delay)
	}
	for i := 0; i < 5; i++ {
		scores, fetched := server.scoreAccounts(context.Background(), []accounts.Account{account})
		if fetched != 0 || len(scores) != 1 || scores[0].Headroom != 0 {
			t.Fatalf("pass %d: fetched=%d scores=%+v, want preserved exhaustion", i, fetched, scores)
		}
	}
	if got := transport.count(account.Token); got != 0 {
		t.Fatalf("known-exhausted subscription triggered %d unnecessary upstream requests", got)
	}
}

func TestKnownExhaustionDoesNotStarveOtherHealthyAccounts(t *testing.T) {
	transport := &quotaResetCountingTransport{}
	ref, accountsList := multiProfileAccountRef(t, transport, 2)
	ref.accounts = append([]accounts.Account(nil), accountsList...)
	exhausted, healthy := accountsList[0], accountsList[1]
	scheduler := selectacct.NewScheduler([]selectacct.Score{
		futureExhaustedQuota(exhausted, time.Now().Add(4*time.Hour)),
		{AccountID: healthy.ID, Provider: healthy.Provider, Headroom: 0.9, ShortHeadroom: 0.9},
	})
	server := Server{AccountRef: ref, SchedulerRef: selectacct.NewSchedulerRef(scheduler), UsageScoreTTL: 30 * time.Second}
	if _, allBlocked := server.allOAuthAccountsWaitingForReset(time.Now()); allBlocked {
		t.Fatal("healthy account was incorrectly classified as waiting for reset")
	}
	if delay := server.nextUsageScoreRefreshDelay(); delay > time.Minute {
		t.Fatalf("healthy account should still refresh on normal TTL; got %v", delay)
	}
	_, scored := server.scoreAccounts(context.Background(), accountsList)
	if scored != 1 {
		t.Fatalf("scored=%d, want one healthy account to refresh", scored)
	}
	if got := transport.count(exhausted.Token); got != 0 {
		t.Fatalf("exhausted account made %d upstream calls while other account was healthy", got)
	}
	if got := transport.count(healthy.Token); got == 0 {
		t.Fatal("healthy account was not refreshed")
	}
}

func TestUnknownOrModelScopedQuotaDoesNotSuppressRefresh(t *testing.T) {
	now := time.Now()
	account := accounts.Account{ID: "a", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth}
	modelOnly := selectacct.Score{
		AccountID: account.ID, Provider: account.Provider,
		Headroom: 0.8, ShortHeadroom: 0.8,
		ModelScores: map[string]selectacct.Score{
			selectacct.ModelKey("claude-fable"): futureExhaustedQuota(account, now.Add(4*time.Hour)),
		},
	}
	if _, ok := knownAccountWideQuotaReset(modelOnly, now); ok {
		t.Fatal("model-specific exhaustion must not suppress all account usage checks")
	}
	unknown := futureExhaustedQuota(account, now.Add(4*time.Hour))
	unknown.ExhaustedResetUnknown = true
	if _, ok := knownAccountWideQuotaReset(unknown, now); ok {
		t.Fatal("unknown reset must not be treated as a certain wake deadline")
	}
	expired := futureExhaustedQuota(account, now.Add(-time.Second))
	if _, ok := knownAccountWideQuotaReset(expired, now); ok {
		t.Fatal("passed reset must allow fresh provider observation")
	}
}
