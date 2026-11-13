package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	agentclaude "github.com/manaflow-ai/subrouter/internal/agents/claude"
	"github.com/manaflow-ai/subrouter/selectacct"
	"github.com/manaflow-ai/subrouter/session"
)

func TestClaudeAccountUpgradeReopensAdmissionForExistingSession(t *testing.T) {
	account := accounts.Account{ID: "upgraded", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "token"}
	ref := NewAccountRef(accounts.CodexStore{Dir: t.TempDir()}, []accounts.Account{account}, nil)
	ref.claudeHealthTracker.Store(newClaudeCredentialHealth("", nil))
	gate := newAdmissionControl()
	key := admissionKeyFor(accounts.ProviderClaude, account.ID, "claude-sonnet")
	gate.observe(key, http.StatusTooManyRequests, http.Header{"Anthropic-Ratelimit-Unified-5h-Status": {"rejected"}, "Anthropic-Ratelimit-Unified-5h-Reset": {time.Now().Add(time.Hour).Format(time.RFC3339)}}, time.Now())
	observedAt := time.Now().Add(-time.Minute)
	ref.usageWindows = map[string]usageWindowsEntry{account.ID + "\x00claude": {windows: healthyClaudeUpgradeWindows(), at: observedAt, orderAt: observedAt, supplementalFresh: true, credentialKey: usageWindowsCredentialKey(account)}}
	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put("claude", "existing-session", account.ID, ""); err != nil {
		t.Fatal(err)
	}
	upstream, _ := url.Parse("https://claude.example")
	server := Server{RefreshAccountFn: func(_ context.Context, account accounts.Account) (accounts.Account, error) { return account, nil }, AccountRef: ref, admission: gate, Sessions: store, Scheduler: selectacct.NewScheduler([]selectacct.Score{{Provider: accounts.ProviderClaude, AccountID: account.ID, Headroom: 1, ShortHeadroom: 1}}), ClaudeUpstream: upstream, MaxBodyBytes: 1 << 20, Transport: proxyRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"type":"message"}`)), Request: r}, nil
	})}
	handler := server.Handler()
	request := func() *http.Request {
		r := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-sonnet-5","messages":[]}`))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("X-Subrouter-Agent", "claude")
		r.Header.Set("X-Subrouter-Session", "existing-session")
		return r
	}
	before := httptest.NewRecorder()
	handler.ServeHTTP(before, request())
	if before.Code != http.StatusServiceUnavailable || !strings.Contains(before.Body.String(), errAdmissionPaused.Error()) {
		t.Fatalf("before quota reconciliation = %d: %s", before.Code, before.Body.String())
	}
	ref.usageWindowsMu.Lock()
	entry := ref.usageWindows[account.ID+"\x00claude"]
	entry.at = time.Now()
	entry.orderAt = entry.at
	ref.usageWindows[account.ID+"\x00claude"] = entry
	ref.usageWindowsMu.Unlock()
	if _, _, err := server.fetchAccountUsageWindows(context.Background(), nil, account); err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request())
	if response.Code != http.StatusOK {
		t.Fatalf("upgraded account request = %d: %s", response.Code, response.Body.String())
	}
}

func healthyClaudeUpgradeWindows() []accounts.UsageWindow {
	return []accounts.UsageWindow{{Name: "5h", LimitWindowSeconds: 18000}, {Name: "7d", LimitWindowSeconds: 604800}}
}

func TestClaudeQuotaRecoveryPreservesOtherCooldowns(t *testing.T) {
	for _, tc := range []struct {
		name             string
		header           http.Header
		windows          []accounts.UsageWindow
		oldObservation   bool
		oldStart         bool
		wantPaused       bool
		modelEvidence    bool
		oldModelEvidence bool
		modelCooked      bool
		source           string
	}{
		{name: "fresh account quota", windows: healthyClaudeUpgradeWindows()},
		{name: "status snapshot reconciles fresh quota", windows: healthyClaudeUpgradeWindows(), source: "status"},
		{name: "successful response reconciles fresh quota", windows: healthyClaudeUpgradeWindows(), source: "response"},
		{name: "observation predates quota rejection", windows: healthyClaudeUpgradeWindows(), oldObservation: true, wantPaused: true},
		{name: "fetch started before newer rejection", windows: healthyClaudeUpgradeWindows(), oldStart: true, wantPaused: true},
		{name: "request throttle Retry-After", header: http.Header{"Retry-After": {"3600"}}, windows: healthyClaudeUpgradeWindows(), wantPaused: true},
		{name: "weekly quota remains exhausted", windows: []accounts.UsageWindow{{Name: "5h", LimitWindowSeconds: 18000}, {Name: "7d", LimitWindowSeconds: 604800, UsedPercent: 100}}, wantPaused: true},
		{name: "unrelated model observation", windows: []accounts.UsageWindow{{Name: "oauth-apps-weekly", Feature: agentclaude.FableFeature, LimitWindowSeconds: 604800}}, wantPaused: true},
		{name: "fresh matching model recovers model-only rejection", header: http.Header{"Anthropic-Ratelimit-Unified-Status": {"rejected"}}, windows: healthyClaudeUpgradeWindows(), modelEvidence: true},
		{name: "older matching model cannot clear model-only rejection", header: http.Header{"Anthropic-Ratelimit-Unified-Status": {"rejected"}}, windows: healthyClaudeUpgradeWindows(), modelEvidence: true, oldModelEvidence: true, wantPaused: true},
		{name: "fresh exhausted model cannot clear model-only rejection", header: http.Header{"Anthropic-Ratelimit-Unified-Status": {"rejected"}}, windows: healthyClaudeUpgradeWindows(), modelEvidence: true, modelCooked: true, wantPaused: true},
		{name: "model-only rejection needs model evidence", header: http.Header{"Anthropic-Ratelimit-Unified-Status": {"rejected"}, "Anthropic-Ratelimit-Unified-7d_oi-Status": {"rejected"}}, windows: healthyClaudeUpgradeWindows(), wantPaused: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			account := accounts.Account{ID: "upgraded", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "token"}
			ref := NewAccountRef(accounts.CodexStore{Dir: t.TempDir()}, []accounts.Account{account}, nil)
			gate := newAdmissionControl()
			key := admissionKeyFor(accounts.ProviderClaude, account.ID, "claude-sonnet")
			if tc.header == nil {
				tc.header = http.Header{"Anthropic-Ratelimit-Unified-5h-Status": {"rejected"}}
			}
			gate.observe(key, http.StatusTooManyRequests, tc.header, time.Now())
			observedAt := time.Now()
			start := observedAt
			if tc.oldObservation {
				observedAt = observedAt.Add(-time.Minute)
				start = observedAt
			}
			if tc.oldStart {
				start = start.Add(-time.Minute)
			}
			ref.usageWindows = map[string]usageWindowsEntry{account.ID + "\x00claude": {windows: tc.windows, at: observedAt, orderAt: start, supplementalFresh: true, credentialKey: usageWindowsCredentialKey(account)}}
			if tc.modelEvidence {
				at := observedAt
				if tc.oldModelEvidence {
					at = at.Add(-time.Minute)
				}
				used := 0.0
				if tc.modelCooked {
					used = 100
				}
				ref.claudeSupplemental = map[string]claudeSupplementalUsage{claudeSupplementalCacheKey(account): {at: at, requestStarted: at, windows: []accounts.UsageWindow{{Name: "sonnet-weekly", Feature: agentclaude.SonnetFeature, UsedPercent: used, LimitWindowSeconds: 604800}}}}
			}
			server := Server{AccountRef: ref, admission: gate}
			switch tc.source {
			case "status":
				ref.usageStatusCache = []AccountUsageStatus{}
				ref.usageStatusAt = time.Now()
				response := httptest.NewRecorder()
				server.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/_subrouter/usage-status?snapshot=1", nil))
				if response.Code != http.StatusOK {
					t.Fatalf("status=%d: %s", response.Code, response.Body.String())
				}
			case "response":
				server.SchedulerRef = selectacct.NewSchedulerRef(selectacct.NewScheduler(nil))
				server.mergeClaudeResponseQuotaScore(account, tc.windows, ref.Generation(), account.CredentialIdentity(), observedAt)
			default:
				if _, _, err := server.fetchAccountUsageWindows(context.Background(), nil, account); err != nil {
					t.Fatal(err)
				}
			}
			if got := gate.paused(key, time.Now()); got != tc.wantPaused {
				t.Fatalf("paused=%v, want%v", got, tc.wantPaused)
			}
		})
	}
}

func TestClaudeQuotaRecoveryDoesNotEraseRejectionDuringUsageFetch(t *testing.T) {
	account := accounts.Account{ID: "upgraded", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "token"}
	ref := NewAccountRef(accounts.CodexStore{Dir: t.TempDir()}, []accounts.Account{account}, nil)
	gate := newAdmissionControl()
	server := Server{AccountRef: ref, admission: gate}
	started, release := make(chan struct{}), make(chan struct{})
	client := &http.Client{Transport: proxyRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		close(started)
		select {
		case <-release:
		case <-r.Context().Done():
			return nil, r.Context().Err()
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"five_hour":{"utilization":0},"seven_day":{"utilization":0},"seven_day_oauth_apps":{"utilization":0}}`)), Request: r}, nil
	})}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { _, _, err := server.fetchAccountUsageWindows(ctx, client, account); done <- err }()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("usage fetch did not start")
	}
	key := admissionKeyFor(accounts.ProviderClaude, account.ID, "claude-sonnet")
	gate.observe(key, http.StatusTooManyRequests, http.Header{"Anthropic-Ratelimit-Unified-5h-Status": {"rejected"}}, time.Now())
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if !gate.paused(key, time.Now()) {
		t.Fatal("a usage fetch started before a newer rejection erased its cooldown")
	}
}

func TestClaudeQuotaRecoveryPreservesIndependentHolds(t *testing.T) {
	for _, tc := range []struct {
		name                          string
		first, second                 time.Duration
		firstThrottle, secondThrottle bool
		firstModel, secondModel       bool
		want                          time.Duration
		wantRamp                      bool
	}{
		{name: "short quota after long request throttle", first: 2 * time.Hour, firstThrottle: true, second: time.Hour, want: 2 * time.Hour, wantRamp: true},
		{name: "short request throttle after long quota", first: 2 * time.Hour, second: time.Hour, secondThrottle: true, want: time.Hour, wantRamp: true},
		{name: "long request throttle after short quota", first: time.Hour, second: 2 * time.Hour, secondThrottle: true, want: 2 * time.Hour, wantRamp: true},
		{name: "short account quota after long model quota", first: 2 * time.Hour, firstModel: true, second: time.Hour, want: 2 * time.Hour},
		{name: "short model quota after long account quota", first: 2 * time.Hour, second: time.Hour, secondModel: true, want: time.Hour},
	} {
		t.Run(tc.name, func(t *testing.T) {
			account := accounts.Account{ID: "recovered", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "token"}
			ref := NewAccountRef(accounts.CodexStore{Dir: t.TempDir()}, []accounts.Account{account}, nil)
			gate := newAdmissionControl()
			key := admissionKeyFor(accounts.ProviderClaude, account.ID, agentclaude.SonnetFeature)
			now := time.Now()
			gate.pauseWithQuotaScope(key, now.Add(tc.first), tc.firstThrottle, tc.firstModel)
			gate.pauseWithQuotaScope(key, now.Add(tc.second), tc.secondThrottle, tc.secondModel)
			observed := time.Now()
			ref.usageWindows = map[string]usageWindowsEntry{account.ID + "\x00claude": {at: observed, orderAt: observed, credentialKey: usageWindowsCredentialKey(account), windows: healthyClaudeUpgradeWindows()}}
			Server{AccountRef: ref, admission: gate}.reconcileClaudeAdmissionQuota()
			gate.mu.Lock()
			until, ramp := gate.buckets[key].pausedTil, gate.buckets[key].recoverSlowly
			gate.mu.Unlock()
			if !until.Equal(now.Add(tc.want)) || ramp != tc.wantRamp {
				t.Fatalf("remaining hold=%s ramp=%v, want%s ramp=%v", until.Sub(now), ramp, tc.want, tc.wantRamp)
			}
		})
	}
}
