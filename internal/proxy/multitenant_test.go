package proxy

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	agentantigravity "github.com/manaflow-ai/subrouter/internal/agents/antigravity"
	agentclaude "github.com/manaflow-ai/subrouter/internal/agents/claude"
	agentkimi "github.com/manaflow-ai/subrouter/internal/agents/kimi"
	agentqwen "github.com/manaflow-ai/subrouter/internal/agents/qwen"
	"github.com/manaflow-ai/subrouter/internal/stackauth"
	"github.com/manaflow-ai/subrouter/internal/tenant"
	"github.com/manaflow-ai/subrouter/selectacct"
	"github.com/manaflow-ai/subrouter/session"
)

type fakeStackVerifier struct {
	claims stackauth.Claims
	err    error
}

func TestTenantFallbackScoresCanonicalizeQwenEndpointAliases(t *testing.T) {
	available := []accounts.Account{
		{ID: "qwen-token:direct", Provider: accounts.ProviderQwenToken, AuthMode: accounts.AuthModeAPIKey},
		{ID: "qwen-token:anthropic", Provider: accounts.ProviderQwenAnthropic, AuthMode: accounts.AuthModeAPIKey},
	}
	scores := tenantFallbackScores(available)
	if len(scores) != len(available) {
		t.Fatalf("fallback score count = %d, want %d", len(scores), len(available))
	}
	for _, score := range scores {
		if score.Provider != accounts.ProviderQwenToken {
			t.Fatalf("fallback score %q provider = %q, want shared Token Plan provider %q", score.AccountID, score.Provider, accounts.ProviderQwenToken)
		}
		if score.Headroom != 0.01 || score.ShortHeadroom != 0.01 {
			t.Fatalf("fallback score %q headroom = %v/%v, want API-key fallback", score.AccountID, score.Headroom, score.ShortHeadroom)
		}
	}
}

func (f fakeStackVerifier) Verify(context.Context, string) (stackauth.Claims, error) {
	return f.claims, f.err
}

type fakeStackTeams struct {
	teams []stackauth.Team
	err   error
}

type signalLogWriter struct {
	match string
	seen  chan struct{}
}

func (w signalLogWriter) Write(body []byte) (int, error) {
	if strings.Contains(string(body), w.match) {
		select {
		case w.seen <- struct{}{}:
		default:
		}
	}
	return len(body), nil
}

const testStackTenantDeleteToken = "0123456789abcdef0123456789abcdef-delete"

func (f fakeStackTeams) ListTeams(context.Context, string) ([]stackauth.Team, error) {
	return f.teams, f.err
}

// newMultiTenantFixture builds an upstream that echoes the Authorization
// header it received, a legacy single-tenant base Server with one account, and
// a MultiTenant wrapper over a fresh registry.
func newMultiTenantFixture(t *testing.T) (*tenant.Registry, http.Handler, func() string) {
	return newMultiTenantFixtureWithAccountImportToken(t, "")
}

func newMultiTenantFixtureWithAccountImportToken(
	t *testing.T,
	accountImportToken string,
) (*tenant.Registry, http.Handler, func() string) {
	return newMultiTenantFixtureWithTransport(t, accountImportToken, nil)
}

func newMultiTenantFixtureWithTransport(
	t *testing.T,
	accountImportToken string,
	transport http.RoundTripper,
) (*tenant.Registry, http.Handler, func() string) {
	t.Helper()
	var lastAuth string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lastAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(lastAuth))
	}))
	t.Cleanup(upstream.Close)
	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	legacySessions, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	base := Server{
		Upstream: upstreamURL,
		Accounts: []accounts.Account{{
			ID:       "legacy@example.com",
			AuthMode: accounts.AuthModeOAuth,
			Token:    "legacy-token",
		}},
		Sessions:           legacySessions,
		Scheduler:          selectacct.NewScheduler(nil),
		MaxBodyBytes:       1024,
		AccountImportToken: accountImportToken,
		Transport:          transport,
	}
	registry := tenant.NewRegistry(t.TempDir())
	multi := &MultiTenant{Base: base, Registry: registry}
	return registry, multi.Handler(base.Handler()), func() string { return lastAuth }
}

func TestMultiTenantUsageStatusNeedsOnlyTheTenantKey(t *testing.T) {
	registry, handler, _ := newMultiTenantFixtureWithAccountImportToken(
		t,
		"legacy-import-token",
	)
	_, key, err := registry.Create("usage")
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(
		http.MethodGet,
		"/t/"+key+"/_subrouter/usage-status",
		nil,
	)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("usage status = %d, body = %s", response.Code, response.Body.String())
	}
	var statuses []AccountUsageStatus
	if err := json.Unmarshal(response.Body.Bytes(), &statuses); err != nil {
		t.Fatalf("decode usage status: %v", err)
	}
}

func TestTenantServerScopesKimiProfilesToTenantState(t *testing.T) {
	registry := tenant.NewRegistry(t.TempDir())
	created, _, err := registry.Create("kimi-isolated")
	if err != nil {
		t.Fatal(err)
	}
	multi := &MultiTenant{Base: Server{}, Registry: registry}
	server, err := multi.newTenantServer(t.Context(), created)
	if err != nil {
		t.Fatal(err)
	}
	store := server.AccountRef.kimiStore()
	wantDir := filepath.Join(registry.Dir(created.ID), "kimi")
	if store.ManagedDir != wantDir || filepath.Dir(store.Path) != wantDir {
		t.Fatalf("tenant Kimi store = path %q managed %q, want root %q", store.Path, store.ManagedDir, wantDir)
	}
	if _, err := store.RefreshAccount(t.Context(), http.DefaultClient, accounts.Account{
		ID: "kimi-code", Provider: accounts.ProviderKimi, AuthMode: accounts.AuthModeOAuth,
	}); err == nil || !strings.Contains(err.Error(), "not routable") {
		t.Fatalf("tenant singleton Kimi refresh error = %v", err)
	}
	installed, err := store.SaveManagedCredential("work", agentkimi.CredentialInfo{
		AccessToken: "access", RefreshToken: "refresh", ExpiresAt: time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	if installed.ID != "kimi-subscription:work" {
		t.Fatalf("installed account = %+v", installed)
	}
	entries, err := os.ReadDir(wantDir)
	if err != nil || len(entries) == 0 {
		t.Fatalf("tenant Kimi credential was not stored under tenant state: entries=%v err=%v", entries, err)
	}
}

func TestTenantServerScopesAntigravityProfilesToTenantState(t *testing.T) {
	registry := tenant.NewRegistry(t.TempDir())
	created, _, err := registry.Create("antigravity-isolated")
	if err != nil {
		t.Fatal(err)
	}
	server, err := (&MultiTenant{Base: Server{}, Registry: registry}).newTenantServer(t.Context(), created)
	if err != nil {
		t.Fatal(err)
	}
	store, ok := server.AccountRef.antigravityStore()
	if !ok {
		t.Fatal("tenant Antigravity store is not configured")
	}
	wantDir := filepath.Join(registry.Dir(created.ID), "antigravity")
	installed, err := store.SaveManagedCredential("work", agentantigravity.CredentialInfo{
		AccessToken: "access", RefreshToken: "refresh", ExpiresAt: time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	if installed.ID != "antigravity-subscription:work" || store.ManagedDir != wantDir {
		t.Fatalf("tenant Antigravity account=%+v dir=%q want=%q", installed, store.ManagedDir, wantDir)
	}
	entries, err := os.ReadDir(wantDir)
	ids, inventoryErr := store.AccountInventoryIDs(t.Context())
	if err != nil || inventoryErr != nil || len(ids) != 1 || ids[0] != installed.ID {
		t.Fatalf("tenant Antigravity state entries=%v err=%v", entries, err)
	}
}

func TestMultiTenantServingStoreDoesNotSyncInteractiveCodexAuth(t *testing.T) {
	registry := tenant.NewRegistry(t.TempDir())
	created, _, err := registry.Create("read-only-serving-store")
	if err != nil {
		t.Fatal(err)
	}
	multi := &MultiTenant{Base: Server{}, Registry: registry}
	server, err := multi.newTenantServer(context.Background(), created)
	if err != nil {
		t.Fatal(err)
	}
	if !server.AccountRef.store.DisableActiveAuthSync {
		t.Fatal("tenant serving store can rewrite interactive Codex auth")
	}
	if !server.AccountRef.store.RequireIsolatedOAuth {
		t.Fatal("tenant serving store accepts OAuth credentials without isolated provenance")
	}
}

func writeTenantAPIKeyAccount(t *testing.T, registry *tenant.Registry, tenantID, email, apiKey string) {
	t.Helper()
	dir := filepath.Join(registry.Dir(tenantID), "codex", "accounts")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf(`{"email":%q,"addedAt":"2026-01-01T00:00:00Z","auth":{"auth_mode":"apikey","OPENAI_API_KEY":%q}}`, email, apiKey)
	name := strings.NewReplacer("@", "_at_", ":", "_").Replace(email)
	if err := os.WriteFile(filepath.Join(dir, name+".json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func doProxyRequest(t *testing.T, handler http.Handler, path, sessionID string, mutate ...func(*http.Request)) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{}`))
	if sessionID != "" {
		req.Header.Set("X-Subrouter-Session", sessionID)
	}
	for _, fn := range mutate {
		fn(req)
	}
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, req)
	return resp
}

func TestMultiTenantIsolatesAccountPools(t *testing.T) {
	registry, handler, _ := newMultiTenantFixture(t)
	alpha, keyA, err := registry.Create("alpha")
	if err != nil {
		t.Fatal(err)
	}
	beta, keyB, err := registry.Create("beta")
	if err != nil {
		t.Fatal(err)
	}
	writeTenantAPIKeyAccount(t, registry, alpha.ID, "apikey:a", "sk-tenant-a")
	writeTenantAPIKeyAccount(t, registry, beta.ID, "apikey:b", "sk-tenant-b")

	for i := 0; i < 3; i++ {
		resp := doProxyRequest(t, handler, "/t/"+keyA+"/v1/responses", fmt.Sprintf("sess-a-%d", i))
		if resp.Code != http.StatusOK {
			t.Fatalf("tenant A status = %d, body = %s", resp.Code, resp.Body.String())
		}
		if got := resp.Body.String(); got != "Bearer sk-tenant-a" {
			t.Fatalf("tenant A routed with %q", got)
		}
	}
	resp := doProxyRequest(t, handler, "/t/"+keyB+"/v1/responses", "sess-b")
	if got := resp.Body.String(); got != "Bearer sk-tenant-b" {
		t.Fatalf("tenant B routed with %q", got)
	}

	// Header-borne key (how Claude Code sends ANTHROPIC_AUTH_TOKEN) routes to
	// the same tenant pool without a path prefix.
	resp = doProxyRequest(t, handler, "/v1/responses", "sess-a-header", func(r *http.Request) {
		r.Header.Set("Authorization", "Bearer "+keyA)
	})
	if got := resp.Body.String(); got != "Bearer sk-tenant-a" {
		t.Fatalf("header-key routing used %q", got)
	}
	resp = doProxyRequest(t, handler, "/v1/responses", "sess-b-header", func(r *http.Request) {
		r.Header.Set("X-Api-Key", keyB)
	})
	if got := resp.Body.String(); got != "Bearer sk-tenant-b" {
		t.Fatalf("x-api-key routing used %q", got)
	}

	// Legacy paths without a tenant key keep the single-tenant pool.
	resp = doProxyRequest(t, handler, "/v1/responses", "sess-legacy")
	if got := resp.Body.String(); got != "Bearer legacy-token" {
		t.Fatalf("legacy routing used %q", got)
	}
}

func TestMultiTenantRejectsUnknownKeys(t *testing.T) {
	registry, handler, _ := newMultiTenantFixture(t)
	if _, _, err := registry.Create("alpha"); err != nil {
		t.Fatal(err)
	}

	resp := doProxyRequest(t, handler, "/t/srt_00000000000000000000000000000000/v1/responses", "s")
	if resp.Code != http.StatusUnauthorized {
		t.Fatalf("unknown path key status = %d", resp.Code)
	}
	resp = doProxyRequest(t, handler, "/t/not-a-key/v1/responses", "s")
	if resp.Code != http.StatusUnauthorized {
		t.Fatalf("malformed path key status = %d", resp.Code)
	}
	resp = doProxyRequest(t, handler, "/v1/responses", "s", func(r *http.Request) {
		r.Header.Set("Authorization", "Bearer srt_00000000000000000000000000000000")
	})
	if resp.Code != http.StatusUnauthorized {
		t.Fatalf("unknown header key status = %d", resp.Code)
	}
}

func TestMultiTenantRejectsADeletedFinalTenantKey(t *testing.T) {
	registry, handler, _ := newMultiTenantFixture(t)
	created, key, err := registry.Create("only-tenant")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := registry.RetireExternal(created.ID); err != nil {
		t.Fatal(err)
	}
	lock, acquired, err := registry.TryAcquireExclusiveUse(created.ID)
	if err != nil || !acquired {
		t.Fatalf("exclusive use lock = %v, %v", acquired, err)
	}
	if _, err := registry.DeleteRetired(created.ID); err != nil {
		lock.Close()
		t.Fatal(err)
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}

	response := doProxyRequest(t, handler, "/v1/responses", "deleted", func(r *http.Request) {
		r.Header.Set("Authorization", "Bearer "+key)
	})
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("deleted final tenant key status = %d, body = %s", response.Code, response.Body.String())
	}
}

func TestMultiTenantRejectsUnknownHeaderKeyBeforeFirstTenant(t *testing.T) {
	_, handler, _ := newMultiTenantFixture(t)
	resp := doProxyRequest(t, handler, "/v1/responses", "s", func(r *http.Request) {
		r.Header.Set("Authorization", "Bearer srt_00000000000000000000000000000000")
	})
	if resp.Code != http.StatusUnauthorized {
		t.Fatalf("unknown tenant-shaped bearer status = %d, body = %q", resp.Code, resp.Body.String())
	}
}

func TestMultiTenantKeyRevocationAndRotation(t *testing.T) {
	registry, handler, _ := newMultiTenantFixture(t)
	created, key, err := registry.Create("gamma")
	if err != nil {
		t.Fatal(err)
	}
	writeTenantAPIKeyAccount(t, registry, created.ID, "apikey:g", "sk-tenant-g")

	if resp := doProxyRequest(t, handler, "/t/"+key+"/v1/responses", "s1"); resp.Code != http.StatusOK {
		t.Fatalf("initial key status = %d", resp.Code)
	}

	// Mint a second key through the admin API, then revoke the first.
	keysReq := httptest.NewRequest(http.MethodPost, "/_subrouter/tenants/"+created.ID+"/keys", nil)
	keysResp := httptest.NewRecorder()
	handler.ServeHTTP(keysResp, keysReq)
	if keysResp.Code != http.StatusOK {
		t.Fatalf("key create status = %d, body = %s", keysResp.Code, keysResp.Body.String())
	}
	var minted struct {
		Key string `json:"key"`
	}
	if err := json.Unmarshal(keysResp.Body.Bytes(), &minted); err != nil {
		t.Fatal(err)
	}

	revokeReq := httptest.NewRequest(http.MethodDelete, "/_subrouter/tenants/"+created.ID+"/keys/"+created.Keys[0].Prefix, nil)
	revokeResp := httptest.NewRecorder()
	handler.ServeHTTP(revokeResp, revokeReq)
	if revokeResp.Code != http.StatusOK {
		t.Fatalf("revoke status = %d, body = %s", revokeResp.Code, revokeResp.Body.String())
	}

	if resp := doProxyRequest(t, handler, "/t/"+key+"/v1/responses", "s2"); resp.Code != http.StatusUnauthorized {
		t.Fatalf("revoked key status = %d", resp.Code)
	}
	resp := doProxyRequest(t, handler, "/t/"+minted.Key+"/v1/responses", "s3")
	if resp.Code != http.StatusOK || resp.Body.String() != "Bearer sk-tenant-g" {
		t.Fatalf("rotated key = %d %q", resp.Code, resp.Body.String())
	}
}

func TestMultiTenantStickySessionsScopedPerTenant(t *testing.T) {
	registry, handler, _ := newMultiTenantFixture(t)
	alpha, keyA, err := registry.Create("alpha")
	if err != nil {
		t.Fatal(err)
	}
	beta, keyB, err := registry.Create("beta")
	if err != nil {
		t.Fatal(err)
	}
	writeTenantAPIKeyAccount(t, registry, alpha.ID, "apikey:a1", "sk-a1")
	writeTenantAPIKeyAccount(t, registry, alpha.ID, "apikey:a2", "sk-a2")
	writeTenantAPIKeyAccount(t, registry, beta.ID, "apikey:b1", "sk-b1")

	const sharedSession = "shared-session-id"
	first := doProxyRequest(t, handler, "/t/"+keyA+"/v1/responses", sharedSession)
	if first.Code != http.StatusOK {
		t.Fatalf("tenant A status = %d", first.Code)
	}
	assigned := first.Body.String()
	if assigned != "Bearer sk-a1" && assigned != "Bearer sk-a2" {
		t.Fatalf("tenant A assigned %q", assigned)
	}
	for i := 0; i < 3; i++ {
		resp := doProxyRequest(t, handler, "/t/"+keyA+"/v1/responses", sharedSession)
		if resp.Body.String() != assigned {
			t.Fatalf("sticky assignment moved from %q to %q", assigned, resp.Body.String())
		}
	}
	// The same session ID on tenant B must not see or reuse tenant A's
	// assignment; it gets tenant B's own account.
	resp := doProxyRequest(t, handler, "/t/"+keyB+"/v1/responses", sharedSession)
	if resp.Body.String() != "Bearer sk-b1" {
		t.Fatalf("tenant B session got %q", resp.Body.String())
	}
	// And tenant A keeps its assignment afterwards.
	resp = doProxyRequest(t, handler, "/t/"+keyA+"/v1/responses", sharedSession)
	if resp.Body.String() != assigned {
		t.Fatalf("tenant A assignment clobbered by tenant B: %q", resp.Body.String())
	}
}

func TestMultiTenantScopedControlEndpoints(t *testing.T) {
	registry, handler, _ := newMultiTenantFixture(t)
	created, key, err := registry.Create("alpha")
	if err != nil {
		t.Fatal(err)
	}
	writeTenantAPIKeyAccount(t, registry, created.ID, "apikey:a", "sk-tenant-a")

	get := func(path string) *httptest.ResponseRecorder {
		resp := httptest.NewRecorder()
		handler.ServeHTTP(resp, httptest.NewRequest(http.MethodGet, path, nil))
		return resp
	}

	accountsResp := get("/t/" + key + "/_subrouter/accounts")
	if accountsResp.Code != http.StatusOK {
		t.Fatalf("tenant accounts status = %d", accountsResp.Code)
	}
	var listed []struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(accountsResp.Body.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || listed[0].ID != "apikey:a" {
		t.Fatalf("tenant accounts = %+v", listed)
	}

	whoami := get("/t/" + key + "/_subrouter/whoami")
	if whoami.Code != http.StatusOK || !strings.Contains(whoami.Body.String(), created.ID) {
		t.Fatalf("whoami = %d %s", whoami.Code, whoami.Body.String())
	}

	if resp := get("/t/" + key + "/_subrouter/sessions"); resp.Code != http.StatusOK {
		t.Fatalf("tenant sessions status = %d", resp.Code)
	}
	// Endpoints outside the tenant-visible allowlist stay hidden.
	for _, path := range []string{"/_subrouter/transcripts", "/_subrouter/dashboard", "/_subrouter/drain-status"} {
		if resp := get("/t/" + key + path); resp.Code != http.StatusNotFound {
			t.Fatalf("%s status = %d, want 404", path, resp.Code)
		}
	}
}

func TestTenantSessionsRequireManageAccountsCapability(t *testing.T) {
	registry, handler, _ := newMultiTenantFixture(t)
	const tenantID = "privacy-test"
	const useKey = "srt_11111111111111111111111111111111"
	const manageKey = "srt_22222222222222222222222222222222"
	if _, err := registry.EnsureExternalRestricted(tenantID, "privacy", useKey, []tenant.Capability{tenant.CapabilityUse}); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.EnsureExternalRestricted(tenantID, "privacy", manageKey, []tenant.Capability{tenant.CapabilityManageAccounts}); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		key  string
		want int
	}{{useKey, http.StatusForbidden}, {manageKey, http.StatusOK}} {
		resp := httptest.NewRecorder()
		handler.ServeHTTP(resp, httptest.NewRequest(http.MethodGet, "/t/"+test.key+"/_subrouter/sessions", nil))
		if resp.Code != test.want {
			t.Fatalf("sessions with scoped key status = %d, want %d", resp.Code, test.want)
		}
	}
}

func TestMultiTenantAccountImportUsesTenantKeyAndStaysInTenantPool(t *testing.T) {
	registry, handler, _ := newMultiTenantFixture(t)
	created, key, err := registry.Create("alpha")
	if err != nil {
		t.Fatal(err)
	}
	payload := `{
		"provider":"codex",
		"codex":{
			"email":"apikey:tenant-import",
			"provider":"codex",
			"auth":{"auth_mode":"apikey","OPENAI_API_KEY":"sk-tenant-import"}
		}
	}`
	request := httptest.NewRequest(http.MethodPost, "/t/"+key+"/_subrouter/account-import", strings.NewReader(payload))
	request.RemoteAddr = "100.64.0.20:4321"
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("tenant import status = %d, body = %s", response.Code, response.Body.String())
	}
	stored, err := (accounts.CodexStore{Dir: filepath.Join(registry.Dir(created.ID), "codex", "accounts")}).ListStored()
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) != 1 || stored[0].Email != "apikey:tenant-import" {
		t.Fatalf("tenant stored accounts = %+v", stored)
	}
	proxied := doProxyRequest(t, handler, "/t/"+key+"/v1/responses", "tenant-import-session")
	if proxied.Code != http.StatusOK || proxied.Body.String() != "Bearer sk-tenant-import" {
		t.Fatalf("tenant imported account was not hot-loaded: %d %q", proxied.Code, proxied.Body.String())
	}

	global := httptest.NewRequest(http.MethodPost, "/_subrouter/account-import", strings.NewReader(payload))
	global.RemoteAddr = "100.64.0.20:4321"
	global.Header.Set("Content-Type", "application/json")
	globalResponse := httptest.NewRecorder()
	handler.ServeHTTP(globalResponse, global)
	if globalResponse.Code != http.StatusUnauthorized {
		t.Fatalf("unscoped import status = %d, want 401", globalResponse.Code)
	}
}

func TestMultiTenantOAuthAccountImportRotatesUntrustedCredential(t *testing.T) {
	idToken := proxyTestCodexJWT("owner@example.com", "id-token", time.Now().Add(time.Hour))
	transport := proxyRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.Method != http.MethodPost {
			return usageOKResponse(), nil
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body: io.NopCloser(strings.NewReader(fmt.Sprintf(
				`{"access_token":%q,"refresh_token":"server-refresh","id_token":%q}`,
				proxyTestCodexJWT("owner@example.com", "access-token", time.Now().Add(time.Hour)), idToken,
			))),
		}, nil
	})
	registry, handler, _ := newMultiTenantFixtureWithTransport(t, "", transport)
	created, key, err := registry.Create("alpha")
	if err != nil {
		t.Fatal(err)
	}
	payload := fmt.Sprintf(`{
		"provider":"codex",
		"codex":{
			"email":"owner@example.com",
			"provider":"codex",
			"oauthCredentialOrigin":"interactive-import",
			"auth":{"auth_mode":"chatgpt","tokens":{
				"access_token":%q,"refresh_token":"caller-refresh","id_token":%q
			}}
		}
	}`, proxyTestCodexJWT("owner@example.com", "caller-access", time.Now().Add(time.Hour)), idToken)
	request := httptest.NewRequest(http.MethodPost, "/t/"+key+"/_subrouter/account-import", strings.NewReader(payload))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("tenant OAuth import status = %d, body = %s", response.Code, response.Body.String())
	}
	stored, err := (accounts.CodexStore{Dir: filepath.Join(registry.Dir(created.ID), "codex", "accounts")}).ListStored()
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) != 1 || stored[0].OAuthCredentialOrigin != accounts.CodexOAuthOriginServerAttested ||
		stored[0].Auth.Tokens == nil || stored[0].Auth.Tokens.RefreshToken != "server-refresh" {
		t.Fatalf("tenant import stored caller-declared provenance: %#v", stored)
	}
}

func TestMultiTenantAdminCRUDRequiresAdminToken(t *testing.T) {
	registry, _, _ := newMultiTenantFixture(t)
	base := Server{AdminToken: "secret", MaxBodyBytes: 1024}
	multi := &MultiTenant{Base: base, Registry: registry}
	handler := multi.Handler(base.Handler())

	unauth := httptest.NewRequest(http.MethodPost, "/_subrouter/tenants", strings.NewReader(`{"name":"acme"}`))
	unauthResp := httptest.NewRecorder()
	handler.ServeHTTP(unauthResp, unauth)
	if unauthResp.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated create status = %d", unauthResp.Code)
	}

	create := httptest.NewRequest(http.MethodPost, "/_subrouter/tenants", strings.NewReader(`{"name":"acme"}`))
	create.Header.Set("Authorization", "Bearer secret")
	createResp := httptest.NewRecorder()
	handler.ServeHTTP(createResp, create)
	if createResp.Code != http.StatusOK {
		t.Fatalf("create status = %d, body = %s", createResp.Code, createResp.Body.String())
	}
	var created struct {
		Tenant struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"tenant"`
		Key string `json:"key"`
	}
	if err := json.Unmarshal(createResp.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.Tenant.Name != "acme" || !tenant.ValidKeyFormat(created.Key) {
		t.Fatalf("create payload = %+v", created)
	}

	list := httptest.NewRequest(http.MethodGet, "/_subrouter/tenants", nil)
	list.Header.Set("Authorization", "Bearer secret")
	listResp := httptest.NewRecorder()
	handler.ServeHTTP(listResp, list)
	if listResp.Code != http.StatusOK {
		t.Fatalf("list status = %d", listResp.Code)
	}
	if body := listResp.Body.String(); !strings.Contains(body, created.Tenant.ID) || strings.Contains(body, created.Key) {
		t.Fatalf("list body leaks key or misses tenant: %s", body)
	}
}

func TestMultiTenantStripsKeyBeforeUpstream(t *testing.T) {
	var lastXAPIKey, lastAuth string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lastXAPIKey = r.Header.Get("X-Api-Key")
		lastAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(upstream.Close)
	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	registry := tenant.NewRegistry(t.TempDir())
	created, key, err := registry.Create("alpha")
	if err != nil {
		t.Fatal(err)
	}
	writeTenantAPIKeyAccount(t, registry, created.ID, "apikey:a", "sk-tenant-a")
	base := Server{Upstream: upstreamURL, MaxBodyBytes: 1024}
	multi := &MultiTenant{Base: base, Registry: registry}
	handler := multi.Handler(base.Handler())

	// Key in X-Api-Key: Codex-routed forwarding leaves X-Api-Key alone, so the
	// wrapper must have scrubbed it before dispatch.
	resp := doProxyRequest(t, handler, "/v1/responses", "s1", func(r *http.Request) {
		r.Header.Set("X-Api-Key", key)
	})
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d", resp.Code)
	}
	if lastXAPIKey != "" {
		t.Fatalf("tenant key leaked upstream in X-Api-Key: %q", lastXAPIKey)
	}
	// Key in the path plus a stray copy in X-Api-Key is scrubbed too.
	resp = doProxyRequest(t, handler, "/t/"+key+"/v1/responses", "s2", func(r *http.Request) {
		r.Header.Set("X-Api-Key", key)
	})
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d", resp.Code)
	}
	if lastXAPIKey != "" || strings.Contains(lastAuth, key) {
		t.Fatalf("tenant key leaked upstream: x-api-key=%q auth=%q", lastXAPIKey, lastAuth)
	}
}

func TestStackLoginCreatesStableTenantAndAcceptsDirectAccountUpload(t *testing.T) {
	registry := tenant.NewRegistry(t.TempDir())
	// A hosted server always has operator tokens. Tenant account reads must
	// still authorize with the tenant path key rather than falling through to
	// the unrelated server-admin gate.
	base := Server{
		MaxBodyBytes:       1024,
		AdminToken:         "operator-secret",
		AccountImportToken: "import-secret",
	}
	multi := &MultiTenant{
		Base: base, Registry: registry, PublicURL: "https://sr.example",
		StackTenantKeySecret:   []byte("0123456789abcdef0123456789abcdef"),
		StackTenantDeleteToken: []byte(testStackTenantDeleteToken),
		StackVerifier: fakeStackVerifier{claims: stackauth.Claims{
			ProjectID: "project", SelectedTeamID: "team-123",
			Email: "user@example.com",
		}},
	}
	handler := multi.Handler(base.Handler())
	exchange := func() map[string]any {
		t.Helper()
		req := httptest.NewRequest(
			http.MethodPost,
			"/_subrouter/auth/stack",
			strings.NewReader(`{"teamId":"team-123","teamName":"Acme"}`),
		)
		req.Header.Set("Authorization", "Bearer stack-access")
		req.Header.Set("X-Subrouter-Stack-Control-Token", testStackTenantDeleteToken)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, req)
		if response.Code != http.StatusOK {
			t.Fatalf("exchange status = %d: %s", response.Code, response.Body.String())
		}
		if got := response.Header().Get("Cache-Control"); got != "no-store" {
			t.Fatalf("cache-control = %q", got)
		}
		var body map[string]any
		if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		return body
	}
	first := exchange()
	second := exchange()
	if first["tenantKey"] != second["tenantKey"] {
		t.Fatalf("tenant key changed: %v != %v", first["tenantKey"], second["tenantKey"])
	}
	key, _ := first["tenantKey"].(string)
	if !tenant.ValidKeyFormat(key) {
		t.Fatalf("bad tenant key %q", key)
	}
	if first["proxyUrl"] != "https://sr.example/t/"+key {
		t.Fatalf("proxy URL = %v", first["proxyUrl"])
	}
	if got := first["capabilities"]; !reflect.DeepEqual(
		got,
		[]any{"manage_accounts", "use"},
	) {
		t.Fatalf("capabilities = %#v", got)
	}

	upload := httptest.NewRequest(
		http.MethodPost,
		"/t/"+key+"/_subrouter/accounts",
		strings.NewReader(`{"provider":"openai-apikey","label":"work","apiKey":"sk-test"}`),
	)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, upload)
	if response.Code != http.StatusOK {
		t.Fatalf("upload status = %d: %s", response.Code, response.Body.String())
	}
	list := httptest.NewRecorder()
	handler.ServeHTTP(list, httptest.NewRequest(http.MethodGet, "/t/"+key+"/_subrouter/accounts", nil))
	if list.Code != http.StatusOK || !strings.Contains(list.Body.String(), "openai-apikey:work") {
		t.Fatalf("accounts = %d: %s", list.Code, list.Body.String())
	}
	remove := httptest.NewRecorder()
	handler.ServeHTTP(
		remove,
		httptest.NewRequest(
			http.MethodDelete,
			"/t/"+key+"/_subrouter/accounts/apikey:openai-apikey:work",
			nil,
		),
	)
	if remove.Code != http.StatusOK {
		t.Fatalf("delete = %d: %s", remove.Code, remove.Body.String())
	}
	missing := httptest.NewRecorder()
	handler.ServeHTTP(
		missing,
		httptest.NewRequest(
			http.MethodDelete,
			"/t/"+key+"/_subrouter/accounts/missing",
			nil,
		),
	)
	if missing.Code != http.StatusNotFound {
		t.Fatalf("missing delete = %d: %s", missing.Code, missing.Body.String())
	}
}

func TestStackLoginRequiresTrustedServiceCredential(t *testing.T) {
	registry := tenant.NewRegistry(t.TempDir())
	base := Server{MaxBodyBytes: 1024}
	handler := (&MultiTenant{
		Base: base, Registry: registry, PublicURL: "https://sr.example",
		StackTenantKeySecret:   []byte("0123456789abcdef0123456789abcdef"),
		StackTenantDeleteToken: []byte(testStackTenantDeleteToken),
		StackVerifier: fakeStackVerifier{claims: stackauth.Claims{
			ProjectID: "project", SelectedTeamID: "team-123",
		}},
	}).Handler(base.Handler())
	req := httptest.NewRequest(
		http.MethodPost,
		"/_subrouter/auth/stack",
		strings.NewReader(`{"teamId":"team-123","teamName":"Acme","capabilities":["use"]}`),
	)
	req.Header.Set("Authorization", "Bearer stack-access")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, req)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401, body = %s", response.Code, response.Body.String())
	}
}

func TestStackUseCredentialCannotManageTenantAccounts(t *testing.T) {
	registry := tenant.NewRegistry(t.TempDir())
	base := Server{MaxBodyBytes: 1024}
	handler := (&MultiTenant{
		Base: base, Registry: registry, PublicURL: "https://sr.example",
		StackTenantKeySecret:   []byte("0123456789abcdef0123456789abcdef"),
		StackTenantDeleteToken: []byte(testStackTenantDeleteToken),
		StackVerifier: fakeStackVerifier{claims: stackauth.Claims{
			ProjectID: "project", SelectedTeamID: "team-123",
		}},
	}).Handler(base.Handler())
	exchange := httptest.NewRequest(
		http.MethodPost,
		"/_subrouter/auth/stack",
		strings.NewReader(`{"teamId":"team-123","teamName":"Acme","capabilities":["use"]}`),
	)
	exchange.Header.Set("Authorization", "Bearer stack-access")
	exchange.Header.Set("X-Subrouter-Stack-Control-Token", testStackTenantDeleteToken)
	exchangeResponse := httptest.NewRecorder()
	handler.ServeHTTP(exchangeResponse, exchange)
	if exchangeResponse.Code != http.StatusOK {
		t.Fatalf("exchange status = %d: %s", exchangeResponse.Code, exchangeResponse.Body.String())
	}
	var body struct {
		TenantKey string `json:"tenantKey"`
	}
	if err := json.Unmarshal(exchangeResponse.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	upload := httptest.NewRequest(
		http.MethodPost,
		"/t/"+body.TenantKey+"/_subrouter/accounts",
		strings.NewReader(`{"provider":"openai-apikey","label":"work","apiKey":"sk-test"}`),
	)
	uploadResponse := httptest.NewRecorder()
	handler.ServeHTTP(uploadResponse, upload)
	if uploadResponse.Code != http.StatusForbidden {
		t.Fatalf("upload status = %d, want 403: %s", uploadResponse.Code, uploadResponse.Body.String())
	}
	listResponse := httptest.NewRecorder()
	handler.ServeHTTP(
		listResponse,
		httptest.NewRequest(
			http.MethodGet,
			"/t/"+body.TenantKey+"/_subrouter/accounts",
			nil,
		),
	)
	if listResponse.Code != http.StatusOK {
		t.Fatalf("list status = %d, want 200: %s", listResponse.Code, listResponse.Body.String())
	}
}

func TestLegacyDirectStackCredentialExpiresAfterDeploymentGrace(t *testing.T) {
	secret := []byte("0123456789abcdef0123456789abcdef")
	legacyKey, err := tenant.DeriveKey(secret, "project", "team-123")
	if err != nil {
		t.Fatal(err)
	}
	registry := tenant.NewRegistry(t.TempDir())
	if _, err := registry.EnsureExternal("team-123", "Acme", legacyKey); err != nil {
		t.Fatal(err)
	}
	base := Server{MaxBodyBytes: 1024}
	now := time.Date(2026, time.August, 4, 0, 0, 0, 0, time.UTC)
	multi := &MultiTenant{
		Base: base, Registry: registry, StackProjectID: "project",
		StackTenantKeySecret: secret,
		StackLegacyKeyCutoff: now.Add(30 * 24 * time.Hour),
		Now:                  func() time.Time { return now },
	}
	handler := multi.Handler(base.Handler())
	request := func() *httptest.ResponseRecorder {
		response := httptest.NewRecorder()
		handler.ServeHTTP(
			response,
			httptest.NewRequest(
				http.MethodGet,
				"/t/"+legacyKey+"/_subrouter/whoami",
				nil,
			),
		)
		return response
	}
	if response := request(); response.Code != http.StatusOK {
		t.Fatalf("grace status = %d, want 200: %s", response.Code, response.Body.String())
	}
	now = multi.StackLegacyKeyCutoff
	if response := request(); response.Code != http.StatusUnauthorized {
		t.Fatalf("expired status = %d, want 401: %s", response.Code, response.Body.String())
	}
}

func TestStackTenantDeletionRequiresTrustedServiceCredential(t *testing.T) {
	registry := tenant.NewRegistry(t.TempDir())
	key, err := tenant.DeriveKey(
		[]byte("0123456789abcdef0123456789abcdef"),
		"project",
		"team-victim",
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := registry.EnsureExternal("team-victim", "Victim", key); err != nil {
		t.Fatal(err)
	}
	base := Server{MaxBodyBytes: 1024}
	handler := (&MultiTenant{
		Base: base, Registry: registry,
		StackVerifier: fakeStackVerifier{claims: stackauth.Claims{
			Subject: "ordinary-member", ProjectID: "project", SelectedTeamID: "team-victim",
		}},
		StackTenantKeySecret:   []byte("0123456789abcdef0123456789abcdef"),
		StackTenantDeleteToken: []byte(testStackTenantDeleteToken),
	}).Handler(base.Handler())
	req := httptest.NewRequest(
		http.MethodDelete,
		"/_subrouter/auth/stack/tenant",
		strings.NewReader(`{"teamId":"team-victim"}`),
	)
	req.Header.Set("Authorization", "Bearer stack-access")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, req)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401, body = %s", response.Code, response.Body.String())
	}
	if resolved, ok, err := registry.Resolve(key); err != nil || !ok || resolved.ID != "team-victim" {
		t.Fatalf("untrusted deletion changed tenant: resolved=%#v ok=%v err=%v", resolved, ok, err)
	}
}

func TestStackTenantDeletionRemovesTenantTranscripts(t *testing.T) {
	registry := tenant.NewRegistry(t.TempDir())
	key, err := tenant.DeriveKey(
		[]byte("0123456789abcdef0123456789abcdef"),
		"project",
		"user-1",
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := registry.EnsureExternal("user-1", "User One", key); err != nil {
		t.Fatal(err)
	}
	transcriptRoot := t.TempDir()
	tenantTranscriptDir := filepath.Join(transcriptRoot, "tenants", "user-1")
	if err := os.MkdirAll(tenantTranscriptDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tenantTranscriptDir, "session.jsonl"), []byte("secret transcript"), 0o600); err != nil {
		t.Fatal(err)
	}

	base := Server{MaxBodyBytes: 1024}
	handler := (&MultiTenant{
		Base: base, Registry: registry, TranscriptDir: transcriptRoot,
		StackVerifier: fakeStackVerifier{claims: stackauth.Claims{
			Subject: "user-1", ProjectID: "project", SelectedTeamID: "team-123",
		}},
		StackTenantKeySecret:   []byte("0123456789abcdef0123456789abcdef"),
		StackTenantDeleteToken: []byte(testStackTenantDeleteToken),
	}).Handler(base.Handler())
	req := httptest.NewRequest(
		http.MethodDelete,
		"/_subrouter/auth/stack/tenant",
		strings.NewReader(`{"teamId":"user-1"}`),
	)
	req.Header.Set("Authorization", "Bearer stack-access")
	req.Header.Set("X-Subrouter-Tenant-Delete-Token", testStackTenantDeleteToken)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, req)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if _, err := os.Stat(tenantTranscriptDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("tenant transcripts remain after deletion: %v", err)
	}
}

func TestTenantTranscriptDeletionFailureStaysRecoverable(t *testing.T) {
	registry := tenant.NewRegistry(t.TempDir())
	key, err := tenant.DeriveKey(
		[]byte("0123456789abcdef0123456789abcdef"),
		"project",
		"user-1",
	)
	if err != nil {
		t.Fatal(err)
	}
	created, err := registry.EnsureExternal("user-1", "User One", key)
	if err != nil {
		t.Fatal(err)
	}
	if retired, err := registry.RetireExternal(created.ID); err != nil || !retired {
		t.Fatalf("retire = %v, %v", retired, err)
	}
	transcriptRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(transcriptRoot, "tenants"), []byte("blocks tenant transcript cleanup"), 0o600); err != nil {
		t.Fatal(err)
	}
	multi := &MultiTenant{Registry: registry, TranscriptDir: transcriptRoot}
	useLock, err := registry.AcquireExclusiveUse(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := multi.deleteRetiredTenant(created.ID); err == nil {
		useLock.Close()
		t.Fatal("blocked transcript cleanup unexpectedly succeeded")
	}
	if err := useLock.Close(); err != nil {
		t.Fatal(err)
	}

	pendingIDs, err := registry.PendingDeletionIDs()
	if err != nil {
		t.Fatal(err)
	}
	pending := false
	for _, id := range pendingIDs {
		if id == created.ID {
			pending = true
			break
		}
	}
	if !pending {
		t.Fatal("transcript cleanup failure finalized the deletion marker")
	}
	if _, err := os.Stat(registry.Dir(created.ID)); err != nil {
		t.Fatalf("credential state was removed before transcript cleanup completed: %v", err)
	}
}

func TestTenantDeletionRecoveryRetriesAfterTransientStartupScanFailure(t *testing.T) {
	stateDir := t.TempDir()
	registry := tenant.NewRegistry(stateDir)
	key, err := tenant.DeriveKey(
		[]byte("0123456789abcdef0123456789abcdef"),
		"project",
		"user-1",
	)
	if err != nil {
		t.Fatal(err)
	}
	created, err := registry.EnsureExternal("user-1", "User One", key)
	if err != nil {
		t.Fatal(err)
	}
	if retired, err := registry.RetireExternal(created.ID); err != nil || !retired {
		t.Fatalf("retire = %v, %v", retired, err)
	}

	registryBackup := registry.Path() + ".backup"
	if err := os.Rename(registry.Path(), registryBackup); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(registry.Path(), 0o700); err != nil {
		t.Fatal(err)
	}
	recoveryFailure := make(chan struct{}, 1)
	base := Server{Logger: slog.New(slog.NewTextHandler(signalLogWriter{
		match: "tenant deletion recovery scan failed",
		seen:  recoveryFailure,
	}, nil))}
	multi := &MultiTenant{Base: base, Registry: registry}
	_ = multi.Handler(base.Handler())
	select {
	case <-recoveryFailure:
	case <-time.After(time.Second):
		t.Fatal("startup recovery scan did not report the injected failure")
	}
	if err := os.Remove(registry.Path()); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(registryBackup, registry.Path()); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		_, err := os.Stat(registry.Dir(created.ID))
		if errors.Is(err, os.ErrNotExist) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if time.Now().After(deadline) {
			t.Fatal("startup deletion recovery was not retried after the registry recovered")
		}
		time.Sleep(10 * time.Millisecond)
	}
	deadline = time.Now().Add(5 * time.Second)
	for {
		multi.deletionMu.Lock()
		_, pending := multi.deletions[created.ID]
		multi.deletionMu.Unlock()
		if !pending {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("startup deletion recovery worker did not finish")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestStackTenantDeletionRetriesAfterExclusiveLockSetupFailure(t *testing.T) {
	stateDir := t.TempDir()
	registry := tenant.NewRegistry(stateDir)
	key, err := tenant.DeriveKey(
		[]byte("0123456789abcdef0123456789abcdef"),
		"project",
		"user-1",
	)
	if err != nil {
		t.Fatal(err)
	}
	created, err := registry.EnsureExternal("user-1", "User One", key)
	if err != nil {
		t.Fatal(err)
	}
	writeTenantAPIKeyAccount(t, registry, created.ID, "apikey:openai-apikey:work", "sk-test")

	// A non-directory at the lock-directory path makes both the synchronous
	// lock attempt and the background worker's first attempt fail.
	lockDirectory := filepath.Join(stateDir, "tenant-use-locks")
	if err := os.WriteFile(lockDirectory, []byte("blocked"), 0o600); err != nil {
		t.Fatal(err)
	}
	base := Server{MaxBodyBytes: 1024}
	multi := &MultiTenant{
		Base: base, Registry: registry,
		StackVerifier: fakeStackVerifier{claims: stackauth.Claims{
			Subject: "user-1", ProjectID: "project", SelectedTeamID: "team-123",
		}},
		StackTenantKeySecret:   []byte("0123456789abcdef0123456789abcdef"),
		StackTenantDeleteToken: []byte(testStackTenantDeleteToken),
	}
	handler := multi.Handler(base.Handler())
	req := httptest.NewRequest(
		http.MethodDelete,
		"/_subrouter/auth/stack/tenant",
		strings.NewReader(`{"teamId":"user-1"}`),
	)
	req.Header.Set("Authorization", "Bearer stack-access")
	req.Header.Set("X-Subrouter-Tenant-Delete-Token", testStackTenantDeleteToken)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, req)
	if response.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500, body = %s", response.Code, response.Body.String())
	}
	if _, ok, err := registry.Resolve(key); err != nil || ok {
		t.Fatalf("retired key still resolves: ok=%v err=%v", ok, err)
	}
	if err := os.Remove(lockDirectory); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		_, err := os.Stat(registry.Dir(created.ID))
		if errors.Is(err, os.ErrNotExist) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if time.Now().After(deadline) {
			t.Fatal("retired tenant was not deleted after the lock setup recovered")
		}
		time.Sleep(10 * time.Millisecond)
	}
	deadline = time.Now().Add(5 * time.Second)
	for {
		multi.deletionMu.Lock()
		_, pending := multi.deletions[created.ID]
		multi.deletionMu.Unlock()
		if !pending {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("background tenant deletion did not finish")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestStackTenantDeletionRetriesRetirementFailures(t *testing.T) {
	for _, testCase := range []struct {
		name  string
		fault func(*testing.T, string, *tenant.Registry) func()
	}{
		{
			name: "after committed revocation",
			fault: func(t *testing.T, stateDir string, _ *tenant.Registry) func() {
				t.Helper()
				blocker := filepath.Join(stateDir, "retiring-tenants")
				if err := os.WriteFile(blocker, []byte("blocked"), 0o600); err != nil {
					t.Fatal(err)
				}
				return func() {
					if err := os.Remove(blocker); err != nil {
						t.Fatal(err)
					}
				}
			},
		},
		{
			name: "before committed revocation",
			fault: func(t *testing.T, _ string, registry *tenant.Registry) func() {
				t.Helper()
				backup := registry.Path() + ".backup"
				if err := os.Rename(registry.Path(), backup); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(registry.Path(), 0o700); err != nil {
					t.Fatal(err)
				}
				return func() {
					if err := os.Remove(registry.Path()); err != nil {
						t.Fatal(err)
					}
					if err := os.Rename(backup, registry.Path()); err != nil {
						t.Fatal(err)
					}
				}
			},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			stateDir := t.TempDir()
			registry := tenant.NewRegistry(stateDir)
			key, err := tenant.DeriveKey(
				[]byte("0123456789abcdef0123456789abcdef"),
				"project",
				"user-1",
			)
			if err != nil {
				t.Fatal(err)
			}
			created, err := registry.EnsureExternal("user-1", "User One", key)
			if err != nil {
				t.Fatal(err)
			}
			writeTenantAPIKeyAccount(t, registry, created.ID, "apikey:openai-apikey:work", "sk-test")
			recoverFault := testCase.fault(t, stateDir, registry)

			base := Server{MaxBodyBytes: 1024}
			multi := &MultiTenant{
				Base: base, Registry: registry,
				StackVerifier: fakeStackVerifier{claims: stackauth.Claims{
					Subject: "user-1", ProjectID: "project", SelectedTeamID: "team-123",
				}},
				StackTenantKeySecret:   []byte("0123456789abcdef0123456789abcdef"),
				StackTenantDeleteToken: []byte(testStackTenantDeleteToken),
			}
			handler := multi.Handler(base.Handler())
			req := httptest.NewRequest(
				http.MethodDelete,
				"/_subrouter/auth/stack/tenant",
				strings.NewReader(`{"teamId":"user-1"}`),
			)
			req.Header.Set("Authorization", "Bearer stack-access")
			req.Header.Set("X-Subrouter-Tenant-Delete-Token", testStackTenantDeleteToken)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, req)
			if response.Code != http.StatusInternalServerError {
				t.Fatalf("status = %d, want 500, body = %s", response.Code, response.Body.String())
			}
			recoverFault()

			deadline := time.Now().Add(time.Second)
			for {
				_, err := os.Stat(registry.Dir(created.ID))
				if errors.Is(err, os.ErrNotExist) {
					break
				}
				if err != nil {
					t.Fatal(err)
				}
				if time.Now().After(deadline) {
					t.Fatal("retired tenant was not deleted after retirement recovered")
				}
				time.Sleep(10 * time.Millisecond)
			}
			for {
				multi.deletionMu.Lock()
				_, pending := multi.deletions[created.ID]
				multi.deletionMu.Unlock()
				if !pending {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("background tenant deletion did not finish")
				}
				time.Sleep(10 * time.Millisecond)
			}
		})
	}
}

func TestStackTenantDeletionRevokesNewRequestsThenDrainsInFlightTraffic(t *testing.T) {
	releaseUpstream := make(chan struct{})
	upstreamReleased := false
	backgroundFailure := make(chan struct{}, 1)
	defer func() {
		if !upstreamReleased {
			close(releaseUpstream)
		}
	}()
	upstreamStarted := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		select {
		case <-upstreamStarted:
		default:
			close(upstreamStarted)
		}
		<-releaseUpstream
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(upstream.Close)
	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	sessions, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	base := Server{
		Upstream:     upstreamURL,
		Sessions:     sessions,
		Scheduler:    selectacct.NewScheduler(nil),
		MaxBodyBytes: 1024,
		Logger: slog.New(slog.NewTextHandler(signalLogWriter{
			match: "background tenant deletion failed",
			seen:  backgroundFailure,
		}, nil)),
	}
	registry := tenant.NewRegistry(t.TempDir())
	key, err := tenant.DeriveKey(
		[]byte("0123456789abcdef0123456789abcdef"),
		"project",
		"user-1",
	)
	if err != nil {
		t.Fatal(err)
	}
	created, err := registry.EnsureExternal("user-1", "User One", key)
	if err != nil {
		t.Fatal(err)
	}
	writeTenantAPIKeyAccount(t, registry, created.ID, "apikey:openai-apikey:work", "sk-test")
	multi := &MultiTenant{
		Base: base, Registry: registry, PublicURL: "https://sr.example",
		StackTenantKeySecret:   []byte("0123456789abcdef0123456789abcdef"),
		StackTenantDeleteToken: []byte(testStackTenantDeleteToken),
		StackVerifier: fakeStackVerifier{claims: stackauth.Claims{
			Subject: "user-1", ProjectID: "project", SelectedTeamID: "team-123",
		}},
	}
	handler := multi.Handler(base.Handler())

	inFlightDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		inFlightDone <- doProxyRequest(t, handler, "/v1/responses", "held-session", func(req *http.Request) {
			req.Header.Set("Authorization", "Bearer "+key)
		})
	}()
	select {
	case <-upstreamStarted:
	case <-time.After(time.Second):
		t.Fatal("tenant request did not reach upstream")
	}

	deleteTenant := func() *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(
			http.MethodDelete,
			"/_subrouter/auth/stack/tenant",
			strings.NewReader(`{"teamId":"user-1"}`),
		)
		req.Header.Set("Authorization", "Bearer stack-access")
		req.Header.Set("X-Subrouter-Tenant-Delete-Token", testStackTenantDeleteToken)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, req)
		return response
	}

	firstDelete := deleteTenant()
	if firstDelete.Code != http.StatusAccepted {
		t.Fatalf("first deletion status = %d, body = %s", firstDelete.Code, firstDelete.Body.String())
	}
	var pending map[string]any
	if err := json.Unmarshal(firstDelete.Body.Bytes(), &pending); err != nil {
		t.Fatal(err)
	}
	if pending["deletionPending"] != true {
		t.Fatalf("pending deletion response = %#v", pending)
	}
	if _, ok, err := registry.Resolve(key); err != nil || ok {
		t.Fatalf("retired key still resolves: ok=%v err=%v", ok, err)
	}
	rejected := doProxyRequest(t, handler, "/v1/responses", "new-session", func(req *http.Request) {
		req.Header.Set("Authorization", "Bearer "+key)
	})
	if rejected.Code != http.StatusUnauthorized {
		t.Fatalf("new request after retirement status = %d", rejected.Code)
	}
	registryBackup := registry.Path() + ".test-backup"
	if err := os.Rename(registry.Path(), registryBackup); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(registry.Path(), 0o700); err != nil {
		t.Fatal(err)
	}

	close(releaseUpstream)
	upstreamReleased = true
	select {
	case response := <-inFlightDone:
		if response.Code != http.StatusOK {
			t.Fatalf("in-flight request status = %d, body = %s", response.Code, response.Body.String())
		}
	case <-time.After(time.Second):
		t.Fatal("in-flight request did not drain")
	}
	select {
	case <-backgroundFailure:
	case <-time.After(time.Second):
		t.Fatal("background tenant deletion did not report the injected transient failure")
	}
	if err := os.Remove(registry.Path()); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(registryBackup, registry.Path()); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for {
		_, err := os.Stat(registry.Dir("user-1"))
		if errors.Is(err, os.ErrNotExist) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if time.Now().After(deadline) {
			t.Fatal("retired tenant was not deleted after in-flight requests drained")
		}
		time.Sleep(10 * time.Millisecond)
	}
	finalDelete := deleteTenant()
	if finalDelete.Code != http.StatusOK {
		t.Fatalf("final deletion status = %d, body = %s", finalDelete.Code, finalDelete.Body.String())
	}
	if _, err := os.Stat(registry.Dir("user-1")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("tenant state remains after deletion: %v", err)
	}
	if _, found, err := registry.Find("user-1"); err != nil || found {
		t.Fatalf("tenant registry entry remains: found=%v err=%v", found, err)
	}
	reactivate := httptest.NewRequest(
		http.MethodPost,
		"/_subrouter/auth/stack",
		strings.NewReader(`{"teamId":"user-1","teamName":"User One","capabilities":["use"]}`),
	)
	reactivate.Header.Set("Authorization", "Bearer stack-access")
	reactivate.Header.Set("X-Subrouter-Stack-Control-Token", testStackTenantDeleteToken)
	reactivateResponse := httptest.NewRecorder()
	handler.ServeHTTP(reactivateResponse, reactivate)
	if reactivateResponse.Code != http.StatusGone {
		t.Fatalf("retired tenant reactivation status = %d, body = %s", reactivateResponse.Code, reactivateResponse.Body.String())
	}
	if repeated := deleteTenant(); repeated.Code != http.StatusOK {
		t.Fatalf("idempotent deletion status = %d, body = %s", repeated.Code, repeated.Body.String())
	}
}

func TestStackTenantDeletionRejectsNonMemberWithoutRetiringTenant(t *testing.T) {
	registry := tenant.NewRegistry(t.TempDir())
	key, err := tenant.DeriveKey(
		[]byte("0123456789abcdef0123456789abcdef"),
		"project",
		"team-victim",
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := registry.EnsureExternal("team-victim", "Victim", key); err != nil {
		t.Fatal(err)
	}
	base := Server{MaxBodyBytes: 1024}
	handler := (&MultiTenant{
		Base: base, Registry: registry,
		StackVerifier: fakeStackVerifier{claims: stackauth.Claims{
			Subject: "user-1", ProjectID: "project", SelectedTeamID: "team-123",
		}},
		StackTeams:             fakeStackTeams{teams: []stackauth.Team{{ID: "team-123"}}},
		StackTenantKeySecret:   []byte("0123456789abcdef0123456789abcdef"),
		StackTenantDeleteToken: []byte(testStackTenantDeleteToken),
	}).Handler(base.Handler())
	req := httptest.NewRequest(
		http.MethodDelete,
		"/_subrouter/auth/stack/tenant",
		strings.NewReader(`{"teamId":"team-victim"}`),
	)
	req.Header.Set("Authorization", "Bearer stack-access")
	req.Header.Set("X-Subrouter-Tenant-Delete-Token", testStackTenantDeleteToken)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, req)
	if response.Code != http.StatusForbidden {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if resolved, ok, err := registry.Resolve(key); err != nil || !ok || resolved.ID != "team-victim" {
		t.Fatalf("unauthorized deletion changed tenant: resolved=%#v ok=%v err=%v", resolved, ok, err)
	}
}

func TestTenantAccountUploadValidatesRepairTargetAndBodyShape(t *testing.T) {
	registry, handler, _ := newMultiTenantFixture(t)
	_, key, err := registry.Create("team")
	if err != nil {
		t.Fatal(err)
	}
	path := "/t/" + key + "/_subrouter/accounts"
	request := func(body string) *httptest.ResponseRecorder {
		t.Helper()
		response := httptest.NewRecorder()
		handler.ServeHTTP(
			response,
			httptest.NewRequest(http.MethodPost, path, strings.NewReader(body)),
		)
		return response
	}

	mismatch := request(`{
		"provider":"openai-apikey",
		"label":"work",
		"apiKey":"sk-test",
		"targetAccountID":"apikey:openai-apikey:other"
	}`)
	if mismatch.Code != http.StatusConflict {
		t.Fatalf("mismatched repair status = %d, body = %s", mismatch.Code, mismatch.Body.String())
	}
	list := httptest.NewRecorder()
	handler.ServeHTTP(list, httptest.NewRequest(http.MethodGet, path, nil))
	if list.Code != http.StatusOK || strings.TrimSpace(list.Body.String()) != "[]" {
		t.Fatalf("mismatched repair mutated accounts: %d %s", list.Code, list.Body.String())
	}

	for name, body := range map[string]string{
		"unknown field":  `{"provider":"openai-apikey","label":"work","apiKey":"sk-test","refreshToken":"secret"}`,
		"trailing value": `{"provider":"openai-apikey","label":"work","apiKey":"sk-test"}{}`,
	} {
		t.Run(name, func(t *testing.T) {
			response := request(body)
			if response.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
			}
		})
	}

	matching := request(`{
		"provider":"openai-apikey",
		"label":"work",
		"apiKey":"sk-test",
		"targetAccountID":"apikey:openai-apikey:work"
	}`)
	if matching.Code != http.StatusOK {
		t.Fatalf("matching repair status = %d, body = %s", matching.Code, matching.Body.String())
	}
}

func TestTenantAccountUploadPreservesDistinctMigrationIDsAndLabels(t *testing.T) {
	registry, handler, _ := newMultiTenantFixture(t)
	_, key, err := registry.Create("team")
	if err != nil {
		t.Fatal(err)
	}
	path := "/t/" + key + "/_subrouter/accounts"
	for _, id := range []string{"legacy-account-a", "legacy-account-b"} {
		body := fmt.Sprintf(`{
			"provider":"openai-apikey",
			"accountId":%q,
			"label":"Shared account",
			"apiKey":"sk-%s"
		}`, id, id)
		response := httptest.NewRecorder()
		handler.ServeHTTP(
			response,
			httptest.NewRequest(http.MethodPost, path, strings.NewReader(body)),
		)
		if response.Code != http.StatusOK {
			t.Fatalf("upload %s status = %d, body = %s", id, response.Code, response.Body.String())
		}
	}

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
	if response.Code != http.StatusOK {
		t.Fatalf("list status = %d, body = %s", response.Code, response.Body.String())
	}
	var accounts []struct {
		ID    string `json:"id"`
		Label string `json:"label"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &accounts); err != nil {
		t.Fatal(err)
	}
	if len(accounts) != 2 {
		t.Fatalf("accounts = %#v, want two distinct migrated accounts", accounts)
	}
	sort.Slice(accounts, func(i, j int) bool { return accounts[i].ID < accounts[j].ID })
	for i, id := range []string{"legacy-account-a", "legacy-account-b"} {
		if accounts[i].ID != id || accounts[i].Label != "Shared account" {
			t.Fatalf("account %d = %#v", i, accounts[i])
		}
	}
}

func TestTenantCodexOAuthUploadRequiresServerAttestedTransfer(t *testing.T) {
	transport := proxyRoundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body: io.NopCloser(strings.NewReader(fmt.Sprintf(
				`{"access_token":%q,"refresh_token":"refresh","id_token":%q}`,
				proxyTestCodexJWT("owner@example.com", "access-token", time.Now().Add(time.Hour)),
				proxyTestCodexJWT("owner@example.com", "id-token", time.Now().Add(time.Hour)),
			))),
		}, nil
	})
	registry, handler, _ := newMultiTenantFixtureWithTransport(t, "", transport)
	_, key, err := registry.Create("team")
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(
		http.MethodPost,
		"/t/"+key+"/_subrouter/accounts",
		strings.NewReader(`{
			"provider":"codex",
			"accountId":"unproven-account",
			"label":"Unproven",
			"oauthCredentialOrigin":"interactive-import",
			"tokens":{
				"accessToken":"access",
				"refreshToken":"refresh",
				"idToken":"id",
				"accountID":"provider"
			}
		}`),
	))
	if response.Code != http.StatusBadRequest {
		t.Fatalf("upload status = %d, body = %s", response.Code, response.Body.String())
	}
}

func TestTenantCodexAccountListSeparatesRoutingIDFromOAuthIdentity(t *testing.T) {
	idToken := proxyTestCodexJWT("owner@example.com", "id-token", time.Now().Add(time.Hour))
	var submittedRefresh string
	transport := proxyRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.Method != http.MethodPost {
			return usageOKResponse(), nil
		}
		var payload struct {
			RefreshToken string `json:"refresh_token"`
		}
		if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		submittedRefresh = payload.RefreshToken
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body: io.NopCloser(strings.NewReader(fmt.Sprintf(
				`{"access_token":%q,"refresh_token":"server-refresh","id_token":%q}`,
				proxyTestCodexJWT("owner@example.com", "access-token", time.Now().Add(time.Hour)), idToken,
			))),
		}, nil
	})
	registry, handler, _ := newMultiTenantFixtureWithTransport(t, "", transport)
	_, key, err := registry.Create("team")
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(
		http.MethodPost,
		"/t/"+key+"/_subrouter/accounts",
		strings.NewReader(fmt.Sprintf(`{
			"provider":"codex",
			"accountId":"stable-routing-id",
			"label":"Production Codex",
			"oauthCredentialOrigin":"interactive-import",
			"tokens":{
				"accessToken":"access",
				"refreshToken":"refresh",
				"idToken":%q,
				"accountID":"provider-account"
			}
		}`, idToken)),
	))
	if response.Code != http.StatusOK {
		t.Fatalf("upload status = %d, body = %s", response.Code, response.Body.String())
	}

	response = httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(
		http.MethodGet,
		"/t/"+key+"/_subrouter/accounts",
		nil,
	))
	if response.Code != http.StatusOK {
		t.Fatalf("list status = %d, body = %s", response.Code, response.Body.String())
	}
	var listed []struct {
		ID    string `json:"id"`
		Label string `json:"label"`
		Email string `json:"email"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || listed[0].ID != "stable-routing-id" || listed[0].Label != "Production Codex" || listed[0].Email != "owner@example.com" {
		t.Fatalf("listed accounts = %#v", listed)
	}
	if submittedRefresh != "refresh" {
		t.Fatalf("server attestation used refresh token %q, want submitted chain", submittedRefresh)
	}
	tenants, err := registry.List()
	if err != nil {
		t.Fatal(err)
	}
	tenantRecord := tenants[0]
	store := accounts.CodexStore{Dir: filepath.Join(registry.Dir(tenantRecord.ID), "codex", "accounts")}
	storedAccounts, err := store.ListStored()
	if err != nil {
		t.Fatal(err)
	}
	if len(storedAccounts) != 1 ||
		storedAccounts[0].OAuthCredentialOrigin != accounts.CodexOAuthOriginServerAttested ||
		storedAccounts[0].Auth.Tokens == nil || storedAccounts[0].Auth.Tokens.RefreshToken != "server-refresh" {
		t.Fatalf("stored credential was not server-attested: %#v", storedAccounts)
	}
}

func TestTenantCodexRepairPreservesOAuthIdentity(t *testing.T) {
	var refreshedIdentity = "owner@example.com"
	var refreshCalls atomic.Int32
	transport := proxyRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.Method == http.MethodPost {
			refreshCalls.Add(1)
		}
		idToken := proxyTestCodexJWT(refreshedIdentity, "id-token", time.Now().Add(time.Hour))
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body: io.NopCloser(strings.NewReader(fmt.Sprintf(
				`{"access_token":%q,"refresh_token":"server-refresh","id_token":%q}`,
				proxyTestCodexJWT(refreshedIdentity, "access-token", time.Now().Add(time.Hour)), idToken,
			))),
		}, nil
	})
	registry, handler, _ := newMultiTenantFixtureWithTransport(t, "", transport)
	created, key, err := registry.Create("team")
	if err != nil {
		t.Fatal(err)
	}
	path := "/t/" + key + "/_subrouter/accounts"
	upload := func(identity, refreshToken, target string) *httptest.ResponseRecorder {
		t.Helper()
		idToken := proxyTestCodexJWT(identity, "submitted-id", time.Now().Add(time.Hour))
		body := fmt.Sprintf(`{
			"provider":"codex","accountId":"stable-routing-id","label":"Production Codex",
			"targetAccountID":%q,
			"tokens":{"accessToken":%q,"refreshToken":%q,"idToken":%q}
		}`,
			target,
			proxyTestCodexJWT(identity, "submitted-access", time.Now().Add(time.Hour)),
			refreshToken,
			idToken,
		)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, path, strings.NewReader(body)))
		return response
	}

	if response := upload("owner@example.com", "owner-refresh", ""); response.Code != http.StatusOK {
		t.Fatalf("initial upload status = %d, body = %s", response.Code, response.Body.String())
	}
	if refreshCalls.Load() != 1 {
		t.Fatalf("initial refresh calls = %d, want 1", refreshCalls.Load())
	}
	if response := upload("owner@example.com", "duplicate-refresh", ""); response.Code != http.StatusConflict {
		t.Fatalf("duplicate add status = %d, want 409, body = %s", response.Code, response.Body.String())
	}
	if refreshCalls.Load() != 1 {
		t.Fatalf("duplicate add consumed refresh token: calls = %d, want 1", refreshCalls.Load())
	}
	refreshedIdentity = "other@example.com"
	response := upload("other@example.com", "other-refresh", "stable-routing-id")
	if response.Code != http.StatusConflict {
		t.Fatalf("cross-identity repair status = %d, want 409, body = %s", response.Code, response.Body.String())
	}
	if refreshCalls.Load() != 1 {
		t.Fatalf("cross-identity repair consumed refresh token: calls = %d, want 1", refreshCalls.Load())
	}
	store := accounts.CodexStore{Dir: filepath.Join(registry.Dir(created.ID), "codex", "accounts")}
	stored, found, err := store.FindStored("stable-routing-id")
	if err != nil || !found {
		t.Fatalf("find original account: found=%v err=%v", found, err)
	}
	identity, err := accounts.ExtractEmailFromJWT(stored.Auth.Tokens.IDToken)
	if err != nil || identity != "owner@example.com" {
		t.Fatalf("cross-identity repair changed stored identity: identity=%q err=%v", identity, err)
	}
}

func TestTenantCodexRepairUsesCanonicalStoredIDForPartialSelector(t *testing.T) {
	const canonicalID = "owner@example.com"
	identityToken := proxyTestCodexJWT(canonicalID, "id-token", time.Now().Add(time.Hour))
	transport := proxyRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.Method != http.MethodPost {
			return usageOKResponse(), nil
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body: io.NopCloser(strings.NewReader(fmt.Sprintf(
				`{"access_token":%q,"refresh_token":"server-refresh","id_token":%q}`,
				proxyTestCodexJWT(canonicalID, "access-token", time.Now().Add(time.Hour)), identityToken,
			))),
		}, nil
	})
	registry, handler, _ := newMultiTenantFixtureWithTransport(t, "", transport)
	created, key, err := registry.Create("team")
	if err != nil {
		t.Fatal(err)
	}
	store := accounts.CodexStore{Dir: filepath.Join(registry.Dir(created.ID), "codex", "accounts")}
	if err := store.SaveStored(accounts.StoredCodexAccount{
		Email: canonicalID, Provider: accounts.ProviderCodex,
		OAuthCredentialOrigin: accounts.CodexOAuthOriginServerAttested,
		Auth: accounts.CodexAuthFile{AuthMode: "chatgpt", Tokens: &accounts.CodexTokens{
			AccessToken:  proxyTestCodexJWT(canonicalID, "old-access", time.Now().Add(time.Hour)),
			RefreshToken: "old-refresh", IDToken: identityToken,
		}},
	}); err != nil {
		t.Fatal(err)
	}
	submittedID := proxyTestCodexJWT(canonicalID, "submitted-id", time.Now().Add(time.Hour))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(
		http.MethodPost,
		"/t/"+key+"/_subrouter/accounts",
		strings.NewReader(fmt.Sprintf(`{
			"provider":"codex","accountId":"owner","targetAccountID":"owner","label":"Owner",
			"tokens":{"accessToken":"access","refreshToken":"replacement-refresh","idToken":%q}
		}`, submittedID)),
	))
	if response.Code != http.StatusOK {
		t.Fatalf("partial repair status = %d, body = %s", response.Code, response.Body.String())
	}
	stored, err := store.ListStored()
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) != 1 || stored[0].Email != canonicalID ||
		stored[0].Auth.Tokens == nil || stored[0].Auth.Tokens.RefreshToken != "server-refresh" {
		t.Fatalf("partial repair did not update canonical account: %#v", stored)
	}
}

func TestTenantCodexUploadRejectsMalformedRefreshedIdentity(t *testing.T) {
	var refreshCalls atomic.Int32
	transport := proxyRoundTripFunc(func(*http.Request) (*http.Response, error) {
		refreshCalls.Add(1)
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body: io.NopCloser(strings.NewReader(
				`{"access_token":"access","refresh_token":"server-refresh","id_token":"not-a-jwt"}`,
			)),
		}, nil
	})
	registry, handler, _ := newMultiTenantFixtureWithTransport(t, "", transport)
	created, key, err := registry.Create("team")
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(
		http.MethodPost,
		"/t/"+key+"/_subrouter/accounts",
		strings.NewReader(fmt.Sprintf(`{
			"provider":"codex","accountId":"stable-routing-id","label":"Production Codex",
			"tokens":{"accessToken":"access","refreshToken":"refresh","idToken":%q}
		}`, proxyTestCodexJWT("owner@example.com", "submitted-id", time.Now().Add(time.Hour)))),
	))
	if response.Code != http.StatusBadRequest {
		t.Fatalf("malformed identity status = %d, want 400, body = %s", response.Code, response.Body.String())
	}
	if refreshCalls.Load() != 1 {
		t.Fatalf("refresh calls = %d, want provider response validation", refreshCalls.Load())
	}
	stored, err := (accounts.CodexStore{
		Dir: filepath.Join(registry.Dir(created.ID), "codex", "accounts"),
	}).ListStored()
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) != 0 {
		t.Fatalf("malformed refreshed identity stored an account: %#v", stored)
	}
}

func TestTenantCodexUploadRejectsIdentityChangedByRefresh(t *testing.T) {
	transport := proxyRoundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body: io.NopCloser(strings.NewReader(fmt.Sprintf(
				`{"access_token":%q,"refresh_token":"server-refresh","id_token":%q}`,
				proxyTestCodexJWT("other@example.com", "access-token", time.Now().Add(time.Hour)),
				proxyTestCodexJWT("other@example.com", "id-token", time.Now().Add(time.Hour)),
			))),
		}, nil
	})
	registry, handler, _ := newMultiTenantFixtureWithTransport(t, "", transport)
	created, key, err := registry.Create("team")
	if err != nil {
		t.Fatal(err)
	}
	submittedID := proxyTestCodexJWT("owner@example.com", "submitted-id", time.Now().Add(time.Hour))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(
		http.MethodPost,
		"/t/"+key+"/_subrouter/accounts",
		strings.NewReader(fmt.Sprintf(`{
			"provider":"codex","accountId":"stable-routing-id","label":"Production Codex",
			"tokens":{"accessToken":"access","refreshToken":"refresh","idToken":%q}
		}`, submittedID)),
	))
	if response.Code != http.StatusConflict {
		t.Fatalf("changed identity status = %d, want 409, body = %s", response.Code, response.Body.String())
	}
	stored, err := (accounts.CodexStore{
		Dir: filepath.Join(registry.Dir(created.ID), "codex", "accounts"),
	}).ListStored()
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) != 0 {
		t.Fatalf("identity-changing refresh stored an account: %#v", stored)
	}
}

func TestTenantAccountTextRejectsControlsAndUsesUTF8ByteLimits(t *testing.T) {
	base := tenantAccountUpload{
		Provider:  "openai-apikey",
		AccountID: "legacy-account",
		Label:     "Shared account",
		APIKey:    "sk-test",
	}
	for name, mutate := range map[string]func(*tenantAccountUpload){
		"label control": func(input *tenantAccountUpload) {
			input.Label = "unsafe\x1b[31m"
		},
		"id control": func(input *tenantAccountUpload) {
			input.AccountID = "unsafe\naccount"
		},
		"label over 320 bytes": func(input *tenantAccountUpload) {
			input.Label = strings.Repeat("é", 161)
		},
		"id over 320 bytes": func(input *tenantAccountUpload) {
			input.AccountID = strings.Repeat("é", 161)
		},
	} {
		t.Run(name, func(t *testing.T) {
			input := base
			mutate(&input)
			if _, err := storedTenantMigrationAccount(input); err == nil {
				t.Fatal("unsafe migration account text was accepted")
			}
		})
	}

	boundary := base
	boundary.AccountID = strings.Repeat("é", 160)
	boundary.Label = strings.Repeat("é", 160)
	if _, err := storedTenantMigrationAccount(boundary); err != nil {
		t.Fatalf("320-byte migration text rejected: %v", err)
	}

	registry, handler, _ := newMultiTenantFixture(t)
	_, key, err := registry.Create("team")
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(map[string]string{
		"provider":  "openai-apikey",
		"accountId": "safe-account",
		"label":     "unsafe\x1b[31m",
		"apiKey":    "sk-test",
	})
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(
		response,
		httptest.NewRequest(
			http.MethodPost,
			"/t/"+key+"/_subrouter/accounts",
			strings.NewReader(string(body)),
		),
	)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("ordinary upload status = %d, body = %s", response.Code, response.Body.String())
	}
}

func TestTenantMigrationBatchStaysInactiveUntilAtomicActivation(t *testing.T) {
	registry, handler, _ := newMultiTenantFixture(t)
	_, key, err := registry.Create("team")
	if err != nil {
		t.Fatal(err)
	}
	basePath := "/t/" + key + "/_subrouter/accounts"
	request := func(path, body string) *httptest.ResponseRecorder {
		t.Helper()
		response := httptest.NewRecorder()
		handler.ServeHTTP(
			response,
			httptest.NewRequest(http.MethodPost, path, strings.NewReader(body)),
		)
		return response
	}
	stage := request(basePath+"/migration/stage", `{
		"migrationId":"legacy-team",
		"accounts":[{
			"provider":"openai-apikey",
			"accountId":"legacy-account",
			"label":"Shared account",
			"apiKey":"sk-test"
		}]
	}`)
	if stage.Code != http.StatusOK {
		t.Fatalf("stage status = %d, body = %s", stage.Code, stage.Body.String())
	}

	list := httptest.NewRecorder()
	handler.ServeHTTP(list, httptest.NewRequest(http.MethodGet, basePath, nil))
	if list.Code != http.StatusOK || strings.TrimSpace(list.Body.String()) != "[]" {
		t.Fatalf("staged account became active: %d %s", list.Code, list.Body.String())
	}

	activate := request(basePath+"/migration/activate", `{
		"migrationId":"legacy-team",
		"accountIds":["legacy-account"]
	}`)
	if activate.Code != http.StatusOK {
		t.Fatalf("activate status = %d, body = %s", activate.Code, activate.Body.String())
	}
	list = httptest.NewRecorder()
	handler.ServeHTTP(list, httptest.NewRequest(http.MethodGet, basePath, nil))
	if list.Code != http.StatusOK || !strings.Contains(list.Body.String(), "legacy-account") {
		t.Fatalf("activated account missing: %d %s", list.Code, list.Body.String())
	}

	rollback := request(basePath+"/migration/rollback", `{
		"migrationId":"legacy-team"
	}`)
	if rollback.Code != http.StatusOK {
		t.Fatalf("rollback status = %d, body = %s", rollback.Code, rollback.Body.String())
	}
	list = httptest.NewRecorder()
	handler.ServeHTTP(list, httptest.NewRequest(http.MethodGet, basePath, nil))
	if list.Code != http.StatusOK || strings.TrimSpace(list.Body.String()) != "[]" {
		t.Fatalf("rolled-back account remained active: %d %s", list.Code, list.Body.String())
	}
}

func TestTenantMigrationRejectsUnattestedCodexOAuth(t *testing.T) {
	registry, handler, _ := newMultiTenantFixture(t)
	_, key, err := registry.Create("team")
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(
		http.MethodPost,
		"/t/"+key+"/_subrouter/accounts/migration/stage",
		strings.NewReader(`{
			"migrationId":"legacy-team",
			"accounts":[{
				"provider":"codex","accountId":"legacy-account","label":"Legacy",
			"oauthCredentialOrigin":"interactive-import",
				"tokens":{"accessToken":"access","refreshToken":"refresh","idToken":"id"}
			}]
		}`),
	))
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "server-attested") {
		t.Fatalf("migration OAuth stage = %d %s", response.Code, response.Body.String())
	}
}

func TestStackLoginRejectsInvalidTokenAndMismatchedTeam(t *testing.T) {
	registry := tenant.NewRegistry(t.TempDir())
	base := Server{MaxBodyBytes: 1024}
	for name, testCase := range map[string]struct {
		verifier fakeStackVerifier
		status   int
	}{
		"invalid token": {
			verifier: fakeStackVerifier{err: errors.New("bad token")},
			status:   http.StatusUnauthorized,
		},
		"wrong team": {
			verifier: fakeStackVerifier{claims: stackauth.Claims{
				ProjectID: "project", SelectedTeamID: "other-team",
			}},
			status: http.StatusForbidden,
		},
	} {
		t.Run(name, func(t *testing.T) {
			handler := (&MultiTenant{
				Base: base, Registry: registry, StackVerifier: testCase.verifier,
				StackTeams:           fakeStackTeams{},
				StackTenantKeySecret: []byte("0123456789abcdef0123456789abcdef"),
			}).Handler(base.Handler())
			req := httptest.NewRequest(
				http.MethodPost,
				"/_subrouter/auth/stack",
				strings.NewReader(`{"teamId":"team-123"}`),
			)
			req.Header.Set("Authorization", "Bearer access")
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, req)
			if response.Code != testCase.status {
				t.Fatalf("status = %d, want %d", response.Code, testCase.status)
			}
		})
	}
}

func TestStackLoginRejectsUnsafeTeamNames(t *testing.T) {
	for name, teamName := range map[string]string{
		"oversized": strings.Repeat("x", 321),
		"control":   "Acme\nInjected",
	} {
		t.Run(name, func(t *testing.T) {
			registry := tenant.NewRegistry(t.TempDir())
			base := Server{MaxBodyBytes: 1024}
			handler := (&MultiTenant{
				Base: base, Registry: registry, PublicURL: "https://sr.example",
				StackVerifier: fakeStackVerifier{claims: stackauth.Claims{
					ProjectID: "project", SelectedTeamID: "team-123",
				}},
				StackTenantKeySecret: []byte("0123456789abcdef0123456789abcdef"),
			}).Handler(base.Handler())
			body, err := json.Marshal(map[string]string{"teamId": "team-123", "teamName": teamName})
			if err != nil {
				t.Fatal(err)
			}
			request := httptest.NewRequest(http.MethodPost, "/_subrouter/auth/stack", strings.NewReader(string(body)))
			request.Header.Set("Authorization", "Bearer access")
			response := httptest.NewRecorder()

			handler.ServeHTTP(response, request)

			if response.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400, body = %s", response.Code, response.Body.String())
			}
			tenants, err := registry.List()
			if err != nil {
				t.Fatal(err)
			}
			if len(tenants) != 0 {
				t.Fatalf("unsafe team name was persisted: %+v", tenants)
			}
		})
	}
}

func TestStackLoginAcceptsAnotherTeamAfterMembershipCheck(t *testing.T) {
	registry := tenant.NewRegistry(t.TempDir())
	base := Server{MaxBodyBytes: 1024}
	handler := (&MultiTenant{
		Base: base, Registry: registry, PublicURL: "https://sr.example",
		StackVerifier: fakeStackVerifier{claims: stackauth.Claims{
			ProjectID: "project", SelectedTeamID: "selected-team",
		}},
		StackTeams: fakeStackTeams{teams: []stackauth.Team{{
			ID: "requested-team", DisplayName: "Requested Team",
		}}},
		StackTenantKeySecret:   []byte("0123456789abcdef0123456789abcdef"),
		StackTenantDeleteToken: []byte(testStackTenantDeleteToken),
	}).Handler(base.Handler())
	req := httptest.NewRequest(
		http.MethodPost,
		"/_subrouter/auth/stack",
		strings.NewReader(`{"teamId":"requested-team","capabilities":["use"]}`),
	)
	req.Header.Set("Authorization", "Bearer access")
	req.Header.Set("X-Subrouter-Stack-Control-Token", testStackTenantDeleteToken)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, req)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", response.Code, response.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["tenantName"] != "Requested Team" {
		t.Fatalf("tenant name = %v", body["tenantName"])
	}
	expectedKey, err := tenant.DeriveKey(
		[]byte("0123456789abcdef0123456789abcdef"),
		stackTenantKeyNamespace("project", []tenant.Capability{tenant.CapabilityUse}),
		"requested-team",
	)
	if err != nil {
		t.Fatal(err)
	}
	if body["tenantKey"] != expectedKey {
		t.Fatalf("tenant key = %v, want %s", body["tenantKey"], expectedKey)
	}
}

func TestStackLoginRequiresPublicURL(t *testing.T) {
	registry := tenant.NewRegistry(t.TempDir())
	base := Server{MaxBodyBytes: 1024}
	handler := (&MultiTenant{
		Base: base, Registry: registry,
		StackVerifier: fakeStackVerifier{claims: stackauth.Claims{
			ProjectID: "project", SelectedTeamID: "team-123",
		}},
		StackTenantKeySecret:   []byte("0123456789abcdef0123456789abcdef"),
		StackTenantDeleteToken: []byte(testStackTenantDeleteToken),
	}).Handler(base.Handler())
	req := httptest.NewRequest(
		http.MethodPost,
		"/_subrouter/auth/stack",
		strings.NewReader(`{"teamId":"team-123","capabilities":["use"]}`),
	)
	req.Header.Set("Authorization", "Bearer access")
	req.Header.Set("X-Subrouter-Stack-Control-Token", testStackTenantDeleteToken)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, req)
	if response.Code != http.StatusInternalServerError ||
		!strings.Contains(response.Body.String(), "proxy URL") {
		t.Fatalf("response = %d: %s", response.Code, response.Body.String())
	}
}

func TestStackLoginReturnsUnavailableWhenMembershipLookupCannotRun(t *testing.T) {
	registry := tenant.NewRegistry(t.TempDir())
	base := Server{MaxBodyBytes: 1024}
	for name, teams := range map[string]interface {
		ListTeams(context.Context, string) ([]stackauth.Team, error)
	}{
		"missing": nil,
		"failed":  fakeStackTeams{err: errors.New("Stack unavailable")},
	} {
		t.Run(name, func(t *testing.T) {
			handler := (&MultiTenant{
				Base: base, Registry: registry, PublicURL: "https://sr.example",
				StackVerifier: fakeStackVerifier{claims: stackauth.Claims{
					ProjectID: "project", SelectedTeamID: "selected-team",
				}},
				StackTeams:           teams,
				StackTenantKeySecret: []byte("0123456789abcdef0123456789abcdef"),
			}).Handler(base.Handler())
			req := httptest.NewRequest(
				http.MethodPost,
				"/_subrouter/auth/stack",
				strings.NewReader(`{"teamId":"other-team"}`),
			)
			req.Header.Set("Authorization", "Bearer access")
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, req)
			if response.Code != http.StatusServiceUnavailable {
				t.Fatalf("response = %d: %s", response.Code, response.Body.String())
			}
		})
	}
}

func TestTenantAccountDeleteReturnsUnavailableWithoutAccountStore(t *testing.T) {
	response := httptest.NewRecorder()
	handleTenantAccountDelete(
		&Server{},
		response,
		httptest.NewRequest(http.MethodDelete, "/_subrouter/accounts/account", nil),
	)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("response = %d: %s", response.Code, response.Body.String())
	}
}

func TestTenantStoredAccountDeletePublishesToOverlappingWorker(t *testing.T) {
	root := t.TempDir()
	store := accounts.CodexStore{Dir: filepath.Join(root, "accounts")}
	stored := accounts.StoredCodexAccount{
		Email: "apikey:work", Provider: accounts.ProviderCodex,
		Auth: accounts.CodexAuthFile{AuthMode: "apikey", OpenAIAPIKey: "model-secret"},
	}
	if err := store.SaveStored(stored); err != nil {
		t.Fatal(err)
	}
	initial, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	claudeStore := agentclaude.Store{Dir: filepath.Join(root, "claude")}
	deleting := NewAccountRef(store, initial, nil)
	deleting.claudeStore = claudeStore
	sibling := NewAccountRef(store, initial, nil)
	sibling.claudeStore = claudeStore
	before := sibling.Generation()

	removed, err := removeTenantAccounts(t.Context(), deleting, stored.Email)
	if err != nil || !removed {
		t.Fatalf("delete = removed %v, err %v", removed, err)
	}
	if got := sibling.All(); len(got) != 1 || got[0].ID != stored.Email {
		t.Fatalf("sibling fixture changed before observing generation: %+v", got)
	}
	reloaded, generation, err := sibling.reloadIfDiskGenerationChanged(t.Context())
	if err != nil || !reloaded || generation <= before {
		t.Fatalf("sibling reload = reloaded %v generation %d before %d err %v", reloaded, generation, before, err)
	}
	if got := sibling.All(); len(got) != 0 {
		t.Fatalf("sibling retained deleted stored secret: %+v", got)
	}
}

func TestTenantClaudeAccountDeletePublishesToOverlappingWorker(t *testing.T) {
	root := t.TempDir()
	store := accounts.CodexStore{Dir: filepath.Join(root, "accounts")}
	claudeStore := agentclaude.Store{Dir: filepath.Join(root, "claude")}
	if _, err := claudeStore.CreateProfile("work"); err != nil {
		t.Fatal(err)
	}
	if err := claudeStore.ImportProfileCredential("work", agentclaude.CredentialInfo{
		AccessToken: "access", RefreshToken: "refresh",
	}); err != nil {
		t.Fatal(err)
	}
	initial, err := claudeStore.ListAccounts(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	deleting := NewAccountRef(store, initial, nil)
	deleting.claudeStore = claudeStore
	sibling := NewAccountRef(store, initial, nil)
	sibling.claudeStore = claudeStore
	before := sibling.Generation()

	removed, err := removeTenantAccounts(t.Context(), deleting, "work")
	if err != nil || !removed {
		t.Fatalf("delete = removed %v, err %v", removed, err)
	}
	reloaded, generation, err := sibling.reloadIfDiskGenerationChanged(t.Context())
	if err != nil || !reloaded || generation <= before {
		t.Fatalf("sibling reload = reloaded %v generation %d before %d err %v", reloaded, generation, before, err)
	}
	if got := sibling.All(); len(got) != 0 {
		t.Fatalf("sibling retained deleted Claude secret: %+v", got)
	}
}

func TestRestartCompletesCompositeClaudeAndStoredAccountDeletion(t *testing.T) {
	if root := os.Getenv("SUBROUTER_TEST_COMPOSITE_DELETE_CRASH_ROOT"); root != "" {
		store := compositeCrashAccountStore(root)
		ref := NewAccountRef(store, nil, nil)
		ref.claudeStore = agentclaude.Store{Dir: filepath.Join(root, "claude")}
		ref.beforeTenantStoredRemovalForTest = func() { os.Exit(91) }
		_, _ = removeTenantAccounts(t.Context(), ref, "work")
		os.Exit(92)
	}
	root := t.TempDir()
	store := compositeCrashAccountStore(root)
	stored := accounts.StoredCodexAccount{
		Email: "work", Provider: accounts.ProviderCodex,
		Auth: accounts.CodexAuthFile{AuthMode: "apikey", OpenAIAPIKey: "model-secret"},
	}
	if err := store.SaveStored(stored); err != nil {
		t.Fatal(err)
	}
	claudeStore := agentclaude.Store{Dir: filepath.Join(root, "claude")}
	if err := claudeStore.ImportProfileCredential("work", agentclaude.CredentialInfo{
		AccessToken: "claude-access", RefreshToken: "claude-refresh",
	}); err != nil {
		t.Fatal(err)
	}
	runCompositeDeleteCrash(t, root)

	journal, active, err := readAccountRollbackJournal(store.StoreDir())
	if err != nil || !active || journal.Target != accountRollbackTargetTenantDelete || journal.Progress != accountRollbackClaudeRemoved {
		t.Fatalf("crash journal = active %v journal %+v err %v", active, journal, err)
	}
	if _, found := claudeStore.FindProfile("work"); found {
		t.Fatal("Claude component remained after crash boundary")
	}
	if current, found, err := store.FindStored("work"); err != nil || !found || current.Auth.OpenAIAPIKey != "model-secret" {
		t.Fatalf("stored component before restart = found %v current %+v err %v", found, current, err)
	}

	restarted, err := OpenAccountRef(store, claudeStore, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := restarted.All(); len(got) != 0 {
		t.Fatalf("restarted accounts resurfaced deleted identity: %+v", got)
	}
	if _, found, err := store.FindStored("work"); err != nil || found {
		t.Fatalf("stored component after restart = found %v err %v", found, err)
	}
	if _, found := claudeStore.FindProfile("work"); found {
		t.Fatal("Claude component resurfaced after restart")
	}
	if active, err := accountRollbackActive(store.StoreDir()); err != nil || active {
		t.Fatalf("completed composite journal = active %v err %v", active, err)
	}
}

func TestCompositeDeleteHoldsStoredLeaseAcrossClaudeCommit(t *testing.T) {
	root := t.TempDir()
	store := accounts.CodexStore{Dir: filepath.Join(root, "accounts"), DisableActiveAuthSync: true}
	stored := proxyStoredOAuthAccount("work", "old", time.Now().Add(-time.Hour))
	if err := store.SaveStored(stored); err != nil {
		t.Fatal(err)
	}
	claudeStore := agentclaude.Store{Dir: filepath.Join(root, "claude")}
	if err := claudeStore.ImportProfileCredential("work", agentclaude.CredentialInfo{
		AccessToken: "claude-access", RefreshToken: "claude-refresh",
	}); err != nil {
		t.Fatal(err)
	}

	var refreshCalls atomic.Int32
	client := &http.Client{Transport: proxyRoundTripFunc(func(*http.Request) (*http.Response, error) {
		refreshCalls.Add(1)
		return nil, errors.New("refresh endpoint must not be called after deletion")
	})}
	ref := NewAccountRef(store, nil, client)
	ref.claudeStore = claudeStore
	refreshAttempted := make(chan struct{})
	refreshResult := make(chan error, 1)
	ref.afterTenantStoredRevalidateForTest = func() {
		go func() {
			close(refreshAttempted)
			_, _, err := store.RefreshStored(context.Background(), client, stored)
			refreshResult <- err
		}()
		<-refreshAttempted
		runtime.Gosched()
		select {
		case err := <-refreshResult:
			t.Fatalf("refresh crossed a held stored-account lease: %v", err)
		default:
		}
	}

	removed, err := removeTenantAccounts(t.Context(), ref, "work")
	if err != nil || !removed {
		t.Fatalf("composite delete = removed %v err %v", removed, err)
	}
	select {
	case err := <-refreshResult:
		if !errors.Is(err, accounts.ErrStoredAccountRemoved) {
			t.Fatalf("waiting refresh error = %v, want removed-account error", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("waiting refresh did not resume after composite deletion")
	}
	if refreshCalls.Load() != 0 {
		t.Fatalf("refresh endpoint calls = %d, want 0", refreshCalls.Load())
	}
	if _, found, err := store.FindStored("work"); err != nil || found {
		t.Fatalf("stored account after delete = found %v err %v", found, err)
	}
	if _, found := claudeStore.FindProfile("work"); found {
		t.Fatal("Claude profile remained after composite deletion")
	}
}

func TestCompositeDeleteRejectsRefreshThatWinsStoredLease(t *testing.T) {
	root := t.TempDir()
	store := accounts.CodexStore{Dir: filepath.Join(root, "accounts"), DisableActiveAuthSync: true}
	stored := proxyStoredOAuthAccount("work", "old", time.Now().Add(-time.Hour))
	if err := store.SaveStored(stored); err != nil {
		t.Fatal(err)
	}
	claudeStore := agentclaude.Store{Dir: filepath.Join(root, "claude")}
	if err := claudeStore.ImportProfileCredential("work", agentclaude.CredentialInfo{
		AccessToken: "claude-access", RefreshToken: "claude-refresh",
	}); err != nil {
		t.Fatal(err)
	}
	newAccess := proxyTestCodexJWT("work", "new-access", time.Now().Add(time.Hour))
	newID := proxyTestCodexJWT("work", "new-id", time.Now().Add(time.Hour))
	client := &http.Client{Transport: proxyRoundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body: io.NopCloser(strings.NewReader(fmt.Sprintf(
				`{"access_token":%q,"refresh_token":"new-refresh","id_token":%q}`, newAccess, newID,
			))),
		}, nil
	})}
	refreshHoldingLease := make(chan struct{})
	releaseRefresh := make(chan struct{})
	refreshResult := make(chan error, 1)
	go func() {
		_, _, err := store.RefreshStoredIfExpiredBeforeRefresh(context.Background(), client, stored, func() error {
			close(refreshHoldingLease)
			<-releaseRefresh
			return nil
		})
		refreshResult <- err
	}()
	<-refreshHoldingLease

	ref := NewAccountRef(store, nil, client)
	ref.claudeStore = claudeStore
	type deleteResult struct {
		removed bool
		err     error
	}
	deleteResultCh := make(chan deleteResult, 1)
	go func() {
		removed, err := removeTenantAccounts(context.Background(), ref, "work")
		deleteResultCh <- deleteResult{removed: removed, err: err}
	}()
	waitForTenantDeleteTransaction(t, ref)
	close(releaseRefresh)
	if err := <-refreshResult; err != nil {
		t.Fatalf("winning refresh = %v", err)
	}
	result := <-deleteResultCh
	if result.removed || result.err == nil || !strings.Contains(result.err.Error(), "stored account changed during removal") {
		t.Fatalf("delete after winning refresh = removed %v err %v", result.removed, result.err)
	}
	current, found, err := store.FindStored("work")
	if err != nil || !found || current.Auth.Tokens == nil || current.Auth.Tokens.RefreshToken != "new-refresh" {
		t.Fatalf("winning refresh credential = found %v account %+v err %v", found, current, err)
	}
	if _, found := claudeStore.FindProfile("work"); !found {
		t.Fatal("stale composite deletion removed Claude before rejecting refreshed stored identity")
	}
}

func TestRestartCompositeDeletePreservesChangedStoredReplacement(t *testing.T) {
	root := t.TempDir()
	store := compositeCrashAccountStore(root)
	original := accounts.StoredCodexAccount{
		Email: "work", Provider: accounts.ProviderCodex,
		Auth: accounts.CodexAuthFile{AuthMode: "apikey", OpenAIAPIKey: "old-model-secret"},
	}
	if err := store.SaveStored(original); err != nil {
		t.Fatal(err)
	}
	claudeStore := agentclaude.Store{Dir: filepath.Join(root, "claude")}
	if err := claudeStore.ImportProfileCredential("work", agentclaude.CredentialInfo{
		AccessToken: "claude-access", RefreshToken: "claude-refresh",
	}); err != nil {
		t.Fatal(err)
	}
	runCompositeDeleteCrash(t, root)
	replacement := original
	replacement.Auth.OpenAIAPIKey = "replacement-model-secret"
	if err := store.SaveStored(replacement); err != nil {
		t.Fatal(err)
	}

	restarted, err := OpenAccountRef(store, claudeStore, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := restarted.All()
	if len(got) != 1 || got[0].ID != "work" || got[0].Token != "replacement-model-secret" {
		t.Fatalf("restarted accounts = %+v, want exact stored replacement", got)
	}
	if _, found := claudeStore.FindProfile("work"); found {
		t.Fatal("removed Claude component resurfaced with stored replacement")
	}
	if active, err := accountRollbackActive(store.StoreDir()); err != nil || active {
		t.Fatalf("changed replacement journal = active %v err %v", active, err)
	}
}

func TestCompositeClaudeAndQwenAccountDelete(t *testing.T) {
	root := t.TempDir()
	store := accounts.CodexStore{Dir: filepath.Join(root, "accounts")}
	stored := accounts.StoredCodexAccount{
		Email: "work", Provider: accounts.ProviderQwenToken,
		Auth: accounts.CodexAuthFile{AuthMode: "apikey", OpenAIAPIKey: "qwen-model-secret"},
	}
	if err := store.SaveStored(stored); err != nil {
		t.Fatal(err)
	}
	claudeStore := agentclaude.Store{Dir: filepath.Join(root, "claude")}
	if err := claudeStore.ImportProfileCredential("work", agentclaude.CredentialInfo{AccessToken: "claude-access", RefreshToken: "claude-refresh"}); err != nil {
		t.Fatal(err)
	}
	ref := NewAccountRef(store, nil, nil)
	ref.claudeStore = claudeStore
	if err := agentqwen.SaveConsoleCredentialIn(ref.qwenRoot(), "work", agentqwen.ConsoleCredential{AccessToken: "qwen-console-secret"}); err != nil {
		t.Fatal(err)
	}
	removed, err := removeTenantAccounts(t.Context(), ref, "work")
	if err != nil || !removed {
		t.Fatalf("composite Qwen delete = removed %v err %v", removed, err)
	}
	if _, found, err := store.FindStored("work"); err != nil || found {
		t.Fatalf("Qwen stored component = found %v err %v", found, err)
	}
	if has, err := agentqwen.HasConsoleCredentialIn(ref.qwenRoot(), "work"); err != nil || has {
		t.Fatalf("Qwen console component = found %v err %v", has, err)
	}
	if _, found := claudeStore.FindProfile("work"); found {
		t.Fatal("Claude component remained after composite Qwen delete")
	}
}

func TestRestartReconcilesStandaloneQwenRemovalCrash(t *testing.T) {
	if root := os.Getenv("SUBROUTER_TEST_QWEN_DELETE_CRASH_ROOT"); root != "" {
		store := accounts.CodexStore{Dir: filepath.Join(root, "accounts")}
		accountID := "qwen-token:work"
		found, version, err := agentqwen.ConsoleCredentialVersionIn(agentqwen.ConsoleRootForStore(store), accountID)
		if err != nil || !found {
			os.Exit(93)
		}
		phase := os.Getenv("SUBROUTER_TEST_QWEN_DELETE_CRASH_PHASE")
		_, _ = agentqwen.RemoveConsoleCredentialExactIn(
			agentqwen.ConsoleRootForStore(store), accountID, true, version,
			func() (bool, error) {
				if phase == "after-model" {
					stored, found, err := store.FindStored(accountID)
					if err != nil || !found {
						os.Exit(94)
					}
					if _, removed, err := store.RemoveStoredExactDurable(stored, syncAccountStateDir); err != nil || !removed {
						os.Exit(95)
					}
					os.Exit(97)
				}
				os.Exit(96)
				return false, nil
			},
		)
		os.Exit(98)
	}

	for _, phase := range []string{"before-model", "after-model"} {
		t.Run(phase, func(t *testing.T) {
			root := t.TempDir()
			store := accounts.CodexStore{Dir: filepath.Join(root, "accounts")}
			stored := accounts.StoredCodexAccount{
				Email: "qwen-token:work", Provider: accounts.ProviderQwenToken,
				Auth: accounts.CodexAuthFile{AuthMode: "apikey", OpenAIAPIKey: "model-secret"},
			}
			if err := store.SaveStored(stored); err != nil {
				t.Fatal(err)
			}
			qwenRoot := agentqwen.ConsoleRootForStore(store)
			if err := agentqwen.SaveConsoleCredentialIn(qwenRoot, stored.Email, agentqwen.ConsoleCredential{AccessToken: "console-secret"}); err != nil {
				t.Fatal(err)
			}
			runStandaloneQwenDeleteCrash(t, root, phase)
			if _, err := OpenAccountRef(store, agentclaude.Store{Dir: filepath.Join(root, "claude")}, nil); err != nil {
				t.Fatalf("restart reconciliation: %v", err)
			}
			_, modelFound, err := store.FindStored(stored.Email)
			if err != nil {
				t.Fatal(err)
			}
			consoleFound, _, err := agentqwen.ConsoleCredentialVersionIn(qwenRoot, stored.Email)
			if err != nil {
				t.Fatal(err)
			}
			wantFound := phase == "before-model"
			if modelFound != wantFound || consoleFound != wantFound {
				t.Fatalf("reconciled state = model %v console %v, want both %v", modelFound, consoleFound, wantFound)
			}
		})
	}
}

func TestCompositeQwenReplayFinalizesConsoleStageAfterModelCrash(t *testing.T) {
	root := t.TempDir()
	store := accounts.CodexStore{Dir: filepath.Join(root, "accounts")}
	stored := accounts.StoredCodexAccount{
		Email: "qwen-token:work", Provider: accounts.ProviderQwenToken,
		Auth: accounts.CodexAuthFile{AuthMode: "apikey", OpenAIAPIKey: "model-secret"},
	}
	if err := store.SaveStored(stored); err != nil {
		t.Fatal(err)
	}
	stored, _, _ = store.FindStored(stored.Email)
	storedVersion := storedAccountMutationVersion(stored)
	qwenRoot := agentqwen.ConsoleRootForStore(store)
	if err := agentqwen.SaveConsoleCredentialIn(qwenRoot, stored.Email, agentqwen.ConsoleCredential{AccessToken: "console-secret"}); err != nil {
		t.Fatal(err)
	}
	consoleFound, consoleVersion, err := agentqwen.ConsoleCredentialVersionIn(qwenRoot, stored.Email)
	if err != nil || !consoleFound {
		t.Fatalf("console version = found %v version %q err %v", consoleFound, consoleVersion, err)
	}
	runStandaloneQwenDeleteCrash(t, root, "after-model")

	journal := accountRollbackJournal{
		StoredTargetID: stored.Email, StoredStoreDir: store.Dir,
		StoredProvider: string(accounts.ProviderQwenToken), StoredVersion: hex.EncodeToString(storedVersion[:]),
		QwenConsoleRoot: qwenRoot, QwenConsoleFound: consoleFound, QwenConsoleVersion: consoleVersion,
	}
	lease, err := store.AcquireStoredAccountLease(stored.Email)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := lease.Close(); err != nil {
			t.Errorf("close stored-account lease: %v", err)
		}
	}()
	removed, err := replayPreparedTenantStoredRemovalWithLease(journal, lease)
	if err != nil || !removed {
		t.Fatalf("composite replay = removed %v err %v", removed, err)
	}
	if found, _, err := agentqwen.ConsoleCredentialVersionIn(qwenRoot, stored.Email); err != nil || found {
		t.Fatalf("console after composite replay = found %v err %v", found, err)
	}
}

func runStandaloneQwenDeleteCrash(t *testing.T, root, phase string) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestRestartReconcilesStandaloneQwenRemovalCrash$")
	cmd.Env = append(os.Environ(),
		"SUBROUTER_TEST_QWEN_DELETE_CRASH_ROOT="+root,
		"SUBROUTER_TEST_QWEN_DELETE_CRASH_PHASE="+phase,
	)
	runErr := cmd.Run()
	wantExit := 96
	if phase == "after-model" {
		wantExit = 97
	}
	if exitErr, ok := runErr.(*exec.ExitError); !ok || exitErr.ExitCode() != wantExit {
		t.Fatalf("crash helper = %v, want exit %d", runErr, wantExit)
	}
}

func runCompositeDeleteCrash(t *testing.T, root string) {
	t.Helper()
	command := exec.Command(os.Args[0], "-test.run=^TestRestartCompletesCompositeClaudeAndStoredAccountDeletion$")
	command.Env = append(os.Environ(), "SUBROUTER_TEST_COMPOSITE_DELETE_CRASH_ROOT="+root)
	if err := command.Run(); err == nil {
		t.Fatal("crash helper unexpectedly completed")
	} else if exitErr, ok := err.(*exec.ExitError); !ok || exitErr.ExitCode() != 91 {
		t.Fatalf("crash helper = %v, want exit 91", err)
	}
}

func compositeCrashAccountStore(root string) accounts.CodexStore {
	return accounts.CodexStore{Dir: filepath.Join(root, "tenant-state", "nonstandard-credentials")}
}

func TestTenantQwenAccountDeletePublishesToOverlappingWorker(t *testing.T) {
	root := t.TempDir()
	store := accounts.CodexStore{Dir: filepath.Join(root, "accounts")}
	stored := accounts.StoredCodexAccount{
		Email: "qwen-token:work", Provider: accounts.ProviderQwenToken,
		Auth: accounts.CodexAuthFile{AuthMode: "apikey", OpenAIAPIKey: "model-secret"},
	}
	if err := store.SaveStored(stored); err != nil {
		t.Fatal(err)
	}
	initial, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	claudeStore := agentclaude.Store{Dir: filepath.Join(root, "claude")}
	deleting := NewAccountRef(store, initial, nil)
	deleting.claudeStore = claudeStore
	sibling := NewAccountRef(store, initial, nil)
	sibling.claudeStore = claudeStore
	if err := agentqwen.SaveConsoleCredentialIn(deleting.qwenRoot(), stored.Email, agentqwen.ConsoleCredential{AccessToken: "console-secret"}); err != nil {
		t.Fatal(err)
	}
	before := sibling.Generation()

	removed, err := removeTenantAccounts(t.Context(), deleting, stored.Email)
	if err != nil || !removed {
		t.Fatalf("delete = removed %v, err %v", removed, err)
	}
	if has, err := agentqwen.HasConsoleCredentialIn(deleting.qwenRoot(), stored.Email); err != nil || has {
		t.Fatalf("deleted Qwen console credential = has %v, err %v", has, err)
	}
	reloaded, generation, err := sibling.reloadIfDiskGenerationChanged(t.Context())
	if err != nil || !reloaded || generation <= before {
		t.Fatalf("sibling reload = reloaded %v generation %d before %d err %v", reloaded, generation, before, err)
	}
	if got := sibling.All(); len(got) != 0 {
		t.Fatalf("sibling retained deleted Qwen secret: %+v", got)
	}
}

func TestTenantQwenAccountDeleteRemovesInterruptedTokenlessConsoleLogin(t *testing.T) {
	root := t.TempDir()
	store := accounts.CodexStore{Dir: filepath.Join(root, "accounts")}
	stored := accounts.StoredCodexAccount{
		Email: "qwen-token:interrupted", Provider: accounts.ProviderQwenToken,
		Auth: accounts.CodexAuthFile{AuthMode: "apikey", OpenAIAPIKey: "model-secret"},
	}
	if err := store.SaveStored(stored); err != nil {
		t.Fatal(err)
	}
	ref := NewAccountRef(store, nil, nil)
	if err := agentqwen.SaveConsoleCredentialIn(ref.qwenRoot(), stored.Email, agentqwen.ConsoleCredential{AccessToken: "old-console-token"}); err != nil {
		t.Fatal(err)
	}
	if err := agentqwen.PrepareConsoleLoginIn(ref.qwenRoot(), stored.Email, "temporary-model-api-key", "https://example.test/v1"); err != nil {
		t.Fatal(err)
	}

	removed, err := removeTenantAccounts(t.Context(), ref, stored.Email)
	if err != nil || !removed {
		t.Fatalf("interrupted login delete = removed %v, err %v", removed, err)
	}
	if _, found, err := store.FindStored(stored.Email); err != nil || found {
		t.Fatalf("interrupted login retained model account: found %v err %v", found, err)
	}
	if _, err := os.Lstat(agentqwen.ConsoleConfigDirIn(ref.qwenRoot(), stored.Email)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("interrupted login retained console secrets: %v", err)
	}
}

func TestTenantStoredDeleteRestoresExactStateAfterPostRenameSyncFailure(t *testing.T) {
	want := errors.New("post-rename directory sync failed")
	originalSync := syncTenantStoredAccountDir
	calls := 0
	syncTenantStoredAccountDir = func(path string) error {
		calls++
		if calls == 1 {
			return want
		}
		return originalSync(path)
	}
	t.Cleanup(func() { syncTenantStoredAccountDir = originalSync })

	for _, provider := range []accounts.Provider{accounts.ProviderCodex, accounts.ProviderQwenToken} {
		t.Run(string(provider), func(t *testing.T) {
			calls = 0
			root := t.TempDir()
			store := accounts.CodexStore{Dir: filepath.Join(root, "accounts")}
			id := "apikey:work"
			if provider == accounts.ProviderQwenToken {
				id = "qwen-token:work"
			}
			stored := accounts.StoredCodexAccount{Email: id, Provider: provider, Auth: accounts.CodexAuthFile{AuthMode: "apikey", OpenAIAPIKey: "model-secret"}}
			if err := store.SaveStored(stored); err != nil {
				t.Fatal(err)
			}
			ref := NewAccountRef(store, nil, nil)
			if provider == accounts.ProviderQwenToken {
				if err := agentqwen.SaveConsoleCredentialIn(ref.qwenRoot(), id, agentqwen.ConsoleCredential{AccessToken: "console-secret"}); err != nil {
					t.Fatal(err)
				}
			}
			removed, err := removeTenantAccounts(t.Context(), ref, id)
			if removed || !errors.Is(err, want) {
				t.Fatalf("post-rename sync failure = removed %v err %v", removed, err)
			}
			got, found, readErr := store.FindStored(id)
			if readErr != nil || !found || got.Auth.OpenAIAPIKey != "model-secret" {
				t.Fatalf("restored model credential = found %v got %+v err %v", found, got, readErr)
			}
			if provider == accounts.ProviderQwenToken {
				credential, readErr := agentqwen.ExportConsoleCredentialIn(ref.qwenRoot(), id)
				if readErr != nil || credential.AccessToken != "console-secret" {
					t.Fatalf("restored Qwen credential = %+v err %v", credential, readErr)
				}
			}
		})
	}
}

func TestRestartReconcilesStagedStoredDeletionWithoutOverwritingSibling(t *testing.T) {
	for _, provider := range []accounts.Provider{accounts.ProviderCodex, accounts.ProviderQwenToken} {
		t.Run(string(provider), func(t *testing.T) {
			root := t.TempDir()
			store := accounts.CodexStore{Dir: filepath.Join(root, "accounts")}
			id, filename := "apikey:work", "apikey_work.json"
			if provider == accounts.ProviderQwenToken {
				id, filename = "qwen-token:work", "qwen-token_work.json"
			}
			target := accounts.StoredCodexAccount{Email: id, Provider: provider, Auth: accounts.CodexAuthFile{AuthMode: "apikey", OpenAIAPIKey: "target-secret"}}
			sibling := accounts.StoredCodexAccount{Email: "apikey:sibling", Provider: accounts.ProviderCodex, Auth: accounts.CodexAuthFile{AuthMode: "apikey", OpenAIAPIKey: "sibling-secret"}}
			if err := store.SaveStored(target); err != nil {
				t.Fatal(err)
			}
			if err := store.SaveStored(sibling); err != nil {
				t.Fatal(err)
			}
			targetPath := filepath.Join(store.Dir, filename)
			if err := os.Rename(targetPath, targetPath+".delete-staged"); err != nil {
				t.Fatal(err)
			}
			if _, err := reconcileCompletedAccountRollback(t.Context(), store, advanceAccountDiskGeneration); err != nil {
				t.Fatal(err)
			}
			if _, found, err := store.FindStored(target.Email); err != nil || found {
				t.Fatalf("target after restart = found %v err %v", found, err)
			}
			got, found, err := store.FindStored(sibling.Email)
			if err != nil || !found || got.Auth.OpenAIAPIKey != "sibling-secret" {
				t.Fatalf("sibling after restart = found %v got %+v err %v", found, got, err)
			}
		})
	}
}

func TestOpenAccountRefReconcilesStandaloneStoredDeleteCrashInCustomAccountDir(t *testing.T) {
	const crashRootEnv = "SUBROUTER_TEST_STANDALONE_STORED_DELETE_CRASH_ROOT"
	if root := os.Getenv(crashRootEnv); root != "" {
		store := accounts.CodexStore{Dir: filepath.Join(root, "tenant-state", "nonstandard-credentials")}
		ref := NewAccountRef(store, nil, nil)
		syncTenantStoredAccountDir = func(string) error { os.Exit(93); return nil }
		_, _ = removeTenantAccounts(t.Context(), ref, "apikey:work")
		os.Exit(94)
	}

	root := t.TempDir()
	store := accounts.CodexStore{Dir: filepath.Join(root, "tenant-state", "nonstandard-credentials")}
	stored := accounts.StoredCodexAccount{
		Email: "apikey:work", Provider: accounts.ProviderCodex,
		Auth: accounts.CodexAuthFile{AuthMode: "apikey", OpenAIAPIKey: "must-delete"},
	}
	if err := store.SaveStored(stored); err != nil {
		t.Fatal(err)
	}
	command := exec.Command(os.Args[0], "-test.run=^TestOpenAccountRefReconcilesStandaloneStoredDeleteCrashInCustomAccountDir$")
	command.Env = append(os.Environ(), crashRootEnv+"="+root)
	err := command.Run()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 93 {
		t.Fatalf("delete crash subprocess = %v, want exit 93", err)
	}
	stages, err := filepath.Glob(filepath.Join(store.Dir, "*.json.delete-staged"))
	if err != nil || len(stages) != 1 {
		t.Fatalf("crash stage = %v err %v, want one staged secret", stages, err)
	}
	if _, found, err := store.FindStored(stored.Email); err != nil || found {
		t.Fatalf("live account after crash = found %v err %v", found, err)
	}

	restarted, err := OpenAccountRef(store, agentclaude.Store{Dir: filepath.Join(root, "claude")}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := restarted.All(); len(got) != 0 {
		t.Fatalf("restart resurfaced staged account: %+v", got)
	}
	stages, err = filepath.Glob(filepath.Join(store.Dir, "*.json.delete-staged"))
	if err != nil || len(stages) != 0 {
		t.Fatalf("restart retained staged secret %v err %v", stages, err)
	}
}

func TestTenantAccountDeletePublicationFailurePreventsCredentialMutation(t *testing.T) {
	want := errors.New("generation publication unavailable")

	t.Run("stored API key", func(t *testing.T) {
		root := t.TempDir()
		store := accounts.CodexStore{Dir: filepath.Join(root, "accounts")}
		stored := accounts.StoredCodexAccount{
			Email: "apikey:work", Provider: accounts.ProviderCodex,
			Auth: accounts.CodexAuthFile{AuthMode: "apikey", OpenAIAPIKey: "model-secret"},
		}
		if err := store.SaveStored(stored); err != nil {
			t.Fatal(err)
		}
		ref := NewAccountRef(store, nil, nil)
		ref.claudeStore = agentclaude.Store{Dir: filepath.Join(root, "claude")}
		ref.publishGenerationForTest = func(string) error { return want }
		removed, err := removeTenantAccounts(t.Context(), ref, stored.Email)
		if removed || !errors.Is(err, want) {
			t.Fatalf("delete = removed %v, err %v", removed, err)
		}
		if current, found, err := store.FindStored(stored.Email); err != nil || !found || current.Auth.OpenAIAPIKey != "model-secret" {
			t.Fatalf("publication failure changed stored credential: found=%v err=%v account=%+v", found, err, current)
		}
	})

	t.Run("Claude profile", func(t *testing.T) {
		root := t.TempDir()
		store := accounts.CodexStore{Dir: filepath.Join(root, "accounts")}
		claudeStore := agentclaude.Store{Dir: filepath.Join(root, "claude")}
		if _, err := claudeStore.CreateProfile("work"); err != nil {
			t.Fatal(err)
		}
		if err := claudeStore.ImportProfileCredential("work", agentclaude.CredentialInfo{
			AccessToken: "access", RefreshToken: "refresh",
		}); err != nil {
			t.Fatal(err)
		}
		ref := NewAccountRef(store, nil, nil)
		ref.claudeStore = claudeStore
		ref.publishGenerationForTest = func(string) error { return want }
		removed, err := removeTenantAccounts(t.Context(), ref, "work")
		if removed || !errors.Is(err, want) {
			t.Fatalf("delete = removed %v, err %v", removed, err)
		}
		if _, found := claudeStore.FindProfile("work"); !found {
			t.Fatal("publication failure removed Claude profile")
		}
		credential, err := claudeStore.ReadCredential(t.Context(), claudeStore.ClaudeConfigDir("work"))
		if err != nil || credential.AccessToken != "access" {
			t.Fatalf("publication failure changed Claude credential: credential=%+v err=%v", credential, err)
		}
	})

	t.Run("Qwen model and console credentials", func(t *testing.T) {
		root := t.TempDir()
		store := accounts.CodexStore{Dir: filepath.Join(root, "accounts")}
		stored := accounts.StoredCodexAccount{
			Email: "qwen-token:work", Provider: accounts.ProviderQwenToken,
			Auth: accounts.CodexAuthFile{AuthMode: "apikey", OpenAIAPIKey: "model-secret"},
		}
		if err := store.SaveStored(stored); err != nil {
			t.Fatal(err)
		}
		ref := NewAccountRef(store, nil, nil)
		ref.claudeStore = agentclaude.Store{Dir: filepath.Join(root, "claude")}
		ref.publishGenerationForTest = func(string) error { return want }
		if err := agentqwen.SaveConsoleCredentialIn(ref.qwenRoot(), stored.Email, agentqwen.ConsoleCredential{AccessToken: "console-secret"}); err != nil {
			t.Fatal(err)
		}
		removed, err := removeTenantAccounts(t.Context(), ref, stored.Email)
		if removed || !errors.Is(err, want) {
			t.Fatalf("delete = removed %v, err %v", removed, err)
		}
		if _, found, err := store.FindStored(stored.Email); err != nil || !found {
			t.Fatalf("publication failure removed Qwen model credential: found=%v err=%v", found, err)
		}
		if has, err := agentqwen.HasConsoleCredentialIn(ref.qwenRoot(), stored.Email); err != nil || !has {
			t.Fatalf("publication failure removed Qwen console credential: has=%v err=%v", has, err)
		}
	})
}

func TestTenantQwenDeleteKeepsAccountWhenCredentialCleanupFails(t *testing.T) {
	store := accounts.CodexStore{Dir: filepath.Join(t.TempDir(), "accounts")}
	stored := accounts.StoredCodexAccount{
		Email:    "qwen-token:work",
		Provider: accounts.ProviderQwenToken,
		Auth: accounts.CodexAuthFile{
			AuthMode:     "apikey",
			OpenAIAPIKey: "model-secret",
		},
	}
	if err := store.SaveStored(stored); err != nil {
		t.Fatal(err)
	}
	stored, _, err := store.FindStored(stored.Email)
	if err != nil {
		t.Fatal(err)
	}
	ref := NewAccountRef(store, nil, nil)
	if err := os.MkdirAll(ref.qwenRoot(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), agentqwen.ConsoleConfigDirIn(ref.qwenRoot(), stored.Email)); err != nil {
		t.Fatal(err)
	}

	response := httptest.NewRecorder()
	handleTenantAccountDelete(
		&Server{AccountRef: ref},
		response,
		httptest.NewRequest(http.MethodDelete, "/_subrouter/accounts/"+stored.Email, nil),
	)
	if response.Code != http.StatusInternalServerError {
		t.Fatalf("response = %d: %s", response.Code, response.Body.String())
	}
	if _, ok, err := store.FindStored(stored.Email); err != nil || !ok {
		t.Fatalf("Qwen account changed before failed cleanup: ok=%v err=%v", ok, err)
	}
}

func TestTenantQwenDeleteRemovesConsoleBeforeAccountAndRestoresOnFailure(t *testing.T) {
	var operations []string
	wantErr := errors.New("account store unavailable")
	removed, err := removeTenantQwenAccountWithOps(
		true,
		func() error {
			operations = append(operations, "remove console")
			return nil
		},
		func() (bool, error) {
			operations = append(operations, "remove account")
			return false, wantErr
		},
		func() error {
			operations = append(operations, "restore console")
			return nil
		},
	)
	if removed || !errors.Is(err, wantErr) {
		t.Fatalf("result = removed %v, err %v", removed, err)
	}
	want := []string{"remove console", "remove account", "restore console"}
	if !reflect.DeepEqual(operations, want) {
		t.Fatalf("operations = %v, want %v", operations, want)
	}
}

func TestTenantQwenConsoleRestoreSyncsProfileAndContainingDirectory(t *testing.T) {
	root := t.TempDir()
	accountID := "qwen-token:work"
	credential := agentqwen.ConsoleCredential{AccessToken: "console-secret", Account: "owner@example.com"}
	var synced []string
	if err := restoreTenantQwenConsoleDurably(root, accountID, credential, func(path string) error {
		synced = append(synced, path)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	got, err := agentqwen.ExportConsoleCredentialIn(root, accountID)
	if err != nil {
		t.Fatal(err)
	}
	if got.AccessToken != credential.AccessToken || got.Account != credential.Account {
		t.Fatalf("restored credential = %+v", got)
	}
	consoleDir := agentqwen.ConsoleConfigDirIn(root, accountID)
	want := []string{consoleDir, filepath.Dir(consoleDir)}
	if !reflect.DeepEqual(synced, want) {
		t.Fatalf("directory sync boundary = %v, want %v", synced, want)
	}
}

func TestTenantQwenDeleteWaitsForAccountMutationTransaction(t *testing.T) {
	root := t.TempDir()
	store := accounts.CodexStore{Dir: filepath.Join(root, "accounts")}
	stored := accounts.StoredCodexAccount{
		Email: "qwen-token:work", Provider: accounts.ProviderQwenToken,
		Auth: accounts.CodexAuthFile{AuthMode: "apikey", OpenAIAPIKey: "model-secret"},
	}
	if err := store.SaveStored(stored); err != nil {
		t.Fatal(err)
	}
	stored, _, err := store.FindStored(stored.Email)
	if err != nil {
		t.Fatal(err)
	}
	ref := NewAccountRef(store, nil, nil)
	if err := agentqwen.SaveConsoleCredentialIn(ref.qwenRoot(), stored.Email, agentqwen.ConsoleCredential{AccessToken: "console-secret"}); err != nil {
		t.Fatal(err)
	}
	held, err := lockAccountImportTransaction(context.Background(), store.StoreDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	removed, removeErr := removeTenantAccounts(ctx, ref, stored.Email)
	if removed || !errors.Is(removeErr, context.DeadlineExceeded) {
		t.Fatalf("blocked delete = removed %v, err %v", removed, removeErr)
	}
	if err := held.Close(); err != nil {
		t.Fatal(err)
	}
	if _, found, err := store.FindStored(stored.Email); err != nil || !found {
		t.Fatalf("blocked delete changed account: found=%v err=%v", found, err)
	}
	if has, err := agentqwen.HasConsoleCredentialIn(ref.qwenRoot(), stored.Email); err != nil || !has {
		t.Fatalf("blocked delete changed console credential: has=%v err=%v", has, err)
	}
	current, found, currentErr := store.FindStored(stored.Email)
	if currentErr != nil || !found || current.Email != stored.Email ||
		storedAccountMutationVersion(current) != storedAccountMutationVersion(stored) {
		t.Fatalf("blocked delete changed durable version: found=%v err=%v current=%+v expected=%+v", found, currentErr, current, stored)
	}
	removed, removeErr = removeTenantAccounts(context.Background(), ref, stored.Email)
	if !removed || removeErr != nil {
		t.Fatalf("delete after transaction release = removed %v, err %v", removed, removeErr)
	}
	if _, found, err := store.FindStored(stored.Email); err != nil || found {
		t.Fatalf("released delete retained account: found=%v err=%v", found, err)
	}
	if has, err := agentqwen.HasConsoleCredentialIn(ref.qwenRoot(), stored.Email); err != nil || has {
		t.Fatalf("released delete retained console credential: has=%v err=%v", has, err)
	}
}

func TestTenantQwenDeleteRejectsCredentialVersionChangeBeforeMutation(t *testing.T) {
	store := accounts.CodexStore{Dir: filepath.Join(t.TempDir(), "accounts")}
	original := accounts.StoredCodexAccount{
		Email: "qwen-token:work", Provider: accounts.ProviderQwenToken,
		Auth: accounts.CodexAuthFile{AuthMode: "apikey", OpenAIAPIKey: "old-model-secret"},
	}
	if err := store.SaveStored(original); err != nil {
		t.Fatal(err)
	}
	original, _, err := store.FindStored(original.Email)
	if err != nil {
		t.Fatal(err)
	}
	ref := NewAccountRef(store, nil, nil)
	if err := agentqwen.SaveConsoleCredentialIn(ref.qwenRoot(), original.Email, agentqwen.ConsoleCredential{AccessToken: "console-secret"}); err != nil {
		t.Fatal(err)
	}
	held, err := lockAccountImportTransaction(context.Background(), store.StoreDir())
	if err != nil {
		t.Fatal(err)
	}
	type deleteResult struct {
		removed bool
		err     error
	}
	resultCh := make(chan deleteResult, 1)
	go func() {
		removed, err := removeTenantAccounts(context.Background(), ref, original.Email)
		resultCh <- deleteResult{removed: removed, err: err}
	}()
	deadline := time.Now().Add(5 * time.Second)
	for ref.installMu.TryLock() {
		ref.installMu.Unlock()
		if time.Now().After(deadline) {
			t.Fatal("delete did not reach the account transaction lock")
		}
		runtime.Gosched()
	}
	replacement := original
	replacement.Auth.OpenAIAPIKey = "replacement-model-secret"
	if err := store.SaveStored(replacement); err != nil {
		t.Fatal(err)
	}
	if err := held.Close(); err != nil {
		t.Fatal(err)
	}
	deleteResultValue := <-resultCh
	removed, err := deleteResultValue.removed, deleteResultValue.err
	if removed || err == nil || !strings.Contains(err.Error(), "changed during removal") {
		t.Fatalf("stale delete = removed %v, err %v", removed, err)
	}
	stored, found, err := store.FindStored(original.Email)
	if err != nil || !found || stored.Auth.OpenAIAPIKey != replacement.Auth.OpenAIAPIKey {
		t.Fatalf("stale delete changed replacement: found=%v err=%v account=%+v", found, err, stored)
	}
	if has, err := agentqwen.HasConsoleCredentialIn(ref.qwenRoot(), original.Email); err != nil || !has {
		t.Fatalf("stale delete removed console credential: has=%v err=%v", has, err)
	}
}

func TestTenantStoredDeleteRejectsCredentialVersionChangeBeforeMutation(t *testing.T) {
	store := accounts.CodexStore{Dir: filepath.Join(t.TempDir(), "accounts")}
	original := accounts.StoredCodexAccount{
		Email: "apikey:work", Provider: accounts.ProviderCodex,
		Auth: accounts.CodexAuthFile{AuthMode: "apikey", OpenAIAPIKey: "old-secret"},
	}
	if err := store.SaveStored(original); err != nil {
		t.Fatal(err)
	}
	ref := NewAccountRef(store, nil, nil)
	ref.claudeStore = agentclaude.Store{Dir: filepath.Join(t.TempDir(), "claude")}
	held, err := lockAccountImportTransaction(t.Context(), store.StoreDir())
	if err != nil {
		t.Fatal(err)
	}
	type deleteResult struct {
		removed bool
		err     error
	}
	resultCh := make(chan deleteResult, 1)
	go func() {
		removed, err := removeTenantAccounts(context.Background(), ref, original.Email)
		resultCh <- deleteResult{removed: removed, err: err}
	}()
	waitForTenantDeleteTransaction(t, ref)
	replacement := original
	replacement.Auth.OpenAIAPIKey = "replacement-secret"
	if err := store.SaveStored(replacement); err != nil {
		t.Fatal(err)
	}
	if err := held.Close(); err != nil {
		t.Fatal(err)
	}
	result := <-resultCh
	if result.removed || result.err == nil || !strings.Contains(result.err.Error(), "changed during removal") {
		t.Fatalf("stale delete = removed %v, err %v", result.removed, result.err)
	}
	stored, found, err := store.FindStored(original.Email)
	if err != nil || !found || stored.Auth.OpenAIAPIKey != replacement.Auth.OpenAIAPIKey {
		t.Fatalf("stale delete changed replacement: found=%v err=%v account=%+v", found, err, stored)
	}
}

func TestTenantStoredDeleteRejectsRepairAfterTransactionRevalidation(t *testing.T) {
	store := accounts.CodexStore{Dir: filepath.Join(t.TempDir(), "accounts")}
	original := accounts.StoredCodexAccount{
		Email: "apikey:work", Provider: accounts.ProviderCodex,
		Auth: accounts.CodexAuthFile{AuthMode: "apikey", OpenAIAPIKey: "old-secret"},
	}
	if err := store.SaveStored(original); err != nil {
		t.Fatal(err)
	}
	ref := NewAccountRef(store, nil, nil)
	ref.claudeStore = agentclaude.Store{Dir: filepath.Join(t.TempDir(), "claude")}
	replacement := original
	replacement.Auth.OpenAIAPIKey = "replacement-secret"
	ref.beforeTenantStoredRemovalForTest = func() {
		if err := store.SaveStored(replacement); err != nil {
			t.Fatal(err)
		}
	}
	removed, err := removeTenantAccounts(t.Context(), ref, original.Email)
	if removed || err == nil || !strings.Contains(err.Error(), "changed during removal") {
		t.Fatalf("post-revalidation repair delete = removed %v, err %v", removed, err)
	}
	stored, found, err := store.FindStored(original.Email)
	if err != nil || !found || stored.Auth.OpenAIAPIKey != replacement.Auth.OpenAIAPIKey {
		t.Fatalf("post-revalidation repair changed: found=%v err=%v account=%+v", found, err, stored)
	}
}

func TestTenantClaudeDeleteRejectsProfileIdentityChangeBeforeMutation(t *testing.T) {
	root := t.TempDir()
	store := accounts.CodexStore{Dir: filepath.Join(root, "accounts")}
	claudeStore := agentclaude.Store{Dir: filepath.Join(root, "claude")}
	if _, err := claudeStore.CreateProfile("work"); err != nil {
		t.Fatal(err)
	}
	if err := claudeStore.ImportProfileCredential("work", agentclaude.CredentialInfo{
		AccessToken: "access", RefreshToken: "refresh",
	}); err != nil {
		t.Fatal(err)
	}
	ref := NewAccountRef(store, nil, nil)
	ref.claudeStore = claudeStore
	held, err := lockAccountImportTransaction(t.Context(), store.StoreDir())
	if err != nil {
		t.Fatal(err)
	}
	type deleteResult struct {
		removed bool
		err     error
	}
	resultCh := make(chan deleteResult, 1)
	go func() {
		removed, err := removeTenantAccounts(context.Background(), ref, "work")
		resultCh <- deleteResult{removed: removed, err: err}
	}()
	waitForTenantDeleteTransaction(t, ref)
	if err := claudeStore.RegisterProfile("work", "replacement"); err != nil {
		t.Fatal(err)
	}
	if err := held.Close(); err != nil {
		t.Fatal(err)
	}
	result := <-resultCh
	if result.removed || result.err == nil || !strings.Contains(result.err.Error(), "changed during removal") {
		t.Fatalf("stale delete = removed %v, err %v", result.removed, result.err)
	}
	profile, found := claudeStore.FindProfile("work")
	if !found || profile.Dir != "replacement" {
		t.Fatalf("stale delete changed replacement profile: found=%v profile=%+v", found, profile)
	}
}

func TestTenantClaudeDeleteRejectsSameDirectoryCredentialRepair(t *testing.T) {
	root := t.TempDir()
	store := accounts.CodexStore{Dir: filepath.Join(root, "accounts")}
	claudeStore := agentclaude.Store{Dir: filepath.Join(root, "claude")}
	if err := claudeStore.ImportProfileCredential("work", agentclaude.CredentialInfo{
		AccessToken: "old-access", RefreshToken: "old-refresh",
	}); err != nil {
		t.Fatal(err)
	}
	ref := NewAccountRef(store, nil, nil)
	ref.claudeStore = claudeStore
	held, err := lockAccountImportTransaction(t.Context(), store.StoreDir())
	if err != nil {
		t.Fatal(err)
	}
	type deleteResult struct {
		removed bool
		err     error
	}
	resultCh := make(chan deleteResult, 1)
	go func() {
		removed, err := removeTenantAccounts(context.Background(), ref, "work")
		resultCh <- deleteResult{removed: removed, err: err}
	}()
	waitForTenantDeleteTransaction(t, ref)
	if err := claudeStore.ImportProfileCredential("work", agentclaude.CredentialInfo{
		AccessToken: "replacement-access", RefreshToken: "replacement-refresh",
	}); err != nil {
		t.Fatal(err)
	}
	if err := held.Close(); err != nil {
		t.Fatal(err)
	}
	result := <-resultCh
	if result.removed || result.err == nil || !strings.Contains(result.err.Error(), "changed during removal") {
		t.Fatalf("stale delete = removed %v, err %v", result.removed, result.err)
	}
	credential, err := claudeStore.ReadCredential(t.Context(), claudeStore.ClaudeConfigDir("work"))
	if err != nil || credential == nil || credential.AccessToken != "replacement-access" {
		t.Fatalf("stale delete changed replacement credential: credential=%+v err=%v", credential, err)
	}
	if active, err := accountRollbackActive(store.StoreDir()); err != nil || active {
		t.Fatalf("stale delete journal = active %v, err %v", active, err)
	}
}

func TestTenantClaudeDeleteCanJournalCorruptCredentialPayload(t *testing.T) {
	root := t.TempDir()
	store := accounts.CodexStore{Dir: filepath.Join(root, "accounts")}
	claudeStore := agentclaude.Store{Dir: filepath.Join(root, "claude")}
	instancePath, err := claudeStore.CreateProfile("work")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(instancePath, ".credentials.json"), []byte("{corrupt-credential"), 0o600); err != nil {
		t.Fatal(err)
	}
	ref := NewAccountRef(store, nil, nil)
	ref.claudeStore = claudeStore
	removed, err := removeTenantAccounts(t.Context(), ref, "work")
	if err != nil || !removed {
		t.Fatalf("delete corrupt credential = removed %v, err %v", removed, err)
	}
	if _, found := claudeStore.FindProfile("work"); found {
		t.Fatal("corrupt Claude credential profile remained registered")
	}
	if active, err := accountRollbackActive(store.StoreDir()); err != nil || active {
		t.Fatalf("completed corrupt credential journal = active %v, err %v", active, err)
	}
}

func TestTenantQwenDeleteRejectsConsoleCredentialRepair(t *testing.T) {
	root := t.TempDir()
	store := accounts.CodexStore{Dir: filepath.Join(root, "accounts")}
	stored := accounts.StoredCodexAccount{
		Email: "qwen-token:work", Provider: accounts.ProviderQwenToken,
		Auth: accounts.CodexAuthFile{AuthMode: "apikey", OpenAIAPIKey: "model-secret"},
	}
	if err := store.SaveStored(stored); err != nil {
		t.Fatal(err)
	}
	ref := NewAccountRef(store, nil, nil)
	ref.claudeStore = agentclaude.Store{Dir: filepath.Join(root, "claude")}
	if err := agentqwen.SaveConsoleCredentialIn(ref.qwenRoot(), stored.Email, agentqwen.ConsoleCredential{AccessToken: "old-console"}); err != nil {
		t.Fatal(err)
	}
	held, err := lockAccountImportTransaction(t.Context(), store.StoreDir())
	if err != nil {
		t.Fatal(err)
	}
	type deleteResult struct {
		removed bool
		err     error
	}
	resultCh := make(chan deleteResult, 1)
	go func() {
		removed, err := removeTenantAccounts(context.Background(), ref, stored.Email)
		resultCh <- deleteResult{removed: removed, err: err}
	}()
	waitForTenantDeleteTransaction(t, ref)
	if err := agentqwen.SaveConsoleCredentialIn(ref.qwenRoot(), stored.Email, agentqwen.ConsoleCredential{AccessToken: "replacement-console"}); err != nil {
		t.Fatal(err)
	}
	if err := held.Close(); err != nil {
		t.Fatal(err)
	}
	result := <-resultCh
	if result.removed || result.err == nil || !strings.Contains(result.err.Error(), "console credential changed") {
		t.Fatalf("stale delete = removed %v, err %v", result.removed, result.err)
	}
	credential, err := agentqwen.ExportConsoleCredentialIn(ref.qwenRoot(), stored.Email)
	if err != nil || credential.AccessToken != "replacement-console" {
		t.Fatalf("stale delete changed replacement console credential: credential=%+v err=%v", credential, err)
	}
	if _, found, err := store.FindStored(stored.Email); err != nil || !found {
		t.Fatalf("stale delete removed Qwen model credential: found=%v err=%v", found, err)
	}
}

func TestTenantQwenDeleteRejectsConsoleRepairAfterTransactionRevalidation(t *testing.T) {
	root := t.TempDir()
	store := accounts.CodexStore{Dir: filepath.Join(root, "accounts")}
	stored := accounts.StoredCodexAccount{
		Email: "qwen-token:work", Provider: accounts.ProviderQwenToken,
		Auth: accounts.CodexAuthFile{AuthMode: "apikey", OpenAIAPIKey: "model-secret"},
	}
	if err := store.SaveStored(stored); err != nil {
		t.Fatal(err)
	}
	ref := NewAccountRef(store, nil, nil)
	ref.claudeStore = agentclaude.Store{Dir: filepath.Join(root, "claude")}
	if err := agentqwen.SaveConsoleCredentialIn(ref.qwenRoot(), stored.Email, agentqwen.ConsoleCredential{AccessToken: "old-console"}); err != nil {
		t.Fatal(err)
	}
	ref.beforeTenantStoredRemovalForTest = func() {
		if err := agentqwen.SaveConsoleCredentialIn(ref.qwenRoot(), stored.Email, agentqwen.ConsoleCredential{AccessToken: "replacement-console"}); err != nil {
			t.Fatal(err)
		}
	}
	removed, err := removeTenantAccounts(t.Context(), ref, stored.Email)
	if removed || err == nil || !strings.Contains(err.Error(), "console credential changed") {
		t.Fatalf("post-revalidation Qwen repair delete = removed %v, err %v", removed, err)
	}
	credential, err := agentqwen.ExportConsoleCredentialIn(ref.qwenRoot(), stored.Email)
	if err != nil || credential.AccessToken != "replacement-console" {
		t.Fatalf("post-revalidation repair changed console: credential=%+v err=%v", credential, err)
	}
	if _, found, err := store.FindStored(stored.Email); err != nil || !found {
		t.Fatalf("post-revalidation repair removed model credential: found=%v err=%v", found, err)
	}
}

func waitForTenantDeleteTransaction(t *testing.T, ref *AccountRef) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for ref.installMu.TryLock() {
		ref.installMu.Unlock()
		if time.Now().After(deadline) {
			t.Fatal("delete did not reach the account transaction lock")
		}
		runtime.Gosched()
	}
}

func TestTenantClaudeDeleteReloadsCommittedRemovalWhenCleanupFails(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS Keychain is only available on Darwin")
	}
	root := t.TempDir()
	accountStore := accounts.CodexStore{Dir: filepath.Join(root, "accounts")}
	claudeStore := agentclaude.Store{Dir: filepath.Join(root, "claude")}
	instancePath, err := claudeStore.CreateProfile("work")
	if err != nil {
		t.Fatal(err)
	}
	if err := claudeStore.ImportProfileCredential("work", agentclaude.CredentialInfo{
		AccessToken: "access", RefreshToken: "refresh",
	}); err != nil {
		t.Fatal(err)
	}
	ref, err := OpenAccountRef(accountStore, claudeStore, nil)
	if err != nil {
		t.Fatal(err)
	}
	if accounts := ref.All(); len(accounts) != 1 || accounts[0].ID != "work" {
		t.Fatalf("initial accounts = %+v, want Claude work profile", accounts)
	}

	fakeBin := t.TempDir()
	securityPath := filepath.Join(fakeBin, "security")
	if err := os.WriteFile(
		securityPath,
		[]byte("#!/bin/sh\nrm -rf \"$SUBROUTER_TEST_INSTANCE_PARENT\"/.work.remove-*\necho 'forced cleanup and rollback failure' >&2\nexit 1\n"),
		0o700,
	); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SUBROUTER_TEST_INSTANCE_PARENT", filepath.Dir(instancePath))
	t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))

	response := httptest.NewRecorder()
	handleTenantAccountDelete(
		&Server{AccountRef: ref},
		response,
		httptest.NewRequest(http.MethodDelete, "/_subrouter/accounts/work", nil),
	)
	if response.Code != http.StatusInternalServerError {
		t.Fatalf("response = %d: %s", response.Code, response.Body.String())
	}
	if got := ref.All(); len(got) != 0 {
		t.Fatalf("live accounts retained durably removed Claude profile: %+v", got)
	}
	if active, err := accountRollbackActive(accountStore.StoreDir()); err != nil || !active {
		t.Fatalf("failed cleanup journal = active %v, err %v", active, err)
	}
}

func TestAccountListDoesNotRefreshOrRewriteOAuthCredentials(t *testing.T) {
	store := accounts.CodexStore{Dir: filepath.Join(t.TempDir(), "accounts")}
	stored := proxyStoredOAuthAccount(
		"broken@example.com",
		"broken",
		time.Now().Add(-time.Hour),
	)
	if err := store.SaveStored(stored); err != nil {
		t.Fatal(err)
	}
	account, ok := stored.Account(stored.SourcePath(store))
	if !ok {
		t.Fatal("stored account was not loadable")
	}
	var upstreamRequests atomic.Int32
	client := &http.Client{Transport: proxyRoundTripFunc(func(*http.Request) (*http.Response, error) {
		upstreamRequests.Add(1)
		return &http.Response{
			StatusCode: http.StatusUnauthorized,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{"error":"invalid_grant"}`)),
		}, nil
	})}
	accountRef := NewAccountRef(store, []accounts.Account{account}, client)
	accountRef.claudeStore = agentclaude.Store{Dir: filepath.Join(t.TempDir(), "claude")}
	server := Server{AccountRef: accountRef}
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(
		response,
		httptest.NewRequest(http.MethodGet, "/_subrouter/accounts", nil),
	)
	if response.Code != http.StatusOK {
		t.Fatalf("response = %d: %s", response.Code, response.Body.String())
	}
	var items []map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &items); err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("items = %#v", items)
	}
	if upstreamRequests.Load() != 0 {
		t.Fatalf("account list made %d provider request(s)", upstreamRequests.Load())
	}
	if _, ok := items[0]["health"]; ok {
		t.Fatalf("account list presented uncached health: %#v", items[0])
	}
}
