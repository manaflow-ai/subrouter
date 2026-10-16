package proxy

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/selectacct"
)

func websocketPlacementRequest(ctx context.Context, path, sessionID string) *http.Request {
	req := httptest.NewRequest(http.MethodGet, path, nil).WithContext(ctx)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Sec-WebSocket-Version", "13")
	req.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
	req.Header.Set("X-Subrouter-Session", sessionID)
	return req
}

func TestWebSocketBurstReservesDuringCredentialPreparation(t *testing.T) {
	const requests = 50
	candidates := []accounts.Account{
		{ID: "a", Provider: accounts.ProviderOpenRouter, AuthMode: accounts.AuthModeAPIKey, Token: "key-a"},
		{ID: "b", Provider: accounts.ProviderOpenRouter, AuthMode: accounts.AuthModeAPIKey, Token: "key-b"},
	}
	var scores []selectacct.Score
	for _, account := range candidates {
		scores = append(scores, selectacct.Score{AccountID: account.ID, Provider: account.Provider, Headroom: 1, ShortHeadroom: 1})
	}
	ref := selectacct.NewSchedulerRef(selectacct.NewScheduler(scores))
	arrived := make(chan string, requests)
	release := make(chan struct{})
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	handler := Server{
		Accounts: candidates, Sessions: newSessionStore(t), SchedulerRef: ref,
		OpenRouterUpstream: mustParseURL(t, "https://upstream.invalid/v1"), MaxBodyBytes: 1024,
		RefreshAccountFn: func(ctx context.Context, account accounts.Account) (accounts.Account, error) {
			arrived <- account.ID
			select {
			case <-release:
				return account, nil
			case <-ctx.Done():
				return account, ctx.Err()
			}
		},
	}.Handler()
	var wg sync.WaitGroup
	for i := range requests {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req := websocketPlacementRequest(ctx, "/openrouter/chat/completions", fmt.Sprintf("socket-%d", i))
			req.Header.Set("X-Subrouter-Agent", "openrouter")
			req.Header.Set("Origin", "https://different-origin.invalid")
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, req)
			if recorder.Code != http.StatusForbidden {
				t.Errorf("origin rejection status = %d, want 403", recorder.Code)
			}
		}()
	}
	counts := map[string]int{}
	for range requests {
		select {
		case id := <-arrived:
			counts[id]++
		case <-ctx.Done():
			close(release)
			wg.Wait()
			t.Fatal("WebSocket preparations did not reach barrier")
		}
	}
	debits := ref.LiveDebits()
	for _, account := range candidates {
		if counts[account.ID] == 0 {
			t.Errorf("socket burst did not use %q: %v", account.ID, counts)
		}
		if got := debits[selectacct.ScoreKey(account.Provider, account.ID)]; got != counts[account.ID] {
			t.Errorf("pending WebSocket load for %q = %d, want %d before refresh finishes", account.ID, got, counts[account.ID])
		}
	}
	close(release)
	wg.Wait()
	if got := ref.LiveDebits(); len(got) != 0 {
		t.Fatalf("rejected connections left routed or pending debit: %v", got)
	}
}

func TestWebSocketStartupFailureReleasesPlacement(t *testing.T) {
	for _, failure := range []string{"origin", "refresh", "admission", "dial"} {
		t.Run(failure, func(t *testing.T) {
			var upstreamCalls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				upstreamCalls.Add(1)
				http.Error(w, "upstream rejected handshake", http.StatusServiceUnavailable)
			}))
			defer upstream.Close()
			account := accounts.Account{ID: "a", Provider: accounts.ProviderCodex, AuthMode: accounts.AuthModeOAuth, Token: "token"}
			ref := selectacct.NewSchedulerRef(selectacct.NewScheduler([]selectacct.Score{{AccountID: account.ID, Provider: account.Provider, Headroom: 1, ShortHeadroom: 1}}))
			admission := newAdmissionControl()
			if failure == "admission" {
				admission.pause(admissionKeyFor(account.Provider, account.ID, ""), time.Now().Add(time.Minute))
			}
			handler := Server{
				Accounts: []accounts.Account{account}, Sessions: newSessionStore(t), SchedulerRef: ref,
				Upstream: mustParseURL(t, upstream.URL), MaxBodyBytes: 1024, admission: admission,
				RefreshAccountFn: func(_ context.Context, account accounts.Account) (accounts.Account, error) {
					if got := ref.LiveDebits()[selectacct.ScoreKey(account.Provider, account.ID)]; got != 1 {
						t.Errorf("pending load before refresh = %d, want 1", got)
					}
					if failure == "refresh" {
						return account, errors.New("temporary credential failure")
					}
					return account, nil
				},
			}.Handler()
			req := websocketPlacementRequest(t.Context(), "/v1/responses", "startup-"+failure)
			if failure == "origin" {
				req.Header.Set("Origin", "https://different-origin.invalid")
			}
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, req)
			wantStatus := http.StatusServiceUnavailable
			if failure == "origin" {
				wantStatus = http.StatusForbidden
			}
			if recorder.Code != wantStatus {
				t.Errorf("status = %d, want %d", recorder.Code, wantStatus)
			}
			wantCalls := int32(0)
			if failure == "dial" {
				wantCalls = 1
			}
			if got := upstreamCalls.Load(); got != wantCalls {
				t.Errorf("upstream calls = %d, want %d", got, wantCalls)
			}
			if got := ref.LiveDebits(); len(got) != 0 {
				t.Errorf("failed connection left pending/routed debit: %v", got)
			}
			if got := ref.InflightCounts(); len(got) != 0 {
				t.Errorf("failed connection left inflight load: %v", got)
			}
		})
	}
}

func TestWebSocketDialCancellationHandsOffPendingLoad(t *testing.T) {
	started := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
	}))
	defer upstream.Close()
	account := accounts.Account{ID: "a", Provider: accounts.ProviderCodex, AuthMode: accounts.AuthModeOAuth, Token: "token"}
	ref := selectacct.NewSchedulerRef(selectacct.NewScheduler(nil))
	handler := Server{Accounts: []accounts.Account{account}, Sessions: newSessionStore(t), SchedulerRef: ref, Upstream: mustParseURL(t, upstream.URL), MaxBodyBytes: 1024}.Handler()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		handler.ServeHTTP(httptest.NewRecorder(), websocketPlacementRequest(ctx, "/v1/responses", "cancel-dial"))
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("upstream handshake did not start")
	}
	if got := ref.LiveDebits(); len(got) != 0 {
		t.Errorf("pending load not handed to inflight: %v", got)
	}
	if got := ref.InflightCounts()[selectacct.ScoreKey(account.Provider, account.ID)]; got != 1 {
		t.Errorf("inflight handoff = %d, want 1", got)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("cancellation did not stop upstream handshake")
	}
	if got := ref.LiveDebits(); len(got) != 0 {
		t.Errorf("canceled dial left routed debit: %v", got)
	}
	if got := ref.InflightCounts(); len(got) != 0 {
		t.Errorf("canceled dial left inflight load: %v", got)
	}
}
