package proxy

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	agentclaude "github.com/manaflow-ai/subrouter/internal/agents/claude"
	"github.com/manaflow-ai/subrouter/selectacct"
)

func testAdmissionControl(limit, queueCap int) *admissionControl {
	return &admissionControl{limit: limit, queueCap: queueCap, buckets: make(map[admissionKey]*admissionBucket)}
}

func TestAdmissionQueuesPhysicalAttemptsAndReleasesFIFO(t *testing.T) {
	control := testAdmissionControl(1, 2)
	key := admissionKeyFor(accounts.ProviderClaude, "account-a", "claude-opus-5-5")
	first, err := control.acquire(context.Background(), key)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	defer first.release()

	secondReady := make(chan *admissionLease, 1)
	go func() {
		lease, _ := control.acquire(context.Background(), key)
		secondReady <- lease
	}()
	select {
	case <-secondReady:
		t.Fatal("second attempt bypassed the account ceiling")
	case <-time.After(20 * time.Millisecond):
	}
	if got := len(control.snapshot().Accounts); got != 1 || control.snapshot().Accounts[0].Queued != 1 {
		t.Fatalf("snapshot while queued = %+v", control.snapshot())
	}

	first.release()
	var second *admissionLease
	select {
	case second = <-secondReady:
	case <-time.After(time.Second):
		t.Fatal("queued attempt was not released")
	}
	if second == nil {
		t.Fatal("queued attempt returned nil lease")
	}
	second.release()
	snapshot := control.snapshot()
	if snapshot.Accounts[0].Active != 0 || snapshot.Accounts[0].Queued != 0 || snapshot.Accounts[0].Served != 2 {
		t.Fatalf("final snapshot = %+v", snapshot)
	}
}

func TestAdmissionPauseHonorsProviderRetryAfterAndCancellation(t *testing.T) {
	control := testAdmissionControl(1, 2)
	key := admissionKeyFor(accounts.ProviderCodex, "account-a", "gpt-6-astra")
	lease, err := control.acquire(context.Background(), key)
	if err != nil {
		t.Fatalf("initial acquire: %v", err)
	}
	lease.release()
	control.observe(key, http.StatusTooManyRequests, http.Header{"Retry-After": []string{"1"}}, time.Now())

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := control.acquire(ctx, key); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("paused acquire error = %v, want deadline exceeded", err)
	}
	if got := control.snapshot().Accounts[0].Blocked; got != 0 {
		t.Fatalf("cancellation counted as queue overflow: %d", got)
	}
}

func TestAdmissionPauseUsesClaudeQuotaReset(t *testing.T) {
	control := testAdmissionControl(1, 2)
	key := admissionKeyFor(accounts.ProviderClaude, "account-a", "claude-opus-5-5")
	now := time.Now()
	reset := now.Add(2 * time.Minute).Unix()
	header := http.Header{}
	header.Set("Anthropic-Ratelimit-Unified-5h-Status", "rejected")
	header.Set("Anthropic-Ratelimit-Unified-5h-Reset", formatInt64(reset))
	control.observe(key, http.StatusTooManyRequests, header, now)
	paused := control.snapshot().Accounts[0].PausedUntil
	if paused == "" {
		t.Fatal("Claude quota rejection did not pause admission")
	}
	got, err := time.Parse(time.RFC3339, paused)
	if err != nil || got.Before(now.Add(90*time.Second)) {
		t.Fatalf("paused until %q, parse error=%v", paused, err)
	}
}

func TestAdmissionClaudeUsageReconcilesExactModelPause(t *testing.T) {
	control := testAdmissionControl(1, 2)
	key := admissionKeyFor(accounts.ProviderClaude, "account-a", "claude-opus")
	pausedAt := time.Now()
	control.pause(key, pausedAt.Add(time.Hour))
	control.reconcileClaudeUsage([]selectacct.Score{{
		AccountID: "account-a", Provider: accounts.ProviderClaude, Fresh: true,
		UsageObservedAt: pausedAt.Add(time.Second),
		ModelScores: map[string]selectacct.Score{
			"claudeopus": {Headroom: 0.97, WeeklyHeadroom: 0.97, WeeklyHeadroomKnown: true, ModelQuotaKnown: true},
		},
	}}, time.Now())
	lease, err := control.acquire(context.Background(), key)
	if err != nil {
		t.Fatalf("fresh positive Opus usage did not clear pause: %v", err)
	}
	lease.release()
}

func TestAdmissionClaudeUsageDoesNotClearStaleOrOtherModelPause(t *testing.T) {
	control := testAdmissionControl(1, 2)
	opus := admissionKeyFor(accounts.ProviderClaude, "account-a", "claude-opus")
	control.pause(opus, time.Now().Add(time.Hour))
	control.reconcileClaudeUsage([]selectacct.Score{{
		AccountID: "account-a", Provider: accounts.ProviderClaude,
		// A stale score and a fresh Sonnet score must not clear Opus.
		ModelScores: map[string]selectacct.Score{
			"claudeopus":    {Headroom: 0.97, WeeklyHeadroom: 0.97, WeeklyHeadroomKnown: true},
			"claude-sonnet": {Headroom: 0.97, WeeklyHeadroom: 0.97, WeeklyHeadroomKnown: true},
		},
	}}, time.Now())
	if _, err := control.acquire(context.Background(), opus); !errors.Is(err, errAdmissionPaused) {
		t.Fatalf("stale/other-model score cleared Opus pause: %v", err)
	}
	control.reconcileClaudeUsage([]selectacct.Score{{
		AccountID: "account-a", Provider: accounts.ProviderClaude, Fresh: true,
		ModelScores: map[string]selectacct.Score{
			"claudeopus": {Headroom: 0, WeeklyHeadroom: 0, WeeklyHeadroomKnown: true},
		},
	}}, time.Now())
	if _, err := control.acquire(context.Background(), opus); !errors.Is(err, errAdmissionPaused) {
		t.Fatalf("zero fresh Opus score cleared genuine pause: %v", err)
	}
}

func TestAdmissionClaudeUsagePreservesLongerTransientHold(t *testing.T) {
	control := testAdmissionControl(1, 2)
	key := admissionKeyFor(accounts.ProviderClaude, "account-a", "claude-opus")
	now := time.Now()
	control.pauseWithRecovery(key, now.Add(10*time.Hour), true)
	control.pauseWithRecovery(key, now.Add(time.Hour), false)
	control.reconcileClaudeUsage([]selectacct.Score{{
		AccountID: "account-a", Provider: accounts.ProviderClaude, Fresh: true,
		UsageObservedAt: now.Add(time.Minute),
		ModelScores: map[string]selectacct.Score{
			"claudeopus": {Headroom: 0.9, WeeklyHeadroom: 0.9, WeeklyHeadroomKnown: true, ModelQuotaKnown: true},
		},
	}}, now.Add(2*time.Minute))
	control.mu.Lock()
	got := control.buckets[key].pausedTil
	control.mu.Unlock()
	if got.Before(now.Add(9 * time.Hour)) {
		t.Fatalf("clearing quota hold discarded longer transient hold: paused until %v", got)
	}
}

func TestUsageStatusRefreshReconcilesClaudeAdmissionPause(t *testing.T) {
	control := testAdmissionControl(1, 2)
	key := admissionKeyFor(accounts.ProviderClaude, "account-a", "claude-opus")
	control.pause(key, time.Now().Add(time.Hour))
	server := Server{
		Accounts: []accounts.Account{{
			ID: "account-a", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth,
		}},
		SchedulerRef: selectacct.NewSchedulerRef(selectacct.NewScheduler([]selectacct.Score{{
			AccountID: "account-a", Provider: accounts.ProviderClaude, Headroom: 1, ShortHeadroom: 1,
		}})),
		admission: control,
	}
	server.updateSchedulerFromUsageStatuses([]AccountUsageStatus{{
		AccountStatus:  AccountStatus{ID: "account-a", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth},
		UsageFresh:     true,
		UsageFetchedAt: time.Now().Add(time.Minute),
		Windows: []accounts.UsageWindow{
			{Name: "5h", UsedPercent: 10, LimitWindowSeconds: 5 * 60 * 60},
			{Name: "7d", UsedPercent: 40, LimitWindowSeconds: 7 * 24 * 60 * 60},
			{Name: "opus-weekly", Feature: agentclaude.OpusFeature, UsedPercent: 20, LimitWindowSeconds: 7 * 24 * 60 * 60},
		},
	}})
	lease, err := control.acquire(context.Background(), key)
	if err != nil {
		t.Fatalf("fresh usage-status refresh left Claude admission paused: %v", err)
	}
	lease.release()
}

func TestInflightAttemptTransportGatesResponseLifetime(t *testing.T) {
	control := testAdmissionControl(1, 2)
	var active, maximum atomic.Int32
	entered := make(chan struct{})
	base := admissionRoundTripFunc(func(*http.Request) (*http.Response, error) {
		current := active.Add(1)
		for {
			old := maximum.Load()
			if current <= old || maximum.CompareAndSwap(old, current) {
				break
			}
		}
		if current == 1 {
			close(entered)
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("ok"))}, nil
	})
	server := &Server{admission: control}
	transport := inflightAttemptTransport{
		base: base,
		attempt: &upstreamAttempt{
			server:    server,
			provider:  accounts.ProviderClaude,
			poolModel: "claude-opus-5-5",
			account:   accounts.Account{ID: "account-a", Provider: accounts.ProviderClaude},
		},
	}
	first, err := transport.RoundTrip(httptest.NewRequest(http.MethodPost, "http://upstream/v1/messages", nil))
	if err != nil {
		t.Fatalf("first round trip: %v", err)
	}
	<-entered
	secondDone := make(chan *http.Response, 1)
	go func() {
		response, _ := transport.RoundTrip(httptest.NewRequest(http.MethodPost, "http://upstream/v1/messages", nil))
		secondDone <- response
	}()
	select {
	case <-secondDone:
		t.Fatal("second physical attempt bypassed admission")
	case <-time.After(20 * time.Millisecond):
	}
	if got := maximum.Load(); got != 1 {
		t.Fatalf("maximum upstream concurrency = %d, want 1", got)
	}
	_ = first.Body.Close()
	select {
	case second := <-secondDone:
		if second == nil {
			t.Fatal("second round trip returned nil response")
		}
		_ = second.Body.Close()
	case <-time.After(time.Second):
		t.Fatal("second physical attempt did not run after response close")
	}
}

type admissionRoundTripFunc func(*http.Request) (*http.Response, error)

func (f admissionRoundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func formatInt64(value int64) string {
	return strconv.FormatInt(value, 10)
}
