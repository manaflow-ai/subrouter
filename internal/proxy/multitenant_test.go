package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	agentclaude "github.com/manaflow-ai/subrouter/internal/agents/claude"
	"github.com/manaflow-ai/subrouter/internal/stackauth"
	"github.com/manaflow-ai/subrouter/internal/tenant"
	"github.com/manaflow-ai/subrouter/selectacct"
	"github.com/manaflow-ai/subrouter/session"
)

type fakeStackVerifier struct {
	claims stackauth.Claims
	err    error
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
			strings.NewReader(`{"teamId":"team-123","teamName":"Acme","capabilities":["manage_accounts"]}`),
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

func TestLegacyDirectStackCredentialIsRejected(t *testing.T) {
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
	handler := (&MultiTenant{
		Base: base, Registry: registry, StackProjectID: "project",
		StackTenantKeySecret: secret,
	}).Handler(base.Handler())
	response := httptest.NewRecorder()
	handler.ServeHTTP(
		response,
		httptest.NewRequest(
			http.MethodGet,
			"/t/"+legacyKey+"/_subrouter/whoami",
			nil,
		),
	)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401: %s", response.Code, response.Body.String())
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
			"provider":"codex",
			"accountId":%q,
			"label":"Shared account",
			"tokens":{
				"accessToken":"access-%s",
				"refreshToken":"refresh-%s",
				"idToken":"id-%s",
				"accountID":"provider-%s"
			}
		}`, id, id, id, id, id)
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
			"provider":"codex",
			"accountId":"legacy-account",
			"label":"Shared account",
			"tokens":{
				"accessToken":"access",
				"refreshToken":"refresh",
				"idToken":"id",
				"accountID":"provider"
			}
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
