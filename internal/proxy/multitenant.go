package proxy

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	agentclaude "github.com/manaflow-ai/subrouter/internal/agents/claude"
	"github.com/manaflow-ai/subrouter/internal/stackauth"
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
	// Enabled is retained for --multi-tenant CLI compatibility. Tenant-shaped
	// credentials now always fail closed when they do not resolve.
	Enabled bool
	// StackVerifier enables normal-user tenant exchange at
	// /_subrouter/auth/stack. StackTenantKeySecret deterministically derives
	// the tenant path key after the verifier binds the request to a Stack team.
	StackVerifier interface {
		Verify(context.Context, string) (stackauth.Claims, error)
	}
	StackTeams interface {
		ListTeams(context.Context, string) ([]stackauth.Team, error)
	}
	StackTenantKeySecret   []byte
	StackTenantDeleteToken []byte
	PublicURL              string

	mu       sync.Mutex
	servers  map[string]*Server
	handlers map[string]http.Handler

	deletionMu   sync.Mutex
	deletions    map[string]struct{}
	resumeDelete sync.Once
}

// Handler wraps the legacy single-tenant handler with tenant routing and the
// admin tenant CRUD endpoints.
func (m *MultiTenant) Handler(fallback http.Handler) http.Handler {
	m.resumeDelete.Do(m.resumeTenantDeletions)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/_subrouter/auth/stack/tenant" {
			m.handleStackTenantDelete(w, r)
			return
		}
		if r.URL.Path == "/_subrouter/auth/stack" {
			m.handleStackAuth(w, r)
			return
		}
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
				m.serveResolvedTenant(w, r, resolved, r.URL.Path, key)
				return
			}
			// srt_ credentials belong exclusively to tenant routing. Always fail
			// closed, including after the last tenant has been deleted, so a
			// retired key can never fall through to the legacy global pool.
			http.Error(w, "unknown tenant key", http.StatusUnauthorized)
			return
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
	m.serveResolvedTenant(w, r, resolved, rest, key)
}

func (m *MultiTenant) serveResolvedTenant(
	w http.ResponseWriter,
	r *http.Request,
	t tenant.Tenant,
	path string,
	key string,
) {
	useLock, err := m.Registry.AcquireUse(t.ID)
	if err != nil {
		http.Error(w, "tenant unavailable", http.StatusServiceUnavailable)
		return
	}
	defer useLock.Close()
	fresh, ok, err := m.Registry.ResolveFresh(key)
	if err != nil {
		http.Error(w, "tenant registry error", http.StatusInternalServerError)
		return
	}
	if !ok || subtle.ConstantTimeCompare([]byte(fresh.ID), []byte(t.ID)) != 1 {
		http.Error(w, "unknown tenant key", http.StatusUnauthorized)
		return
	}
	handler, err := m.handlerFor(r.Context(), t)
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

func (m *MultiTenant) handlerFor(ctx context.Context, t tenant.Tenant) (http.Handler, error) {
	m.mu.Lock()
	if handler, ok := m.handlers[t.ID]; ok {
		m.mu.Unlock()
		return handler, nil
	}
	m.mu.Unlock()

	server, err := m.newTenantServer(ctx, t)
	if err != nil {
		return nil, err
	}
	handler := tenantScopedHandler(*server, t)
	m.mu.Lock()
	defer m.mu.Unlock()
	if existing, ok := m.handlers[t.ID]; ok {
		return existing, nil
	}
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
func (m *MultiTenant) newTenantServer(ctx context.Context, t tenant.Tenant) (*Server, error) {
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
	client := &http.Client{Timeout: 15 * time.Second, Transport: m.Base.Transport}
	ref, err := OpenAccountRefContext(ctx, codexStore, claudeStore, client)
	if err != nil {
		return nil, err
	}
	initial, accountGeneration := ref.Snapshot()

	server := m.Base
	server.Accounts = nil
	server.AccountRef = ref
	// A hosted tenant is the credential authority. It must never inherit a
	// local-egress broker from the process template and recursively lease from
	// itself.
	server.CredentialBroker = nil
	server.Sessions = sessions
	server.Scheduler = selectacct.Scheduler{}
	server.SchedulerRef = selectacct.NewSchedulerRef(selectacct.NewScheduler(tenantFallbackScores(initial)))
	server.SchedulerRef.AdvanceAccountGeneration(accountGeneration)
	server.ActiveSessions = NewActiveSessions()
	server.CacheFlight = newSingleFlight()
	// Reaching a tenant handler already proves possession of the tenant key,
	// so the tenant-visible _subrouter read endpoints need no admin token.
	server.AdminToken = ""
	server.tenantAccountImportAuthorized = true
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
	"/_subrouter/account-import":  true,
}

func tenantScopedHandler(server Server, t tenant.Tenant) http.Handler {
	inner := server.Handler()
	credentialLeases := newTenantCredentialLeaseStore()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/_subrouter/whoami" {
			writeJSON(w, map[string]any{"tenant_id": t.ID, "name": t.Name})
			return
		}
		if r.URL.Path == "/_subrouter/accounts" {
			switch r.Method {
			case http.MethodGet:
				server.handleAccounts(w, r)
				return
			case http.MethodPost:
				handleTenantAccountUpload(&server, w, r)
				return
			}
		}
		if strings.HasPrefix(r.URL.Path, "/_subrouter/accounts/") && r.Method == http.MethodDelete {
			handleTenantAccountDelete(&server, w, r)
			return
		}
		if r.URL.Path == "/_subrouter/leases" && r.Method == http.MethodPost {
			credentialLeases.handleIssue(&server, t, w, r)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/_subrouter/leases/") &&
			strings.HasSuffix(r.URL.Path, "/events") &&
			r.Method == http.MethodPost {
			credentialLeases.handleReport(&server, w, r)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/_subrouter/") && !tenantControlPaths[r.URL.Path] {
			http.NotFound(w, r)
			return
		}
		inner.ServeHTTP(w, r)
	})
}

func (m *MultiTenant) handleStackAuth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if m.StackVerifier == nil || len(m.StackTenantKeySecret) < 32 {
		http.NotFound(w, r)
		return
	}
	token := bearerToken(r.Header.Get("Authorization"))
	if token == "" {
		http.Error(w, "Stack access token required", http.StatusUnauthorized)
		return
	}
	claims, err := m.StackVerifier.Verify(r.Context(), token)
	if err != nil {
		if m.Base.Logger != nil {
			m.Base.Logger.Warn("Stack tenant exchange rejected", "error", err)
		}
		http.Error(w, "invalid Stack access token", http.StatusUnauthorized)
		return
	}
	var input struct {
		TeamID   string `json:"teamId"`
		TeamName string `json:"teamName"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, tenantAdminMaxBodyBytes)).Decode(&input); err != nil && !errors.Is(err, io.EOF) {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	teamID := strings.TrimSpace(input.TeamID)
	teamName := strings.TrimSpace(input.TeamName)
	if !validStackTeamName(teamName) {
		http.Error(w, "team name is invalid", http.StatusBadRequest)
		return
	}
	if teamID == "" {
		teamID = claims.SelectedTeamID
	}
	if subtle.ConstantTimeCompare([]byte(teamID), []byte(claims.SelectedTeamID)) != 1 &&
		subtle.ConstantTimeCompare([]byte(teamID), []byte(claims.Subject)) != 1 {
		if m.StackTeams == nil {
			http.Error(w, "Stack team membership cannot be verified", http.StatusServiceUnavailable)
			return
		}
		teams, err := m.StackTeams.ListTeams(r.Context(), token)
		if err != nil {
			if m.Base.Logger != nil {
				m.Base.Logger.Warn("Stack team membership lookup failed", "error", err)
			}
			http.Error(w, "Stack team membership unavailable", http.StatusServiceUnavailable)
			return
		}
		matched := false
		for _, team := range teams {
			if subtle.ConstantTimeCompare([]byte(team.ID), []byte(teamID)) != 1 {
				continue
			}
			matched = true
			if teamName == "" {
				teamName = strings.TrimSpace(team.DisplayName)
			}
			break
		}
		if !matched {
			http.Error(w, "Stack access token does not belong to that team", http.StatusForbidden)
			return
		}
	}
	base := strings.TrimRight(strings.TrimSpace(m.PublicURL), "/")
	if base == "" {
		http.Error(w, "hosted proxy URL is not configured", http.StatusInternalServerError)
		return
	}
	key, err := tenant.DeriveKey(m.StackTenantKeySecret, claims.ProjectID, teamID)
	if err != nil {
		http.Error(w, "tenant key unavailable", http.StatusInternalServerError)
		return
	}
	if teamName == "" {
		teamName = teamID
	}
	if !validStackTeamName(teamName) {
		http.Error(w, "team name is invalid", http.StatusBadRequest)
		return
	}
	created, err := m.Registry.EnsureExternal(teamID, teamName, key)
	if err != nil {
		if errors.Is(err, tenant.ErrTenantRetired) {
			http.Error(w, "tenant is retired", http.StatusGone)
			return
		}
		http.Error(w, "tenant unavailable", http.StatusInternalServerError)
		return
	}
	proxyURL := base + "/t/" + key
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, map[string]any{
		"tenantId": created.ID, "tenantName": created.Name,
		"tenantKey": key, "proxyUrl": proxyURL,
	})
}

func (m *MultiTenant) handleStackTenantDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if m.StackVerifier == nil || len(m.StackTenantKeySecret) < 32 || len(m.StackTenantDeleteToken) < 32 {
		http.NotFound(w, r)
		return
	}
	deleteToken := strings.TrimSpace(r.Header.Get("X-Subrouter-Tenant-Delete-Token"))
	if subtle.ConstantTimeCompare([]byte(deleteToken), m.StackTenantDeleteToken) != 1 {
		http.Error(w, "trusted tenant deletion credential required", http.StatusUnauthorized)
		return
	}
	token := bearerToken(r.Header.Get("Authorization"))
	if token == "" {
		http.Error(w, "Stack access token required", http.StatusUnauthorized)
		return
	}
	claims, err := m.StackVerifier.Verify(r.Context(), token)
	if err != nil {
		if m.Base.Logger != nil {
			m.Base.Logger.Warn("Stack tenant deletion rejected", "error", err)
		}
		http.Error(w, "invalid Stack access token", http.StatusUnauthorized)
		return
	}
	var input struct {
		TeamID string `json:"teamId"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, tenantAdminMaxBodyBytes)).Decode(&input); err != nil && !errors.Is(err, io.EOF) {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	teamID := strings.TrimSpace(input.TeamID)
	if teamID == "" {
		teamID = claims.SelectedTeamID
	}
	if !tenant.ValidExternalID(teamID) {
		http.Error(w, "team ID is invalid", http.StatusBadRequest)
		return
	}
	authorized := subtle.ConstantTimeCompare([]byte(teamID), []byte(claims.SelectedTeamID)) == 1 ||
		subtle.ConstantTimeCompare([]byte(teamID), []byte(claims.Subject)) == 1
	if !authorized {
		if m.StackTeams == nil {
			http.Error(w, "Stack team membership cannot be verified", http.StatusServiceUnavailable)
			return
		}
		teams, err := m.StackTeams.ListTeams(r.Context(), token)
		if err != nil {
			if m.Base.Logger != nil {
				m.Base.Logger.Warn("Stack tenant deletion membership lookup failed", "error", err)
			}
			http.Error(w, "Stack team membership unavailable", http.StatusServiceUnavailable)
			return
		}
		for _, candidate := range teams {
			if subtle.ConstantTimeCompare([]byte(candidate.ID), []byte(teamID)) == 1 {
				authorized = true
				break
			}
		}
	}
	if !authorized {
		http.Error(w, "Stack access token does not belong to that team", http.StatusForbidden)
		return
	}
	retired, err := m.Registry.RetireExternal(teamID)
	if err != nil {
		http.Error(w, "tenant retirement failed", http.StatusInternalServerError)
		return
	}
	useLock, acquired, err := m.Registry.TryAcquireExclusiveUse(teamID)
	if err != nil {
		m.scheduleTenantDeletion(teamID)
		http.Error(w, "tenant retirement failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	if !acquired {
		m.scheduleTenantDeletion(teamID)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		writeJSON(w, map[string]any{"ok": false, "deletionPending": true})
		return
	}
	defer useLock.Close()
	deleted, err := m.Registry.DeleteRetired(teamID)
	if err != nil {
		m.scheduleTenantDeletion(teamID)
		http.Error(w, "tenant deletion failed", http.StatusInternalServerError)
		return
	}
	m.forgetTenant(teamID)
	writeJSON(w, map[string]any{"ok": true, "deleted": retired || deleted})
}

func (m *MultiTenant) resumeTenantDeletions() {
	if m.Registry == nil {
		return
	}
	ids, err := m.Registry.PendingDeletionIDs()
	if err != nil {
		if m.Base.Logger != nil {
			m.Base.Logger.Error("tenant deletion recovery scan failed", "error", err)
		}
		return
	}
	for _, id := range ids {
		m.scheduleTenantDeletion(id)
	}
}

func (m *MultiTenant) scheduleTenantDeletion(id string) {
	m.deletionMu.Lock()
	if m.deletions == nil {
		m.deletions = map[string]struct{}{}
	}
	if _, exists := m.deletions[id]; exists {
		m.deletionMu.Unlock()
		return
	}
	m.deletions[id] = struct{}{}
	m.deletionMu.Unlock()

	go func() {
		defer func() {
			m.deletionMu.Lock()
			delete(m.deletions, id)
			m.deletionMu.Unlock()
		}()
		backoff := 100 * time.Millisecond
		for {
			deletionLock, err := m.Registry.AcquireExclusiveUse(id)
			if err == nil {
				_, err = m.Registry.DeleteRetired(id)
				closeErr := deletionLock.Close()
				if err == nil {
					err = closeErr
				}
			}
			if err == nil {
				m.forgetTenant(id)
				return
			}
			if m.Base.Logger != nil {
				m.Base.Logger.Error("background tenant deletion failed", "tenant", id, "error", err)
			}
			if m.Base.Lifecycle != nil && m.Base.Lifecycle.Draining() {
				return
			}
			timer := time.NewTimer(backoff)
			<-timer.C
			if backoff < 30*time.Second {
				backoff *= 2
				if backoff > 30*time.Second {
					backoff = 30 * time.Second
				}
			}
		}
	}()
}

func (m *MultiTenant) forgetTenant(id string) {
	m.mu.Lock()
	delete(m.servers, id)
	delete(m.handlers, id)
	m.mu.Unlock()
}

func validStackTeamName(name string) bool {
	return len(name) <= 320 && !containsTerminalControl(name)
}

type tenantAccountUpload struct {
	Provider        string `json:"provider"`
	Label           string `json:"label"`
	APIKey          string `json:"apiKey"`
	TargetAccountID string `json:"targetAccountID,omitempty"`
	Tokens          *struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		IDToken      string `json:"idToken"`
		AccountID    string `json:"accountID"`
	} `json:"tokens"`
	ClaudeAIOAuth *agentclaude.CredentialInfo `json:"claudeAiOauth"`
}

func handleTenantAccountUpload(server *Server, w http.ResponseWriter, r *http.Request) {
	if server.AccountRef == nil {
		http.Error(w, "tenant account store unavailable", http.StatusServiceUnavailable)
		return
	}
	bodyLimit := server.MaxBodyBytes
	if bodyLimit <= 0 {
		bodyLimit = 1 << 20
	}
	var input tenantAccountUpload
	decoder := json.NewDecoder(io.LimitReader(r.Body, bodyLimit))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	input.Provider = strings.ToLower(strings.TrimSpace(input.Provider))
	input.Label = strings.TrimSpace(input.Label)
	input.TargetAccountID = strings.TrimSpace(input.TargetAccountID)
	if input.Label == "" || len(input.Label) > 320 {
		http.Error(w, "account label is required", http.StatusBadRequest)
		return
	}
	var id string
	var kind string
	validateRepairTarget := func(candidate string) bool {
		return input.TargetAccountID == "" || subtle.ConstantTimeCompare(
			[]byte(input.TargetAccountID), []byte(candidate),
		) == 1
	}
	switch input.Provider {
	case "codex":
		if input.Tokens == nil || input.Tokens.AccessToken == "" || input.Tokens.RefreshToken == "" || input.Tokens.IDToken == "" {
			http.Error(w, "complete Codex OAuth tokens are required", http.StatusBadRequest)
			return
		}
		id, kind = input.Label, "codex"
		if !validateRepairTarget(id) {
			http.Error(w, "repair target does not match uploaded account", http.StatusConflict)
			return
		}
		err := server.AccountRef.store.SaveStored(accounts.StoredCodexAccount{
			Email: input.Label, Provider: accounts.ProviderCodex,
			Auth: accounts.CodexAuthFile{
				AuthMode: "chatgpt",
				Tokens: &accounts.CodexTokens{
					AccessToken: input.Tokens.AccessToken, RefreshToken: input.Tokens.RefreshToken,
					IDToken: input.Tokens.IDToken, AccountID: input.Tokens.AccountID,
				},
			},
		})
		if err != nil {
			http.Error(w, "save Codex account", http.StatusInternalServerError)
			return
		}
	case "openai-apikey", "anthropic-apikey":
		if strings.TrimSpace(input.APIKey) == "" {
			http.Error(w, "API key is required", http.StatusBadRequest)
			return
		}
		provider := accounts.ProviderCodex
		if input.Provider == "anthropic-apikey" {
			provider = accounts.ProviderClaude
		}
		id, kind = "apikey:"+input.Provider+":"+input.Label, input.Provider
		if !validateRepairTarget(id) {
			http.Error(w, "repair target does not match uploaded account", http.StatusConflict)
			return
		}
		if err := server.AccountRef.store.SaveStored(accounts.StoredCodexAccount{
			Email: id, Provider: provider,
			Auth: accounts.CodexAuthFile{AuthMode: "apikey", OpenAIAPIKey: strings.TrimSpace(input.APIKey)},
		}); err != nil {
			http.Error(w, "save API key", http.StatusInternalServerError)
			return
		}
	case "claude":
		if input.ClaudeAIOAuth == nil || input.ClaudeAIOAuth.AccessToken == "" || input.ClaudeAIOAuth.RefreshToken == "" {
			http.Error(w, "complete Claude OAuth tokens are required", http.StatusBadRequest)
			return
		}
		id, kind = input.Label, "claude"
		if !validateRepairTarget(id) {
			http.Error(w, "repair target does not match uploaded account", http.StatusConflict)
			return
		}
		if _, err := server.AccountRef.claudeStore.UpsertCredentialProfile(input.Label, *input.ClaudeAIOAuth); err != nil {
			http.Error(w, "save Claude account", http.StatusInternalServerError)
			return
		}
	default:
		http.Error(w, "unsupported provider", http.StatusBadRequest)
		return
	}
	if _, _, err := server.reloadAccounts(r.Context()); err != nil {
		http.Error(w, "account saved but reload failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"account": map[string]any{
		"id": id, "kind": kind, "label": input.Label,
	}})
}

func handleTenantAccountDelete(server *Server, w http.ResponseWriter, r *http.Request) {
	if server.AccountRef == nil {
		http.Error(w, "tenant account store unavailable", http.StatusServiceUnavailable)
		return
	}
	id := strings.TrimSpace(strings.TrimPrefix(r.URL.Path, "/_subrouter/accounts/"))
	if id == "" {
		http.NotFound(w, r)
		return
	}
	removed := false
	if _, ok, err := server.AccountRef.store.RemoveStored(id); err != nil {
		http.Error(w, "remove account", http.StatusInternalServerError)
		return
	} else if ok {
		removed = true
	}
	if ok, err := server.AccountRef.claudeStore.RemoveProfile(id); err != nil {
		http.Error(w, "remove account", http.StatusInternalServerError)
		return
	} else if ok {
		removed = true
	}
	if !removed {
		http.Error(w, "account not found", http.StatusNotFound)
		return
	}
	if _, _, err := server.reloadAccounts(r.Context()); err != nil {
		http.Error(w, "account removed but reload failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
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
