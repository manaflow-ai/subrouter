package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/selectacct"
	"github.com/manaflow-ai/subrouter/session"
)

// placementCountersFor returns one account's counters from the ref, zeros
// when nothing was recorded.
func placementCountersFor(ref *selectacct.SchedulerRef, provider accounts.Provider, accountID string) selectacct.AccountPlacementStats {
	for _, acct := range ref.PlacementStats(nil, nil).Accounts {
		if acct.Provider == provider && acct.AccountID == accountID {
			return acct
		}
	}
	return selectacct.AccountPlacementStats{Provider: provider, AccountID: accountID}
}

func placementTestServer(t *testing.T) Server {
	t.Helper()
	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	return Server{
		Accounts: []accounts.Account{
			{ID: "roomy@example.com", Provider: accounts.ProviderCodex, AuthMode: accounts.AuthModeOAuth, Token: "tok-roomy"},
			{ID: "idle@example.com", Provider: accounts.ProviderCodex, AuthMode: accounts.AuthModeOAuth, Token: "tok-idle"},
		},
		Sessions: store,
		SchedulerRef: selectacct.NewSchedulerRef(selectacct.NewScheduler([]selectacct.Score{
			// Only roomy is usable for a new session, so Pick is deterministic.
			{AccountID: "roomy@example.com", Provider: accounts.ProviderCodex, Headroom: 0.9, ShortHeadroom: 0.9},
			{AccountID: "idle@example.com", Provider: accounts.ProviderCodex, Headroom: 0.2, ShortHeadroom: 0.2},
		})),
		MaxBodyBytes: 1024,
		AdminToken:   "admin-secret",
		Lifecycle:    NewLifecycle(),
	}
}

func placementSelect(t *testing.T, server Server, sessionID string) accounts.Account {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, "https://subrouter.test/v1/responses", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	account, _, _, err := server.accountForSessionProvider(accounts.ProviderCodex, "codex", sessionID, req)
	if err != nil {
		t.Fatal(err)
	}
	return account
}

// Only a session with no assignment counts as a placement; its later turns
// reuse the sticky account and do not.
func TestNewSessionPlacementIsCountedOnce(t *testing.T) {
	server := placementTestServer(t)
	for i := 0; i < 3; i++ {
		if got := placementSelect(t, server, "session-1"); got.ID != "roomy@example.com" {
			t.Fatalf("turn %d account = %q, want roomy", i, got.ID)
		}
	}
	placementSelect(t, server, "session-2")
	if got := placementCountersFor(server.SchedulerRef, accounts.ProviderCodex, "roomy@example.com"); got.Placements != 2 {
		t.Fatalf("roomy placements = %d, want 2 (two new sessions, repeat turns not counted)", got.Placements)
	}
	snapshot := server.SchedulerRef.PlacementStats(SchedulerSessionCounts(server.Sessions), SchedulerAccounts(server.Accounts))
	if len(snapshot.Pools) != 1 {
		t.Fatalf("pools = %+v, want the one Codex pool", snapshot.Pools)
	}
	pool := snapshot.Pools[0]
	if pool.Provider != accounts.ProviderCodex || pool.Pool != "" || pool.Placements != 2 ||
		pool.BusiestAccountID != "roomy@example.com" || pool.BusiestShare != 1 {
		t.Fatalf("pool = %+v, want every placement on roomy (share 1)", pool)
	}
	var idle, roomy *selectacct.AccountPlacementStats
	for i := range snapshot.Accounts {
		switch snapshot.Accounts[i].AccountID {
		case "idle@example.com":
			idle = &snapshot.Accounts[i]
		case "roomy@example.com":
			roomy = &snapshot.Accounts[i]
		}
	}
	if idle == nil || idle.Placements != 0 {
		t.Fatalf("idle account missing or counted: %+v", idle)
	}
	if roomy == nil || roomy.Sessions != 2 {
		t.Fatalf("roomy sessions = %+v, want 2 current sessions", roomy)
	}
}

func TestPlacementStatsEndpointsServeAdmins(t *testing.T) {
	server := placementTestServer(t)
	placementSelect(t, server, "session-1")
	server.SchedulerRef.NoteRouted(accounts.ProviderCodex, "roomy@example.com")
	server.SchedulerRef.NoteFailover(accounts.ProviderCodex, "idle@example.com", selectacct.FailoverUsageLimit)
	handler := server.Handler()

	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, loopbackAdminRequest(http.MethodGet, PlacementStatsPath, "127.0.0.1:31415", nil))
	if resp.Code != http.StatusOK {
		t.Fatalf("placement-stats status = %d, body = %s", resp.Code, resp.Body.String())
	}
	var snapshot selectacct.PlacementStatsSnapshot
	if err := json.Unmarshal(resp.Body.Bytes(), &snapshot); err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Accounts) != 2 || len(snapshot.Pools) != 1 {
		t.Fatalf("snapshot = %+v", snapshot)
	}
	if strings.Contains(resp.Body.String(), "tok-") {
		t.Fatalf("placement stats leaked a credential: %s", resp.Body.String())
	}

	resp = httptest.NewRecorder()
	handler.ServeHTTP(resp, loopbackAdminRequest(http.MethodGet, MetricsPath, "localhost:31415", nil))
	if resp.Code != http.StatusOK {
		t.Fatalf("metrics status = %d, body = %s", resp.Code, resp.Body.String())
	}
	if ct := resp.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Fatalf("metrics content type = %q", ct)
	}
	for _, want := range []string{
		`subrouter_placements_total{provider="codex",account="roomy@example.com"} 1`,
		`subrouter_routed_requests_total{provider="codex",account="roomy@example.com"} 1`,
		`subrouter_sessions{provider="codex",account="roomy@example.com"} 1`,
		`subrouter_failovers_total{provider="codex",account="idle@example.com",reason="usage_limit"} 1`,
		`subrouter_pool_busiest_share_1h{provider="codex",pool=""} 1`,
	} {
		if !strings.Contains(resp.Body.String(), want) {
			t.Fatalf("metrics missing %q:\n%s", want, resp.Body.String())
		}
	}
}

// The new endpoints sit behind the same origin/host checks as their admin
// neighbours.
func TestPlacementStatsEndpointsRejectNonAdminOrigins(t *testing.T) {
	handler := placementTestServer(t).Handler()
	for _, path := range []string{PlacementStatsPath, MetricsPath} {
		for _, test := range []struct {
			name    string
			host    string
			headers map[string]string
			want    int
		}{
			{"cross-site origin", "127.0.0.1:31415", map[string]string{"Origin": "https://evil.example"}, http.StatusForbidden},
			{"sec-fetch-site cross-site", "localhost:31415", map[string]string{"Sec-Fetch-Site": "cross-site"}, http.StatusForbidden},
			{"non-loopback host", "rebind.evil.example:31415", nil, http.StatusForbidden},
		} {
			t.Run(path+" "+test.name, func(t *testing.T) {
				resp := httptest.NewRecorder()
				handler.ServeHTTP(resp, loopbackAdminRequest(http.MethodGet, path, test.host, test.headers))
				if resp.Code != test.want {
					t.Fatalf("status = %d, want %d; body = %s", resp.Code, test.want, resp.Body.String())
				}
			})
		}
		t.Run(path+" remote without token", func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, path, nil)
			req.RemoteAddr = "203.0.113.9:4444"
			resp := httptest.NewRecorder()
			handler.ServeHTTP(resp, req)
			if resp.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401; body = %s", resp.Code, resp.Body.String())
			}
		})
	}
}

// The Codex overload transport's account switch counts as a capacity
// failover on the account it left (same pool fixture as
// TestCodexOverloadFailoverSwitchesAccountAndSticks, with a live ref).
func TestCodexOverloadSwitchCountsCapacityFailover(t *testing.T) {
	pool, seen := codexOverloadPool(t, "oauth-token-0")
	poolURL, _ := url.Parse(pool.URL)
	server := codexOverloadServer(t, poolURL, 2, true)
	server.SchedulerRef = selectacct.NewSchedulerRef(server.Scheduler)
	if _, err := server.Sessions.Put("codex", "session-a", "codex-account-0", ""); err != nil {
		t.Fatal(err)
	}
	proxy := httptest.NewServer(server.Handler())
	defer proxy.Close()
	status, body := codexEgressPost(t, proxy.URL, "session-a")
	if status != http.StatusOK || !strings.Contains(body, "served-from-oauth-token-1") {
		t.Fatalf("status=%d body=%s seen=%v", status, body, seen())
	}
	overloaded := placementCountersFor(server.SchedulerRef, accounts.ProviderCodex, "codex-account-0")
	if overloaded.Failovers[selectacct.FailoverCapacity] != 1 || overloaded.FailoverTotal() != 1 {
		t.Fatalf("account 0 failovers = %v, want exactly one capacity", overloaded.Failovers)
	}
	if overloaded.CapacityMarks != 2 || overloaded.Placements != 0 {
		t.Fatalf("account 0 = %+v, want 2 capacity marks and no placement", overloaded)
	}
	if served := placementCountersFor(server.SchedulerRef, accounts.ProviderCodex, "codex-account-1"); served.Routed == 0 {
		t.Fatalf("account 1 = %+v, want the replayed request routed there", served)
	}
}
