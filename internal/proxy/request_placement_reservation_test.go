package proxy

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/selectacct"
)

func TestHTTPFreshPlacementsReserveAcrossConcurrentSnapshots(t *testing.T) {
	testHTTPPlacementsReserveAcrossConcurrentSnapshots(t, false)
}

func TestHTTPStickyMigrationsReserveAcrossConcurrentSnapshots(t *testing.T) {
	testHTTPPlacementsReserveAcrossConcurrentSnapshots(t, true)
}

func testHTTPPlacementsReserveAcrossConcurrentSnapshots(t *testing.T, migrateSticky bool) {
	const requests = 50
	const model = "test-model"
	candidates := []accounts.Account{
		{ID: "a", Provider: accounts.ProviderOpenRouter, AuthMode: accounts.AuthModeAPIKey, Token: "key-a"},
		{ID: "b", Provider: accounts.ProviderOpenRouter, AuthMode: accounts.AuthModeAPIKey, Token: "key-b"},
	}
	var scores []selectacct.Score
	for _, candidate := range candidates {
		score := selectacct.Score{AccountID: candidate.ID, Provider: candidate.Provider, Headroom: 1, ShortHeadroom: 1}
		score.ModelScores = map[string]selectacct.Score{selectacct.ModelKey(model): score}
		scores = append(scores, score)
	}
	sessions := newSessionStore(t)
	if migrateSticky {
		const rejectedID = "rejected"
		candidates = append(candidates, accounts.Account{ID: rejectedID, Provider: accounts.ProviderOpenRouter, AuthMode: accounts.AuthModeAPIKey, Token: "rejected-key"})
		scores = append(scores, selectacct.Score{AccountID: rejectedID, Provider: accounts.ProviderOpenRouter})
		for i := range requests {
			if _, err := sessions.Put("openrouter", fmt.Sprintf("request-%d", i), rejectedID, ""); err != nil {
				t.Fatal(err)
			}
		}
	}
	ref := selectacct.NewSchedulerRef(selectacct.NewScheduler(scores))
	barrier := &leasePlacementSnapshotBarrier{arrived: make(chan struct{}, requests), release: make(chan struct{})}
	counts := map[string]int{}
	var mu sync.Mutex
	handler := Server{
		Accounts: candidates, Sessions: sessions, SchedulerRef: ref,
		Logger: slog.New(barrier), MaxBodyBytes: 1024,
		OpenRouterUpstream: mustParseURL(t, "https://upstream.test/v1"),
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			mu.Lock()
			counts[r.Header.Get("Authorization")]++
			mu.Unlock()
			return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{}`)), Request: r}, nil
		}),
	}.Handler()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	for i := range requests {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req := httptest.NewRequest(http.MethodPost, "/openrouter/chat/completions", strings.NewReader(`{"model":"test-model","messages":[]}`)).WithContext(ctx)
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-Subrouter-Model", model)
			req.Header.Set("X-Subrouter-Agent", "openrouter")
			req.Header.Set("X-Subrouter-Session", fmt.Sprintf("request-%d", i))
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, req)
			if recorder.Code != http.StatusOK {
				t.Errorf("request status = %d: %s", recorder.Code, recorder.Body.String())
			}
		}()
	}
	for range requests {
		select {
		case <-barrier.arrived:
		case <-ctx.Done():
			close(barrier.release)
			wg.Wait()
			t.Fatal("concurrent requests did not reach snapshot barrier")
		}
	}
	close(barrier.release)
	wg.Wait()
	if counts["Bearer rejected-key"] != 0 {
		t.Fatalf("migrated request reached rejected account: %v", counts)
	}
	if counts["Bearer key-a"] == 0 || counts["Bearer key-b"] == 0 {
		t.Fatalf("fresh HTTP requests herded from stale snapshot: %v", counts)
	}
	debits := ref.LiveDebits()
	for _, candidate := range candidates {
		if got, want := debits[selectacct.ScoreKey(candidate.Provider, candidate.ID)], counts[candidate.AuthorizationHeader()]; got != want {
			t.Errorf("live debits for %q = %d, want %d (exactly one per routed request)", candidate.ID, got, want)
		}
	}
}

func TestHTTPPlacementReleasesReservationBeforeUpstreamFailure(t *testing.T) {
	ref := selectacct.NewSchedulerRef(selectacct.NewScheduler(nil))
	handler := Server{
		Accounts: []accounts.Account{{ID: "missing-credential", Provider: accounts.ProviderOpenRouter, AuthMode: accounts.AuthModeAPIKey}},
		Sessions: newSessionStore(t), SchedulerRef: ref, MaxBodyBytes: 1024,
		OpenRouterUpstream: mustParseURL(t, "https://upstream.test/v1"),
		Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			t.Error("request without a credential reached upstream")
			return nil, fmt.Errorf("unexpected upstream request")
		}),
	}.Handler()
	req := httptest.NewRequest(http.MethodPost, "/openrouter/chat/completions", strings.NewReader(`{"model":"test-model","messages":[]}`))
	req.Header.Set("X-Subrouter-Agent", "openrouter")
	req.Header.Set("X-Subrouter-Session", "missing-credential")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", recorder.Code)
	}
	if debits := ref.LiveDebits(); len(debits) != 0 {
		t.Fatalf("request rejected before routing left placement debit: %v", debits)
	}
}
