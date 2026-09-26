package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/internal/broker"
	"github.com/manaflow-ai/subrouter/internal/tenant"
	"github.com/manaflow-ai/subrouter/selectacct"
	"github.com/manaflow-ai/subrouter/session"
)

func tenantLeaseTestAccount(id string, provider accounts.Provider) accounts.Account {
	return accounts.Account{
		ID: id, Provider: provider, AuthMode: accounts.AuthModeOAuth,
		Token: "token-" + id,
	}
}

func tenantLeaseTestServer(items ...accounts.Account) *Server {
	scores := make([]selectacct.Score, 0, len(items))
	for _, item := range items {
		scores = append(scores, selectacct.Score{
			AccountID: item.ID, Provider: item.Provider,
			Headroom: 1, ShortHeadroom: 1,
		})
	}
	return &Server{
		Accounts: items,
		SchedulerRef: selectacct.NewSchedulerRef(
			selectacct.NewScheduler(scores),
		),
	}
}

func tenantLeaseTestBrokerClient(
	t *testing.T,
	server *Server,
	store *tenantCredentialLeaseStore,
) *broker.Client {
	t.Helper()
	const tenantKey = "srt_0123456789abcdef0123456789abcdef"
	hosted := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		prefix := "/t/" + tenantKey
		if !strings.HasPrefix(r.URL.Path, prefix) {
			http.NotFound(w, r)
			return
		}
		r.URL.Path = strings.TrimPrefix(r.URL.Path, prefix)
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/_subrouter/leases":
			store.handleIssue(server, tenant.Tenant{ID: "team"}, w, r)
		case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/_subrouter/leases/") && strings.HasSuffix(r.URL.Path, "/events"):
			store.handleReport(server, w, r)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(hosted.Close)
	client := broker.NewClient(broker.Config{
		Version: 1, BaseURL: broker.DefaultBaseURL,
		AccessToken: "cmux-access", RefreshToken: "cmux-refresh",
		TeamID: "team", CredentialSource: broker.CredentialSourceTeam,
		HostedURL: hosted.URL, TenantKey: tenantKey,
	})
	client.HTTPClient = hosted.Client()
	return client
}

func tenantLeaseTestLease(
	account accounts.Account,
	agentType string,
	sessionID string,
	model string,
	now time.Time,
) tenantCredentialLease {
	return tenantCredentialLease{
		accountID: account.ID, provider: account.Provider,
		authMode: account.AuthMode, credentialIdentity: account.CredentialIdentity(),
		agentType: agentType, sessionID: sessionID,
		model:     tenantCredentialLeasePoolModel(account.Provider, model),
		expiresAt: now.Add(time.Minute),
	}
}

func TestTenantCredentialLeaseReportStatusMatrix(t *testing.T) {
	now := time.Now()
	tests := map[string]struct {
		lease     tenantCredentialLease
		report    tenantCredentialLeaseReport
		wantScope broker.LeaseCooldownScope
		wantError bool
	}{
		"success": {
			lease:  tenantCredentialLease{provider: accounts.ProviderCodex},
			report: tenantCredentialLeaseReport{Outcome: broker.LeaseSuccess, StatusCode: http.StatusOK},
		},
		"unauthorized": {
			lease:     tenantCredentialLease{provider: accounts.ProviderCodex},
			report:    tenantCredentialLeaseReport{Outcome: broker.LeaseUnauthorized, StatusCode: http.StatusUnauthorized},
			wantScope: broker.LeaseCooldownAccount,
		},
		"unauthorized wrong status": {
			lease:     tenantCredentialLease{provider: accounts.ProviderCodex},
			report:    tenantCredentialLeaseReport{Outcome: broker.LeaseUnauthorized, StatusCode: http.StatusForbidden},
			wantError: true,
		},
		"Claude OAuth forbidden": {
			lease:     tenantCredentialLease{provider: accounts.ProviderClaude, authMode: accounts.AuthModeOAuth},
			report:    tenantCredentialLeaseReport{Outcome: broker.LeaseForbidden, StatusCode: http.StatusForbidden},
			wantScope: broker.LeaseCooldownAccount,
		},
		"API key forbidden": {
			lease:     tenantCredentialLease{provider: accounts.ProviderClaude, authMode: accounts.AuthModeAPIKey},
			report:    tenantCredentialLeaseReport{Outcome: broker.LeaseForbidden, StatusCode: http.StatusForbidden, Scope: broker.LeaseCooldownAccount},
			wantScope: broker.LeaseCooldownQuota,
		},
		"Claude unified rejected 403": {
			lease:     tenantCredentialLease{provider: accounts.ProviderClaude, authMode: accounts.AuthModeOAuth},
			report:    tenantCredentialLeaseReport{Outcome: broker.LeaseRateLimited, StatusCode: http.StatusForbidden, Scope: broker.LeaseCooldownAccount},
			wantScope: broker.LeaseCooldownAccount,
		},
		"Claude rejected 500": {
			lease:     tenantCredentialLease{provider: accounts.ProviderClaude, authMode: accounts.AuthModeOAuth},
			report:    tenantCredentialLeaseReport{Outcome: broker.LeaseRateLimited, StatusCode: http.StatusInternalServerError},
			wantScope: broker.LeaseCooldownQuota,
		},
		"Claude successful response cannot masquerade as rate limit": {
			lease:     tenantCredentialLease{provider: accounts.ProviderClaude, authMode: accounts.AuthModeOAuth},
			report:    tenantCredentialLeaseReport{Outcome: broker.LeaseRateLimited, StatusCode: http.StatusOK},
			wantError: true,
		},
		"Claude redirect cannot masquerade as rate limit": {
			lease:     tenantCredentialLease{provider: accounts.ProviderClaude, authMode: accounts.AuthModeOAuth},
			report:    tenantCredentialLeaseReport{Outcome: broker.LeaseRateLimited, StatusCode: http.StatusTemporaryRedirect},
			wantError: true,
		},
		"Claude rate limit without scope stays model local": {
			lease:     tenantCredentialLease{provider: accounts.ProviderClaude, authMode: accounts.AuthModeOAuth},
			report:    tenantCredentialLeaseReport{Outcome: broker.LeaseRateLimited, StatusCode: http.StatusForbidden},
			wantScope: broker.LeaseCooldownQuota,
		},
		"Codex 429": {
			lease:     tenantCredentialLease{provider: accounts.ProviderCodex},
			report:    tenantCredentialLeaseReport{Outcome: broker.LeaseRateLimited, StatusCode: http.StatusTooManyRequests, Scope: broker.LeaseCooldownQuota},
			wantScope: broker.LeaseCooldownAccount,
		},
		"Codex forbidden cannot masquerade as rate limit": {
			lease:     tenantCredentialLease{provider: accounts.ProviderCodex},
			report:    tenantCredentialLeaseReport{Outcome: broker.LeaseRateLimited, StatusCode: http.StatusForbidden},
			wantError: true,
		},
		"provider error": {
			lease:  tenantCredentialLease{provider: accounts.ProviderCodex},
			report: tenantCredentialLeaseReport{Outcome: broker.LeaseProviderError, StatusCode: http.StatusBadGateway},
		},
		"unknown scope": {
			lease:     tenantCredentialLease{provider: accounts.ProviderCodex},
			report:    tenantCredentialLeaseReport{Outcome: broker.LeaseRateLimited, StatusCode: http.StatusTooManyRequests, Scope: "global"},
			wantError: true,
		},
		"retry deadline on non-rate-limit": {
			lease:     tenantCredentialLease{provider: accounts.ProviderCodex},
			report:    tenantCredentialLeaseReport{Outcome: broker.LeaseUnauthorized, StatusCode: http.StatusUnauthorized, RetryAt: now.Add(time.Minute).UnixMilli()},
			wantError: true,
		},
	}
	for name, testCase := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := normalizeTenantCredentialLeaseReport(testCase.lease, testCase.report, now)
			if testCase.wantError {
				if err == nil {
					t.Fatalf("report accepted: %+v", got)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got.Scope != testCase.wantScope {
				t.Fatalf("scope = %q, want %q", got.Scope, testCase.wantScope)
			}
		})
	}
}

func TestTenantCredentialLeaseReportIsSessionLocal(t *testing.T) {
	now := time.Now()
	accountA := tenantLeaseTestAccount("account-a", accounts.ProviderCodex)
	accountB := tenantLeaseTestAccount("account-b", accounts.ProviderCodex)
	server := tenantLeaseTestServer(accountA, accountB)
	store := newTenantCredentialLeaseStore()
	store.put("lease-a", tenantLeaseTestLease(accountA, "codex", "session-a", "gpt-5", now), now)

	request := httptest.NewRequest(
		http.MethodPost,
		"/_subrouter/leases/lease-a/events",
		strings.NewReader(`{"outcome":"rate_limited","statusCode":429,"scope":"account","retryAt":9999999999999}`),
	)
	response := httptest.NewRecorder()
	store.handleReport(server, response, request)
	if response.Code != http.StatusNoContent {
		t.Fatalf("report status = %d: %s", response.Code, response.Body.String())
	}

	shared := server.SchedulerRef.Get()
	if shared.Exhausted(accounts.ProviderCodex, accountA.ID) ||
		shared.ForModel("gpt-5").Exhausted(accounts.ProviderCodex, accountA.ID) {
		t.Fatal("tenant report mutated the shared scheduler")
	}

	sameSession := tenantCredentialLeaseRequest{
		Provider: string(accounts.ProviderCodex), AgentType: "codex",
		SessionID: "session-a", PreferAccountID: accountA.ID, Model: "gpt-5",
	}
	picked, err := pickTenantCredentialLeaseAccount(store, server, []accounts.Account{accountA, accountB}, nil, sameSession)
	if err != nil {
		t.Fatal(err)
	}
	if picked.ID != accountB.ID {
		t.Fatalf("same-session pick = %s, want %s", picked.ID, accountB.ID)
	}

	otherSession := sameSession
	otherSession.SessionID = "session-b"
	picked, err = pickTenantCredentialLeaseAccount(store, server, []accounts.Account{accountA, accountB}, nil, otherSession)
	if err != nil {
		t.Fatal(err)
	}
	if picked.ID != accountA.ID {
		t.Fatalf("other-session pick = %s, want preferred %s", picked.ID, accountA.ID)
	}
}

func TestTenantCredentialLeaseCallerChosenSessionCannotTargetAnotherCapability(t *testing.T) {
	now := time.Now()
	account := tenantLeaseTestAccount("account-a", accounts.ProviderCodex)
	store := newTenantCredentialLeaseStore()
	legitimate := tenantCredentialLeaseRequest{
		Provider: string(accounts.ProviderCodex), AgentType: "codex",
		SessionID: "shared-session", Model: "gpt-5",
	}
	legitimateToken, err := store.resolveSessionToken(legitimate, accounts.ProviderCodex, now)
	if err != nil {
		t.Fatal(err)
	}
	legitimate.SessionToken = legitimateToken
	lease := tenantLeaseTestLease(account, "codex", legitimate.SessionID, legitimate.Model, now)
	lease.sessionToken = legitimateToken
	store.put("lease", lease, now)
	if _, err := store.consumeReport("lease", tenantCredentialLeaseReport{
		Outcome: broker.LeaseRateLimited, StatusCode: http.StatusTooManyRequests,
	}, now); err != nil {
		t.Fatal(err)
	}

	attacker := legitimate
	attacker.SessionToken = ""
	attackerToken, err := store.resolveSessionToken(attacker, accounts.ProviderCodex, now)
	if err != nil {
		t.Fatal(err)
	}
	if attackerToken == legitimateToken {
		t.Fatal("caller without capability inherited another session's token")
	}
	attacker.SessionToken = attackerToken
	pool := tenantCredentialLeasePoolModel(accounts.ProviderCodex, attacker.Model)
	if _, avoided := store.avoidanceUntil(attacker, account, pool, now); avoided {
		t.Fatal("caller-chosen session id inherited another capability's avoidance")
	}
	if _, avoided := store.avoidanceUntil(legitimate, account, pool, now); !avoided {
		t.Fatal("originating capability did not retain its avoidance")
	}
}

func TestTenantCredentialLeaseIssueReportReissueUsesCanonicalPoolKey(t *testing.T) {
	accountA := tenantLeaseTestAccount("account-a", accounts.ProviderClaude)
	accountB := tenantLeaseTestAccount("account-b", accounts.ProviderClaude)
	server := tenantLeaseTestServer(accountA, accountB)
	server.RefreshAccountFn = func(_ context.Context, account accounts.Account) (accounts.Account, error) {
		return account, nil
	}
	store := newTenantCredentialLeaseStore()
	issue := func(sessionToken string) struct {
		LeaseID      string `json:"leaseId"`
		AccountID    string `json:"accountId"`
		SessionToken string `json:"sessionToken"`
	} {
		body := `{"provider":"claude","agentType":"claude","sessionId":"session-a","preferAccountId":"account-a","model":"op-us"}`
		if sessionToken != "" {
			body = fmt.Sprintf(
				`{"provider":"claude","agentType":"claude","sessionId":"session-a","preferAccountId":"account-a","model":"op-us","sessionToken":%q}`,
				sessionToken,
			)
		}
		response := httptest.NewRecorder()
		store.handleIssue(
			server, tenant.Tenant{ID: "team"}, response,
			httptest.NewRequest(http.MethodPost, "/_subrouter/leases", strings.NewReader(body)),
		)
		if response.Code != http.StatusOK {
			t.Fatalf("issue status = %d: %s", response.Code, response.Body.String())
		}
		var envelope struct {
			Lease struct {
				LeaseID      string `json:"leaseId"`
				AccountID    string `json:"accountId"`
				SessionToken string `json:"sessionToken"`
			} `json:"lease"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
			t.Fatal(err)
		}
		return envelope.Lease
	}
	first := issue("")
	if first.AccountID != accountA.ID || first.SessionToken == "" {
		t.Fatalf("first lease = %#v", first)
	}
	report := httptest.NewRecorder()
	store.handleReport(server, report, httptest.NewRequest(
		http.MethodPost,
		"/_subrouter/leases/"+first.LeaseID+"/events",
		strings.NewReader(`{"outcome":"rate_limited","statusCode":403}`),
	))
	if report.Code != http.StatusNoContent {
		t.Fatalf("report status = %d: %s", report.Code, report.Body.String())
	}
	second := issue(first.SessionToken)
	if second.AccountID != accountB.ID {
		t.Fatalf("reissued account = %s, want %s", second.AccountID, accountB.ID)
	}
	if second.SessionToken != first.SessionToken {
		t.Fatalf("session capability changed: first=%q second=%q", first.SessionToken, second.SessionToken)
	}
}

func TestTenantCredentialLeaseAvoidanceKeysRoundTripWithSessionCapability(t *testing.T) {
	now := time.Now()
	for _, test := range []struct {
		name    string
		outcome broker.LeaseOutcome
		status  int
	}{
		{name: "credential", outcome: broker.LeaseUnauthorized, status: http.StatusUnauthorized},
		{name: "quota", outcome: broker.LeaseRateLimited, status: http.StatusTooManyRequests},
	} {
		t.Run(test.name, func(t *testing.T) {
			account := tenantLeaseTestAccount("account-a", accounts.ProviderClaude)
			store := newTenantCredentialLeaseStore()
			input := tenantCredentialLeaseRequest{
				Provider: string(account.Provider), AgentType: "claude",
				SessionID: "session-a", SessionToken: "server-issued", Model: "claude-opus-4",
			}
			lease := tenantLeaseTestLease(account, "claude", input.SessionID, input.Model, now)
			lease.sessionToken = input.SessionToken
			store.put("lease", lease, now)
			if _, err := store.consumeReport("lease", tenantCredentialLeaseReport{
				Outcome: test.outcome, StatusCode: test.status,
			}, now); err != nil {
				t.Fatal(err)
			}
			pool := tenantCredentialLeasePoolModel(account.Provider, input.Model)
			if _, avoided := store.avoidanceUntil(input, account, pool, now); !avoided {
				t.Fatal("inserted avoidance did not round-trip through lookup key")
			}
		})
	}
}

func TestTenantCredentialLeaseClaudeQuota403AndModelLessFailOver(t *testing.T) {
	now := time.Now()
	claudeA := tenantLeaseTestAccount("claude-a", accounts.ProviderClaude)
	claudeB := tenantLeaseTestAccount("claude-b", accounts.ProviderClaude)
	server := tenantLeaseTestServer(claudeA, claudeB)
	store := newTenantCredentialLeaseStore()
	store.put("claude-lease", tenantLeaseTestLease(claudeA, "claude", "claude-session", "claude-opus-4-8", now), now)
	if _, err := store.consumeReport("claude-lease", tenantCredentialLeaseReport{
		Outcome: broker.LeaseRateLimited, StatusCode: http.StatusForbidden,
		Scope: broker.LeaseCooldownAccount,
	}, now); err != nil {
		t.Fatalf("Claude unified rejected 403 was not accepted: %v", err)
	}
	if _, avoided := store.avoidanceUntil(tenantCredentialLeaseRequest{
		AgentType: "claude", SessionID: "claude-session",
	}, claudeA, tenantCredentialLeasePoolModel(accounts.ProviderClaude, "claude-sonnet-4"), now); !avoided {
		t.Fatal("account-scoped Claude report did not block the same account across model pools")
	}
	picked, err := pickTenantCredentialLeaseAccount(store, server, []accounts.Account{claudeA, claudeB}, nil, tenantCredentialLeaseRequest{
		Provider: string(accounts.ProviderClaude), AgentType: "claude",
		SessionID: "claude-session", PreferAccountID: claudeA.ID,
		Model: "claude-opus-4-8",
	})
	if err != nil {
		t.Fatal(err)
	}
	if picked.ID != claudeB.ID {
		t.Fatalf("Claude quota failover pick = %s, want %s", picked.ID, claudeB.ID)
	}

	codexA := tenantLeaseTestAccount("codex-a", accounts.ProviderCodex)
	codexB := tenantLeaseTestAccount("codex-b", accounts.ProviderCodex)
	modelLessServer := tenantLeaseTestServer(codexA, codexB)
	modelLessStore := newTenantCredentialLeaseStore()
	modelLessStore.put("model-less", tenantLeaseTestLease(codexA, "codex", "catalog-session", "", now), now)
	if lease, ok := modelLessStore.get("model-less", now); !ok || lease.model != tenantCredentialLeaseUnspecifiedModelPool {
		t.Fatalf("model-less lease pool=%q exists=%v, want sentinel %q", lease.model, ok, tenantCredentialLeaseUnspecifiedModelPool)
	}
	if _, err := modelLessStore.consumeReport("model-less", tenantCredentialLeaseReport{
		Outcome: broker.LeaseForbidden, StatusCode: http.StatusForbidden,
	}, now); err != nil {
		t.Fatalf("model-less report was not accepted: %v", err)
	}
	picked, err = pickTenantCredentialLeaseAccount(modelLessStore, modelLessServer, []accounts.Account{codexA, codexB}, nil, tenantCredentialLeaseRequest{
		Provider: string(accounts.ProviderCodex), AgentType: "codex",
		SessionID: "catalog-session", PreferAccountID: codexA.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if picked.ID != codexB.ID {
		t.Fatalf("model-less failover pick = %s, want %s", picked.ID, codexB.ID)
	}
}

func TestTenantCredentialLeaseFiltersBeforePreferredAndSticky(t *testing.T) {
	now := time.Now()
	accountA := tenantLeaseTestAccount("account-a", accounts.ProviderCodex)
	accountB := tenantLeaseTestAccount("account-b", accounts.ProviderCodex)
	server := tenantLeaseTestServer(accountA, accountB)
	sessions, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	server.Sessions = sessions
	if _, err := sessions.Put("codex", "session-a", accountA.ID, ""); err != nil {
		t.Fatal(err)
	}
	store := newTenantCredentialLeaseStore()
	store.put("lease-a", tenantLeaseTestLease(accountA, "codex", "session-a", "gpt-5", now), now)
	if _, err := store.consumeReport("lease-a", tenantCredentialLeaseReport{
		Outcome: broker.LeaseRateLimited, StatusCode: http.StatusTooManyRequests,
	}, now); err != nil {
		t.Fatal(err)
	}
	picked, err := pickTenantCredentialLeaseAccount(store, server, []accounts.Account{accountA, accountB}, nil, tenantCredentialLeaseRequest{
		Provider: string(accounts.ProviderCodex), AgentType: "codex",
		SessionID: "session-a", PreferAccountID: accountA.ID, Model: "gpt-5",
	})
	if err != nil {
		t.Fatal(err)
	}
	if picked.ID != accountB.ID {
		t.Fatalf("pick = %s, want %s after preferred+sticky filtering", picked.ID, accountB.ID)
	}

	otherStore := newTenantCredentialLeaseStore()
	server.SchedulerRef.MarkExhaustedUntil(
		accounts.ProviderCodex, accountA.ID, "gpt-5", now.Add(time.Minute),
	)
	picked, err = pickTenantCredentialLeaseAccount(otherStore, server, []accounts.Account{accountA, accountB}, nil, tenantCredentialLeaseRequest{
		Provider: string(accounts.ProviderCodex), AgentType: "codex",
		SessionID: "other-session", PreferAccountID: accountA.ID, Model: "gpt-5",
	})
	if err != nil {
		t.Fatal(err)
	}
	if picked.ID != accountB.ID {
		t.Fatalf("shared-cooldown pick = %s, want %s", picked.ID, accountB.ID)
	}
}

func TestTenantCredentialBrokerPreferenceYieldsToReroutedStickySession(t *testing.T) {
	accountA := tenantLeaseTestAccount("account-a", accounts.ProviderClaude)
	accountB := tenantLeaseTestAccount("account-b", accounts.ProviderClaude)
	server := tenantLeaseTestServer(accountA, accountB)
	sessions, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	server.Sessions = sessions
	server.RefreshAccountFn = func(_ context.Context, account accounts.Account) (accounts.Account, error) {
		return account, nil
	}
	store := newTenantCredentialLeaseStore()
	client := tenantLeaseTestBrokerClient(t, server, store)
	request := broker.LeaseRequest{
		Provider: accounts.ProviderClaude, AgentType: "claude",
		SessionID: "preferred-session", PreferAccountID: accountA.ID, Model: "claude-opus-4",
	}

	first, err := client.Lease(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if first.Account.ID != accountA.ID {
		t.Fatalf("initial preferred lease = %s, want %s", first.Account.ID, accountA.ID)
	}
	if err := client.Report(context.Background(), first.ID, broker.LeaseReport{
		Outcome: broker.LeaseRateLimited, StatusCode: http.StatusTooManyRequests,
	}); err != nil {
		t.Fatal(err)
	}
	second, err := client.Lease(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if second.Account.ID != accountB.ID {
		t.Fatalf("rerouted lease = %s, want %s while preferred account is avoided", second.Account.ID, accountB.ID)
	}

	// Make A eligible again without changing the durable session placement.
	// The next lease still carries the original soft preference, but sticky B
	// must now outrank it.
	store.mu.Lock()
	for key, avoidance := range store.avoidances {
		avoidance.expiresAt = time.Now().Add(-time.Minute)
		store.avoidances[key] = avoidance
	}
	store.mu.Unlock()
	client.InvalidateLease(second.ID)
	third, err := client.Lease(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if third.Account.ID != accountB.ID {
		t.Fatalf("post-recovery lease = %s, want sticky %s instead of preferred %s", third.Account.ID, accountB.ID, accountA.ID)
	}
}

func TestTenantCredentialBrokerForcedAccountOverridesStickyAndFailsClosed(t *testing.T) {
	accountA := tenantLeaseTestAccount("account-a", accounts.ProviderClaude)
	accountB := tenantLeaseTestAccount("account-b", accounts.ProviderClaude)
	server := tenantLeaseTestServer(accountA, accountB)
	sessions, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sessions.Put("claude", "forced-session", accountB.ID, ""); err != nil {
		t.Fatal(err)
	}
	server.Sessions = sessions
	server.RefreshAccountFn = func(_ context.Context, account accounts.Account) (accounts.Account, error) {
		return account, nil
	}
	store := newTenantCredentialLeaseStore()
	client := tenantLeaseTestBrokerClient(t, server, store)
	request := broker.LeaseRequest{
		Provider: accounts.ProviderClaude, AgentType: "claude",
		SessionID: "forced-session", ForceAccountID: accountA.ID, Model: "claude-opus-4",
	}

	forced, err := client.Lease(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if forced.Account.ID != accountA.ID {
		t.Fatalf("forced lease = %s, want %s despite sticky %s", forced.Account.ID, accountA.ID, accountB.ID)
	}
	if err := client.Report(context.Background(), forced.ID, broker.LeaseReport{
		Outcome: broker.LeaseRateLimited, StatusCode: http.StatusTooManyRequests,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Lease(context.Background(), request); err == nil {
		t.Fatalf("avoided forced account silently failed over to %s", accountB.ID)
	} else {
		var statusErr *broker.HTTPStatusError
		if !errors.As(err, &statusErr) || statusErr.StatusCode != http.StatusServiceUnavailable {
			t.Fatalf("forced unavailable error = %v, want broker 503", err)
		}
	}
}

func TestTenantCredentialLeaseRetriesMeasuredZeroAfterExplicitCooldownExpires(t *testing.T) {
	now := time.Now()
	account := tenantLeaseTestAccount("recovered-account", accounts.ProviderCodex)
	server := tenantLeaseTestServer(account)
	server.SchedulerRef.Set(selectacct.NewScheduler([]selectacct.Score{{
		AccountID: account.ID, Provider: account.Provider,
		Headroom: 0, ShortHeadroom: 0, Fresh: false,
	}}))
	server.SchedulerRef.MarkExhaustedUntil(
		account.Provider, account.ID, "gpt-5", now.Add(-time.Second),
	)

	picked, err := pickTenantCredentialLeaseAccount(
		newTenantCredentialLeaseStore(), server, []accounts.Account{account}, nil,
		tenantCredentialLeaseRequest{
			Provider: string(account.Provider), AgentType: "codex",
			SessionID: "session-a", PreferAccountID: account.ID, Model: "gpt-5",
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if picked.ID != account.ID {
		t.Fatalf("picked = %s, want recovered %s", picked.ID, account.ID)
	}
	published := false
	if _, ok := server.SchedulerRef.RunIfAccountNotBlocked(
		account.Provider, account.ID, "gpt-5", now, func() { published = true },
	); !ok || !published {
		t.Fatal("expired cooldown did not allow the recovery probe to publish")
	}
	if _, ok := server.SchedulerRef.RunIfAccountNotBlocked(
		account.Provider, account.ID, "gpt-5", now, func() { t.Fatal("second probe published") },
	); ok {
		t.Fatal("expired cooldown allowed more than one recovery probe")
	}
}

func TestTenantCredentialLeaseAccountCooldownRecoversModelZeroSnapshot(t *testing.T) {
	now := time.Now()
	account := tenantLeaseTestAccount("recovered-model-account", accounts.ProviderClaude)
	model := tenantCredentialLeasePoolModel(account.Provider, "claude-opus-4")
	server := tenantLeaseTestServer(account)
	server.SchedulerRef.Set(selectacct.NewScheduler([]selectacct.Score{{
		AccountID: account.ID, Provider: account.Provider,
		Headroom: 1, ShortHeadroom: 1,
		ModelScores: map[string]selectacct.Score{
			model: {AccountID: account.ID, Provider: account.Provider},
		},
	}}))
	server.SchedulerRef.MarkExhaustedUntil(
		account.Provider, account.ID, "", now.Add(-time.Second),
	)
	_ = server.SchedulerRef.Get() // exercise normal read-time expiry pruning

	if until, blocked := tenantCredentialLeaseTrustedBlockedUntil(server, account, model, now); blocked {
		t.Fatalf("recovered model remained blocked until %v", until)
	}
	published := false
	if _, ok := server.SchedulerRef.RunIfAccountNotBlocked(
		account.Provider, account.ID, model, now, func() { published = true },
	); !ok || !published {
		t.Fatal("account-wide expiry did not allow a model recovery probe")
	}
	if _, ok := server.SchedulerRef.RunIfAccountNotBlocked(
		account.Provider, account.ID, model, now, func() { t.Fatal("second probe published") },
	); ok {
		t.Fatal("account-wide expiry allowed more than one model recovery probe")
	}
}

func TestTenantCredentialLeaseFreshZeroRevokesExpiredRecoveryProbe(t *testing.T) {
	now := time.Now()
	account := tenantLeaseTestAccount("still-exhausted", accounts.ProviderCodex)
	server := tenantLeaseTestServer(account)
	zero := selectacct.Score{
		AccountID: account.ID, Provider: account.Provider,
		Headroom: 0, ShortHeadroom: 0,
	}
	server.SchedulerRef.Set(selectacct.NewScheduler([]selectacct.Score{zero}))
	server.SchedulerRef.MarkExhaustedUntil(
		account.Provider, account.ID, "", now.Add(-time.Second),
	)
	_ = server.SchedulerRef.Get()
	zero.Fresh = true
	zero.ShortResetAfterSeconds = int64(time.Hour / time.Second)
	server.SchedulerRef.Set(selectacct.NewScheduler([]selectacct.Score{zero}))

	if until, blocked := server.SchedulerRef.BlockedUntilFor(
		account.Provider, account.ID, "gpt-5", now,
	); !blocked || until.Before(now.Add(50*time.Minute)) {
		t.Fatalf("fresh zero blocked=%v until=%v, want refreshed cooldown", blocked, until)
	}
	if _, ok := server.SchedulerRef.RunIfAccountNotBlocked(
		account.Provider, account.ID, "gpt-5", now, func() { t.Fatal("fresh zero published") },
	); ok {
		t.Fatal("fresh zero retained an expired recovery probe")
	}
}

func TestTenantCredentialLeaseAllAvoidedReturnsRetryAfter(t *testing.T) {
	now := time.Now()
	accountA := tenantLeaseTestAccount("account-a", accounts.ProviderCodex)
	accountB := tenantLeaseTestAccount("account-b", accounts.ProviderCodex)
	server := tenantLeaseTestServer(accountA, accountB)
	server.RefreshAccountFn = func(_ context.Context, account accounts.Account) (accounts.Account, error) {
		return account, nil
	}
	store := newTenantCredentialLeaseStore()
	input := tenantCredentialLeaseRequest{
		Provider: string(accounts.ProviderCodex), AgentType: "codex",
		SessionID: "session-a", Model: "gpt-5",
	}
	sessionToken, err := store.resolveSessionToken(input, accounts.ProviderCodex, now)
	if err != nil {
		t.Fatal(err)
	}
	for index, account := range []accounts.Account{accountA, accountB} {
		leaseID := fmt.Sprintf("lease-%d", index)
		lease := tenantLeaseTestLease(account, "codex", "session-a", "gpt-5", now)
		lease.sessionToken = sessionToken
		store.put(leaseID, lease, now)
		if _, err := store.consumeReport(leaseID, tenantCredentialLeaseReport{
			Outcome: broker.LeaseRateLimited, StatusCode: http.StatusTooManyRequests,
			RetryAt: now.Add(time.Minute).UnixMilli(),
		}, now); err != nil {
			t.Fatal(err)
		}
	}
	request := httptest.NewRequest(
		http.MethodPost,
		"/_subrouter/leases",
		strings.NewReader(fmt.Sprintf(
			`{"provider":"codex","agentType":"codex","sessionId":"session-a","model":"gpt-5","sessionToken":%q}`,
			sessionToken,
		)),
	)
	response := httptest.NewRecorder()
	store.handleIssue(server, tenant.Tenant{ID: "team"}, response, request)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503: %s", response.Code, response.Body.String())
	}
	if response.Header().Get("Retry-After") == "" {
		t.Fatal("all-avoided response omitted Retry-After")
	}
}

func TestTenantCredentialLeaseRetryAfterIncludesAccountUnavailable(t *testing.T) {
	account := tenantLeaseTestAccount("account-a", accounts.ProviderClaude)
	server := tenantLeaseTestServer(account)
	now := time.Now()
	server.SchedulerRef.MarkAccountUnavailableUntil(
		account.Provider, account.ID, now.Add(2*time.Hour),
	)
	response := httptest.NewRecorder()
	newTenantCredentialLeaseStore().handleIssue(
		server, tenant.Tenant{ID: "team"}, response,
		httptest.NewRequest(http.MethodPost, "/_subrouter/leases", strings.NewReader(
			`{"provider":"claude","agentType":"claude","sessionId":"session-a","model":"claude-opus-4"}`,
		)),
	)
	seconds, err := strconv.Atoi(response.Header().Get("Retry-After"))
	if err != nil {
		t.Fatalf("Retry-After = %q: %v", response.Header().Get("Retry-After"), err)
	}
	if response.Code != http.StatusServiceUnavailable || seconds < 7100 {
		t.Fatalf("status=%d Retry-After=%d body=%s", response.Code, seconds, response.Body.String())
	}
}

func TestTenantCredentialLeaseClaudeAPIKeyFallbackIgnoresMissingOAuthPoolScore(t *testing.T) {
	oauth := tenantLeaseTestAccount("oauth", accounts.ProviderClaude)
	apiKey := tenantLeaseTestAccount("api-key", accounts.ProviderClaude)
	apiKey.AuthMode = accounts.AuthModeAPIKey
	model := tenantCredentialLeasePoolModel(accounts.ProviderClaude, "claude-opus-4")
	server := tenantLeaseTestServer(oauth, apiKey)
	server.SchedulerRef.Set(selectacct.NewScheduler([]selectacct.Score{
		{
			AccountID: oauth.ID, Provider: oauth.Provider, Headroom: 1, ShortHeadroom: 1,
			ModelScores: map[string]selectacct.Score{
				model: {AccountID: oauth.ID, Provider: oauth.Provider},
			},
		},
		{AccountID: apiKey.ID, Provider: apiKey.Provider, Headroom: 1, ShortHeadroom: 1},
	}))

	for _, testCase := range []struct {
		name      string
		configure func(*testing.T, *Server, *tenantCredentialLeaseRequest)
	}{
		{
			name: "preferred oauth",
			configure: func(_ *testing.T, _ *Server, input *tenantCredentialLeaseRequest) {
				input.PreferAccountID = oauth.ID
			},
		},
		{
			name: "sticky oauth",
			configure: func(t *testing.T, server *Server, input *tenantCredentialLeaseRequest) {
				sessions, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
				if err != nil {
					t.Fatal(err)
				}
				if _, err := sessions.Put("claude", input.SessionID, oauth.ID, ""); err != nil {
					t.Fatal(err)
				}
				server.Sessions = sessions
			},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			copyServer := *server
			input := tenantCredentialLeaseRequest{
				Provider: string(accounts.ProviderClaude), AgentType: "claude",
				SessionID: "session-a", Model: "claude-opus-4",
			}
			testCase.configure(t, &copyServer, &input)
			body, err := json.Marshal(input)
			if err != nil {
				t.Fatal(err)
			}
			response := httptest.NewRecorder()
			newTenantCredentialLeaseStore().handleIssue(
				&copyServer, tenant.Tenant{ID: "team"}, response,
				httptest.NewRequest(http.MethodPost, "/_subrouter/leases", strings.NewReader(string(body))),
			)
			if response.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
			var output struct {
				Lease struct {
					AccountID string `json:"accountId"`
				} `json:"lease"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &output); err != nil {
				t.Fatal(err)
			}
			if output.Lease.AccountID != apiKey.ID {
				t.Fatalf("picked=%s, want API-key fallback %s", output.Lease.AccountID, apiKey.ID)
			}
		})
	}
}

func TestTenantCredentialLeaseClaudeAPIKeyFallbackHonorsExplicitUnavailability(t *testing.T) {
	oauth := tenantLeaseTestAccount("oauth", accounts.ProviderClaude)
	apiKey := tenantLeaseTestAccount("api-key", accounts.ProviderClaude)
	apiKey.AuthMode = accounts.AuthModeAPIKey
	model := tenantCredentialLeasePoolModel(accounts.ProviderClaude, "claude-opus-4")
	server := tenantLeaseTestServer(oauth, apiKey)
	server.SchedulerRef.Set(selectacct.NewScheduler([]selectacct.Score{
		{
			AccountID: oauth.ID, Provider: oauth.Provider, Headroom: 1, ShortHeadroom: 1,
			ModelScores: map[string]selectacct.Score{
				model: {AccountID: oauth.ID, Provider: oauth.Provider},
			},
		},
		{AccountID: apiKey.ID, Provider: apiKey.Provider, Headroom: 1, ShortHeadroom: 1},
	}))
	server.SchedulerRef.MarkAccountUnavailableUntil(
		apiKey.Provider, apiKey.ID, time.Now().Add(time.Hour),
	)
	response := httptest.NewRecorder()
	newTenantCredentialLeaseStore().handleIssue(
		server, tenant.Tenant{ID: "team"}, response,
		httptest.NewRequest(http.MethodPost, "/_subrouter/leases", strings.NewReader(
			`{"provider":"claude","agentType":"claude","sessionId":"session-a","model":"claude-opus-4"}`,
		)),
	)
	if response.Code != http.StatusServiceUnavailable || response.Header().Get("Retry-After") == "" {
		t.Fatalf("status=%d retry-after=%q body=%s", response.Code, response.Header().Get("Retry-After"), response.Body.String())
	}
}

func TestTenantCredentialLeaseRetryDeadlineAccumulatesAcrossRefreshAndScheduler(t *testing.T) {
	now := time.Now()
	accountA := tenantLeaseTestAccount("account-a", accounts.ProviderCodex)
	accountB := tenantLeaseTestAccount("account-b", accounts.ProviderCodex)
	server := tenantLeaseTestServer(accountA, accountB)
	server.RefreshAccountFn = func(_ context.Context, account accounts.Account) (accounts.Account, error) {
		return account, nil
	}
	server.SchedulerRef.MarkExhaustedUntil(
		accountB.Provider, accountB.ID, "gpt-5", now.Add(20*time.Minute),
	)
	store := newTenantCredentialLeaseStore()
	lease := tenantLeaseTestLease(accountA, "codex", "session-a", "gpt-5", now)
	store.put("lease-a", lease, now)
	if _, err := store.consumeReport("lease-a", tenantCredentialLeaseReport{
		Outcome: broker.LeaseUnauthorized, StatusCode: http.StatusUnauthorized,
	}, now); err != nil {
		t.Fatal(err)
	}
	_, _, err := selectTenantCredentialLeaseAccount(
		context.Background(), store, server, accounts.ProviderCodex, "",
		tenantCredentialLeaseRequest{
			Provider: string(accounts.ProviderCodex), AgentType: "codex",
			SessionID: "session-a", PreferAccountID: accountA.ID, Model: "gpt-5",
		},
	)
	var allAvoided *tenantCredentialLeaseAllAvoidedError
	if !errors.As(err, &allAvoided) {
		t.Fatalf("error=%v, want all-avoided", err)
	}
	if allAvoided.retryAt.Before(now.Add(4*time.Minute)) ||
		allAvoided.retryAt.After(now.Add(6*time.Minute)) {
		t.Fatalf("retry=%v, want earliest candidate around 5m", allAvoided.retryAt)
	}
}

func TestTenantCredentialLeaseRetryDeadlineIncludesNewRefreshFailureMark(t *testing.T) {
	now := time.Now()
	accountA := tenantLeaseTestAccount("account-a", accounts.ProviderCodex)
	accountB := tenantLeaseTestAccount("account-b", accounts.ProviderCodex)
	server := tenantLeaseTestServer(accountA, accountB)
	server.RefreshAccountFn = func(_ context.Context, account accounts.Account) (accounts.Account, error) {
		if account.ID == accountA.ID {
			return accounts.Account{}, errors.New("temporary refresh failure")
		}
		return account, nil
	}
	server.SchedulerRef.MarkExhaustedUntil(
		accountB.Provider, accountB.ID, "gpt-5", now.Add(20*time.Minute),
	)
	_, _, err := selectTenantCredentialLeaseAccount(
		context.Background(), newTenantCredentialLeaseStore(), server,
		accounts.ProviderCodex, "", tenantCredentialLeaseRequest{
			Provider: string(accounts.ProviderCodex), AgentType: "codex",
			SessionID: "session-a", PreferAccountID: accountA.ID, Model: "gpt-5",
		},
	)
	var allAvoided *tenantCredentialLeaseAllAvoidedError
	if !errors.As(err, &allAvoided) {
		t.Fatalf("error=%v, want all-avoided", err)
	}
	if allAvoided.retryAt.Before(now.Add(9*time.Minute)) ||
		allAvoided.retryAt.After(now.Add(11*time.Minute)) {
		t.Fatalf("retry=%v, want newly marked refresh candidate around 10m", allAvoided.retryAt)
	}
}

func TestTenantCredentialLeaseRechecksSchedulerBeforePublication(t *testing.T) {
	accountA := tenantLeaseTestAccount("account-a", accounts.ProviderCodex)
	accountB := tenantLeaseTestAccount("account-b", accounts.ProviderCodex)
	server := tenantLeaseTestServer(accountA, accountB)
	server.RefreshAccountFn = func(_ context.Context, account accounts.Account) (accounts.Account, error) {
		if account.ID == accountA.ID {
			server.SchedulerRef.MarkExhaustedUntil(
				account.Provider, account.ID, "gpt-5", time.Now().Add(time.Hour),
			)
		}
		return account, nil
	}
	request := httptest.NewRequest(
		http.MethodPost,
		"/_subrouter/leases",
		strings.NewReader(`{"provider":"codex","agentType":"codex","sessionId":"session-a","preferAccountId":"account-a","model":"gpt-5"}`),
	)
	response := httptest.NewRecorder()
	newTenantCredentialLeaseStore().handleIssue(
		server, tenant.Tenant{ID: "team"}, response, request,
	)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", response.Code, response.Body.String())
	}
	var body struct {
		Lease struct {
			AccountID string `json:"accountId"`
		} `json:"lease"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Lease.AccountID != accountB.ID {
		t.Fatalf("published account = %s, want %s after concurrent cooldown", body.Lease.AccountID, accountB.ID)
	}
}

func TestTenantCredentialLeaseIssueRetriesAreBounded(t *testing.T) {
	accountsList := []accounts.Account{
		tenantLeaseTestAccount("account-a", accounts.ProviderCodex),
		tenantLeaseTestAccount("account-b", accounts.ProviderCodex),
		tenantLeaseTestAccount("account-c", accounts.ProviderCodex),
	}
	server := tenantLeaseTestServer(accountsList...)
	refreshes := 0
	server.RefreshAccountFn = func(_ context.Context, account accounts.Account) (accounts.Account, error) {
		refreshes++
		server.SchedulerRef.MarkExhaustedUntil(
			account.Provider, account.ID, "gpt-5", time.Now().Add(time.Hour),
		)
		return account, nil
	}
	response := httptest.NewRecorder()
	newTenantCredentialLeaseStore().handleIssue(
		server, tenant.Tenant{ID: "team"}, response,
		httptest.NewRequest(http.MethodPost, "/_subrouter/leases", strings.NewReader(
			`{"provider":"codex","agentType":"codex","sessionId":"session-a","model":"gpt-5"}`,
		)),
	)
	if response.Code != http.StatusServiceUnavailable || response.Header().Get("Retry-After") == "" {
		t.Fatalf("status=%d retry-after=%q body=%s", response.Code, response.Header().Get("Retry-After"), response.Body.String())
	}
	if refreshes != tenantCredentialLeaseIssueMaxAttempts {
		t.Fatalf("refresh attempts = %d, want bounded %d", refreshes, tenantCredentialLeaseIssueMaxAttempts)
	}
}

func TestTenantCredentialLeaseAvoidanceExpiresAndCredentialRepairRecovers(t *testing.T) {
	now := time.Now()
	old := tenantLeaseTestAccount("account-a", accounts.ProviderCodex)
	repaired := old
	repaired.Token = "repaired-token"
	store := newTenantCredentialLeaseStore()
	store.put("lease-a", tenantLeaseTestLease(old, "codex", "session-a", "gpt-5", now), now)
	if _, err := store.consumeReport("lease-a", tenantCredentialLeaseReport{
		Outcome: broker.LeaseUnauthorized, StatusCode: http.StatusUnauthorized,
	}, now); err != nil {
		t.Fatal(err)
	}
	input := tenantCredentialLeaseRequest{
		Provider: string(accounts.ProviderCodex), AgentType: "codex",
		SessionID: "session-a", Model: "gpt-5", PreferAccountID: old.ID,
	}
	poolModel := tenantCredentialLeasePoolModel(accounts.ProviderCodex, "gpt-5")
	if _, avoided := store.avoidanceUntil(input, old, poolModel, now); !avoided {
		t.Fatal("original credential was not avoided")
	}
	if _, avoided := store.avoidanceUntil(input, repaired, poolModel, now); avoided {
		t.Fatal("credential repair inherited stale unauthorized avoidance")
	}
	server := tenantLeaseTestServer(old, tenantLeaseTestAccount("account-b", accounts.ProviderCodex))
	server.RefreshAccountFn = func(_ context.Context, account accounts.Account) (accounts.Account, error) {
		if account.ID == old.ID {
			return repaired, nil
		}
		return account, nil
	}
	picked, _, err := selectTenantCredentialLeaseAccount(
		context.Background(), store, server, accounts.ProviderCodex, "", input,
	)
	if err != nil {
		t.Fatal(err)
	}
	if picked.ID != repaired.ID || picked.Token != repaired.Token {
		t.Fatalf("refresh-repaired pick = %#v, want repaired credential", picked)
	}

	expiredNow := now.Add(tenantCredentialLeaseReportDefaultCooldown + time.Second)
	if _, avoided := store.avoidanceUntil(input, old, poolModel, expiredNow); avoided {
		t.Fatal("expired avoidance remained active")
	}
	picked, err = pickTenantCredentialLeaseAccount(store, server, server.Accounts, nil, tenantCredentialLeaseRequest{
		Provider: string(accounts.ProviderCodex), AgentType: "codex",
		SessionID: "session-a", PreferAccountID: old.ID, Model: "gpt-5",
	})
	if err != nil {
		t.Fatal(err)
	}
	if picked.ID != old.ID {
		t.Fatalf("post-expiry pick = %s, want restored %s", picked.ID, old.ID)
	}
}

func TestTenantCredentialLeaseTerminalReportIsAtomicAndBounded(t *testing.T) {
	now := time.Now()
	account := tenantLeaseTestAccount("account-a", accounts.ProviderCodex)
	store := newTenantCredentialLeaseStore()
	lease := tenantLeaseTestLease(account, "codex", "session-a", "gpt-5", now)
	store.put("lease-a", lease, now)
	store.put("lease-b", lease, now)

	var wg sync.WaitGroup
	results := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := store.consumeReport("lease-a", tenantCredentialLeaseReport{
				Outcome: broker.LeaseRateLimited, StatusCode: http.StatusTooManyRequests,
				RetryAt: now.Add(30 * 24 * time.Hour).UnixMilli(),
			}, now)
			results <- err
		}()
	}
	wg.Wait()
	close(results)
	succeeded, notFound := 0, 0
	for err := range results {
		switch {
		case err == nil:
			succeeded++
		case errors.Is(err, errTenantCredentialLeaseNotFound):
			notFound++
		default:
			t.Fatalf("unexpected report error: %v", err)
		}
	}
	if succeeded != 1 || notFound != 1 {
		t.Fatalf("report results: succeeded=%d not_found=%d", succeeded, notFound)
	}
	if _, ok := store.get("lease-b", now); ok {
		t.Fatal("terminal report did not invalidate sibling lease")
	}
	store.mu.Lock()
	if len(store.avoidances) != 1 {
		t.Fatalf("avoidance count = %d, want 1", len(store.avoidances))
	}
	for _, avoidance := range store.avoidances {
		if avoidance.expiresAt.After(now.Add(tenantCredentialLeaseReportMaxCooldown)) {
			t.Fatalf("avoidance exceeded maximum: %v", avoidance.expiresAt)
		}
	}
	store.mu.Unlock()

	invalidStore := newTenantCredentialLeaseStore()
	invalidStore.put("lease", lease, now)
	if _, err := invalidStore.consumeReport("lease", tenantCredentialLeaseReport{
		Outcome: broker.LeaseUnauthorized, StatusCode: http.StatusForbidden,
	}, now); err == nil {
		t.Fatal("invalid report was accepted")
	}
	if _, ok := invalidStore.get("lease", now); !ok {
		t.Fatal("invalid report consumed its lease")
	}
	if len(invalidStore.avoidances) != 0 {
		t.Fatal("invalid report inserted avoidance")
	}

	expiredStore := newTenantCredentialLeaseStore()
	expiredLease := lease
	expiredLease.expiresAt = now.Add(-time.Second)
	expiredStore.put("expired", expiredLease, now.Add(-time.Minute))
	if _, err := expiredStore.consumeReport("expired", tenantCredentialLeaseReport{
		Outcome: broker.LeaseUnauthorized, StatusCode: http.StatusUnauthorized,
	}, now); !errors.Is(err, errTenantCredentialLeaseNotFound) {
		t.Fatalf("expired report error = %v", err)
	}
	if len(expiredStore.avoidances) != 0 {
		t.Fatal("expired report inserted avoidance")
	}
}

func TestTenantCredentialLeaseQuotaReportOnlyConsumesSamePoolSiblings(t *testing.T) {
	now := time.Now()
	account := tenantLeaseTestAccount("account-a", accounts.ProviderClaude)
	store := newTenantCredentialLeaseStore()
	opus := tenantLeaseTestLease(account, "claude", "session-a", "claude-opus-4", now)
	opus.sessionToken = "session-token"
	sonnet := tenantLeaseTestLease(account, "claude", "session-a", "claude-sonnet-4", now)
	sonnet.sessionToken = opus.sessionToken
	store.put("opus-a", opus, now)
	store.put("opus-b", opus, now)
	store.put("sonnet", sonnet, now)
	if _, err := store.consumeReport("opus-a", tenantCredentialLeaseReport{
		Outcome: broker.LeaseRateLimited, StatusCode: http.StatusTooManyRequests,
	}, now); err != nil {
		t.Fatal(err)
	}
	if _, ok := store.get("opus-b", now); ok {
		t.Fatal("quota report retained same-pool sibling")
	}
	if _, ok := store.get("sonnet", now); !ok {
		t.Fatal("quota report consumed a different model-pool sibling")
	}
	if _, avoided := store.avoidanceUntil(tenantCredentialLeaseRequest{
		AgentType: "claude", SessionID: "session-a", SessionToken: opus.sessionToken,
	}, account, tenantCredentialLeasePoolModel(accounts.ProviderClaude, "claude-sonnet-4"), now); avoided {
		t.Fatal("model-scoped Claude report blocked a different model pool")
	}
}

func TestTenantCredentialLeaseSessionCapacityEvictsEarliestWithoutRejecting(t *testing.T) {
	now := time.Now()
	store := newTenantCredentialLeaseStore()
	store.mu.Lock()
	for index := 0; index < tenantCredentialLeaseMax; index++ {
		store.sessions[fmt.Sprintf("flood-%d", index)] = tenantCredentialLeaseSession{
			agentType: "codex", sessionID: fmt.Sprintf("flood-%d", index),
			provider:  accounts.ProviderCodex,
			expiresAt: now.Add(time.Duration(index+1) * time.Minute),
		}
	}
	store.mu.Unlock()
	newToken, err := store.resolveSessionToken(tenantCredentialLeaseRequest{
		Provider: string(accounts.ProviderCodex), AgentType: "codex", SessionID: "new",
	}, accounts.ProviderCodex, now)
	if err != nil || newToken == "" {
		t.Fatalf("full table blocked a new capability: token=%q err=%v", newToken, err)
	}
	store.mu.Lock()
	_, retainedOldest := store.sessions["flood-0"]
	count := len(store.sessions)
	store.mu.Unlock()
	if retainedOldest || count != tenantCredentialLeaseMax {
		t.Fatalf("earliest eviction: retained_oldest=%v count=%d", retainedOldest, count)
	}
}

func TestTenantCredentialLeaseFirstTerminalReportPreservesCapability(t *testing.T) {
	now := time.Now()
	store := newTenantCredentialLeaseStore()
	input := tenantCredentialLeaseRequest{
		Provider: string(accounts.ProviderCodex), AgentType: "codex", SessionID: "session-a",
	}
	token, err := store.resolveSessionToken(input, accounts.ProviderCodex, now)
	if err != nil {
		t.Fatal(err)
	}
	input.SessionToken = token
	account := tenantLeaseTestAccount("account-a", accounts.ProviderCodex)
	lease := tenantLeaseTestLease(account, "codex", input.SessionID, "gpt-5", now)
	lease.sessionToken = token
	store.put("lease", lease, now)
	reset := now.Add(time.Hour)
	if _, err := store.consumeReport("lease", tenantCredentialLeaseReport{
		Outcome: broker.LeaseRateLimited, StatusCode: http.StatusTooManyRequests,
		RetryAt: reset.UnixMilli(),
	}, now); err != nil {
		t.Fatal(err)
	}
	afterLeaseExpiry := now.Add(tenantCredentialLeaseTTL + time.Second)
	got, err := store.resolveSessionToken(input, accounts.ProviderCodex, afterLeaseExpiry)
	if err != nil || got != token {
		t.Fatalf("first terminal report lost capability: token=%q err=%v", got, err)
	}
}

func TestTenantCredentialLeaseCapabilityCoversPublishedLeaseExpiry(t *testing.T) {
	now := time.Now()
	store := newTenantCredentialLeaseStore()
	input := tenantCredentialLeaseRequest{
		Provider: string(accounts.ProviderCodex), AgentType: "codex", SessionID: "session-a",
	}
	token, err := store.resolveSessionToken(input, accounts.ProviderCodex, now)
	if err != nil {
		t.Fatal(err)
	}
	account := tenantLeaseTestAccount("account-a", accounts.ProviderCodex)
	lease := tenantLeaseTestLease(account, "codex", input.SessionID, "gpt-5", now)
	lease.sessionToken = token
	lease.expiresAt = now.Add(tenantCredentialLeaseTTL + time.Minute)
	store.put("lease", lease, now)
	store.mu.Lock()
	store.pruneLocked(now.Add(tenantCredentialLeaseTTL + time.Second))
	_, sessionExists := store.sessions[token]
	_, leaseExists := store.leases["lease"]
	store.mu.Unlock()
	if !sessionExists || !leaseExists {
		t.Fatalf("published lease outlived capability: session=%v lease=%v", sessionExists, leaseExists)
	}
}

func TestTenantCredentialLeasePublicationRecreatesEvictedSession(t *testing.T) {
	now := time.Now()
	store := newTenantCredentialLeaseStore()
	account := tenantLeaseTestAccount("account-a", accounts.ProviderCodex)
	input := tenantCredentialLeaseRequest{
		Provider: string(account.Provider), AgentType: "codex", SessionID: "session-a",
	}
	token, err := store.resolveSessionToken(input, account.Provider, now)
	if err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	delete(store.sessions, token)
	store.mu.Unlock()
	lease := tenantLeaseTestLease(account, "codex", "session-a", "gpt-5", now)
	lease.sessionToken = token
	store.put("lease", lease, now)
	store.mu.Lock()
	session, exists := store.sessions[token]
	store.mu.Unlock()
	if !exists || session.expiresAt.Before(lease.expiresAt) {
		t.Fatalf("published capability not recreated: exists=%v expiry=%v", exists, session.expiresAt)
	}
}

func TestTenantCredentialLeaseCapacityNeverRejectsExistingOccupancy(t *testing.T) {
	now := time.Now()
	store := newTenantCredentialLeaseStore()
	account := tenantLeaseTestAccount("account-a", accounts.ProviderCodex)
	reserved := tenantCredentialLeaseSession{
		agentType: "codex", sessionID: "reserved", provider: account.Provider,
		expiresAt: now.Add(time.Minute),
	}
	active := tenantCredentialLeaseSession{
		agentType: "codex", sessionID: "active", provider: account.Provider,
		expiresAt: now.Add(time.Minute),
	}
	avoided := tenantCredentialLeaseSession{
		agentType: "codex", sessionID: "avoided", provider: account.Provider,
		expiresAt: now.Add(time.Minute),
	}
	store.mu.Lock()
	store.sessions["reserved"] = reserved
	store.sessions["active"] = active
	store.sessions["avoided"] = avoided
	store.avoidances[tenantCredentialLeaseAvoidanceKey{
		agentType: "codex", sessionID: "avoided", sessionToken: "avoided",
		provider: account.Provider, accountID: account.ID,
	}] = tenantCredentialLeaseAvoidance{expiresAt: now.Add(time.Minute)}
	activeLease := tenantLeaseTestLease(account, "codex", "active", "gpt-5", now)
	activeLease.sessionToken = "active"
	store.leases["active"] = activeLease
	for len(store.sessions) < tenantCredentialLeaseMax {
		index := len(store.sessions)
		store.sessions[fmt.Sprintf("evictable-%d", index)] = tenantCredentialLeaseSession{
			agentType: "codex", sessionID: fmt.Sprintf("evictable-%d", index),
			provider: account.Provider, expiresAt: now.Add(time.Minute),
		}
	}
	store.mu.Unlock()
	if _, err := store.resolveSessionToken(tenantCredentialLeaseRequest{
		Provider: string(account.Provider), AgentType: "codex", SessionID: "new",
	}, account.Provider, now); err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	count := len(store.sessions)
	store.mu.Unlock()
	if count != tenantCredentialLeaseMax {
		t.Fatalf("session count=%d, want cap=%d", count, tenantCredentialLeaseMax)
	}
}

func TestTenantCredentialLeaseReportAndPublicationAreAtomic(t *testing.T) {
	now := time.Now()
	account := tenantLeaseTestAccount("account-a", accounts.ProviderCodex)
	for iteration := 0; iteration < 100; iteration++ {
		store := newTenantCredentialLeaseStore()
		lease := tenantLeaseTestLease(account, "codex", "session-a", "gpt-5", now)
		store.put("reported", lease, now)

		start := make(chan struct{})
		var wg sync.WaitGroup
		var reportErr error
		var publicationAvoided bool
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			_, reportErr = store.consumeReport("reported", tenantCredentialLeaseReport{
				Outcome: broker.LeaseRateLimited, StatusCode: http.StatusTooManyRequests,
			}, now)
		}()
		go func() {
			defer wg.Done()
			<-start
			_, publicationAvoided = store.putIfEligible("published", lease, now, nil)
		}()
		close(start)
		wg.Wait()

		if reportErr != nil {
			t.Fatalf("iteration %d report error: %v", iteration, reportErr)
		}
		if _, ok := store.get("published", now); ok {
			t.Fatalf("iteration %d left a sibling lease published after rejection", iteration)
		}
		if publicationAvoided {
			if _, ok := store.get("reported", now); ok {
				t.Fatalf("iteration %d retained reported lease after avoidance won", iteration)
			}
		}
	}
}

func TestTenantCredentialLeasePublicationLockOrderDoesNotDeadlock(t *testing.T) {
	account := tenantLeaseTestAccount("account-a", accounts.ProviderCodex)
	server := tenantLeaseTestServer(account)
	store := newTenantCredentialLeaseStore()
	now := time.Now()
	done := make(chan struct{})
	go func() {
		defer close(done)
		var wg sync.WaitGroup
		for iteration := 0; iteration < 100; iteration++ {
			wg.Add(2)
			go func(index int) {
				defer wg.Done()
				lease := tenantLeaseTestLease(account, "codex", fmt.Sprintf("session-%d", index), "gpt-5", now)
				store.putIfEligible(fmt.Sprintf("lease-%d", index), lease, now, server.SchedulerRef)
			}(iteration)
			go func() {
				defer wg.Done()
				server.SchedulerRef.MarkExhaustedUntil(
					account.Provider, account.ID, "gpt-5", now.Add(time.Millisecond),
				)
			}()
		}
		wg.Wait()
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("publication and scheduler updates deadlocked")
	}
}

func TestTenantCredentialLeaseAvoidanceCapacityIsBounded(t *testing.T) {
	store := newTenantCredentialLeaseStore()
	now := time.Now()
	store.mu.Lock()
	for i := 0; i < tenantCredentialLeaseMax; i++ {
		store.avoidances[tenantCredentialLeaseAvoidanceKey{
			agentType: "codex", sessionID: fmt.Sprintf("session-%d", i),
			provider: accounts.ProviderCodex, accountID: "account",
		}] = tenantCredentialLeaseAvoidance{expiresAt: now.Add(time.Duration(i+1) * time.Minute)}
	}
	store.mu.Unlock()
	account := tenantLeaseTestAccount("new-account", accounts.ProviderCodex)
	lease := tenantLeaseTestLease(account, "codex", "new-session", "gpt-5", now)
	lease.sessionToken = "session-new"
	store.mu.Lock()
	store.sessions[lease.sessionToken] = tenantCredentialLeaseSession{
		agentType: "codex", sessionID: lease.sessionID, provider: account.Provider,
		expiresAt: now.Add(time.Hour),
	}
	store.mu.Unlock()
	store.put("lease", lease, now)
	if _, err := store.consumeReport("lease", tenantCredentialLeaseReport{
		Outcome: broker.LeaseUnauthorized, StatusCode: http.StatusUnauthorized,
	}, now); err != nil {
		t.Fatal(err)
	}
	if len(store.avoidances) != tenantCredentialLeaseMax {
		t.Fatalf("avoidance count = %d, want cap %d", len(store.avoidances), tenantCredentialLeaseMax)
	}
	store.mu.Lock()
	_, preserved := store.avoidances[tenantCredentialLeaseAvoidanceKey{
		agentType: "codex", sessionID: "session-0",
		provider: accounts.ProviderCodex, accountID: "account",
	}]
	newKey := tenantCredentialLeaseAvoidanceKey{
		agentType: lease.agentType, sessionID: lease.sessionID,
		sessionToken: lease.sessionToken, provider: lease.provider,
		accountID:  lease.accountID,
		credential: tenantCredentialLeaseCredentialKey(lease.credentialIdentity),
	}
	_, inserted := store.avoidances[newKey]
	store.mu.Unlock()
	if preserved || !inserted {
		t.Fatalf("capacity eviction: oldest_preserved=%v inserted=%v", preserved, inserted)
	}
	input := tenantCredentialLeaseRequest{
		Provider: string(account.Provider), AgentType: "codex",
		SessionID: lease.sessionID, SessionToken: lease.sessionToken, Model: "gpt-5",
	}
	if _, avoided := store.avoidanceUntil(
		input, account, tenantCredentialLeasePoolModel(account.Provider, input.Model), now,
	); !avoided {
		t.Fatal("capacity eviction lost the reporting capability's derived avoidance")
	}
}

func TestTenantCredentialLeaseTenThousandDistinctSessionsRemainAvailableAndBounded(t *testing.T) {
	t.Parallel()
	now := time.Now()
	store := newTenantCredentialLeaseStore()
	account := tenantLeaseTestAccount("account-a", accounts.ProviderCodex)
	for index := 0; index < 10_000; index++ {
		input := tenantCredentialLeaseRequest{
			Provider: string(account.Provider), AgentType: "codex",
			SessionID: fmt.Sprintf("attacker-%d", index),
		}
		token, err := store.resolveSessionToken(input, account.Provider, now.Add(time.Duration(index)*time.Nanosecond))
		if err != nil {
			t.Fatalf("resolve %d: %v", index, err)
		}
		lease := tenantLeaseTestLease(account, "codex", input.SessionID, "gpt-5", now)
		lease.sessionToken = token
		store.put(fmt.Sprintf("lease-%d", index), lease, now)
		if _, err := store.consumeReport(fmt.Sprintf("lease-%d", index), tenantCredentialLeaseReport{
			Outcome: broker.LeaseRateLimited, StatusCode: http.StatusTooManyRequests,
			RetryAt: now.Add(tenantCredentialLeaseReportMaxCooldown).UnixMilli(),
		}, now.Add(time.Duration(index)*time.Nanosecond)); err != nil {
			t.Fatalf("report %d: %v", index, err)
		}
	}

	newToken, err := store.resolveSessionToken(tenantCredentialLeaseRequest{
		Provider: string(account.Provider), AgentType: "codex", SessionID: "victim",
	}, account.Provider, now.Add(time.Second))
	if err != nil || newToken == "" {
		t.Fatalf("avoidance flood denied new capability: token=%q err=%v", newToken, err)
	}
	accountB := tenantLeaseTestAccount("account-b", accounts.ProviderCodex)
	server := tenantLeaseTestServer(account, accountB)
	issue := func() (string, string) {
		response := httptest.NewRecorder()
		store.handleIssue(
			server, tenant.Tenant{ID: "team"}, response,
			httptest.NewRequest(http.MethodPost, "/_subrouter/leases", strings.NewReader(fmt.Sprintf(
				`{"provider":"codex","agentType":"codex","sessionId":"victim","sessionToken":%q,"model":"gpt-5"}`,
				newToken,
			))),
		)
		if response.Code != http.StatusOK {
			t.Fatalf("victim issue status=%d body=%s", response.Code, response.Body.String())
		}
		var body struct {
			Lease struct {
				LeaseID   string `json:"leaseId"`
				AccountID string `json:"accountId"`
			} `json:"lease"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		return body.Lease.LeaseID, body.Lease.AccountID
	}
	firstLease, firstAccount := issue()
	reportResponse := httptest.NewRecorder()
	store.handleReport(server, reportResponse, httptest.NewRequest(
		http.MethodPost, "/_subrouter/leases/"+firstLease+"/events",
		strings.NewReader(`{"outcome":"rate_limited","statusCode":429,"retryAt":9999999999999}`),
	))
	if reportResponse.Code != http.StatusNoContent {
		t.Fatalf("victim report status=%d body=%s", reportResponse.Code, reportResponse.Body.String())
	}
	_, secondAccount := issue()
	if secondAccount == firstAccount {
		t.Fatalf("victim did not fail over under full occupancy: account=%s", firstAccount)
	}
	store.mu.Lock()
	sessionCount := len(store.sessions)
	avoidanceCount := len(store.avoidances)
	store.mu.Unlock()
	if sessionCount != tenantCredentialLeaseMax || avoidanceCount != tenantCredentialLeaseMax {
		t.Fatalf("bounded state: sessions=%d avoidances=%d", sessionCount, avoidanceCount)
	}
}

func TestTenantCredentialLeaseCapacityWithOnlyActiveLeasesStillIssues(t *testing.T) {
	now := time.Now()
	store := newTenantCredentialLeaseStore()
	store.mu.Lock()
	for index := 0; index < tenantCredentialLeaseMax; index++ {
		store.sessions[fmt.Sprintf("active-%d", index)] = tenantCredentialLeaseSession{
			agentType: "codex", sessionID: fmt.Sprintf("active-%d", index),
			provider: accounts.ProviderCodex, expiresAt: now.Add(time.Minute),
		}
	}
	store.mu.Unlock()
	token, err := store.resolveSessionToken(tenantCredentialLeaseRequest{
		Provider: string(accounts.ProviderCodex), AgentType: "codex", SessionID: "new",
	}, accounts.ProviderCodex, now)
	if err != nil || token == "" {
		t.Fatalf("active occupancy blocked issue: token=%q err=%v", token, err)
	}
	store.mu.Lock()
	count := len(store.sessions)
	store.mu.Unlock()
	if count != tenantCredentialLeaseMax {
		t.Fatalf("session count=%d, want cap=%d", count, tenantCredentialLeaseMax)
	}
}

func TestTenantCredentialLeaseConcurrentCapacityFloodDoesNotBlockVictimOrScheduler(t *testing.T) {
	t.Parallel()
	now := time.Now()
	accountA := tenantLeaseTestAccount("account-a", accounts.ProviderCodex)
	accountB := tenantLeaseTestAccount("account-b", accounts.ProviderCodex)
	server := tenantLeaseTestServer(accountA, accountB)
	store := newTenantCredentialLeaseStore()
	start := make(chan struct{})
	errs := make(chan error, 16)
	var wg sync.WaitGroup
	for worker := 0; worker < 4; worker++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			<-start
			for index := 0; index < 1500; index++ {
				sessionID := fmt.Sprintf("flood-%d-%d", worker, index)
				token, err := store.resolveSessionToken(tenantCredentialLeaseRequest{
					Provider: string(accountA.Provider), AgentType: "codex", SessionID: sessionID,
				}, accountA.Provider, now)
				if err != nil {
					errs <- err
					return
				}
				leaseID := fmt.Sprintf("flood-lease-%d-%d", worker, index)
				lease := tenantLeaseTestLease(accountA, "codex", sessionID, "gpt-5", now)
				lease.sessionToken = token
				store.put(leaseID, lease, now)
				if _, err := store.consumeReport(leaseID, tenantCredentialLeaseReport{
					Outcome: broker.LeaseRateLimited, StatusCode: http.StatusTooManyRequests,
				}, now); err != nil {
					errs <- err
					return
				}
			}
		}(worker)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		for index := 0; index < 200; index++ {
			server.SchedulerRef.MarkExhaustedUntil(
				accountA.Provider, accountA.ID, "gpt-5", time.Now().Add(time.Minute),
			)
		}
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		for index := 0; index < 100; index++ {
			response := httptest.NewRecorder()
			store.handleIssue(
				server, tenant.Tenant{ID: "team"}, response,
				httptest.NewRequest(http.MethodPost, "/_subrouter/leases", strings.NewReader(fmt.Sprintf(
					`{"provider":"codex","agentType":"codex","sessionId":"victim-%d","model":"gpt-5"}`,
					index,
				))),
			)
			if response.Code != http.StatusOK {
				errs <- fmt.Errorf("victim issue %d: status=%d body=%s", index, response.Code, response.Body.String())
				return
			}
		}
	}()
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.sessions) > tenantCredentialLeaseMax ||
		len(store.leases) > tenantCredentialLeaseMax ||
		len(store.avoidances) > tenantCredentialLeaseMax {
		t.Fatalf("unbounded state: sessions=%d leases=%d avoidances=%d", len(store.sessions), len(store.leases), len(store.avoidances))
	}
}

func TestQwenAnthropicLeaseSelectionUsesSharedTokenPlanScores(t *testing.T) {
	const model = "qwen3.7-plus"
	available := []accounts.Account{
		{ID: "qwen-token:a-cooked", Provider: accounts.ProviderQwenAnthropic, AuthMode: accounts.AuthModeAPIKey, Token: "cooked"},
		{ID: "qwen-token:z-healthy", Provider: accounts.ProviderQwenAnthropic, AuthMode: accounts.AuthModeAPIKey, Token: "healthy"},
	}
	scheduler := selectacct.NewScheduler([]selectacct.Score{
		{AccountID: "qwen-token:a-cooked", Provider: accounts.ProviderQwenToken, Headroom: 1, ShortHeadroom: 1},
		{AccountID: "qwen-token:z-healthy", Provider: accounts.ProviderQwenToken, Headroom: 1, ShortHeadroom: 1},
	})
	schedulerRef := selectacct.NewSchedulerRef(scheduler)
	schedulerRef.MarkExhaustedUntil(accounts.ProviderQwenToken, "qwen-token:a-cooked", model, time.Now().Add(time.Hour))
	server := &Server{SchedulerRef: schedulerRef}

	for _, testCase := range []struct {
		name      string
		configure func(*testing.T, *Server, *tenantCredentialLeaseRequest)
	}{
		{name: "scheduler"},
		{
			name: "preferred cooked account",
			configure: func(_ *testing.T, _ *Server, input *tenantCredentialLeaseRequest) {
				input.PreferAccountID = "qwen-token:a-cooked"
			},
		},
		{
			name: "sticky cooked account",
			configure: func(t *testing.T, server *Server, input *tenantCredentialLeaseRequest) {
				sessions, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
				if err != nil {
					t.Fatal(err)
				}
				if _, err := sessions.Put(string(accounts.ProviderQwenAnthropic), input.SessionID, "qwen-token:a-cooked", ""); err != nil {
					t.Fatal(err)
				}
				server.Sessions = sessions
			},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			copyServer := *server
			input := tenantCredentialLeaseRequest{
				Provider:  string(accounts.ProviderQwenAnthropic),
				SessionID: "session-a", Model: model,
			}
			if testCase.configure != nil {
				testCase.configure(t, &copyServer, &input)
			}
			picked, err := pickTenantCredentialLeaseAccount(nil, &copyServer, available, nil, input)
			if err != nil {
				t.Fatal(err)
			}
			if picked.ID != "qwen-token:z-healthy" {
				t.Fatalf("picked %q, want healthy shared Token Plan account", picked.ID)
			}
		})
	}
}

func TestQwenAnthropicLeasePublicationHonorsSharedTokenPlanCooldown(t *testing.T) {
	const model = "qwen3.7-plus"
	now := time.Now()
	account := accounts.Account{
		ID: "qwen-token:cooked", Provider: accounts.ProviderQwenAnthropic,
		AuthMode: accounts.AuthModeAPIKey, Token: "key",
	}
	schedulerRef := selectacct.NewSchedulerRef(selectacct.NewScheduler([]selectacct.Score{{
		AccountID: account.ID, Provider: accounts.ProviderQwenToken,
		Headroom: 1, ShortHeadroom: 1,
	}}))
	schedulerRef.MarkExhaustedUntil(accounts.ProviderQwenToken, account.ID, model, now.Add(time.Hour))
	store := newTenantCredentialLeaseStore()
	lease := tenantLeaseTestLease(account, string(accounts.ProviderQwenAnthropic), "session-a", model, now)
	lease.authMode = accounts.AuthModeAPIKey
	lease.model = model

	until, avoided := store.putIfEligible("lease", lease, now, schedulerRef)
	if !avoided || until.Before(now.Add(50*time.Minute)) {
		t.Fatalf("publication guard: avoided=%v until=%v", avoided, until)
	}
	if _, ok := store.get("lease", now); ok {
		t.Fatal("published a lease for a shared Token Plan account under cooldown")
	}
}

func TestQwenLeaseAvoidanceUsesSharedTokenPlanIdentityAcrossProtocols(t *testing.T) {
	now := time.Now()
	store := newTenantCredentialLeaseStore()
	input := tenantCredentialLeaseRequest{
		Provider:  string(accounts.ProviderQwenAnthropic),
		SessionID: "session-a",
		Model:     "qwen3.7-plus",
	}
	token, err := store.resolveSessionToken(input, accounts.ProviderQwenAnthropic, now)
	if err != nil {
		t.Fatal(err)
	}
	input.SessionToken = token
	account := accounts.Account{
		ID: "qwen-token:shared", Provider: accounts.ProviderQwenToken,
		AuthMode: accounts.AuthModeAPIKey, Token: "key",
	}
	lease := tenantLeaseTestLease(account, tenantCredentialLeaseAgentType(input, accounts.ProviderQwenAnthropic), input.SessionID, input.Model, now)
	// Model the requested transport alias exactly as the issue path receives it;
	// internal quota identity must canonicalize it before report and lookup.
	lease.provider = accounts.ProviderQwenAnthropic
	lease.sessionToken = token
	store.put("lease", lease, now)
	if _, err := store.consumeReport("lease", tenantCredentialLeaseReport{
		Outcome: broker.LeaseRateLimited, StatusCode: http.StatusTooManyRequests,
		Scope: broker.LeaseCooldownQuota,
	}, now); err != nil {
		t.Fatal(err)
	}
	pool := tenantCredentialLeasePoolModel(accounts.ProviderQwenToken, input.Model)
	if _, avoided := store.avoidanceUntil(input, account, pool, now); !avoided {
		t.Fatal("Qwen Anthropic report did not cool the shared Token Plan account")
	}

	canonical := input
	canonical.Provider = string(accounts.ProviderQwenToken)
	canonicalToken, err := store.resolveSessionToken(canonical, accounts.ProviderQwenToken, now)
	if err != nil {
		t.Fatal(err)
	}
	if canonicalToken != token {
		t.Fatal("Qwen protocol alias did not retain the session capability")
	}
	canonical.SessionToken = canonicalToken
	if _, avoided := store.avoidanceUntil(canonical, account, pool, now); !avoided {
		t.Fatal("Qwen Token report lookup did not share the alias cooldown")
	}
	other := account
	other.ID = "qwen-token:other"
	if _, avoided := store.avoidanceUntil(canonical, other, pool, now); avoided {
		t.Fatal("shared-provider cooldown leaked to another account")
	}
}

func TestTenantCredentialLeaseSessionCountsAreProviderScoped(t *testing.T) {
	const sharedID = "shared@example.com"
	shared := accounts.Account{
		ID: sharedID, Provider: accounts.ProviderCodex,
		AuthMode: accounts.AuthModeAPIKey, Token: "shared-key",
	}
	other := accounts.Account{
		ID: "z-other@example.com", Provider: accounts.ProviderCodex,
		AuthMode: accounts.AuthModeAPIKey, Token: "other-key",
	}
	server := tenantLeaseTestServer(shared, other)
	sessions, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sessions.Put("claude", "claude-session", sharedID, ""); err != nil {
		t.Fatal(err)
	}
	server.Sessions = sessions

	picked, err := pickTenantCredentialLeaseAccount(nil, server, []accounts.Account{shared, other}, nil, tenantCredentialLeaseRequest{
		Provider: string(accounts.ProviderCodex), AgentType: "codex",
		SessionID: "new-codex-session", Model: "gpt-5",
	})
	if err != nil {
		t.Fatal(err)
	}
	if picked.ID != sharedID {
		t.Fatalf("Claude session count damped same-ID Codex account: picked %q, want %q", picked.ID, sharedID)
	}
}
