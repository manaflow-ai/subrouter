package proxy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	"github.com/manaflow-ai/subrouter/internal/accounts"
	agentclaude "github.com/manaflow-ai/subrouter/internal/agents/claude"
	"github.com/manaflow-ai/subrouter/internal/selectacct"
	"github.com/manaflow-ai/subrouter/internal/session"
	"github.com/manaflow-ai/subrouter/internal/transcript"
)

type Server struct {
	Upstream       *url.URL
	CodexUpstream  *url.URL
	APIUpstream    *url.URL
	ClaudeUpstream *url.URL
	Accounts       []accounts.Account
	AccountRef     *AccountRef
	Sessions       *session.Store
	Scheduler      selectacct.Scheduler
	SchedulerRef   *selectacct.SchedulerRef
	UsageScoreTTL  time.Duration
	ScoreAccounts  func(context.Context, []accounts.Account) ([]selectacct.Score, int)
	Transport      http.RoundTripper
	Logger         *slog.Logger
	ActiveSessions *ActiveSessions
	Lifecycle      *Lifecycle
	AdminToken     string
	MaxBodyBytes   int64
	Transcripts    *transcript.Recorder
	ReadCache      *readCache
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
		status := AccountStatus{
			ID:       stored.Email,
			Provider: accounts.ProviderCodex,
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
		status := AccountUsageStatus{
			AccountStatus: AccountStatus{
				ID:       stored.Email,
				Provider: accounts.ProviderCodex,
				Email:    stored.Email,
				Source:   stored.SourcePath(r.store),
			},
			Active: stored.Email == active,
		}
		if stored.IsAPIKey() {
			status.AuthMode = accounts.AuthModeAPIKey
			status.PlanType = "api key"
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
			windows, _, err := r.FetchUsageWindowsCached(ctx, r.client, account)
			if err != nil {
				next.Error = err.Error()
				out[i] = next
				return
			}
			next.PlanType = "claude"
			next.Windows = windows
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

func claudeUsageWindows(usage *agentclaude.UsageResponse) []accounts.UsageWindow {
	if usage == nil {
		return nil
	}
	var windows []accounts.UsageWindow
	add := func(name string, limit *agentclaude.RateLimit) {
		if limit == nil || limit.Utilization == nil {
			return
		}
		window := accounts.UsageWindow{
			Name:        name,
			UsedPercent: *limit.Utilization,
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
	add("5h", usage.FiveHour)
	add("7d", usage.SevenDay)
	add("opus-weekly", usage.SevenDayOpus)
	add("sonnet-weekly", usage.SevenDaySonnet)
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
	limitWindows := make([]selectacct.LimitWindow, 0, len(windows))
	for _, window := range windows {
		limitWindows = append(limitWindows, selectacct.LimitWindow{
			Name:               window.Name,
			UsedPercent:        window.UsedPercent,
			LimitWindowSeconds: window.LimitWindowSeconds,
			ResetAfterSeconds:  window.ResetAfterSeconds,
			Feature:            window.Feature,
		})
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
	mux.HandleFunc("/_subrouter/drain", s.requireAdmin(s.handleDrain))
	mux.HandleFunc("/_subrouter/drain-status", s.requireAdmin(s.handleDrainStatus))
	mux.HandleFunc("/_subrouter/accounts", s.requireAdmin(s.handleAccounts))
	mux.HandleFunc("/_subrouter/account-status", s.requireAdmin(s.handleAccountStatus))
	mux.HandleFunc("/_subrouter/usage-status", s.requireAdmin(s.handleUsageStatus))
	mux.HandleFunc("/_subrouter/rate-limit-reset", s.requireAdmin(s.handleRateLimitReset))
	mux.HandleFunc("/_subrouter/reload-accounts", s.requireAdmin(s.handleReloadAccounts))
	mux.HandleFunc("/_subrouter/sessions", s.requireAdmin(s.handleSessions))
	mux.HandleFunc("/_subrouter/dashboard", s.requireAdmin(s.handleDashboard))
	mux.HandleFunc("/_subrouter/transcripts", s.requireAdmin(s.handleTranscriptList))
	mux.HandleFunc("/_subrouter/transcripts/", s.requireAdmin(s.handleTranscriptDetail))
	mux.HandleFunc("/_subrouter/", http.NotFound)
	mux.Handle("/", s.proxyHandler())
	return mux
}

func (s Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, map[string]any{"ok": true})
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
		writeJSON(w, s.AccountRef.UsageStatuses(r.Context()))
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
	writeJSON(w, out)
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
			scores[idx] = scoreFromUsageWindows(account.Provider, account.ID, windows)
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
		if err != nil {
			return nil, err
		}
		return claudeUsageWindows(usage), nil
	}
	return accounts.FetchCodexUsage(ctx, client, account)
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
		account, sessionID, userEmail, err := s.accountForSession(agentType, sessionID, r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		account, err = s.refreshSelectedAccount(r.Context(), agentType, sessionID, userEmail, r, account)
		if err != nil {
			http.Error(w, "refresh selected account: "+err.Error(), http.StatusServiceUnavailable)
			return
		}

		auth := account.AuthorizationHeader()
		if auth == "" {
			http.Error(w, "selected account has no usable credential", http.StatusServiceUnavailable)
			return
		}

		endActive := func() {}
		if activeSessionRequest(agentType, r) {
			endActive = s.ActiveSessions.Begin(agentType, sessionID)
			defer endActive()
		}

		setAccountAuthHeaders(r.Header, account)

		upstream := s.upstreamForRequest(r.URL.Path, account)
		if upstream == nil {
			http.Error(w, "no upstream configured", http.StatusServiceUnavailable)
			return
		}
		if websocket.IsWebSocketUpgrade(r) {
			s.proxyWebSocket(w, r, account, agentType, sessionID, userEmail, upstream)
			return
		}
		proxyRequest := r.Clone(r.Context())
		proxyRequest.URL = cloneURL(r.URL)
		proxyRequest.URL.Path = s.pathForUpstream(proxyRequest.URL.Path, account)
		proxyRequest.URL.RawPath = ""
		session.StripSubrouterHeaders(proxyRequest.Header)
		requestProvider := providerForAgent(agentType)
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
				s.Logger.Warn("retryable request body exceeds retry buffer", "agent", agentType, "session", sessionID, "account", account.ID, "method", r.Method, "path", proxyRequest.URL.Path, "content_length", r.ContentLength, "max_bytes", replayablePostMaxBodyBytes)
			}
		}
		s.recordHTTPMeta(proxyRequest, agentType, sessionID, userEmail, account, upstream)
		if retryPost && postReplayable {
			s.recordReplayableRequestBody(proxyRequest, agentType, sessionID)
		} else {
			s.captureRequestBody(proxyRequest, agentType, sessionID)
		}

		rp := httputil.NewSingleHostReverseProxy(upstream)
		transport := s.transport()
		if retryPost && postReplayable &&
			(requestProvider == accounts.ProviderCodex || requestProvider == accounts.ProviderClaude) &&
			account.AuthMode == accounts.AuthModeOAuth {
			transport = usageLimitRetryTransport{
				base:        transport,
				server:      &s,
				logger:      s.Logger,
				provider:    requestProvider,
				agent:       agentType,
				session:     sessionID,
				userEmail:   userEmail,
				account:     account.ID,
				method:      r.Method,
				path:        proxyRequest.URL.Path,
				upstream:    upstream.Host,
				maxAttempts: replayablePostMaxAttempts,
			}
		}
		if retryPost && postReplayable {
			transport = replayablePostRetryTransport{
				base:        transport,
				logger:      s.Logger,
				agent:       agentType,
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
		originalDirector := rp.Director
		rp.Director = func(r *http.Request) {
			originalDirector(r)
			r.Host = upstream.Host
		}
		rp.ModifyResponse = func(response *http.Response) error {
			s.captureResponseBody(response, agentType, sessionID, account.ID, account.Provider, proxyRequest.URL.Path)
			return nil
		}
		if s.Logger != nil {
			rp.ErrorLog = log.New(proxyErrorWriter{
				logger:   s.Logger,
				agent:    agentType,
				session:  sessionID,
				account:  account.ID,
				method:   r.Method,
				path:     proxyRequest.URL.Path,
				upstream: upstream.Host,
			}, "", 0)
			rp.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, err error) {
				s.Logger.Error("proxy request failed", "agent", agentType, "session", sessionID, "account", account.ID, "method", r.Method, "path", proxyRequest.URL.Path, "upstream", upstream.Host, "error", err)
				http.Error(w, "upstream request failed", http.StatusBadGateway)
			}
		}

		if s.Logger != nil {
			s.Logger.Info("proxy request", "agent", agentType, "session", sessionID, "user", userEmail, "account", account.ID, "method", r.Method, "path", r.URL.Path, "upstream", upstream.Host)
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

func baseURLProbeRequest(r *http.Request) bool {
	return r.Method == http.MethodHead && (r.URL.Path == "" || r.URL.Path == "/")
}

func (s Server) proxyWebSocket(w http.ResponseWriter, r *http.Request, account accounts.Account, agentType, sessionID, userEmail string, upstream *url.URL) {
	upstreamURL := cloneURL(r.URL)
	upstreamURL.Scheme = websocketScheme(upstream.Scheme)
	upstreamURL.Host = upstream.Host
	upstreamURL.Path = joinURLPath(upstream.Path, s.pathForUpstream(upstreamURL.Path, account))
	upstreamURL.RawPath = ""

	headers := r.Header.Clone()
	stripWebSocketDialHeaders(headers)
	session.StripSubrouterHeaders(headers)
	setAccountAuthHeaders(headers, account)
	s.recordWebSocketMeta(r, upstreamURL, headers, agentType, sessionID, userEmail, account, upstream)

	upstreamConn, response, err := websocket.DefaultDialer.Dial(upstreamURL.String(), headers)
	if err != nil {
		status := 502
		if response != nil {
			status = response.StatusCode
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

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		s.copyWebSocketMessages(agentType, sessionID, account.ID, "client_to_upstream", clientConn, upstreamConn)
		_ = upstreamConn.Close()
	}()
	go func() {
		defer wg.Done()
		s.copyWebSocketMessages(agentType, sessionID, account.ID, "upstream_to_client", upstreamConn, clientConn)
		_ = clientConn.Close()
	}()
	wg.Wait()
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

func (s Server) copyWebSocketMessages(agentType, sessionID, accountID, direction string, src, dst *websocket.Conn) {
	for {
		messageType, body, err := src.ReadMessage()
		if err != nil {
			return
		}
		if s.Transcripts != nil {
			s.Transcripts.RecordPayload(agentType, sessionID, "websocket_message", direction, body, map[string]any{
				"opcode": websocketMessageType(messageType),
			})
		}
		if direction == "upstream_to_client" && messageType == websocket.TextMessage && usageLimitJSON(body) {
			s.markAccountExhausted(providerForAgent(agentType), accountID)
		}
		if err := dst.WriteMessage(messageType, body); err != nil {
			return
		}
	}
}

func (s Server) markAccountExhausted(provider accounts.Provider, accountID string) {
	if s.SchedulerRef != nil {
		s.SchedulerRef.MarkExhausted(provider, accountID)
	}
}

func usageLimitJSON(body []byte) bool {
	var event map[string]any
	if err := json.Unmarshal(body, &event); err != nil {
		return false
	}
	return usageLimitMap(event)
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

func (s Server) captureResponseBody(response *http.Response, agentType, sessionID, accountID string, provider accounts.Provider, path string) {
	// Anthropic signals subscription exhaustion with a plain 429 and a dead or
	// expired OAuth token with a plain 401, neither with a codex-style
	// usage-limit body to inspect. Both mean this account can't serve the
	// request, so drop it from selection and let failover pick another.
	if provider == accounts.ProviderClaude && accountID != "" && claudeAccountUnusableStatus(response.StatusCode) {
		s.markAccountExhausted(provider, accountID)
	}
	inspectUsageLimit := s.SchedulerRef != nil && accountID != "" && responseStatusCanExhaust(response.StatusCode)
	if response.Body == nil || (s.Transcripts == nil && s.Logger == nil && !inspectUsageLimit) {
		return
	}
	payload := map[string]any{"status": response.StatusCode}
	var inspect func([]byte)
	if inspectUsageLimit {
		inspect = func(body []byte) {
			if usageLimitJSON(body) {
				s.markAccountExhausted(provider, accountID)
			}
		}
	}
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
			if s.Logger != nil {
				s.Logger.Error("proxy response stream read failed", "agent", agentType, "session", sessionID, "account", accountID, "path", path, "status", response.StatusCode, "bytes", bytesRead, "error", err)
			}
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
		headers.Del("X-Api-Key")
		ensureCommaHeaderValue(headers, "Anthropic-Beta", claudeOAuthBetaHeader)
	case accounts.ProviderCodex, "":
		if account.AccountID != "" {
			headers.Set("ChatGPT-Account-ID", account.AccountID)
		}
	}
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
	userEmail := session.ExtractUserEmail(r)
	forcedAccountID := session.ExtractAccountID(r)
	model := session.ExtractModel(r, s.MaxBodyBytes)
	provider := providerForAgent(agentType)
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
	if provider == accounts.ProviderCodex || provider == accounts.ProviderClaude {
		s.refreshUsageScoresIfStale(r.Context())
	}
	base := s.scheduler()
	if model != "" && s.Logger != nil && base.HasModelPool(model) {
		s.Logger.Info("model quota pool matched", "agent", agentType, "model", model, "pool", selectacct.ModelKey(model))
	}
	scheduler := base.ForModel(model).WithSessionCounts(s.Sessions.CountByAccount())
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
	if s.Logger == nil || providerForAgent(agentType) != accounts.ProviderCodex || account.AuthMode != accounts.AuthModeOAuth {
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
	if providerForAgent(agentType) == accounts.ProviderCodex && account.AuthMode == accounts.AuthModeOAuth {
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
	return legacy
}

func (s Server) accountList() []accounts.Account {
	out := append([]accounts.Account(nil), s.Accounts...)
	if s.AccountRef != nil {
		out = append(out, s.AccountRef.All()...)
	}
	return out
}

func (s Server) refreshAccount(ctx context.Context, account accounts.Account) (accounts.Account, error) {
	if s.AccountRef == nil {
		return account, nil
	}
	return s.AccountRef.Refresh(ctx, account)
}

func (s Server) refreshSelectedAccount(ctx context.Context, agentType, sessionID, userEmail string, r *http.Request, account accounts.Account) (accounts.Account, error) {
	refreshed, err := s.refreshAccount(ctx, account)
	if err == nil {
		return refreshed, nil
	}
	provider := providerForAgent(agentType)
	if provider != accounts.ProviderClaude || session.ExtractAccountID(r) != "" {
		return account, err
	}
	if s.Logger != nil {
		s.Logger.Warn("selected Claude account refresh failed, trying another account", "account", account.ID, "error", err)
	}
	tried := map[string]struct{}{account.ID: {}}
	s.markAccountExhausted(provider, account.ID)
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
		s.markAccountExhausted(provider, next.ID)
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
		path == "/v1/responses/compact"
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
}

// responseUsageLimited reports whether the upstream response means the current
// account is out of quota. Anthropic signals subscription exhaustion with a
// plain 429 (no codex-style "usage_limit_reached" body), so claude is detected
// by status alone.
func (t usageLimitRetryTransport) responseUsageLimited(response *http.Response) (bool, error) {
	if t.provider == accounts.ProviderClaude {
		return response != nil && claudeAccountUnusableStatus(response.StatusCode), nil
	}
	return responseUsageLimit(response)
}

// claudeAccountUnusableStatus reports whether an upstream status means the
// selected Claude account cannot serve this request and subrouter should fail
// over to another account. 429 is quota exhaustion; 401 is a dead or expired
// OAuth token (Anthropic returns authentication_error). Both are
// account-specific, so replaying the same request on a different account is the
// correct response instead of surfacing the failure to the client.
func claudeAccountUnusableStatus(code int) bool {
	return code == http.StatusTooManyRequests || code == http.StatusUnauthorized
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
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		response, err := base.RoundTrip(attemptReq)
		if err != nil || req.GetBody == nil || req.Context().Err() != nil {
			return response, err
		}
		usageLimited, inspectErr := t.responseUsageLimited(response)
		if inspectErr != nil {
			if t.logger != nil {
				t.logger.Warn("usage-limit response inspection failed", "agent", t.agent, "session", t.session, "account", accountID, "method", t.method, "path", t.path, "upstream", t.upstream, "error", inspectErr)
			}
			return response, nil
		}
		if !usageLimited {
			return response, nil
		}
		if t.server != nil {
			t.server.markAccountExhausted(t.provider, accountID)
		}
		if attempt == maxAttempts || t.server == nil {
			return response, nil
		}
		nextAccount, pickErr := t.server.oauthRetryAccount(req.Context(), t.provider, t.agent, t.session, t.userEmail, tried)
		if pickErr != nil {
			if t.logger != nil {
				t.logger.Warn("usage-limit retry has no alternate account", "agent", t.agent, "session", t.session, "account", accountID, "method", t.method, "path", t.path, "upstream", t.upstream, "error", pickErr)
			}
			return response, nil
		}
		body, bodyErr := req.GetBody()
		if bodyErr != nil {
			if t.logger != nil {
				t.logger.Warn("usage-limit retry could not replay request body", "agent", t.agent, "session", t.session, "account", accountID, "method", t.method, "path", t.path, "upstream", t.upstream, "error", bodyErr)
			}
			return response, nil
		}
		if response.Body != nil {
			_ = response.Body.Close()
		}
		previousAccount := accountID
		accountID = nextAccount.ID
		tried[accountID] = struct{}{}
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

func (s Server) oauthRetryAccount(ctx context.Context, provider accounts.Provider, agentType, sessionID, userEmail string, tried map[string]struct{}) (accounts.Account, error) {
	allCandidates := oauthAccounts(filterAccountsForProvider(s.accountList(), provider))
	if len(allCandidates) == 0 {
		return accounts.Account{}, fmt.Errorf("no OAuth %s accounts available", provider)
	}
	candidates := make([]accounts.Account, 0, len(allCandidates))
	for _, account := range allCandidates {
		if _, ok := tried[account.ID]; ok {
			continue
		}
		candidates = append(candidates, account)
	}
	if len(candidates) == 0 {
		return accounts.Account{}, fmt.Errorf("no untried OAuth %s accounts available", provider)
	}
	s.refreshUsageScoresIfStale(ctx)
	scheduler := s.scheduler()
	if s.Sessions != nil {
		scheduler = scheduler.WithSessionCounts(s.Sessions.CountByAccount())
	}
	account, err := scheduler.Pick(candidates)
	if err != nil {
		return accounts.Account{}, err
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
		return accounts.Account{}, err
	}
	if s.Sessions != nil {
		if _, err := s.Sessions.Put(agentType, sessionID, refreshed.ID, userEmail); err != nil {
			return accounts.Account{}, err
		}
	}
	return refreshed, nil
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

func (t replayablePostRetryTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	maxAttempts := t.maxAttempts
	if maxAttempts < 1 {
		maxAttempts = 1
	}
	attemptReq := req
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		response, err := t.roundTrip(attemptReq)
		retryStatus := err == nil && retryablePostUpstreamStatus(response)
		retryTransportErr := err != nil && retryablePostTransportError(err)
		if (!retryStatus && !retryTransportErr) || req.GetBody == nil || req.Context().Err() != nil || attempt == maxAttempts {
			return response, err
		}
		body, bodyErr := req.GetBody()
		if bodyErr != nil {
			return response, err
		}
		if response != nil && response.Body != nil {
			_ = response.Body.Close()
		}
		if closer, ok := t.base.(interface{ CloseIdleConnections() }); ok {
			closer.CloseIdleConnections()
		}
		attemptReq = req.Clone(req.Context())
		attemptReq.Body = body
		attemptReq.GetBody = req.GetBody
		attemptReq.ContentLength = req.ContentLength
		if t.logger != nil {
			if retryStatus {
				t.logger.Warn("retrying replayable upstream request after upstream timeout status", "agent", t.agent, "session", t.session, "account", t.account, "method", t.method, "path", t.path, "upstream", t.upstream, "attempt", attempt+1, "max_attempts", maxAttempts, "status", response.StatusCode, "cf_ray", response.Header.Get("Cf-Ray"), "request_id", response.Header.Get("X-Request-ID"))
			} else {
				t.logger.Warn("retrying replayable upstream request after transport failure", "agent", t.agent, "session", t.session, "account", t.account, "method", t.method, "path", t.path, "upstream", t.upstream, "attempt", attempt+1, "max_attempts", maxAttempts, "error", err)
			}
		}
	}
	return t.roundTrip(req)
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
	return transport
}

func (s Server) scheduler() selectacct.Scheduler {
	if s.SchedulerRef != nil {
		return s.SchedulerRef.Get()
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
