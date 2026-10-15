package proxy

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/selectacct"
	"github.com/manaflow-ai/subrouter/session"
)

// TestNewWorkerRoutesOnSavedUsageBeforeFirstSweep is the worker-swap path:
// the previous worker saved its usage snapshot, a new worker restores it and
// serves Claude traffic before its first live usage sweep (stubbed here to
// produce nothing). Without seeding, every Claude account read as fully
// healthy, so new sessions landed on a weekly-exhausted account and walked
// the pool one 429 at a time. With seeding they must avoid the account whose
// saved weekly window is still exhausted, while an account whose window has
// reset since the save is usable again.
func TestNewWorkerRoutesOnSavedUsageBeforeFirstSweep(t *testing.T) {
	var mu sync.Mutex
	hits := map[string]int{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		mu.Lock()
		hits[strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer tok-")]++
		mu.Unlock()
		_, _ = w.Write([]byte(`{"id":"msg_ok","type":"message"}`))
	}))
	defer upstream.Close()
	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	pool := []accounts.Account{
		{ID: "cooked", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "tok-cooked"},
		{ID: "roomy", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "tok-roomy"},
		{ID: "recovered", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "tok-recovered"},
	}
	store := accounts.CodexStore{Dir: filepath.Join(dir, "codex-accounts")}
	now := time.Now()
	observed := now.Add(-5 * time.Minute)
	window := func(name string, used float64, length time.Duration, resetAt time.Time) accounts.UsageWindow {
		return accounts.UsageWindow{Name: name, UsedPercent: used, LimitWindowSeconds: int64(length / time.Second), ResetAt: resetAt}
	}
	status := func(id string, windows ...accounts.UsageWindow) AccountUsageStatus {
		return AccountUsageStatus{
			AccountStatus:  AccountStatus{ID: id, Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth},
			Windows:        windows,
			UsageFetchedAt: observed,
		}
	}
	previous := &AccountRef{accounts: pool, store: store}
	previous.persistUsageStatusSnapshot([]AccountUsageStatus{
		status("cooked", window("5h", 0, 5*time.Hour, now.Add(4*time.Hour)), window("7d", 100, 7*24*time.Hour, now.Add(72*time.Hour))),
		status("roomy", window("5h", 10, 5*time.Hour, now.Add(3*time.Hour)), window("7d", 20, 7*24*time.Hour, now.Add(96*time.Hour))),
		status("recovered", window("5h", 0, 5*time.Hour, now.Add(4*time.Hour)), window("7d", 100, 7*24*time.Hour, now.Add(-time.Minute))),
	})

	restarted := &AccountRef{accounts: pool, store: store}
	restarted.restoreUsageStatusSnapshot()
	sessions, err := session.NewStore(filepath.Join(dir, "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	server := Server{
		ClaudeUpstream: upstreamURL,
		AccountRef:     restarted,
		Sessions:       sessions,
		SchedulerRef:   selectacct.NewSchedulerRef(selectacct.NewScheduler(nil)),
		// The first live sweep has not produced anything yet.
		ScoreAccounts: func(context.Context, []accounts.Account) ([]selectacct.Score, int) { return nil, 0 },
		RefreshAccountFn: func(_ context.Context, account accounts.Account) (accounts.Account, error) {
			return account, nil
		},
		MaxBodyBytes: 1 << 20,
	}
	if seeded := server.SeedScoresFromRestoredUsage(now); seeded != 3 {
		t.Fatalf("seeded %d accounts from the saved snapshot, want 3", seeded)
	}
	subrouter := httptest.NewServer(server.Handler())
	defer subrouter.Close()

	for i := 0; i < 40; i++ {
		req, err := http.NewRequest(http.MethodPost, subrouter.URL+"/v1/messages", strings.NewReader(`{"model":"claude-opus-5-5","messages":[]}`))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Subrouter-Agent", "claude")
		req.Header.Set("X-Subrouter-Session", fmt.Sprintf("new-session-%d", i))
		response, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(response.Body)
		response.Body.Close()
		if response.StatusCode != http.StatusOK {
			t.Fatalf("request %d status = %d body=%.300s", i, response.StatusCode, body)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if hits["cooked"] != 0 {
		t.Fatalf("new sessions reached the weekly-exhausted account before the first sweep: %v", hits)
	}
	if hits["roomy"] == 0 || hits["recovered"] == 0 {
		t.Fatalf("hits %v, want both the roomy account and the one whose window reset since the save", hits)
	}
}
