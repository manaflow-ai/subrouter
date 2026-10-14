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
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/selectacct"
	"github.com/manaflow-ai/subrouter/session"
)

// TestNewSessionsIgnoreEndedSessionsOnAnAccount drives Server.Handler() with
// two equally full Claude accounts, one of which still holds 40 retained
// assignments from sessions that ended hours ago. Placement used to count
// every retained assignment as load (2026-10-14: 40-57 "sessions" per account
// against 1-6 requests in flight), so the account with old history got almost
// no new sessions. Only sessions active within the last 30 minutes count now,
// so new sessions split across both accounts.
func TestNewSessionsIgnoreEndedSessionsOnAnAccount(t *testing.T) {
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

	path := filepath.Join(t.TempDir(), "sessions.json")
	ended := time.Now().UTC().Add(-3 * time.Hour)
	retained := map[string]session.Assignment{}
	for i := 0; i < 40; i++ {
		id := fmt.Sprintf("ended-%02d", i)
		retained[session.ScopedSessionKey("claude", id)] = session.Assignment{
			AgentType: "claude", SessionID: id, AccountID: "history@example.com", CreatedAt: ended, UpdatedAt: ended,
		}
	}
	body, err := json.Marshal(retained)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := session.NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := SchedulerSessionCounts(store)[selectacct.ScoreKey(accounts.ProviderClaude, "history@example.com")]; got != 40 {
		t.Fatalf("retained assignments = %d, want 40", got)
	}

	handler := Server{
		ClaudeUpstream: upstreamURL,
		Accounts: []accounts.Account{
			{ID: "history@example.com", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "tok-history"},
			{ID: "fresh@example.com", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "tok-fresh"},
		},
		Sessions: store,
		SchedulerRef: selectacct.NewSchedulerRef(selectacct.NewScheduler([]selectacct.Score{
			{AccountID: "history@example.com", Provider: accounts.ProviderClaude, Headroom: 0.9, ShortHeadroom: 0.9},
			{AccountID: "fresh@example.com", Provider: accounts.ProviderClaude, Headroom: 0.9, ShortHeadroom: 0.9},
		})),
		MaxBodyBytes: 1 << 20,
	}.Handler()
	subrouter := httptest.NewServer(handler)
	defer subrouter.Close()

	const placements = 40
	for i := 0; i < placements; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, subrouter.URL+"/v1/messages", strings.NewReader(`{"model":"claude-opus-5-5","messages":[]}`))
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Subrouter-Agent", "claude")
		req.Header.Set("X-Subrouter-Session", fmt.Sprintf("new-%02d", i))
		response, err := http.DefaultClient.Do(req)
		if err != nil {
			cancel()
			t.Fatalf("request %d: %v", i, err)
		}
		_, _ = io.Copy(io.Discard, response.Body)
		response.Body.Close()
		cancel()
		if response.StatusCode != http.StatusOK {
			t.Fatalf("request %d status = %d", i, response.StatusCode)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if hits["history"] < placements/4 || hits["fresh"] < placements/4 {
		t.Fatalf("new sessions per account = %v, want both accounts to take at least a quarter of %d", hits, placements)
	}
}
