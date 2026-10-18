package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/selectacct"
	"github.com/manaflow-ai/subrouter/session"
)

// TestClaudeSubscriptionFirstAtProxyBoundary exercises the real HTTP routing
// boundary. Subscription quota must win for new and legacy API-key sticky
// sessions; an explicit account selector remains a pin; once the subscription
// is exhausted the API key is the last-resort fallback.
func TestClaudeSubscriptionFirstAtProxyBoundary(t *testing.T) {
	var seen []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("X-Api-Key"); got != "" {
			seen = append(seen, "key:"+got)
		} else {
			seen = append(seen, "oauth:"+strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
		}
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_subscription_first","type":"message"}`)
	}))
	defer upstream.Close()
	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	server := Server{
		ClaudeUpstream: upstreamURL,
		Accounts: []accounts.Account{
			{ID: "subscription@example.com", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "subscription-token"},
			{ID: "claude:paid-key", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeAPIKey, Token: "paid-key"},
		},
		Sessions:      store,
		SchedulerRef:  selectacct.NewSchedulerRef(selectacct.NewScheduler(nil)),
		UsageScoreTTL: time.Hour,
		MaxBodyBytes:  1 << 20,
	}
	server.SchedulerRef.Set(selectacct.NewScheduler([]selectacct.Score{
		{AccountID: "subscription@example.com", Provider: accounts.ProviderClaude, Headroom: 0.77, ShortHeadroom: 0.77, Fresh: true},
		{AccountID: "claude:paid-key", Provider: accounts.ProviderClaude, Headroom: 0.99, ShortHeadroom: 0.99},
	}))
	handler := httptest.NewServer(server.Handler())
	defer handler.Close()

	request := func(sessionID string, forced string) {
		req := httptest.NewRequest(http.MethodPost, handler.URL+"/v1/messages", strings.NewReader(`{"model":"claude-sonnet-4-6","messages":[]}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Subrouter-Agent", "claude")
		req.Header.Set("X-Subrouter-Session", sessionID)
		if forced != "" {
			req.Header.Set("X-Subrouter-Account-ID", forced)
		}
		response := httptest.NewRecorder()
		handler.Config.Handler.ServeHTTP(response, req)
		if response.Code != http.StatusOK {
			t.Fatalf("session %s status = %d, body = %s", sessionID, response.Code, response.Body.String())
		}
	}

	request("new", "")
	if got := seen[len(seen)-1]; got != "oauth:subscription-token" {
		t.Fatalf("new session used %q, want subscription quota", got)
	}

	if _, err := store.Put("claude", "legacy-key", "claude:paid-key", ""); err != nil {
		t.Fatal(err)
	}
	request("legacy-key", "")
	if got := seen[len(seen)-1]; got != "oauth:subscription-token" {
		t.Fatalf("legacy API-key sticky used %q, want migration to subscription", got)
	}

	request("forced-key", "claude:paid-key")
	if got := seen[len(seen)-1]; got != "key:paid-key" {
		t.Fatalf("forced account used %q, want explicit API-key pin", got)
	}

	server.SchedulerRef.Set(selectacct.NewScheduler([]selectacct.Score{
		{AccountID: "subscription@example.com", Provider: accounts.ProviderClaude, Headroom: 0, ShortHeadroom: 0, WeeklyHeadroom: 0, WeeklyHeadroomKnown: true, Fresh: true},
		{AccountID: "claude:paid-key", Provider: accounts.ProviderClaude, Headroom: 0.99, ShortHeadroom: 0.99},
	}))
	request("last-resort", "")
	if got := seen[len(seen)-1]; got != "key:paid-key" {
		t.Fatalf("exhausted subscription used %q, want API-key last resort", got)
	}
}
