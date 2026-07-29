package proxy

import (
	"bytes"
	"context"
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

	"github.com/gorilla/websocket"
	"github.com/manaflow-ai/subrouter/internal/accounts"
	agentclaude "github.com/manaflow-ai/subrouter/internal/agents/claude"
	"github.com/manaflow-ai/subrouter/internal/broker"
	"github.com/manaflow-ai/subrouter/internal/transcript"
	"github.com/manaflow-ai/subrouter/selectacct"
	"github.com/manaflow-ai/subrouter/session"
)

type Server struct {
	Upstream       *url.URL
	CodexUpstream  *url.URL
	APIUpstream    *url.URL
	ClaudeUpstream *url.URL
	KimiUpstream   *url.URL
	ZAIUpstream    *url.URL
	Accounts       []accounts.Account
	AccountRef     *AccountRef
	Sessions       *session.Store
	Scheduler      selectacct.Scheduler
	SchedulerRef   *selectacct.SchedulerRef
	UsageScoreTTL  time.Duration
	ScoreAccounts  func(context.Context, []accounts.Account) ([]selectacct.Score, int)
	// RefreshAccountFn, when set, replaces the default OAuth refresh path. Test
	// seam for simulating dead/expired refresh tokens; nil in production.
	RefreshAccountFn func(context.Context, accounts.Account) (accounts.Account, error)
	// CredentialBroker selects a team account and returns an access-only,
	// short-lived lease. When configured, local refresh-token stores and the
	// local scheduler are bypassed entirely.
	CredentialBroker interface {
		Lease(context.Context, broker.LeaseRequest) (broker.Lease, error)
		Report(context.Context, string, broker.LeaseReport) error
	}
	Transport      http.RoundTripper
	Logger         *slog.Logger
	ActiveSessions *ActiveSessions
	// StreamDrops counts dropped response streams by which side ended them,
	// so the expected client-hangup case is countable without a log line each.
	StreamDrops *StreamDropStats
	Lifecycle   *Lifecycle
	AdminToken  string
	// LocalProxyToken protects provider proxy routes in cloud mode. Health and
	// readiness stay unauthenticated so supervisors can probe the daemon.
	LocalProxyToken string
	MaxBodyBytes    int64
	Transcripts     *transcript.Recorder
	ReadCache       *readCache
	// Bedrock, when set, enables the /bedrock/* SigV4 signing gateway.
	Bedrock *BedrockConfig
	// ClaudeFableAPIKey, when set, serves Claude Fable requests via this Anthropic
	// API key (x-api-key) instead of the subscription pool or Bedrock. It applies
	// ONLY to Fable; Opus/Sonnet/etc. continue to use the OAuth pool and never
	// touch this key.
	ClaudeFableAPIKey string
	// FableBedrockPrimary, when true, routes Claude Fable requests to AWS Bedrock
	// FIRST, before the subscription pool, instead of using Bedrock only as a
	// fallback. It only takes effect when the Bedrock gateway is configured; a
	// non-2xx Bedrock response (or an unreachable Bedrock) falls through to the
	// normal pool path, which keeps its own Bedrock/API-key fallback. Applies
	// ONLY to Fable; other Claude models are unaffected.
	FableBedrockPrimary bool
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
	draining  atomic.Bool
	active    atomic.Int64
}

func NewLifecycle() *Lifecycle {
	return &Lifecycle{startedAt: time.Now().UTC()}
}

func (l *Lifecycle) BeginProxyRequest() func() {
	if l == nil {
		return func() {}
	}
	l.active.Add(1)
	return func() {
		l.active.Add(-1)
	}
}

func (l *Lifecycle) Drain() {
	if l != nil {
		l.draining.Store(true)
	}
}

func (l *Lifecycle) Draining() bool {
	return l != nil && l.draining.Load()
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
			"draining":              false,
			"active_proxy_requests": int64(0),
		}
	}
	return map[string]any{
		"draining":              l.Draining(),
		"active_proxy_requests": l.ActiveProxyRequests(),
		"started_at":            l.startedAt.Format(time.RFC3339),
	}
}

type AccountRef struct {
	mu          sync.RWMutex
	accounts    []accounts.Account
	store       accounts.CodexStore
	claudeStore agentclaude.Store
	client      *http.Client

	usageStatusMu    sync.Mutex
	usageStatusCache []AccountUsageStatus
	usageStatusAt    time.Time
	lastGoodUsage    map[string]usageStatusSnapshot

	usageWindowsMu sync.Mutex
	usageWindows   map[string]usageWindowsEntry
}

type usageStatusSnapshot struct {
	status AccountUsageStatus
	at     time.Time
}

type usageWindowsEntry struct {
	windows []accounts.UsageWindow
	at      time.Time
}

const usageWindowsTTL = 2 * time.Minute
const usageWindowsLastGoodTTL = 15 * time.Minute

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
	windows, err := fetchAccountUsageWindowsLive(ctx, client, account)
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
	PlanType           string                           `json:"plan_type,omitempty"`
	Windows            []accounts.UsageWindow           `json:"windows,omitempty"`
	Credits            *accounts.CreditsInfo            `json:"credits,omitempty"`
	ComplimentaryReset *accounts.ComplimentaryResetInfo `json:"complimentary_reset,omitempty"`
	UsageFresh         bool                             `json:"-"`
}

func NewAccountRef(store accounts.CodexStore, initial []accounts.Account, client *http.Client) *AccountRef {
	return &AccountRef{
		accounts:    append([]accounts.Account(nil), initial...),
		store:       store,
		claudeStore: agentclaude.DefaultStore(),
		client:      client,
	}
}

func (r *AccountRef) All() []accounts.Account {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return append([]accounts.Account(nil), r.accounts...)
}

func (r *AccountRef) Reload() ([]accounts.Account, error) {
	if r == nil {
		return nil, nil
	}
	loaded, err := r.store.List()
	if err != nil {
		return nil, err
	}
	claudeAccounts, err := r.claudeStore.ListAccounts(context.Background())
	if err != nil {
		return nil, err
	}
	loaded = append(loaded, claudeAccounts...)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.accounts = append([]accounts.Account(nil), loaded...)
	return append([]accounts.Account(nil), loaded...), nil
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
			r.accounts[i] = next
			replaced = true
			break
		}
	}
	if !replaced {
		r.accounts = append(r.accounts, next)
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
		account, didRefresh, err := r.claudeStore.RefreshCredentialIfExpired(ctx, r.client, profile)
		if err != nil {
			status.AuthValid = false
			status.Error = err.Error()
			out = append(out, status)
			continue
		}
		if forceRefresh {
			account, didRefresh, err = r.forceRefreshClaude(ctx, profile)
			if err != nil {
				status.AuthValid = false
				status.Error = err.Error()
				out = append(out, status)
				continue
			}
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
	switch provider {
	case accounts.ProviderKimi:
		return "kimi api key"
	case accounts.ProviderZAI:
		return "zai api key"
	default:
		return "api key"
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
	out := make([]AccountUsageStatus, len(storedAccounts))
	var wg sync.WaitGroup
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
			status.PlanType = apiKeyPlanType(provider)
			out[i] = status
			continue
		}
		status.AuthMode = accounts.AuthModeOAuth
		status.AuthChecked = true
		out[i] = status
		wg.Add(1)
		go func() {
			defer wg.Done()
			refreshCtx := accounts.WithCodexRefreshReason(ctx, "usage-status.if-expired")
			refreshed, didRefresh, refreshErr := r.store.RefreshStoredIfExpired(refreshCtx, r.client, stored)
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
			details, err := accounts.FetchCodexUsageDetails(ctx, r.client, account)
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
	wg.Wait()
	claudeProfiles := r.claudeStore.ListProfiles()
	claudeOffset := len(out)
	out = append(out, make([]AccountUsageStatus, len(claudeProfiles))...)
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
			Active: profile.Name == activeClaude,
		}
		out[i] = status
		wg.Add(1)
		go func() {
			defer wg.Done()
			account, didRefresh, err := r.claudeStore.RefreshCredentialIfExpired(ctx, r.client, profile)
			next := out[i]
			next.Refreshed = didRefresh
			if err != nil {
				next.AuthValid = false
				next.Error = err.Error()
				out[i] = next
				return
			}
			next.AuthValid = true
			r.replace(account)
			windows, fresh, err := r.FetchUsageWindowsCached(ctx, r.client, account)
			if err != nil {
				next.Error = err.Error()
				out[i] = next
				return
			}
			next.PlanType = "claude"
			next.Windows = windows
			next.UsageFresh = fresh
			out[i] = next
		}()
	}
	wg.Wait()
	promoteUsableClaudeStatus(out[claudeOffset:])
	return out
}

func (r *AccountRef) forceRefreshClaude(ctx context.Context, profile agentclaude.Profile) (accounts.Account, bool, error) {
	configDir := r.claudeStore.ClaudeConfigDir(profile.Name)
	credential, err := r.claudeStore.ReadCredential(ctx, configDir)
	if err != nil {
		return accounts.Account{}, false, err
	}
	if credential == nil || credential.RefreshToken == "" {
		return accounts.Account{}, false, fmt.Errorf("Claude profile %q has no refresh token", profile.Name)
	}
	refreshed, err := agentclaude.RefreshCredential(ctx, r.client, *credential)
	if err != nil {
		return accounts.Account{}, false, err
	}
	if err := r.claudeStore.WriteCredential(ctx, configDir, refreshed); err != nil {
		return accounts.Account{}, false, err
	}
	return claudeAccountFromCredential(profile, configDir, &refreshed), true, nil
}

func claudeAccountFromCredential(profile agentclaude.Profile, configDir string, credential *agentclaude.CredentialInfo) accounts.Account {
	addedAt, _ := time.Parse(time.RFC3339, profile.CreatedAt)
	return accounts.Account{
		ID:       profile.Name,
		Provider: accounts.ProviderClaude,
		AuthMode: accounts.AuthModeOAuth,
		Label:    profile.Name,
		Email:    claudeProfileEmail(profile.Name),
		AddedAt:  addedAt,
		Token:    credential.AccessToken,
		Source:   configDir,
	}
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
			r.accounts[i] = account
			return
		}
	}
	r.accounts = append(r.accounts, account)
}

func (s Server) Handler() http.Handler {
	if s.ActiveSessions == nil {
		s.ActiveSessions = NewActiveSessions()
	}
	if s.Lifecycle == nil {
		s.Lifecycle = NewLifecycle()
	}
	if s.ReadCache == nil {
		s.ReadCache = newReadCache()
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/_subrouter/health", s.handleHealth)
	mux.HandleFunc("/_subrouter/ready", s.handleReady)
	mux.HandleFunc("/_subrouter/stream-stats", s.handleStreamStats)
	mux.HandleFunc("/_subrouter/drain", s.requireAdmin(s.handleDrain))
	mux.HandleFunc("/_subrouter/drain-status", s.requireAdmin(s.handleDrainStatus))
	mux.HandleFunc("/_subrouter/accounts", s.requireAdmin(s.handleAccounts))
	mux.HandleFunc("/_subrouter/account-status", s.requireAdmin(s.handleAccountStatus))
	mux.HandleFunc("/_subrouter/usage-status", s.requireAdmin(s.handleUsageStatus))
	mux.HandleFunc("/_subrouter/rate-limit-reset", s.requireAdmin(s.handleRateLimitReset))
	mux.HandleFunc("/_subrouter/reset-credits", s.requireAdmin(s.handleResetCredits))
	mux.HandleFunc("/_subrouter/reload-accounts", s.requireAdmin(s.handleReloadAccounts))
	mux.HandleFunc("/_subrouter/sessions", s.requireAdmin(s.handleSessions))
	mux.HandleFunc("/_subrouter/dashboard", s.requireAdmin(s.handleDashboard))
	mux.HandleFunc("/_subrouter/transcripts", s.requireAdmin(s.handleTranscriptList))
	mux.HandleFunc("/_subrouter/transcripts/", s.requireAdmin(s.handleTranscriptDetail))
	mux.HandleFunc("/_subrouter/bedrock-cost", s.requireAdmin(s.handleBedrockCost))
	mux.HandleFunc("/_subrouter/", http.NotFound)
	if s.Bedrock != nil {
		mux.Handle("/bedrock/", s.bedrockHandler())
	}
	mux.Handle("/", s.proxyHandler())
	return mux
}

func (s Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, map[string]any{"ok": true})
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

func (s Server) lifecycleStatus(ok bool) map[string]any {
	status := map[string]any{"ok": ok}
	for key, value := range s.Lifecycle.Status() {
		status[key] = value
	}
	return status
}

func (s Server) handleAccounts(w http.ResponseWriter, _ *http.Request) {
	type safeAccount struct {
		ID       string            `json:"id"`
		Provider accounts.Provider `json:"provider"`
		AuthMode accounts.AuthMode `json:"auth_mode"`
		Email    string            `json:"email,omitempty"`
		Source   string            `json:"source"`
	}
	availableAccounts := s.accountList()
	out := make([]safeAccount, 0, len(availableAccounts))
	for _, account := range availableAccounts {
		out = append(out, safeAccount{
			ID:       account.ID,
			Provider: account.Provider,
			AuthMode: account.AuthMode,
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
		writeJSON(w, s.AccountRef.Statuses(r.Context(), forceRefresh))
		return
	}
	accounts := s.accountList()
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
		statuses := s.AccountRef.UsageStatuses(r.Context())
		s.updateSchedulerFromUsageStatuses(statuses)
		writeJSON(w, s.withRequestTimeExhaustionWindows(statuses))
		return
	}
	accounts := s.accountList()
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
	writeJSON(w, s.withRequestTimeExhaustionWindows(out))
}

func (s Server) updateSchedulerFromUsageStatuses(statuses []AccountUsageStatus) {
	if s.SchedulerRef == nil {
		return
	}
	available := oauthAccounts(s.accountList())
	if len(available) == 0 {
		return
	}
	current := s.scheduler()
	scores := make([]selectacct.Score, 0, len(available))
	scoreByID := make(map[string]int, len(available))
	for _, account := range available {
		seed := current.ScoreFor(account.Provider, account.ID)
		seed.AccountID = account.ID
		seed.Provider = account.Provider
		seed.Fresh = false
		scoreByID[selectacct.ScoreKey(account.Provider, account.ID)] = len(scores)
		scores = append(scores, seed)
	}
	scored := 0
	for _, status := range statuses {
		if !status.UsageFresh || status.AuthMode != accounts.AuthModeOAuth || len(status.Windows) == 0 {
			continue
		}
		if idx, ok := scoreByID[selectacct.ScoreKey(status.Provider, status.ID)]; ok {
			next := scoreFromUsageWindows(status.Provider, status.ID, status.Windows)
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
		scheduler = scheduler.WithSessionCounts(s.Sessions.CountByAccount())
	}
	s.SchedulerRef.Set(scheduler)
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

func (s Server) reloadAccounts(ctx context.Context) (int, int, error) {
	if s.AccountRef == nil {
		return 0, 0, fmt.Errorf("account reload is not configured")
	}
	loaded, err := s.AccountRef.Reload()
	if err != nil {
		return 0, 0, err
	}
	if s.SchedulerRef == nil {
		return len(loaded), 0, nil
	}
	scores, scored := s.scoreAccounts(ctx, loaded)
	if scored == 0 {
		if s.Logger != nil {
			s.Logger.Warn("account reload usage score update skipped", "reason", "no fresh OAuth usage scores")
		}
		return len(loaded), scored, nil
	}
	scheduler := selectacct.NewScheduler(scores)
	if s.Sessions != nil {
		scheduler = scheduler.WithSessionCounts(s.Sessions.CountByAccount())
	}
	s.SchedulerRef.Set(scheduler)
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
			entry := ResetCreditsAccount{Email: account.Email}
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
	result := RateLimitResetResult{Email: account.Email, DryRun: dryRun}
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
	current := s.scheduler()
	scores := make([]selectacct.Score, 0, len(available))
	scoreByID := make(map[string]int, len(available))
	for _, account := range available {
		seed := current.ScoreFor(account.Provider, account.ID)
		seed.AccountID = account.ID
		seed.Provider = account.Provider
		seed.Fresh = false // carried forward until a fresh fetch overwrites it
		if account.AuthMode == accounts.AuthModeAPIKey {
			seed.Headroom = 0.01
			seed.ShortHeadroom = 0.01
		}
		scoreByID[selectacct.ScoreKey(account.Provider, account.ID)] = len(scores)
		scores = append(scores, seed)
	}

	client := (*http.Client)(nil)
	if s.AccountRef != nil {
		client = s.AccountRef.client
	}
	if client == nil {
		client = &http.Client{Timeout: defaultUsageFetchTimeout}
	}
	scored := 0
	for _, account := range available {
		if account.AuthMode != accounts.AuthModeOAuth {
			continue
		}
		refreshCtx := accounts.WithCodexRefreshReason(ctx, "proxy.score-accounts")
		refreshed, err := s.refreshAccount(refreshCtx, account)
		if err != nil {
			if s.Logger != nil {
				s.Logger.Warn("account reload refresh failed", "account", account.ID, "error", err)
			}
			// Only zero on a confident auth failure (dead/invalid token).
			// Transient refresh errors preserve the seed.
			if authLikeUsageError(err.Error()) {
				setZeroScore(scores, scoreByID, account.Provider, account.ID)
			}
			continue
		}
		windows, fresh, err := s.fetchAccountUsageWindows(ctx, client, refreshed)
		if err != nil {
			if s.Logger != nil {
				s.Logger.Warn("account reload usage fetch failed", "account", account.ID, "error", err)
			}
			// Auth failures mean the account is unusable; zero it so the
			// scheduler avoids it. Transient failures preserve the seed.
			if authLikeUsageError(err.Error()) {
				setZeroScore(scores, scoreByID, account.Provider, account.ID)
			}
			continue
		}
		if !fresh {
			// Stale last-known-good windows are not a confident signal. Keep
			// the seeded score rather than risk demoting a healthy account.
			continue
		}
		if idx, ok := scoreByID[selectacct.ScoreKey(account.Provider, account.ID)]; ok {
			next := scoreFromUsageWindows(account.Provider, account.ID, windows)
			next.Fresh = true
			scores[idx] = next
			scored++
		}
	}
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

func (s Server) authorizeAdmin(r *http.Request) bool {
	if isLoopbackRemote(r.RemoteAddr) {
		return true
	}
	token := strings.TrimSpace(s.AdminToken)
	if token == "" {
		return true
	}
	got := strings.TrimSpace(r.Header.Get("X-Subrouter-Admin-Token"))
	if got == "" {
		auth := strings.TrimSpace(r.Header.Get("Authorization"))
		if before, after, ok := strings.Cut(auth, " "); ok && strings.EqualFold(before, "Bearer") {
			got = strings.TrimSpace(after)
		}
	}
	if got == "" || len(got) != len(token) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(token)) == 1
}

func (s Server) handleSessions(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, s.Sessions.All())
}

func (s Server) proxyHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if baseURLProbeRequest(r) {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if !s.localProxyAuthorized(r) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		// Serve from cache for heavy read-only polling endpoints (e.g. plugins/installed).
		// Check before account selection so constrained accounts aren't hammered by polls.
		if r.Method == http.MethodGet {
			if cacheTTL := cacheablePath(r.URL.Path); cacheTTL > 0 {
				if entry, ok := s.ReadCache.get(r.URL.Path); ok {
					for k, vs := range entry.headers {
						for _, v := range vs {
							w.Header().Add(k, v)
						}
					}
					w.Header().Set("X-Subrouter-Cache", "HIT")
					w.WriteHeader(entry.statusCode)
					_, _ = w.Write(entry.body)
					return
				}
			}
		}

		agentType := session.ExtractAgentType(r)
		sessionID := session.ExtractID(r, s.MaxBodyBytes)
		if s.Lifecycle != nil && s.Lifecycle.Draining() && !s.allowDrainingProxyRequest(agentType, sessionID) {
			http.Error(w, "subrouter is draining", http.StatusServiceUnavailable)
			return
		}
		endProxyRequest := s.Lifecycle.BeginProxyRequest()
		defer endProxyRequest()
		requestProvider := providerForRequest(agentType, r.URL.Path)
		// Claude Fable routing order: subscription pool (Max accounts) first, then
		// AWS Bedrock, then the dedicated Anthropic API key. The fallback stages
		// run when the pool gives up (usageLimitRetryTransport exhausts failover)
		// or cannot start (no usable OAuth account). Other Claude models use the
		// normal pool unchanged.
		requestPoolModel := ""
		retryPoolModel := ""
		if requestProvider == accounts.ProviderCodex {
			retryPoolModel = session.ExtractModel(r, s.MaxBodyBytes)
		}
		fableFallbackConfigured := false
		if requestProvider == accounts.ProviderClaude {
			requestModel := session.ExtractModel(r, s.MaxBodyBytes)
			requestPoolModel = claudePoolModel(requestModel)
			retryPoolModel = requestPoolModel
			fableFallbackConfigured = s.CredentialBroker == nil &&
				s.claudeFableEnabled() && claudeFableModel(requestModel) &&
				r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/v1/messages")
		}
		// Bedrock-primary: when enabled, serve Fable straight from Bedrock before
		// touching the subscription pool. A non-2xx or unreachable Bedrock restores
		// the body and falls through to the normal pool path (which still carries
		// its own Bedrock/API-key fallback), so this never hard-fails Fable.
		if s.FableBedrockPrimary && fableFallbackConfigured && s.Bedrock != nil && s.Bedrock.configured() {
			if s.serveClaudeFableBedrockPrimary(w, r) {
				return
			}
		}
		sessionAgentType := agentTypeForProviderSession(agentType, requestProvider)
		userEmail := session.ExtractUserEmail(r)
		var account accounts.Account
		var credentialLease *broker.Lease
		var err error
		if s.CredentialBroker != nil {
			requiredAuthMode := accounts.AuthMode("")
			if requestProvider == accounts.ProviderCodex &&
				chatGPTBackendPath(r.URL.Path) {
				requiredAuthMode = accounts.AuthModeOAuth
			}
			lease, leaseErr := s.CredentialBroker.Lease(r.Context(), broker.LeaseRequest{
				Provider:         requestProvider,
				RequiredAuthMode: requiredAuthMode,
				AgentType:        sessionAgentType,
				SessionID:        sessionID,
				UserEmail:        userEmail,
				PreferAccountID:  session.ExtractAccountID(r),
				Model:            session.ExtractModel(r, s.MaxBodyBytes),
			})
			if leaseErr != nil {
				err = leaseErr
			} else {
				account = lease.Account
				credentialLease = &lease
			}
		} else {
			account, sessionID, userEmail, err = s.accountForSessionProvider(
				requestProvider,
				sessionAgentType,
				sessionID,
				r,
			)
			if err == nil {
				account, err = s.refreshSelectedAccount(
					r.Context(),
					requestProvider,
					sessionAgentType,
					sessionID,
					userEmail,
					r,
					account,
				)
				if err != nil {
					err = fmt.Errorf("refresh selected account: %w", err)
				}
			}
		}
		if err != nil {
			if fableFallbackConfigured && s.serveClaudeFableFallback(w, r) {
				return
			}
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}

		auth := account.AuthorizationHeader()
		if auth == "" {
			if fableFallbackConfigured && s.serveClaudeFableFallback(w, r) {
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

		setAccountAuthHeaders(r.Header, account)

		upstream := s.upstreamForRequest(r.URL.Path, account)
		if upstream == nil {
			http.Error(w, "no upstream configured", http.StatusServiceUnavailable)
			return
		}
		if websocket.IsWebSocketUpgrade(r) {
			s.proxyWebSocket(w, r, account, credentialLease, sessionAgentType, sessionID, userEmail, requestPoolModel, retryPoolModel, upstream)
			return
		}
		proxyRequest := r.Clone(r.Context())
		proxyRequest.URL = cloneURL(r.URL)
		proxyRequest.URL.Path = s.pathForUpstream(proxyRequest.URL.Path, account)
		proxyRequest.URL.RawPath = ""
		session.StripSubrouterHeaders(proxyRequest.Header)
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
		if retryPost && postReplayable &&
			(requestProvider == accounts.ProviderCodex || requestProvider == accounts.ProviderClaude) &&
			account.AuthMode == accounts.AuthModeOAuth &&
			s.CredentialBroker == nil {
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
				base:          transport,
				server:        &s,
				logger:        s.Logger,
				provider:      requestProvider,
				agent:         sessionAgentType,
				session:       sessionID,
				userEmail:     userEmail,
				account:       account.ID,
				method:        r.Method,
				path:          proxyRequest.URL.Path,
				upstream:      upstream.Host,
				maxAttempts:   s.usageLimitRetryMaxAttempts(requestProvider),
				poolModel:     retryPoolModel,
				fableFallback: fableFallback,
			}
		}
		if retryPost && postReplayable {
			transport = replayablePostRetryTransport{
				base:        transport,
				logger:      s.Logger,
				agent:       sessionAgentType,
				session:     sessionID,
				account:     account.ID,
				method:      r.Method,
				path:        proxyRequest.URL.Path,
				upstream:    upstream.Host,
				maxAttempts: replayablePostMaxAttempts,
				limiter:     replayablePostUploadLimiter,
			}
		}
		rp.Transport = transport
		rp.ModifyResponse = func(response *http.Response) error {
			s.captureResponseBody(response, r.Context(), sessionAgentType, sessionID, account.ID, account.Provider, requestPoolModel, retryPoolModel, proxyRequest.URL.Path)
			if credentialLease != nil {
				s.reportCredentialLease(
					credentialLease.ID,
					account.Provider,
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
			s.SchedulerRef.NoteRouted(requestProvider, account.ID)
		}
		if s.Logger != nil {
			// remote_addr + user_agent attribute each request to a source (tailnet
			// device / client type). Without them a concurrency spike is an
			// anonymous wall of user="" sessions that cannot be traced to whatever
			// fired it (a load-test loader, an agent fleet, etc.).
			s.Logger.Info("proxy request", "agent", sessionAgentType, "session", sessionID, "user", userEmail, "account", account.ID, "method", r.Method, "path", r.URL.Path, "upstream", upstream.Host, "remote_addr", clientRemoteIP(r), "user_agent", r.UserAgent())
		}

		// For cacheable GET paths, buffer the response so we can store it.
		if r.Method == http.MethodGet {
			if cacheTTL := cacheablePath(r.URL.Path); cacheTTL > 0 {
				rec := &cacheRecorder{}
				rp.ServeHTTP(rec, proxyRequest)
				if rec.code >= 200 && rec.code < 300 {
					s.ReadCache.set(r.URL.Path, rec.code, rec.header, rec.buf.Bytes(), cacheTTL)
				}
				for k, vs := range rec.header {
					for _, v := range vs {
						w.Header().Add(k, v)
					}
				}
				if rec.code != 0 {
					w.WriteHeader(rec.code)
				}
				_, _ = w.Write(rec.buf.Bytes())
				return
			}
		}

		rp.ServeHTTP(w, proxyRequest)
	})
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
	statusCode int,
	header http.Header,
) broker.LeaseReport {
	report := broker.LeaseReport{
		Outcome:    credentialLeaseOutcome(provider, statusCode, header),
		StatusCode: statusCode,
	}
	if report.Outcome == broker.LeaseRateLimited {
		report.CooldownScope = broker.LeaseCooldownAccount
		if provider == accounts.ProviderClaude &&
			(claudeRejectionIsModelPoolScoped(header) ||
				(statusCode == http.StatusTooManyRequests &&
					claudeUnifiedStatus(header) != "rejected")) {
			report.CooldownScope = broker.LeaseCooldownQuota
		}
		report.RetryAt = claudeExhaustionExpiry(header, time.Now())
		return report
	}
	if statusCode != http.StatusForbidden ||
		cloudflareChallengeResponse(header) {
		return report
	}
	report.Outcome = broker.LeaseForbidden
	report.CooldownScope = broker.LeaseCooldownQuota
	if provider == accounts.ProviderClaude {
		// Anthropic uses a bare 403 when OAuth is disabled for the account's
		// organization. Keep every model off that credential without refreshing
		// its still-valid chain. Rejected quota headers took precedence above.
		report.CooldownScope = broker.LeaseCooldownAccount
	}
	return report
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
	statusCode int,
	header http.Header,
) {
	if s.CredentialBroker == nil || leaseID == "" {
		return
	}
	report := credentialLeaseReport(provider, statusCode, header)
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

func (s Server) proxyWebSocket(w http.ResponseWriter, r *http.Request, account accounts.Account, credentialLease *broker.Lease, agentType, sessionID, userEmail, poolModel, compatibilityModel string, upstream *url.URL) {
	upstreamURL := cloneURL(r.URL)
	upstreamURL.Scheme = websocketScheme(upstream.Scheme)
	upstreamURL.Host = upstream.Host
	upstreamURL.Path = joinURLPath(upstream.Path, s.pathForUpstream(upstreamURL.Path, account))
	upstreamURL.RawPath = ""

	headers := r.Header.Clone()
	stripWebSocketDialHeaders(headers)
	session.StripSubrouterHeaders(headers)
	stripOutboundForwardingHeaders(headers)
	setAccountAuthHeaders(headers, account)
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

	upgrader := websocket.Upgrader{
		CheckOrigin:       func(_ *http.Request) bool { return true },
		EnableCompression: true,
	}
	responseHeader := http.Header{}
	if response != nil {
		responseHeader = cloneWebSocketResponseHeaders(response.Header)
	}
	clientConn, err := upgrader.Upgrade(w, r, responseHeader)
	if err != nil {
		return
	}
	defer clientConn.Close()

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
			statusCode,
			nil,
		)
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		s.copyWebSocketMessages(r.Context(), agentType, sessionID, userEmail, account.ID, poolModel, modelState, "client_to_upstream", clientConn, upstreamConn, nil)
		_ = upstreamConn.Close()
	}()
	go func() {
		defer wg.Done()
		s.copyWebSocketMessages(r.Context(), agentType, sessionID, userEmail, account.ID, poolModel, modelState, "upstream_to_client", upstreamConn, clientConn, reportLeaseFailure)
		_ = clientConn.Close()
	}()
	wg.Wait()
	if credentialLease != nil &&
		leaseFailureReported.CompareAndSwap(false, true) {
		s.reportCredentialLease(
			credentialLease.ID,
			account.Provider,
			http.StatusSwitchingProtocols,
			nil,
		)
	}
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

func (s Server) copyWebSocketMessages(ctx context.Context, agentType, sessionID, userEmail, accountID, poolModel string, modelState *webSocketModelState, direction string, src, dst *websocket.Conn, reportLeaseFailure func(int)) {
	provider := providerForRequest(agentType, "")
	for {
		messageType, body, err := src.ReadMessage()
		if err != nil {
			forwardWebSocketClose(dst, err)
			return
		}
		if s.Transcripts != nil {
			s.Transcripts.RecordPayload(agentType, sessionID, "websocket_message", direction, body, map[string]any{
				"opcode": websocketMessageType(messageType),
			})
		}
		if messageType == websocket.TextMessage && direction == "client_to_upstream" && provider == accounts.ProviderCodex {
			modelState.observe(body)
		}
		if direction == "upstream_to_client" && messageType == websocket.TextMessage {
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
					_, _ = s.rerouteModelIncompatibility(ctx, provider, agentType, sessionID, userEmail, accountID, model, nil)
				}
			}
			if provider == accounts.ProviderCodex && codexWebSocketResponseFinished(body) {
				modelState.complete()
			}
		}
		if err := dst.WriteMessage(messageType, body); err != nil {
			return
		}
	}
}

const webSocketCloseWriteTimeout = time.Second

// forwardWebSocketClose preserves the close handshake across the proxy. A raw
// TCP close makes the other peer report abnormal closure 1006 even when the
// first peer sent a valid WebSocket close frame. Unexpected transport loss is
// translated to 1011 so the proxy still terminates with a valid close frame.
func forwardWebSocketClose(dst *websocket.Conn, readErr error) {
	code := websocket.CloseInternalServerErr
	reason := "peer connection closed unexpectedly"
	var closeErr *websocket.CloseError
	if errors.As(readErr, &closeErr) && webSocketCloseCodeCanBeForwarded(closeErr.Code) {
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
		s.SchedulerRef.MarkExhausted(provider, accountID, poolKey)
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
	if s.SchedulerRef == nil {
		return
	}
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		// Dead/expired credential (401) or org-level OAuth disablement (403):
		// not a rate-limit window, no reset header exists, and neither
		// self-heals on a schedule. A longer TTL avoids probing every few
		// minutes while still picking the account back up within the hour
		// after a re-auth or an org re-enable.
		s.markAccountExhaustedCredential(provider, accountID, "")
		return
	}
	s.SchedulerRef.MarkExhaustedUntil(provider, accountID, poolKey, claudeExhaustionExpiry(header, time.Now()))
}

// credentialExhaustionTTL is how long an account with a dead credential
// (401 / invalid_grant) stays out of routing before one re-probe. Credentials
// only recover via human re-auth, so probes are pure overhead; but the mark
// must still lapse so a re-authed account rejoins without waiting for a
// successful usage refresh.
const credentialExhaustionTTL = time.Hour

func (s Server) markAccountExhaustedCredential(provider accounts.Provider, accountID, poolKey string) {
	if s.SchedulerRef == nil {
		return
	}
	s.SchedulerRef.MarkExhaustedUntil(provider, accountID, "", time.Now().Add(credentialExhaustionTTL))
}

// markAccountExhaustedRefreshFailure picks the mark TTL by failure class: a
// terminal credential error gets the long credential TTL, anything transient
// gets the short default so the account rejoins quickly.
func (s Server) markAccountExhaustedRefreshFailure(provider accounts.Provider, accountID, poolKey string, err error) {
	if isTerminalCredentialError(err) {
		s.markAccountExhaustedCredential(provider, accountID, "")
		return
	}
	s.markAccountExhausted(provider, accountID, "")
}

// claudeExhaustionExpiry picks when an exhaustion mark should lapse:
// anthropic-ratelimit-unified-reset (epoch seconds, the authoritative window
// reset) when present, else Retry-After seconds, else the scheduler default.
// Clamped to [now+1m, now+8d]: the floor guards clock skew / already-passed
// resets, the ceiling guards a nonsense far-future header pinning an account
// out forever.
func claudeExhaustionExpiry(header http.Header, now time.Time) time.Time {
	until := now.Add(selectacct.DefaultExhaustedTTL)
	if raw := strings.TrimSpace(claudeHeaderGet(header, "anthropic-ratelimit-unified-reset")); raw != "" {
		if epoch, err := strconv.ParseInt(raw, 10, 64); err == nil && epoch > 0 {
			until = time.Unix(epoch, 0)
		}
	} else if ra := strings.TrimSpace(claudeHeaderGet(header, "Retry-After")); ra != "" {
		if secs, err := strconv.Atoi(ra); err == nil && secs > 0 {
			until = now.Add(time.Duration(secs) * time.Second)
		}
	}
	if min := now.Add(time.Minute); until.Before(min) {
		return min
	}
	if max := now.Add(8 * 24 * time.Hour); until.After(max) {
		return max
	}
	return until
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

func (s Server) recordHTTPMeta(r *http.Request, agentType, sessionID, userEmail string, account accounts.Account, upstream *url.URL) {
	if s.Transcripts == nil {
		return
	}
	s.Transcripts.RecordMeta(agentType, sessionID, map[string]any{
		"transport": "http",
		"user":      userEmail,
		"account":   account.ID,
		"method":    r.Method,
		"path":      r.URL.Path,
		"upstream":  upstream.String(),
		"headers":   transcript.RedactedHeaders(r.Header),
	})
}

func (s Server) recordWebSocketMeta(r *http.Request, upstreamURL *url.URL, headers http.Header, agentType, sessionID, userEmail string, account accounts.Account, upstream *url.URL) {
	if s.Transcripts == nil {
		return
	}
	s.Transcripts.RecordMeta(agentType, sessionID, map[string]any{
		"transport":    "websocket",
		"user":         userEmail,
		"account":      account.ID,
		"method":       r.Method,
		"path":         r.URL.Path,
		"upstream":     upstream.String(),
		"upstream_url": upstreamURL.String(),
		"headers":      transcript.RedactedHeaders(headers),
	})
}

func (s Server) captureRequestBody(r *http.Request, agentType, sessionID string) {
	if s.Transcripts == nil || r.Body == nil {
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
	if s.Transcripts == nil || r.GetBody == nil || !s.Transcripts.Enabled() {
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
	if provider == "" {
		provider = accounts.ProviderCodex
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
			s.markAccountExhaustedFromResponse(provider, accountID, poolModel, response.StatusCode, response.Header)
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
	inspectModelCompatibility := s.SchedulerRef != nil && accountID != "" && compatibilityModel != "" &&
		provider == accounts.ProviderCodex && response.StatusCode == http.StatusBadRequest
	if response.Body == nil || (s.Transcripts == nil && s.Logger == nil && !inspectUsageLimit && !inspectModelCompatibility && !claudeUnusable) {
		return
	}
	payload := map[string]any{"status": response.StatusCode}
	responseCtx := context.Background()
	if response.Request != nil {
		responseCtx = response.Request.Context()
	}
	var inspect func([]byte)
	if inspectUsageLimit || inspectModelCompatibility || claudeUnusable {
		loggedBody := false
		inspect = func(body []byte) {
			if inspectUsageLimit && usageLimitJSON(body) {
				// Use the response's headers so a header-derived reset expiry set
				// above is recomputed identically, not overwritten with the short
				// default TTL.
				s.markAccountExhaustedFromResponse(provider, accountID, poolModel, response.StatusCode, response.Header)
			}
			if inspectModelCompatibility && codexChatGPTModelUnsupportedJSON(body) {
				_, _ = s.rerouteModelIncompatibility(responseCtx, provider, agentType, sessionID, "", accountID, compatibilityModel, nil)
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
	response.Body = newStreamingTranscriptReadCloser(streamingTranscriptConfig{
		ReadCloser: response.Body,
		Recorder:   s.Transcripts,
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

func setAccountAuthHeaders(headers http.Header, account accounts.Account) {
	headers.Set("Authorization", account.AuthorizationHeader())
	headers.Del("ChatGPT-Account-ID")
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
	case accounts.ProviderKimi:
		headers.Set("Authorization", account.AuthorizationHeader())
		headers.Set("X-Api-Key", account.Token)
		if headers.Get("Anthropic-Version") == "" {
			headers.Set("Anthropic-Version", "2023-06-01")
		}
	case accounts.ProviderZAI:
		headers.Del("X-Api-Key")
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
	if account.Provider == accounts.ProviderKimi {
		return s.KimiUpstream
	}
	if account.Provider == accounts.ProviderZAI {
		return s.ZAIUpstream
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
	if account.Provider == accounts.ProviderKimi {
		return stripProviderPathPrefix(path, "kimi")
	}
	if account.Provider == accounts.ProviderZAI {
		return stripProviderPathPrefix(path, "zai")
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
	userEmail := session.ExtractUserEmail(r)
	forcedAccountID := session.ExtractAccountID(r)
	model := session.ExtractModel(r, s.MaxBodyBytes)
	availableAccounts := filterAccountsForProvider(s.accountList(), provider)
	if forcedAccountID != "" {
		account, ok := findAccount(availableAccounts, forcedAccountID)
		if !ok {
			return accounts.Account{}, sessionID, userEmail, fmt.Errorf("requested account %q not found", forcedAccountID)
		}
		if provider == accounts.ProviderCodex && chatGPTBackendPath(r.URL.Path) && account.AuthMode != accounts.AuthModeOAuth {
			return accounts.Account{}, sessionID, userEmail, fmt.Errorf("requested account %q cannot be used for ChatGPT backend paths", forcedAccountID)
		}
		assignment, err := s.Sessions.Put(agentType, sessionID, account.ID, userEmail)
		if err != nil {
			return accounts.Account{}, sessionID, userEmail, err
		}
		return account, sessionID, assignment.UserEmail, nil
	}
	if provider == accounts.ProviderCodex && chatGPTBackendPath(r.URL.Path) {
		availableAccounts = oauthAccounts(availableAccounts)
	}
	if provider == accounts.ProviderClaude && s.claudeFableEnabled() && claudeFableModel(model) {
		// Fable order is subscription pool -> Bedrock -> dedicated API key, so
		// metered API-key pool accounts never preempt the Bedrock stage. With no
		// OAuth account usable, selection fails and the handler serves the
		// fallback chain directly.
		availableAccounts = oauthAccounts(availableAccounts)
	}
	if provider == accounts.ProviderCodex || provider == accounts.ProviderClaude {
		s.refreshUsageScoresIfStale(r.Context())
	}
	base := s.scheduler()
	poolModel := model
	if provider == accounts.ProviderClaude {
		poolModel = claudePoolModel(model)
	}
	if poolModel != "" && s.Logger != nil && base.HasModelPool(poolModel) {
		s.Logger.Info("model quota pool matched", "agent", agentType, "model", model, "pool", selectacct.ModelKey(poolModel))
	}
	scheduler := base.ForModel(poolModel).WithSessionCounts(s.Sessions.CountByAccount())
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
				return account, sessionID, userEmail, nil
			}
			if s.Logger != nil {
				s.Logger.Info("rerouting cold sticky session from constrained account",
					"agent", agentType,
					"session", sessionID,
					"account", account.ID,
					"active", s.activeSession(agentType, sessionID),
					"usable_for_new_session", scheduler.UsableForNewSession(account.Provider, account.ID),
					"exhausted", scheduler.Exhausted(account.Provider, account.ID),
				)
			}
		}
	}

	account, err := scheduler.Pick(availableAccounts)
	if err != nil {
		return accounts.Account{}, sessionID, userEmail, err
	}
	if account.AuthMode == accounts.AuthModeOAuth && provider == accounts.ProviderClaude && scheduler.Exhausted(account.Provider, account.ID) {
		return accounts.Account{}, sessionID, userEmail, fmt.Errorf("no non-exhausted %s accounts available", provider)
	}
	if account.AuthMode == accounts.AuthModeOAuth && !scheduler.UsableForNewSession(account.Provider, account.ID) && s.Logger != nil {
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
			"exhausted", scheduler.Exhausted(account.Provider, account.ID),
			"threshold", selectacct.MinNewSessionHeadroom)
	}
	assignment, err := s.Sessions.Put(agentType, sessionID, account.ID, userEmail)
	if err != nil {
		return accounts.Account{}, sessionID, userEmail, err
	}
	return account, sessionID, assignment.UserEmail, nil
}

func (s Server) logStickyReuse(agentType, sessionID string, account accounts.Account, scheduler selectacct.Scheduler) {
	if s.Logger == nil || accountProviderOrCodex(account) != accounts.ProviderCodex || account.AuthMode != accounts.AuthModeOAuth {
		return
	}
	if scheduler.UsableForNewSession(account.Provider, account.ID) || scheduler.Exhausted(account.Provider, account.ID) || !s.activeSession(agentType, sessionID) {
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

func (s Server) reuseStickyAssignment(agentType, sessionID string, account accounts.Account, scheduler selectacct.Scheduler) bool {
	if scheduler.Exhausted(account.Provider, account.ID) {
		return false
	}
	if s.activeSession(agentType, sessionID) {
		return true
	}
	if accountProviderOrCodex(account) == accounts.ProviderCodex && account.AuthMode == accounts.AuthModeOAuth {
		return scheduler.UsableForNewSession(account.Provider, account.ID)
	}
	return true
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
	if s.SchedulerRef == nil || !s.SchedulerRef.BeginRefreshIfStale(s.UsageScoreTTL) {
		return
	}
	availableAccounts := oauthAccounts(s.accountList())
	scoreAccounts := s.ScoreAccounts
	if scoreAccounts == nil {
		scoreAccounts = s.scoreAccounts
	}
	scores, scored := scoreAccounts(ctx, availableAccounts)
	if scored == 0 {
		s.SchedulerRef.FinishRefresh(selectacct.Scheduler{}, false)
		if s.Logger != nil {
			s.Logger.Warn("usage score refresh skipped", "reason", "no fresh OAuth usage scores")
		}
		return
	}
	scheduler := selectacct.NewScheduler(scores)
	if s.Sessions != nil {
		scheduler = scheduler.WithSessionCounts(s.Sessions.CountByAccount())
	}
	s.SchedulerRef.FinishRefresh(scheduler, true)
	if s.Logger != nil {
		s.Logger.Debug("usage scores refreshed before account selection", "accounts", len(availableAccounts), "scored", scored)
	}
}

func chatGPTBackendPath(path string) bool {
	_, ok := stripChatGPTBackendPath(path)
	return ok
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
	switch provider {
	case accounts.ProviderKimi, accounts.ProviderZAI:
		return string(provider)
	default:
		return agentType
	}
}

func providerForPath(path string) (accounts.Provider, bool) {
	switch firstPathSegment(path) {
	case "kimi":
		return accounts.ProviderKimi, true
	case "zai":
		return accounts.ProviderZAI, true
	default:
		return "", false
	}
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
	filtered := make([]accounts.Account, 0, len(all))
	legacy := make([]accounts.Account, 0)
	for _, account := range all {
		if account.Provider == provider {
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
	if provider == accounts.ProviderKimi || provider == accounts.ProviderZAI {
		return nil
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
	out := append([]accounts.Account(nil), s.Accounts...)
	if s.AccountRef != nil {
		out = append(out, s.AccountRef.All()...)
	}
	return out
}

func (s Server) refreshAccount(ctx context.Context, account accounts.Account) (accounts.Account, error) {
	if s.RefreshAccountFn != nil {
		return s.RefreshAccountFn(ctx, account)
	}
	if s.AccountRef == nil {
		return account, nil
	}
	return s.AccountRef.Refresh(ctx, account)
}

func (s Server) refreshSelectedAccount(ctx context.Context, provider accounts.Provider, agentType, sessionID, userEmail string, r *http.Request, account accounts.Account) (accounts.Account, error) {
	refreshed, err := s.refreshAccount(ctx, account)
	if err == nil {
		return refreshed, nil
	}
	if provider != accounts.ProviderClaude || session.ExtractAccountID(r) != "" {
		return account, err
	}
	if s.Logger != nil {
		s.Logger.Warn("selected Claude account refresh failed, trying another account", "account", account.ID, "error", err)
	}
	tried := map[string]struct{}{account.ID: {}}
	s.markAccountExhaustedRefreshFailure(provider, account.ID, "", err)
	lastErr := err
	for {
		next, pickErr := s.retryAccount(ctx, provider, agentType, sessionID, userEmail, tried)
		if pickErr != nil {
			return account, lastErr
		}
		tried[next.ID] = struct{}{}
		refreshed, err = s.refreshAccount(ctx, next)
		if err == nil {
			return refreshed, nil
		}
		lastErr = err
		s.markAccountExhaustedRefreshFailure(provider, next.ID, "", err)
		if s.Logger != nil {
			s.Logger.Warn("retry Claude account refresh failed", "account", next.ID, "error", err)
		}
	}
}

func (s Server) retryAccount(ctx context.Context, provider accounts.Provider, agentType, sessionID, userEmail string, tried map[string]struct{}) (accounts.Account, error) {
	candidates := filterAccountsForProvider(s.accountList(), provider)
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
		scheduler = scheduler.WithSessionCounts(s.Sessions.CountByAccount())
	}
	account, err := scheduler.Pick(untried)
	if err != nil {
		return accounts.Account{}, err
	}
	if scheduler.Exhausted(account.Provider, account.ID) {
		return accounts.Account{}, fmt.Errorf("no non-exhausted %s accounts available", provider)
	}
	if s.Sessions != nil {
		if _, err := s.Sessions.Put(agentType, sessionID, account.ID, userEmail); err != nil {
			return accounts.Account{}, err
		}
	}
	return account, nil
}

const replayablePostMaxBodyBytes = 128 << 20
const replayablePostMaxAttempts = 6

func (s Server) usageLimitRetryMaxAttempts(provider accounts.Provider) int {
	count := len(filterAccountsForProvider(s.accountList(), provider))
	if count > replayablePostMaxAttempts {
		return count
	}
	return replayablePostMaxAttempts
}

const replayablePostMaxConcurrentUploads = 4

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
	if provider == accounts.ProviderClaude {
		if r == nil || r.Method != http.MethodPost {
			return false
		}
		path := r.URL.Path
		return path == "/v1/messages" || path == "/messages"
	}
	return retryableResponsesPostRequest(r)
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
}

type usageLimitRetryTransport struct {
	base        http.RoundTripper
	server      *Server
	logger      *slog.Logger
	provider    accounts.Provider
	agent       string
	session     string
	userEmail   string
	account     string
	method      string
	path        string
	upstream    string
	maxAttempts int
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
	return fallback, true
}

// claudeOverloadMaxRetries bounds the same-account retries subrouter itself
// performs on an Anthropic 5xx/529 overload before passing the error through to
// the client (which retries further with its own backoff). Small on purpose:
// subrouter absorbs brief blips without stacking long waits on top of the
// client's retry budget or amplifying load during a sustained outage.
const claudeOverloadMaxRetries = 2

// claudeOverloadMaxWait caps a single overload backoff wait, including one
// requested via Retry-After, so a pathological header cannot hold a proxied
// request hostage.
const claudeOverloadMaxWait = 10 * time.Second

// claudeOverloadStatus reports whether the upstream response is an
// Anthropic-side server failure (529 overloaded_error or another 5xx) worth
// retrying on the SAME account: it is not account-specific, so rotating
// accounts cannot help and would only burn the failover budget.
func claudeOverloadStatus(code int) bool {
	return code >= 500 && code <= 599
}

// claudeOverloadBackoff picks the wait before an overload retry: Retry-After
// when the upstream sent one (capped), else 1s << retry (1s, 2s, ...).
func claudeOverloadBackoff(header http.Header, retry int) time.Duration {
	if ra := strings.TrimSpace(claudeHeaderGet(header, "Retry-After")); ra != "" {
		if secs, err := strconv.Atoi(ra); err == nil && secs > 0 {
			wait := time.Duration(secs) * time.Second
			if wait > claudeOverloadMaxWait {
				return claudeOverloadMaxWait
			}
			return wait
		}
	}
	wait := time.Second << retry
	if wait > claudeOverloadMaxWait {
		return claudeOverloadMaxWait
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

// responseUsageLimited reports whether the upstream response means the current
// account is out of quota. Anthropic signals subscription exhaustion with a
// plain 429 (no codex-style "usage_limit_reached" body), so claude is detected
// by status alone.
func (t usageLimitRetryTransport) responseUsageLimited(response *http.Response) (bool, error) {
	if t.provider == accounts.ProviderClaude {
		if response == nil {
			return false, nil
		}
		// Fail over on a hard status (429/401) OR on the authoritative
		// out-of-quota header even when the upstream answered 200 via overage.
		// Anthropic serves a "rejected" account through paid overage and returns
		// 200, but Claude Code reads anthropic-ratelimit-unified-status=rejected
		// and hard-blocks the user, so a 200 from a rejected account is unusable
		// from the client's view and must be rerouted to a healthy account.
		return claudeAccountUnusableStatus(response.StatusCode) || claudeResponseRejected(response.Header), nil
	}
	return responseUsageLimit(response)
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
	tried := map[string]struct{}{}
	if accountID != "" {
		tried[accountID] = struct{}{}
	}
	overloadRetries := 0
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		response, err := base.RoundTrip(attemptReq)
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
		if t.provider == accounts.ProviderClaude && claudeOverloadStatus(response.StatusCode) && !claudeResponseRejected(response.Header) {
			if overloadRetries >= claudeOverloadMaxRetries {
				if fallback, ok := t.fableFallbackResponse(response, accountID, "overload"); ok {
					return fallback, nil
				}
				return response, nil
			}
			wait := claudeOverloadBackoff(response.Header, overloadRetries)
			overloadRetries++
			if t.logger != nil {
				t.logger.Warn("retrying after anthropic overload", "agent", t.agent, "session", t.session, "account", accountID, "method", t.method, "path", t.path, "upstream", t.upstream, "status", response.StatusCode, "wait", wait.String(), "overload_retry", overloadRetries, "max_overload_retries", claudeOverloadMaxRetries)
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
			attempt-- // overload retries do not consume the account-failover budget
			continue
		}
		usageLimited, inspectErr := t.responseUsageLimited(response)
		if inspectErr != nil {
			if t.logger != nil {
				t.logger.Warn("usage-limit response inspection failed", "agent", t.agent, "session", t.session, "account", accountID, "method", t.method, "path", t.path, "upstream", t.upstream, "error", inspectErr)
			}
			return response, nil
		}
		modelUnsupported := false
		if !usageLimited && t.provider == accounts.ProviderCodex {
			modelUnsupported, inspectErr = responseCodexChatGPTModelUnsupported(response)
			if inspectErr != nil {
				if t.logger != nil {
					t.logger.Warn("codex model compatibility response inspection failed", "agent", t.agent, "session", t.session, "account", accountID, "method", t.method, "path", t.path, "upstream", t.upstream, "error", inspectErr)
				}
				return response, nil
			}
		}
		if !usageLimited && !modelUnsupported {
			return response, nil
		}
		exhausted := true
		exhaustionPool := selectacct.ModelKey(t.poolModel)
		if t.provider == accounts.ProviderCodex && usageLimited {
			// Codex usage_limit_reached exhausts the subscription account, not
			// only the model named by this request. Model compatibility errors
			// below remain scoped to the rejected model.
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
		}
		var compatibilityNext accounts.Account
		var compatibilityPickErr error
		if modelUnsupported && t.server != nil {
			compatibilityNext, compatibilityPickErr = t.server.rerouteModelIncompatibility(
				req.Context(), t.provider, t.agent, t.session, t.userEmail, accountID, exhaustionPool, tried,
			)
		}
		if t.server != nil && exhausted && !modelUnsupported {
			// Use the response's own reset time so the mark self-expires when the
			// window recovers (codex responses lack these headers and fall back
			// to the default TTL inside claudeExhaustionExpiry).
			t.server.markAccountExhaustedFromResponse(t.provider, accountID, exhaustionPool, response.StatusCode, response.Header)
		}
		if attempt == maxAttempts || t.server == nil {
			reason := "max_attempts"
			if t.server == nil {
				reason = "no_server"
			}
			if fallback, ok := t.fableFallbackResponse(response, accountID, reason); ok {
				return fallback, nil
			}
			t.logClaudeFailoverExhausted(response, accountID, reason, attempt, maxAttempts, len(tried))
			return response, nil
		}
		nextAccount, pickErr := compatibilityNext, compatibilityPickErr
		if !modelUnsupported {
			nextAccount, pickErr = t.server.oauthRetryAccount(req.Context(), t.provider, t.agent, t.session, t.userEmail, t.poolModel, tried, t.fableFallback != nil)
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
		if response.Body != nil {
			_ = response.Body.Close()
		}
		previousAccount := accountID
		accountID = nextAccount.ID
		tried[accountID] = struct{}{}
		if t.server != nil && t.server.SchedulerRef != nil {
			t.server.SchedulerRef.NoteRouted(t.provider, accountID)
		}
		attemptReq = req.Clone(req.Context())
		attemptReq.Body = body
		attemptReq.GetBody = req.GetBody
		attemptReq.ContentLength = req.ContentLength
		setAccountAuthHeaders(attemptReq.Header, nextAccount)
		if t.logger != nil {
			t.logger.Warn("retrying replayable upstream request after usage limit", "agent", t.agent, "session", t.session, "previous_account", previousAccount, "account", accountID, "method", t.method, "path", t.path, "upstream", t.upstream, "attempt", attempt+1, "max_attempts", maxAttempts)
		}
	}
	return base.RoundTrip(req)
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
	msg := strings.ToLower(err.Error())
	for _, terminal := range []string{
		"invalid_grant",
		"refresh token not found",
		"no refresh token",
		"has no refresh token",
		"no usable credential",
		"invalid_client",
		"unauthorized_client",
	} {
		if strings.Contains(msg, terminal) {
			return true
		}
	}
	return false
}

// oauthRetryAccount picks the next failover candidate. oauthOnly restricts the
// pool to OAuth accounts; Fable requests with a fallback chain set it so a
// metered API-key pool account never preempts the Bedrock stage (the dedicated
// Fable API key is the chain's own last stage).
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
	account, err := s.oauthRetryAccount(ctx, provider, agentType, sessionID, userEmail, model, tried, true)
	if err != nil && s.Logger != nil {
		s.Logger.Warn("model incompatibility has no alternate OAuth account",
			"provider", provider,
			"agent", agentType,
			"session", sessionID,
			"account", accountID,
			"model", model,
			"error", err)
	}
	return account, err
}

func (s Server) oauthRetryAccount(ctx context.Context, provider accounts.Provider, agentType, sessionID, userEmail, poolModel string, tried map[string]struct{}, oauthOnly bool) (accounts.Account, error) {
	allCandidates := filterAccountsForProvider(s.accountList(), provider)
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
			scheduler = scheduler.WithSessionCounts(s.Sessions.CountByAccount())
		}
		candidates := make([]accounts.Account, 0, len(allCandidates))
		for _, account := range allCandidates {
			if _, ok := tried[account.ID]; ok {
				continue
			}
			if oauthOnly && account.AuthMode != accounts.AuthModeOAuth {
				continue
			}
			if account.AuthMode == accounts.AuthModeOAuth && scheduler.Exhausted(account.Provider, account.ID) {
				continue
			}
			candidates = append(candidates, account)
		}
		if len(candidates) == 0 {
			if lastErr != nil {
				return accounts.Account{}, lastErr
			}
			return accounts.Account{}, fmt.Errorf("no untried non-exhausted %s accounts available", provider)
		}
		account, err := scheduler.Pick(candidates)
		if err != nil {
			return accounts.Account{}, err
		}
		if account.AuthMode == accounts.AuthModeAPIKey {
			if s.Sessions != nil {
				if _, err := s.Sessions.Put(agentType, sessionID, account.ID, userEmail); err != nil {
					return accounts.Account{}, err
				}
			}
			return account, nil
		}
		// Do not stop failover when the best untried account looks exhausted: scores
		// can be stale, so trying it (the retry loop is bounded by maxAttempts and
		// the tried set) is strictly better than refusing an account that may have
		// quota. A truly exhausted account just returns the upstream's own limit.
		if !scheduler.UsableForNewSession(account.Provider, account.ID) && s.Logger != nil {
			s.Logger.Warn("usage-limit retry selecting OAuth account below new-session headroom; trying anyway",
				"provider", provider,
				"account", account.ID,
				"exhausted", scheduler.Exhausted(account.Provider, account.ID),
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
				s.markAccountExhaustedCredential(provider, account.ID, "")
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
		if s.Sessions != nil {
			if _, err := s.Sessions.Put(agentType, sessionID, refreshed.ID, userEmail); err != nil {
				return accounts.Account{}, err
			}
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
		if (!retryStatus && !retryTransportErr) || req.GetBody == nil || req.Context().Err() != nil || attempt == maxAttempts {
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
	// Keep ChatGPT traffic on IPv4 and pooled HTTP/1.1 connections. HTTP/2
	// multiplexing lets one upstream TLS failure tear down unrelated streams.
	dialer := &net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
	}
	transport.DialContext = func(ctx context.Context, _, addr string) (net.Conn, error) {
		return dialer.DialContext(ctx, "tcp4", addr)
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
	return transport
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

func accountMatches(account accounts.Account, id string) bool {
	if id == "" {
		return false
	}
	if strings.EqualFold(account.ID, id) || strings.EqualFold(account.Label, id) || strings.EqualFold(account.Email, id) {
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
				return dialer.DialContext(ctx, "tcp4", addr)
			},
			// Pin http/1.1. A websocket upgrade cannot proceed over h2, and
			// advertising nothing lets the edge choose, which is what differs
			// from the working HTTP path.
			TLSClientConfig:   &tls.Config{NextProtos: []string{"http/1.1"}},
			EnableCompression: true,
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
