package proxy

import (
	"context"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	accountpkg "github.com/manaflow-ai/subrouter/account"
	"github.com/manaflow-ai/subrouter/internal/accounts"
	agentclaude "github.com/manaflow-ai/subrouter/internal/agents/claude"
	"github.com/manaflow-ai/subrouter/selectacct"
)

// Anthropic answers a revoked OAuth token's usage probe with 429, so the
// score sweep used to poll revoked accounts forever. It must skip them
// until re-login changes the credential.
func TestScoreAccountsSkipsRevokedClaudeCredentialUntilRelogin(t *testing.T) {
	transport := &usageRoundTripper{responses: []*http.Response{usage429Response(), usageOKResponse()}}
	ref := cacheTestAccountRef(t, transport)
	account := accounts.Account{ID: "claude@example.com", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "tok"}
	costs := newClaudeCostStore(filepath.Join(t.TempDir(), "costs.json"))
	// The refreshed account carries the on-disk credential version.
	costs.markRevoked(account.ID, accountpkg.OAuthCredentialVersion("tok", "ref"))
	costs.loadedAt = time.Time{}

	server := Server{
		AccountRef:  ref,
		claudeCosts: costs,
		SchedulerRef: selectacct.NewSchedulerRef(selectacct.NewScheduler([]selectacct.Score{
			{AccountID: account.ID, Provider: accounts.ProviderClaude, Headroom: 1, ShortHeadroom: 1},
		})),
	}
	scores, scored := server.scoreAccounts(context.Background(), []accounts.Account{account})
	if transport.calls != 0 {
		t.Fatalf("transport.calls = %d, want 0 (a revoked credential must not be polled)", transport.calls)
	}
	if scored != 0 || len(scores) != 1 || scores[0].Headroom != 0 || scores[0].ShortHeadroom != 0 {
		t.Fatalf("scored=%d scores=%+v, want the revoked account zeroed without a fetch", scored, scores)
	}

	transport.responses = []*http.Response{usageOKResponse()}
	if err := ref.claudeStore.ImportProfileCredential(account.ID, agentclaude.CredentialInfo{
		AccessToken: "tok-relogged", RefreshToken: "ref-relogged", SubscriptionType: "max",
		ExpiresAt: time.Now().Add(time.Hour).UnixMilli(),
	}); err != nil {
		t.Fatal(err)
	}
	relogged := account
	relogged.Token = "tok-relogged"
	scores, scored = server.scoreAccounts(context.Background(), []accounts.Account{relogged})
	if transport.calls == 0 || scored != 1 {
		t.Fatalf("calls=%d scored=%d scores=%+v, want a re-login to resume polling", transport.calls, scored, scores)
	}
}

type tokenCountingUsageTransport struct {
	mu    sync.Mutex
	calls map[string]int
}

func (c *tokenCountingUsageTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	c.mu.Lock()
	c.calls[strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer ")]++
	c.mu.Unlock()
	return &http.Response{StatusCode: http.StatusTooManyRequests, Status: "429 Too Many Requests",
		Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"type":"error"}`))}, nil
}

func (c *tokenCountingUsageTransport) count(token string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls[token]
}

func TestUsageStatusSweepSkipsRevokedClaudeCredentialUntilRelogin(t *testing.T) {
	store := agentclaude.Store{Dir: t.TempDir()}
	expires := time.Now().Add(time.Hour).UnixMilli()
	for name, token := range map[string]string{"revoked": "revoked-token", "healthy": "healthy-token"} {
		if err := store.ImportProfileCredential(name, agentclaude.CredentialInfo{
			AccessToken: token, RefreshToken: token + "-refresh", SubscriptionType: "max", ExpiresAt: expires,
		}); err != nil {
			t.Fatal(err)
		}
	}
	transport := &tokenCountingUsageTransport{calls: map[string]int{}}
	ref := &AccountRef{
		store:       accounts.CodexStore{Dir: t.TempDir()},
		claudeStore: store,
		client:      &http.Client{Transport: transport},
	}
	costs := newClaudeCostStore(filepath.Join(t.TempDir(), "costs.json"))
	costs.markRevoked("revoked", accountpkg.OAuthCredentialVersion("revoked-token", "revoked-token-refresh"))
	costs.loadedAt = time.Time{}
	ref.claudeCosts.Store(costs)

	rows := map[string]AccountUsageStatus{}
	for _, status := range ref.UsageStatusesFresh(context.Background()) {
		rows[status.ID] = status
	}
	if got := transport.count("revoked-token"); got != 0 {
		t.Fatalf("revoked credential polled %d times, want 0", got)
	}
	if transport.count("healthy-token") == 0 {
		t.Fatal("healthy account was not polled")
	}
	revoked := rows["revoked"]
	if revoked.RevokedSince.IsZero() || revoked.UsageThrottled || revoked.Error != "" {
		t.Fatalf("revoked row = %+v, want revoked_since and no throttle/error", revoked)
	}
	if !rows["healthy"].UsageThrottled {
		t.Fatalf("healthy row = %+v, want the 429 still reported as a throttle", rows["healthy"])
	}

	// Re-login writes a new credential; the sweep polls it again.
	if err := store.ImportProfileCredential("revoked", agentclaude.CredentialInfo{
		AccessToken: "relogged-token", RefreshToken: "relogged-refresh", SubscriptionType: "max", ExpiresAt: expires,
	}); err != nil {
		t.Fatal(err)
	}
	for _, status := range ref.UsageStatusesFresh(context.Background()) {
		if status.ID == "revoked" && !status.RevokedSince.IsZero() {
			t.Fatalf("re-logged row still revoked: %+v", status)
		}
	}
	if transport.count("relogged-token") == 0 {
		t.Fatal("re-logged credential was not polled")
	}
}
