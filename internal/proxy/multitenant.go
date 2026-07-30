package proxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	agentclaude "github.com/manaflow-ai/subrouter/internal/agents/claude"
	"github.com/manaflow-ai/subrouter/internal/tenant"
	"github.com/manaflow-ai/subrouter/internal/transcript"
	"github.com/manaflow-ai/subrouter/selectacct"
	"github.com/manaflow-ai/subrouter/session"
)

// MultiTenant routes requests carrying a tenant key to a per-tenant Server
// whose account store, scheduler, sticky sessions, and transcripts live under
// <state-dir>/tenants/<id>/. A key arrives either as a /t/<key>/... URL path
// prefix (agent CLIs can only override base URLs) or as an Authorization
// Bearer / x-api-key value (Claude Code sends ANTHROPIC_AUTH_TOKEN there).
// Requests without a tenant key fall through to the legacy single-tenant
// handler unchanged.
type MultiTenant struct {
	// Base is the template every tenant Server is copied from: upstreams,
	// transport, logger, request limits, usage-score TTL, and the shared
	// Lifecycle. Per-tenant state (accounts, sessions, scheduler, transcripts,
	// caches) is replaced per tenant.
	Base     Server
	Registry *tenant.Registry
	// TranscriptDir, when set, scopes each tenant's transcripts under
	// <TranscriptDir>/tenants/<id>.
	TranscriptDir string
	// Enabled forces tenant-key semantics for header-borne keys even before
	// the first tenant exists (the --multi-tenant serve flag). Path-borne
	// /t/<key>/ requests are always tenant-scoped.
	Enabled bool

	mu       sync.Mutex
	servers  map[string]*Server
	handlers map[string]http.Handler
}

// Handler wraps the legacy single-tenant handler with tenant routing and the
// admin tenant CRUD endpoints.
func (m *MultiTenant) Handler(fallback http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/_subrouter/tenants" || strings.HasPrefix(r.URL.Path, "/_subrouter/tenants/") {
			m.handleTenantAdmin(w, r)
			return
		}
		if key, rest, ok := splitTenantPath(r.URL.Path); ok {
			m.serveTenant(w, r, key, rest)
			return
		}
		if key := tenantKeyFromHeaders(r); key != "" {
			resolved, ok, err := m.Registry.Resolve(key)
			if err != nil {
				http.Error(w, "tenant registry error", http.StatusInternalServerError)
				return
			}
			if ok {
				m.serveResolvedTenant(w, r, resolved, r.URL.Path)
				return
			}
			// A key-shaped credential that resolves to nothing is rejected once
			// multi-tenant mode is active; before that, legacy traffic that
			// happens to carry such a token keeps today's behavior.
			if m.Enabled || m.Registry.HasTenants() {
				http.Error(w, "unknown tenant key", http.StatusUnauthorized)
				return
			}
		}
		if r.Method == http.MethodPost && r.URL.Path == "/_subrouter/reload-accounts" && isLoopbackRemote(r.RemoteAddr) {
			// The account-upload flow POSTs the global reload endpoint from
			// loopback after installing files; reload instantiated tenants too so
			// tenant uploads become visible without a restart. Gated on loopback
			// like the endpoint itself so a rejected caller triggers no work.
			m.reloadTenantAccounts(r.Context())
		}
		fallback.ServeHTTP(w, r)
	})
}

func splitTenantPath(path string) (key, rest string, ok bool) {
	const prefix = "/t/"
	if !strings.HasPrefix(path, prefix) {
		return "", "", false
	}
	remainder := path[len(prefix):]
	key = remainder
	rest = "/"
	if idx := strings.IndexByte(remainder, '/'); idx >= 0 {
		key = remainder[:idx]
		rest = remainder[idx:]
	}
	return key, rest, true
}

func tenantKeyFromHeaders(r *http.Request) string {
	if v := strings.TrimSpace(r.Header.Get("X-Api-Key")); tenant.ValidKeyFormat(v) {
		return v
	}
	auth := strings.TrimSpace(r.Header.Get("Authorization"))
	if before, after, ok := strings.Cut(auth, " "); ok && strings.EqualFold(before, "Bearer") {
		if v := strings.TrimSpace(after); tenant.ValidKeyFormat(v) {
			return v
		}
	}
	return ""
}

func (m *MultiTenant) serveTenant(w http.ResponseWriter, r *http.Request, key, rest string) {
	resolved, ok, err := m.Registry.Resolve(key)
	if err != nil {
		http.Error(w, "tenant registry error", http.StatusInternalServerError)
		return
	}
	if !ok {
		http.Error(w, "unknown tenant key", http.StatusUnauthorized)
		return
	}
	m.serveResolvedTenant(w, r, resolved, rest)
}

func (m *MultiTenant) serveResolvedTenant(w http.ResponseWriter, r *http.Request, t tenant.Tenant, path string) {
	handler, err := m.handlerFor(t)
	if err != nil {
		if m.Base.Logger != nil {
			m.Base.Logger.Error("tenant handler init failed", "tenant", t.ID, "error", err)
		}
		http.Error(w, "tenant unavailable", http.StatusInternalServerError)
		return
	}
	scoped := r.Clone(r.Context())
	scoped.URL = cloneURL(r.URL)
	scoped.URL.Path = path
	scoped.URL.RawPath = ""
	// The tenant key has served its purpose; scrub it so no downstream path
	// (Codex forwarding keeps X-Api-Key, logging, transcripts) can see it.
	stripTenantCredentialHeaders(scoped.Header)
	handler.ServeHTTP(w, scoped)
}

// stripTenantCredentialHeaders removes key-shaped tenant credentials from the
// auth headers. setAccountAuthHeaders later overwrites Authorization for
// proxied requests, but X-Api-Key passes through untouched on Codex-routed
// paths, so a tenant key parked there would leak upstream.
func stripTenantCredentialHeaders(headers http.Header) {
	if tenant.ValidKeyFormat(strings.TrimSpace(headers.Get("X-Api-Key"))) {
		headers.Del("X-Api-Key")
	}
	auth := strings.TrimSpace(headers.Get("Authorization"))
	if before, after, ok := strings.Cut(auth, " "); ok && strings.EqualFold(before, "Bearer") && tenant.ValidKeyFormat(strings.TrimSpace(after)) {
		headers.Del("Authorization")
	}
}

func (m *MultiTenant) handlerFor(t tenant.Tenant) (http.Handler, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if handler, ok := m.handlers[t.ID]; ok {
		return handler, nil
	}
	server, err := m.newTenantServer(t)
	if err != nil {
		return nil, err
	}
	handler := tenantScopedHandler(*server, t)
	if m.servers == nil {
		m.servers = map[string]*Server{}
		m.handlers = map[string]http.Handler{}
	}
	m.servers[t.ID] = server
	m.handlers[t.ID] = handler
	return handler, nil
}

// newTenantServer instantiates the existing single-tenant Server machinery
// against the tenant's own state dir, so account selection, sticky sessions,
// usage scoring, and transcripts are all scoped per tenant without threading
// tenant IDs through the proxy internals.
func (m *MultiTenant) newTenantServer(t tenant.Tenant) (*Server, error) {
	dir := m.Registry.Dir(t.ID)
	if err := os.MkdirAll(filepath.Join(dir, "codex", "accounts"), 0o700); err != nil {
		return nil, err
	}
	codexStore := accounts.CodexStore{Dir: filepath.Join(dir, "codex", "accounts")}
	claudeStore := agentclaude.Store{Dir: filepath.Join(dir, "codex")}
	sessions, err := session.NewStore(filepath.Join(dir, "sessions.json"))
	if err != nil {
		return nil, err
	}
	initial, err := codexStore.List()
	if err != nil {
		return nil, err
	}
	claudeAccounts, err := claudeStore.ListAccounts(context.Background())
	if err == nil {
		initial = append(initial, claudeAccounts...)
	} else if m.Base.Logger != nil {
		m.Base.Logger.Warn("tenant claude accounts skipped", "tenant", t.ID, "error", err)
	}
	client := &http.Client{Timeout: 15 * time.Second, Transport: m.Base.Transport}
	ref := NewAccountRef(codexStore, initial, client)
	ref.claudeStore = claudeStore

	server := m.Base
	server.Accounts = nil
	server.AccountRef = ref
	server.Sessions = sessions
	server.Scheduler = selectacct.Scheduler{}
	server.SchedulerRef = selectacct.NewSchedulerRef(selectacct.NewScheduler(tenantFallbackScores(initial)))
	server.ActiveSessions = NewActiveSessions()
	server.ReadCache = newReadCache()
	// Reaching a tenant handler already proves possession of the tenant key,
	// so the tenant-visible _subrouter read endpoints need no admin token.
	server.AdminToken = ""
	server.Transcripts = nil
	if m.TranscriptDir != "" {
		server.Transcripts = transcript.NewRecorder(filepath.Join(m.TranscriptDir, "tenants", t.ID))
	}
	return &server, nil
}

func tenantFallbackScores(available []accounts.Account) []selectacct.Score {
	scores := make([]selectacct.Score, 0, len(available))
	for _, account := range available {
		headroom := 1.0
		if account.AuthMode == accounts.AuthModeAPIKey {
			headroom = 0.01
		}
		scores = append(scores, selectacct.Score{AccountID: account.ID, Provider: account.Provider, Headroom: headroom, ShortHeadroom: headroom})
	}
	return scores
}

// tenantControlPaths are the _subrouter endpoints reachable with a tenant key.
// Everything else under _subrouter (drain, transcripts, dashboard,
// rate-limit-reset, ...) stays admin-only on the global handler.
var tenantControlPaths = map[string]bool{
	"/_subrouter/health":          true,
	"/_subrouter/accounts":        true,
	"/_subrouter/account-status":  true,
	"/_subrouter/usage-status":    true,
	"/_subrouter/sessions":        true,
	"/_subrouter/reload-accounts": true, // loopback-only inside the Server handler
}

func tenantScopedHandler(server Server, t tenant.Tenant) http.Handler {
	inner := server.Handler()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/_subrouter/whoami" {
			writeJSON(w, map[string]any{"tenant_id": t.ID, "name": t.Name})
			return
		}
		if strings.HasPrefix(r.URL.Path, "/_subrouter/") && !tenantControlPaths[r.URL.Path] {
			http.NotFound(w, r)
			return
		}
		inner.ServeHTTP(w, r)
	})
}

func (m *MultiTenant) reloadTenantAccounts(ctx context.Context) {
	m.mu.Lock()
	servers := make([]*Server, 0, len(m.servers))
	for _, server := range m.servers {
		servers = append(servers, server)
	}
	m.mu.Unlock()
	for _, server := range servers {
		if _, _, err := server.reloadAccounts(ctx); err != nil && m.Base.Logger != nil {
			m.Base.Logger.Warn("tenant account reload failed", "error", err)
		} else if server.AccountRef != nil {
			server.AccountRef.InvalidateUsageStatusCache()
		}
	}
}

type tenantKeyView struct {
	Prefix    string    `json:"prefix"`
	CreatedAt time.Time `json:"createdAt"`
}

type tenantView struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	CreatedAt time.Time       `json:"createdAt"`
	Keys      []tenantKeyView `json:"keys"`
}

func viewOf(t tenant.Tenant) tenantView {
	keys := make([]tenantKeyView, 0, len(t.Keys))
	for _, key := range t.Keys {
		keys = append(keys, tenantKeyView{Prefix: key.Prefix, CreatedAt: key.CreatedAt})
	}
	return tenantView{ID: t.ID, Name: t.Name, CreatedAt: t.CreatedAt, Keys: keys}
}

// handleTenantAdmin serves the global-admin tenant CRUD:
//
//	GET    /_subrouter/tenants                     list tenants (key prefixes only)
//	POST   /_subrouter/tenants        {"name":..}  create tenant, returns key once
//	POST   /_subrouter/tenants/<id>/keys           mint an extra key, returns it once
//	DELETE /_subrouter/tenants/<id>/keys/<prefix>  revoke keys matching prefix
func (m *MultiTenant) handleTenantAdmin(w http.ResponseWriter, r *http.Request) {
	if !m.Base.authorizeAdmin(r) {
		http.Error(w, "admin token required", http.StatusUnauthorized)
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/_subrouter/tenants")
	rest = strings.Trim(rest, "/")
	switch {
	case rest == "":
		switch r.Method {
		case http.MethodGet:
			tenants, err := m.Registry.List()
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			views := make([]tenantView, 0, len(tenants))
			for _, t := range tenants {
				views = append(views, viewOf(t))
			}
			writeJSON(w, views)
		case http.MethodPost:
			var payload struct {
				Name string `json:"name"`
			}
			if err := json.NewDecoder(io.LimitReader(r.Body, tenantAdminMaxBodyBytes)).Decode(&payload); err != nil {
				http.Error(w, "invalid JSON body", http.StatusBadRequest)
				return
			}
			created, key, err := m.Registry.Create(payload.Name)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			writeJSON(w, map[string]any{"tenant": viewOf(created), "key": key})
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	default:
		parts := strings.Split(rest, "/")
		if len(parts) >= 2 && parts[1] == "keys" {
			tenantID := parts[0]
			switch {
			case len(parts) == 2 && r.Method == http.MethodPost:
				updated, key, err := m.Registry.CreateKey(tenantID)
				if err != nil {
					http.Error(w, err.Error(), http.StatusNotFound)
					return
				}
				writeJSON(w, map[string]any{"tenant": viewOf(updated), "key": key})
				return
			case len(parts) == 3 && r.Method == http.MethodDelete:
				revoked, err := m.Registry.RevokeKey(tenantID, parts[2])
				if err != nil {
					http.Error(w, err.Error(), http.StatusNotFound)
					return
				}
				writeJSON(w, map[string]any{"ok": true, "revoked": revoked})
				return
			}
		}
		http.NotFound(w, r)
	}
}

const tenantAdminMaxBodyBytes = 1 << 16
