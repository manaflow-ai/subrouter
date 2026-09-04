package proxy

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"log"
	"log/slog"
	"math"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"

	"github.com/gorilla/websocket"
	accountpkg "github.com/manaflow-ai/subrouter/account"
	"github.com/manaflow-ai/subrouter/internal/accounts"
	agentantigravity "github.com/manaflow-ai/subrouter/internal/agents/antigravity"
	agentclaude "github.com/manaflow-ai/subrouter/internal/agents/claude"
	agentgrok "github.com/manaflow-ai/subrouter/internal/agents/grok"
	agentkimi "github.com/manaflow-ai/subrouter/internal/agents/kimi"
	agentqwen "github.com/manaflow-ai/subrouter/internal/agents/qwen"
	"github.com/manaflow-ai/subrouter/internal/broker"
	"github.com/manaflow-ai/subrouter/internal/transcript"
	"github.com/manaflow-ai/subrouter/selectacct"
	"github.com/manaflow-ai/subrouter/session"
)

// Account import states reported by /_subrouter/health.
const (
	StoreHandshakePath    = "/_subrouter/store-handshake"
	AccountImportEnabled  = "enabled"
	AccountImportDisabled = "disabled"

	// ShadowHealthChallengeHeader is an opt-in challenge used by the disposable
	// shadow rehearsal helper to prove that it reached the candidate it started.
	// Ordinary health clients do not send it and see no shadow-only fields.
	ShadowHealthChallengeHeader = "X-Subrouter-Shadow-Challenge"
	ShadowHealthProofField      = "shadow_candidate_proof"
	shadowHealthDomain          = "subrouter-shadow-health-v1\x00"
)

type localDataConnectionContextKey struct{}

type localDataConnectionState struct {
	authorized atomic.Bool
}

// LocalDataConnContext gives one HTTP connection a mutable authorization
// state. A successful store handshake marks only that connection; no bearer
// credential is copied into the launching CLI process.
func LocalDataConnContext(ctx context.Context, _ net.Conn) context.Context {
	return context.WithValue(ctx, localDataConnectionContextKey{}, &localDataConnectionState{})
}

func authorizeLocalDataConnection(request *http.Request) {
	if state, ok := request.Context().Value(localDataConnectionContextKey{}).(*localDataConnectionState); ok && state != nil {
		state.authorized.Store(true)
	}
}

func localDataConnectionAuthorized(request *http.Request) bool {
	state, ok := request.Context().Value(localDataConnectionContextKey{}).(*localDataConnectionState)
	return ok && state != nil && state.authorized.Load()
}

type CredentialBroker interface {
	Lease(context.Context, broker.LeaseRequest) (broker.Lease, error)
	Report(context.Context, string, broker.LeaseReport) error
}

type Server struct {
	Upstream            *url.URL
	CodexUpstream       *url.URL
	APIUpstream         *url.URL
	ClaudeUpstream      *url.URL
	KimiUpstream        *url.URL
	ZAIUpstream         *url.URL
	OpenRouterUpstream  *url.URL
	DeepSeekUpstream    *url.URL
	TogetherUpstream    *url.URL
	FireworksUpstream   *url.URL
	OpenCodeZenUpstream *url.URL
	GrokUpstream        *url.URL
	// GrokSubscriptionUpstream is where a Grok subscription (device-code
	// OAuth) terminates; API-key accounts keep GrokUpstream.
	GrokSubscriptionUpstream *url.URL
	QwenUpstream             *url.URL
	QwenTokenUpstream        *url.URL
	QwenAnthropicUpstream    *url.URL
	// AntigravityUpstream fronts Google's cloudcode-pa endpoint, which the
	// Antigravity CLI reaches when CLOUD_CODE_URL points at this proxy.
	AntigravityUpstream *url.URL
	// LegacyStoreAttestation keeps v1/direct loopback clients compatible. A
	// supervisor with a private v2 data socket disables this public proof path.
	LegacyStoreAttestation bool
	Accounts               []accounts.Account
	AccountRef             *AccountRef
	Sessions               *session.Store
	Scheduler              selectacct.Scheduler
	SchedulerRef           *selectacct.SchedulerRef
	UsageScoreTTL          time.Duration
	// ReadyCheck gates supervisor readiness on asynchronous startup state that
	// must be coherent before this worker may receive traffic.
	ReadyCheck    func() error
	ScoreAccounts func(context.Context, []accounts.Account) ([]selectacct.Score, int)
	// RefreshAccountFn, when set, replaces the default OAuth refresh path. Test
	// seam for simulating dead/expired refresh tokens; nil in production.
	RefreshAccountFn func(context.Context, accounts.Account) (accounts.Account, error)
	// CredentialBroker selects a team account and returns an access-only,
	// short-lived lease. When configured, local refresh-token stores and the
	// local scheduler are bypassed entirely.
	CredentialBroker CredentialBroker
	// antigravityImportAttestForTest is immutable after construction and lets
	// hermetic import tests replace Google's token endpoint.
	antigravityImportAttestForTest func(context.Context, *http.Client, agentantigravity.CredentialInfo, time.Time) (agentantigravity.CredentialInfo, error)
	Transport                      http.RoundTripper
	Logger                         *slog.Logger
	ActiveSessions                 *ActiveSessions
	RequireSessionLease            bool
	// ForwardSessionHeaders preserves the selected session identity across an
	// explicitly configured Subrouter-to-Subrouter delegation hop.
	ForwardSessionHeaders bool
	sessionLeases         *sessionLeaseStore
	cutoverChallenges     *cutoverChallengeRegistry
	// StreamDrops counts dropped response streams by which side ended them,
	// so the expected client-hangup case is countable without a log line each.
	StreamDrops *StreamDropStats
	Lifecycle   *Lifecycle
	AdminToken  string
	// ShadowHealthKey is an ephemeral, per-process attestation key used only by
	// the optional shadow rehearsal. Nil keeps the normal health response.
	ShadowHealthKey []byte
	// AccountImportToken authorizes only the protected account-import endpoint.
	// It is intentionally distinct from AdminToken, which can read operational
	// state and transcripts.
	AccountImportToken string
	// TailnetAuth authenticates callers with the tailnet itself instead of a
	// token, for self-hosted servers whose port is already restricted to a
	// tailnet by ACL. Identity comes from this machine's tailscaled, so it is
	// an assertion about a WireGuard-authenticated peer rather than a claim
	// carried in the request. Nil disables the mode, which is the default and
	// the only supported configuration for shared cloud deployments.
	TailnetAuth TailnetAuthorizer
	// LocalProxyToken protects provider proxy routes in cloud mode. Health and
	// readiness stay unauthenticated so supervisors can probe the daemon.
	LocalProxyToken string
	MaxBodyBytes    int64
	Transcripts     *transcript.Recorder
	// CacheFlight collapses identical concurrent requests to read-heavy
	// polling endpoints into one upstream fetch. Nothing is stored between
	// requests; see request_coalesce.go for why there is no response cache.
	CacheFlight *singleFlight
	// Bedrock, when set, enables the /bedrock/* SigV4 signing gateway.
	Bedrock *BedrockConfig
	// ClaudeFableAPIKey, when set, serves Claude Fable requests via this Anthropic
	// API key (x-api-key) instead of the subscription pool or Bedrock. It applies
	// ONLY to Fable; Opus/Sonnet/etc. continue to use the OAuth pool and never
	// touch this key.
	ClaudeFableAPIKey string

	// ClaudeFableCacheTTLUpgradeOff disables the Bedrock-path rewrite of bare
	// ephemeral cache_control blocks to the 1-hour TTL (see
	// upgradeEphemeralCacheTTL). Default off = upgrade enabled.
	ClaudeFableCacheTTLUpgradeOff bool
	// AzureCodex, when set, serves Codex Responses requests from Azure OpenAI
	// after the subscription pool has spent its retry budget and still failed,
	// and pins the session to Azure afterwards so later turns keep hitting the
	// same prompt cache. It never preempts the pool.
	AzureCodex *AzureCodexConfig
	// azureCodexSessions holds those pins.
	azureCodexSessions *azureCodexSticky
	// azureCodexRejects remembers request fields an Azure deployment refused.
	azureCodexRejects *azureCodexFieldMemory
	// FableBedrockPrimary, when true, routes Claude Fable requests to AWS Bedrock
	// FIRST, before the subscription pool, instead of using Bedrock only as a
	// fallback. It only takes effect when the Bedrock gateway is configured; a
	// non-2xx Bedrock response (or an unreachable Bedrock) falls through to the
	// normal pool path, which keeps its own Bedrock/API-key fallback. Applies
	// ONLY to Fable; other Claude models are unaffected.
	FableBedrockPrimary bool
	// tenantAccountImportAuthorized is set only on a server reached after the
	// MultiTenant router has validated its tenant key. Global remote imports
	// require a configured admin token instead.
	tenantAccountImportAuthorized bool
}

type ActiveSessions struct {
	mu     sync.Mutex
	counts map[string]int
}

func NewActiveSessions() *ActiveSessions {
	return &ActiveSessions{counts: map[string]int{}}
}

func (a *ActiveSessions) Begin(agentType, sessionID string) func() {
	if a == nil || sessionID == "" {
		return func() {}
	}
	key := session.ScopedSessionKey(agentType, sessionID)
	a.mu.Lock()
	a.counts[key]++
	a.mu.Unlock()
	return func() {
		a.mu.Lock()
		defer a.mu.Unlock()
		if a.counts[key] <= 1 {
			delete(a.counts, key)
			return
		}
		a.counts[key]--
	}
}

func (a *ActiveSessions) Active(agentType, sessionID string) bool {
	if a == nil || sessionID == "" {
		return false
	}
	key := session.ScopedSessionKey(agentType, sessionID)
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.counts[key] > 0
}

type Lifecycle struct {
	startedAt time.Time
	gateMu    sync.Mutex
	draining  atomic.Bool
	quiesced  atomic.Bool
	active    atomic.Int64
}

func NewLifecycle() *Lifecycle {
	return &Lifecycle{startedAt: time.Now().UTC()}
}

func (l *Lifecycle) BeginProxyRequest() func() {
	end, _ := l.TryBeginProxyRequest()
	return end
}

// TryBeginProxyRequest atomically admits a request against strict quiescence.
// A successful quiesce therefore partitions requests cleanly: every request
// either incremented active before the gate closed, or is rejected afterward.
func (l *Lifecycle) TryBeginProxyRequest() (func(), bool) {
	if l == nil {
		return func() {}, true
	}
	l.gateMu.Lock()
	defer l.gateMu.Unlock()
	if l.quiesced.Load() {
		return func() {}, false
	}
	l.active.Add(1)
	return func() {
		l.active.Add(-1)
	}, true
}

func (l *Lifecycle) Drain() {
	if l != nil {
		l.draining.Store(true)
	}
}

func (l *Lifecycle) Quiesce() {
	if l == nil {
		return
	}
	l.gateMu.Lock()
	l.draining.Store(true)
	l.quiesced.Store(true)
	l.gateMu.Unlock()
}

func (l *Lifecycle) Resume() {
	if l == nil {
		return
	}
	l.gateMu.Lock()
	l.quiesced.Store(false)
	l.draining.Store(false)
	l.gateMu.Unlock()
}

func (l *Lifecycle) Draining() bool {
	return l != nil && l.draining.Load()
}

func (l *Lifecycle) Quiesced() bool {
	return l != nil && l.quiesced.Load()
}

func (l *Lifecycle) ActiveProxyRequests() int64 {
	if l == nil {
		return 0
	}
	return l.active.Load()
}

func (l *Lifecycle) Status() map[string]any {
	if l == nil {
		return map[string]any{
			"draining":                 false,
			"quiesced":                 false,
			"accepting_proxy_requests": true,
			"active_proxy_requests":    int64(0),
		}
	}
	return map[string]any{
		"draining":                 l.Draining(),
		"quiesced":                 l.Quiesced(),
		"accepting_proxy_requests": !l.Quiesced(),
		"active_proxy_requests":    l.ActiveProxyRequests(),
		"started_at":               l.startedAt.Format(time.RFC3339),
	}
}

type AccountRef struct {
	mu        sync.RWMutex
	installMu sync.Mutex
	// publishGenerationForTest is immutable after AccountRef construction.
	// Production constructors leave it nil and always use the durable publisher.
	publishGenerationForTest           func(string) error
	afterTenantStoredRevalidateForTest func()
	beforeTenantStoredRemovalForTest   func()
	accounts                           []accounts.Account
	accountGeneration                  uint64
	credentialRevision                 uint64
	diskGeneration                     string
	store                              accounts.CodexStore
	claudeStore                        agentclaude.Store
	oauthSources                       []OAuthAccountSource
	client                             *http.Client
	qwenConsoleRoot                    string
	apiKeyUpstreams                    map[accounts.Provider]string

	usageStatusMu    sync.Mutex
	usageStatusCache []AccountUsageStatus
	usageStatusAt    time.Time
	lastGoodUsage    map[string]usageStatusSnapshot

	usageWindowsMu sync.Mutex
	usageWindows   map[string]usageWindowsEntry

	credFailMu sync.Mutex
	credFail   map[string]credFailure
}

func (r *AccountRef) qwenRoot() string {
	if r != nil && strings.TrimSpace(r.qwenConsoleRoot) != "" {
		return r.qwenConsoleRoot
	}
	if r != nil {
		return agentqwen.ConsoleRootForStore(r.store)
	}
	return agentqwen.DefaultConsoleRoot()
}

// configureAPIKeyUpstreams binds status probes to the same effective
// endpoints as inference routing. It is called while Handler is assembled,
// before requests can observe the AccountRef.
func (r *AccountRef) configureAPIKeyUpstreams(server Server) {
	if r == nil {
		return
	}
	upstreams := make(map[accounts.Provider]string)
	for _, entry := range keyedProviders() {
		if entry.Upstream == nil {
			continue
		}
		if upstream := entry.Upstream(server, accounts.AuthModeAPIKey); upstream != nil {
			upstreams[entry.Provider] = upstream.String()
		}
	}
	r.mu.Lock()
	r.apiKeyUpstreams = upstreams
	r.mu.Unlock()
}

func (r *AccountRef) apiKeyUpstream(provider accounts.Provider) string {
	if r == nil {
		return ""
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.apiKeyUpstreams[provider]
}

func (r *AccountRef) kimiStore() agentkimi.Store {
	if r != nil {
		for _, source := range r.oauthSources {
			switch store := source.(type) {
			case agentkimi.Store:
				return store
			case *agentkimi.Store:
				if store != nil {
					return *store
				}
			}
		}
	}
	return agentkimi.ServingStore()
}

func (r *AccountRef) antigravityStore() (*agentantigravity.Store, bool) {
	if r != nil {
		for _, source := range r.oauthSources {
			if store, ok := source.(*agentantigravity.Store); ok && store != nil {
				return store, true
			}
		}
	}
	return nil, false
}

func (r *AccountRef) hasOAuthUsageSource(provider accounts.Provider) bool {
	if provider == "" || provider == accounts.ProviderCodex || provider == accounts.ProviderClaude {
		return true
	}
	if r == nil {
		return false
	}
	for _, source := range r.oauthSources {
		if source.Provider() != provider {
			continue
		}
		_, ok := source.(OAuthUsageSource)
		return ok
	}
	return false
}

// OAuthAccountSource is one provider's OAuth credential store. Claude and
// Codex predate it and keep their bespoke wiring; every OAuth provider added
// since (Kimi, Antigravity, Grok) plugs in here instead of growing another
// hardcoded branch in Refresh and loadAccountRefAccounts.
type OAuthAccountSource interface {
	Provider() accounts.Provider
	ListAccounts(ctx context.Context) ([]accounts.Account, error)
	RefreshAccount(ctx context.Context, client *http.Client, account accounts.Account) (accounts.Account, error)
}

// OAuthUsageSource is an OAuth provider whose subscription service publishes
// quota windows. Keeping this optional means credential-only providers still
// participate in routing without pretending that an unavailable quota API
// exists.
type OAuthUsageSource interface {
	OAuthAccountSource
	FetchUsage(ctx context.Context, client *http.Client, account accounts.Account) (planType string, windows []accounts.UsageWindow, err error)
}

// OAuthIdentityUsageSource optionally returns an identity verified by the
// provider while fetching usage. It avoids trusting local labels or unsigned
// token claims when a usage endpoint can identify the exact OAuth principal.
type OAuthIdentityUsageSource interface {
	OAuthUsageSource
	FetchUsageIdentity(ctx context.Context, client *http.Client, account accounts.Account) (email, planType string, windows []accounts.UsageWindow, err error)
}

type usageStatusSnapshot struct {
	status AccountUsageStatus
	at     time.Time
}

// credFailure remembers a terminal credential error for an account. These
// errors recover through re-authentication, not by retrying the same refresh
// request, so repeated status sweeps must not probe the account again.
type credFailure struct {
	err string
	at  time.Time
}

type usageWindowsEntry struct {
	windows []accounts.UsageWindow
	at      time.Time
}

const usageWindowsTTL = 2 * time.Minute
const usageWindowsLastGoodTTL = 15 * time.Minute

// Keep one account's refresh and usage request from holding an interactive
// status sweep open indefinitely. The sweep runs accounts concurrently, so a
// slow account costs at most this timeout instead of adding to every other
// account's latency.
const (
	usageStatusFetchTimeout = 5 * time.Second
	accountFetchConcurrency = 4
)

const credFailureTTL = credentialExhaustionTTL

var errOAuthUsageUnavailable = errors.New("OAuth usage unavailable")

func (r *AccountRef) terminalCredFailure(account accounts.Account) (string, bool) {
	if r == nil {
		return "", false
	}
	r.credFailMu.Lock()
	defer r.credFailMu.Unlock()
	failure, ok := r.credFail[credFailureKey(account)]
	if !ok || time.Since(failure.at) > credFailureTTL {
		return "", false
	}
	return failure.err, true
}

func (r *AccountRef) noteCredResult(account accounts.Account, err error) {
	if r == nil {
		return
	}
	r.credFailMu.Lock()
	defer r.credFailMu.Unlock()
	if r.credFail == nil {
		r.credFail = make(map[string]credFailure)
	}
	now := time.Now()
	for candidate, failure := range r.credFail {
		if now.Sub(failure.at) > credFailureTTL {
			delete(r.credFail, candidate)
		}
	}
	key := credFailureKey(account)
	if isTerminalCredentialError(err) {
		r.credFail[key] = credFailure{err: err.Error(), at: now}
		return
	}
	delete(r.credFail, key)
}

func credFailureKey(account accounts.Account) string {
	fingerprint := sha256.Sum256([]byte(account.CredentialIdentity()))
	return string(account.Provider) + "\x00" + account.ID + "\x00" + string(fingerprint[:])
}

func (r *AccountRef) credentialSnapshot(provider accounts.Provider, id string) accounts.Account {
	if r != nil {
		r.mu.RLock()
		defer r.mu.RUnlock()
		for _, candidate := range r.accounts {
			if sameCredentialProvider(candidate.Provider, provider) && candidate.ID == id {
				return candidate
			}
		}
	}
	return accounts.Account{ID: id, Provider: provider, AuthMode: accounts.AuthModeOAuth}
}

// FetchUsageWindowsCached is the single path for reading an account's usage
// windows. Every consumer (scheduler scoring, the usage-status sweep,
// auto-switch) used to fetch live, and with many pooled accounts the combined
// rate tripped the upstream usage endpoints' per-IP limits, which then
// cascaded: failed fetches zeroed scores and made healthy accounts look
// exhausted. Fresh-within-TTL returns the cache; a transient fetch failure
// falls back to the last-known-good windows instead of erroring.
//
// The returned bool reports whether the windows are FRESH (a recent successful
// fetch) versus a STALE last-known-good fallback. Callers must not treat stale
// windows as a confident exhaustion signal: stale cooked data was overwriting
// healthy accounts' scores and routing traffic to dead accounts.
func (r *AccountRef) FetchUsageWindowsCached(ctx context.Context, client *http.Client, account accounts.Account) ([]accounts.UsageWindow, bool, error) {
	if r == nil {
		windows, err := fetchAccountUsageWindowsLive(ctx, client, account)
		return windows, err == nil, err
	}
	key := account.ID + "\x00" + string(account.Provider)
	now := time.Now()
	r.usageWindowsMu.Lock()
	entry, ok := r.usageWindows[key]
	r.usageWindowsMu.Unlock()
	if ok && now.Sub(entry.at) < usageWindowsTTL {
		return append([]accounts.UsageWindow(nil), entry.windows...), true, nil
	}
	windows, err := r.fetchAccountUsageWindowsLive(ctx, client, account)
	if err == nil {
		r.usageWindowsMu.Lock()
		if r.usageWindows == nil {
			r.usageWindows = map[string]usageWindowsEntry{}
		}
		r.usageWindows[key] = usageWindowsEntry{windows: append([]accounts.UsageWindow(nil), windows...), at: now}
		r.usageWindowsMu.Unlock()
		return windows, true, nil
	}
	if !authLikeUsageError(err.Error()) && ok && now.Sub(entry.at) < usageWindowsLastGoodTTL {
		return append([]accounts.UsageWindow(nil), entry.windows...), false, nil
	}
	return nil, false, err
}

// fetchAccountUsageWindowsLive dispatches subscription usage through the same
// provider source that owns the OAuth credential. Without this dispatch, a
// newer OAuth provider such as Kimi fell through to the legacy Codex endpoint,
// which could incorrectly zero a healthy account's routing score on reload.
func (r *AccountRef) fetchAccountUsageWindowsLive(ctx context.Context, client *http.Client, account accounts.Account) ([]accounts.UsageWindow, error) {
	if account.AuthMode == accounts.AuthModeOAuth {
		for _, source := range r.oauthSources {
			if source.Provider() != account.Provider {
				continue
			}
			usageSource, ok := source.(OAuthUsageSource)
			if !ok {
				return nil, fmt.Errorf("%w for provider %q", errOAuthUsageUnavailable, account.Provider)
			}
			_, windows, err := usageSource.FetchUsage(ctx, client, account)
			return windows, err
		}
	}
	return fetchAccountUsageWindowsLive(ctx, client, account)
}

// ResolvedAccount returns a refreshed, token-bearing OAuth account for the
// given email, refreshing the stored token if it has expired. The bool is
// false (with no error) when no stored OAuth account matches email. It is the
// single path the rate-limit-reset handler uses to get a live credential for a
// server-owned account without going through the usage-status sweep.
func (r *AccountRef) ResolvedAccount(ctx context.Context, email string) (accounts.Account, bool, error) {
	if r == nil {
		return accounts.Account{}, false, fmt.Errorf("account store not configured")
	}
	storedAccounts, err := r.store.ListStored()
	if err != nil {
		return accounts.Account{}, false, err
	}
	var matched *accounts.StoredCodexAccount
	for i := range storedAccounts {
		if storedAccounts[i].Email == email {
			s := storedAccounts[i]
			matched = &s
			break
		}
	}
	if matched == nil {
		return accounts.Account{}, false, nil
	}
	if matched.IsAPIKey() {
		return accounts.Account{}, false, fmt.Errorf("account %s is an API-key account", email)
	}
	refreshed, _, err := r.store.RefreshStoredIfExpired(ctx, r.client, *matched)
	if err != nil {
		return accounts.Account{}, false, err
	}
	account, ok := refreshed.Account(refreshed.SourcePath(r.store))
	if !ok {
		return accounts.Account{}, false, fmt.Errorf("account %s has no access token", email)
	}
	r.replace(account)
	return account, true, nil
}

type AccountStatus struct {
	ID          string            `json:"id"`
	Provider    accounts.Provider `json:"provider"`
	AuthMode    accounts.AuthMode `json:"auth_mode"`
	Email       string            `json:"email,omitempty"`
	Source      string            `json:"source"`
	AuthChecked bool              `json:"auth_checked"`
	AuthValid   bool              `json:"auth_valid"`
	Refreshed   bool              `json:"refreshed,omitempty"`
	Error       string            `json:"error,omitempty"`
}

type AccountUsageStatus struct {
	AccountStatus
	Active             bool                             `json:"active,omitempty"`
	KeyFingerprint     string                           `json:"key_fingerprint,omitempty"`
	AssignedSessions   int                              `json:"assigned_sessions,omitempty"`
	SessionsKnown      bool                             `json:"sessions_known,omitempty"`
	PlanType           string                           `json:"plan_type,omitempty"`
	ProviderHealth     string                           `json:"provider_health,omitempty"`
	ProviderModels     *int                             `json:"provider_models,omitempty"`
	ProviderEndpoints  []string                         `json:"provider_endpoints,omitempty"`
	QuotaStatus        string                           `json:"quota_status,omitempty"`
	AccountIdentity    string                           `json:"account_identity,omitempty"`
	QuotaUsageKnown    bool                             `json:"quota_usage_known,omitempty"`
	Windows            []accounts.UsageWindow           `json:"windows,omitempty"`
	Credits            *accounts.CreditsInfo            `json:"credits,omitempty"`
	ComplimentaryReset *accounts.ComplimentaryResetInfo `json:"complimentary_reset,omitempty"`
	UsageFresh         bool                             `json:"-"`
}

func NewAccountRef(store accounts.CodexStore, initial []accounts.Account, client *http.Client) *AccountRef {
	claudeStore := agentclaude.DefaultStore()
	ref := &AccountRef{
		accounts:        append([]accounts.Account(nil), initial...),
		store:           store,
		claudeStore:     claudeStore,
		client:          client,
		qwenConsoleRoot: agentqwen.ConsoleRootForStore(store),
	}
	transactionLock, err := lockAccountImportTransaction(context.Background(), store.StoreDir())
	if err != nil {
		return ref
	}
	defer transactionLock.Close()
	diskGeneration, err := readAccountDiskGeneration(store.StoreDir())
	if err != nil {
		return ref
	}
	// A generation marker means an HTTP import has occurred. Reload while the
	// transaction lock is held instead of pairing a caller's pre-lock snapshot
	// with the post-import marker. Legacy stores without a marker retain the
	// supplied snapshot for test and compatibility callers.
	if diskGeneration != "" {
		if loaded, loadErr := loadAccountRefAccounts(store, claudeStore, nil); loadErr == nil {
			ref.accounts = loaded
			ref.diskGeneration = diskGeneration
		}
		return ref
	}
	ref.diskGeneration = diskGeneration
	return ref
}

// OpenAccountRef loads accounts and their disk generation under the same
// cross-process transaction lock. Production callers use this constructor so
// worker startup can never make a stale snapshot look current.
func OpenAccountRef(store accounts.CodexStore, claudeStore agentclaude.Store, client *http.Client) (*AccountRef, error) {
	return OpenAccountRefWithSources(context.Background(), store, claudeStore, client, nil)
}

// OpenAccountRefContext loads one account snapshot while honoring cancellation
// if another process is currently committing an import transaction.
func OpenAccountRefContext(ctx context.Context, store accounts.CodexStore, claudeStore agentclaude.Store, client *http.Client) (*AccountRef, error) {
	return OpenAccountRefWithSources(ctx, store, claudeStore, client, nil)
}

// OpenAccountRefWithSources is OpenAccountRefContext plus the OAuth account
// sources of every provider beyond Codex and Claude.
func OpenAccountRefWithSources(ctx context.Context, store accounts.CodexStore, claudeStore agentclaude.Store, client *http.Client, sources []OAuthAccountSource) (*AccountRef, error) {
	configuredSources := append([]OAuthAccountSource(nil), sources...)
	refreshTransaction := func(ctx context.Context, refresh func() error) error {
		return withAccountDiskTransaction(ctx, store, refresh)
	}
	for i, source := range configuredSources {
		switch store := source.(type) {
		case *agentantigravity.Store:
			if store != nil {
				store.ForServing()
				store.RefreshTransaction = refreshTransaction
				configuredSources[i] = store
			}
		case agentkimi.Store:
			store = store.ForServing()
			store.RefreshTransaction = refreshTransaction
			configuredSources[i] = store
		case *agentkimi.Store:
			if store != nil {
				clone := store.ForServing()
				clone.RefreshTransaction = refreshTransaction
				configuredSources[i] = &clone
			}
		case agentgrok.Store:
			store.RefreshTransaction = refreshTransaction
			configuredSources[i] = store
		case *agentgrok.Store:
			if store != nil {
				clone := *store
				clone.RefreshTransaction = refreshTransaction
				configuredSources[i] = &clone
			}
		}
	}
	transactionLock, err := lockAccountImportTransaction(ctx, store.StoreDir())
	if err != nil {
		return nil, err
	}
	defer transactionLock.Close()
	_, rollbackErr := reconcileCompletedAccountRollback(ctx, store, advanceAccountDiskGeneration)
	if rollbackErr != nil && !errors.Is(rollbackErr, errAccountRollbackIncomplete) {
		return nil, rollbackErr
	}
	if rollbackErr == nil {
		// The account journal covers published mutations. A process can also die
		// after Claude credential directories are staged but before the registry
		// changes; reconcile that pre-commit window from registry authority before
		// loading any routable account snapshot.
		if err := claudeStore.ReconcileProfileInstanceStagesContext(ctx); err != nil {
			return nil, err
		}
		qwenRoot := agentqwen.ConsoleRootForStore(store)
		if err := agentqwen.ReconcileConsoleCredentialRemovalsIn(qwenRoot, func(accountID string) (bool, error) {
			_, found, err := store.FindStored(accountID)
			return found, err
		}); err != nil {
			return nil, err
		}
	}
	rollbackActive := rollbackErr != nil
	if !rollbackActive {
		if err := recoverPendingCodexAttestations(store); err != nil {
			return nil, err
		}
	}
	rollbackActive, err = accountRollbackActive(store.StoreDir())
	if err != nil {
		return nil, err
	}
	var loaded []accounts.Account
	if !rollbackActive {
		loaded, err = loadAccountRefAccounts(store, claudeStore, configuredSources)
		if err != nil {
			return nil, err
		}
	}
	diskGeneration, err := readAccountDiskGeneration(store.StoreDir())
	if err != nil {
		return nil, err
	}
	return &AccountRef{
		accounts:        loaded,
		diskGeneration:  diskGeneration,
		store:           store,
		claudeStore:     claudeStore,
		oauthSources:    configuredSources,
		client:          client,
		qwenConsoleRoot: agentqwen.ConsoleRootForStore(store),
	}, nil
}

func loadAccountRefAccounts(store accounts.CodexStore, claudeStore agentclaude.Store, sources []OAuthAccountSource) ([]accounts.Account, error) {
	loaded, err := store.List()
	if err != nil {
		return nil, err
	}
	claudeAccounts, err := claudeStore.ListAccounts(context.Background())
	if err != nil {
		return nil, err
	}
	loaded = append(loaded, claudeAccounts...)
	for _, source := range sources {
		sourceAccounts, err := source.ListAccounts(context.Background())
		loaded = append(loaded, sourceAccounts...)
		if err != nil {
			slog.Warn("OAuth account source unavailable; keeping other accounts", "provider", source.Provider(), "error", err)
			continue
		}
	}
	return loaded, nil
}

func (r *AccountRef) All() []accounts.Account {
	accounts, _ := r.Snapshot()
	return accounts
}

func (r *AccountRef) Snapshot() ([]accounts.Account, uint64) {
	if r == nil {
		return nil, 0
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return append([]accounts.Account(nil), r.accounts...), r.accountGeneration
}

func (r *AccountRef) CredentialSnapshot() ([]accounts.Account, uint64, uint64) {
	if r == nil {
		return nil, 0, 0
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return append([]accounts.Account(nil), r.accounts...), r.accountGeneration, r.credentialRevision
}

func (r *AccountRef) Generation() uint64 {
	if r == nil {
		return 0
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.accountGeneration
}

func (r *AccountRef) Reload() ([]accounts.Account, error) {
	loaded, _, err := r.ReloadSnapshot()
	return loaded, err
}

func (r *AccountRef) ReloadSnapshot() ([]accounts.Account, uint64, error) {
	if r == nil {
		return nil, 0, nil
	}
	rollbackActive, err := accountRollbackActive(r.store.StoreDir())
	if err != nil {
		return nil, 0, err
	}
	var loaded []accounts.Account
	if !rollbackActive {
		loaded, err = loadAccountRefAccounts(r.store, r.claudeStore, r.oauthSources)
		if err != nil {
			return nil, 0, err
		}
	}
	diskGeneration, err := readAccountDiskGeneration(r.store.StoreDir())
	if err != nil {
		return nil, 0, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.accounts = append([]accounts.Account(nil), loaded...)
	r.accountGeneration++
	r.credentialRevision++
	r.diskGeneration = diskGeneration
	return append([]accounts.Account(nil), loaded...), r.accountGeneration, nil
}

func (r *AccountRef) Refresh(ctx context.Context, account accounts.Account) (accounts.Account, error) {
	if r == nil || account.AuthMode != accounts.AuthModeOAuth {
		return account, nil
	}
	if account.Provider == accounts.ProviderClaude {
		refreshed, _, err := r.claudeStore.RefreshAccountIfExpired(ctx, r.client, account)
		if err != nil {
			return account, err
		}
		r.replace(refreshed)
		return refreshed, nil
	}
	for _, source := range r.oauthSources {
		if source.Provider() != account.Provider {
			continue
		}
		refreshed, err := source.RefreshAccount(ctx, r.client, account)
		if err != nil {
			return account, err
		}
		r.replace(refreshed)
		return refreshed, nil
	}
	if account.Provider != "" && account.Provider != accounts.ProviderCodex {
		return account, nil
	}
	if accounts.CodexRefreshReason(ctx) == "" {
		ctx = accounts.WithCodexRefreshReason(ctx, "proxy.account-refresh")
	}
	stored, ok, err := r.store.FindStored(account.ID)
	if err != nil || !ok {
		return account, err
	}
	refreshed, _, err := r.store.RefreshStoredIfExpired(ctx, r.client, stored)
	if err != nil {
		return account, err
	}
	next, ok := refreshed.Account(refreshed.SourcePath(r.store))
	if !ok {
		return account, nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	replaced := false
	for i := range r.accounts {
		if sameProvider(r.accounts[i].Provider, account.Provider) && accountMatches(r.accounts[i], account.ID) {
			if r.accounts[i].CredentialIdentity() != next.CredentialIdentity() {
				r.credentialRevision++
			}
			r.accounts[i] = next
			replaced = true
			break
		}
	}
	if !replaced {
		r.accounts = append(r.accounts, next)
		r.credentialRevision++
	}
	return next, nil
}

func (r *AccountRef) Statuses(ctx context.Context, forceRefresh bool) []AccountStatus {
	if r == nil {
		return nil
	}
	storedAccounts, err := r.store.ListStored()
	if err != nil {
		return []AccountStatus{{
			Provider:    accounts.ProviderCodex,
			AuthChecked: true,
			AuthValid:   false,
			Error:       err.Error(),
		}}
	}
	out := make([]AccountStatus, 0, len(storedAccounts))
	for _, stored := range storedAccounts {
		provider := stored.ProviderOrDefault()
		status := AccountStatus{
			ID:       stored.Email,
			Provider: provider,
			Email:    stored.Email,
			Source:   stored.SourcePath(r.store),
		}
		if stored.IsAPIKey() {
			status.AuthMode = accounts.AuthModeAPIKey
			out = append(out, status)
			continue
		}
		status.AuthMode = accounts.AuthModeOAuth
		status.AuthChecked = true
		refreshCtx := accounts.WithCodexRefreshReason(ctx, "account-status.if-expired")
		if forceRefresh {
			refreshCtx = accounts.WithCodexRefreshReason(ctx, "account-status.force")
		}
		refreshed := stored
		didRefresh := false
		var refreshErr error
		if forceRefresh {
			refreshed, didRefresh, refreshErr = r.store.RefreshStored(refreshCtx, r.client, stored)
		} else {
			refreshed, didRefresh, refreshErr = r.store.RefreshStoredIfExpired(refreshCtx, r.client, stored)
		}
		if refreshErr != nil {
			status.AuthValid = false
			status.Error = refreshErr.Error()
			out = append(out, status)
			continue
		}
		status.AuthValid = true
		status.Refreshed = didRefresh
		if account, ok := refreshed.Account(refreshed.SourcePath(r.store)); ok {
			r.replace(account)
		}
		out = append(out, status)
	}
	for _, profile := range r.claudeStore.ListProfiles() {
		status := AccountStatus{
			ID:          profile.Name,
			Provider:    accounts.ProviderClaude,
			AuthMode:    accounts.AuthModeOAuth,
			Email:       claudeProfileEmail(profile.Name),
			Source:      r.claudeStore.ClaudeConfigDir(profile.Name),
			AuthChecked: true,
		}
		var account accounts.Account
		var didRefresh bool
		var err error
		if forceRefresh {
			account, didRefresh, err = r.claudeStore.ForceRefreshCredential(ctx, r.client, profile)
		} else {
			account, didRefresh, err = r.claudeStore.RefreshCredentialIfExpired(ctx, r.client, profile)
		}
		if err != nil {
			status.AuthValid = false
			status.Error = err.Error()
			out = append(out, status)
			continue
		}
		status.AuthValid = true
		status.Refreshed = didRefresh
		r.replace(account)
		out = append(out, status)
	}
	return out
}

const usageStatusCacheTTL = 30 * time.Second
const usageStatusLastGoodTTL = 15 * time.Minute

// UsageStatuses serves a cached sweep when one is fresh, and backfills
// accounts whose live usage fetch transiently failed (the upstream usage
// endpoints rate-limit bursts) with their last-known-good windows, so brief
// 429s do not blank a healthy account's quota display.
func (r *AccountRef) UsageStatuses(ctx context.Context) []AccountUsageStatus {
	if r == nil {
		return nil
	}
	r.usageStatusMu.Lock()
	defer r.usageStatusMu.Unlock()
	if !r.usageStatusAt.IsZero() && time.Since(r.usageStatusAt) < usageStatusCacheTTL && r.usageStatusCache != nil {
		return append([]AccountUsageStatus(nil), r.usageStatusCache...)
	}
	out := r.usageStatusesLive(ctx)
	now := time.Now()
	if r.lastGoodUsage == nil {
		r.lastGoodUsage = map[string]usageStatusSnapshot{}
	}
	for i := range out {
		status := out[i]
		key := status.ID + "\x00" + string(status.Provider)
		if status.QuotaUsageKnown {
			// A successful Qwen response may intentionally contain no windows.
			// Record that empty result instead of resurrecting older limits.
			r.lastGoodUsage[key] = usageStatusSnapshot{status: status, at: now}
			continue
		}
		if len(status.Windows) > 0 {
			r.lastGoodUsage[key] = usageStatusSnapshot{status: status, at: now}
			continue
		}
		if authLikeUsageError(status.Error) {
			continue
		}
		snapshot, ok := r.lastGoodUsage[key]
		if !ok || now.Sub(snapshot.at) > usageStatusLastGoodTTL {
			continue
		}
		restored := snapshot.status
		restored.Active = status.Active
		restored.Refreshed = status.Refreshed
		restored.Error = ""
		restored.UsageFresh = false
		out[i] = restored
	}
	r.usageStatusCache = append([]AccountUsageStatus(nil), out...)
	r.usageStatusAt = now
	return out
}

func (r *AccountRef) InvalidateUsageStatusCache() {
	if r == nil {
		return
	}
	r.usageStatusMu.Lock()
	defer r.usageStatusMu.Unlock()
	r.usageStatusAt = time.Time{}
}

func authLikeUsageError(message string) bool {
	lower := strings.ToLower(message)
	for _, marker := range []string{"401", "403", "unauthorized", "forbidden", "invalid_grant", "no access token", "no usable credential"} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func apiKeyPlanType(provider accounts.Provider) string {
	if entry, ok := keyedProviderForName(string(provider)); ok {
		return entry.PlanLabel
	}
	return "api key"
}

func (r *AccountRef) keyedAPIUsageStatus(ctx context.Context, stored accounts.StoredCodexAccount, status AccountUsageStatus) AccountUsageStatus {
	provider := accountProviderFor(stored.ProviderOrDefault())
	account, ok := stored.Account(stored.SourcePath(r.store))
	if !ok {
		status.Error = "API-key account has no credential"
		return status
	}
	probe := ProbeProviderKeyStatus(ctx, r.client, provider, r.apiKeyUpstream(provider), account.Token)
	status.ProviderHealth = probe.State
	if probe.Models >= 0 {
		models := probe.Models
		status.ProviderModels = &models
	}
	status.AuthChecked = probe.State != ""
	status.AuthValid = probe.State == "auth ok"
	status.QuotaStatus = probe.QuotaStatus
	status.QuotaUsageKnown = probe.QuotaUsageKnown
	status.Windows = probe.Windows
	status.Credits = probe.Credits
	status.UsageFresh = probe.QuotaUsageKnown

	switch provider {
	case accounts.ProviderQwenToken:
		status.AccountIdentity = agentqwen.ConsoleAccountIn(r.qwenRoot(), stored.Email)
		hasCredential, credentialErr := agentqwen.HasConsoleCredentialIn(r.qwenRoot(), stored.Email)
		if credentialErr != nil {
			status.QuotaStatus = "error"
			status.Error = credentialErr.Error()
			return status
		}
		if !hasCredential {
			status.QuotaStatus = "login needed"
			return status
		}
		usage, usageErr := agentqwen.FetchUsageIn(ctx, r.client, r.qwenRoot(), stored.Email)
		subscription, subscriptionErr := agentqwen.FetchSubscriptionIn(ctx, r.client, r.qwenRoot(), stored.Email)
		if subscriptionErr == nil {
			status.PlanType = subscription.Plan
			if status.AccountIdentity == "" {
				status.AccountIdentity = subscription.InstanceCode
			}
			if subscription.Status != "" && subscription.Status != "valid" {
				status.QuotaStatus = subscription.Status
			} else {
				status.QuotaStatus = "live"
			}
		}
		if usageErr == nil {
			status.QuotaUsageKnown = true
			if usage.FiveHour != nil {
				status.Windows = append(status.Windows, *usage.FiveHour)
			}
			if usage.Weekly != nil {
				status.Windows = append(status.Windows, *usage.Weekly)
			}
			status.UsageFresh = true
		}
		if usageErr != nil || subscriptionErr != nil {
			combinedErr := agentqwen.StatusError(stored.Email, usageErr, subscriptionErr)
			status.Error = combinedErr.Error()
			if errors.Is(combinedErr, agentqwen.ErrConsoleLoginRequired) {
				status.QuotaStatus = "login needed"
			} else if usageErr != nil && subscriptionErr != nil {
				status.QuotaStatus = "error"
			} else if subscriptionErr != nil || subscription.Status == "" || subscription.Status == "valid" {
				status.QuotaStatus = "partial"
			}
		}
	case accounts.ProviderKimi:
		if !providerUpstreamUsesDefaultAuthority(provider, r.apiKeyUpstream(provider)) {
			break
		}
		plan, windows, usageErr := r.kimiStore().FetchUsage(ctx, r.client, account)
		if usageErr == nil {
			status.PlanType = plan + " key"
			status.QuotaStatus = "live"
			status.QuotaUsageKnown = true
			status.Windows = windows
			status.UsageFresh = true
		}
	}
	return status
}

func acquireAccountFetchSlot(ctx context.Context, sem chan struct{}) bool {
	if ctx.Err() != nil {
		return false
	}
	select {
	case sem <- struct{}{}:
		if ctx.Err() != nil {
			<-sem
			return false
		}
		return true
	case <-ctx.Done():
		return false
	}
}

func (r *AccountRef) usageStatusesLive(ctx context.Context) []AccountUsageStatus {
	storedAccounts, err := r.store.ListStored()
	if err != nil {
		return []AccountUsageStatus{{
			AccountStatus: AccountStatus{
				Provider:    accounts.ProviderCodex,
				AuthChecked: true,
				AuthValid:   false,
				Error:       err.Error(),
			},
		}}
	}
	active, _ := r.store.DetectActiveAccount()
	claudeProfiles := r.claudeStore.ListProfiles()
	claudeOffset := len(storedAccounts)
	out := make([]AccountUsageStatus, claudeOffset+len(claudeProfiles))
	// The deadline intentionally covers the whole pool rather than each queued
	// account. Status latency must stay bounded as pools grow; entries that do
	// not acquire a slot retain the identity/status seeded below and are retried
	// by the next sweep instead of extending this request by another batch.
	sweepCtx, cancelSweep := context.WithTimeout(ctx, usageStatusFetchTimeout)
	defer cancelSweep()
	var wg sync.WaitGroup
	sem := make(chan struct{}, accountFetchConcurrency)
	for i, stored := range storedAccounts {
		i, stored := i, stored
		provider := stored.ProviderOrDefault()
		status := AccountUsageStatus{
			AccountStatus: AccountStatus{
				ID:       stored.Email,
				Provider: provider,
				Email:    stored.Email,
				Source:   stored.SourcePath(r.store),
			},
			Active: stored.Email == active,
		}
		if stored.IsAPIKey() {
			status.AuthMode = accounts.AuthModeAPIKey
			status.KeyFingerprint = accounts.APIKeyFingerprint(stored.Auth.OpenAIAPIKey)
			status.PlanType = apiKeyPlanType(provider)
			if metering := ProviderMetering(provider); metering != "" {
				status.PlanType = metering
			}
			requestedEntry, keyedProvider := keyedProviderForName(string(provider))
			if keyedProvider {
				status.Provider = accountProviderFor(requestedEntry.Provider)
				status.ProviderEndpoints = ProviderEndpoints(requestedEntry.Provider)
			}
			out[i] = status
			if keyedProvider {
				wg.Add(1)
				go func() {
					defer wg.Done()
					if !acquireAccountFetchSlot(sweepCtx, sem) {
						next := out[i]
						next.Error = sweepCtx.Err().Error()
						out[i] = next
						return
					}
					defer func() { <-sem }()
					out[i] = r.keyedAPIUsageStatus(sweepCtx, stored, out[i])
				}()
			}
			continue
		}
		status.AuthMode = accounts.AuthModeOAuth
		status.AuthChecked = true
		credential := accounts.Account{ID: stored.Email, Provider: provider, AuthMode: accounts.AuthModeOAuth}
		if stored.Auth.Tokens != nil {
			credential.Token = stored.Auth.Tokens.AccessToken
			credential.CredentialVersion = accountpkg.OAuthCredentialVersion(
				stored.Auth.Tokens.AccessToken, stored.Auth.Tokens.RefreshToken,
			)
		}
		if failure, dead := r.terminalCredFailure(credential); dead {
			status.AuthValid = false
			status.Error = failure
			out[i] = status
			continue
		}
		out[i] = status
		wg.Add(1)
		go func() {
			defer wg.Done()
			if !acquireAccountFetchSlot(sweepCtx, sem) {
				next := out[i]
				next.Error = sweepCtx.Err().Error()
				out[i] = next
				return
			}
			defer func() { <-sem }()
			refreshCtx := accounts.WithCodexRefreshReason(sweepCtx, "usage-status.if-expired")
			refreshed, didRefresh, refreshErr := r.store.RefreshStoredIfExpired(refreshCtx, r.client, stored)
			r.noteCredResult(credential, refreshErr)
			next := out[i]
			next.Refreshed = didRefresh
			if refreshErr != nil {
				next.AuthValid = false
				next.Error = refreshErr.Error()
				out[i] = next
				return
			}
			next.AuthValid = true
			account, ok := refreshed.Account(refreshed.SourcePath(r.store))
			if !ok {
				next.Error = "account has no access token"
				out[i] = next
				return
			}
			r.replace(account)
			details, err := accounts.FetchCodexUsageDetails(sweepCtx, r.client, account)
			if err != nil {
				next.Error = err.Error()
				out[i] = next
				return
			}
			next.PlanType = details.PlanType
			next.Windows = details.Windows
			next.Credits = details.Credits
			next.ComplimentaryReset = details.ComplimentaryReset
			next.UsageFresh = true
			out[i] = next
		}()
	}
	activeClaude := r.claudeStore.ActiveProfile()
	for i, profile := range claudeProfiles {
		i, profile := claudeOffset+i, profile
		status := AccountUsageStatus{
			AccountStatus: AccountStatus{
				ID:          profile.Name,
				Provider:    accounts.ProviderClaude,
				AuthMode:    accounts.AuthModeOAuth,
				Email:       claudeProfileEmail(profile.Name),
				Source:      r.claudeStore.ClaudeConfigDir(profile.Name),
				AuthChecked: true,
			},
			Active:   profile.Name == activeClaude,
			PlanType: "unknown",
		}
		credential := accounts.Account{ID: profile.Name, Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth}
		if current, err := r.claudeStore.ReadCredential(ctx, r.claudeStore.ClaudeConfigDir(profile.Name)); err == nil && current != nil {
			credential.Token = current.AccessToken
			credential.CredentialVersion = accountpkg.OAuthCredentialVersion(
				current.AccessToken, current.RefreshToken,
			)
		}
		if failure, dead := r.terminalCredFailure(credential); dead {
			status.AuthValid = false
			status.Error = failure
			out[i] = status
			continue
		}
		out[i] = status
		wg.Add(1)
		go func() {
			defer wg.Done()
			if !acquireAccountFetchSlot(sweepCtx, sem) {
				next := out[i]
				next.Error = sweepCtx.Err().Error()
				out[i] = next
				return
			}
			defer func() { <-sem }()
			account, details, didRefresh, err := r.claudeStore.RefreshCredentialDetailsIfExpired(sweepCtx, r.client, profile)
			r.noteCredResult(credential, err)
			next := out[i]
			next.Refreshed = didRefresh
			if err != nil {
				next.AuthValid = false
				next.Error = err.Error()
				out[i] = next
				return
			}
			next.AuthValid = true
			next.PlanType = details.PlanType()
			r.replace(account)
			windows, fresh, err := r.FetchUsageWindowsCached(sweepCtx, r.client, account)
			if err != nil {
				next.Error = err.Error()
				out[i] = next
				return
			}
			next.Windows = windows
			next.UsageFresh = fresh
			out[i] = next
		}()
	}
	sourceBatches := make([][]AccountUsageStatus, len(r.oauthSources))
	for sourceIndex, source := range r.oauthSources {
		sourceIndex, source := sourceIndex, source
		wg.Add(1)
		go func() {
			defer wg.Done()
			if !acquireAccountFetchSlot(sweepCtx, sem) {
				sourceBatches[sourceIndex] = []AccountUsageStatus{{AccountStatus: AccountStatus{
					ID:          string(source.Provider()),
					Provider:    source.Provider(),
					AuthMode:    accounts.AuthModeOAuth,
					AuthChecked: true,
					Error:       sweepCtx.Err().Error(),
				}}}
				return
			}
			sourceAccounts, listErr := source.ListAccounts(sweepCtx)
			<-sem
			errorRows := 0
			if listErr != nil {
				errorRows = 1
			}
			batch := make([]AccountUsageStatus, errorRows+len(sourceAccounts))
			if listErr != nil {
				batch[0] = AccountUsageStatus{AccountStatus: AccountStatus{
					ID:          string(source.Provider()),
					Provider:    source.Provider(),
					AuthMode:    accounts.AuthModeOAuth,
					AuthChecked: true,
					Error:       listErr.Error(),
				}}
			}
			usageSource, hasUsage := source.(OAuthUsageSource)
			var accountWG sync.WaitGroup
			for accountIndex, sourceAccount := range sourceAccounts {
				accountIndex, sourceAccount := errorRows+accountIndex, sourceAccount
				batch[accountIndex] = AccountUsageStatus{
					AccountStatus: AccountStatus{
						ID:          sourceAccount.ID,
						Provider:    sourceAccount.Provider,
						AuthMode:    accounts.AuthModeOAuth,
						Email:       sourceAccount.Email,
						Source:      sourceAccount.Source,
						AuthChecked: true,
					},
					AccountIdentity: sourceAccount.Label,
				}
				accountWG.Add(1)
				go func() {
					defer accountWG.Done()
					status := batch[accountIndex]
					if !acquireAccountFetchSlot(sweepCtx, sem) {
						status.Error = sweepCtx.Err().Error()
						batch[accountIndex] = status
						return
					}
					defer func() { <-sem }()
					refreshed, refreshErr := source.RefreshAccount(sweepCtx, r.client, sourceAccount)
					if refreshErr != nil {
						status.Error = refreshErr.Error()
						batch[accountIndex] = status
						return
					}
					status.AuthValid = true
					r.replace(refreshed)
					if !hasUsage {
						status.PlanType = "subscription"
						batch[accountIndex] = status
						return
					}
					var planType string
					var windows []accounts.UsageWindow
					var usageErr error
					if identitySource, ok := usageSource.(OAuthIdentityUsageSource); ok {
						var verifiedEmail string
						verifiedEmail, planType, windows, usageErr = identitySource.FetchUsageIdentity(sweepCtx, r.client, refreshed)
						if verifiedEmail != "" {
							status.Email = verifiedEmail
							status.AccountIdentity = verifiedEmail
						}
						if usageErr != nil {
							// Identity-aware telemetry is an optional read-only probe.
							// Credential refresh above remains the routing-health gate.
							status.QuotaStatus = "unavailable"
						}
					} else {
						planType, windows, usageErr = usageSource.FetchUsage(sweepCtx, r.client, refreshed)
					}
					status.PlanType = planType
					status.Windows = windows
					status.QuotaUsageKnown = len(windows) > 0
					status.UsageFresh = usageErr == nil
					if usageErr != nil && status.QuotaStatus == "" {
						status.Error = usageErr.Error()
					}
					batch[accountIndex] = status
				}()
			}
			accountWG.Wait()
			sourceBatches[sourceIndex] = batch
		}()
	}
	wg.Wait()
	promoteUsableClaudeStatus(out[claudeOffset:])
	for _, batch := range sourceBatches {
		out = append(out, batch...)
	}
	return out
}

func claudeProfileEmail(name string) string {
	if strings.Contains(name, "@") {
		return name
	}
	return ""
}

// claudePoolModel canonicalizes a Claude request model to the quota-pool
// family stamped on usage windows, so "claude-opus-4-8" and
// "claude-opus-4-8[1m]" both resolve to the opus weekly pool. Non-Claude and
// unrecognized models pass through unchanged (strict generic matching).
func claudePoolModel(model string) string {
	lower := strings.ToLower(model)
	switch {
	case strings.Contains(lower, "fable"):
		return agentclaude.FableFeature
	case strings.Contains(lower, "opus"):
		return agentclaude.OpusFeature
	case strings.Contains(lower, "sonnet"):
		return agentclaude.SonnetFeature
	}
	return model
}

// antigravityPoolModel maps vendor model names onto the two independent quota
// families published by Antigravity. Unknown names stay unpooled so a future
// model cannot be denied merely because this client has not learned its name.
func antigravityFamilyPoolModel(model string) string {
	lower := strings.ToLower(strings.TrimSpace(model))
	switch {
	case strings.Contains(lower, "gemini"):
		return "gemini"
	case strings.Contains(lower, "claude"), strings.Contains(lower, "gpt"), strings.Contains(lower, "openai"):
		return "claude-gpt"
	default:
		return model
	}
}

func antigravityPoolModel(scheduler selectacct.Scheduler, model string) string {
	if scheduler.HasModelPoolFor(accounts.ProviderAntigravity, model) {
		return model
	}
	return antigravityFamilyPoolModel(model)
}

func claudeUsageWindows(usage *agentclaude.UsageResponse) []accounts.UsageWindow {
	if usage == nil {
		return nil
	}
	var windows []accounts.UsageWindow
	// LimitWindowSeconds lets the scheduler classify a window as "short"
	// (<= 6h) versus account-wide. Anthropic's usage endpoint reports
	// utilization and a reset time but not the window length, so we encode the
	// known fixed lengths here. Without this the 5h window is never recognized
	// as short, so ShortHeadroom/ShortResetAfterSeconds/ExpiryPressure stay
	// dead for every Claude account and the GTO routing calculation ignores the
	// 5h rate limit (the one that actually bites mid-session).
	add := func(name string, windowSeconds int64, limit *agentclaude.RateLimit) {
		if limit == nil || limit.Utilization == nil {
			return
		}
		window := accounts.UsageWindow{
			Name:               name,
			UsedPercent:        *limit.Utilization,
			LimitWindowSeconds: windowSeconds,
		}
		switch name {
		case agentclaude.FableWindowName:
			window.Feature = agentclaude.FableFeature
		case "opus-weekly":
			window.Feature = agentclaude.OpusFeature
		case "sonnet-weekly":
			window.Feature = agentclaude.SonnetFeature
		}
		if reset, err := time.Parse(time.RFC3339, limit.ResetsAt); err == nil {
			seconds := int64(time.Until(reset).Seconds())
			if seconds < 0 {
				seconds = 0
			}
			window.ResetAfterSeconds = seconds
		}
		windows = append(windows, window)
	}
	const (
		fiveHourSeconds = int64(5 * 60 * 60)
		sevenDaySeconds = int64(7 * 24 * 60 * 60)
	)
	add("5h", fiveHourSeconds, usage.FiveHour)
	add("7d", sevenDaySeconds, usage.SevenDay)
	add(agentclaude.FableWindowName, sevenDaySeconds, usage.SevenDayOAuthApps)
	add("opus-weekly", sevenDaySeconds, usage.SevenDayOpus)
	add("sonnet-weekly", sevenDaySeconds, usage.SevenDaySonnet)
	// The usage endpoint reports opus/sonnet weekly buckets as null until the
	// account has used that model family. Null means "unused", not "cannot
	// serve": emit a 0%-used window so ForModel never zero-fills a fresh
	// account out of opus/sonnet routing once other accounts grow real
	// windows.
	if !usageWindowNamed(windows, "opus-weekly") {
		windows = append(windows, accounts.UsageWindow{Name: "opus-weekly", LimitWindowSeconds: sevenDaySeconds, Feature: agentclaude.OpusFeature})
	}
	if !usageWindowNamed(windows, "sonnet-weekly") {
		windows = append(windows, accounts.UsageWindow{Name: "sonnet-weekly", LimitWindowSeconds: sevenDaySeconds, Feature: agentclaude.SonnetFeature})
	}
	if usage.ExtraUsage != nil && usage.ExtraUsage.IsEnabled && usage.ExtraUsage.Utilization != nil {
		windows = append(windows, accounts.UsageWindow{Name: "extra", UsedPercent: *usage.ExtraUsage.Utilization})
	}
	return windows
}

func promoteUsableClaudeStatus(statuses []AccountUsageStatus) {
	activeUsable := false
	bestIdx := -1
	var best selectacct.Score
	for i := range statuses {
		status := &statuses[i]
		if status.Provider != accounts.ProviderClaude {
			continue
		}
		usable := false
		if status.AuthValid && status.Error == "" {
			score := scoreFromUsageWindows(status.Provider, status.ID, status.Windows)
			usable = scoreUsableForNewSession(score)
			if usable && (bestIdx == -1 || betterClaudeActiveCandidate(score, best)) {
				bestIdx = i
				best = score
			}
		}
		if status.Active && usable {
			activeUsable = true
		}
	}
	if activeUsable {
		return
	}
	for i := range statuses {
		if statuses[i].Provider == accounts.ProviderClaude {
			statuses[i].Active = false
		}
	}
	if bestIdx >= 0 {
		statuses[bestIdx].Active = true
	}
}

func betterClaudeActiveCandidate(left, right selectacct.Score) bool {
	if left.Headroom != right.Headroom {
		return left.Headroom > right.Headroom
	}
	if left.ShortHeadroom != right.ShortHeadroom {
		return left.ShortHeadroom > right.ShortHeadroom
	}
	if left.ShortResetAfterSeconds != right.ShortResetAfterSeconds {
		return left.ShortResetAfterSeconds > right.ShortResetAfterSeconds
	}
	return left.AccountID < right.AccountID
}

func scoreUsableForNewSession(score selectacct.Score) bool {
	return score.Headroom >= selectacct.MinNewSessionHeadroom && score.ShortHeadroom >= selectacct.MinNewSessionHeadroom
}

func scoreFromUsageWindows(provider accounts.Provider, accountID string, windows []accounts.UsageWindow) selectacct.Score {
	limitWindows := make([]selectacct.LimitWindow, 0, len(windows)*2)
	// A model-pool request (fable/opus/sonnet) consumes the account-wide 5h/7d
	// windows too, so every feature pool's score must also reflect the base
	// windows: duplicate each base window into every distinct feature present.
	// Without this a pool with quota left on an account whose 5h window is
	// cooked would still score high and route traffic into guaranteed 429s.
	var features []string
	if provider == accounts.ProviderClaude {
		seen := map[string]bool{}
		for _, window := range windows {
			if window.Feature != "" && !seen[window.Feature] {
				seen[window.Feature] = true
				features = append(features, window.Feature)
			}
		}
	}
	for _, window := range windows {
		limitWindows = append(limitWindows, selectacct.LimitWindow{
			Name:               window.Name,
			UsedPercent:        window.UsedPercent,
			LimitWindowSeconds: window.LimitWindowSeconds,
			ResetAfterSeconds:  window.ResetAfterSeconds,
			Feature:            window.Feature,
		})
		if window.Feature == "" {
			for _, feature := range features {
				limitWindows = append(limitWindows, selectacct.LimitWindow{
					Name:               window.Name,
					UsedPercent:        window.UsedPercent,
					LimitWindowSeconds: window.LimitWindowSeconds,
					ResetAfterSeconds:  window.ResetAfterSeconds,
					Feature:            feature,
				})
			}
		}
	}
	score := selectacct.ScoreFromLimitWindows(accountID, 0, limitWindows)
	score.Provider = provider
	return score
}

func (r *AccountRef) replace(account accounts.Account) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := range r.accounts {
		if sameProvider(r.accounts[i].Provider, account.Provider) && accountMatches(r.accounts[i], account.ID) {
			if r.accounts[i].CredentialIdentity() != account.CredentialIdentity() {
				r.credentialRevision++
			}
			r.accounts[i] = account
			return
		}
	}
	r.accounts = append(r.accounts, account)
	r.credentialRevision++
}

func (s Server) Handler() http.Handler {
	FreezeOpenAICompatibleProviders()
	if s.AccountRef != nil {
		s.AccountRef.configureAPIKeyUpstreams(s)
	}
	s.CredentialBroker = normalizedCredentialBroker(s.CredentialBroker)
	if s.ActiveSessions == nil {
		s.ActiveSessions = NewActiveSessions()
	}
	if s.Lifecycle == nil {
		s.Lifecycle = NewLifecycle()
	}
	if s.sessionLeases == nil {
		s.sessionLeases = newSessionLeaseStore()
	}
	if s.cutoverChallenges == nil {
		s.cutoverChallenges = newCutoverChallengeRegistry()
	}
	if s.CacheFlight == nil {
		s.CacheFlight = newSingleFlight()
	}
	if s.azureCodexSessions == nil {
		path := ""
		if s.AzureCodex != nil {
			path = s.AzureCodex.PinStorePath
		}
		s.azureCodexSessions = newPersistentAzureCodexSticky(path)
	}
	if s.azureCodexRejects == nil {
		s.azureCodexRejects = newAzureCodexFieldMemory()
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/internal/v1/session-leases", s.requireSessionLeaseAdmin(s.handleSessionLeases))
	mux.HandleFunc("/internal/v1/session-leases/", s.requireSessionLeaseAdmin(s.handleSessionLease))
	mux.HandleFunc("/_subrouter/health", s.handleHealth)
	mux.HandleFunc(StoreHandshakePath, s.handleStoreHandshake)
	mux.HandleFunc("/_subrouter/ready", s.handleReady)
	mux.HandleFunc("/_subrouter/stream-stats", s.handleStreamStats)
	mux.HandleFunc("/_subrouter/drain", s.requireAdmin(s.handleDrain))
	mux.HandleFunc("/_subrouter/drain-status", s.requireAdmin(s.handleDrainStatus))
	mux.HandleFunc("/_subrouter/quiesce", s.requireAdmin(s.handleQuiesce))
	mux.HandleFunc("/_subrouter/resume", s.requireAdmin(s.handleResume))
	mux.HandleFunc("/_subrouter/accounts", s.requireAdmin(s.handleAccounts))
	mux.HandleFunc("/_subrouter/account-status", s.requireAdmin(s.handleAccountStatus))
	mux.HandleFunc("/_subrouter/usage-status", s.requireAdmin(s.handleUsageStatus))
	mux.HandleFunc("/_subrouter/qwen-console", s.requireAccountImportAuth(s.handleQwenConsoleImport))
	mux.HandleFunc("/_subrouter/rate-limit-reset", s.requireAdmin(s.handleRateLimitReset))
	mux.HandleFunc("/_subrouter/reset-credits", s.requireAdmin(s.handleResetCredits))
	mux.HandleFunc("/_subrouter/reload-accounts", s.requireAdmin(s.handleReloadAccounts))
	mux.HandleFunc("/_subrouter/account-import", s.requireAccountImportAuth(s.handleAccountImport))
	mux.HandleFunc("/_subrouter/sessions", s.requireAdmin(s.handleSessions))
	mux.HandleFunc("/_subrouter/cutover-challenge", s.requireAdmin(s.handleCutoverChallenge))
	mux.HandleFunc("/_subrouter/dashboard", s.requireAdmin(s.handleDashboard))
	mux.HandleFunc("/_subrouter/transcripts", s.requireAdmin(s.handleTranscriptList))
	mux.HandleFunc("/_subrouter/transcripts/", s.requireAdmin(s.handleTranscriptDetail))
	mux.HandleFunc("/_subrouter/bedrock-cost", s.requireAdmin(s.handleBedrockCost))
	mux.HandleFunc("/_subrouter/azure-codex-cost", s.requireAdmin(s.handleAzureCodexCost))
	mux.HandleFunc("/_subrouter/", http.NotFound)
	if s.Bedrock != nil && !s.RequireSessionLease {
		mux.Handle("/bedrock/", s.bedrockHandler())
	}
	mux.Handle("/", s.proxyHandler())
	return mux
}

func normalizedCredentialBroker(value CredentialBroker) CredentialBroker {
	if client, ok := value.(*broker.Client); ok && client == nil {
		return nil
	}
	return value
}

func (s Server) handleHealth(w http.ResponseWriter, request *http.Request) {
	payload := map[string]any{
		"ok":             true,
		"account_import": s.AccountImportState(),
		"auth":           s.AuthMode(),
	}
	// Compatibility for v1 bindings and direct local daemons. v2 clients use
	// the mutually authenticated private-socket handshake and never accept this
	// legacy proof as a private-channel response.
	if s.LegacyStoreAttestation && s.AccountRef != nil {
		if challenge := request.Header.Get(accounts.StoreAuthorityChallengeHeader); challenge != "" {
			if proof, err := accounts.StoreAuthorityProof(s.AccountRef.store.Dir, challenge); err == nil {
				if authorityID, idErr := accounts.StoreAuthorityID(s.AccountRef.store.Dir); idErr == nil {
					payload["account_store_id"] = authorityID
					payload["account_store_proof"] = proof
				}
			}
		}
	}
	if proof, ok := s.shadowHealthProof(request.Header.Get(ShadowHealthChallengeHeader)); ok {
		payload[ShadowHealthProofField] = proof
	}
	// Whether the Codex Azure fallback is armed is otherwise invisible until a
	// pool outage, which is exactly when nobody wants to discover it was
	// misconfigured. Endpoint names only; keys never leave the process.
	if names := s.AzureCodex.endpointNames(); len(names) > 0 {
		payload["azure_codex"] = names
	}
	writeJSON(w, payload)
}

func (s Server) handleStoreHandshake(w http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost || s.AccountRef == nil {
		http.NotFound(w, request)
		return
	}
	nonce := request.Header.Get(accounts.StoreHandshakeNonceHeader)
	verified, err := accounts.VerifyStoreHandshakeRequest(
		s.AccountRef.store.Dir, nonce, request.Header.Get(accounts.StoreHandshakeRequestHeader),
	)
	if err != nil || !verified {
		http.NotFound(w, request)
		return
	}
	storeID, err := accounts.StoreAuthorityID(s.AccountRef.store.Dir)
	if err != nil {
		http.NotFound(w, request)
		return
	}
	proof, err := accounts.ExistingStoreHandshakeResponseProof(s.AccountRef.store.Dir, nonce)
	if err != nil {
		http.NotFound(w, request)
		return
	}
	authorizeLocalDataConnection(request)
	writeJSON(w, map[string]string{"account_store_id": storeID, "account_store_proof": proof})
}

func (s Server) shadowHealthProof(challengeHex string) (string, bool) {
	if len(s.ShadowHealthKey) != sha256.Size || len(challengeHex) != hex.EncodedLen(sha256.Size) {
		return "", false
	}
	challenge, err := hex.DecodeString(challengeHex)
	if err != nil || len(challenge) != sha256.Size {
		return "", false
	}
	mac := hmac.New(sha256.New, s.ShadowHealthKey)
	_, _ = mac.Write([]byte(shadowHealthDomain))
	_, _ = mac.Write(challenge)
	return hex.EncodeToString(mac.Sum(nil)), true
}

// AccountImportState reports whether this server can accept `sr add` uploads.
// Account import denies every request when no credential is configured, and
// nothing else on the server reflects that, so a host whose binary updated past
// the credential requirement while its configuration stayed behind looks
// healthy right up until someone tries to add an account.
func (s Server) AccountImportState() string {
	if s.tenantAccountImportAuthorized ||
		s.TailnetAuth != nil ||
		strings.TrimSpace(s.AccountImportToken) != "" ||
		strings.TrimSpace(s.AdminToken) != "" {
		return AccountImportEnabled
	}
	return AccountImportDisabled
}

// handleStreamStats reports dropped response streams grouped by which side
// ended them. A non-zero "proxy" count is the signal that subrouter dropped a
// stream while the client was still connected.
func (s Server) handleStreamStats(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, s.StreamDrops.Snapshot())
}

func (s Server) handleReady(w http.ResponseWriter, _ *http.Request) {
	if s.Lifecycle != nil && s.Lifecycle.Draining() {
		w.WriteHeader(http.StatusServiceUnavailable)
		writeJSON(w, map[string]any{"ok": false, "draining": true})
		return
	}
	if s.ReadyCheck != nil {
		if err := s.ReadyCheck(); err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			writeJSON(w, map[string]any{"ok": false, "draining": false, "startup_ready": false})
			return
		}
	}
	writeJSON(w, map[string]any{"ok": true, "draining": false})
}

func (s Server) handleDrain(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !isLoopbackRemote(r.RemoteAddr) {
		http.Error(w, "drain is only available from loopback", http.StatusForbidden)
		return
	}
	if s.Lifecycle != nil {
		s.Lifecycle.Drain()
	}
	writeJSON(w, s.lifecycleStatus(true))
}

func (s Server) handleDrainStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	writeJSON(w, s.lifecycleStatus(false))
}

func (s Server) handleQuiesce(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !isLoopbackRemote(r.RemoteAddr) {
		http.Error(w, "quiesce is only available from loopback", http.StatusForbidden)
		return
	}
	if s.Lifecycle != nil {
		s.Lifecycle.Quiesce()
	}
	writeJSON(w, s.lifecycleStatus(true))
}

func (s Server) handleResume(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !isLoopbackRemote(r.RemoteAddr) {
		http.Error(w, "resume is only available from loopback", http.StatusForbidden)
		return
	}
	if s.Lifecycle != nil {
		s.Lifecycle.Resume()
	}
	writeJSON(w, s.lifecycleStatus(true))
}

func (s Server) lifecycleStatus(ok bool) map[string]any {
	status := map[string]any{"ok": ok}
	for key, value := range s.Lifecycle.Status() {
		status[key] = value
	}
	return status
}

func (s Server) handleAccounts(w http.ResponseWriter, r *http.Request) {
	type safeAccount struct {
		ID       string            `json:"id"`
		Provider accounts.Provider `json:"provider"`
		AuthMode accounts.AuthMode `json:"auth_mode"`
		Label    string            `json:"label,omitempty"`
		Email    string            `json:"email,omitempty"`
		Source   string            `json:"source"`
	}
	availableAccounts := s.accountListContext(r.Context())
	out := make([]safeAccount, 0, len(availableAccounts))
	for _, account := range availableAccounts {
		out = append(out, safeAccount{
			ID:       account.ID,
			Provider: account.Provider,
			AuthMode: account.AuthMode,
			Label:    account.Label,
			Email:    account.Email,
			Source:   account.Source,
		})
	}
	writeJSON(w, out)
}

func (s Server) handleAccountStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	forceRefresh := r.Method == http.MethodPost
	if s.AccountRef != nil {
		statuses := s.AccountRef.Statuses(r.Context(), forceRefresh)
		if s.SchedulerRef != nil {
			loaded, generation, credentialRevision := s.AccountRef.CredentialSnapshot()
			s.SchedulerRef.SyncAccountCredentials(generation, credentialRevision, SchedulerAccounts(loaded))
		}
		writeJSON(w, statuses)
		return
	}
	accounts := s.accountListContext(r.Context())
	out := make([]AccountStatus, 0, len(accounts))
	for _, account := range accounts {
		out = append(out, AccountStatus{
			ID:       account.ID,
			Provider: account.Provider,
			AuthMode: account.AuthMode,
			Email:    account.Email,
			Source:   account.Source,
		})
	}
	writeJSON(w, out)
}

func (s Server) handleUsageStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.AccountRef != nil {
		scoreRevision := uint64(0)
		if s.SchedulerRef != nil {
			scoreRevision = s.SchedulerRef.ScoreRevision()
		}
		statuses := s.AccountRef.UsageStatuses(r.Context())
		if s.SchedulerRef != nil {
			loaded, generation, credentialRevision := s.AccountRef.CredentialSnapshot()
			s.SchedulerRef.SyncAccountCredentials(generation, credentialRevision, SchedulerAccounts(loaded))
		}
		s.updateSchedulerFromUsageStatusesAtScoreRevision(r.Context(), statuses, scoreRevision)
		writeJSON(w, s.withSessionCounts(s.withRequestTimeExhaustionWindows(statuses)))
		return
	}
	accounts := s.accountListContext(r.Context())
	out := make([]AccountUsageStatus, 0, len(accounts))
	for _, account := range accounts {
		out = append(out, AccountUsageStatus{
			AccountStatus: AccountStatus{
				ID:       account.ID,
				Provider: account.Provider,
				AuthMode: account.AuthMode,
				Email:    account.Email,
				Source:   account.Source,
			},
		})
	}
	out = s.withKeyedProviderHealth(r.Context(), out)
	writeJSON(w, s.withSessionCounts(s.withRequestTimeExhaustionWindows(out)))
}

func (s Server) handleQwenConsoleImport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var input struct {
		AccountID  string                      `json:"account_id"`
		Credential agentqwen.ConsoleCredential `json:"credential"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&input); err != nil {
		http.Error(w, "invalid Qwen console credential", http.StatusBadRequest)
		return
	}
	accountID := strings.TrimSpace(input.AccountID)
	found := false
	for _, account := range s.accountListContext(r.Context()) {
		if account.ID == accountID && account.Provider == accounts.ProviderQwenToken && account.AuthMode == accounts.AuthModeAPIKey {
			found = true
			break
		}
	}
	if !found {
		http.Error(w, "Qwen Token Plan account not found", http.StatusNotFound)
		return
	}
	if input.Credential.Account != "" && !validTenantAccountText(input.Credential.Account) {
		http.Error(w, "invalid Qwen console account label", http.StatusBadRequest)
		return
	}
	root := agentqwen.DefaultConsoleRoot()
	if s.AccountRef != nil {
		if err := saveQwenConsoleCredentialMutation(r.Context(), s.AccountRef, accountID, input.Credential); err != nil {
			http.Error(w, "could not save Qwen console credential", http.StatusBadRequest)
			return
		}
	} else if err := agentqwen.SaveConsoleCredentialIn(root, accountID, input.Credential); err != nil {
		http.Error(w, "could not save Qwen console credential", http.StatusBadRequest)
		return
	}
	if s.AccountRef != nil {
		s.AccountRef.InvalidateUsageStatusCache()
	}
	w.WriteHeader(http.StatusNoContent)
}

func saveQwenConsoleCredentialMutation(
	ctx context.Context,
	ref *AccountRef,
	accountID string,
	credential agentqwen.ConsoleCredential,
) (err error) {
	if err := lockMutexContext(ctx, &ref.installMu); err != nil {
		return err
	}
	defer ref.installMu.Unlock()
	transactionLock, err := lockAccountImportTransaction(ctx, ref.store.StoreDir())
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, transactionLock.Close()) }()
	if _, err := reconcileCompletedAccountRollback(ctx, ref.store, advanceAccountDiskGeneration); err != nil {
		return err
	}
	stored, found, err := ref.store.FindStored(accountID)
	if err != nil {
		return err
	}
	if !found || stored.Email != accountID || stored.ProviderOrDefault() != accounts.ProviderQwenToken || stored.IsAPIKey() == false {
		return errors.New("Qwen Token Plan account changed before console credential save")
	}
	if err := ref.advanceDiskGeneration(); err != nil {
		return err
	}
	return restoreTenantQwenConsoleDurably(ref.qwenRoot(), accountID, credential, syncAccountStateDir)
}

func (s Server) withSessionCounts(statuses []AccountUsageStatus) []AccountUsageStatus {
	if s.Sessions == nil {
		return statuses
	}
	counts := SchedulerSessionCounts(s.Sessions)
	out := append([]AccountUsageStatus(nil), statuses...)
	for i := range out {
		out[i].AssignedSessions = counts[selectacct.ScoreKey(accountProviderFor(out[i].Provider), out[i].ID)]
		out[i].SessionsKnown = true
		if out[i].Provider == accounts.ProviderKimi || out[i].Provider == accounts.ProviderAntigravity ||
			(out[i].Provider == accounts.ProviderGrok && out[i].AuthMode == accounts.AuthModeOAuth) {
			out[i].Active = out[i].AssignedSessions > 0
		}
	}
	return out
}

func (s Server) withKeyedProviderHealth(ctx context.Context, statuses []AccountUsageStatus) []AccountUsageStatus {
	out := append([]AccountUsageStatus(nil), statuses...)
	byID := make(map[string]accounts.Account)
	for _, account := range s.accountListContext(ctx) {
		byID[account.ID] = account
	}
	client := &http.Client{Timeout: 6 * time.Second}
	if s.AccountRef != nil && s.AccountRef.client != nil {
		client = s.AccountRef.client
	} else if s.Transport != nil {
		client.Transport = s.Transport
	}

	var wg sync.WaitGroup
	for i := range out {
		i := i
		status := out[i]
		if status.AuthMode != accounts.AuthModeAPIKey {
			continue
		}
		requestedEntry, ok := keyedProviderForName(string(status.Provider))
		if !ok {
			continue
		}
		owner := accountProviderFor(requestedEntry.Provider)
		healthEntry, ok := keyedProviderFor(owner)
		if !ok {
			continue
		}
		out[i].Provider = owner
		if strings.TrimSpace(out[i].PlanType) == "" {
			out[i].PlanType = healthEntry.PlanLabel
		}
		out[i].ProviderHealth = "not checked"
		out[i].ProviderEndpoints = ProviderEndpoints(requestedEntry.Provider)
		account, ok := byID[status.ID]
		if !ok || strings.TrimSpace(account.Token) == "" || healthEntry.Upstream == nil {
			continue
		}
		upstream := healthEntry.Upstream(s, account.AuthMode)
		if upstream == nil || ProviderHealthURL(healthEntry.Provider, upstream.String()) == "" {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			probeCtx, cancel := context.WithTimeout(ctx, 6*time.Second)
			defer cancel()
			state, models := ProbeProviderKey(probeCtx, client, healthEntry.Provider, upstream.String(), account.Token)
			if state == "" {
				return
			}
			out[i].ProviderHealth = state
			out[i].ProviderModels = &models
		}()
	}
	wg.Wait()
	return out
}

func (s Server) updateSchedulerFromUsageStatuses(statuses []AccountUsageStatus) {
	s.updateSchedulerFromUsageStatusesContext(context.Background(), statuses)
}

func (s Server) updateSchedulerFromUsageStatusesContext(ctx context.Context, statuses []AccountUsageStatus) {
	scoreRevision := uint64(0)
	if s.SchedulerRef != nil {
		scoreRevision = s.SchedulerRef.ScoreRevision()
	}
	s.updateSchedulerFromUsageStatusesAtScoreRevision(ctx, statuses, scoreRevision)
}

func (s Server) updateSchedulerFromUsageStatusesAtScoreRevision(
	ctx context.Context,
	statuses []AccountUsageStatus,
	scoreRevision uint64,
) {
	if s.SchedulerRef == nil {
		return
	}
	allAccounts, accountGeneration := s.accountListSnapshotContext(ctx)
	available := oauthAccounts(allAccounts)
	if len(available) == 0 {
		return
	}
	current := s.Scheduler
	if s.SchedulerRef != nil {
		current = s.SchedulerRef.RefreshSeed()
	}
	scores := make([]selectacct.Score, 0, len(available))
	scoreByID := make(map[string]int, len(available))
	for _, account := range available {
		scoreProvider := schedulerAccountProvider(account.Provider)
		seed := current.ScoreFor(scoreProvider, account.ID)
		seed.AccountID = account.ID
		seed.Provider = scoreProvider
		seed.Fresh = false
		scoreByID[selectacct.ScoreKey(scoreProvider, account.ID)] = len(scores)
		scores = append(scores, seed)
	}
	scored := 0
	for _, status := range statuses {
		if !status.UsageFresh || status.AuthMode != accounts.AuthModeOAuth || len(status.Windows) == 0 {
			continue
		}
		statusProvider := schedulerAccountProvider(status.Provider)
		if idx, ok := scoreByID[selectacct.ScoreKey(statusProvider, status.ID)]; ok {
			next := scoreFromUsageWindows(statusProvider, status.ID, status.Windows)
			next.Fresh = true
			scores[idx] = next
			scored++
		}
	}
	if scored == 0 {
		return
	}
	scheduler := selectacct.NewScheduler(scores)
	if s.Sessions != nil {
		scheduler = scheduler.WithSessionCounts(SchedulerSessionCounts(s.Sessions))
	}
	s.SchedulerRef.SetForAccountGenerationAtScoreRevision(scheduler, accountGeneration, scoreRevision)
}

func (s Server) withRequestTimeExhaustionWindows(statuses []AccountUsageStatus) []AccountUsageStatus {
	if s.SchedulerRef == nil {
		return statuses
	}
	now := time.Now()
	out := append([]AccountUsageStatus(nil), statuses...)
	for i := range out {
		status := &out[i]
		if status.AuthMode != accounts.AuthModeOAuth {
			continue
		}
		until, ok := s.SchedulerRef.ExhaustedUntilFor(status.Provider, status.ID, "")
		if !ok || !until.After(now) {
			continue
		}
		resetAfter := int64(until.Sub(now).Seconds())
		if resetAfter < 0 {
			resetAfter = 0
		}
		name := "request-limit"
		windowSeconds := int64(7 * 24 * 60 * 60)
		feature := ""
		if status.Provider == accounts.ProviderClaude {
			name = agentclaude.FableWindowName
			feature = agentclaude.FableModel
		}
		if usageWindowNamed(status.Windows, name) {
			continue
		}
		status.Windows = append(append([]accounts.UsageWindow(nil), status.Windows...), accounts.UsageWindow{
			Name:               name,
			UsedPercent:        100,
			LimitWindowSeconds: windowSeconds,
			ResetAfterSeconds:  resetAfter,
			Feature:            feature,
		})
	}
	return out
}

func usageWindowNamed(windows []accounts.UsageWindow, name string) bool {
	for _, window := range windows {
		if window.Name == name {
			return true
		}
	}
	return false
}

func (s Server) handleReloadAccounts(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !isLoopbackRemote(r.RemoteAddr) {
		http.Error(w, "reload-accounts is only available from loopback", http.StatusForbidden)
		return
	}
	loaded, scored, err := s.reloadAccounts(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.AccountRef.InvalidateUsageStatusCache()
	writeJSON(w, map[string]any{
		"ok":              true,
		"accounts":        loaded,
		"usage_refreshed": scored,
	})
}

const (
	maxAccountImportBodyBytes int64 = 128 << 10
	maxAccountImportAccounts        = 256
)

type accountImportRequest struct {
	Provider    accounts.Provider            `json:"provider"`
	Codex       *accounts.StoredCodexAccount `json:"codex,omitempty"`
	Claude      *claudeAccountImport         `json:"claude,omitempty"`
	Kimi        *kimiAccountImport           `json:"kimi,omitempty"`
	Antigravity *antigravityAccountImport    `json:"antigravity,omitempty"`
}

type antigravityAccountImport struct {
	Label      string                          `json:"label"`
	Credential agentantigravity.CredentialInfo `json:"credential,omitempty"`
	Remove     bool                            `json:"remove,omitempty"`
}

type claudeAccountImport struct {
	Name       string                     `json:"name"`
	Credential agentclaude.CredentialInfo `json:"credential"`
}

type kimiAccountImport struct {
	Label      string                   `json:"label"`
	Credential agentkimi.CredentialInfo `json:"credential,omitempty"`
	Remove     bool                     `json:"remove,omitempty"`
}

func (s Server) handleAccountImport(w http.ResponseWriter, r *http.Request) {
	if s.AccountRef == nil || s.CredentialBroker != nil {
		http.Error(w, "account import is not configured", http.StatusServiceUnavailable)
		return
	}
	switch r.Method {
	case http.MethodGet:
		providers := append([]string{"codex", "claude"}, keyedProviderNames()...)
		if _, configured := s.AccountRef.antigravityStore(); configured {
			providers = append(providers, string(accounts.ProviderAntigravity))
		}
		writeJSON(w, map[string]any{
			"ok":        true,
			"providers": providers,
		})
		return
	case http.MethodPost:
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if contentType := strings.ToLower(strings.TrimSpace(r.Header.Get("Content-Type"))); !strings.HasPrefix(contentType, "application/json") {
		http.Error(w, "content type must be application/json", http.StatusUnsupportedMediaType)
		return
	}
	if r.ContentLength > maxAccountImportBodyBytes {
		http.Error(w, "account import body is too large", http.StatusRequestEntityTooLarge)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxAccountImportBodyBytes)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			http.Error(w, "account import body is too large", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "invalid account import body", http.StatusBadRequest)
		return
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var input accountImportRequest
	if err := decoder.Decode(&input); err != nil {
		http.Error(w, "invalid account import body", http.StatusBadRequest)
		return
	}
	if err := rejectTrailingJSON(decoder); err != nil {
		http.Error(w, "invalid account import body", http.StatusBadRequest)
		return
	}
	canonicalizeAccountImportProvider(&input)

	accountID, err := s.installImportedAccount(r.Context(), input)
	if err != nil {
		var validationErr *accountImportValidationError
		if errors.As(err, &validationErr) {
			http.Error(w, validationErr.Error(), http.StatusBadRequest)
			return
		}
		var capacityErr *accountImportCapacityError
		if errors.As(err, &capacityErr) {
			http.Error(w, capacityErr.Error(), http.StatusInsufficientStorage)
			return
		}
		var inventoryErr *accountImportInventoryUnavailableError
		if errors.As(err, &inventoryErr) {
			if s.Logger != nil {
				s.Logger.Error("account import inventory unavailable", "source", inventoryErr.source, "error", inventoryErr.err)
			}
			http.Error(w, inventoryErr.Error(), http.StatusServiceUnavailable)
			return
		}
		var collisionErr *accounts.StorageKeyCollisionError
		if errors.As(err, &collisionErr) {
			http.Error(w, "account identifier conflicts with an existing account", http.StatusConflict)
			return
		}
		if errors.Is(err, agentantigravity.ErrManagedIdentityExists) {
			http.Error(w, "Antigravity OAuth identity already exists in this account pool", http.StatusConflict)
			return
		}
		if s.Logger != nil {
			s.Logger.Error("account import failed", "provider", input.Provider, "error", err)
		}
		http.Error(w, "account import failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{
		"ok":       true,
		"provider": input.Provider,
		"account":  accountID,
	})
}

func canonicalizeAccountImportProvider(input *accountImportRequest) {
	if input == nil {
		return
	}
	original := input.Provider
	canonical, ok := canonicalKeyedProvider(original)
	if !ok {
		return
	}
	canonical = accountProviderFor(canonical)
	if canonical == original {
		return
	}
	if input.Codex != nil {
		originalPrefix := string(original) + ":"
		if strings.HasPrefix(input.Codex.Email, originalPrefix) {
			input.Codex.Email = string(canonical) + ":" + strings.TrimPrefix(input.Codex.Email, originalPrefix)
		}
		input.Codex.Provider = canonical
	}
	input.Provider = canonical
}

type accountImportValidationError struct {
	message string
}

func (e *accountImportValidationError) Error() string { return e.message }

func invalidAccountImport(message string) error {
	return &accountImportValidationError{message: message}
}

type accountImportCapacityError struct{}

func (*accountImportCapacityError) Error() string {
	return "account import pool has reached its capacity"
}

type accountImportInventoryUnavailableError struct {
	source string
	err    error
}

func (e *accountImportInventoryUnavailableError) Error() string {
	return fmt.Sprintf("account inventory unavailable for %s; repair that source before importing accounts", e.source)
}

func (e *accountImportInventoryUnavailableError) Unwrap() error { return e.err }

func rejectTrailingJSON(decoder *json.Decoder) error {
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("trailing JSON value")
		}
		return err
	}
	return nil
}

func (s Server) installImportedAccount(ctx context.Context, input accountImportRequest) (accountID string, err error) {
	return s.installAccountMutation(ctx, func() (string, func() error, error) {
		switch {
		case input.Provider == accounts.ProviderAntigravity && input.Antigravity != nil:
			if input.Codex != nil || input.Claude != nil || input.Kimi != nil {
				return "", nil, invalidAccountImport("exactly one matching account payload is required")
			}
			label, credential, remove, err := validateAntigravityAccountImport(*input.Antigravity)
			if err != nil {
				return "", nil, err
			}
			store, configured := s.AccountRef.antigravityStore()
			if !configured {
				return "", nil, invalidAccountImport("Antigravity account import is not configured for this pool")
			}
			id, err := agentantigravity.ManagedAccountID(label)
			if err != nil {
				return "", nil, invalidAccountImport("Antigravity account label is invalid")
			}
			if remove {
				exists, err := store.ManagedAccountExists(label)
				if err != nil {
					return "", nil, err
				}
				if !exists {
					return "", nil, invalidAccountImport("managed Antigravity account was not found")
				}
				return id, func() error {
					_, ok, err := store.RemoveManagedAccount(label)
					if err != nil {
						return err
					}
					if !ok {
						return errors.New("managed Antigravity account disappeared during removal")
					}
					return nil
				}, nil
			}
			exists, err := store.ManagedAccountExists(label)
			if err != nil {
				return "", nil, err
			}
			if !exists {
				if err := s.ensureNewOAuthSourceAccountImportCapacity(ctx, accounts.ProviderAntigravity, id); err != nil {
					return "", nil, err
				}
			}
			submitted := credential
			attest := agentantigravity.RefreshCredential
			if s.antigravityImportAttestForTest != nil {
				attest = s.antigravityImportAttestForTest
			}
			attested, err := attest(ctx, s.AccountRef.client, submitted, time.Now())
			if err != nil {
				return "", nil, invalidAccountImport("Antigravity OAuth credential could not be attested by the server")
			}
			return id, func() error { _, err := store.SaveManagedCredentialFromGrant(label, attested, submitted); return err }, nil
		case input.Provider == accounts.ProviderKimi && input.Kimi != nil:
			if input.Codex != nil || input.Claude != nil || input.Antigravity != nil {
				return "", nil, invalidAccountImport("exactly one matching account payload is required")
			}
			label, credential, remove, err := validateKimiAccountImport(*input.Kimi)
			if err != nil {
				return "", nil, err
			}
			store := s.AccountRef.kimiStore()
			id, err := agentkimi.ManagedAccountID(label)
			if err != nil {
				return "", nil, invalidAccountImport("Kimi account label is invalid")
			}
			if remove {
				exists, err := store.ManagedAccountExists(label)
				if err != nil {
					return "", nil, err
				}
				if !exists {
					return "", nil, invalidAccountImport("managed Kimi account was not found")
				}
				return id, func() error {
					_, ok, err := store.RemoveManagedAccount(label)
					if err != nil {
						return err
					}
					if !ok {
						return errors.New("managed Kimi account disappeared during removal")
					}
					return nil
				}, nil
			}
			exists, err := store.ManagedAccountExists(label)
			if err != nil {
				return "", nil, err
			}
			if !exists {
				if err := s.ensureNewOAuthSourceAccountImportCapacity(ctx, accounts.ProviderKimi, id); err != nil {
					return "", nil, err
				}
			}
			return id, func() error {
				_, err := store.SaveManagedCredential(label, credential)
				return err
			}, nil
		case input.Provider == accounts.ProviderCodex || isKeyedProvider(input.Provider):
			if input.Codex == nil || input.Claude != nil || input.Kimi != nil || input.Antigravity != nil {
				return "", nil, invalidAccountImport("exactly one matching account payload is required")
			}
			remoteCodexOAuth := input.Provider == accounts.ProviderCodex && !input.Codex.IsAPIKey()
			account, err := validateStoredAccountImportOrigin(input.Provider, *input.Codex, !remoteCodexOAuth)
			if err != nil {
				return "", nil, err
			}
			canonicalID, err := s.ensureAccountImportCapacity(ctx, account.Email, false)
			if err != nil {
				return "", nil, err
			}
			account.Email = canonicalID
			return account.Email, func() error {
				if remoteCodexOAuth {
					return attestAndSaveTenantCodexOAuth(
						ctx, s.AccountRef.client, s.AccountRef.store, account,
						func(attested *accounts.StoredCodexAccount) error {
							validated, validateErr := validateStoredAccountImport(input.Provider, *attested)
							if validateErr == nil {
								*attested = validated
							}
							return validateErr
						},
					)
				}
				return s.AccountRef.store.SaveStored(account)
			}, nil
		case input.Provider == accounts.ProviderClaude:
			if input.Claude != nil && input.Codex == nil && input.Kimi == nil && input.Antigravity == nil {
				name, credential, err := validateClaudeAccountImport(*input.Claude)
				if err != nil {
					return "", nil, err
				}
				canonicalName, err := s.ensureAccountImportCapacity(ctx, name, true)
				if err != nil {
					return "", nil, err
				}
				return canonicalName, func() error {
					return s.AccountRef.claudeStore.ImportProfileCredential(canonicalName, credential)
				}, nil
			}
			if input.Codex == nil || input.Claude != nil || input.Kimi != nil || input.Antigravity != nil {
				return "", nil, invalidAccountImport("exactly one matching account payload is required")
			}
			account, err := validateStoredAccountImport(input.Provider, *input.Codex)
			if err != nil {
				return "", nil, err
			}
			canonicalID, err := s.ensureAccountImportCapacity(ctx, account.Email, false)
			if err != nil {
				return "", nil, err
			}
			account.Email = canonicalID
			return account.Email, func() error { return s.AccountRef.store.SaveStored(account) }, nil
		default:
			return "", nil, invalidAccountImport("unsupported account provider")
		}
	})
}

// installAccountMutation serializes validation and capacity checks with the
// disk mutation, then publishes the generation marker before touching any
// credential. Other workers that observe the marker block on the transaction
// lock until the credential mutation is complete.
func (s Server) installAccountMutation(
	ctx context.Context,
	prepare func() (accountID string, mutate func() error, err error),
) (accountID string, err error) {
	if err := lockMutexContext(ctx, &s.AccountRef.installMu); err != nil {
		return "", err
	}
	defer s.AccountRef.installMu.Unlock()
	transactionLock, err := lockAccountImportTransaction(ctx, s.AccountRef.store.StoreDir())
	if err != nil {
		return "", err
	}
	defer func() {
		if transactionLock != nil {
			if closeErr := transactionLock.Close(); err == nil {
				err = closeErr
			}
		}
	}()
	if _, err := reconcileCompletedAccountRollback(ctx, s.AccountRef.store, advanceAccountDiskGeneration); err != nil {
		return "", err
	}
	accountID, mutate, err := prepare()
	if err != nil {
		return "", err
	}
	if mutate == nil {
		return "", errors.New("account mutation is not configured")
	}
	if err := s.AccountRef.advanceDiskGeneration(); err != nil {
		return "", err
	}
	mutationErr := mutate()
	loaded, accountGeneration, reloadErr := s.AccountRef.ReloadSnapshot()
	if reloadErr == nil && errors.Is(mutationErr, agentclaude.ErrProfileRegistryWriteCommitted) {
		for _, account := range loaded {
			if account.ID == accountID {
				slog.Warn("accepting committed Claude account mutation after exact snapshot verification", "account", accountID, "error", mutationErr)
				mutationErr = nil
				break
			}
		}
	}
	if reloadErr == nil {
		if s.SchedulerRef != nil {
			s.SchedulerRef.AdvanceAccountGeneration(accountGeneration)
		}
	}
	closeErr := transactionLock.Close()
	transactionLock = nil
	// The durable mutation may have succeeded even when snapshot reload did
	// not. Invalidate only after releasing the cross-process transaction lock:
	// a usage sweep holds this cache mutex while refresh may acquire that lock.
	s.AccountRef.InvalidateUsageStatusCache()
	var finishErr error
	if reloadErr == nil {
		_, _, finishErr = s.finishAccountReload(ctx, loaded, accountGeneration)
	}
	if err := errors.Join(mutationErr, reloadErr, closeErr, finishErr); err != nil {
		return "", err
	}
	return accountID, nil
}

func (s Server) ensureAccountImportCapacity(ctx context.Context, accountID string, claudeProfile bool) (string, error) {
	stored, err := s.AccountRef.store.ListStored()
	if err != nil {
		return "", err
	}
	profiles := s.AccountRef.claudeStore.ListProfiles()
	if claudeProfile {
		for _, profile := range profiles {
			if strings.EqualFold(profile.Name, accountID) {
				return profile.Name, nil
			}
		}
	} else {
		for _, account := range stored {
			if strings.EqualFold(account.Email, accountID) {
				return account.Email, nil
			}
		}
	}
	_, count, err := s.accountImportInventory(ctx)
	if err != nil {
		return "", err
	}
	if count >= maxAccountImportAccounts {
		return "", &accountImportCapacityError{}
	}
	return accountID, nil
}

func (s Server) ensureNewOAuthSourceAccountImportCapacity(ctx context.Context, provider accounts.Provider, accountID string) error {
	for _, source := range s.AccountRef.oauthSources {
		if source.Provider() != provider {
			continue
		}
		if inventory, ok := source.(oauthAccountDurableInventory); ok {
			ids, err := inventory.AccountInventoryIDs(ctx)
			if err != nil {
				return err
			}
			for _, existingID := range ids {
				if strings.EqualFold(existingID, accountID) {
					return &accounts.StorageKeyCollisionError{
						Identifier:         accountID,
						ExistingIdentifier: existingID,
					}
				}
			}
		}
	}
	all, count, err := s.accountImportInventory(ctx)
	if err != nil {
		return err
	}
	for _, account := range all {
		if account.Provider == provider && strings.EqualFold(account.ID, accountID) {
			return &accounts.StorageKeyCollisionError{
				Identifier:         accountID,
				ExistingIdentifier: account.ID,
			}
		}
	}
	if count >= maxAccountImportAccounts {
		return &accountImportCapacityError{}
	}
	return nil
}

// accountImportInventory reads every credential source while the caller holds
// the cross-process import transaction. Counting the in-memory snapshot here
// makes the limit order-dependent and lets another worker's completed import,
// or an OAuth-source account, disappear from the capacity decision.
type oauthAccountInventoryCounter interface {
	AccountInventoryCount(context.Context) (int, error)
}

type oauthAccountDurableInventory interface {
	AccountInventoryIDs(context.Context) ([]string, error)
}

func (s Server) accountImportInventory(ctx context.Context) ([]accounts.Account, int, error) {
	stored, err := s.AccountRef.store.ListStored()
	if err != nil {
		return nil, 0, err
	}
	all, err := s.AccountRef.store.List()
	if err != nil {
		return nil, 0, err
	}
	profileCount, err := s.AccountRef.claudeStore.ProfileInventoryCount()
	if err != nil {
		return nil, 0, &accountImportInventoryUnavailableError{source: "claude", err: err}
	}
	count := len(stored) + profileCount
	claudeAccounts, _ := s.AccountRef.claudeStore.ListAccounts(ctx)
	all = append(all, claudeAccounts...)
	for _, source := range s.AccountRef.oauthSources {
		sourceAccounts, sourceErr := source.ListAccounts(ctx)
		all = append(all, sourceAccounts...)
		sourceCount := len(sourceAccounts)
		if counter, ok := source.(oauthAccountInventoryCounter); ok {
			durableCount, countErr := counter.AccountInventoryCount(ctx)
			if countErr != nil {
				// A multi-account store has an unbounded unknown size when its
				// durable inventory cannot be read. New distinct imports must
				// fail closed; callers establish in-place repairs before here.
				return nil, 0, &accountImportInventoryUnavailableError{source: string(source.Provider()), err: countErr}
			}
			if durableCount > sourceCount {
				sourceCount = durableCount
			}
		} else if sourceErr != nil {
			// Single-credential sources (Grok and Antigravity) return no
			// account when their credential is unreadable. Reserve its slot.
			sourceCount++
		}
		count += sourceCount
	}
	return all, count, nil
}

func validateStoredAccountImport(provider accounts.Provider, account accounts.StoredCodexAccount) (accounts.StoredCodexAccount, error) {
	return validateStoredAccountImportOrigin(provider, account, true)
}

func validateStoredAccountImportOrigin(provider accounts.Provider, account accounts.StoredCodexAccount, requireTrustedOrigin bool) (accounts.StoredCodexAccount, error) {
	account.Email = strings.TrimSpace(account.Email)
	if account.Email == "" || strings.HasPrefix(account.Email, ".") || len(account.Email) > 320 || strings.ContainsAny(account.Email, "/\\") || containsTerminalControl(account.Email) {
		return account, invalidAccountImport("account identifier is invalid")
	}
	account.Provider = provider
	account.AddedAt = time.Now().UTC().Format(time.RFC3339)
	account.Breadcrumbs = nil
	account.Auth.RefreshFailure = nil
	if account.IsAPIKey() {
		if account.Auth.Tokens != nil || strings.TrimSpace(account.Auth.OpenAIAPIKey) == "" {
			return account, invalidAccountImport("API-key account payload is invalid")
		}
		prefix := string(provider) + ":"
		if provider == accounts.ProviderCodex {
			prefix = "apikey:"
		}
		if !strings.HasPrefix(account.Email, prefix) || strings.TrimSpace(strings.TrimPrefix(account.Email, prefix)) == "" {
			return account, invalidAccountImport("API-key account label does not match its provider")
		}
		if provider == accounts.ProviderCodex && !strings.HasPrefix(account.Auth.OpenAIAPIKey, "sk-") {
			return account, invalidAccountImport("Codex API key format is invalid")
		}
		account.Auth.AuthMode = "apikey"
		return account, nil
	}
	if provider != accounts.ProviderCodex || account.Auth.Tokens == nil || account.Auth.OpenAIAPIKey != "" {
		return account, invalidAccountImport("OAuth account payload is invalid")
	}
	if requireTrustedOrigin &&
		account.OAuthCredentialOrigin != accounts.CodexOAuthOriginIsolatedServerLogin &&
		account.OAuthCredentialOrigin != accounts.CodexOAuthOriginServerAttested {
		return account, invalidAccountImport("OAuth account payload must come from an isolated server login")
	}
	tokens := account.Auth.Tokens
	if strings.TrimSpace(tokens.AccessToken) == "" || strings.TrimSpace(tokens.RefreshToken) == "" || strings.TrimSpace(tokens.IDToken) == "" {
		return account, invalidAccountImport("OAuth account payload is incomplete")
	}
	email, err := accounts.ExtractEmailFromJWT(tokens.IDToken)
	if err != nil || !strings.EqualFold(strings.TrimSpace(email), account.Email) {
		return account, invalidAccountImport("OAuth identity does not match the account identifier")
	}
	if expiresAt, ok := accounts.JWTExpiryMillis(tokens.AccessToken); !ok || expiresAt <= time.Now().UnixMilli() {
		return account, invalidAccountImport("OAuth access token is not fresh")
	}
	account.Email = strings.TrimSpace(email)
	return account, nil
}

func attestTenantCodexOAuth(ctx context.Context, client *http.Client, account accounts.StoredCodexAccount) (accounts.StoredCodexAccount, error) {
	if account.Auth.Tokens == nil ||
		strings.TrimSpace(account.Auth.Tokens.AccessToken) == "" ||
		strings.TrimSpace(account.Auth.Tokens.RefreshToken) == "" ||
		strings.TrimSpace(account.Auth.Tokens.IDToken) == "" {
		return account, invalidAccountImport("OAuth account payload is incomplete")
	}
	submittedRefreshToken := account.Auth.Tokens.RefreshToken
	refreshed, err := accounts.RefreshCodexAuth(ctx, client, account.Auth)
	if err != nil {
		return account, invalidAccountImport("OAuth credential transfer could not be attested by the server")
	}
	if refreshed.Tokens == nil || subtle.ConstantTimeCompare(
		[]byte(refreshed.Tokens.RefreshToken), []byte(submittedRefreshToken),
	) == 1 {
		return account, invalidAccountImport("OAuth provider did not rotate the submitted credential")
	}
	account.Auth = refreshed
	account.OAuthCredentialOrigin = accounts.CodexOAuthOriginServerAttested
	return account, nil
}

func containsTerminalControl(value string) bool {
	for _, char := range value {
		if unicode.IsControl(char) || unicode.In(char, unicode.Cf, unicode.Zl, unicode.Zp) {
			return true
		}
	}
	return false
}

func validateKimiAccountImport(input kimiAccountImport) (string, agentkimi.CredentialInfo, bool, error) {
	label := strings.TrimSpace(input.Label)
	if _, err := agentkimi.ManagedAccountID(label); err != nil || len(label) > 160 || containsTerminalControl(label) {
		return "", input.Credential, input.Remove, invalidAccountImport("Kimi account label is invalid")
	}
	if input.Remove {
		if strings.TrimSpace(input.Credential.AccessToken) != "" || strings.TrimSpace(input.Credential.RefreshToken) != "" {
			return "", input.Credential, true, invalidAccountImport("Kimi removal must not include a credential")
		}
		return label, input.Credential, true, nil
	}
	if strings.TrimSpace(input.Credential.AccessToken) == "" || strings.TrimSpace(input.Credential.RefreshToken) == "" {
		return "", input.Credential, false, invalidAccountImport("Kimi OAuth payload is incomplete")
	}
	deviceID := strings.TrimSpace(input.Credential.OAuthDeviceID)
	if err := agentkimi.ValidateOAuthDeviceID(deviceID); err != nil {
		return "", input.Credential, false, invalidAccountImport("Kimi OAuth device identity is invalid")
	}
	if input.Credential.ExpiresAt.IsZero() || !input.Credential.ExpiresAt.After(time.Now()) {
		return "", input.Credential, false, invalidAccountImport("Kimi OAuth access token is not fresh")
	}
	return label, input.Credential, false, nil
}

func validateAntigravityAccountImport(input antigravityAccountImport) (string, agentantigravity.CredentialInfo, bool, error) {
	label := strings.TrimSpace(input.Label)
	if _, err := agentantigravity.ManagedAccountID(label); err != nil || len(label) > 160 || containsTerminalControl(label) {
		return "", input.Credential, input.Remove, invalidAccountImport("Antigravity account label is invalid")
	}
	if input.Remove {
		if strings.TrimSpace(input.Credential.AccessToken) != "" || strings.TrimSpace(input.Credential.RefreshToken) != "" {
			return "", input.Credential, true, invalidAccountImport("Antigravity removal must not include a credential")
		}
		return label, input.Credential, true, nil
	}
	if strings.TrimSpace(input.Credential.AccessToken) == "" || strings.TrimSpace(input.Credential.RefreshToken) == "" {
		return "", input.Credential, false, invalidAccountImport("Antigravity OAuth payload is incomplete")
	}
	if strings.TrimSpace(input.Credential.OAuthClientID) == "" || strings.TrimSpace(input.Credential.OAuthClientSecret) == "" {
		return "", input.Credential, false, invalidAccountImport("Antigravity OAuth client binding is missing")
	}
	if input.Credential.ExpiresAt.IsZero() || !input.Credential.ExpiresAt.After(time.Now()) {
		return "", input.Credential, false, invalidAccountImport("Antigravity OAuth access token is not fresh")
	}
	return label, input.Credential, false, nil
}

func validateClaudeAccountImport(input claudeAccountImport) (string, agentclaude.CredentialInfo, error) {
	name := strings.TrimSpace(input.Name)
	if name == "" || len(name) > 320 || strings.ContainsAny(name, "\r\n\x00/\\") {
		return "", input.Credential, invalidAccountImport("Claude profile name is invalid")
	}
	if err := agentclaude.ValidateProfileNameAllowEmail(name); err != nil {
		return "", input.Credential, invalidAccountImport("Claude profile name is invalid")
	}
	if strings.TrimSpace(input.Credential.AccessToken) == "" || strings.TrimSpace(input.Credential.RefreshToken) == "" {
		return "", input.Credential, invalidAccountImport("Claude OAuth payload is incomplete")
	}
	return name, input.Credential, nil
}

func (s Server) reloadAccounts(ctx context.Context) (accountCount int, scoredCount int, err error) {
	if s.AccountRef == nil {
		return 0, 0, fmt.Errorf("account reload is not configured")
	}
	if err := lockMutexContext(ctx, &s.AccountRef.installMu); err != nil {
		return 0, 0, err
	}
	defer s.AccountRef.installMu.Unlock()
	transactionLock, err := lockAccountImportTransaction(ctx, s.AccountRef.store.StoreDir())
	if err != nil {
		return 0, 0, err
	}
	if _, rollbackErr := reconcileCompletedAccountRollback(ctx, s.AccountRef.store, advanceAccountDiskGeneration); rollbackErr != nil && !errors.Is(rollbackErr, errAccountRollbackIncomplete) {
		_ = transactionLock.Close()
		return 0, 0, rollbackErr
	}
	loaded, accountGeneration, err := s.AccountRef.ReloadSnapshot()
	if err != nil {
		_ = transactionLock.Close()
		return 0, 0, err
	}
	loaded, accountGeneration, credentialRevision := s.AccountRef.CredentialSnapshot()
	if s.SchedulerRef != nil {
		s.SchedulerRef.AdvanceAccountGenerationWithAccounts(accountGeneration, credentialRevision, SchedulerAccounts(loaded))
	}
	if err := transactionLock.Close(); err != nil {
		return 0, 0, err
	}
	return s.finishAccountReload(ctx, loaded, accountGeneration)
}

func (s Server) finishAccountReload(ctx context.Context, loaded []accounts.Account, accountGeneration uint64) (int, int, error) {
	if s.SchedulerRef == nil {
		return len(loaded), 0, nil
	}
	// In team mode the vault owns these credentials and the broker picks the
	// account, so local scoring is both unnecessary and destructive: scoring
	// refreshes every OAuth account, the provider rotates the refresh token on
	// every use, and a second refresher invalidates the vault's chain (and vice
	// versa). Two refreshers permanently break each other's accounts.
	if s.CredentialBroker != nil {
		return len(loaded), 0, nil
	}
	scoreRevision := s.SchedulerRef.ScoreRevision()
	scoreAccounts := s.ScoreAccounts
	if scoreAccounts == nil {
		scoreAccounts = s.scoreAccounts
	}
	scores, scored := scoreAccounts(ctx, loaded)
	if scored == 0 {
		if s.Logger != nil {
			s.Logger.Warn("account reload usage score update skipped", "reason", "no fresh OAuth usage scores")
		}
		return len(loaded), scored, nil
	}
	scheduler := selectacct.NewScheduler(scores)
	if s.Sessions != nil {
		scheduler = scheduler.WithSessionCounts(SchedulerSessionCounts(s.Sessions))
	}
	if !s.SchedulerRef.SetForAccountGenerationAtScoreRevision(scheduler, accountGeneration, scoreRevision) {
		if s.Logger != nil {
			s.Logger.Debug("account reload usage score update discarded after a newer account reload")
		}
		return len(loaded), 0, nil
	}
	return len(loaded), scored, nil
}

// RateLimitResetResult is the per-account outcome of a rate-limit reset
// request, whether the account was a single target or part of an --all sweep.
type RateLimitResetResult struct {
	Email         string                         `json:"email"`
	Eligible      bool                           `json:"eligible"`
	Reset         bool                           `json:"reset"`
	DryRun        bool                           `json:"dry_run,omitempty"`
	Credit        *accounts.RateLimitResetCredit `json:"credit,omitempty"`
	WindowsBefore []accounts.UsageWindow         `json:"windows_before,omitempty"`
	WindowsAfter  []accounts.UsageWindow         `json:"windows_after,omitempty"`
	Error         string                         `json:"error,omitempty"`
}

// handleRateLimitReset redeems a ChatGPT Pro "rate-limit reset" credit for one
// account (?email=...) or for every cooked OAuth account that still has a
// credit available (?all=true). Pass dry_run=true to list candidates without
// consuming a credit. The handler is admin-gated like the other _subrouter
// endpoints because redeeming a credit is a real, externally-visible action on
// the upstream account.
func (s Server) handleRateLimitReset(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.AccountRef == nil {
		http.Error(w, "account store not configured", http.StatusInternalServerError)
		return
	}
	ctx := r.Context()
	dryRun := parseBoolParam(r, "dry_run", "dry-run")
	all := parseBoolParam(r, "all", "all_accounts")
	email := strings.TrimSpace(r.URL.Query().Get("email"))
	if email == "" && r.URL.Query().Has("account") {
		email = strings.TrimSpace(r.URL.Query().Get("account"))
	}

	var results []RateLimitResetResult
	switch {
	case all:
		results = s.rateLimitResetAllAccounts(ctx, dryRun)
	case email != "":
		results = []RateLimitResetResult{s.rateLimitResetOne(ctx, email, dryRun)}
	default:
		http.Error(w, "email or all=true is required", http.StatusBadRequest)
		return
	}

	resetCount := 0
	for i := range results {
		if results[i].Reset {
			resetCount++
		}
	}
	// Invalidate the cached usage-status snapshot so the next status read
	// reflects the freshly reset windows instead of the pre-reset picture.
	s.AccountRef.InvalidateUsageStatusCache()
	writeJSON(w, map[string]any{
		"ok":      true,
		"reset":   resetCount,
		"dry_run": dryRun,
		"results": results,
	})
}

// ResetCreditsAccount is one account's redeemable rate-limit reset credits,
// including each credit's expiry so callers can see when they lapse.
type ResetCreditsAccount struct {
	Email   string                          `json:"email"`
	Count   int                             `json:"count"`
	Credits []accounts.RateLimitResetCredit `json:"credits,omitempty"`
	Error   string                          `json:"error,omitempty"`
}

// handleResetCredits lists every stored OAuth account's available reset credits
// with granted/expiry timestamps. This is read-only (a GET) but still admin
// gated because it makes an authenticated call per account against the upstream.
func (s Server) handleResetCredits(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.AccountRef == nil {
		http.Error(w, "account store not configured", http.StatusInternalServerError)
		return
	}
	ctx := r.Context()
	storedAccounts, err := s.AccountRef.store.ListStored()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	sem := make(chan struct{}, 4)
	var wg sync.WaitGroup
	var mu sync.Mutex
	out := make([]ResetCreditsAccount, 0, len(storedAccounts))
	for i := range storedAccounts {
		stored := storedAccounts[i]
		if stored.IsAPIKey() {
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			account, ok := stored.Account(stored.SourcePath(s.AccountRef.store))
			if !ok || account.Token == "" {
				return
			}
			// This value is also the selector clients send back to the reset
			// endpoint, so expose the stable routing ID rather than the OAuth
			// identity email decoded from the token.
			entry := ResetCreditsAccount{Email: account.ID}
			credits, err := accounts.ListRateLimitResetCredits(ctx, s.AccountRef.client, account)
			if err != nil {
				entry.Error = err.Error()
			} else {
				for _, c := range credits {
					if c.Status == "" || c.Status == "available" {
						entry.Credits = append(entry.Credits, c)
					}
				}
				entry.Count = len(entry.Credits)
			}
			mu.Lock()
			out = append(out, entry)
			mu.Unlock()
		}()
	}
	wg.Wait()
	sort.Slice(out, func(i, j int) bool { return out[i].Email < out[j].Email })
	writeJSON(w, map[string]any{"ok": true, "accounts": out})
}

// rateLimitResetOne resolves a single account by email and redeems a credit if
// it is cooked on its 7d window and has a credit available.
func (s Server) rateLimitResetOne(ctx context.Context, email string, dryRun bool) RateLimitResetResult {
	result := RateLimitResetResult{Email: email, DryRun: dryRun}
	account, ok, err := s.AccountRef.ResolvedAccount(ctx, email)
	if err != nil {
		result.Error = err.Error()
		return result
	}
	if !ok {
		result.Error = "account not found"
		return result
	}
	return s.redeemAccountIfEligible(ctx, account, dryRun)
}

// rateLimitResetAllAccounts sweeps every stored OAuth account, redeems a credit
// for each one that is cooked on its 7d window and still has a credit available.
// Accounts that are healthy or out of credits are skipped silently.
func (s Server) rateLimitResetAllAccounts(ctx context.Context, dryRun bool) []RateLimitResetResult {
	storedAccounts, err := s.AccountRef.store.ListStored()
	if err != nil {
		return []RateLimitResetResult{{
			Email: "",
			Error: err.Error(),
		}}
	}
	// Cap concurrent usage fetches so a large pool does not trip the upstream
	// usage endpoint's per-IP rate limit (the same reason FetchUsageWindowsCached
	// exists). Eligibility is decided from usage, then redemption runs serially.
	sem := make(chan struct{}, 4)
	var wg sync.WaitGroup
	type candidate struct {
		account accounts.Account
		before  []accounts.UsageWindow
	}
	candidates := make([]candidate, 0, len(storedAccounts))
	var mu sync.Mutex
	for i := range storedAccounts {
		stored := storedAccounts[i]
		if stored.IsAPIKey() {
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			account, ok := stored.Account(stored.SourcePath(s.AccountRef.store))
			if !ok || account.Token == "" {
				return
			}
			details, err := accounts.FetchCodexUsageDetails(ctx, s.AccountRef.client, account)
			if err != nil {
				return
			}
			if !rateLimitCooked(details) {
				return
			}
			if !rateLimitHasCredit(details) {
				return
			}
			mu.Lock()
			candidates = append(candidates, candidate{account: account, before: details.Windows})
			mu.Unlock()
		}()
	}
	wg.Wait()

	results := make([]RateLimitResetResult, 0, len(candidates))
	for _, c := range candidates {
		res := s.redeemAccountIfEligible(ctx, c.account, dryRun)
		// Preserve the before-windows captured during the sweep when the
		// redeem path could not refetch them.
		if len(res.WindowsBefore) == 0 {
			res.WindowsBefore = c.before
		}
		results = append(results, res)
	}
	return results
}

// redeemAccountIfEligible fetches current usage, and if the account is cooked
// on its 7d window with a credit available, redeems one credit. dryRun lists
// eligibility without consuming.
func (s Server) redeemAccountIfEligible(ctx context.Context, account accounts.Account, dryRun bool) RateLimitResetResult {
	result := RateLimitResetResult{Email: account.ID, DryRun: dryRun}
	before, err := accounts.FetchCodexUsageDetails(ctx, s.AccountRef.client, account)
	if err != nil {
		result.Error = err.Error()
		return result
	}
	result.WindowsBefore = before.Windows
	if !rateLimitCooked(before) {
		result.Error = "account is not cooked; skipping"
		return result
	}
	if !rateLimitHasCredit(before) {
		result.Eligible = false
		result.Error = "no rate-limit reset credits available"
		return result
	}
	result.Eligible = true
	if dryRun {
		return result
	}
	credit, err := accounts.RedeemRateLimitReset(ctx, s.AccountRef.client, account)
	if err != nil {
		result.Error = err.Error()
		return result
	}
	result.Credit = &credit
	result.Reset = true
	if after, err := accounts.FetchCodexUsageDetails(ctx, s.AccountRef.client, account); err == nil {
		result.WindowsAfter = after.Windows
	}
	return result
}

// rateLimitCooked reports whether an account is currently blocked by its
// account-wide 7d (secondary) rate-limit window. We treat the upstream
// limit_reached flag as authoritative and fall back to the secondary window
// being fully consumed.
func rateLimitCooked(details accounts.CodexUsageDetails) bool {
	if details.RawRateLimit.LimitReached {
		return true
	}
	if sw := details.RawRateLimit.SecondaryWindow; sw != nil && sw.UsedPercent >= 100 {
		return true
	}
	return false
}

// rateLimitHasCredit reports whether usage advertised at least one redeemable
// rate-limit reset credit for the account.
func rateLimitHasCredit(details accounts.CodexUsageDetails) bool {
	return details.ComplimentaryReset != nil && details.ComplimentaryReset.Available
}

// parseBoolParam accepts a few truthy spellings ("1", "true", "yes", "on") for
// a boolean query parameter, checking aliases in order.
func parseBoolParam(r *http.Request, aliases ...string) bool {
	q := r.URL.Query()
	for _, alias := range aliases {
		v := strings.ToLower(strings.TrimSpace(q.Get(alias)))
		if v == "1" || v == "true" || v == "yes" || v == "on" {
			return true
		}
	}
	return false
}

func (s Server) scoreAccounts(ctx context.Context, available []accounts.Account) ([]selectacct.Score, int) {
	// Seed each account from the scheduler's current score so a refresh that
	// can't get FRESH usage data for an account preserves its last known score
	// instead of overwriting it. Previously a refresh built every score from
	// scratch and, when the usage cache served stale "last good" cooked data
	// (the upstream usage endpoint rate-limits under load), it clobbered
	// healthy accounts to exhausted and the router then routed traffic to dead
	// accounts. A score is only overwritten on confident, fresh evidence.
	current := s.Scheduler
	if s.SchedulerRef != nil {
		current = s.SchedulerRef.RefreshSeed()
	}
	scores := make([]selectacct.Score, 0, len(available))
	scoreByID := make(map[string]int, len(available))
	for _, account := range available {
		scoreProvider := schedulerAccountProvider(account.Provider)
		seed := current.ScoreFor(scoreProvider, account.ID)
		seed.AccountID = account.ID
		seed.Provider = scoreProvider
		seed.Fresh = false // carried forward until a fresh fetch overwrites it
		if account.AuthMode == accounts.AuthModeAPIKey {
			seed.Headroom = 0.01
			seed.ShortHeadroom = 0.01
		}
		scoreByID[selectacct.ScoreKey(scoreProvider, account.ID)] = len(scores)
		scores = append(scores, seed)
	}

	client := (*http.Client)(nil)
	if s.AccountRef != nil {
		client = s.AccountRef.client
	}
	if client == nil {
		client = &http.Client{Timeout: defaultUsageFetchTimeout}
	}
	// Keep one wall-clock budget for the entire pool. A per-account deadline
	// after semaphore acquisition would make a sweep grow by five seconds per
	// batch. Accounts that do not acquire a slot preserve their scheduler seed,
	// so a saturated upstream cannot erase prior routing evidence.
	sweepCtx, cancelSweep := context.WithTimeout(ctx, usageStatusFetchTimeout)
	defer cancelSweep()
	// Score accounts in parallel without allowing an unbounded number of
	// refresh/usage pairs to occupy transports and provider endpoints.
	scored := 0
	var scoreMu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, accountFetchConcurrency)
	for _, account := range available {
		if account.AuthMode != accounts.AuthModeOAuth {
			continue
		}
		if failure, dead := s.AccountRef.terminalCredFailure(account); dead {
			if s.Logger != nil {
				s.Logger.Debug("skipping account with known-dead credential", "account", account.ID, "error", failure)
			}
			scoreMu.Lock()
			setZeroScore(scores, scoreByID, schedulerAccountProvider(account.Provider), account.ID)
			scoreMu.Unlock()
			continue
		}
		wg.Add(1)
		go func(account accounts.Account) {
			defer wg.Done()
			if !acquireAccountFetchSlot(sweepCtx, sem) {
				return
			}
			defer func() { <-sem }()
			refreshCtx := accounts.WithCodexRefreshReason(sweepCtx, "proxy.score-accounts")
			refreshed, err := s.refreshAccount(refreshCtx, account)
			s.AccountRef.noteCredResult(account, err)
			if err != nil {
				if s.Logger != nil {
					s.Logger.Warn("account reload refresh failed", "account", account.ID, "error", err)
				}
				// Only zero on a confident auth failure (dead/invalid token).
				// Transient refresh errors preserve the seed.
				if authLikeUsageError(err.Error()) {
					scoreMu.Lock()
					setZeroScore(scores, scoreByID, schedulerAccountProvider(account.Provider), account.ID)
					scoreMu.Unlock()
				}
				return
			}
			if !s.AccountRef.hasOAuthUsageSource(refreshed.Provider) {
				// Credential-only OAuth providers deliberately publish no quota API.
				// Successful refresh proves authentication, not quota recovery. Keep
				// the seed and publish no positive quota evidence so a request-time
				// exhaustion overlay remains effective until its own expiry.
				return
			}
			windows, fresh, err := s.fetchAccountUsageWindows(sweepCtx, client, refreshed)
			if err != nil {
				if s.Logger != nil {
					s.Logger.Warn("account reload usage fetch failed", "account", account.ID, "error", err)
				}
				// Auth failures mean the account is unusable; zero it so the
				// scheduler avoids it. Transient failures preserve the seed.
				if authLikeUsageError(err.Error()) {
					scoreMu.Lock()
					setZeroScore(scores, scoreByID, schedulerAccountProvider(account.Provider), account.ID)
					scoreMu.Unlock()
				}
				return
			}
			if !fresh {
				// Stale last-known-good windows are not a confident signal. Keep
				// the seeded score rather than risk demoting a healthy account.
				return
			}
			scoreMu.Lock()
			defer scoreMu.Unlock()
			scoreProvider := schedulerAccountProvider(account.Provider)
			if idx, ok := scoreByID[selectacct.ScoreKey(scoreProvider, account.ID)]; ok {
				next := scoreFromUsageWindows(scoreProvider, account.ID, windows)
				next.Fresh = true
				scores[idx] = next
				scored++
			}
		}(account)
	}
	wg.Wait()
	return scores, scored
}

// fetchAccountUsageWindows returns an account's usage windows and whether they
// are fresh (a recent successful fetch) versus a stale last-known-good fallback.
func (s Server) fetchAccountUsageWindows(ctx context.Context, client *http.Client, account accounts.Account) ([]accounts.UsageWindow, bool, error) {
	if s.AccountRef != nil {
		return s.AccountRef.FetchUsageWindowsCached(ctx, client, account)
	}
	windows, err := fetchAccountUsageWindowsLive(ctx, client, account)
	return windows, err == nil, err
}

func fetchAccountUsageWindowsLive(ctx context.Context, client *http.Client, account accounts.Account) ([]accounts.UsageWindow, error) {
	if account.Provider == accounts.ProviderClaude {
		usage, err := agentclaude.FetchUsage(ctx, client, account.Token)
		windows := claudeUsageWindows(usage)
		if !usageWindowNamed(windows, agentclaude.FableWindowName) {
			if fableWindows, probeErr := agentclaude.FetchFableUsageWindows(ctx, client, account.Token); probeErr == nil && len(fableWindows) > 0 {
				if err == nil || fableProbeHasPrimaryWindows(fableWindows) {
					windows = mergeUsageWindows(windows, fableWindows)
				}
			} else if err != nil {
				if probeErr != nil {
					return nil, probeErr
				}
				return nil, err
			}
		}
		if err != nil && len(windows) == 0 {
			return nil, err
		}
		return windows, nil
	}
	if account.Provider != "" && account.Provider != accounts.ProviderCodex {
		return nil, fmt.Errorf("%w for provider %q", errOAuthUsageUnavailable, account.Provider)
	}
	return accounts.FetchCodexUsage(ctx, client, account)
}

func fableProbeHasPrimaryWindows(windows []accounts.UsageWindow) bool {
	for _, window := range windows {
		if window.Name == "5h" || window.Name == "7d" {
			return true
		}
	}
	return false
}

func mergeUsageWindows(base, extra []accounts.UsageWindow) []accounts.UsageWindow {
	out := append([]accounts.UsageWindow(nil), base...)
	index := make(map[string]int, len(out))
	for i, window := range out {
		index[usageWindowMergeKey(window)] = i
	}
	for _, window := range extra {
		key := usageWindowMergeKey(window)
		if i, ok := index[key]; ok {
			out[i] = window
			continue
		}
		index[key] = len(out)
		out = append(out, window)
	}
	return out
}

func usageWindowMergeKey(window accounts.UsageWindow) string {
	return strings.ToLower(window.Name) + "\x00" + strings.ToLower(window.Feature)
}

const defaultUsageFetchTimeout = 10 * time.Second

// usageScoreRefreshTimeout bounds one whole score-refresh sweep (every OAuth
// account's token refresh plus usage fetch, serially). The sweep runs detached
// from the triggering request's context, so this deadline is the only thing
// keeping a wedged upstream from holding that request's account selection
// open indefinitely.
const usageScoreRefreshTimeout = 60 * time.Second

func setZeroScore(scores []selectacct.Score, scoreByID map[string]int, provider accounts.Provider, accountID string) {
	if idx, ok := scoreByID[selectacct.ScoreKey(provider, accountID)]; ok {
		scores[idx] = selectacct.Score{AccountID: accountID, Provider: provider, Headroom: 0, ShortHeadroom: 0}
	}
}

func isLoopbackRemote(remoteAddr string) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// clientRemoteIP returns the authoritative source IP for request attribution:
// the socket peer (RemoteAddr) host with the port stripped. subrouter is reached
// directly on the tailnet with no trusted proxy in front, so X-Forwarded-For is
// deliberately ignored (a caller could spoof it to frame another device); the
// socket peer cannot be forged. On the tailnet this maps to a device via
// `tailscale status`.
func clientRemoteIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func userEmailHash(value string) string {
	value = session.NormalizeUserEmail(value)
	if value == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:8])
}

func stripOutboundForwardingHeaders(headers http.Header) {
	headers.Del("Forwarded")
	headers.Del("X-Forwarded-For")
	headers.Del("X-Forwarded-Host")
	headers.Del("X-Forwarded-Proto")
	headers.Del("X-Forwarded-Ssl")
	headers.Del("X-Real-IP")
}

func (s Server) requireAdmin(next func(http.ResponseWriter, *http.Request)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.authorizeAdmin(r) {
			next(w, r)
			return
		}
		http.Error(w, "admin token required", http.StatusUnauthorized)
	}
}

func (s Server) requireAccountImportAuth(next func(http.ResponseWriter, *http.Request)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.tenantAccountImportAuthorized {
			next(w, r)
			return
		}
		if localDataConnectionAuthorized(r) {
			next(w, r)
			return
		}
		if s.matchesConfiguredAccountImportToken(r) || s.matchesConfiguredAdminToken(r) {
			next(w, r)
			return
		}
		if identity, ok := s.authorizeTailnet(r); ok {
			// Name who onboarded an account. A tailnet identity is the one
			// thing a shared token could never tell us.
			if s.Logger != nil {
				s.Logger.Info("account import authorized by tailnet identity", "peer", identity.String())
			}
			next(w, r)
			return
		}
		http.Error(w, "protected account import credential required", http.StatusUnauthorized)
	}
}

func (s Server) matchesConfiguredAccountImportToken(r *http.Request) bool {
	return matchesConfiguredBearerToken(r, s.AccountImportToken, "X-Subrouter-Account-Import-Token")
}

func (s Server) matchesConfiguredAdminToken(r *http.Request) bool {
	return matchesConfiguredBearerToken(r, s.AdminToken, "X-Subrouter-Admin-Token")
}

func matchesConfiguredBearerToken(r *http.Request, configuredToken, dedicatedHeader string) bool {
	token := strings.TrimSpace(configuredToken)
	if token == "" {
		return false
	}
	got := strings.TrimSpace(r.Header.Get(dedicatedHeader))
	if got == "" {
		auth := strings.TrimSpace(r.Header.Get("Authorization"))
		if before, after, ok := strings.Cut(auth, " "); ok && strings.EqualFold(before, "Bearer") {
			got = strings.TrimSpace(after)
		}
	}
	return len(got) == len(token) && subtle.ConstantTimeCompare([]byte(got), []byte(token)) == 1
}

func (s Server) authorizeAdmin(r *http.Request) bool {
	if isLoopbackRemote(r.RemoteAddr) || localDataConnectionAuthorized(r) {
		return true
	}
	if _, ok := s.authorizeTailnet(r); ok {
		return true
	}
	if s.TailnetAuth != nil {
		// Tailnet identity is this server's configured mechanism, so a caller it
		// does not recognize gets only the token path. Falling through to the
		// unsecured legacy default would make enabling authentication widen
		// access to anything that can route to the port.
		return s.matchesConfiguredAdminToken(r)
	}
	if strings.TrimSpace(s.AdminToken) == "" {
		// Preserve explicitly unsecured legacy/local configurations, but never let
		// a scoped import token become the only barrier around remote admin APIs.
		return strings.TrimSpace(s.AccountImportToken) == ""
	}
	return s.matchesConfiguredAdminToken(r)
}

func (s Server) handleSessions(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		assignments := s.Sessions.All()
		views := make([]sessionAdminView, 0, len(assignments))
		for _, assignment := range assignments {
			views = append(views, sessionAdminView{
				Assignment: assignment,
				Active:     s.activeSession(assignment.AgentType, assignment.SessionID),
			})
		}
		writeJSON(w, views)
	case http.MethodDelete:
		agentType := strings.TrimSpace(r.URL.Query().Get("agent_type"))
		sessionID := strings.TrimSpace(r.URL.Query().Get("session_id"))
		if agentType == "" || sessionID == "" {
			http.Error(w, "agent_type and session_id are required", http.StatusBadRequest)
			return
		}
		deleted, err := s.Sessions.Delete(agentType, sessionID)
		if err != nil {
			http.Error(w, "delete session assignment", http.StatusInternalServerError)
			return
		}
		if !deleted {
			http.Error(w, "session assignment not found", http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		w.Header().Set("Allow", "GET, DELETE")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s Server) proxyHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if baseURLProbeRequest(r) {
			if s.RequireSessionLease || !s.localProxyAuthorized(r) {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodPost {
			w.Header().Set("Allow", "GET, POST")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if !s.localProxyAuthorized(r) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		agentType := session.ExtractAgentType(r)
		requestProvider := providerForRequest(agentType, r.URL.Path)
		modelCatalogRequest := requestProvider == accounts.ProviderCodex && codexModelCatalogRequest(r)
		sessionID := session.ExtractID(r, s.MaxBodyBytes)
		if modelCatalogRequest {
			sessionID = codexModelCatalogSessionID
		}
		var boundLease *sessionLease
		if lease, presented, err := s.resolveSessionLease(r); presented {
			if err != nil {
				http.Error(w, err.Error(), http.StatusUnauthorized)
				return
			}
			if !lease.allowsRequest(r) {
				http.Error(w, "session lease does not allow the requested endpoint", http.StatusForbidden)
				return
			}
			if err := lease.validateRequestModel(r); err != nil {
				if s.Logger != nil {
					s.Logger.Warn("session lease model request rejected",
						"provider", lease.Provider,
						"model", lease.Model,
						"method", r.Method,
						"path", r.URL.Path,
						"content_type", r.Header.Get("Content-Type"),
						"content_encoding", r.Header.Get("Content-Encoding"),
						"content_length", r.ContentLength,
						"reason", err,
					)
				}
				http.Error(w, "session lease does not allow the requested model", http.StatusForbidden)
				return
			}
			boundLease = &lease
			agentType = lease.Agent
			sessionID = lease.SessionKey
			requestProvider = lease.Provider
			r.Header.Set("X-Subrouter-Agent", lease.Agent)
			r.Header.Set("X-Subrouter-Session", lease.SessionKey)
			r.Header.Set("X-Subrouter-Account-ID", lease.AccountID)
			if lease.Model != "" {
				r.Header.Set("X-Subrouter-Model", lease.Model)
			}
		} else if s.RequireSessionLease {
			http.Error(w, "session lease required", http.StatusUnauthorized)
			return
		}
		routingRequest := r
		if modelCatalogRequest && boundLease == nil {
			routingRequest = codexModelCatalogRoutingRequest(r)
		}
		// A caller-supplied account selector is a strict per-request binding, not
		// a preference. Session leases install the same header internally but are
		// already exact-bound by their lease, so keep this flag scoped to ordinary
		// caller routing.
		forcedAccountID := ""
		forcedAccountSelection := false
		noRetry := subrouterNoRetryRequest(routingRequest)
		if boundLease == nil {
			var err error
			forcedAccountID, forcedAccountSelection, err = session.ExtractAccountIDWithPresence(routingRequest)
			if err != nil {
				http.Error(w, "invalid forced account selector", http.StatusBadRequest)
				return
			}
		}
		preferredAccountID := ""
		if !forcedAccountSelection {
			preferredAccountID = session.NormalizeAccountID(routingRequest.Header.Get("X-Subrouter-Preferred-Account-ID"))
		}

		if s.Lifecycle != nil && s.Lifecycle.Quiesced() {
			http.Error(w, "subrouter is quiesced", http.StatusServiceUnavailable)
			return
		}
		if s.Lifecycle != nil && s.Lifecycle.Draining() && !s.allowDrainingProxyRequest(agentType, sessionID) {
			http.Error(w, "subrouter is draining", http.StatusServiceUnavailable)
			return
		}
		endProxyRequest, admitted := s.Lifecycle.TryBeginProxyRequest()
		if !admitted {
			http.Error(w, "subrouter is quiesced", http.StatusServiceUnavailable)
			return
		}
		defer endProxyRequest()
		// Claude Fable routing order: subscription pool (Max accounts) first, then
		// AWS Bedrock, then the dedicated Anthropic API key. The fallback stages
		// run when the pool gives up (usageLimitRetryTransport exhausts failover)
		// or cannot start (no usable OAuth account). Other Claude models use the
		// normal pool unchanged.
		requestModel := session.ExtractModel(routingRequest, s.MaxBodyBytes)
		requestPoolModel := ""
		retryPoolModel := requestModel
		fableFallbackConfigured := false
		if requestProvider == accounts.ProviderClaude {
			requestPoolModel = claudePoolModel(requestModel)
			retryPoolModel = requestPoolModel
			fableFallbackConfigured = !noRetry && !forcedAccountSelection && boundLease == nil && s.CredentialBroker == nil &&
				s.claudeFableEnabled() && claudeFableModel(requestModel) &&
				r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/v1/messages")
		}
		// Bedrock-primary: when enabled, serve Fable straight from Bedrock before
		// touching the subscription pool. A non-2xx or unreachable Bedrock restores
		// the body and falls through to the normal pool path (which still carries
		// its own Bedrock/API-key fallback), so this never hard-fails Fable.
		if preferredAccountID == "" && s.FableBedrockPrimary && fableFallbackConfigured && s.Bedrock != nil && s.Bedrock.configured() {
			if s.serveClaudeFableBedrockPrimary(w, r) {
				return
			}
		}
		sessionAgentType := agentTypeForProviderSession(agentType, requestProvider)
		// Codex Azure fallback: the pool stays primary, and Azure answers only
		// after the pool has spent its retry budget or cannot start. Once it
		// has answered, the session is pinned so the following turns reuse the
		// same Azure prompt cache instead of alternating providers, which would
		// re-upload the whole conversation as a cache miss every turn.
		azureCodexConfigured := !forcedAccountSelection && boundLease == nil && s.CredentialBroker == nil &&
			requestProvider == accounts.ProviderCodex && s.AzureCodex.configured() &&
			azureCodexRequest(r.Method, r.URL.Path)
		azureCodexSessionKey := ""
		// A forced request must never quietly fall through to the pool: it is
		// the command that proves the Azure route works, so a misconfigured
		// endpoint has to surface as an error rather than a ChatGPT answer.
		if azureCodexForced(r) && requestProvider == accounts.ProviderCodex &&
			azureCodexRequest(r.Method, r.URL.Path) {
			if !azureCodexConfigured {
				http.Error(w, azureCodexForceUnavailableMessage(s, boundLease != nil, r), http.StatusServiceUnavailable)
				return
			}
			if s.serveAzureCodex(w, r, azureCodexSessionKeyFor(sessionAgentType, sessionID), -1, "forced", false) {
				return
			}
			http.Error(w, "azure codex route could not serve this request; see the daemon log for the endpoint status", http.StatusBadGateway)
			return
		}
		if azureCodexConfigured {
			azureCodexSessionKey = azureCodexSessionKeyFor(sessionAgentType, sessionID)
			if !noRetry {
				if pinned, found := s.azureCodexSessions.lookup(azureCodexSessionKey); found {
					if s.serveAzureCodex(w, r, azureCodexSessionKey, pinned, "sticky_session", true) {
						return
					}
				}
			}
		}
		userEmail := session.ExtractUserEmail(r)
		var account accounts.Account
		var credentialLease *broker.Lease
		var pendingSessionCommit bool
		var pendingSessionExpectedAccount string
		var err error
		if s.CredentialBroker != nil {
			requiredAuthMode := accounts.AuthMode("")
			if requestProvider == accounts.ProviderCodex &&
				(chatGPTBackendPath(r.URL.Path) || modelCatalogRequest) {
				requiredAuthMode = accounts.AuthModeOAuth
			}
			lease, leaseErr := s.CredentialBroker.Lease(r.Context(), broker.LeaseRequest{
				Provider:         requestProvider,
				RequiredAuthMode: requiredAuthMode,
				AgentType:        sessionAgentType,
				SessionID:        sessionID,
				UserEmail:        userEmail,
				PreferAccountID:  preferredAccountID,
				ForceAccountID:   forcedAccountID,
				Model:            session.ExtractModel(routingRequest, s.MaxBodyBytes),
			})
			if leaseErr != nil {
				err = leaseErr
			} else {
				account = lease.Account
				credentialLease = &lease
				if forcedAccountSelection && !accountMatches(account, forcedAccountID) {
					err = fmt.Errorf("requested account %q is unavailable", forcedAccountID)
				}
			}
		} else {
			account, sessionID, userEmail, err = s.accountForSessionProviderWithOptions(
				requestProvider,
				sessionAgentType,
				sessionID,
				routingRequest,
				accountSelectionOptions{oauthOnly: modelCatalogRequest, preferredAccountID: preferredAccountID, pendingSessionCommit: &pendingSessionCommit},
			)
			if err == nil {
				pendingSessionExpectedAccount = account.ID
				if s.Sessions != nil {
					if assignment, ok := s.Sessions.Get(sessionAgentType, sessionID); ok {
						pendingSessionExpectedAccount = assignment.AccountID
					}
				}
				var refreshPendingSessionCommit bool
				account, refreshPendingSessionCommit, err = s.refreshSelectedAccount(
					r.Context(),
					requestProvider,
					sessionAgentType,
					sessionID,
					userEmail,
					routingRequest,
					account,
				)
				pendingSessionCommit = pendingSessionCommit || refreshPendingSessionCommit
				if err != nil {
					err = fmt.Errorf("refresh selected account: %w", err)
				}
			}
		}
		if err != nil {
			if fableFallbackConfigured && s.serveClaudeFableFallback(w, r) {
				return
			}
			if !noRetry && azureCodexConfigured && s.serveAzureCodex(w, r, azureCodexSessionKey, -1, "no_usable_account", true) {
				return
			}
			if !forcedAccountSelection && azureCodexUpgradeShouldFallBack(s, boundLease, requestProvider, r) {
				http.Error(w, "codex pool has no usable account; retry over https", http.StatusUpgradeRequired)
				return
			}
			var brokerHTTPError *broker.HTTPStatusError
			if errors.As(err, &brokerHTTPError) && brokerHTTPError.RetryAfter != "" {
				w.Header().Set("Retry-After", brokerHTTPError.RetryAfter)
			}
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		// A broker lease already carries its own short-lived credential and the
		// account is not in the local store, so refreshing it here would strip
		// the very token the lease provided. The non-broker branch above does
		// its own refresh.
		if boundLease != nil && !boundLease.allowsAccount(account) {
			http.Error(w, "session lease account binding is unavailable", http.StatusServiceUnavailable)
			return
		}

		auth := account.AuthorizationHeader()
		if auth == "" {
			if fableFallbackConfigured && s.serveClaudeFableFallback(w, r) {
				return
			}
			if !noRetry && azureCodexConfigured && s.serveAzureCodex(w, r, azureCodexSessionKey, -1, "no_usable_credential", true) {
				return
			}
			if !forcedAccountSelection && azureCodexUpgradeShouldFallBack(s, boundLease, requestProvider, r) {
				http.Error(w, "codex pool has no usable credential; retry over https", http.StatusUpgradeRequired)
				return
			}
			http.Error(w, "selected account has no usable credential", http.StatusServiceUnavailable)
			return
		}

		endActive := func() {}
		if activeSessionRequest(sessionAgentType, r) {
			endActive = s.ActiveSessions.Begin(sessionAgentType, sessionID)
			defer endActive()
		}

		setAccountAuthHeaders(r.Header, account, requestModel)

		upstream := s.upstreamForRequest(r.URL.Path, account)
		if upstream == nil {
			http.Error(w, "no upstream configured", http.StatusServiceUnavailable)
			return
		}
		if websocket.IsWebSocketUpgrade(r) {
			var azureDivert func(model string) bool
			if !forcedAccountSelection && boundLease == nil && s.CredentialBroker == nil &&
				requestProvider == accounts.ProviderCodex && s.AzureCodex.configured() {
				key := azureCodexSessionKeyFor(sessionAgentType, sessionID)
				if _, pinned := s.azureCodexSessions.lookup(key); pinned {
					// 426 is the one refusal Codex answers by switching the
					// session to the HTTP transport, where the sticky pin
					// serves it from Azure. Any other status ends the turn.
					http.Error(w, "codex session is pinned to the azure fallback; retry over https", http.StatusUpgradeRequired)
					return
				}
				azureDivert = func(model string) bool {
					return s.azureCodexWebSocketDivert(sessionAgentType, sessionID, model)
				}
			}
			s.proxyWebSocket(w, r, account, credentialLease, sessionAgentType, sessionID, userEmail, requestPoolModel, retryPoolModel, upstream, azureDivert, pendingSessionCommit, pendingSessionExpectedAccount)
			return
		}
		proxyRequest := r.Clone(r.Context())
		proxyRequest.URL = cloneURL(r.URL)
		proxyRequest.URL.Path = s.pathForUpstream(proxyRequest.URL.Path, account)
		proxyRequest.URL.RawPath = ""
		session.StripSubrouterHeaders(proxyRequest.Header)
		proxyRequest.Header.Del("X-Subrouter-Preferred-Account-ID")
		s.setDelegatedSessionHeaders(proxyRequest.Header, sessionAgentType, sessionID)
		stripOutboundForwardingHeaders(proxyRequest.Header)
		retryPost := retryableUpstreamPostRequest(requestProvider, proxyRequest)
		postReplayable := false
		if retryPost {
			var replayErr error
			postReplayable, replayErr = makeRequestBodyReplayable(proxyRequest, replayablePostMaxBodyBytes)
			if replayErr != nil {
				http.Error(w, "buffer retryable request body: "+replayErr.Error(), http.StatusBadGateway)
				return
			}
			if !postReplayable && s.Logger != nil {
				s.Logger.Warn("retryable request body exceeds retry buffer", "agent", sessionAgentType, "session", sessionID, "account", account.ID, "method", r.Method, "path", proxyRequest.URL.Path, "content_length", r.ContentLength, "max_bytes", replayablePostMaxBodyBytes)
			}
		}
		s.recordHTTPMeta(proxyRequest, sessionAgentType, sessionID, userEmail, account, upstream)
		if retryPost && postReplayable {
			s.recordReplayableRequestBody(proxyRequest, sessionAgentType, sessionID)
		} else {
			s.captureRequestBody(proxyRequest, sessionAgentType, sessionID)
		}

		rp := &httputil.ReverseProxy{
			Rewrite: func(pr *httputil.ProxyRequest) {
				pr.SetURL(upstream)
				stripOutboundForwardingHeaders(pr.Out.Header)
			},
		}
		transport := s.transport()
		azureCodexFallbackReady := !noRetry && azureCodexConfigured && retryPost && postReplayable
		_, keyedRequestProvider := keyedProviderFor(requestProvider)
		localUsageFailover := account.AuthMode == accounts.AuthModeOAuth &&
			(requestProvider == accounts.ProviderCodex || requestProvider == accounts.ProviderClaude ||
				requestProvider == accounts.ProviderAntigravity || keyedRequestProvider)
		localUsageFailover = localUsageFailover ||
			(account.AuthMode == accounts.AuthModeAPIKey && keyedRequestProvider)
		usageRetryMaxAttempts := 0
		installUsageFailover := !noRetry && !forcedAccountSelection && boundLease == nil && retryPost &&
			postReplayable && localUsageFailover && s.CredentialBroker == nil
		if installUsageFailover {
			usageRetryMaxAttempts = s.usageLimitRetryMaxAttempts(r.Context(), requestProvider)
		}
		// One retry budget per client request, shared by both retry layers. It is
		// required even without a fallback: otherwise each outer POST replay gets
		// a fresh account-failover allowance and six attempts multiply into 36.
		requestMaxAttempts := replayablePostMaxAttempts
		var requestRetryBudget *attemptBudget
		if retryPost && postReplayable {
			requestRetryBudget = newAttemptBudget(requestMaxAttempts - 1)
		}
		usageFailoverInstalled := false
		if installUsageFailover {
			var fableFallback func() (*http.Response, bool)
			if fableFallbackConfigured {
				fableFallback = func() (*http.Response, bool) {
					rc, err := proxyRequest.GetBody()
					if err != nil {
						return nil, false
					}
					fallbackBody, err := io.ReadAll(rc)
					_ = rc.Close()
					if err != nil {
						return nil, false
					}
					return s.claudeFableFallbackResponse(proxyRequest, fallbackBody)
				}
			}
			transport = usageLimitRetryTransport{
				base:              transport,
				server:            &s,
				logger:            s.Logger,
				provider:          requestProvider,
				agent:             sessionAgentType,
				session:           sessionID,
				userEmail:         userEmail,
				account:           account.ID,
				accountCredential: account.CredentialIdentity(),
				method:            r.Method,
				// Keep the client path, before the initially selected account's
				// auth-mode rewrite, so a mixed-auth retry can derive its own path.
				path:               r.URL.Path,
				upstream:           upstream.Host,
				maxAttempts:        usageRetryMaxAttempts,
				poolModel:          retryPoolModel,
				fableFallback:      fableFallback,
				budget:             requestRetryBudget,
				commitFirstSuccess: pendingSessionCommit,
				expectedAccount:    pendingSessionExpectedAccount,
			}
			usageFailoverInstalled = true
		}
		if retryPost && postReplayable {
			postMaxAttempts := replayablePostMaxAttempts
			if noRetry {
				// Deployment canaries need one observable upstream attempt. This is
				// independent of forced-account routing, whose ordinary traffic keeps
				// bounded same-account transport recovery.
				postMaxAttempts = 1
			}
			transport = replayablePostRetryTransport{
				base:        transport,
				logger:      s.Logger,
				agent:       sessionAgentType,
				session:     sessionID,
				account:     account.ID,
				method:      r.Method,
				path:        proxyRequest.URL.Path,
				upstream:    upstream.Host,
				maxAttempts: postMaxAttempts,
				limiter:     replayablePostUploadLimiter,
				budget:      requestRetryBudget,
			}
		}
		if azureCodexFallbackReady {
			transport = azureCodexFallbackTransport{
				base:       transport,
				server:     &s,
				sessionKey: azureCodexSessionKey,
				accountID:  account.ID,
				// requestPoolModel, not retryPoolModel: a Codex usage limit is
				// account-wide, so the mark must not be scoped to one model.
				poolModel: requestPoolModel,
				replayBody: func() ([]byte, bool) {
					rc, err := proxyRequest.GetBody()
					if err != nil {
						return nil, false
					}
					defer rc.Close()
					body, err := io.ReadAll(rc)
					if err != nil {
						return nil, false
					}
					return body, true
				},
			}
		}
		rp.Transport = transport
		rp.ModifyResponse = func(response *http.Response) error {
			if pendingSessionCommit && !usageFailoverInstalled && response.StatusCode >= 200 && response.StatusCode < 300 {
				if err := s.commitSuccessfulHTTPResponse(response, sessionAgentType, sessionID, pendingSessionExpectedAccount, account.ID, userEmail); err != nil {
					return fmt.Errorf("persist successful session reassignment: %w", err)
				}
			}
			responseAccount := account
			if routed, ok := routedResponseAccount(response); ok {
				responseAccount = routed
			}
			s.captureResponseBodyForAccount(response, r.Context(), sessionAgentType, sessionID, responseAccount, requestPoolModel, retryPoolModel, proxyRequest.URL.Path)
			if credentialLease != nil {
				s.reportCredentialLease(
					credentialLease.ID,
					account.Provider,
					account.AuthMode,
					response.StatusCode,
					response.Header,
				)
			}
			return nil
		}
		if s.Logger != nil {
			rp.ErrorLog = log.New(proxyErrorWriter{
				logger:   s.Logger,
				agent:    sessionAgentType,
				session:  sessionID,
				account:  account.ID,
				method:   r.Method,
				path:     proxyRequest.URL.Path,
				upstream: upstream.Host,
			}, "", 0)
		}
		rp.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, err error) {
			if s.Logger != nil {
				s.Logger.Error("proxy request failed", "agent", sessionAgentType, "session", sessionID, "account", account.ID, "method", r.Method, "path", proxyRequest.URL.Path, "upstream", upstream.Host, "error", err)
			}
			if credentialLease != nil {
				s.reportCredentialLease(
					credentialLease.ID,
					account.Provider,
					account.AuthMode,
					http.StatusBadGateway,
					nil,
				)
			}
			http.Error(w, "upstream request failed", http.StatusBadGateway)
		}

		if s.SchedulerRef != nil && s.CredentialBroker == nil {
			// Live debit: the scheduler sees its own routed traffic draining
			// the usage snapshot between refreshes (sticky and fresh picks
			// both consume quota).
			s.SchedulerRef.NoteRouted(schedulerAccountProvider(requestProvider), account.ID)
		}
		if s.Logger != nil {
			// remote_addr + user_agent attribute each request to a source (tailnet
			// device / client type). Without them a concurrency spike is an
			// anonymous wall of user="" sessions that cannot be traced to whatever
			// fired it (a load-test loader, an agent fleet, etc.).
			if markerHash, armed := cutoverCanaryRequestEvidence(proxyRequest, sessionAgentType, sessionID, s.activeSession(sessionAgentType, sessionID), s.cutoverChallenges, time.Now().UTC()); armed {
				// Gate-5 evidence must bind the exact challenged request without
				// publishing the selected account identity. The normal non-canary log
				// shape below remains unchanged for operational compatibility.
				s.Logger.Info("proxy request", privacySafeCutoverRequestLogAttrs(sessionAgentType, sessionID, userEmailHash(userEmail), r.Method, r.URL.Path, upstream.Host, clientRemoteIP(r), r.UserAgent(), markerHash)...)
			} else {
				s.Logger.Info("proxy request", "agent", sessionAgentType, "session", sessionID, "user_hash", userEmailHash(userEmail), "account", account.ID, "method", r.Method, "path", r.URL.Path, "upstream", upstream.Host, "remote_addr", clientRemoteIP(r), "user_agent", r.UserAgent())
			}
		}

		// Read-heavy polling endpoints: identical concurrent requests share
		// one upstream fetch, and the whole catalog walk happens inside the
		// flight so a burst of cold clients costs one walk, not one each.
		// Nothing is retained after the flight completes.
		if r.Method == http.MethodGet && coalescablePath(r.URL.Path) {
			flight, _ := s.CacheFlight.do(flightKey(r), func() flightResult {
				// The flight's work is shared by every waiter, so it must not
				// die with the leader: detach it from the leader's context or
				// one disconnecting client cancels the walk for everyone.
				flightRequest := proxyRequest.Clone(context.WithoutCancel(proxyRequest.Context()))
				rec := &responseRecorder{}
				rp.ServeHTTP(rec, flightRequest)
				body := rec.buf.Bytes()
				if rec.code >= 200 && rec.code < 300 {
					merged, pages, entries, ok, err := aggregateCatalogPages(transport, flightRequest, upstream, body)
					if err != nil {
						// A partial catalog has no continuation token, so the
						// client would cache it as complete (codex pins it on
						// disk for 3 hours). Fail the request instead; a retry
						// is cheap, undoing wrong data is not.
						if s.Logger != nil {
							s.Logger.Warn("catalog aggregation failed", "account", account.ID,
								"path", r.URL.Path, "pages", pages, "entries", entries, "error", err)
						}
						return flightResult{
							statusCode: http.StatusBadGateway,
							header:     http.Header{"Content-Type": []string{"text/plain; charset=utf-8"}},
							body:       []byte("catalog aggregation failed upstream\n"),
						}
					}
					if ok {
						body = merged
						if s.Logger != nil {
							s.Logger.Info("aggregated plugin catalog pages", "account", account.ID,
								"path", r.URL.Path, "pages", pages, "entries", entries)
						}
					}
				}
				header := rec.header
				if header == nil {
					header = make(http.Header)
				}
				header.Del("Content-Length")
				return flightResult{statusCode: rec.code, header: header, body: body}
			})
			for k, vs := range flight.header {
				for _, v := range vs {
					w.Header().Add(k, v)
				}
			}
			if flight.statusCode != 0 {
				w.WriteHeader(flight.statusCode)
			}
			_, _ = w.Write(flight.body)
			return
		}

		rp.ServeHTTP(w, proxyRequest)
	})
}

func privacySafeCutoverRequestLogAttrs(agent, sessionID, userHash, method, path, upstream, remoteAddr, userAgent, markerHash string) []any {
	return []any{
		"agent", agent,
		"session", sessionID,
		"user_hash", userHash,
		"method", method,
		"path", path,
		"upstream", upstream,
		"remote_addr", remoteAddr,
		"user_agent", userAgent,
		"cutover_marker_hash", markerHash,
	}
}

// azureCodexUpgradeShouldFallBack reports whether a Codex websocket upgrade
// that the pool cannot start should be refused with 426 instead of 503. Codex
// switches the session to the HTTP transport only on 426, and only the HTTP
// path can serve the turn from Azure; a 503 ends the turn with nothing tried.
func azureCodexUpgradeShouldFallBack(s Server, boundLease *sessionLease, provider accounts.Provider, r *http.Request) bool {
	return boundLease == nil && s.CredentialBroker == nil &&
		provider == accounts.ProviderCodex && s.AzureCodex.configured() &&
		websocket.IsWebSocketUpgrade(r)
}

func (s Server) localProxyAuthorized(r *http.Request) bool {
	token := strings.TrimSpace(s.LocalProxyToken)
	if token == "" {
		return true
	}
	authorization := strings.TrimSpace(r.Header.Get("Authorization"))
	if len(authorization) <= len("Bearer ") ||
		!strings.EqualFold(authorization[:len("Bearer ")], "Bearer ") {
		return false
	}
	got := strings.TrimSpace(authorization[len("Bearer "):])
	return len(got) == len(token) &&
		subtle.ConstantTimeCompare([]byte(got), []byte(token)) == 1
}

func baseURLProbeRequest(r *http.Request) bool {
	return r.Method == http.MethodHead && (r.URL.Path == "" || r.URL.Path == "/")
}

func credentialLeaseOutcome(
	provider accounts.Provider,
	statusCode int,
	header http.Header,
) broker.LeaseOutcome {
	if statusCode == http.StatusUnauthorized {
		return broker.LeaseUnauthorized
	}
	if provider == accounts.ProviderClaude {
		if claudeUnifiedStatus(header) == "rejected" ||
			statusCode == http.StatusTooManyRequests {
			return broker.LeaseRateLimited
		}
		if statusCode >= 200 && statusCode < 400 {
			return broker.LeaseSuccess
		}
		return broker.LeaseProviderError
	}
	switch {
	case statusCode >= 200 && statusCode < 400:
		return broker.LeaseSuccess
	case statusCode == http.StatusTooManyRequests:
		return broker.LeaseRateLimited
	default:
		return broker.LeaseProviderError
	}
}

func credentialLeaseReport(
	provider accounts.Provider,
	authMode accounts.AuthMode,
	statusCode int,
	header http.Header,
) broker.LeaseReport {
	report := broker.LeaseReport{
		Outcome:    credentialLeaseOutcome(provider, statusCode, header),
		StatusCode: statusCode,
	}
	if report.Outcome == broker.LeaseRateLimited {
		report.CooldownScope = broker.LeaseCooldownAccount
		if provider == accounts.ProviderClaude {
			if claudeRejectionIsModelPoolScoped(header) ||
				(statusCode == http.StatusTooManyRequests &&
					claudeUnifiedStatus(header) != "rejected") {
				report.CooldownScope = broker.LeaseCooldownQuota
			}
			report.RetryAt = claudeExhaustionExpiry(header, time.Now())
		} else {
			report.RetryAt = retryAfterExpiry(header, time.Now())
		}
		return report
	}
	if statusCode != http.StatusForbidden ||
		cloudflareChallengeResponse(header) {
		return report
	}
	report.Outcome = broker.LeaseForbidden
	report.CooldownScope = broker.LeaseCooldownQuota
	if provider == accounts.ProviderClaude &&
		authMode == accounts.AuthModeOAuth {
		// Anthropic uses a bare 403 when OAuth is disabled for the account's
		// organization. Keep every model off that credential without refreshing
		// its still-valid chain. Rejected quota headers took precedence above.
		report.CooldownScope = broker.LeaseCooldownAccount
	}
	return report
}

func retryAfterExpiry(header http.Header, now time.Time) time.Time {
	until := parseRetryAfter(strings.TrimSpace(header.Get("Retry-After")), now)
	if until.IsZero() {
		return time.Time{}
	}
	if maximum := now.Add(8 * 24 * time.Hour); until.After(maximum) {
		return maximum
	}
	return until
}

// parseRetryAfter accepts both forms allowed by HTTP: positive delta-seconds
// and an HTTP-date. Past dates and malformed values carry no retry deadline.
func parseRetryAfter(raw string, now time.Time) time.Time {
	if raw == "" {
		return time.Time{}
	}
	var until time.Time
	if seconds, err := strconv.Atoi(raw); err == nil && seconds > 0 {
		until = now.Add(time.Duration(seconds) * time.Second)
	} else if parsed, err := http.ParseTime(raw); err == nil {
		until = parsed
	}
	if !until.After(now) {
		return time.Time{}
	}
	return until
}

func cloudflareChallengeResponse(header http.Header) bool {
	return strings.EqualFold(
		strings.TrimSpace(header.Get("Cf-Mitigated")),
		"challenge",
	)
}

func (s Server) reportCredentialLease(
	leaseID string,
	provider accounts.Provider,
	authMode accounts.AuthMode,
	statusCode int,
	header http.Header,
) {
	if s.CredentialBroker == nil || leaseID == "" {
		return
	}
	report := credentialLeaseReport(provider, authMode, statusCode, header)
	if report.Outcome == broker.LeaseUnauthorized ||
		report.Outcome == broker.LeaseForbidden ||
		report.Outcome == broker.LeaseRateLimited {
		if invalidator, ok := s.CredentialBroker.(interface {
			InvalidateLease(string)
		}); ok {
			invalidator.InvalidateLease(leaseID)
		}
		// The next local request must not race ahead of central refresh or quota
		// rotation and receive the same failed credential again.
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := s.CredentialBroker.Report(ctx, leaseID, report); err != nil && s.Logger != nil {
			s.Logger.Warn("credential lease report failed", "lease", leaseID, "status", statusCode, "error", err)
		}
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := s.CredentialBroker.Report(ctx, leaseID, report); err != nil && s.Logger != nil {
			s.Logger.Warn("credential lease report failed", "lease", leaseID, "status", statusCode, "error", err)
		}
	}()
}

func (s Server) proxyWebSocket(w http.ResponseWriter, r *http.Request, account accounts.Account, credentialLease *broker.Lease, agentType, sessionID, userEmail, poolModel, compatibilityModel string, upstream *url.URL, azureDivert func(model string) bool, pendingSessionCommit bool, pendingSessionExpectedAccount string) {
	if !webSocketOriginAllowed(r) {
		http.Error(w, "websocket origin not allowed", http.StatusForbidden)
		return
	}

	upstreamURL := cloneURL(r.URL)
	upstreamURL.Scheme = websocketScheme(upstream.Scheme)
	upstreamURL.Host = upstream.Host
	upstreamURL.Path = joinURLPath(upstream.Path, s.pathForUpstream(upstreamURL.Path, account))
	upstreamURL.RawPath = ""

	headers := r.Header.Clone()
	stripWebSocketDialHeaders(headers)
	session.StripSubrouterHeaders(headers)
	s.setDelegatedSessionHeaders(headers, agentType, sessionID)
	stripOutboundForwardingHeaders(headers)
	setAccountAuthHeaders(headers, account, compatibilityModel)
	s.recordWebSocketMeta(r, upstreamURL, headers, agentType, sessionID, userEmail, account, upstream)

	upstreamConn, response, err := outboundWebSocketDialer().Dial(upstreamURL.String(), headers)
	if err != nil {
		status := 502
		if response != nil {
			status = response.StatusCode
		}
		// Log it. A failed dial previously produced no log line at all, so a
		// client seeing "Connection reset without closing handshake" left
		// nothing behind to explain it: the whole log had zero websocket
		// entries despite every attempt failing. The upstream's status and the
		// first bytes of its body are what distinguish an edge challenge from
		// an origin rejection, and they are discarded once this returns.
		if s.Logger != nil {
			s.Logger.Error("websocket upstream dial failed",
				"agent", agentType,
				"session", sessionID,
				"account", account.ID,
				"upstream", upstreamURL.Host,
				"path", upstreamURL.Path,
				"status", status,
				"error", err,
				"upstream_server", websocketResponseHeader(response, "Server"),
				"cf_mitigated", websocketResponseHeader(response, "Cf-Mitigated"),
				"cf_ray", websocketResponseHeader(response, "Cf-Ray"),
				"content_type", websocketResponseHeader(response, "Content-Type"))
		}
		if credentialLease != nil {
			var responseHeader http.Header
			if response != nil {
				responseHeader = response.Header
			}
			s.reportCredentialLease(
				credentialLease.ID,
				account.Provider,
				account.AuthMode,
				status,
				responseHeader,
			)
		}
		http.Error(w, err.Error(), status)
		return
	}
	defer upstreamConn.Close()
	if response != nil && response.Body != nil {
		defer response.Body.Close()
	}

	upgrader := websocket.Upgrader{}
	responseHeader := http.Header{}
	if response != nil {
		responseHeader = cloneWebSocketResponseHeaders(response.Header)
	}
	clientConn, err := upgrader.Upgrade(w, r, responseHeader)
	if err != nil {
		return
	}
	defer clientConn.Close()
	if pendingSessionCommit {
		if _, err := s.commitSessionReassignment(agentType, sessionID, pendingSessionExpectedAccount, account.ID, userEmail); err != nil {
			if s.Logger != nil {
				s.Logger.Error("closing websocket after session reassignment persistence failed", "agent", agentType, "session", sessionID, "account", account.ID, "error", err)
			}
			return
		}
	}
	clientConn.SetReadLimit(maxWebSocketMessageBytes)
	upstreamConn.SetReadLimit(maxWebSocketMessageBytes)

	modelState := &webSocketModelState{model: compatibilityModel}
	var leaseFailureReported atomic.Bool
	reportLeaseFailure := func(statusCode int) {
		if credentialLease == nil ||
			!leaseFailureReported.CompareAndSwap(false, true) {
			return
		}
		s.reportCredentialLease(
			credentialLease.ID,
			account.Provider,
			account.AuthMode,
			statusCode,
			nil,
		)
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		s.copyWebSocketMessages(r.Context(), account.Provider, agentType, sessionID, userEmail, account.ID, poolModel, modelState, "client_to_upstream", clientConn, upstreamConn, nil, nil)
		_ = upstreamConn.Close()
	}()
	go func() {
		defer wg.Done()
		s.copyWebSocketMessages(r.Context(), account.Provider, agentType, sessionID, userEmail, account.ID, poolModel, modelState, "upstream_to_client", upstreamConn, clientConn, reportLeaseFailure, azureDivert)
		_ = clientConn.Close()
	}()
	wg.Wait()
	if credentialLease != nil &&
		leaseFailureReported.CompareAndSwap(false, true) {
		s.reportCredentialLease(
			credentialLease.ID,
			account.Provider,
			account.AuthMode,
			http.StatusSwitchingProtocols,
			nil,
		)
	}
}

func webSocketOriginAllowed(r *http.Request) bool {
	origin := strings.TrimSpace(r.Header.Get("Origin"))
	if origin == "" {
		// Native Codex and Claude clients do not send Origin. Browser clients do,
		// and must be constrained to the host they reached.
		return true
	}
	parsed, err := url.Parse(origin)
	return err == nil && parsed.Host != "" && strings.EqualFold(parsed.Host, r.Host)
}

// setDelegatedSessionHeaders preserves routing affinity only for an explicit
// --upstream proxy chain. Provider endpoints never receive these internal
// headers through their normal provider-specific upstream configuration.
func (s Server) setDelegatedSessionHeaders(headers http.Header, agentType, sessionID string) {
	if s.Upstream == nil || !s.ForwardSessionHeaders {
		return
	}
	headers.Set("X-Subrouter-Agent", agentType)
	headers.Set("X-Subrouter-Session", sessionID)
}

func stripWebSocketDialHeaders(headers http.Header) {
	for _, key := range []string{
		"Connection",
		"Upgrade",
		"Sec-Websocket-Key",
		"Sec-WebSocket-Key",
		"Sec-Websocket-Version",
		"Sec-WebSocket-Version",
		"Sec-Websocket-Extensions",
		"Sec-WebSocket-Extensions",
		"Sec-Websocket-Accept",
		"Sec-WebSocket-Accept",
	} {
		headers.Del(key)
	}
}

func cloneWebSocketResponseHeaders(headers http.Header) http.Header {
	out := http.Header{}
	for key, values := range headers {
		lower := strings.ToLower(key)
		if lower == "connection" || lower == "upgrade" || strings.HasPrefix(lower, "sec-websocket-") {
			continue
		}
		out[key] = append([]string(nil), values...)
	}
	return out
}

type webSocketModelState struct {
	mu      sync.RWMutex
	model   string
	pending []string
}

func (s *webSocketModelState) observe(body []byte) {
	model, ok := codexWebSocketRequestModelEvent(body)
	if !ok {
		return
	}
	s.mu.Lock()
	if model == "" {
		model = s.model
	}
	s.pending = append(s.pending, model)
	s.mu.Unlock()
}

func (s *webSocketModelState) current() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if len(s.pending) > 0 {
		return s.pending[0]
	}
	return s.model
}

func (s *webSocketModelState) complete() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.pending) > 0 {
		s.pending = s.pending[1:]
	}
}

func codexWebSocketRequestModel(body []byte) string {
	model, _ := codexWebSocketRequestModelEvent(body)
	return model
}

func codexWebSocketRequestModelEvent(body []byte) (string, bool) {
	var event map[string]any
	if err := json.Unmarshal(body, &event); err != nil || !strings.EqualFold(stringField(event, "type"), "response.create") {
		return "", false
	}
	if model := session.NormalizeModel(stringField(event, "model")); model != "" {
		return model, true
	}
	response, _ := event["response"].(map[string]any)
	return session.NormalizeModel(stringField(response, "model")), true
}

func codexWebSocketResponseFinished(body []byte) bool {
	var event map[string]any
	if err := json.Unmarshal(body, &event); err != nil {
		return false
	}
	switch strings.ToLower(stringField(event, "type")) {
	case "error", "response.completed", "response.failed", "response.incomplete", "response.done":
		return true
	default:
		return false
	}
}

func (s Server) copyWebSocketMessages(ctx context.Context, provider accounts.Provider, agentType, sessionID, userEmail, accountID, poolModel string, modelState *webSocketModelState, direction string, src, dst *websocket.Conn, reportLeaseFailure func(int), azureDivert func(model string) bool) {
	observeMessage := func(messageType int, body []byte) error {
		if messageType == websocket.TextMessage && direction == "client_to_upstream" && provider == accounts.ProviderCodex {
			modelState.observe(body)
		}
		if direction == "upstream_to_client" && messageType == websocket.TextMessage {
			if provider == accounts.ProviderCodex && !codexChatGPTModelUnsupportedJSON(body) {
				switch codexTurnFailureClass(body) {
				case codexFailureQuota:
					// The event is terminal for Codex, so it must not be
					// delivered. Mark the account and close 1012: the
					// reconnect picks another account, and when nothing in
					// the pool can start it, the upgrade answers 426 and the
					// HTTP path reaches the fallback.
					s.markAccountExhausted(provider, accountID, poolModel)
					if reportLeaseFailure != nil {
						reportLeaseFailure(http.StatusTooManyRequests)
					}
					return errCodexWebSocketReroute
				case codexFailureServer:
					if azureDivert != nil {
						model := modelState.current()
						if model == "" {
							model = poolModel
						}
						if azureDivert(model) {
							return errAzureCodexWebSocketDivert
						}
					}
				}
			}
			switch {
			case usageLimitJSON(body):
				s.markAccountExhausted(provider, accountID, poolModel)
				if reportLeaseFailure != nil {
					reportLeaseFailure(http.StatusTooManyRequests)
				}
			case credentialUnauthorizedJSON(body):
				if reportLeaseFailure != nil {
					reportLeaseFailure(http.StatusUnauthorized)
				}
			case provider == accounts.ProviderCodex && codexChatGPTModelUnsupportedJSON(body):
				if model := modelState.current(); model != "" {
					// A WebSocket cannot replay the rejected turn in-place. The
					// selected account is therefore the destination for the next
					// reconnect, unlike replayable HTTP where it stays provisional
					// until a replacement response succeeds.
					err := s.rerouteModelIncompatibilityForReconnect(ctx, provider, agentType, sessionID, userEmail, accountID, model)
					if err != nil && s.Logger != nil {
						s.Logger.Error("websocket model reroute could not persist next account", "agent", agentType, "session", sessionID, "account", accountID, "model", model, "error", err)
					}
				}
			}
			if provider == accounts.ProviderCodex && codexWebSocketResponseFinished(body) {
				modelState.complete()
			}
		}
		return nil
	}
	for {
		err := s.forwardWebSocketMessage(ctx, agentType, sessionID, direction, src, dst, observeMessage)
		if err != nil {
			if errors.Is(err, errAzureCodexWebSocketDivert) {
				closeWebSocketWithServiceRestart(dst, "codex pool is at capacity; reconnect")
				return
			}
			if errors.Is(err, errCodexWebSocketReroute) {
				closeWebSocketWithServiceRestart(dst, "codex account exhausted; reconnect")
				return
			}
			forwardWebSocketClose(dst, err)
			return
		}
	}
}

func (s Server) forwardWebSocketMessage(ctx context.Context, agentType, sessionID, direction string, src, dst *websocket.Conn, observe func(int, []byte) error) error {
	messageType, reader, err := src.NextReader()
	if err != nil {
		return err
	}
	observer := newWebSocketMessageObserver(s.Transcripts, agentType, sessionID, direction, messageType)
	_, release, err := streamWebSocketMessage(ctx, reader, func() (io.WriteCloser, error) {
		return dst.NextWriter(messageType)
	}, observer, webSocketForwardBuffers, func(body []byte) error {
		return observe(messageType, body)
	})
	if release != nil {
		release()
	}
	return err
}

func streamWebSocketMessage(
	ctx context.Context,
	reader io.Reader,
	openWriter func() (io.WriteCloser, error),
	observer *webSocketMessageObserver,
	buffers *webSocketCopyBufferPool,
	beforeClose func([]byte) error,
) ([]byte, func(), error) {
	buffer, releaseBuffer, err := buffers.acquire(ctx)
	if err != nil {
		observer.abort()
		return nil, nil, err
	}
	defer releaseBuffer()

	writer, err := openWriter()
	if err != nil {
		observer.abort()
		return nil, nil, err
	}
	writerOpen := true
	closeWriter := func() error {
		if !writerOpen {
			return nil
		}
		writerOpen = false
		return writer.Close()
	}
	defer func() { _ = closeWriter() }()

	var total int64
	for {
		n, readErr := reader.Read(buffer)
		if n > 0 {
			if total+int64(n) > maxWebSocketMessageBytes {
				observer.abort()
				return nil, nil, websocket.ErrReadLimit
			}
			chunk := buffer[:n]
			observer.observe(chunk)
			_, writeErr := writer.Write(chunk)
			if writeErr != nil {
				observer.abort()
				return nil, nil, writeErr
			}
			total += int64(n)
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			observer.abort()
			return nil, nil, readErr
		}
	}
	body, release := observer.finish()
	if beforeClose != nil {
		if err := beforeClose(body); err != nil {
			// The message must not be delivered: mark the writer closed
			// without closing it, so the final frame is never flushed, and
			// surface the veto to the copy loop.
			writerOpen = false
			return body, release, err
		}
	}
	if err := closeWriter(); err != nil {
		return body, release, err
	}
	return body, release, nil
}

const webSocketCloseWriteTimeout = time.Second

const (
	maxWebSocketMessageBytes     = 8 << 20
	webSocketCopyChunkBytes      = 32 << 10
	webSocketForwardBudgetBytes  = 32 << 20
	webSocketInspectMessageBytes = maxWebSocketMessageBytes
	webSocketInspectBudgetBytes  = 32 << 20
)

var (
	webSocketForwardBuffers = newWebSocketCopyBufferPool(
		newWebSocketByteBudget(webSocketForwardBudgetBytes),
		webSocketCopyChunkBytes,
	)
	webSocketInspectBudget = newWebSocketByteBudget(webSocketInspectBudgetBytes)
)

type webSocketByteBudget struct {
	mu      sync.Mutex
	limit   int64
	used    int64
	changed chan struct{}
}

func newWebSocketByteBudget(limit int64) *webSocketByteBudget {
	return &webSocketByteBudget{limit: limit, changed: make(chan struct{})}
}

func (b *webSocketByteBudget) reserve(ctx context.Context, bytes int64) (func(), error) {
	if bytes < 0 || bytes > b.limit {
		return nil, fmt.Errorf("websocket buffer reservation %d exceeds budget %d", bytes, b.limit)
	}
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		b.mu.Lock()
		if b.used+bytes <= b.limit {
			b.used += bytes
			b.mu.Unlock()
			var once sync.Once
			return func() { once.Do(func() { b.release(bytes) }) }, nil
		}
		changed := b.changed
		b.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-changed:
		}
	}
}

type webSocketCopyBufferPool struct {
	budget   *webSocketByteBudget
	size     int
	allocate func(int) []byte
}

func newWebSocketCopyBufferPool(budget *webSocketByteBudget, size int) *webSocketCopyBufferPool {
	return &webSocketCopyBufferPool{budget: budget, size: size}
}

func (p *webSocketCopyBufferPool) acquire(ctx context.Context) ([]byte, func(), error) {
	release, err := p.budget.reserve(ctx, int64(p.size))
	if err != nil {
		return nil, nil, err
	}
	allocate := p.allocate
	if allocate == nil {
		allocate = func(size int) []byte { return make([]byte, size) }
	}
	return allocate(p.size), release, nil
}

func (b *webSocketByteBudget) tryReserve(bytes int64) bool {
	if bytes < 0 || bytes > b.limit {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.used+bytes > b.limit {
		return false
	}
	b.used += bytes
	return true
}

func (b *webSocketByteBudget) release(bytes int64) {
	b.mu.Lock()
	b.used -= bytes
	close(b.changed)
	b.changed = make(chan struct{})
	b.mu.Unlock()
}

type webSocketMessageObserver struct {
	recorder    *transcript.Recorder
	agentType   string
	sessionID   string
	direction   string
	messageType int
	streamID    string
	hasher      hash.Hash
	capture     []byte
	reserved    int64
	bytesRead   int64
	chunks      int
	chunked     bool
}

func newWebSocketMessageObserver(recorder *transcript.Recorder, agentType, sessionID, direction string, messageType int) *webSocketMessageObserver {
	return &webSocketMessageObserver{
		recorder: recorder, agentType: agentType, sessionID: sessionID, direction: direction,
		messageType: messageType, streamID: nextTranscriptStreamID(), hasher: sha256.New(),
	}
}

func (o *webSocketMessageObserver) observe(body []byte) {
	_, _ = o.hasher.Write(body)
	if !o.chunked && len(o.capture)+len(body) <= webSocketInspectMessageBytes && webSocketInspectBudget.tryReserve(int64(len(body))) {
		o.capture = append(o.capture, body...)
		o.reserved += int64(len(body))
		o.bytesRead += int64(len(body))
		return
	}
	if !o.chunked {
		o.chunked = true
		o.recordChunks(o.capture, 0)
		if o.reserved > 0 {
			webSocketInspectBudget.release(o.reserved)
		}
		o.reserved = 0
		o.capture = nil
	}
	o.recordChunks(body, o.bytesRead)
	o.bytesRead += int64(len(body))
}

func (o *webSocketMessageObserver) recordChunks(body []byte, offset int64) {
	if o.recorder == nil || !o.recorder.Enabled() {
		return
	}
	payload := map[string]any{"opcode": websocketMessageType(o.messageType)}
	for len(body) > 0 {
		chunk := body
		if len(chunk) > transcriptHTTPChunkBytes {
			chunk = body[:transcriptHTTPChunkBytes]
		}
		o.recorder.RecordPayloadChunk(o.agentType, o.sessionID, "websocket_message", o.direction, o.streamID, o.chunks, offset, chunk, payload)
		o.chunks++
		offset += int64(len(chunk))
		body = body[len(chunk):]
	}
}

func (o *webSocketMessageObserver) finish() ([]byte, func()) {
	payload := map[string]any{"opcode": websocketMessageType(o.messageType)}
	if o.chunked {
		if o.recorder != nil && o.recorder.Enabled() {
			o.recorder.RecordPayloadSummary(o.agentType, o.sessionID, "websocket_message", o.direction, o.streamID, o.bytesRead, hex.EncodeToString(o.hasher.Sum(nil)), o.chunks, payload)
		}
		return nil, func() {}
	}
	if o.recorder != nil && o.recorder.Enabled() {
		o.recorder.RecordPayload(o.agentType, o.sessionID, "websocket_message", o.direction, o.capture, payload)
	}
	var once sync.Once
	return o.capture, func() {
		once.Do(func() {
			if o.reserved > 0 {
				webSocketInspectBudget.release(o.reserved)
			}
			o.reserved = 0
			o.capture = nil
		})
	}
}

func (o *webSocketMessageObserver) abort() {
	if o.reserved > 0 {
		webSocketInspectBudget.release(o.reserved)
		o.reserved = 0
	}
	o.capture = nil
}

// forwardWebSocketClose preserves the close handshake across the proxy. A raw
// TCP close makes the other peer report abnormal closure 1006 even when the
// first peer sent a valid WebSocket close frame. Unexpected transport loss is
// translated to 1011 so the proxy still terminates with a valid close frame.
// errAzureCodexWebSocketDivert says an upstream Codex websocket message was a
// capacity failure the Azure fallback will absorb: the message must not reach
// the client (Codex treats it as terminal), the session is already pinned,
// and both connections close so the client reconnects — its next upgrade is
// refused with 426, which is the one status Codex answers by switching to the
// HTTP transport, where the sticky pin serves the turn from Azure.
var errAzureCodexWebSocketDivert = errors.New("codex websocket turn diverted to the azure fallback")

// errCodexWebSocketReroute says an upstream Codex websocket message was an
// account-level failure (quota): the account is already marked exhausted, the
// message must not reach the client, and the connection closes with 1012 so
// the client reconnects and the account pick lands somewhere usable. No pin:
// the pool's other accounts are free and come first; the fallback catches the
// reconnect only when nothing in the pool can start it.
var errCodexWebSocketReroute = errors.New("codex websocket turn rerouted off an exhausted account")

// closeWebSocketWithServiceRestart ends the client connection with 1012
// (service restart), which Codex handles by reconnecting with a full
// response.create.
func closeWebSocketWithServiceRestart(dst *websocket.Conn, reason string) {
	_ = dst.WriteControl(
		websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseServiceRestart, reason),
		time.Now().Add(webSocketCloseWriteTimeout),
	)
}

func forwardWebSocketClose(dst *websocket.Conn, readErr error) {
	code := websocket.CloseInternalServerErr
	reason := "peer connection closed unexpectedly"
	var closeErr *websocket.CloseError
	if errors.Is(readErr, websocket.ErrReadLimit) {
		code = websocket.CloseMessageTooBig
		reason = "websocket message exceeds proxy limit"
	} else if errors.As(readErr, &closeErr) && webSocketCloseCodeCanBeForwarded(closeErr.Code) {
		code = closeErr.Code
		reason = closeErr.Text
	}
	_ = dst.WriteControl(
		websocket.CloseMessage,
		websocket.FormatCloseMessage(code, reason),
		time.Now().Add(webSocketCloseWriteTimeout),
	)
}

func webSocketCloseCodeCanBeForwarded(code int) bool {
	switch code {
	case websocket.CloseNoStatusReceived, websocket.CloseAbnormalClosure, websocket.CloseTLSHandshake:
		return false
	default:
		return true
	}
}

func (s Server) markAccountExhausted(provider accounts.Provider, accountID, poolKey string) {
	if s.SchedulerRef != nil {
		s.SchedulerRef.MarkExhausted(schedulerAccountProvider(provider), accountID, poolKey)
	}
}

// markAccountExhaustedFromResponse marks the account exhausted until the reset
// time the upstream itself reported, so the mark self-expires exactly when the
// account's window recovers. Observed live: an account whose weekly window had
// reset (real quota available) stayed zero-scored for hours because marks only
// cleared on a successful usage refresh, which the loaded usage endpoint kept
// failing; failover then burned its attempts on genuinely-cooked accounts and
// never reached the recovered one.
func (s Server) markAccountExhaustedFromResponse(provider accounts.Provider, accountID, poolKey string, status int, header http.Header) {
	s.markAccountExhaustedFromResponseForAccount(
		s.AccountRef.credentialSnapshot(provider, accountID), poolKey, status, header,
	)
}

func (s Server) markAccountExhaustedFromResponseForAccount(account accounts.Account, poolKey string, status int, header http.Header) {
	if s.SchedulerRef == nil {
		return
	}
	if status == http.StatusUnauthorized {
		// A dead/expired credential is repaired by replacing its OAuth chain.
		s.markAccountExhaustedCredentialForAccount(account)
		return
	}
	// Kimi is the exception: its documented quota limits are 403s, and the
	// caller reaches this method only after matching an exact quota message.
	if status == http.StatusForbidden && account.Provider != accounts.ProviderKimi {
		// Org-level OAuth disablement is account state, not credential state:
		// not a rate-limit window, no reset header exists, and neither
		// self-heals on a schedule. A longer TTL avoids probing every few
		// minutes while still picking the account back up within the hour after
		// an org re-enable. Token rotation must not clear this exclusion.
		s.SchedulerRef.MarkAccountUnavailableUntil(
			schedulerAccountProvider(account.Provider), account.ID, time.Now().Add(credentialExhaustionTTL),
		)
		return
	}
	s.SchedulerRef.MarkExhaustedUntil(schedulerAccountProvider(account.Provider), account.ID, poolKey, claudeExhaustionExpiry(header, time.Now()))
}

// credentialExhaustionTTL is how long an account with a dead credential
// (401 / invalid_grant) stays out of routing before one re-probe. Credentials
// only recover via human re-auth, so probes are pure overhead; but the mark
// must still lapse so a re-authed account rejoins without waiting for a
// successful usage refresh.
const credentialExhaustionTTL = time.Hour

func (s Server) markAccountExhaustedCredential(provider accounts.Provider, accountID, poolKey string) {
	s.markAccountExhaustedCredentialForAccount(s.AccountRef.credentialSnapshot(provider, accountID))
}

func (s Server) markAccountExhaustedCredentialForAccount(account accounts.Account) {
	if s.SchedulerRef == nil {
		return
	}
	// Legacy/test servers without a reloadable AccountRef have no credential
	// generation to advance, so retain the historical TTL-backed behavior.
	if s.AccountRef == nil {
		s.SchedulerRef.MarkExhaustedUntil(
			schedulerAccountProvider(account.Provider), account.ID, "", time.Now().Add(credentialExhaustionTTL),
		)
		return
	}
	loaded, generation, credentialRevision := s.AccountRef.CredentialSnapshot()
	current := false
	for _, candidate := range loaded {
		if sameCredentialProvider(candidate.Provider, account.Provider) && candidate.ID == account.ID &&
			candidate.CredentialIdentity() == account.CredentialIdentity() {
			current = true
			break
		}
	}
	if !current {
		return
	}
	s.SchedulerRef.MarkCredentialExhaustedForSnapshot(
		schedulerAccountProvider(account.Provider), account.ID, account.CredentialIdentity(), time.Now().Add(credentialExhaustionTTL),
		generation, credentialRevision, SchedulerAccounts(loaded),
	)
}

// markAccountExhaustedRefreshFailure picks the mark TTL by failure class: a
// terminal credential error gets the long credential TTL, anything transient
// gets the short default so the account rejoins quickly.
func (s Server) markAccountExhaustedRefreshFailure(account accounts.Account, err error) {
	if isTerminalCredentialError(err) {
		s.markAccountExhaustedCredentialForAccount(account)
		return
	}
	s.markAccountExhausted(account.Provider, account.ID, "")
}

// claudeExhaustionExpiry picks when an exhaustion mark should lapse:
// anthropic-ratelimit-unified-reset (epoch seconds, the authoritative window
// reset) when present, else Retry-After delta-seconds or HTTP-date, else the
// scheduler default.
// Clamped to [now+1m, now+8d]: the floor guards clock skew / already-passed
// resets, the ceiling guards a nonsense far-future header pinning an account
// out forever.
func claudeExhaustionExpiry(header http.Header, now time.Time) time.Time {
	until := now.Add(selectacct.DefaultExhaustedTTL)
	if raw := strings.TrimSpace(claudeHeaderGet(header, "anthropic-ratelimit-unified-reset")); raw != "" {
		if epoch, err := strconv.ParseInt(raw, 10, 64); err == nil && epoch > 0 {
			until = time.Unix(epoch, 0)
		}
	} else if retryAt := parseRetryAfter(strings.TrimSpace(claudeHeaderGet(header, "Retry-After")), now); !retryAt.IsZero() {
		until = retryAt
	}
	if min := now.Add(time.Minute); until.Before(min) {
		return min
	}
	if max := now.Add(8 * 24 * time.Hour); until.After(max) {
		return max
	}
	return until
}

// codexFailureClass says who can fix a failed Codex turn, which decides where
// the proxy routes it.
type codexFailureClass int

const (
	// codexFailureNone: not a turn failure; forward it.
	codexFailureNone codexFailureClass = iota
	// codexFailureClient: the request's own fault (too long, flagged,
	// malformed). Every provider refuses it the same way, so it is forwarded
	// untouched.
	codexFailureClient
	// codexFailureQuota: this account is out. Mark it exhausted and reroute
	// so the next attempt lands on another account or the fallback.
	codexFailureQuota
	// codexFailureServer: the provider's fault, including every code this
	// proxy has never seen. Codex treats an unrecognized failure as the end
	// of the turn (or retries it into the same broken pool), so the default
	// for an unknown code is to absorb it and let the fallback try. If the
	// fallback cannot serve it, the original error still reaches the client.
	codexFailureServer
)

// codexTurnFailureClass classifies a Codex turn-failure event. Only
// failure-shaped payloads classify: a response.failed or error event, or one
// carrying an error object with a code. Everything else is codexFailureNone.
func codexTurnFailureClass(body []byte) codexFailureClass {
	var event map[string]any
	if err := json.Unmarshal(body, &event); err != nil {
		return codexFailureNone
	}
	eventType := strings.ToLower(strings.TrimSpace(stringField(event, "type")))
	code, message := codexFailureCodeAndMessage(event)
	failureShaped := eventType == "response.failed" || eventType == "error" || code != ""
	if !failureShaped {
		return codexFailureNone
	}
	switch code {
	// The request's own fault: same refusal from every provider.
	case "context_length_exceeded", "invalid_prompt", "bio_policy", "cyber_policy",
		"unknown_parameter", "unsupported_parameter", "unsupported_value",
		"invalid_encrypted_content", "invalid_request_error", "invalid_image",
		"invalid_base64", "image_parse_error":
		return codexFailureClient
	// This account is out; another one (or the fallback) can still serve.
	case "usage_limit_reached", "insufficient_quota", "usage_not_included",
		"quota_exceeded", "rate_limit_exceeded":
		return codexFailureQuota
	}
	lower := strings.ToLower(message)
	if strings.Contains(lower, "context window") ||
		strings.Contains(lower, "context length") ||
		strings.Contains(lower, "maximum context") {
		return codexFailureClient
	}
	if usageLimitMessage(message) {
		return codexFailureQuota
	}
	return codexFailureServer
}

// codexFailureCodeAndMessage digs the error code and message out of a failure
// event, whichever level they sit at: top level, under error, or under
// response.error.
func codexFailureCodeAndMessage(event map[string]any) (string, string) {
	code := strings.ToLower(strings.TrimSpace(stringField(event, "code")))
	message := strings.TrimSpace(stringField(event, "message"))
	for _, key := range []string{"error", "response"} {
		nested, ok := event[key].(map[string]any)
		if !ok {
			continue
		}
		nestedCode, nestedMessage := codexFailureCodeAndMessage(nested)
		if code == "" {
			code = nestedCode
		}
		if message == "" {
			message = nestedMessage
		}
	}
	return code, message
}

func usageLimitJSON(body []byte) bool {
	var event map[string]any
	if err := json.Unmarshal(body, &event); err != nil {
		return false
	}
	return usageLimitMap(event)
}

func credentialUnauthorizedJSON(body []byte) bool {
	var event map[string]any
	if err := json.Unmarshal(body, &event); err != nil {
		return false
	}
	return credentialUnauthorizedMap(event)
}

func credentialUnauthorizedMap(event map[string]any) bool {
	for _, key := range []string{"type", "code"} {
		switch strings.ToLower(strings.TrimSpace(stringField(event, key))) {
		case "authentication_error",
			"unauthorized",
			"invalid_token",
			"invalid_api_key",
			"token_expired",
			"invalid_grant":
			return true
		}
	}
	message := strings.ToLower(strings.TrimSpace(stringField(event, "message")))
	if strings.Contains(message, "invalid authentication") ||
		strings.Contains(message, "invalid access token") ||
		strings.Contains(message, "token has expired") {
		return true
	}
	if nested, ok := event["error"].(map[string]any); ok {
		return credentialUnauthorizedMap(nested)
	}
	return false
}

func usageLimitMap(event map[string]any) bool {
	if usageLimitCode(stringField(event, "type")) || usageLimitCode(stringField(event, "code")) {
		return true
	}
	if usageLimitMessage(stringField(event, "message")) {
		return true
	}
	switch value := event["error"].(type) {
	case map[string]any:
		return usageLimitMap(value)
	case string:
		return usageLimitMessage(value)
	default:
		return false
	}
}

func usageLimitCode(value string) bool {
	return strings.EqualFold(value, "usage_limit_reached")
}

func usageLimitMessage(value string) bool {
	lower := strings.ToLower(value)
	return strings.Contains(lower, "usage limit") &&
		(strings.Contains(lower, "reached") || strings.Contains(lower, "hit") || strings.Contains(lower, "exceeded"))
}

func codexChatGPTModelUnsupportedJSON(body []byte) bool {
	var event map[string]any
	if err := json.Unmarshal(body, &event); err != nil {
		return false
	}
	return codexChatGPTModelUnsupportedMap(event)
}

func codexChatGPTModelUnsupportedMap(event map[string]any) bool {
	message := strings.ToLower(stringField(event, "message"))
	if strings.Contains(message, "model is not supported when using codex with a chatgpt account") ||
		(strings.Contains(message, "model") &&
			strings.Contains(message, "not supported") &&
			strings.Contains(message, "codex") &&
			strings.Contains(message, "chatgpt account")) {
		return true
	}
	if nested, ok := event["error"].(map[string]any); ok {
		return codexChatGPTModelUnsupportedMap(nested)
	}
	return false
}

func stringField(values map[string]any, key string) string {
	value, _ := values[key].(string)
	return value
}

func websocketScheme(scheme string) string {
	if scheme == "https" {
		return "wss"
	}
	return "ws"
}

func joinURLPath(basePath, requestPath string) string {
	if basePath == "" || basePath == "/" {
		if requestPath == "" {
			return "/"
		}
		return requestPath
	}
	if requestPath == "" || requestPath == "/" {
		return basePath
	}
	return strings.TrimRight(basePath, "/") + "/" + strings.TrimLeft(requestPath, "/")
}

func websocketMessageType(messageType int) string {
	switch messageType {
	case websocket.TextMessage:
		return "text"
	case websocket.BinaryMessage:
		return "binary"
	case websocket.CloseMessage:
		return "close"
	case websocket.PingMessage:
		return "ping"
	case websocket.PongMessage:
		return "pong"
	default:
		return "unknown"
	}
}

// transcriptWorthRecording reports whether a session's traffic belongs in a
// transcript.
//
// The synthetic catalog session is not a conversation: every Codex client polls
// /models continuously, all of it lands under one session id, and each poll
// records the request meta plus the whole catalog body. On the team server that
// single file reached 30 GB in two days, dwarfed every real transcript,
// saturated the upload, and filled the disk. Nothing about it is useful for
// debugging an agent.
func transcriptWorthRecording(sessionID string) bool {
	return sessionID != codexModelCatalogSessionID
}

func (s Server) recordHTTPMeta(r *http.Request, agentType, sessionID, userEmail string, account accounts.Account, upstream *url.URL) {
	if s.Transcripts == nil || !transcriptWorthRecording(sessionID) {
		return
	}
	metadata := map[string]any{
		"transport": "http",
		"account":   account.ID,
		"method":    r.Method,
		"path":      r.URL.Path,
		"upstream":  redactedTranscriptURL(upstream),
		"headers":   transcript.RedactedHeaders(r.Header),
	}
	if hash := userEmailHash(userEmail); hash != "" {
		metadata["user_hash"] = hash
	}
	s.Transcripts.RecordMeta(agentType, sessionID, metadata)
}

func (s Server) recordWebSocketMeta(r *http.Request, upstreamURL *url.URL, headers http.Header, agentType, sessionID, userEmail string, account accounts.Account, upstream *url.URL) {
	if s.Transcripts == nil || !transcriptWorthRecording(sessionID) {
		return
	}
	metadata := map[string]any{
		"transport":    "websocket",
		"account":      account.ID,
		"method":       r.Method,
		"path":         r.URL.Path,
		"upstream":     redactedTranscriptURL(upstream),
		"upstream_url": redactedTranscriptURL(upstreamURL),
		"headers":      transcript.RedactedHeaders(headers),
	}
	if hash := userEmailHash(userEmail); hash != "" {
		metadata["user_hash"] = hash
	}
	s.Transcripts.RecordMeta(agentType, sessionID, metadata)
}

// transcriptURL retains enough location data to identify an upstream without
// persisting credentials or opaque tokens supplied in its query string. The
// copy is deliberate: the live request keeps its query for forwarding.
func redactedTranscriptURL(upstream *url.URL) string {
	if upstream == nil {
		return ""
	}
	redacted := *upstream
	redacted.RawQuery = ""
	redacted.ForceQuery = false
	redacted.Fragment = ""
	return redacted.Redacted()
}

func (s Server) captureRequestBody(r *http.Request, agentType, sessionID string) {
	if s.Transcripts == nil || r.Body == nil || !transcriptWorthRecording(sessionID) {
		return
	}
	r.Body = newStreamingTranscriptReadCloser(streamingTranscriptConfig{
		ReadCloser: r.Body,
		Recorder:   s.Transcripts,
		AgentType:  agentType,
		SessionID:  sessionID,
		EventType:  "http_body",
		Direction:  "client_to_upstream",
		StreamID:   nextTranscriptStreamID(),
	})
}

func (s Server) recordReplayableRequestBody(r *http.Request, agentType, sessionID string) {
	if s.Transcripts == nil || r.GetBody == nil || !s.Transcripts.Enabled() || !transcriptWorthRecording(sessionID) {
		return
	}
	body, err := r.GetBody()
	if err != nil {
		return
	}
	defer body.Close()
	streamID := nextTranscriptStreamID()
	hasher := sha256.New()
	buffer := make([]byte, transcriptHTTPChunkBytes)
	var bytesRead int64
	chunks := 0
	for {
		n, readErr := body.Read(buffer)
		if n > 0 {
			chunk := buffer[:n]
			_, _ = hasher.Write(chunk)
			s.Transcripts.RecordPayloadChunk(agentType, sessionID, "http_body", "client_to_upstream", streamID, chunks, bytesRead, chunk, nil)
			bytesRead += int64(n)
			chunks++
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return
		}
	}
	s.Transcripts.RecordPayloadSummary(agentType, sessionID, "http_body", "client_to_upstream", streamID, bytesRead, hex.EncodeToString(hasher.Sum(nil)), chunks, nil)
}

// streamCancelAttribution reports which side ended a response stream. A read
// error is only meaningful once you know whether the downstream client hung up
// or the cancellation came from inside the proxy: both surface as
// "context canceled" on the upstream body, but only the second is our bug.
func streamCancelAttribution(clientCtx context.Context, err error) (string, error) {
	if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		return "upstream", nil
	}
	if clientCtx == nil {
		return "unknown", nil
	}
	if clientErr := clientCtx.Err(); clientErr != nil {
		return "client", clientErr
	}
	// The upstream read was canceled while the client was still connected, so
	// something on our side dropped a live stream.
	return "proxy", nil
}

func (s Server) captureResponseBody(response *http.Response, clientCtx context.Context, agentType, sessionID, accountID string, provider accounts.Provider, poolModel, compatibilityModel, path string) {
	s.captureResponseBodyForAccount(
		response, clientCtx, agentType, sessionID,
		s.AccountRef.credentialSnapshot(provider, accountID),
		poolModel, compatibilityModel, path,
	)
}

func (s Server) captureResponseBodyForAccount(response *http.Response, clientCtx context.Context, agentType, sessionID string, account accounts.Account, poolModel, compatibilityModel, path string) {
	accountID := account.ID
	provider := account.Provider
	if provider == "" {
		provider = accounts.ProviderCodex
	}
	_, keyedProvider := keyedProviderFor(provider)
	inspectKimiUnauthorized := s.SchedulerRef != nil && provider == accounts.ProviderKimi &&
		accountID != "" && response.StatusCode == http.StatusUnauthorized && response.Body != nil
	if keyedProvider && accountID != "" && response.StatusCode == http.StatusUnauthorized && !inspectKimiUnauthorized {
		// Non-replayable requests cannot rotate accounts safely, but the rejected
		// credential must still leave the routing pool immediately. Scope the mark
		// to the exact response credential so a concurrent repair is not poisoned.
		// Kimi also uses 401 for plan/model capability errors, so its body must be
		// inspected before deciding whether the credential itself is bad.
		s.markAccountExhaustedCredentialForAccount(account)
	}
	// Anthropic signals subscription exhaustion with a plain 429 and a dead or
	// expired OAuth token with a plain 401, neither with a codex-style
	// usage-limit body to inspect. Both mean this account can't serve the
	// request, so drop it from selection and let failover pick another.
	// A 429/401 is unusable by status; a 200 carrying
	// anthropic-ratelimit-unified-status=rejected is also unusable, because the
	// account is depleted (served via overage) and Claude Code hard-blocks the
	// user on that header even though the request "succeeded".
	claudeUnusable := provider == accounts.ProviderClaude && accountID != "" &&
		(claudeAccountUnusableStatus(response.StatusCode) || claudeResponseRejected(response.Header))
	if claudeUnusable {
		// Only poison the routing score when the account is genuinely out of
		// quota (401, a 429 the upstream marks "rejected", or any response with
		// the rejected header). A transient "allowed"/"allowed_warning" 429 still
		// fails over for this request but must not mark a healthy account exhausted.
		if claudeAccountExhaustedByResponse(response.StatusCode, response.Header) {
			s.markAccountExhaustedFromResponseForAccount(account, poolModel, response.StatusCode, response.Header)
		}
		// Surface the genuine upstream rate-limit signal (headers now, body
		// prefix below). Anthropic conveys subscription exhaustion only via the
		// status plus these headers and an opaque JSON body, none of which were
		// logged before, so the actual 429 message was invisible in the wild.
		if s.Logger != nil {
			s.Logger.Warn("claude account unusable upstream response",
				append([]any{
					"agent", agentType,
					"session", sessionID,
					"account", accountID,
					"path", path,
					"status", response.StatusCode,
				}, claudeRateLimitHeaderFields(response.Header)...)...)
		}
	}
	// Upstream server errors (529 overloaded_error, 5xx) are an Anthropic-side
	// capacity problem, not account-specific, so subrouter passes them through
	// (the client retries with backoff and eventually surfaces them). They were
	// invisible before; log them so overload periods are observable. Do NOT mark
	// the account exhausted or fail over: rerouting cannot fix an API-wide
	// capacity issue and just amplifies load on the overloaded endpoint.
	if provider == accounts.ProviderClaude && response.StatusCode >= 500 && s.Logger != nil {
		s.Logger.Warn("claude upstream server error",
			append([]any{
				"agent", agentType,
				"session", sessionID,
				"account", accountID,
				"path", path,
				"status", response.StatusCode,
			}, claudeRateLimitHeaderFields(response.Header)...)...)
	}
	inspectUsageLimit := s.SchedulerRef != nil && accountID != "" && responseStatusCanExhaust(response.StatusCode)
	inspectCredentialFailure := s.SchedulerRef != nil && keyedProvider && accountID != "" &&
		(response.StatusCode == http.StatusBadRequest || response.StatusCode == http.StatusForbidden)
	inspectModelCompatibility := s.SchedulerRef != nil && accountID != "" && compatibilityModel != "" &&
		provider == accounts.ProviderCodex && response.StatusCode == http.StatusBadRequest
	if response.Body == nil || (s.Transcripts == nil && s.Logger == nil && !inspectUsageLimit && !inspectCredentialFailure && !inspectModelCompatibility && !inspectKimiUnauthorized && !claudeUnusable) {
		return
	}
	payload := map[string]any{"status": response.StatusCode}
	responseCtx := context.Background()
	if response.Request != nil {
		responseCtx = response.Request.Context()
	}
	var inspect func([]byte)
	if inspectUsageLimit || inspectCredentialFailure || inspectModelCompatibility || inspectKimiUnauthorized || claudeUnusable {
		loggedBody := false
		inspect = func(body []byte) {
			if inspectKimiUnauthorized {
				if kimiModelCapabilityErrorJSON(body) {
					if compatibilityModel != "" {
						if err := s.rerouteModelIncompatibilityForReconnect(responseCtx, provider, agentType, sessionID, "", accountID, compatibilityModel); err != nil && s.Logger != nil {
							s.Logger.Error("http model reroute could not persist next account", "agent", agentType, "session", sessionID, "account", accountID, "model", compatibilityModel, "error", err)
						}
					}
				} else {
					s.markAccountExhaustedCredentialForAccount(account)
				}
			}
			if inspectCredentialFailure && credentialUnauthorizedJSON(body) {
				s.markAccountExhaustedCredentialForAccount(account)
			}
			if inspectUsageLimit && usageLimitJSON(body) {
				// Use the response's headers so a header-derived reset expiry set
				// above is recomputed identically, not overwritten with the short
				// default TTL.
				s.markAccountExhaustedFromResponseForAccount(account, poolModel, response.StatusCode, response.Header)
			}
			if inspectModelCompatibility && codexChatGPTModelUnsupportedJSON(body) {
				if err := s.rerouteModelIncompatibilityForReconnect(responseCtx, provider, agentType, sessionID, "", accountID, compatibilityModel); err != nil && s.Logger != nil {
					s.Logger.Error("http model reroute could not persist next account", "agent", agentType, "session", sessionID, "account", accountID, "model", compatibilityModel, "error", err)
				}
			}
			// Only log the body for the original hard rate-limit statuses
			// (429/401), whose body is a known rate-limit/auth error envelope. A
			// rejected-header response on any other status (a 200 overage
			// completion, or an unrelated 4xx/5xx) must not have its body logged.
			if claudeUnusable && claudeAccountUnusableStatus(response.StatusCode) && !loggedBody && s.Logger != nil {
				loggedBody = true
				s.Logger.Warn("claude account unusable upstream body",
					"agent", agentType,
					"session", sessionID,
					"account", accountID,
					"path", path,
					"status", response.StatusCode,
					"body", string(body))
			}
		}
	}
	streamStarted := time.Now()
	// The catalog session still needs its body inspected for quota signals, so
	// it reaches here; it just must not be written down.
	bodyRecorder := s.Transcripts
	if !transcriptWorthRecording(sessionID) {
		bodyRecorder = nil
	}
	response.Body = newStreamingTranscriptReadCloser(streamingTranscriptConfig{
		ReadCloser: response.Body,
		Recorder:   bodyRecorder,
		AgentType:  agentType,
		SessionID:  sessionID,
		EventType:  "http_body",
		Direction:  "upstream_to_client",
		StreamID:   nextTranscriptStreamID(),
		Payload:    payload,
		InspectMax: usageLimitInspectMaxBytes,
		OnInspect:  inspect,
		onReadError: func(err error, bytesRead int) {
			canceledBy, clientErr := streamCancelAttribution(clientCtx, err)
			s.StreamDrops.Observe(canceledBy, time.Now())
			if s.Logger == nil {
				return
			}
			// A client hanging up and retrying is expected and happens ~1000
			// times a day; it is counted, not written, so the log stays
			// bounded. Everything else is rare and actionable, so it keeps a
			// full line.
			if canceledBy == "client" {
				s.Logger.Debug("proxy response stream ended by client", "agent", agentType, "session", sessionID, "account", accountID, "path", path, "bytes", bytesRead, "stream_age_ms", time.Since(streamStarted).Milliseconds())
				return
			}
			s.Logger.Error("proxy response stream read failed", "agent", agentType, "session", sessionID, "account", accountID, "path", path, "status", response.StatusCode, "bytes", bytesRead, "error", err, "canceled_by", canceledBy, "client_ctx_err", clientErr, "stream_age_ms", time.Since(streamStarted).Milliseconds())
		},
	})
}

func responseStatusCanExhaust(status int) bool {
	return status >= 400 && status < 500
}

const transcriptHTTPChunkBytes = 64 * 1024
const usageLimitInspectMaxBytes = 1 << 20

var transcriptStreamCounter atomic.Uint64

func nextTranscriptStreamID() string {
	return fmt.Sprintf("body-%d", transcriptStreamCounter.Add(1))
}

type streamingTranscriptConfig struct {
	ReadCloser  io.ReadCloser
	Recorder    *transcript.Recorder
	AgentType   string
	SessionID   string
	EventType   string
	Direction   string
	StreamID    string
	Payload     map[string]any
	InspectMax  int64
	OnInspect   func([]byte)
	onReadError func(error, int)
}

func newStreamingTranscriptReadCloser(config streamingTranscriptConfig) io.ReadCloser {
	return &streamingTranscriptReadCloser{
		ReadCloser:  config.ReadCloser,
		recorder:    config.Recorder,
		agentType:   config.AgentType,
		sessionID:   config.SessionID,
		eventType:   config.EventType,
		direction:   config.Direction,
		streamID:    config.StreamID,
		payload:     config.Payload,
		inspectMax:  config.InspectMax,
		onInspect:   config.OnInspect,
		onReadError: config.onReadError,
		hasher:      sha256.New(),
	}
}

type streamingTranscriptReadCloser struct {
	io.ReadCloser
	recorder    *transcript.Recorder
	agentType   string
	sessionID   string
	eventType   string
	direction   string
	streamID    string
	payload     map[string]any
	inspect     []byte
	inspectMax  int64
	onInspect   func([]byte)
	hasher      hash.Hash
	bytesRead   int
	chunks      int
	onReadError func(error, int)
	closeOnce   sync.Once
	readErrOnce sync.Once
}

func (r *streamingTranscriptReadCloser) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)
	if n > 0 {
		r.bytesRead += n
	}
	if n > 0 {
		r.recordChunk(p[:n])
	}
	if err != nil && err != io.EOF && r.onReadError != nil {
		r.readErrOnce.Do(func() {
			r.onReadError(err, r.bytesRead)
		})
	}
	return n, err
}

func (r *streamingTranscriptReadCloser) recordChunk(body []byte) {
	_, _ = r.hasher.Write(body)
	r.captureInspectBytes(body)
	if r.recorder == nil || !r.recorder.Enabled() {
		return
	}
	offset := int64(r.bytesRead - len(body))
	for len(body) > 0 {
		chunk := body
		if len(chunk) > transcriptHTTPChunkBytes {
			chunk = body[:transcriptHTTPChunkBytes]
		}
		r.recorder.RecordPayloadChunk(r.agentType, r.sessionID, r.eventType, r.direction, r.streamID, r.chunks, offset, chunk, r.payload)
		r.chunks++
		offset += int64(len(chunk))
		body = body[len(chunk):]
	}
}

func (r *streamingTranscriptReadCloser) captureInspectBytes(body []byte) {
	if r.onInspect == nil || r.inspectMax <= 0 || int64(len(r.inspect)) >= r.inspectMax {
		return
	}
	remaining := int(r.inspectMax) - len(r.inspect)
	if remaining > len(body) {
		remaining = len(body)
	}
	r.inspect = append(r.inspect, body[:remaining]...)
}

func (r *streamingTranscriptReadCloser) Close() error {
	err := r.ReadCloser.Close()
	r.closeOnce.Do(func() {
		sum := hex.EncodeToString(r.hasher.Sum(nil))
		if r.recorder != nil && r.recorder.Enabled() {
			r.recorder.RecordPayloadSummary(r.agentType, r.sessionID, r.eventType, r.direction, r.streamID, int64(r.bytesRead), sum, r.chunks, r.payload)
		}
		if r.onInspect != nil {
			r.onInspect(r.inspect)
		}
	})
	return err
}

type proxyErrorWriter struct {
	logger   *slog.Logger
	agent    string
	session  string
	account  string
	method   string
	path     string
	upstream string
}

func (w proxyErrorWriter) Write(p []byte) (int, error) {
	message := strings.TrimSpace(string(p))
	if message != "" && w.logger != nil {
		w.logger.Error("reverse proxy error", "agent", w.agent, "session", w.session, "account", w.account, "method", w.method, "path", w.path, "upstream", w.upstream, "message", message)
	}
	return len(p), nil
}

const claudeOAuthBetaHeader = "oauth-2025-04-20"

func setAccountAuthHeaders(headers http.Header, account accounts.Account, model string) {
	headers.Set("Authorization", account.AuthorizationHeader())
	headers.Del("ChatGPT-Account-ID")
	if entry, ok := keyedProviderFor(account.Provider); ok {
		applyKeyedProviderAuth(headers, account, entry, model)
		return
	}
	switch account.Provider {
	case accounts.ProviderClaude:
		if account.AuthMode == accounts.AuthModeAPIKey {
			headers.Del("Authorization")
			headers.Set("X-Api-Key", account.Token)
			removeCommaHeaderValue(headers, "Anthropic-Beta", claudeOAuthBetaHeader)
			return
		}
		headers.Del("X-Api-Key")
		ensureCommaHeaderValue(headers, "Anthropic-Beta", claudeOAuthBetaHeader)
	case accounts.ProviderCodex, "":
		if account.AccountID != "" {
			headers.Set("ChatGPT-Account-ID", account.AccountID)
		}
	}
}

func removeCommaHeaderValue(headers http.Header, key, value string) {
	existing := headers.Get(key)
	if existing == "" {
		return
	}
	parts := strings.Split(existing, ",")
	kept := parts[:0]
	for _, part := range parts {
		trimmed := strings.TrimSpace(part)
		if trimmed == "" || trimmed == value {
			continue
		}
		kept = append(kept, trimmed)
	}
	if len(kept) == 0 {
		headers.Del(key)
		return
	}
	headers.Set(key, strings.Join(kept, ","))
}

func ensureCommaHeaderValue(headers http.Header, key, value string) {
	existing := headers.Get(key)
	if existing == "" {
		headers.Set(key, value)
		return
	}
	for _, part := range strings.Split(existing, ",") {
		if strings.TrimSpace(part) == value {
			return
		}
	}
	headers.Set(key, existing+","+value)
}

func (s Server) upstreamForRequest(path string, account accounts.Account) *url.URL {
	if s.Upstream != nil {
		return s.Upstream
	}
	if account.Provider == accounts.ProviderClaude {
		return s.ClaudeUpstream
	}
	if entry, ok := keyedProviderFor(account.Provider); ok {
		return entry.Upstream(s, account.AuthMode)
	}
	if account.Provider == accounts.ProviderAntigravity {
		return s.AntigravityUpstream
	}
	if account.AuthMode == accounts.AuthModeAPIKey {
		return s.APIUpstream
	}
	if chatGPTBackendPath(path) {
		return chatGPTBackendUpstream(s.CodexUpstream)
	}
	return s.CodexUpstream
}

func (s Server) pathForUpstream(path string, account accounts.Account) string {
	if s.Upstream != nil {
		return path
	}
	if path == "" {
		path = "/"
	}
	if account.Provider == accounts.ProviderClaude {
		return path
	}
	if entry, ok := keyedProviderFor(account.Provider); ok {
		path = stripProviderPathPrefix(path, entry.PathPrefix)
		if entry.CollapseVersionSegment {
			return collapseDuplicateVersionSegment(path, entry.Upstream(s, account.AuthMode))
		}
		return path
	}
	if account.Provider == accounts.ProviderAntigravity {
		// The CLI sends the API version itself (v1internal:method), so only
		// the provider prefix is stripped.
		return stripProviderPathPrefix(path, "antigravity")
	}
	if account.AuthMode == accounts.AuthModeOAuth {
		if stripped, ok := stripChatGPTBackendPath(path); ok {
			return stripped
		}
	}
	if account.AuthMode == accounts.AuthModeAPIKey {
		if path == "/v1" || strings.HasPrefix(path, "/v1/") {
			return path
		}
		return "/v1" + path
	}
	if path == "/v1" {
		return "/"
	}
	if strings.HasPrefix(path, "/v1/") {
		return strings.TrimPrefix(path, "/v1")
	}
	return path
}

func chatGPTBackendUpstream(codexUpstream *url.URL) *url.URL {
	upstream := cloneURL(codexUpstream)
	if upstream == nil {
		return nil
	}
	path := strings.TrimRight(upstream.Path, "/")
	if strings.HasSuffix(path, "/backend-api/codex") {
		upstream.Path = strings.TrimSuffix(path, "/codex")
	}
	return upstream
}

func stripChatGPTBackendPath(path string) (string, bool) {
	const prefix = "/backend-api"
	if path == prefix {
		return "/", true
	}
	if strings.HasPrefix(path, prefix+"/") {
		return strings.TrimPrefix(path, prefix), true
	}
	return path, false
}

func cloneURL(value *url.URL) *url.URL {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func (s Server) accountFor(agentType string, r *http.Request) (accounts.Account, string, string, error) {
	return s.accountForSession(agentType, session.ExtractID(r, s.MaxBodyBytes), r)
}

func (s Server) accountForSession(agentType, sessionID string, r *http.Request) (accounts.Account, string, string, error) {
	return s.accountForSessionProvider(providerForRequest(agentType, r.URL.Path), agentType, sessionID, r)
}

func (s Server) accountForSessionProvider(provider accounts.Provider, agentType, sessionID string, r *http.Request) (accounts.Account, string, string, error) {
	return s.accountForSessionProviderWithOptions(provider, agentType, sessionID, r, accountSelectionOptions{})
}

type accountSelectionOptions struct {
	allowFableAPIKeyPool bool
	ignoreForcedAccount  bool
	oauthOnly            bool
	preferredAccountID   string
	// pendingSessionCommit is set when an existing sticky assignment is
	// provisionally rerouted. Request-serving callers commit it only after the
	// replacement account succeeds; nil preserves eager assignment for direct
	// selection callers that have no upstream success boundary.
	pendingSessionCommit *bool
}

func (s Server) accountForSessionProviderWithOptions(provider accounts.Provider, agentType, sessionID string, r *http.Request, options accountSelectionOptions) (accounts.Account, string, string, error) {
	userEmail := session.ExtractUserEmail(r)
	forcedAccountID := ""
	if !options.ignoreForcedAccount {
		var err error
		forcedAccountID, _, err = session.ExtractAccountIDWithPresence(r)
		if err != nil {
			return accounts.Account{}, sessionID, userEmail, fmt.Errorf("invalid forced account selector: %w", err)
		}
	}
	model := session.ExtractModel(r, s.MaxBodyBytes)
	availableAccounts := filterAccountsForProvider(s.accountListContext(r.Context()), provider)
	if options.oauthOnly {
		availableAccounts = oauthAccounts(availableAccounts)
	}
	// The upstream prompt cache is per account, so moving a session to another
	// account re-bills its whole conversation prefix as uncached input. Record
	// where the session was before any branch can reassign it.
	previousAccountID := ""
	if assignment, ok := s.Sessions.Get(agentType, sessionID); ok {
		previousAccountID = assignment.AccountID
	}
	if forcedAccountID != "" {
		account, ok := findAccount(availableAccounts, forcedAccountID)
		if !ok {
			return accounts.Account{}, sessionID, userEmail, fmt.Errorf("requested account %q not found", forcedAccountID)
		}
		if provider == accounts.ProviderCodex && chatGPTBackendPath(r.URL.Path) && account.AuthMode != accounts.AuthModeOAuth {
			return accounts.Account{}, sessionID, userEmail, fmt.Errorf("requested account %q cannot be used for ChatGPT backend paths", forcedAccountID)
		}
		s.logAccountMove(agentType, sessionID, model, previousAccountID, account.ID, provider, nil)
		assignment, err := s.Sessions.Put(agentType, sessionID, account.ID, userEmail)
		if err != nil {
			return accounts.Account{}, sessionID, userEmail, err
		}
		return account, sessionID, assignment.UserEmail, nil
	}
	if provider == accounts.ProviderCodex && chatGPTBackendPath(r.URL.Path) {
		availableAccounts = oauthAccounts(availableAccounts)
	}
	if provider == accounts.ProviderClaude && !options.allowFableAPIKeyPool && s.claudeFableEnabled() && claudeFableModel(model) {
		// Fable order is subscription pool -> Bedrock -> dedicated API key, so
		// metered API-key pool accounts never preempt the Bedrock stage. With no
		// OAuth account usable, selection fails and the handler serves the
		// fallback chain directly.
		availableAccounts = oauthAccounts(availableAccounts)
	}
	if provider == accounts.ProviderCodex || provider == accounts.ProviderClaude || provider == accounts.ProviderKimi || provider == accounts.ProviderAntigravity {
		s.refreshUsageScoresIfStale(r.Context())
	}
	base := s.scheduler()
	poolModel := model
	if provider == accounts.ProviderClaude {
		poolModel = claudePoolModel(model)
	} else if provider == accounts.ProviderAntigravity {
		poolModel = antigravityPoolModel(base, model)
	}
	if poolModel != "" && s.Logger != nil && base.HasModelPool(poolModel) {
		s.Logger.Info("model quota pool matched", "agent", agentType, "model", model, "pool", selectacct.ModelKey(poolModel))
	}
	scheduler := base.ForModel(poolModel).WithSessionCounts(SchedulerSessionCounts(s.Sessions))
	// picked carries a placement decided inside the sticky branch (the
	// constrained account's replacement) into the shared assignment tail, so
	// the account that was judged materially better is the one assigned.
	var picked *accounts.Account
	if assignment, ok := s.Sessions.Get(agentType, sessionID); ok {
		if userEmail != "" && userEmail != assignment.UserEmail {
			updated, err := s.Sessions.Put(agentType, sessionID, assignment.AccountID, userEmail)
			if err != nil {
				return accounts.Account{}, sessionID, userEmail, err
			}
			assignment = updated
		}
		if userEmail == "" {
			userEmail = assignment.UserEmail
		}
		if account, ok := findAccount(availableAccounts, assignment.AccountID); ok {
			if s.reuseStickyAssignment(agentType, sessionID, account, scheduler) {
				s.logStickyReuse(agentType, sessionID, account, scheduler)
				s.touchSessionBestEffort(agentType, sessionID)
				return account, sessionID, userEmail, nil
			}
			candidate, pickErr := pickRoutingAccount(scheduler, availableAccounts)
			if pickErr != nil || s.keepConstrainedStickyAssignment(scheduler, account, candidate) {
				if s.Logger != nil {
					s.Logger.Info("keeping sticky session on constrained account; no materially better account",
						"agent", agentType,
						"session", sessionID,
						"account", account.ID,
						"candidate", candidate.ID,
						"active", s.activeSession(agentType, sessionID),
						"exhausted", scheduler.Exhausted(schedulerAccountProvider(account.Provider), account.ID),
					)
				}
				s.touchSessionBestEffort(agentType, sessionID)
				return account, sessionID, userEmail, nil
			}
			if candidate.AuthMode == accounts.AuthModeOAuth && provider == accounts.ProviderClaude && scheduler.Exhausted(schedulerAccountProvider(candidate.Provider), candidate.ID) {
				// The whole pool is exhausted: Pick ranks exhausted accounts
				// last but still returns one, and the post-selection check
				// below rejects it before the assignment is ever persisted.
				// Reaching that check through this branch used to log a
				// "rerouting" to an unusable account that never happened,
				// once per request for as long as the pool stayed exhausted.
				// Fail the selection here so the handler goes straight to the
				// fallback chain.
				return accounts.Account{}, sessionID, userEmail, fmt.Errorf("no non-exhausted %s accounts available", provider)
			}
			if s.Logger != nil {
				s.Logger.Info("rerouting cold sticky session from constrained account",
					"agent", agentType,
					"session", sessionID,
					"account", account.ID,
					"to_account", candidate.ID,
					"active", s.activeSession(agentType, sessionID),
					"usable_for_new_session", scheduler.UsableForNewSession(schedulerAccountProvider(account.Provider), account.ID),
					"usable_for_sticky_session", scheduler.UsableForStickySession(schedulerAccountProvider(account.Provider), account.ID),
					"exhausted", scheduler.Exhausted(schedulerAccountProvider(account.Provider), account.ID),
				)
			}
			picked = &candidate
		}
	}

	var account accounts.Account
	if picked == nil && options.preferredAccountID != "" {
		preferred, ok := findAccount(availableAccounts, options.preferredAccountID)
		// A preference is intentionally softer than a pin. If the selected
		// account disappeared after the picker ran or is already known exhausted,
		// start on the scheduler's current recommendation instead of failing the
		// pooled launch before a request.
		if ok && !scheduler.Exhausted(schedulerAccountProvider(preferred.Provider), preferred.ID) {
			picked = &preferred
		}
	}
	if picked != nil {
		account = *picked
	} else {
		var err error
		account, err = pickRoutingAccount(scheduler, availableAccounts)
		if err != nil {
			return accounts.Account{}, sessionID, userEmail, err
		}
	}
	if account.AuthMode == accounts.AuthModeOAuth && provider == accounts.ProviderClaude && scheduler.Exhausted(schedulerAccountProvider(account.Provider), account.ID) {
		return accounts.Account{}, sessionID, userEmail, fmt.Errorf("no non-exhausted %s accounts available", provider)
	}
	if account.AuthMode == accounts.AuthModeOAuth && !scheduler.UsableForNewSession(schedulerAccountProvider(account.Provider), account.ID) && s.Logger != nil {
		// Never refuse here based on the scheduler's view. Usage scores can be
		// stale: the per-request re-score reads usage through a cache that falls
		// back to stale "last good" data when the upstream usage endpoint
		// rate-limits, which made healthy accounts look exhausted and produced
		// bogus "no usable accounts" 503s while real quota existed. The
		// scheduler is a load-balancing hint, not a hard gate. Route the best
		// account and let the real upstream response drive per-request
		// usage-limit failover; a genuinely exhausted account surfaces the
		// upstream's own 429 instead of a premature rejection.
		s.Logger.Warn("selected OAuth account below new-session headroom; routing optimistically",
			"provider", provider,
			"account", account.ID,
			"exhausted", scheduler.Exhausted(schedulerAccountProvider(account.Provider), account.ID),
			"threshold", selectacct.MinNewSessionHeadroom)
	}
	if options.pendingSessionCommit != nil && previousAccountID != "" && previousAccountID != account.ID {
		*options.pendingSessionCommit = true
		return account, sessionID, userEmail, nil
	}
	s.logAccountMove(agentType, sessionID, model, previousAccountID, account.ID, provider, &scheduler)
	assignment, err := s.Sessions.Put(agentType, sessionID, account.ID, userEmail)
	if err != nil {
		return accounts.Account{}, sessionID, userEmail, err
	}
	return account, sessionID, assignment.UserEmail, nil
}

// logAccountMove records that a session left the account holding its upstream
// prompt cache. scheduler is nil when the caller forced the account and no
// routing scores were consulted.
func (s Server) logAccountMove(agentType, sessionID, model, fromAccountID, toAccountID string, provider accounts.Provider, scheduler *selectacct.Scheduler) {
	if s.Logger == nil || fromAccountID == "" || fromAccountID == toAccountID {
		return
	}
	fields := []any{
		"agent", agentType,
		"session", sessionID,
		"model", model,
		"from_account", fromAccountID,
		"to_account", toAccountID,
	}
	if scheduler == nil {
		fields = append(fields, "forced", true)
	} else {
		fields = append(fields,
			"from_exhausted", scheduler.Exhausted(schedulerAccountProvider(provider), fromAccountID),
			"from_usable_for_sticky_session", scheduler.UsableForStickySession(schedulerAccountProvider(provider), fromAccountID),
			"retention_threshold", selectacct.MinStickyRetentionHeadroom,
		)
	}
	s.Logger.Warn("session moved to another account; upstream prompt cache is cold", fields...)
}

func (s Server) logStickyReuse(agentType, sessionID string, account accounts.Account, scheduler selectacct.Scheduler) {
	if s.Logger == nil || accountProviderOrCodex(account) != accounts.ProviderCodex || account.AuthMode != accounts.AuthModeOAuth {
		return
	}
	if scheduler.UsableForNewSession(schedulerAccountProvider(account.Provider), account.ID) || scheduler.Exhausted(schedulerAccountProvider(account.Provider), account.ID) || !s.activeSession(agentType, sessionID) {
		return
	}
	s.Logger.Info("keeping active sticky session on constrained account",
		"agent", agentType,
		"session", sessionID,
		"account", account.ID,
		"usable_for_new_session", false,
		"exhausted", false,
	)
}

func (s Server) touchSessionBestEffort(agentType, sessionID string) {
	if s.Sessions == nil {
		return
	}
	if _, _, err := s.Sessions.Touch(agentType, sessionID); err != nil && s.Logger != nil {
		s.Logger.Warn("session activity update failed; continuing with sticky assignment",
			"agent", agentType,
			"session", sessionID,
			"error", err,
		)
	}
}

// providerSelectionPolicy states, per provider, how session routing treats
// the account that already holds a session's upstream prompt cache. Add one
// entry when onboarding a provider; every knob defaults to false, which is
// the safest choice for a provider whose cache and quota economics are
// unknown: sessions stay on their account unconditionally short of
// exhaustion, and exhaustion-driven moves keep their existing semantics.
// Placement spreading needs no entry here at all — Scheduler.Pick spreads
// within equal-pressure bands for every provider, and the provider's own
// usage windows (whether they carry reset-time pressure) decide how wide a
// band gets.
type providerSelectionPolicy struct {
	// retentionFloorGated: an idle session leaves its account once the
	// account's measured headroom falls below MinStickyRetentionHeadroom.
	// Codex sets this (#216): its per-account prompt cache makes moving
	// expensive, but holding a session on a truly empty account is worse.
	retentionFloorGated bool
	// constrainedMoveGated: when a session must consider leaving, the move
	// happens only for a materially better target (see
	// keepConstrainedStickyAssignment). Codex sets this to stop
	// constrained-pool ping-pong. Claude must NOT set it: a Claude session
	// only reaches the constrained branch when its account is exhausted, and
	// for fable an exhausted pool has to surface a selection failure so the
	// Bedrock fallback chain runs instead of a keep.
	constrainedMoveGated bool
}

var providerSelectionPolicies = map[accounts.Provider]providerSelectionPolicy{
	accounts.ProviderCodex:  {retentionFloorGated: true, constrainedMoveGated: true},
	accounts.ProviderClaude: {},
}

func selectionPolicyFor(account accounts.Account) providerSelectionPolicy {
	return providerSelectionPolicies[accountProviderOrCodex(account)]
}

func (s Server) reuseStickyAssignment(agentType, sessionID string, account accounts.Account, scheduler selectacct.Scheduler) bool {
	if scheduler.Exhausted(schedulerAccountProvider(account.Provider), account.ID) {
		return false
	}
	if s.activeSession(agentType, sessionID) {
		return true
	}
	if selectionPolicyFor(account).retentionFloorGated && account.AuthMode == accounts.AuthModeOAuth {
		// Retention, not placement: this session already built the upstream
		// prompt cache on this account. Gating it on the new-session threshold
		// moved every idle session off any account past 60% used, which in a
		// busy pool is all of them, so stickiness stopped existing exactly
		// when the pool could least afford re-billing whole prefixes.
		return scheduler.UsableForStickySession(schedulerAccountProvider(account.Provider), account.ID)
	}
	return true
}

// minStickyMoveHeadroomGain is how much more measured headroom a destination
// account must have before a constrained sticky session moves there. Moving
// drops the session's per-account upstream prompt cache and re-bills the
// whole conversation prefix as uncached input, so trading a nearly-empty
// account for an almost-as-empty one is pure loss.
const minStickyMoveHeadroomGain = 0.10

// keepConstrainedStickyAssignment reports whether a Codex session whose
// account fell below the sticky-retention floor should stay there anyway
// because the pool offers nothing materially better. Overnight before
// 2026-08-18 every account sat below the retention floor, so every request
// rerouted its session to an equally-drained account and Pick shuffled the
// destinations: 26,165 moves in one day, peaking at 8,043 in one hour, each
// one re-billing a cached prefix. A move is worth that price only when the
// destination could itself retain the session (at or above the retention
// floor) AND offers materially more room than the current account; an
// exhausted account is different — anything non-exhausted beats it. The
// retention floor itself is unchanged.
//
// The headroom comparison reads measured scores (ScoreFor), not live-debited
// ones: the debit floors a busy account's headroom at 0.01, which would veto
// every destination under load even when it has real room. Non-Codex
// providers keep their existing move-on-constraint behaviour, including the
// Claude fable path whose selection failure triggers the Bedrock fallback.
func (s Server) keepConstrainedStickyAssignment(scheduler selectacct.Scheduler, current, picked accounts.Account) bool {
	if !selectionPolicyFor(current).constrainedMoveGated || current.AuthMode != accounts.AuthModeOAuth {
		return false
	}
	if picked.ID == current.ID {
		return true
	}
	if scheduler.Exhausted(schedulerAccountProvider(current.Provider), current.ID) {
		return scheduler.Exhausted(schedulerAccountProvider(picked.Provider), picked.ID)
	}
	currentScore := scheduler.ScoreFor(schedulerAccountProvider(current.Provider), current.ID)
	pickedScore := scheduler.ScoreFor(schedulerAccountProvider(picked.Provider), picked.ID)
	currentMin := math.Min(currentScore.Headroom, currentScore.ShortHeadroom)
	pickedMin := math.Min(pickedScore.Headroom, pickedScore.ShortHeadroom)
	if pickedMin < selectacct.MinStickyRetentionHeadroom {
		return true
	}
	return pickedMin < currentMin+minStickyMoveHeadroomGain
}

func (s Server) activeSession(agentType, sessionID string) bool {
	return s.ActiveSessions != nil && s.ActiveSessions.Active(agentType, sessionID)
}

func (s Server) allowDrainingProxyRequest(agentType, sessionID string) bool {
	if s.activeSession(agentType, sessionID) {
		return true
	}
	if s.Sessions == nil {
		return false
	}
	_, ok := s.Sessions.Get(agentType, sessionID)
	return ok
}

func activeSessionRequest(agentType string, r *http.Request) bool {
	if r == nil {
		return false
	}
	if websocket.IsWebSocketUpgrade(r) {
		return true
	}
	if session.NormalizeAgentType(agentType) != "codex" || r.Method != http.MethodPost {
		return false
	}
	return codexResponsePath(r.URL.Path)
}

func codexResponsePath(path string) bool {
	switch path {
	case "/responses", "/v1/responses", "/responses/compact", "/v1/responses/compact",
		"/backend-api/codex/responses", "/backend-api/codex/responses/compact":
		return true
	default:
		return false
	}
}

// refreshUsageScoresIfStale rebuilds the scheduler from every OAuth account's
// usage, across all providers. Scoring the full list (not just the requesting
// provider's accounts) matters because FinishRefresh replaces the scheduler
// wholesale: a codex-triggered refresh must not wipe claude scores or vice
// versa.
func (s Server) refreshUsageScoresIfStale(ctx context.Context) {
	// See reloadAccounts: in team mode refreshing local OAuth accounts rotates
	// refresh tokens the vault owns, which invalidates them for both sides.
	if s.CredentialBroker != nil {
		return
	}
	if s.SchedulerRef == nil {
		return
	}
	allAccounts, accountGeneration := s.accountListSnapshotContext(ctx)
	if !s.SchedulerRef.BeginRefreshIfStaleForAccountGeneration(s.UsageScoreTTL, accountGeneration) {
		return
	}
	// The Begin claim set refreshing=true, and nothing else can clear it: a
	// panic anywhere below (swallowed by net/http's per-request recover)
	// would leave every future BeginRefresh returning false, freezing usage
	// scores for the life of the process. Release the claim on the way out
	// if no Finish call has.
	finished := false
	defer func() {
		if !finished {
			s.SchedulerRef.FinishRefreshForAccountGeneration(selectacct.Scheduler{}, false, accountGeneration)
		}
	}()
	availableAccounts := oauthAccounts(allAccounts)
	scoreAccounts := s.ScoreAccounts
	if scoreAccounts == nil {
		scoreAccounts = s.scoreAccounts
	}
	// The refresh runs on whichever request happened to find the scores stale.
	// Its context must not be that request's: a client that disconnects or
	// times out mid-refresh cancels every remaining per-account refresh and
	// usage fetch with "context canceled", and FinishRefresh still stamps the
	// TTL window, so one impatient client starves the whole pool of fresh
	// scores for another full TTL — repeatedly, under load, which pinned
	// exhausted accounts as exhausted long after their windows reset. Detach
	// from the caller's cancellation and bound the refresh on its own clock.
	scoreCtx, cancelScore := context.WithTimeout(context.WithoutCancel(ctx), usageScoreRefreshTimeout)
	defer cancelScore()
	scores, scored := scoreAccounts(scoreCtx, availableAccounts)
	if scored == 0 {
		s.SchedulerRef.FinishRefreshForAccountGeneration(selectacct.Scheduler{}, false, accountGeneration)
		finished = true
		if s.Logger != nil {
			s.Logger.Warn("usage score refresh skipped", "reason", "no fresh OAuth usage scores")
		}
		return
	}
	scheduler := selectacct.NewScheduler(scores)
	if s.Sessions != nil {
		scheduler = scheduler.WithSessionCounts(SchedulerSessionCounts(s.Sessions))
	}
	finished = true
	if !s.SchedulerRef.FinishRefreshForAccountGeneration(scheduler, true, accountGeneration) {
		if s.Logger != nil {
			s.Logger.Debug("usage score refresh discarded after account reload")
		}
		return
	}
	if s.Logger != nil {
		s.Logger.Debug("usage scores refreshed before account selection", "accounts", len(availableAccounts), "scored", scored)
	}
}

func chatGPTBackendPath(path string) bool {
	_, ok := stripChatGPTBackendPath(path)
	return ok
}

const codexModelCatalogSessionID = "internal:codex-model-catalog"

// Codex asks its provider for richer runtime metadata using client_version.
// The OpenAI API-key /models response has a different schema, so this request
// must use the ChatGPT OAuth upstream even when response traffic is pinned to
// an API-key account.
func codexModelCatalogRequest(r *http.Request) bool {
	if r == nil || r.Method != http.MethodGet || strings.TrimSpace(r.URL.Query().Get("client_version")) == "" {
		return false
	}
	return r.URL.Path == "/models" || r.URL.Path == "/v1/models"
}

func codexModelCatalogRoutingRequest(r *http.Request) *http.Request {
	request := r.Clone(r.Context())
	request.Header = r.Header.Clone()
	for _, header := range []string{
		"X-Subrouter-Account-ID",
		"X-Subrouter-Account",
		"X-Subrouter-Model",
		"X-Model",
	} {
		request.Header.Del(header)
	}
	return request
}

func oauthAccounts(all []accounts.Account) []accounts.Account {
	filtered := make([]accounts.Account, 0, len(all))
	for _, account := range all {
		if account.AuthMode == accounts.AuthModeOAuth {
			filtered = append(filtered, account)
		}
	}
	return filtered
}

func providerForAgent(agentType string) accounts.Provider {
	switch session.NormalizeAgentType(agentType) {
	case "claude":
		return accounts.ProviderClaude
	default:
		return accounts.ProviderCodex
	}
}

func providerForRequest(agentType, path string) accounts.Provider {
	if provider, ok := providerForPath(path); ok {
		return provider
	}
	return providerForAgent(agentType)
}

func agentTypeForProviderSession(agentType string, provider accounts.Provider) string {
	if isKeyedProvider(provider) || provider == accounts.ProviderAntigravity {
		return string(provider)
	}
	return agentType
}

func providerForPath(path string) (accounts.Provider, bool) {
	// Antigravity is OAuth-only and stays out of the keyed-provider registry,
	// but its path prefix routes the same way.
	if firstPathSegment(path) == "antigravity" {
		return accounts.ProviderAntigravity, true
	}
	entry, ok := keyedProviderForPathPrefix(firstPathSegment(path))
	if !ok {
		return "", false
	}
	return entry.Provider, true
}

func firstPathSegment(path string) string {
	path = strings.TrimPrefix(path, "/")
	if path == "" {
		return ""
	}
	segment, _, _ := strings.Cut(path, "/")
	return segment
}

func stripProviderPathPrefix(path, provider string) string {
	if path == "" || path == "/" {
		return "/"
	}
	prefix := "/" + provider
	if path == prefix {
		return "/"
	}
	if strings.HasPrefix(path, prefix+"/") {
		return strings.TrimPrefix(path, prefix)
	}
	return path
}

func filterAccountsForProvider(all []accounts.Account, provider accounts.Provider) []accounts.Account {
	// A provider that shares a subscription with another selects that
	// provider's accounts, so one stored credential serves both. The selected
	// account is stamped with the requested provider, because everything
	// downstream — upstream selection, path rewriting, auth — must follow the
	// endpoint the client asked for, not the one that owns the credential.
	// Leaving the owner's provider on it routes an Anthropic-protocol request
	// through the OpenAI upstream and its path rules, which 404s.
	// Providers that share a subscription form one credential group, named by
	// the provider that owns it. Matching on the group rather than the exact
	// name means one stored key serves every protocol endpoint, and a key added
	// against the endpoint a user actually calls is not silently orphaned.
	credentialProvider := accountProviderFor(provider)
	filtered := make([]accounts.Account, 0, len(all))
	legacy := make([]accounts.Account, 0)
	for _, account := range all {
		if account.Provider != "" && accountProviderFor(account.Provider) == credentialProvider {
			account.Provider = provider
			filtered = append(filtered, account)
			continue
		}
		if account.Provider == "" {
			legacy = append(legacy, account)
		}
	}
	if len(filtered) > 0 {
		return filtered
	}
	// Built-in Codex and Claude routing historically accepted provider-less
	// static accounts. Keyed and Antigravity pools must never inherit them.
	// Stamp the requested built-in provider before returning: downstream paths
	// such as WebSocket failure classification cannot reinterpret an empty value.
	if isKeyedProvider(provider) || provider == accounts.ProviderAntigravity {
		return nil
	}
	for i := range legacy {
		legacy[i].Provider = provider
	}
	return legacy
}

func accountProviderOrCodex(account accounts.Account) accounts.Provider {
	if account.Provider == "" {
		return accounts.ProviderCodex
	}
	return account.Provider
}

func (s Server) accountList() []accounts.Account {
	return s.accountListContext(context.Background())
}

func (s Server) accountListContext(ctx context.Context) []accounts.Account {
	accounts, _ := s.accountListSnapshotContext(ctx)
	return accounts
}

func (s Server) accountListSnapshot() ([]accounts.Account, uint64) {
	return s.accountListSnapshotContext(context.Background())
}

func (s Server) accountListSnapshotContext(ctx context.Context) ([]accounts.Account, uint64) {
	out := append([]accounts.Account(nil), s.Accounts...)
	if s.AccountRef != nil {
		reloaded, _, err := s.AccountRef.reloadIfDiskGenerationChanged(ctx)
		if err != nil && s.Logger != nil {
			s.Logger.Error("account state generation reload failed", "error", err)
		}
		if reloaded {
			// reloadIfDiskGenerationChanged releases the cross-process lock
			// before returning; cache invalidation must not invert that lock
			// against a concurrent usage refresh.
			s.AccountRef.InvalidateUsageStatusCache()
		}
		loaded, generation, credentialRevision := s.AccountRef.CredentialSnapshot()
		if reloaded && s.SchedulerRef != nil {
			s.SchedulerRef.AdvanceAccountGenerationWithAccounts(generation, credentialRevision, SchedulerAccounts(loaded))
		} else if s.SchedulerRef != nil {
			s.SchedulerRef.SyncAccountCredentials(generation, credentialRevision, SchedulerAccounts(loaded))
		}
		out = append(out, loaded...)
		return out, generation
	}
	return out, 0
}

func (s Server) refreshAccount(ctx context.Context, account accounts.Account) (accounts.Account, error) {
	if s.RefreshAccountFn != nil {
		return s.RefreshAccountFn(ctx, account)
	}
	if s.AccountRef == nil {
		return account, nil
	}
	refreshed, err := s.AccountRef.Refresh(ctx, account)
	if err == nil && s.SchedulerRef != nil {
		loaded, generation, credentialRevision := s.AccountRef.CredentialSnapshot()
		s.SchedulerRef.SyncAccountCredentials(generation, credentialRevision, SchedulerAccounts(loaded))
	}
	return refreshed, err
}

func (s Server) refreshSelectedAccount(ctx context.Context, provider accounts.Provider, agentType, sessionID, userEmail string, r *http.Request, account accounts.Account) (accounts.Account, bool, error) {
	refreshed, err := s.refreshAccount(ctx, account)
	if err == nil {
		return refreshed, false, nil
	}
	if session.ExtractAccountID(r) != "" || account.AuthMode != accounts.AuthModeOAuth {
		return account, false, err
	}
	// A refresh that failed for a transient reason (cancelled context, timeout,
	// upstream 5xx) says nothing about the credential, so failing over would mark
	// a healthy account exhausted and hold it out for the credential TTL. Only a
	// terminal credential error justifies that. This applies to every managed
	// OAuth source, not only the older Codex and Claude stores: otherwise an
	// expired Kimi profile with a revoked refresh token aborts before the
	// response-level failover transport can try another profile.
	if !isTerminalCredentialError(err) {
		return account, false, err
	}
	if s.Logger != nil {
		s.Logger.Warn("selected OAuth account refresh failed, trying another account", "provider", provider, "account", account.ID, "error", err)
	}
	tried := map[string]struct{}{account.ID: {}}
	s.markAccountExhaustedRefreshFailure(account, err)
	lastErr := err
	oauthOnly := provider == accounts.ProviderCodex &&
		(chatGPTBackendPath(r.URL.Path) || codexModelCatalogRequest(r))
	for {
		next, pickErr := s.retryAccount(ctx, provider, agentType, sessionID, userEmail, tried, oauthOnly)
		if pickErr != nil {
			return account, false, lastErr
		}
		tried[next.ID] = struct{}{}
		refreshed, err = s.refreshAccount(ctx, next)
		if err == nil {
			return refreshed, true, nil
		}
		if !isTerminalCredentialError(err) {
			return next, false, err
		}
		lastErr = err
		s.markAccountExhaustedRefreshFailure(next, err)
		if s.Logger != nil {
			s.Logger.Warn("retry OAuth account refresh failed", "provider", provider, "account", next.ID, "error", err)
		}
	}
}

func (s Server) retryAccount(ctx context.Context, provider accounts.Provider, agentType, sessionID, userEmail string, tried map[string]struct{}, oauthOnly bool) (accounts.Account, error) {
	candidates := filterAccountsForProvider(s.accountListContext(ctx), provider)
	if oauthOnly {
		candidates = oauthAccounts(candidates)
	}
	if len(candidates) == 0 {
		return accounts.Account{}, fmt.Errorf("no %s accounts available", provider)
	}
	untried := make([]accounts.Account, 0, len(candidates))
	for _, account := range candidates {
		if _, ok := tried[account.ID]; ok {
			continue
		}
		untried = append(untried, account)
	}
	if len(untried) == 0 {
		return accounts.Account{}, fmt.Errorf("no untried %s accounts available", provider)
	}
	if provider == accounts.ProviderCodex || provider == accounts.ProviderClaude {
		s.refreshUsageScoresIfStale(ctx)
	}
	scheduler := s.scheduler()
	if s.Sessions != nil {
		scheduler = scheduler.WithSessionCounts(SchedulerSessionCounts(s.Sessions))
	}
	account, err := pickRoutingAccount(scheduler, untried)
	if err != nil {
		return accounts.Account{}, err
	}
	if scheduler.Exhausted(schedulerAccountProvider(account.Provider), account.ID) {
		return accounts.Account{}, fmt.Errorf("no non-exhausted %s accounts available", provider)
	}
	return account, nil
}

const replayablePostMaxBodyBytes = 128 << 20
const replayablePostMaxAttempts = 6

const subrouterNoRetryHeader = "X-Subrouter-No-Retry"

func subrouterNoRetryRequest(r *http.Request) bool {
	if r == nil {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(r.Header.Get(subrouterNoRetryHeader))) {
	case "1", "true", "yes":
		return true
	default:
		return false
	}
}

func (s Server) usageLimitRetryMaxAttempts(_ context.Context, _ accounts.Provider) int {
	// The budget is request-wide, not pool-sized. A large account pool must not
	// turn one client request into one provider call per account.
	return replayablePostMaxAttempts
}

const replayablePostMaxConcurrentUploads = 4

// replayablePostLimiterMinBytes exempts ordinary requests from the upload
// limiter. The limiter exists to keep a few concurrent huge uploads from
// saturating the uplink; the body is already buffered in memory before the
// transport runs, so the limiter bounds bandwidth, not memory. Holding a slot
// is expensive: one slot spans the entire nested retry chain — account
// failover, overload backoff sleeps, the Fable fallback including Bedrock's
// commit-window peek, and the wait for response headers — so gating every
// POST serialized all message traffic proxy-wide behind 4 slots. Under a
// long-thinking model that looked like the proxy hanging: at most 4 requests
// made progress and everything else queued in the limiter until the client
// gave up. Requests below this size skip the limiter entirely.
const replayablePostLimiterMinBytes = 8 << 20

var replayablePostUploadLimiter = make(chan struct{}, replayablePostMaxConcurrentUploads)

func retryableResponsesPostRequest(r *http.Request) bool {
	if r == nil || r.Method != http.MethodPost {
		return false
	}
	path := r.URL.Path
	return path == "/responses" ||
		path == "/v1/responses" ||
		path == "/responses/compact" ||
		path == "/v1/responses/compact" ||
		// Codex's web-search backend. The body carries a query, so replaying it
		// is as safe as replaying a GET; without it every transport-level blip
		// (TLS record failure, port exhaustion, broken pipe) reached the client
		// as a 502 that the retry layer already absorbs for /responses.
		path == "/alpha/search" ||
		path == "/v1/alpha/search"
}

func retryableUpstreamPostRequest(provider accounts.Provider, r *http.Request) bool {
	if provider == accounts.ProviderAntigravity {
		// Cloud Code exposes all AGY operations as POSTs under v1internal. The
		// request body is replayable and account-specific quota/auth failures can
		// be retried on another OAuth profile.
		if r == nil || r.Method != http.MethodPost {
			return false
		}
		path := strings.TrimPrefix(r.URL.Path, "/antigravity")
		return strings.HasPrefix(path, "/v1internal:") || strings.HasPrefix(path, "/v1internal/")
	}
	if provider == accounts.ProviderClaude {
		if r == nil || r.Method != http.MethodPost {
			return false
		}
		path := r.URL.Path
		return path == "/v1/messages" || path == "/messages"
	}
	if _, keyed := keyedProviderFor(provider); keyed {
		if r == nil || r.Method != http.MethodPost {
			return false
		}
		switch r.URL.Path {
		case "/chat/completions", "/v1/chat/completions", "/messages", "/v1/messages", "/responses", "/v1/responses":
			return true
		default:
			return false
		}
	}
	return retryableResponsesPostRequest(r)
}

func qwenPlanProvider(provider accounts.Provider) bool {
	switch provider {
	case accounts.ProviderQwen, accounts.ProviderQwenToken, accounts.ProviderQwenAnthropic:
		return true
	default:
		return false
	}
}

func makeRequestBodyReplayable(r *http.Request, maxBytes int64) (bool, error) {
	if r.Body == nil || r.GetBody != nil {
		return true, nil
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBytes+1))
	if err != nil {
		return false, err
	}
	if int64(len(body)) > maxBytes {
		r.Body = prefixReadCloser{
			Reader: io.MultiReader(bytes.NewReader(body), r.Body),
			Closer: r.Body,
		}
		return false, nil
	}
	if err := r.Body.Close(); err != nil {
		return false, err
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	r.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(body)), nil
	}
	r.ContentLength = int64(len(body))
	return true, nil
}

type prefixReadCloser struct {
	io.Reader
	io.Closer
}

type replayablePostRetryTransport struct {
	base        http.RoundTripper
	logger      *slog.Logger
	agent       string
	session     string
	account     string
	method      string
	path        string
	upstream    string
	maxAttempts int
	limiter     chan struct{}
	// budget is the request's shared retry allowance. It is shared with the
	// usage-limit transport below so nested retry loops cannot multiply into one
	// full budget per layer. Nil is reserved for standalone/unbounded use.
	budget *attemptBudget
}

type usageLimitRetryTransport struct {
	base              http.RoundTripper
	server            *Server
	logger            *slog.Logger
	provider          accounts.Provider
	agent             string
	session           string
	userEmail         string
	account           string
	accountCredential string
	method            string
	path              string
	upstream          string
	maxAttempts       int
	// commitFirstSuccess means pre-request refresh selected an alternate
	// provisionally. Its first 2xx must commit stickiness even though this
	// transport has not yet performed a response-level retry.
	commitFirstSuccess bool
	// expectedAccount is the sticky assignment this request started from. The
	// delayed success commit uses compare-and-swap so it cannot overwrite a
	// newer forced/admin move while a response body is still streaming.
	expectedAccount string
	// sleep waits for the backoff duration or until the context is cancelled.
	// Injectable for tests; nil means a real timer wait.
	sleep func(context.Context, time.Duration) error
	// poolModel is the canonicalized quota-pool model for this request (e.g.
	// "claude-fable"); failover scores candidates against that pool so an
	// account whose pool is cooked but whose base windows are healthy is not
	// retried for a model it cannot serve.
	poolModel string
	// fableFallback, when set, serves the request via the Fable fallback chain
	// (Bedrock, then the dedicated Anthropic API key) once the subscription
	// pool gives up. Set only for Claude Fable requests with a chain configured;
	// it also restricts account failover to OAuth accounts so metered API-key
	// pool accounts never preempt the Bedrock stage.
	fableFallback func() (*http.Response, bool)
	// budget is the request's shared pool-retry allowance; see
	// replayablePostRetryTransport.budget.
	budget *attemptBudget
}

type routedResponseAccountKey struct{}

func tagRoutedResponseAccount(response *http.Response, routed accounts.Account) *http.Response {
	if response == nil {
		return nil
	}
	request := response.Request
	if request == nil {
		request = &http.Request{}
	}
	response.Request = request.WithContext(context.WithValue(request.Context(), routedResponseAccountKey{}, routed))
	return response
}

func routedResponseAccount(response *http.Response) (accounts.Account, bool) {
	if response == nil || response.Request == nil {
		return accounts.Account{}, false
	}
	routed, ok := response.Request.Context().Value(routedResponseAccountKey{}).(accounts.Account)
	return routed, ok
}

// fableFallbackResponse swaps the pool's give-up response for one served by the
// Fable fallback chain (Bedrock, then the dedicated API key). Returns false
// when no chain is configured for this request or every stage failed, in which
// case the caller returns its original response.
func (t usageLimitRetryTransport) fableFallbackResponse(giveUp *http.Response, accountID, reason string) (*http.Response, bool) {
	if t.fableFallback == nil {
		return nil, false
	}
	fallback, ok := t.fableFallback()
	if !ok {
		return nil, false
	}
	if t.logger != nil {
		giveUpStatus := 0
		if giveUp != nil {
			giveUpStatus = giveUp.StatusCode
		}
		t.logger.Warn("serving claude fable via fallback chain",
			"agent", t.agent,
			"session", t.session,
			"account", accountID,
			"reason", reason,
			"pool_status", giveUpStatus,
			"fallback_status", fallback.StatusCode)
	}
	if giveUp != nil && giveUp.Body != nil {
		_ = giveUp.Body.Close()
	}
	// The fallback chain is not the subscription account that produced the
	// rejected response, so prevent passive response capture from attributing
	// the fallback result to that account.
	return tagRoutedResponseAccount(fallback, accounts.Account{Provider: t.provider}), true
}

// providerOverloadMaxRetries bounds same-account overload retries for providers
// whose overload signal is not account-specific (Anthropic 5xx/529 and Kimi
// 429). Small on purpose: Subrouter absorbs brief blips without stacking long
// waits on top of the client's retry budget or amplifying a sustained outage.
const providerOverloadMaxRetries = 2

// providerOverloadMaxWait caps a single overload backoff wait, including one
// requested via Retry-After, so a pathological header cannot hold a proxied
// request hostage.
const providerOverloadMaxWait = 10 * time.Second

// claudeOverloadStatus reports whether the upstream response is an
// Anthropic-side server failure (529 overloaded_error or another 5xx) worth
// retrying on the SAME account: it is not account-specific, so rotating
// accounts cannot help and would only burn the failover budget.
func claudeOverloadStatus(code int) bool {
	return code >= 500 && code <= 599
}

// providerOverloadBackoff picks the wait before an overload retry: Retry-After
// when the upstream sent one (capped), else 1s << retry (1s, 2s, ...).
func providerOverloadBackoff(header http.Header, retry int) time.Duration {
	return providerOverloadBackoffAt(header, retry, time.Now())
}

func providerOverloadBackoffAt(header http.Header, retry int, now time.Time) time.Duration {
	if retryAt := parseRetryAfter(strings.TrimSpace(claudeHeaderGet(header, "Retry-After")), now); !retryAt.IsZero() {
		wait := retryAt.Sub(now)
		if wait > providerOverloadMaxWait {
			return providerOverloadMaxWait
		}
		return wait
	}
	wait := time.Second << retry
	if wait > providerOverloadMaxWait {
		return providerOverloadMaxWait
	}
	return wait
}

// sleepCtx waits for d or until ctx is cancelled, using the injected sleep when
// present (tests) and a real timer otherwise.
func (t usageLimitRetryTransport) sleepCtx(ctx context.Context, d time.Duration) error {
	if t.sleep != nil {
		return t.sleep(ctx, d)
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// responseUsageLimited separates "try another account for this request" from
// "hold this account out of later routing." Some providers reuse one status
// for quota and transient capacity, so a single boolean silently cooks healthy
// accounts.
func (t usageLimitRetryTransport) responseUsageLimited(response *http.Response) (limited, exhausted, credentialFailure bool, err error) {
	if t.provider == accounts.ProviderClaude {
		if response == nil {
			return false, false, false, nil
		}
		// Fail over on a hard status (429/401) OR on the authoritative
		// out-of-quota header even when the upstream answered 200 via overage.
		// Anthropic serves a "rejected" account through paid overage and returns
		// 200, but Claude Code reads anthropic-ratelimit-unified-status=rejected
		// and hard-blocks the user, so a 200 from a rejected account is unusable
		// from the client's view and must be rerouted to a healthy account.
		limited = claudeAccountUnusableStatus(response.StatusCode) || claudeResponseRejected(response.Header)
		return limited, limited && claudeAccountExhaustedByResponse(response.StatusCode, response.Header),
			response.StatusCode == http.StatusUnauthorized, nil
	}
	if response == nil {
		return false, false, false, nil
	}
	if _, keyedProvider := keyedProviderFor(t.provider); keyedProvider {
		credentialFailure, err = responseKeyedCredentialFailure(response)
		if err != nil || credentialFailure {
			return credentialFailure, credentialFailure, credentialFailure, err
		}
	}
	switch t.provider {
	case accounts.ProviderDeepSeek:
		// DeepSeek documents 402 as exhausted balance and 429 as a transient
		// request-rate limit. Both may use an alternate key for this replay, but
		// only 402 should poison later routing.
		switch response.StatusCode {
		case http.StatusPaymentRequired:
			return true, true, false, nil
		case http.StatusTooManyRequests:
			return true, false, false, nil
		default:
			return false, false, false, nil
		}
	case accounts.ProviderKimi:
		limited, exhausted, err = responseKimiUsageLimit(response)
		return limited, exhausted, false, err
	case accounts.ProviderCodex:
		// Codex can return a headerless 429 for a short request burst. Treat it
		// as request-scoped failover, but do not poison the account scheduler;
		// only an explicit usage_limit_reached payload should mark exhaustion.
		if response.StatusCode == http.StatusTooManyRequests {
			return true, false, false, nil
		}
		if response.StatusCode == http.StatusUnauthorized {
			return true, true, true, nil
		}
	case accounts.ProviderAntigravity:
		// Cloud Code uses 429 for both account quota exhaustion and short-lived
		// allocation throttles. Either way another OAuth account is a safe
		// request-level fallback. A bare 429 is deliberately not marked as
		// scheduler exhaustion: without an authoritative quota marker, doing so
		// can cook every account during a transient provider-wide throttle.
		if response.StatusCode == http.StatusTooManyRequests {
			return true, false, false, nil
		}
		if response.StatusCode == http.StatusUnauthorized {
			// An expired/revoked AGY OAuth access token should be refreshed and,
			// if refresh cannot repair it, skipped in favor of another profile.
			return true, true, true, nil
		}
		return false, false, false, nil
	}
	// API-key providers commonly use a plain 429 for either account credit
	// exhaustion or a temporary key-specific allocation throttle. In both cases
	// another separately funded key may still work, so fail over immediately.
	// The scheduler mark uses Retry-After when present and otherwise self-expires
	// after its short default TTL.
	_, keyedProvider := keyedProviderFor(t.provider)
	if keyedProvider && response.StatusCode == http.StatusTooManyRequests {
		return true, true, false, nil
	}
	limited, err = responseUsageLimit(response)
	return limited, limited, false, err
}

// responseKeyedCredentialFailure identifies an upstream rejection of the exact
// API credential used for this attempt. A 401 is authoritative by status.
// Some OpenAI-compatible gateways instead return a structured auth error with
// 400 or 403, so inspect only those statuses and preserve the body for either a
// retry decision or the final client response. Quota/payment/model errors do
// not match credentialUnauthorizedJSON and remain in their own classifiers.
func responseKeyedCredentialFailure(response *http.Response) (bool, error) {
	if response == nil {
		return false, nil
	}
	if response.StatusCode == http.StatusUnauthorized {
		return true, nil
	}
	if response.StatusCode != http.StatusBadRequest && response.StatusCode != http.StatusForbidden || response.Body == nil {
		return false, nil
	}
	body := response.Body
	prefix, err := io.ReadAll(io.LimitReader(body, usageLimitInspectMaxBytes+1))
	if err != nil {
		response.Body = prefixReadCloser{Reader: io.MultiReader(bytes.NewReader(prefix), body), Closer: body}
		return false, err
	}
	if int64(len(prefix)) > usageLimitInspectMaxBytes {
		response.Body = prefixReadCloser{Reader: io.MultiReader(bytes.NewReader(prefix), body), Closer: body}
		return false, nil
	}
	closeErr := body.Close()
	response.Body = io.NopCloser(bytes.NewReader(prefix))
	if closeErr != nil {
		return false, closeErr
	}
	return credentialUnauthorizedJSON(prefix), nil
}

// responseKimiModelCapabilityFailure recognizes the two documented Kimi 401
// responses that describe plan/model capability rather than authentication.
// Keep these exact: an arbitrary Kimi 401 must continue to invalidate the
// credential that produced it.
func responseKimiModelCapabilityFailure(response *http.Response) (bool, error) {
	if response == nil || response.StatusCode != http.StatusUnauthorized || response.Body == nil {
		return false, nil
	}
	body := response.Body
	prefix, err := io.ReadAll(io.LimitReader(body, usageLimitInspectMaxBytes+1))
	if err != nil {
		response.Body = prefixReadCloser{Reader: io.MultiReader(bytes.NewReader(prefix), body), Closer: body}
		return false, err
	}
	if int64(len(prefix)) > usageLimitInspectMaxBytes {
		response.Body = prefixReadCloser{Reader: io.MultiReader(bytes.NewReader(prefix), body), Closer: body}
		return false, nil
	}
	closeErr := body.Close()
	response.Body = io.NopCloser(bytes.NewReader(prefix))
	if closeErr != nil {
		return false, closeErr
	}
	return kimiModelCapabilityErrorJSON(prefix), nil
}

func kimiModelCapabilityErrorJSON(body []byte) bool {
	switch normalizedProviderErrorMessage(body) {
	case "your current subscription does not have access to k3. upgrade to an moderato plan or above. upgrade: https://www.kimi.com/membership/pricing?from=server_k3_error",
		"your current plan supports only kimi-k3 up to 256k context. 1m context is available on higher-tier plans. upgrade: https://www.kimi.com/membership/pricing?from=server_k3_error":
		return true
	default:
		return false
	}
}

func responseKimiUsageLimit(response *http.Response) (limited, exhausted bool, err error) {
	if response == nil || response.StatusCode != http.StatusForbidden || response.Body == nil {
		return false, false, nil
	}
	body := response.Body
	prefix, err := io.ReadAll(io.LimitReader(body, usageLimitInspectMaxBytes+1))
	if err != nil {
		response.Body = prefixReadCloser{Reader: io.MultiReader(bytes.NewReader(prefix), body), Closer: body}
		return false, false, err
	}
	if int64(len(prefix)) > usageLimitInspectMaxBytes {
		response.Body = prefixReadCloser{Reader: io.MultiReader(bytes.NewReader(prefix), body), Closer: body}
		return false, false, nil
	}
	closeErr := body.Close()
	response.Body = io.NopCloser(bytes.NewReader(prefix))
	if closeErr != nil {
		return false, false, closeErr
	}
	message := normalizedProviderErrorMessage(prefix)
	for _, quotaPrefix := range []string{
		"you've reached your 5-hour usage limit",
		"you've reached your weekly (7-day) usage limit",
		"you've reached your monthly usage limit for this billing cycle",
		"your credit balance is insufficient",
		"you've reached your usage limit for this billing cycle",
	} {
		if providerMessageStartsWith(message, quotaPrefix) {
			return true, true, nil
		}
	}
	if providerMessageStartsWith(message, "you've reached your concurrent request limit") {
		return true, false, nil
	}
	return false, false, nil
}

func normalizedProviderErrorMessage(body []byte) string {
	var payload map[string]any
	if json.Unmarshal(body, &payload) != nil {
		return ""
	}
	message := providerErrorMessage(payload)
	return strings.ToLower(strings.Join(strings.Fields(message), " "))
}

func providerMessageStartsWith(message, leadingSentence string) bool {
	return message == leadingSentence || strings.HasPrefix(message, leadingSentence+".")
}

func providerErrorMessage(payload map[string]any) string {
	if message := strings.TrimSpace(stringField(payload, "message")); message != "" {
		return message
	}
	switch nested := payload["error"].(type) {
	case map[string]any:
		return providerErrorMessage(nested)
	case string:
		return strings.TrimSpace(nested)
	default:
		return ""
	}
}

// claudeAccountUnusableStatus reports whether an upstream status means the
// selected Claude account cannot serve this request and subrouter should fail
// over to another account. 429 is quota exhaustion; 401 is a dead or expired
// OAuth token (Anthropic returns authentication_error); 403 is an org-level
// permission_error ("OAuth authentication is currently not allowed for this
// organization" — Anthropic disabling Claude Code subscription access for one
// account's org, observed live 2026-07-04). All are account-specific, so
// replaying the same request on a different account is the correct response
// instead of surfacing the failure to the client; before 403 was included, a
// sticky session pinned to an org-disabled account black-holed every request
// while the rest of the pool was healthy.
func claudeAccountUnusableStatus(code int) bool {
	return code == http.StatusTooManyRequests || code == http.StatusUnauthorized || code == http.StatusForbidden
}

// claudeAccountExhaustedByResponse reports whether an upstream response means the
// account is genuinely out of subscription quota and should be dropped from the
// routing scheduler (not merely failed over for this one request). The
// authoritative quota signal is the anthropic-ratelimit-unified-status header:
// "rejected" marks the account exhausted regardless of HTTP status, because
// Anthropic answers a depleted account with 200 via paid overage while still
// reporting rejected.
//
// A bare 429 with no unified-status header is NOT treated as exhaustion.
// Observed in production: healthy accounts (80-100% quota) return a headerless
// rate_limit_error 429 under short-window request bursts, and poisoning their
// routing score on that is wrong. Such a 429 still fails over
// (claudeAccountUnusableStatus is status-based), but genuine quota exhaustion is
// detected by the rejected header and the periodic usage-score refresh, not by a
// bare 429. A 401 is always a dead/expired token.
func claudeAccountExhaustedByResponse(status int, header http.Header) bool {
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		return true
	}
	if claudeUnifiedStatus(header) != "rejected" {
		return false
	}
	// A rejection caused solely by a model-scoped window (e.g. the Fable
	// 7d_oi bucket) while the account-wide 5h/7d windows are still allowed
	// must not cook the whole account: opus/sonnet traffic can still use it,
	// and the usage refresh zeroes the affected pool on its own evidence.
	// Before this, a wave of Fable traffic marked every account exhausted and
	// starved opus/sonnet routing.
	return !claudeRejectionIsModelPoolScoped(header)
}

// claudeModelPoolWindowPrefixes are the unified-header window prefixes that
// meter a single model family rather than the whole account. 7d_oi is the
// hidden Fable/OAuth-apps weekly bucket.
var claudeModelPoolWindowPrefixes = []string{"7d_oi"}

// claudeRejectionIsModelPoolScoped reports whether a rejected response is
// attributable only to a model-scoped window: some pool window says rejected
// and no account-wide window does. Responses without per-window statuses are
// treated as account-wide (conservative).
func claudeRejectionIsModelPoolScoped(header http.Header) bool {
	windowStatus := func(prefix string) string {
		return strings.ToLower(strings.TrimSpace(claudeHeaderGet(header, "anthropic-ratelimit-unified-"+prefix+"-status")))
	}
	poolRejected := false
	for _, prefix := range claudeModelPoolWindowPrefixes {
		if windowStatus(prefix) == "rejected" {
			poolRejected = true
			break
		}
	}
	if !poolRejected {
		return false
	}
	for _, prefix := range []string{"5h", "7d"} {
		if windowStatus(prefix) == "rejected" {
			return false
		}
	}
	return true
}

// claudeRejectionIsExplicitlyAccountWide distinguishes an authoritative
// account-window rejection from a bare unified rejection. Bare rejections are
// held to the request's model pool because Anthropic omits individual window
// statuses on some Fable/Opus responses. Credential and organization failures
// are intrinsically account-wide.
func claudeRejectionIsExplicitlyAccountWide(status int, header http.Header) bool {
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		return true
	}
	for _, prefix := range []string{"5h", "7d"} {
		windowStatus := strings.ToLower(strings.TrimSpace(claudeHeaderGet(
			header, "anthropic-ratelimit-unified-"+prefix+"-status",
		)))
		if windowStatus == "rejected" {
			return true
		}
	}
	return false
}

// claudeUnifiedStatus returns the lowercased anthropic-ratelimit-unified-status
// header ("allowed" | "allowed_warning" | "rejected"), or "" when absent.
func claudeUnifiedStatus(header http.Header) string {
	return strings.ToLower(strings.TrimSpace(claudeHeaderGet(header, "anthropic-ratelimit-unified-status")))
}

// claudeResponseRejected reports whether Anthropic flagged the account as out of
// quota for this response, even if it answered 200 via overage.
func claudeResponseRejected(header http.Header) bool {
	return claudeUnifiedStatus(header) == "rejected"
}

func claudeHeaderGet(header http.Header, key string) string {
	if header == nil {
		return ""
	}
	return header.Get(key)
}

// claudeRateLimitHeaderFields extracts the upstream rate-limit headers Anthropic
// returns on a 429/401 (retry-after plus the anthropic-ratelimit-unified-*
// family) as flat key/value slog fields, so the genuine reset/remaining signal
// is captured in logs instead of being silently dropped.
func claudeRateLimitHeaderFields(header http.Header) []any {
	if header == nil {
		return nil
	}
	type kv struct{ key, value string }
	pairs := make([]kv, 0, len(header))
	for key, values := range header {
		lower := strings.ToLower(key)
		if lower == "retry-after" || strings.HasPrefix(lower, "anthropic-ratelimit") {
			pairs = append(pairs, kv{key: lower, value: strings.Join(values, ",")})
		}
	}
	sort.Slice(pairs, func(i, j int) bool { return pairs[i].key < pairs[j].key })
	fields := make([]any, 0, len(pairs)*2)
	for _, pair := range pairs {
		fields = append(fields, pair.key, pair.value)
	}
	return fields
}

func (t usageLimitRetryTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	maxAttempts := t.maxAttempts
	if maxAttempts < 1 {
		maxAttempts = 1
	}
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	attemptReq := req
	accountID := t.account
	accountCredential := t.accountCredential
	// Native AGY includes the project selected by the local CLI in every
	// generation envelope.  A pooled launch may select a different server
	// account before the first upstream attempt, so bind that envelope to the
	// selected account up front (not only after a failover).  Cloud Code treats
	// a bearer/project mismatch as an allocation failure and may return a
	// misleading 429.
	if t.provider == accounts.ProviderAntigravity && t.server != nil && accountID != "" {
		var rawBody []byte
		var readErr error
		if req.GetBody != nil {
			body, bodyErr := req.GetBody()
			if bodyErr != nil {
				return nil, bodyErr
			}
			rawBody, readErr = io.ReadAll(body)
			_ = body.Close()
		} else if req.Body != nil {
			rawBody, readErr = io.ReadAll(io.LimitReader(req.Body, 1<<20+1))
			_ = req.Body.Close()
		}
		if readErr != nil {
			return nil, readErr
		}
		if antigravityProjectFromBody(rawBody) != "" {
			bearer := strings.TrimSpace(strings.TrimPrefix(attemptReq.Header.Get("Authorization"), "Bearer "))
			if bearer == "" {
				return nil, errors.New("AGY pooled request has no bearer for project binding")
			}
			upstream := t.server.upstreamForRequest(t.path, accounts.Account{ID: accountID, Provider: accounts.ProviderAntigravity})
			if upstream == nil {
				return nil, errors.New("AGY project binding has no upstream")
			}
			project, projectErr := t.server.antigravityProject(req.Context(), accounts.Account{ID: accountID, Token: bearer}, upstream)
			if projectErr != nil {
				return nil, projectErr
			}
			rewritten, changed, rewriteErr := rewriteAntigravityProject(rawBody, project)
			if rewriteErr != nil {
				return nil, rewriteErr
			}
			if changed {
				attemptReq = req.Clone(req.Context())
				attemptReq.Body = io.NopCloser(bytes.NewReader(rewritten))
				attemptReq.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(rewritten)), nil }
				attemptReq.ContentLength = int64(len(rewritten))
			} else if req.GetBody == nil {
				// Restore a one-shot body even when no rewrite was needed; the
				// snapshot above consumed it while inspecting the envelope.
				attemptReq = req.Clone(req.Context())
				attemptReq.Body = io.NopCloser(bytes.NewReader(rawBody))
				attemptReq.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(rawBody)), nil }
				attemptReq.ContentLength = int64(len(rawBody))
			}
		} else if req.GetBody == nil && len(rawBody) > 0 {
			attemptReq = req.Clone(req.Context())
			attemptReq.Body = io.NopCloser(bytes.NewReader(rawBody))
			attemptReq.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(rawBody)), nil }
			attemptReq.ContentLength = int64(len(rawBody))
		}
	}
	tried := map[string]struct{}{}
	if accountID != "" {
		tried[accountID] = struct{}{}
	}
	overloadRetries := 0
	sealedStripped := false
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		response, err := base.RoundTrip(attemptReq)
		response = tagRoutedResponseAccount(response, accounts.Account{
			ID: accountID, Provider: t.provider, CredentialVersion: accountCredential,
		})
		if err != nil || req.GetBody == nil || req.Context().Err() != nil {
			return response, err
		}
		// Anthropic overload (529/5xx): retry the SAME account after a bounded
		// backoff. Overload is API-wide, not account-specific, so no failover, no
		// exhaustion-marking, and no failover-budget consumption. Once the small
		// overload budget is spent the 5xx passes through and the client's own
		// backoff takes over. A 5xx that carries the rejected unified-status
		// header is NOT overload-retried: rejected means this account is out of
		// quota regardless of HTTP status, so it falls through to the usage-limit
		// path below and fails over to a healthy account instead.
		claudeOverload := t.provider == accounts.ProviderClaude && claudeOverloadStatus(response.StatusCode) && !claudeResponseRejected(response.Header)
		kimiOverload := t.provider == accounts.ProviderKimi && response.StatusCode == http.StatusTooManyRequests
		if claudeOverload || kimiOverload {
			if overloadRetries >= providerOverloadMaxRetries {
				if fallback, ok := t.fableFallbackResponse(response, accountID, "overload"); ok {
					return fallback, nil
				}
				return response, nil
			}
			if !t.budget.consume() {
				return response, nil
			}
			wait := providerOverloadBackoff(response.Header, overloadRetries)
			overloadRetries++
			if t.logger != nil {
				t.logger.Warn("retrying after provider overload", "provider", t.provider, "agent", t.agent, "session", t.session, "account", accountID, "method", t.method, "path", t.path, "upstream", t.upstream, "status", response.StatusCode, "wait", wait.String(), "overload_retry", overloadRetries, "max_overload_retries", providerOverloadMaxRetries)
			}
			if response.Body != nil {
				_ = response.Body.Close()
			}
			if sleepErr := t.sleepCtx(req.Context(), wait); sleepErr != nil {
				return nil, sleepErr
			}
			body, bodyErr := req.GetBody()
			if bodyErr != nil {
				return nil, bodyErr
			}
			// Preserve the CURRENT attempt's headers: after an earlier account
			// failover attemptReq carries that account's auth, and cloning from the
			// original req would silently revert to the first account.
			currentHeader := attemptReq.Header.Clone()
			attemptReq = req.Clone(req.Context())
			attemptReq.Body = body
			attemptReq.GetBody = req.GetBody
			attemptReq.ContentLength = req.ContentLength
			attemptReq.Header = currentHeader
			attempt-- // retry the same account without spending a failover slot
			continue
		}
		// A conversation that came back from another provider carries reasoning
		// that provider sealed, and OpenAI cannot read Azure's any more than
		// Azure could read OpenAI's. The client sees a hard 400 unless the
		// sealed items are dropped here, which is the same repair the Azure
		// route already performs in the other direction. It is not an account
		// problem, so it retries the same account without spending a failover
		// slot. It still spends the request-wide retry budget.
		if t.provider == accounts.ProviderCodex && !sealedStripped &&
			response.StatusCode == http.StatusBadRequest && req.GetBody != nil {
			stripped, retryReq, handled := t.retryWithoutSealedReasoning(req, attemptReq, response)
			if handled {
				sealedStripped = true
				if stripped {
					if !t.budget.consume() {
						return response, nil
					}
					attemptReq = retryReq
					attempt--
					continue
				}
				return response, nil
			}
		}
		modelUnsupported := false
		var inspectErr error
		switch t.provider {
		case accounts.ProviderCodex:
			modelUnsupported, inspectErr = responseCodexChatGPTModelUnsupported(response)
		case accounts.ProviderKimi:
			modelUnsupported, inspectErr = responseKimiModelCapabilityFailure(response)
		}
		if inspectErr != nil {
			if t.logger != nil {
				t.logger.Warn("model capability response inspection failed", "agent", t.agent, "session", t.session, "account", accountID, "method", t.method, "path", t.path, "upstream", t.upstream, "error", inspectErr)
			}
			return response, nil
		}
		usageLimited, exhausted, credentialFailure := false, false, false
		if !modelUnsupported {
			usageLimited, exhausted, credentialFailure, inspectErr = t.responseUsageLimited(response)
			if inspectErr != nil {
				if t.logger != nil {
					t.logger.Warn("usage-limit response inspection failed", "agent", t.agent, "session", t.session, "account", accountID, "method", t.method, "path", t.path, "upstream", t.upstream, "error", inspectErr)
				}
				return response, nil
			}
			if t.provider == accounts.ProviderAntigravity && usageLimited {
				t.logAntigravityUnusableResponse(response, accountID)
			}
		}
		if !usageLimited && !modelUnsupported {
			if err := t.commitSuccessfulFailover(response, attempt, accountID); err != nil {
				if response.Body != nil {
					_ = response.Body.Close()
				}
				return nil, err
			}
			return response, nil
		}
		exhaustionPool := selectacct.ModelKey(t.poolModel)
		if t.provider == accounts.ProviderCodex && usageLimited {
			// Codex usage_limit_reached exhausts the subscription account, not
			// only the model named by this request. Model compatibility errors
			// below remain scoped to the rejected model.
			exhaustionPool = ""
		}
		if _, keyedProvider := keyedProviderFor(t.provider); keyedProvider && exhausted {
			// Provider-plan and API-key quota errors apply to the credential,
			// regardless of which request model happened to observe them.
			exhaustionPool = ""
		}
		if t.provider == accounts.ProviderClaude {
			// Surface the genuine upstream rate-limit signal. The active retry
			// path consumes this 429 before the passive ModifyResponse capture
			// runs, so without logging here the real message would be invisible
			// whenever failover succeeds.
			t.logClaudeUnusableResponse(response, accountID)
			// Only poison the score on genuine quota exhaustion; a transient
			// "allowed"/"allowed_warning" 429 still fails over for this request.
			exhausted = claudeAccountExhaustedByResponse(response.StatusCode, response.Header)
			if exhausted && claudeRejectionIsExplicitlyAccountWide(response.StatusCode, response.Header) {
				// Account-wide Claude windows apply across every model pool. The
				// direct retry path sees the headers before passive response
				// inspection, so it must clear the request model's pool key here.
				exhaustionPool = ""
			}
		}
		var compatibilityNext accounts.Account
		var compatibilityPickErr error
		if modelUnsupported && t.server != nil {
			compatibilityNext, compatibilityPickErr = t.server.rerouteModelIncompatibility(
				req.Context(), t.provider, t.agent, t.session, t.userEmail, accountID, exhaustionPool, tried,
			)
		}
		if t.server != nil && credentialFailure && !modelUnsupported {
			// Bind auth rejection to the exact credential identity captured for
			// this attempt. A concurrent key rotation must make this late response
			// harmless rather than cooking the repaired account generation.
			t.server.markAccountExhaustedCredentialForAccount(accounts.Account{
				ID: accountID, Provider: t.provider, CredentialVersion: accountCredential,
			})
		} else if t.server != nil && exhausted && !modelUnsupported {
			// Use the response's own reset time so the mark self-expires when the
			// window recovers (codex responses lack these headers and fall back
			// to the default TTL inside claudeExhaustionExpiry).
			t.server.markAccountExhaustedFromResponseForAccount(accounts.Account{
				ID: accountID, Provider: t.provider, CredentialVersion: accountCredential,
			}, exhaustionPool, response.StatusCode, response.Header)
		}
		budgetExhausted := false
		if attempt < maxAttempts && t.server != nil {
			budgetExhausted = !t.budget.consume()
		}
		if attempt == maxAttempts || t.server == nil || budgetExhausted {
			reason := "max_attempts"
			switch {
			case t.server == nil:
				reason = "no_server"
			case budgetExhausted:
				reason = "retry_budget"
			}
			if fallback, ok := t.fableFallbackResponse(response, accountID, reason); ok {
				return fallback, nil
			}
			t.logClaudeFailoverExhausted(response, accountID, reason, attempt, maxAttempts, len(tried))
			return response, nil
		}
		nextAccount, pickErr := compatibilityNext, compatibilityPickErr
		if !modelUnsupported {
			nextAccount, pickErr = t.server.oauthRetryCandidate(req.Context(), t.provider, t.agent, t.session, t.userEmail, t.poolModel, tried, t.fableFallback != nil)
		}
		if pickErr != nil {
			if t.logger != nil {
				t.logger.Warn("usage-limit retry has no alternate account", "agent", t.agent, "session", t.session, "account", accountID, "method", t.method, "path", t.path, "upstream", t.upstream, "error", pickErr)
			}
			if fallback, ok := t.fableFallbackResponse(response, accountID, "no_alternate_account"); ok {
				return fallback, nil
			}
			t.logClaudeFailoverExhausted(response, accountID, "no_alternate_account", attempt, maxAttempts, len(tried))
			return response, nil
		}
		body, bodyErr := req.GetBody()
		if bodyErr != nil {
			if t.logger != nil {
				t.logger.Warn("usage-limit retry could not replay request body", "agent", t.agent, "session", t.session, "account", accountID, "method", t.method, "path", t.path, "upstream", t.upstream, "error", bodyErr)
			}
			t.logClaudeFailoverExhausted(response, accountID, "replay_failed", attempt, maxAttempts, len(tried))
			return response, nil
		}
		rawBody, readBodyErr := io.ReadAll(body)
		_ = body.Close()
		if readBodyErr != nil {
			return response, nil
		}
		body = io.NopCloser(bytes.NewReader(rawBody))
		if response.Body != nil {
			_ = response.Body.Close()
		}
		previousAccount := accountID
		accountID = nextAccount.ID
		accountCredential = nextAccount.CredentialIdentity()
		tried[accountID] = struct{}{}
		if t.server != nil && t.server.SchedulerRef != nil {
			t.server.SchedulerRef.NoteRouted(schedulerAccountProvider(t.provider), accountID)
		}
		attemptReq = req.Clone(req.Context())
		attemptReq.Body = body
		attemptReq.GetBody = req.GetBody
		attemptReq.ContentLength = req.ContentLength
		// A provider may route API-key and subscription credentials to different
		// hosts (Grok does). Rebuild the target from the replacement account so a
		// mixed-auth failover never sends a credential to the previous account's
		// upstream.
		if nextUpstream := t.server.upstreamForRequest(t.path, nextAccount); nextUpstream != nil {
			attemptReq.URL.Scheme = nextUpstream.Scheme
			attemptReq.URL.Host = nextUpstream.Host
			attemptReq.URL.User = nextUpstream.User
			attemptReq.URL.Path = joinURLPath(nextUpstream.Path, t.server.pathForUpstream(t.path, nextAccount))
			attemptReq.URL.RawPath = ""
			if t.provider == accounts.ProviderAntigravity && antigravityProjectFromBody(rawBody) != "" {
				project, projectErr := t.server.antigravityProject(req.Context(), nextAccount, nextUpstream)
				if projectErr != nil {
					if t.logger != nil {
						t.logger.Warn("AGY failover refused without replacement project", "agent", t.agent, "session", t.session, "account", nextAccount.ID, "error", projectErr)
					}
					return response, nil
				}
				rewritten, changed, rewriteErr := rewriteAntigravityProject(rawBody, project)
				if rewriteErr != nil {
					return response, nil
				}
				if changed {
					body = io.NopCloser(bytes.NewReader(rewritten))
					attemptReq.Body = body
					attemptReq.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(rewritten)), nil }
					attemptReq.ContentLength = int64(len(rewritten))
				}
			}
		}
		setAccountAuthHeaders(attemptReq.Header, nextAccount, t.poolModel)
		if t.logger != nil {
			t.logger.Warn("retrying replayable upstream request after usage limit", "agent", t.agent, "session", t.session, "previous_account", previousAccount, "account", accountID, "method", t.method, "path", t.path, "upstream", t.upstream, "attempt", attempt+1, "max_attempts", maxAttempts)
		}
	}
	response, err := base.RoundTrip(req)
	return tagRoutedResponseAccount(response, accounts.Account{
		ID: accountID, Provider: t.provider, CredentialVersion: accountCredential,
	}), err
}

// commitSuccessfulFailover moves durable stickiness only after the replacement
// account has accepted the request. Candidate selection is provisional: a
// replay/body failure or another upstream error must leave the prior session
// assignment intact so the next request does not start on an unproven account.
func (t usageLimitRetryTransport) commitSuccessfulFailover(response *http.Response, attempt int, accountID string) error {
	if (!t.commitFirstSuccess && attempt <= 1) || response == nil || response.StatusCode < 200 || response.StatusCode >= 300 ||
		t.server == nil || t.server.Sessions == nil {
		return nil
	}
	expectedAccount := t.expectedAccount
	if expectedAccount == "" {
		expectedAccount = t.account
	}
	if err := t.server.commitSuccessfulHTTPResponse(response, t.agent, t.session, expectedAccount, accountID, t.userEmail); err != nil {
		return fmt.Errorf("persist successful session reassignment: %w", err)
	}
	return nil
}

func (s Server) commitSuccessfulHTTPResponse(response *http.Response, agentType, sessionID, expectedAccountID, accountID, userEmail string) error {
	commit := func() error {
		_, err := s.commitSessionReassignment(agentType, sessionID, expectedAccountID, accountID, userEmail)
		return err
	}
	if response == nil || response.Body == nil || response.Body == http.NoBody {
		return commit()
	}
	if strings.HasPrefix(strings.ToLower(response.Header.Get("Content-Type")), "text/event-stream") {
		// A streaming API has only accepted the request when its terminal event
		// says the turn completed. HTTP 200 headers alone are not enough: Codex
		// and Anthropic-shaped providers can end a 200 stream with a failure.
		response.Body = newSessionCommitSSEReadCloser(response.Body, commit)
		return nil
	}
	// A non-streaming 2xx can still reset or truncate while its body is copied
	// to the client. Commit only on clean EOF so such a response does not pin
	// the session to the account that failed it.
	response.Body = newSessionCommitEOFReadCloser(response.Body, commit)
	return nil
}

type sessionCommitEOFReadCloser struct {
	reader   io.Reader
	closer   io.Closer
	commit   func() error
	finished bool
}

func newSessionCommitEOFReadCloser(body io.ReadCloser, commit func() error) io.ReadCloser {
	return &sessionCommitEOFReadCloser{reader: body, closer: body, commit: commit}
}

func (r *sessionCommitEOFReadCloser) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	if r.finished {
		return n, err
	}
	if err == io.EOF {
		r.finished = true
		if r.commit != nil {
			if commitErr := r.commit(); commitErr != nil {
				return n, fmt.Errorf("persist successful response session reassignment: %w", commitErr)
			}
		}
	}
	return n, err
}

func (r *sessionCommitEOFReadCloser) Close() error {
	return r.closer.Close()
}

const sessionCommitSSEMaxLineBytes = 1 << 20

type sessionCommitSSEReadCloser struct {
	io.ReadCloser
	commit       func() error
	line         []byte
	eventName    string
	eventData    []byte
	discardEvent bool
	terminal     bool
}

func newSessionCommitSSEReadCloser(body io.ReadCloser, commit func() error) io.ReadCloser {
	return &sessionCommitSSEReadCloser{ReadCloser: body, commit: commit}
}

func (r *sessionCommitSSEReadCloser) Read(p []byte) (int, error) {
	n, readErr := r.ReadCloser.Read(p)
	if n > 0 && !r.terminal {
		terminal, success := r.observe(p[:n], false)
		if terminal {
			r.terminal = true
			if success && r.commit != nil {
				if err := r.commit(); err != nil {
					// Do not deliver the terminal-success bytes after failing to
					// persist their routing consequence. Earlier streamed content
					// may already be visible, but the client sees a broken stream,
					// not a falsely completed turn.
					return 0, fmt.Errorf("persist successful streamed session reassignment: %w", err)
				}
			}
		}
	}
	if readErr == io.EOF && !r.terminal {
		terminal, success := r.observe(nil, true)
		if terminal {
			r.terminal = true
			if success && r.commit != nil {
				if err := r.commit(); err != nil {
					return n, fmt.Errorf("persist successful streamed session reassignment: %w", err)
				}
			}
		}
	}
	return n, readErr
}

func (r *sessionCommitSSEReadCloser) observe(chunk []byte, _ bool) (bool, bool) {
	for _, b := range chunk {
		if b != '\n' {
			if len(r.line) < sessionCommitSSEMaxLineBytes {
				r.line = append(r.line, b)
			} else {
				r.discardEvent = true
			}
			continue
		}
		if terminal, success := r.finishLine(); terminal {
			return terminal, success
		}
	}
	// EOF is deliberately not a success signal. A cleanly closed HTTP body can
	// still be a provider-side truncation, and committing a provisional account
	// on that ambiguity would pin the session to an account that never completed
	// the turn. Only a recognized terminal success event may commit.
	return false, false
}

func (r *sessionCommitSSEReadCloser) finishLine() (bool, bool) {
	line := bytes.TrimSuffix(r.line, []byte{'\r'})
	r.line = r.line[:0]
	if len(line) == 0 {
		return r.finishEvent()
	}
	if r.discardEvent {
		return false, false
	}
	if bytes.HasPrefix(line, []byte("event:")) {
		r.eventName = strings.ToLower(strings.TrimSpace(string(bytes.TrimPrefix(line, []byte("event:")))))
		return false, false
	}
	if !bytes.HasPrefix(line, []byte("data:")) {
		return false, false
	}
	data := bytes.TrimPrefix(line, []byte("data:"))
	data = bytes.TrimPrefix(data, []byte{' '})
	if len(r.eventData) > 0 {
		r.eventData = append(r.eventData, '\n')
	}
	if len(r.eventData)+len(data) > sessionCommitSSEMaxLineBytes {
		r.discardEvent = true
		r.eventData = r.eventData[:0]
		return false, false
	}
	r.eventData = append(r.eventData, data...)
	return false, false
}

func (r *sessionCommitSSEReadCloser) finishEvent() (bool, bool) {
	if r.discardEvent {
		r.discardEvent = false
		r.eventData = r.eventData[:0]
		r.eventName = ""
		return false, false
	}
	payload := bytes.TrimSpace(r.eventData)
	r.eventData = r.eventData[:0]
	eventName := r.eventName
	r.eventName = ""
	if bytes.Equal(payload, []byte("[DONE]")) {
		return true, true
	}
	var event map[string]any
	if len(payload) > 0 && json.Unmarshal(payload, &event) == nil {
		if payloadType := strings.ToLower(strings.TrimSpace(stringField(event, "type"))); payloadType != "" {
			eventName = payloadType
		}
	}
	switch eventName {
	case "response.completed", "response.done", "message_stop", "done":
		return true, true
	case "response.failed", "response.incomplete", "error":
		return true, false
	default:
		return false, false
	}
}

func (s Server) commitSessionReassignment(agentType, sessionID, expectedAccountID, accountID, userEmail string) (bool, error) {
	if s.Sessions == nil {
		return true, nil
	}
	_, swapped, err := s.Sessions.CompareAndPut(agentType, sessionID, expectedAccountID, accountID, userEmail)
	if err != nil {
		return false, err
	}
	return swapped, nil
}

// retryWithoutSealedReasoning inspects a 400 and, when the upstream refused
// reasoning it cannot decrypt, rebuilds the request without those sealed items.
//
// handled reports whether the response was consumed here. stripped reports
// whether a retry request was produced; a rejection with nothing to strip is
// returned to the client untouched, because resending an identical body would
// only fail again.
func (t usageLimitRetryTransport) retryWithoutSealedReasoning(
	req *http.Request,
	attemptReq *http.Request,
	response *http.Response,
) (bool, *http.Request, bool) {
	errorBody, err := io.ReadAll(io.LimitReader(response.Body, azureCodexMaxErrorBodyBytes))
	_ = response.Body.Close()
	if err != nil {
		return false, nil, false
	}
	response.Body = io.NopCloser(bytes.NewReader(errorBody))
	if !azureCodexEncryptedContentRejected(errorBody) {
		return false, nil, false
	}
	rc, err := req.GetBody()
	if err != nil {
		return false, nil, true
	}
	wireBody, err := io.ReadAll(rc)
	_ = rc.Close()
	if err != nil {
		return false, nil, true
	}
	maxBytes := int64(replayablePostMaxBodyBytes)
	if t.server != nil && t.server.MaxBodyBytes > maxBytes {
		maxBytes = t.server.MaxBodyBytes
	}
	decoded, err := decodedJSONRequestBody(wireBody, attemptReq.Header.Get("Content-Encoding"), maxBytes)
	if err != nil {
		return false, nil, true
	}
	var payload map[string]json.RawMessage
	if json.Unmarshal(decoded, &payload) != nil || payload == nil {
		return false, nil, true
	}
	if !azureCodexStripEncryptedReasoning(payload) {
		return false, nil, true
	}
	rebuilt, err := json.Marshal(payload)
	if err != nil {
		return false, nil, true
	}
	if t.logger != nil {
		t.logger.Warn("upstream cannot decrypt reasoning from another provider; retrying without it",
			"agent", t.agent, "session", t.session, "account", t.account,
			"method", t.method, "path", t.path, "upstream", t.upstream)
	}
	retryReq := attemptReq.Clone(req.Context())
	retryReq.Body = io.NopCloser(bytes.NewReader(rebuilt))
	retryReq.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(rebuilt)), nil
	}
	retryReq.ContentLength = int64(len(rebuilt))
	// The rebuilt body is plain JSON whatever the client sent.
	retryReq.Header.Del("Content-Encoding")
	if response.Body != nil {
		_ = response.Body.Close()
	}
	return true, retryReq, true
}

// logClaudeFailoverExhausted emits the single authoritative signal that a Claude
// rate-limit (or 401) is being returned to the client because failover gave up.
// "no alternate account" and a silent maxAttempts exhaustion are both failures
// from the client's view, but only the former was logged before, so a request
// that burned through maxAttempts while untried accounts still had quota failed
// invisibly. Monitoring keys on this one string; tried_accounts vs the account
// pool tells whether quota may have remained elsewhere.
func (t usageLimitRetryTransport) logClaudeFailoverExhausted(response *http.Response, accountID, reason string, attempt, maxAttempts, triedCount int) {
	if t.logger == nil || t.provider != accounts.ProviderClaude {
		return
	}
	status := 0
	if response != nil {
		status = response.StatusCode
	}
	t.logger.Warn("claude rate-limit returned to client after failover exhausted",
		"agent", t.agent,
		"session", t.session,
		"final_account", accountID,
		"reason", reason,
		"status", status,
		"attempts", attempt,
		"max_attempts", maxAttempts,
		"tried_accounts", triedCount,
		"method", t.method,
		"path", t.path,
		"upstream", t.upstream)
}

// logClaudeUnusableResponse logs the upstream rate-limit headers and a bounded
// body prefix for a Claude 429/401, then restores the body so a final-attempt
// response can still be returned to the client intact.
func (t usageLimitRetryTransport) logClaudeUnusableResponse(response *http.Response, accountID string) {
	if t.logger == nil || response == nil {
		return
	}
	t.logger.Warn("claude account unusable upstream response",
		append([]any{
			"agent", t.agent,
			"session", t.session,
			"account", accountID,
			"method", t.method,
			"path", t.path,
			"upstream", t.upstream,
			"status", response.StatusCode,
		}, claudeRateLimitHeaderFields(response.Header)...)...)
	// Only the original hard rate-limit statuses (429/401) carry a known
	// rate-limit/auth error envelope worth logging. A rejected-header response on
	// any other status (a 200 served via overage, or an unrelated 4xx/5xx that
	// merely carries the header) may contain a real completion or request details,
	// so log headers only; the headers above already convey the rejected signal.
	if response.Body == nil || !claudeAccountUnusableStatus(response.StatusCode) {
		return
	}
	prefix, err := io.ReadAll(io.LimitReader(response.Body, usageLimitInspectMaxBytes+1))
	response.Body = prefixReadCloser{
		Reader: io.MultiReader(bytes.NewReader(prefix), response.Body),
		Closer: response.Body,
	}
	if err != nil {
		return
	}
	t.logger.Warn("claude account unusable upstream body",
		"agent", t.agent,
		"session", t.session,
		"account", accountID,
		"method", t.method,
		"path", t.path,
		"status", response.StatusCode,
		"body", string(prefix))
}

// logAntigravityUnusableResponse records only bounded, non-content metadata for
// Cloud Code 429/401 responses.  AGY's quota summary is not authoritative for
// a particular model/session allocation, so this makes the upstream reason
// observable without logging prompts or OAuth credentials.
func (t usageLimitRetryTransport) logAntigravityUnusableResponse(response *http.Response, accountID string) {
	if t.logger == nil || response == nil {
		return
	}
	fields := []any{
		"agent", t.agent,
		"session", t.session,
		"account", accountID,
		"method", t.method,
		"path", t.path,
		"upstream", t.upstream,
		"pool_model", t.poolModel,
		"status", response.StatusCode,
		"retry_after", response.Header.Get("Retry-After"),
	}
	if response.Body != nil {
		prefix, err := io.ReadAll(io.LimitReader(response.Body, usageLimitInspectMaxBytes+1))
		response.Body = prefixReadCloser{Reader: io.MultiReader(bytes.NewReader(prefix), response.Body), Closer: response.Body}
		if err == nil {
			message := normalizedProviderErrorMessage(prefix)
			if message != "" {
				fields = append(fields, "error_message", message)
			} else {
				fields = append(fields, "error_body", "non_json_or_missing_message")
			}
		}
	}
	t.logger.Warn("antigravity Cloud Code account unusable", fields...)
}

// isTerminalCredentialError reports whether an account refresh failed because
// its credential is dead and re-auth is required (so the account should be
// dropped from selection), as opposed to a transient or context failure.
func isTerminalCredentialError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var storedCodexFailure *accounts.CodexStoredRefreshFailureError
	if errors.As(err, &storedCodexFailure) {
		return true
	}
	var unisolatedCredential *accounts.CodexUnisolatedCredentialError
	if errors.As(err, &unisolatedCredential) {
		return true
	}
	var codexRefreshFailure *accounts.CodexAuthRefreshError
	if errors.As(err, &codexRefreshFailure) {
		return codexRefreshFailure.StatusCode == http.StatusUnauthorized ||
			codexRefreshFailure.ProviderCode == "refresh_token_reused" ||
			codexRefreshFailure.ProviderCode == "invalid_grant"
	}
	msg := strings.ToLower(err.Error())
	for _, terminal := range []string{
		"invalid_grant",
		"refresh_token_reused",
		"reauth required",
		"refresh token not found",
		"no refresh token",
		"has no refresh token",
		"no usable credential",
		// A stored credential that will not decode cannot be refreshed, so it
		// needs re-auth exactly like a rejected refresh token does. Treating it
		// as transient would retry the same unparseable blob forever instead of
		// failing over to an account that still works.
		"unreadable credential",
		"kimi credential for",
		"grok subscription credential was not found",
		"antigravity keychain credential is missing",
		"invalid_client",
		"unauthorized_client",
	} {
		if strings.Contains(msg, terminal) {
			return true
		}
	}
	return false
}

// rerouteModelIncompatibility picks the next failover candidate. Codex ChatGPT
// model compatibility is OAuth-only; Kimi plan/model capability applies to the
// selected account regardless of how that account's credential is represented.
func (s Server) rerouteModelIncompatibility(ctx context.Context, provider accounts.Provider, agentType, sessionID, userEmail, accountID, model string, tried map[string]struct{}) (accounts.Account, error) {
	if model != "" && s.SchedulerRef != nil {
		s.SchedulerRef.MarkModelIncompatible(provider, accountID, model)
	}
	if tried == nil {
		tried = make(map[string]struct{}, 1)
	}
	if accountID != "" {
		tried[accountID] = struct{}{}
	}
	// This runs inside the replay transport, so selection is provisional until
	// the replacement request returns 2xx. Persisting here would pin the session
	// to an account that may immediately reject or fail the replay.
	account, err := s.oauthRetryCandidate(ctx, provider, agentType, sessionID, userEmail, model, tried, provider == accounts.ProviderCodex)
	if err != nil && s.Logger != nil {
		s.Logger.Warn("model incompatibility has no alternate account",
			"provider", provider,
			"agent", agentType,
			"session", sessionID,
			"account", accountID,
			"model", model,
			"error", err)
	}
	return account, err
}

func (s Server) rerouteModelIncompatibilityForReconnect(ctx context.Context, provider accounts.Provider, agentType, sessionID, userEmail, accountID, model string) error {
	next, err := s.rerouteModelIncompatibility(ctx, provider, agentType, sessionID, userEmail, accountID, model, nil)
	if err != nil {
		return err
	}
	_, err = s.commitSessionReassignment(agentType, sessionID, accountID, next.ID, userEmail)
	return err
}

// oauthRetryCandidate selects and refreshes a candidate without changing the
// durable sticky assignment. In-request replay commits only after a 2xx
// response, while pre-request refresh callers return a pending-commit bit to
// their protocol-specific success boundary.
func (s Server) oauthRetryCandidate(ctx context.Context, provider accounts.Provider, agentType, sessionID, userEmail, poolModel string, tried map[string]struct{}, oauthOnly bool) (accounts.Account, error) {
	allCandidates := filterAccountsForProvider(s.accountListContext(ctx), provider)
	if len(allCandidates) == 0 {
		return accounts.Account{}, fmt.Errorf("no %s accounts available", provider)
	}
	s.refreshUsageScoresIfStale(ctx)
	// Loop so a single account with a dead OAuth token (refresh returns
	// invalid_grant) does NOT abort failover: skip it and try the next untried
	// candidate. Before this, one expired refresh token in the pool made the
	// caller log "no alternate account" and return the 429 to the client even
	// though healthy accounts remained untried.
	var lastErr error
	for {
		scheduler := s.scheduler().ForModel(poolModel)
		if s.Sessions != nil {
			scheduler = scheduler.WithSessionCounts(SchedulerSessionCounts(s.Sessions))
		}
		candidates := make([]accounts.Account, 0, len(allCandidates))
		for _, account := range allCandidates {
			if _, ok := tried[account.ID]; ok {
				continue
			}
			if oauthOnly && account.AuthMode != accounts.AuthModeOAuth {
				continue
			}
			candidates = append(candidates, account)
		}
		if len(candidates) == 0 {
			if lastErr != nil {
				return accounts.Account{}, lastErr
			}
			return accounts.Account{}, fmt.Errorf("no untried %s accounts available", provider)
		}
		account, err := pickRoutingAccount(scheduler, candidates)
		if err != nil {
			return accounts.Account{}, err
		}
		if account.AuthMode == accounts.AuthModeAPIKey {
			return account, nil
		}
		// Do not stop failover when the best untried account looks exhausted: scores
		// can be stale, so trying it (the retry loop is bounded by maxAttempts and
		// the tried set) is strictly better than refusing an account that may have
		// quota. A truly exhausted account just returns the upstream's own limit.
		if !scheduler.UsableForNewSession(schedulerAccountProvider(account.Provider), account.ID) && s.Logger != nil {
			s.Logger.Warn("usage-limit retry selecting OAuth account below new-session headroom; trying anyway",
				"provider", provider,
				"account", account.ID,
				"exhausted", scheduler.Exhausted(schedulerAccountProvider(account.Provider), account.ID),
				"threshold", selectacct.MinNewSessionHeadroom)
		}
		refreshed, err := s.refreshAccount(ctx, account)
		if err != nil {
			if ctx.Err() != nil {
				// The request itself was cancelled or timed out; abort failover
				// without poisoning the account's routing score.
				return accounts.Account{}, err
			}
			// Skip this account for the rest of THIS failover and try the next
			// untried candidate. Only poison the routing score for a terminal
			// credential failure (a dead/expired refresh token, invalid_grant); a
			// transient refresh failure (network blip, token-service hiccup,
			// keychain error) must not mark an otherwise-healthy account exhausted.
			tried[account.ID] = struct{}{}
			lastErr = err
			terminal := isTerminalCredentialError(err)
			if terminal {
				// Credential TTL, not the short default: a dead token only heals
				// via human re-auth, so frequent probes are pure overhead.
				s.markAccountExhaustedCredentialForAccount(account)
			}
			if s.Logger != nil {
				s.Logger.Warn("usage-limit retry skipping OAuth account with failed refresh",
					"provider", provider,
					"account", account.ID,
					"terminal", terminal,
					"error", err)
			}
			continue
		}
		return refreshed, nil
	}
}

func responseUsageLimit(response *http.Response) (bool, error) {
	if response == nil || response.Body == nil || !responseStatusCanExhaust(response.StatusCode) {
		return false, nil
	}
	body := response.Body
	prefix, err := io.ReadAll(io.LimitReader(body, usageLimitInspectMaxBytes+1))
	if err != nil {
		response.Body = prefixReadCloser{
			Reader: io.MultiReader(bytes.NewReader(prefix), body),
			Closer: body,
		}
		return false, err
	}
	if int64(len(prefix)) > usageLimitInspectMaxBytes {
		response.Body = prefixReadCloser{
			Reader: io.MultiReader(bytes.NewReader(prefix), body),
			Closer: body,
		}
		return false, nil
	}
	closeErr := body.Close()
	response.Body = io.NopCloser(bytes.NewReader(prefix))
	if closeErr != nil {
		return false, closeErr
	}
	return usageLimitJSON(prefix), nil
}

func responseCodexChatGPTModelUnsupported(response *http.Response) (bool, error) {
	if response == nil || response.Body == nil || response.StatusCode != http.StatusBadRequest {
		return false, nil
	}
	body := response.Body
	prefix, err := io.ReadAll(io.LimitReader(body, usageLimitInspectMaxBytes+1))
	if err != nil {
		response.Body = prefixReadCloser{
			Reader: io.MultiReader(bytes.NewReader(prefix), body),
			Closer: body,
		}
		return false, err
	}
	if int64(len(prefix)) > usageLimitInspectMaxBytes {
		response.Body = prefixReadCloser{
			Reader: io.MultiReader(bytes.NewReader(prefix), body),
			Closer: body,
		}
		return false, nil
	}
	closeErr := body.Close()
	response.Body = io.NopCloser(bytes.NewReader(prefix))
	if closeErr != nil {
		return false, closeErr
	}
	return codexChatGPTModelUnsupportedJSON(prefix), nil
}

func (t replayablePostRetryTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	maxAttempts := t.maxAttempts
	if maxAttempts < 1 {
		maxAttempts = 1
	}
	attemptReq := req
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		trace := newUploadAttemptTrace(attemptReq.ContentLength)
		response, err := t.roundTrip(trace.attach(attemptReq))
		retryStatus := err == nil && retryablePostUpstreamStatus(response)
		retryTransportErr := err != nil && retryablePostTransportError(err)
		if (!retryStatus && !retryTransportErr) || req.GetBody == nil || req.Context().Err() != nil || attempt == maxAttempts || !t.budget.consume() {
			// The last attempt's failure is what the client sees as a 502, so
			// record how the transport got there before giving up.
			if err != nil && t.logger != nil {
				t.logger.Error("replayable upstream request exhausted",
					append([]any{"agent", t.agent, "session", t.session, "account", t.account,
						"method", t.method, "path", t.path, "upstream", t.upstream,
						"attempts", attempt, "max_attempts", maxAttempts, "error", err},
						trace.attrs()...)...)
			}
			return response, err
		}
		body, bodyErr := req.GetBody()
		if bodyErr != nil {
			return response, err
		}
		if response != nil && response.Body != nil {
			_ = response.Body.Close()
		}
		// Deliberately do NOT call CloseIdleConnections here.
		//
		// It used to run on every retry, to avoid handing the next attempt
		// another connection the peer had already closed. But t.base is the one
		// transport shared by every session, so a single retry discarded the
		// entire pool for every host and every caller. Under failure that is a
		// feedback loop: failures cause retries, retries empty the pool, every
		// subsequent request has to dial, the extra dials exhaust the machine's
		// ephemeral ports, and the resulting dial failures cause more retries.
		// That is how cmux-mac-mini ran out of ports a second time, with
		// 33k sockets in TIME_WAIT against a 32k range.
		//
		// The staleness this guarded against is now prevented at the source:
		// IdleConnTimeout is held below the upstream's measured 15s idle close,
		// so a pooled connection is dropped by us before the peer drops it. Go's
		// transport also retires the specific connection that just errored, so
		// the next attempt will not reuse it.
		attemptReq = req.Clone(req.Context())
		attemptReq.Body = body
		attemptReq.GetBody = req.GetBody
		attemptReq.ContentLength = req.ContentLength
		if t.logger != nil {
			if retryStatus {
				t.logger.Warn("retrying replayable upstream request after upstream timeout status", "agent", t.agent, "session", t.session, "account", t.account, "method", t.method, "path", t.path, "upstream", t.upstream, "attempt", attempt+1, "max_attempts", maxAttempts, "status", response.StatusCode, "cf_ray", response.Header.Get("Cf-Ray"), "request_id", response.Header.Get("X-Request-ID"))
			} else {
				t.logger.Warn("retrying replayable upstream request after transport failure",
					append([]any{"agent", t.agent, "session", t.session, "account", t.account,
						"method", t.method, "path", t.path, "upstream", t.upstream,
						"attempt", attempt + 1, "max_attempts", maxAttempts, "error", err},
						trace.attrs()...)...)
			}
		}
		if !sleepForRetry(req.Context(), retryBackoff(attempt)) {
			return response, err
		}
	}
	return t.roundTrip(req)
}

// retryBackoff returns how long to wait before the attempt after n.
//
// Retries used to fire back to back, so all six attempts completed within
// microseconds. That is the worst possible strategy against an upstream that is
// briefly refusing connections: the whole retry budget burns before the far end
// recovers, and the client gets a 502. Backing off gives it time to come back,
// while the cap keeps the added latency bounded on a genuinely dead upstream.
func retryBackoff(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	backoff := retryBaseBackoff << (attempt - 1)
	if backoff > retryMaxBackoff {
		backoff = retryMaxBackoff
	}
	return backoff
}

const (
	retryBaseBackoff = 100 * time.Millisecond
	// Bounds the worst case: with six attempts the added latency stays near a
	// couple of seconds, far better than surfacing a 502 to the caller.
	retryMaxBackoff = 800 * time.Millisecond
)

// sleepForRetry waits for the backoff, reporting false if the caller gave up
// first so a client that has already hung up is not kept waiting.
func sleepForRetry(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return true
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}

func (t replayablePostRetryTransport) roundTrip(req *http.Request) (*http.Response, error) {
	if t.limiter == nil {
		return t.base.RoundTrip(req)
	}
	// Only genuinely large uploads contend for a slot. Zero means unknown as
	// well as empty (the client convention for a chunked body), so bypass
	// only on a known positive below-threshold length or a definitely-empty
	// body; unknown lengths are treated as large.
	if (req.ContentLength > 0 && req.ContentLength < replayablePostLimiterMinBytes) ||
		(req.ContentLength == 0 && (req.Body == nil || req.Body == http.NoBody)) {
		return t.base.RoundTrip(req)
	}
	select {
	case t.limiter <- struct{}{}:
		defer func() { <-t.limiter }()
	case <-req.Context().Done():
		return nil, req.Context().Err()
	}
	return t.base.RoundTrip(req)
}

func retryablePostUpstreamStatus(response *http.Response) bool {
	return response != nil && response.StatusCode == http.StatusRequestTimeout
}

func retryablePostTransportError(err error) bool {
	if err == nil {
		return false
	}
	message := err.Error()
	return strings.Contains(message, "broken pipe") ||
		strings.Contains(message, "use of closed network connection") ||
		strings.Contains(message, "tls: bad record MAC") ||
		strings.Contains(message, "connection reset by peer") ||
		strings.Contains(message, "unexpected EOF")
}

func (s Server) transport() http.RoundTripper {
	if s.Transport != nil {
		return s.Transport
	}
	return http.DefaultTransport
}

func NewOutboundTransport() *http.Transport {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	// Keep hostname-based upstream traffic on the established IPv4 path and use
	// pooled HTTP/1.1 connections. Literal IPv6 provider endpoints use tcp6 so
	// every address accepted by provider validation remains reachable without
	// changing ChatGPT's historically stable DNS/address-family behavior.
	// HTTP/2 multiplexing lets one upstream TLS failure tear down unrelated streams.
	dialer := &net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
	}
	transport.DialContext = func(ctx context.Context, _, addr string) (net.Conn, error) {
		return dialer.DialContext(ctx, outboundDialNetwork(addr), addr)
	}
	transport.ForceAttemptHTTP2 = false
	transport.TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{}
	if transport.TLSClientConfig == nil {
		transport.TLSClientConfig = &tls.Config{}
	} else {
		transport.TLSClientConfig = transport.TLSClientConfig.Clone()
	}
	transport.TLSClientConfig.NextProtos = []string{"http/1.1"}
	// Pool aggressively per host. HTTP/1.1 cannot multiplex, so each in-flight
	// request needs its own connection, and DefaultTransport only keeps
	// MaxIdleConnsPerHost (2) for reuse. Every concurrent request past the
	// second one therefore built a fresh TCP+TLS connection and discarded it,
	// leaving a socket in TIME_WAIT for 2*MSL. Because nearly all traffic goes
	// to a handful of hosts, that churn exhausted the machine's ephemeral port
	// range and upstream dials began failing with EADDRNOTAVAIL
	// ("can't assign requested address"), which surfaced to clients as 502.
	transport.MaxIdleConnsPerHost = outboundMaxIdleConnsPerHost
	transport.MaxIdleConns = outboundMaxIdleConns
	transport.IdleConnTimeout = outboundIdleConnTimeout
	transport.ResponseHeaderTimeout = outboundResponseHeaderTimeout
	return transport
}

func outboundDialNetwork(address string) string {
	host, _, err := net.SplitHostPort(address)
	if err == nil {
		if zone := strings.LastIndexByte(host, '%'); zone >= 0 {
			host = host[:zone]
		}
		if ip := net.ParseIP(host); ip != nil && ip.To4() == nil {
			return "tcp6"
		}
	}
	return "tcp4"
}

const (
	// Sized for many concurrent streaming requests against a small number of
	// upstream hosts, which is the shape of this proxy's traffic.
	outboundMaxIdleConnsPerHost = 256
	outboundMaxIdleConns        = 1024
	// upstreamIdleCloseAfter is how long our upstreams keep an idle connection
	// before closing it. Measured 2026-07-27 from the mac mini by opening a TLS
	// connection and waiting for EOF:
	//
	//	chatgpt.com:443       peer closed after 15s
	//	api.anthropic.com:443 peer closed after 15s
	//
	// Pooling a connection past this point guarantees handing out one the peer
	// already closed, which fails the next write with "use of closed network
	// connection" and surfaces to clients as 502 upstream request failed.
	upstreamIdleCloseAfter = 15 * time.Second
	// Comfortably below upstreamIdleCloseAfter so a pooled connection is always
	// dropped by us before the peer drops it. Still long enough to absorb the
	// bursts of concurrent requests that pooling exists to serve.
	outboundIdleConnTimeout = 10 * time.Second
	// The transport deliberately has no overall deadline (responses stream),
	// and dial and TLS are individually bounded — but the wait for response
	// headers was not: an upstream that accepted the request and then went
	// silent held the client request, its goroutine, and its upload-limiter
	// slot forever. Ten minutes is far above any observed legitimate
	// time-to-first-byte, including non-streaming Bedrock invokes of
	// long-thinking models, while turning a blackholed connection from a
	// permanent wedge into a retryable 502.
	outboundResponseHeaderTimeout = 10 * time.Minute
)

func (s Server) scheduler() selectacct.Scheduler {
	if s.SchedulerRef != nil {
		return s.SchedulerRef.Get().WithLiveDebits(s.SchedulerRef.LiveDebits())
	}
	return s.Scheduler
}

func findAccount(haystack []accounts.Account, id string) (accounts.Account, bool) {
	needle := strings.TrimSpace(id)
	for _, account := range haystack {
		if accountMatches(account, needle) {
			return account, true
		}
	}
	return accounts.Account{}, false
}

// sameProvider reports whether two providers refer to the same upstream,
// treating the empty provider as Codex (its historical default). Account-list
// mutations must compare provider too: a Codex account and a Claude account
// routinely share the same ID (a Codex email equals a Claude profile name), so
// matching by ID alone let one provider's refresh overwrite the other's entry,
// dropping the Codex account from selection entirely.
func sameProvider(a, b accounts.Provider) bool {
	if a == "" {
		a = accounts.ProviderCodex
	}
	if b == "" {
		b = accounts.ProviderCodex
	}
	return a == b
}

func sameCredentialProvider(a, b accounts.Provider) bool {
	if a == "" {
		a = accounts.ProviderCodex
	}
	if b == "" {
		b = accounts.ProviderCodex
	}
	return schedulerAccountProvider(a) == schedulerAccountProvider(b)
}

func accountMatches(account accounts.Account, id string) bool {
	if id == "" {
		return false
	}
	if strings.EqualFold(account.ID, id) || strings.EqualFold(account.Label, id) {
		return true
	}
	if account.AuthMode == accounts.AuthModeAPIKey && strings.EqualFold(strings.TrimPrefix(account.ID, "apikey:"), id) {
		return true
	}
	return false
}

func writeJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	_ = encoder.Encode(value)
}

func websocketResponseHeader(response *http.Response, key string) string {
	if response == nil {
		return ""
	}
	return response.Header.Get(key)
}

// outboundWebSocketDialer matches the TLS and dial behavior of
// NewOutboundTransport, which is the configuration our upstreams actually
// accept.
//
// websocket.DefaultDialer offers no ALPN and dials dual-stack. The plain HTTP
// path deliberately does neither: it pins http/1.1 and dials IPv4, because the
// upstream edge treats protocols differently. Probing the same URL showed a
// bot challenge over HTTP/2 versus a real origin response over HTTP/1.1.
//
// Left on the default dialer, a websocket upgrade to
// chatgpt.com/backend-api/codex/responses closed with EOF before any response
// headers arrived (no status, no Server, no Cf-Ray), which reached clients as
// "Connection reset without closing handshake".
func outboundWebSocketDialer() *websocket.Dialer {
	outboundWebSocketDialerOnce.Do(func() {
		dialer := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
		outboundWebSocketDialerValue = &websocket.Dialer{
			Proxy:            http.ProxyFromEnvironment,
			HandshakeTimeout: websocketHandshakeTimeout,
			NetDialContext: func(ctx context.Context, _, addr string) (net.Conn, error) {
				return dialer.DialContext(ctx, outboundDialNetwork(addr), addr)
			},
			// Pin http/1.1. A websocket upgrade cannot proceed over h2, and
			// advertising nothing lets the edge choose, which is what differs
			// from the working HTTP path.
			TLSClientConfig: &tls.Config{NextProtos: []string{"http/1.1"}},
		}
	})
	return outboundWebSocketDialerValue
}

// websocketHandshakeTimeout bounds the upgrade itself. The default dialer has
// no timeout, so a silent upstream could hang the handshake indefinitely.
const websocketHandshakeTimeout = 30 * time.Second

var (
	outboundWebSocketDialerOnce  sync.Once
	outboundWebSocketDialerValue *websocket.Dialer
)
