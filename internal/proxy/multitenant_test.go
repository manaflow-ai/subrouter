package proxy

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/internal/tenant"
	"github.com/manaflow-ai/subrouter/selectacct"
	"github.com/manaflow-ai/subrouter/session"
)

// newMultiTenantFixture builds an upstream that echoes the Authorization
// header it received, a legacy single-tenant base Server with one account, and
// a MultiTenant wrapper over a fresh registry.
func newMultiTenantFixture(t *testing.T) (*tenant.Registry, http.Handler, func() string) {
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
		Sessions:     legacySessions,
		Scheduler:    selectacct.NewScheduler(nil),
		MaxBodyBytes: 1024,
	}
	registry := tenant.NewRegistry(t.TempDir())
	multi := &MultiTenant{Base: base, Registry: registry}
	return registry, multi.Handler(base.Handler()), func() string { return lastAuth }
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

func TestMultiTenantHeaderKeyFallsThroughBeforeFirstTenant(t *testing.T) {
	_, handler, _ := newMultiTenantFixture(t)
	// No tenants exist and --multi-tenant is off: a key-shaped bearer token
	// keeps legacy behavior instead of a 401.
	resp := doProxyRequest(t, handler, "/v1/responses", "s", func(r *http.Request) {
		r.Header.Set("Authorization", "Bearer srt_00000000000000000000000000000000")
	})
	if resp.Code != http.StatusOK || resp.Body.String() != "Bearer legacy-token" {
		t.Fatalf("legacy fallthrough = %d %q", resp.Code, resp.Body.String())
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
