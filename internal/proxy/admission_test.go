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

func TestAdmissionPauseHonorsRetryAfterOnCapacity503(t *testing.T) {
	control := testAdmissionControl(1, 2)
	key := admissionKeyFor(accounts.ProviderCodex, "account-a", "gpt-6-astra")
	now := time.Now()
	control.observe(key, http.StatusServiceUnavailable, http.Header{"Retry-After": []string{"60"}}, now)
	paused := control.snapshot().Accounts[0].PausedUntil
	if paused == "" {
		t.Fatal("capacity 503 did not pause admission")
	}
	got, err := time.Parse(time.RFC3339, paused)
	if err != nil || got.Before(now.Add(59*time.Second)) || got.After(now.Add(61*time.Second)) {
		t.Fatalf("paused until %q, parse error=%v, want about 60s", paused, err)
	}
}

func TestAdmissionPauseUsesClaudeQuotaReset(t *testing.T) {
	control := testAdmissionControl(1, 2)
	key := admissionKeyFor(accounts.ProviderClaude, "account-a", "claude-opus-5-5")
	now := time.Now()
	reset := now.Add(2 * time.Minute).Unix()
	control.observe(key, http.StatusTooManyRequests, http.Header{
		"anthropic-ratelimit-unified-5h-status": []string{"rejected"},
		"anthropic-ratelimit-unified-5h-reset":  []string{formatInt64(reset)},
	}, now)
	paused := control.snapshot().Accounts[0].PausedUntil
	if paused == "" {
		t.Fatal("Claude quota rejection did not pause admission")
	}
	got, err := time.Parse(time.RFC3339, paused)
	if err != nil || got.Before(now.Add(90*time.Second)) {
		t.Fatalf("paused until %q, parse error=%v", paused, err)
	}
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
