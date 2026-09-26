package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	agentclaude "github.com/manaflow-ai/subrouter/internal/agents/claude"
	"github.com/manaflow-ai/subrouter/selectacct"
	"github.com/manaflow-ai/subrouter/session"
)

// gatedUsageTransport is a slow fake usage endpoint: every call blocks until
// release is closed (or the request context ends), and calls are counted.
type gatedUsageTransport struct {
	calls   atomic.Int64
	release chan struct{}
	started chan struct{}
}

func newGatedUsageTransport() *gatedUsageTransport {
	return &gatedUsageTransport{release: make(chan struct{}), started: make(chan struct{}, 1024)}
}

func (g *gatedUsageTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	g.calls.Add(1)
	select {
	case g.started <- struct{}{}:
	default:
	}
	select {
	case <-g.release:
	case <-req.Context().Done():
		return nil, req.Context().Err()
	}
	return claudeUsageOK(), nil
}

// multiProfileAccountRef builds an AccountRef with n Claude OAuth profiles,
// each with a distinct valid access token.
func multiProfileAccountRef(t *testing.T, transport http.RoundTripper, n int) (*AccountRef, []accounts.Account) {
	t.Helper()
	dir := t.TempDir()
	claudeDir := filepath.Join(dir, ".subrouter", "codex")
	profiles := map[string]any{}
	var accts []accounts.Account
	expires := time.Now().Add(time.Hour).UnixMilli()
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("claude%d@example.com", i)
		sub := fmt.Sprintf("_p%d", i)
		profileDir := filepath.Join(claudeDir, "claude", sub)
		if err := os.MkdirAll(profileDir, 0o700); err != nil {
			t.Fatal(err)
		}
		profiles[name] = map[string]any{"name": name, "dir": sub}
		token := fmt.Sprintf("tok-%d", i)
		credential := fmt.Sprintf(`{"claudeAiOauth":{"accessToken":%q,"refreshToken":"ref-%d","expiresAt":%d,"subscriptionType":"max"}}`, token, i, expires)
		if err := os.WriteFile(filepath.Join(profileDir, ".credentials.json"), []byte(credential), 0o600); err != nil {
			t.Fatal(err)
		}
		accts = append(accts, accounts.Account{ID: name, Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: token})
	}
	body, err := json.Marshal(map[string]any{"profiles": profiles})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(claudeDir, "claude.json"), body, 0o600); err != nil {
		t.Fatal(err)
	}
	return &AccountRef{
		store:       accounts.CodexStore{Dir: filepath.Join(dir, "codex-accounts")},
		claudeStore: agentclaude.Store{Dir: claudeDir},
		client:      &http.Client{Transport: transport},
	}, accts
}

func waitStarted(t *testing.T, g *gatedUsageTransport) {
	t.Helper()
	select {
	case <-g.started:
	case <-time.After(5 * time.Second):
		t.Fatal("upstream usage fetch never started")
	}
}

// Concurrent readers of one account's usage (the score sweep and the status
// sweep overlapping) must share a single upstream fetch.
func TestFetchUsageWindowsCachedCoalescesConcurrentFetches(t *testing.T) {
	transport := newGatedUsageTransport()
	ref := &AccountRef{}
	client := &http.Client{Transport: transport}
	account := accounts.Account{ID: "a@example.com", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "tok"}
	const callers = 6
	var wg sync.WaitGroup
	errs := make(chan error, callers)
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			windows, fresh, err := ref.FetchUsageWindowsCached(context.Background(), client, account)
			if err != nil || len(windows) == 0 || !fresh {
				errs <- fmt.Errorf("windows=%v fresh=%v err=%v", windows, fresh, err)
			}
		}()
	}
	waitStarted(t, transport)
	time.Sleep(150 * time.Millisecond) // let every caller reach the fetch
	close(transport.release)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	// One usage call plus one Fable probe, shared by every caller.
	if got := transport.calls.Load(); got != 2 {
		t.Fatalf("upstream calls = %d, want 2 (one shared fetch for %d concurrent callers)", got, callers)
	}
}

// A waiter whose context ends must return promptly without cancelling the
// shared fetch other callers depend on.
func TestFetchUsageWindowsCachedWaiterCancellationKeepsSharedFetch(t *testing.T) {
	transport := newGatedUsageTransport()
	ref := &AccountRef{}
	client := &http.Client{Transport: transport}
	account := accounts.Account{ID: "a@example.com", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "tok"}

	leaderCtx, cancelLeader := context.WithCancel(context.Background())
	leaderDone := make(chan error, 1)
	go func() {
		_, _, err := ref.FetchUsageWindowsCached(leaderCtx, client, account)
		leaderDone <- err
	}()
	waitStarted(t, transport)
	otherDone := make(chan error, 1)
	go func() {
		windows, _, err := ref.FetchUsageWindowsCached(context.Background(), client, account)
		if err == nil && len(windows) == 0 {
			err = fmt.Errorf("no windows")
		}
		otherDone <- err
	}()
	time.Sleep(50 * time.Millisecond)
	cancelLeader()
	select {
	case <-leaderDone:
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled caller did not return promptly")
	}
	close(transport.release)
	select {
	case err := <-otherDone:
		if err != nil {
			t.Fatalf("surviving caller lost the shared fetch: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("surviving caller never returned")
	}
}

// A dashboard client that disconnects mid-sweep must not abort the shared
// sweep other status callers are waiting on.
func TestUsageStatusesCancelledCallerDoesNotAbortSharedSweep(t *testing.T) {
	transport := newGatedUsageTransport()
	ref, _ := multiProfileAccountRef(t, transport, 1)

	ctxA, cancelA := context.WithCancel(context.Background())
	doneA := make(chan struct{})
	go func() {
		defer close(doneA)
		ref.UsageStatuses(ctxA)
	}()
	waitStarted(t, transport)
	resultB := make(chan []AccountUsageStatus, 1)
	go func() { resultB <- ref.UsageStatuses(context.Background()) }()
	time.Sleep(50 * time.Millisecond)
	cancelA()
	select {
	case <-doneA:
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled dashboard request did not return promptly")
	}
	close(transport.release)
	var statuses []AccountUsageStatus
	select {
	case statuses = <-resultB:
	case <-time.After(10 * time.Second):
		t.Fatal("surviving dashboard request never returned")
	}
	if len(statuses) != 1 {
		t.Fatalf("statuses = %d, want 1", len(statuses))
	}
	if statuses[0].Error != "" || len(statuses[0].Windows) == 0 {
		t.Fatalf("shared sweep was aborted by the cancelled caller: %+v", statuses[0])
	}
}

// The status sweep's network fan-out must not hold usageStatusMu: cache
// invalidation (account reloads) must not block behind a slow sweep.
func TestUsageStatusesFanOutDoesNotHoldStatusLock(t *testing.T) {
	transport := newGatedUsageTransport()
	ref, _ := multiProfileAccountRef(t, transport, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ref.UsageStatuses(context.Background())
	}()
	waitStarted(t, transport)
	invalidated := make(chan struct{})
	go func() {
		ref.InvalidateUsageStatusCache()
		close(invalidated)
	}()
	select {
	case <-invalidated:
	case <-time.After(2 * time.Second):
		t.Fatal("InvalidateUsageStatusCache blocked behind an in-flight status sweep")
	}
	close(transport.release)
	<-done
}

// Concurrent status sweeps and a concurrent score sweep over the same pool
// call the upstream usage endpoint once per account.
func TestConcurrentUsageSweepsCallUpstreamOncePerAccount(t *testing.T) {
	const pool = 3
	transport := newGatedUsageTransport()
	ref, accts := multiProfileAccountRef(t, transport, pool)
	server := Server{AccountRef: ref, SchedulerRef: selectacct.NewSchedulerRef(selectacct.NewScheduler(nil))}

	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ref.UsageStatuses(context.Background())
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		server.scoreAccounts(context.Background(), accts)
	}()
	waitStarted(t, transport)
	time.Sleep(200 * time.Millisecond)
	close(transport.release)
	wg.Wait()
	// Usage + Fable probe per account.
	if got, want := transport.calls.Load(), int64(2*pool); got != want {
		t.Fatalf("upstream calls = %d, want %d (one shared fetch per account)", got, want)
	}
}

func staleScoreCodexServer(t *testing.T, scoreAccounts func(context.Context, []accounts.Account) ([]selectacct.Score, int)) (*httptest.Server, *selectacct.SchedulerRef) {
	t.Helper()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(upstream.Close)
	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	schedulerRef := selectacct.NewSchedulerRef(selectacct.NewScheduler([]selectacct.Score{
		{AccountID: "a@example.com", Headroom: 0.8, ShortHeadroom: 0.8},
		{AccountID: "b@example.com", Headroom: 0.8, ShortHeadroom: 0.8},
	}))
	schedulerRef.SetUpdatedAt(time.Now().Add(-time.Hour)) // stale but present
	handler := Server{
		Upstream: upstreamURL,
		Accounts: []accounts.Account{
			{ID: "a@example.com", AuthMode: accounts.AuthModeOAuth, Token: "a-token"},
			{ID: "b@example.com", AuthMode: accounts.AuthModeOAuth, Token: "b-token"},
		},
		Sessions:      store,
		SchedulerRef:  schedulerRef,
		UsageScoreTTL: time.Minute,
		ScoreAccounts: scoreAccounts,
		MaxBodyBytes:  1024,
	}.Handler()
	subrouter := httptest.NewServer(handler)
	t.Cleanup(subrouter.Close)
	return subrouter, schedulerRef
}

// With stale-but-present scores, the request that notices staleness must be
// served from the current scores while the refresh runs in the background.
func TestStaleUsageScoresRefreshOffRequestPath(t *testing.T) {
	release := make(chan struct{})
	var refreshes atomic.Int64
	subrouter, schedulerRef := staleScoreCodexServer(t, func(context.Context, []accounts.Account) ([]selectacct.Score, int) {
		refreshes.Add(1)
		select {
		case <-release:
		case <-time.After(5 * time.Second): // bounded so a pre-fix run fails instead of hanging
		}
		return []selectacct.Score{
			{AccountID: "a@example.com", Headroom: 0.3, ShortHeadroom: 0.3},
			{AccountID: "b@example.com", Headroom: 0.9, ShortHeadroom: 0.9},
		}, 2
	})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	revisionBefore := schedulerRef.ScoreRevision()

	start := time.Now()
	req, err := http.NewRequest(http.MethodPost, subrouter.URL+"/v1/responses", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Subrouter-Session", "stale-session")
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, response.Body)
	response.Body.Close()
	elapsed := time.Since(start)
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", response.StatusCode)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("request took %v: it waited on the stale-score refresh instead of using current scores", elapsed)
	}
	close(release)
	deadline := time.Now().Add(5 * time.Second)
	for schedulerRef.ScoreRevision() == revisionBefore {
		if time.Now().After(deadline) {
			t.Fatal("background refresh never published fresh scores")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := refreshes.Load(); got != 1 {
		t.Fatalf("refreshes = %d, want exactly 1 (singleflight)", got)
	}
}

// Idle pools must still refresh: the background loop refreshes stale scores
// without any request traffic and stops when its context ends.
func TestUsageScoreRefresherRefreshesIdlePoolAndStops(t *testing.T) {
	var refreshes atomic.Int64
	schedulerRef := selectacct.NewSchedulerRef(selectacct.NewScheduler(nil))
	schedulerRef.SetUpdatedAt(time.Now().Add(-time.Hour))
	server := Server{
		Accounts:      []accounts.Account{{ID: "a@example.com", AuthMode: accounts.AuthModeOAuth, Token: "a"}},
		SchedulerRef:  schedulerRef,
		UsageScoreTTL: 40 * time.Millisecond,
		ScoreAccounts: func(context.Context, []accounts.Account) ([]selectacct.Score, int) {
			refreshes.Add(1)
			return []selectacct.Score{{AccountID: "a@example.com", Headroom: 0.5, ShortHeadroom: 0.5}}, 1
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		server.RunUsageScoreRefresher(ctx)
	}()
	deadline := time.Now().Add(5 * time.Second)
	for refreshes.Load() < 3 {
		if time.Now().After(deadline) {
			t.Fatalf("idle pool refreshed %d times, want >= 3", refreshes.Load())
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("refresher did not stop after cancellation")
	}
	after := refreshes.Load()
	time.Sleep(200 * time.Millisecond)
	if got := refreshes.Load(); got != after {
		t.Fatalf("refresher kept running after stop: %d -> %d", after, got)
	}
}

func TestUsageScoreRefresherStopsOnDrain(t *testing.T) {
	lifecycle := NewLifecycle()
	server := Server{
		SchedulerRef:  selectacct.NewSchedulerRef(selectacct.NewScheduler(nil)),
		UsageScoreTTL: 20 * time.Millisecond,
		Lifecycle:     lifecycle,
		ScoreAccounts: func(context.Context, []accounts.Account) ([]selectacct.Score, int) { return nil, 0 },
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		server.RunUsageScoreRefresher(context.Background())
	}()
	lifecycle.Drain()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("refresher did not stop after drain")
	}
}
