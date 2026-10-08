package proxy

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/selectacct"
)

type quotaResetCountingTransport struct {
	mu      sync.Mutex
	calls   map[string]int
	started chan string
}

func (c *quotaResetCountingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	c.mu.Lock()
	if c.calls == nil {
		c.calls = make(map[string]int)
	}
	c.calls[req.Header.Get("Authorization")]++
	c.mu.Unlock()
	select {
	case c.started <- req.Header.Get("Authorization"):
	default:
	}
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
	if delay < 30*time.Second || delay > 35*time.Second {
		t.Fatalf("background recheck=%v, want a bounded recheck near the 30s score TTL", delay)
	}
	server.UsageScoreTTL = 2 * time.Hour
	if delay := server.nextUsageScoreRefreshDelay(); delay > time.Minute+8*time.Second {
		t.Fatalf("large score TTL allowed an unbounded quota recheck=%v", delay)
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
	// A re-login/account generation change invalidates measured scores.
	// Old quota clocks must not suppress fresh credential verification.
	server.SchedulerRef.SetUpdatedAt(time.Time{})
	if _, blocked := server.allOAuthAccountsWaitingForReset(time.Now()); blocked {
		t.Fatal("invalidated score generation retained its old provider reset hold")
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

func TestKnownExhaustedQuotaNoticesReplacementCredential(t *testing.T) {
	transport := &quotaResetCountingTransport{started: make(chan string, 16)}
	ref, all := multiProfileAccountRef(t, transport, 1)
	ref.accounts = all
	account := all[0]
	scheduler := selectacct.NewSchedulerRef(selectacct.NewScheduler([]selectacct.Score{
		futureExhaustedQuota(account, time.Now().Add(3*time.Hour)),
	}))
	scheduler.SetUpdatedAt(time.Now().Add(-time.Hour))
	server := Server{AccountRef: ref, SchedulerRef: scheduler, UsageScoreTTL: 10 * time.Millisecond}
	ctx := t.Context()

	// Rechecks while the original credential is exhausted stay local and do
	// not poll its provider quota endpoint.
	for range 3 {
		server.refreshUsageScoresForRequest(ctx)
	}
	select {
	case token := <-transport.started:
		t.Fatalf("exhausted account triggered a provider poll: %q", token)
	case <-time.After(100 * time.Millisecond):
	}

	// Simulate a login committed by another supervisor worker. The refresher
	// has to observe the disk generation before deciding the old quota clock
	// still applies, then verify the replacement credential immediately.
	credential := fmt.Sprintf(`{"claudeAiOauth":{"accessToken":"replacement-token","refreshToken":"replacement-refresh","expiresAt":%d,"subscriptionType":"max"}}`, time.Now().Add(time.Hour).UnixMilli())
	err := withAccountDiskMutationPublication(ctx, ref.store, advanceAccountDiskGeneration, func(publish func() error) error {
		if err := os.WriteFile(filepath.Join(ref.claudeStore.ClaudeConfigDir(account.ID), ".credentials.json"), []byte(credential), 0o600); err != nil {
			return err
		}
		return publish()
	})
	if err != nil {
		t.Fatal(err)
	}
	server.refreshUsageScoresForRequest(ctx)
	select {
	case token := <-transport.started:
		if token != "Bearer replacement-token" {
			t.Fatalf("usage poll used %q, want the replacement credential", token)
		}
	case <-time.After(time.Second):
		t.Fatal("replacement login remained asleep behind the old 3h quota reset")
	}
	if got := transport.count(account.Token); got != 0 {
		t.Fatalf("known-exhausted credential was polled %d times", got)
	}
}
