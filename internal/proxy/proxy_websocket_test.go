package proxy

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/manaflow-ai/subrouter/internal/accounts"
	agentclaude "github.com/manaflow-ai/subrouter/internal/agents/claude"
	"github.com/manaflow-ai/subrouter/selectacct"
	"github.com/manaflow-ai/subrouter/session"
	"github.com/manaflow-ai/subrouter/internal/transcript"
)

func TestHandlerProxiesWebSocketWithSelectedAccountAuth(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(_ *http.Request) bool { return true }}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !websocket.IsWebSocketUpgrade(r) {
			t.Fatalf("expected websocket upgrade")
		}
		if got := r.Header.Get("Authorization"); got != "Bearer selected-token" {
			t.Fatalf("Authorization = %q, want selected token", got)
		}
		if got := r.Header.Get("ChatGPT-Account-ID"); got != "acct-1" {
			t.Fatalf("ChatGPT-Account-ID = %q, want acct-1", got)
		}
		if got := r.Header.Get("x-codex-window-id"); got != "window-1" {
			t.Fatalf("x-codex-window-id = %q, want window-1", got)
		}

		conn, err := upgrader.Upgrade(w, r, http.Header{"x-codex-turn-state": []string{"turn-1"}})
		if err != nil {
			t.Fatalf("upgrade: %v", err)
		}
		defer conn.Close()
		if err := conn.WriteMessage(websocket.TextMessage, []byte("ok")); err != nil {
			t.Fatalf("write message: %v", err)
		}
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
	handler := Server{
		Upstream: upstreamURL,
		Accounts: []accounts.Account{{
			ID:        "a@example.com",
			AuthMode:  accounts.AuthModeOAuth,
			Token:     "selected-token",
			AccountID: "acct-1",
		}},
		Sessions:     store,
		Scheduler:    selectacct.NewScheduler(nil),
		MaxBodyBytes: 1024,
	}.Handler()
	subrouter := httptest.NewServer(handler)
	defer subrouter.Close()

	wsURL := "ws" + strings.TrimPrefix(subrouter.URL, "http") + "/v1/responses"
	header := http.Header{"x-codex-window-id": []string{"window-1"}}
	conn, response, err := websocket.DefaultDialer.Dial(wsURL, header)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer response.Body.Close()
	defer conn.Close()
	if got := response.Header.Get("x-codex-turn-state"); got != "turn-1" {
		t.Fatalf("x-codex-turn-state = %q, want turn-1", got)
	}

	_, body, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read message: %v", err)
	}
	if string(body) != "ok" {
		t.Fatalf("message = %q, want ok", string(body))
	}
}

func TestHandlerProxiesWebSocketWhenAllOAuthAccountsBelowProtectedHeadroom(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(_ *http.Request) bool { return true }}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !websocket.IsWebSocketUpgrade(r) {
			t.Fatalf("expected websocket upgrade")
		}
		if got := r.Header.Get("Authorization"); got != "Bearer best-low-token" {
			t.Fatalf("Authorization = %q, want best low-headroom token", got)
		}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Fatalf("upgrade: %v", err)
		}
		defer conn.Close()
		if err := conn.WriteMessage(websocket.TextMessage, []byte("ok")); err != nil {
			t.Fatalf("write message: %v", err)
		}
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
	handler := Server{
		Upstream: upstreamURL,
		Accounts: []accounts.Account{
			{ID: "lowest@example.com", AuthMode: accounts.AuthModeOAuth, Token: "lowest-token"},
			{ID: "best-low@example.com", AuthMode: accounts.AuthModeOAuth, Token: "best-low-token"},
		},
		Sessions: store,
		Scheduler: selectacct.NewScheduler([]selectacct.Score{
			{AccountID: "lowest@example.com", Headroom: 0.12, ShortHeadroom: 0.12},
			{AccountID: "best-low@example.com", Headroom: 0.39, ShortHeadroom: 0.39},
		}),
		MaxBodyBytes: 1024,
	}.Handler()
	subrouter := httptest.NewServer(handler)
	defer subrouter.Close()

	wsURL := "ws" + strings.TrimPrefix(subrouter.URL, "http") + "/v1/responses"
	conn, response, err := websocket.DefaultDialer.Dial(wsURL, http.Header{"X-Codex-Session-ID": []string{"low-headroom-session"}})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer response.Body.Close()
	defer conn.Close()

	_, body, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read message: %v", err)
	}
	if string(body) != "ok" {
		t.Fatalf("message = %q, want ok", string(body))
	}
	assignment, ok := store.Get("codex", "low-headroom-session")
	if !ok {
		t.Fatal("missing low-headroom session assignment")
	}
	if assignment.AccountID != "best-low@example.com" {
		t.Fatalf("AccountID = %q, want best-low@example.com", assignment.AccountID)
	}
}

func TestHandlerMapsV1RequestsToCodexBackendPaths(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Path; got != "/backend-api/codex/responses" {
			t.Fatalf("path = %q, want /backend-api/codex/responses", got)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer oauth-token" {
			t.Fatalf("Authorization = %q, want oauth token", got)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()

	codexUpstream, err := url.Parse(upstream.URL + "/backend-api/codex")
	if err != nil {
		t.Fatal(err)
	}
	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	handler := Server{
		CodexUpstream: codexUpstream,
		Accounts: []accounts.Account{{
			ID:       "codex-account",
			AuthMode: accounts.AuthModeOAuth,
			Token:    "oauth-token",
		}},
		Sessions:     store,
		Scheduler:    selectacct.NewScheduler(nil),
		MaxBodyBytes: 1024,
	}.Handler()
	subrouter := httptest.NewServer(handler)
	defer subrouter.Close()

	response, err := http.Post(subrouter.URL+"/v1/responses", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if _, err := io.Copy(io.Discard, response.Body); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", response.StatusCode)
	}
}

func TestHandlerProxiesClaudeOAuthSubscriptionRequest(t *testing.T) {
	requestBody := []byte(`{
		"model": "claude-haiku-4-5",
		"max_tokens": 8,
		"system": [
			{
				"type": "text",
				"text": "stable project instructions",
				"cache_control": {"type": "ephemeral", "ttl": "1h"}
			}
		],
		"messages": [
			{"role": "user", "content": "ping"}
		]
	}`)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Path; got != "/v1/messages" {
			t.Fatalf("path = %q, want /v1/messages", got)
		}
		if got := r.URL.Query().Get("beta"); got != "true" {
			t.Fatalf("beta query = %q, want true", got)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer claude-profile-token" {
			t.Fatalf("Authorization = %q, want Claude profile token", got)
		}
		beta := r.Header.Get("Anthropic-Beta")
		if !strings.Contains(beta, "claude-code-20250219") {
			t.Fatalf("Anthropic-Beta = %q, missing Claude Code beta", beta)
		}
		if !strings.Contains(beta, claudeOAuthBetaHeader) {
			t.Fatalf("Anthropic-Beta = %q, missing OAuth beta", beta)
		}
		if !strings.Contains(beta, "extended-cache-ttl-2025-04-11") {
			t.Fatalf("Anthropic-Beta = %q, missing extended cache TTL beta", beta)
		}
		if got := r.Header.Get("ChatGPT-Account-ID"); got != "" {
			t.Fatalf("ChatGPT-Account-ID = %q, want empty", got)
		}
		if got := r.Header.Get("X-Api-Key"); got != "" {
			t.Fatalf("X-Api-Key = %q, want empty", got)
		}
		if got := r.Header.Get("X-Subrouter-Agent"); got != "" {
			t.Fatalf("X-Subrouter-Agent leaked upstream: %q", got)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Contains(body, []byte(`"cache_control"`)) {
			t.Fatalf("body = %s, missing cache_control", body)
		}
		if !bytes.Contains(body, []byte(`"ttl": "1h"`)) {
			t.Fatalf("body = %s, missing 1h cache TTL", body)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()

	claudeUpstream, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	handler := Server{
		ClaudeUpstream: claudeUpstream,
		Accounts: []accounts.Account{{
			ID:       "work",
			Provider: accounts.ProviderClaude,
			AuthMode: accounts.AuthModeOAuth,
			Token:    "claude-profile-token",
		}},
		Sessions:     store,
		Scheduler:    selectacct.NewScheduler(nil),
		MaxBodyBytes: 1024,
	}.Handler()
	subrouter := httptest.NewServer(handler)
	defer subrouter.Close()

	req, err := http.NewRequest(http.MethodPost, subrouter.URL+"/v1/messages?beta=true", bytes.NewReader(requestBody))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer client-token")
	req.Header.Set("Anthropic-Beta", "claude-code-20250219,extended-cache-ttl-2025-04-11")
	req.Header.Set("X-Api-Key", "client-key")
	req.Header.Set("X-Claude-Code-Session-Id", "claude-session")
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if _, err := io.Copy(io.Discard, response.Body); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", response.StatusCode)
	}
	assignment, ok := store.Get("claude", "claude-session")
	if !ok {
		t.Fatal("missing Claude session assignment")
	}
	if assignment.AccountID != "work" {
		t.Fatalf("AccountID = %q, want work", assignment.AccountID)
	}
}

func TestHandlerKeepsClaudeConversationOnSameAccount(t *testing.T) {
	seen := make(chan string, 3)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Path; got != "/v1/messages" {
			t.Fatalf("path = %q, want /v1/messages", got)
		}
		seen <- r.Header.Get("Authorization")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()

	claudeUpstream, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	handler := Server{
		ClaudeUpstream: claudeUpstream,
		Accounts: []accounts.Account{
			{ID: "claude-a", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "claude-a-token"},
			{ID: "claude-b", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "claude-b-token"},
		},
		Sessions:     store,
		Scheduler:    selectacct.NewScheduler(nil),
		MaxBodyBytes: 1024,
	}.Handler()
	subrouter := httptest.NewServer(handler)
	defer subrouter.Close()

	postClaudeMessage := func(sessionID string) {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, subrouter.URL+"/v1/messages", strings.NewReader(`{}`))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("User-Agent", "claude-cli/2.1.150 (external, sdk-cli)")
		req.Header.Set("X-Claude-Code-Session-Id", sessionID)
		response, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		if _, err := io.Copy(io.Discard, response.Body); err != nil {
			t.Fatal(err)
		}
		if response.StatusCode != http.StatusNoContent {
			t.Fatalf("status = %d, want 204", response.StatusCode)
		}
	}

	postClaudeMessage("claude-session")
	postClaudeMessage("claude-session")
	postClaudeMessage("other-claude-session")

	first := <-seen
	second := <-seen
	third := <-seen
	if first != "Bearer claude-a-token" {
		t.Fatalf("first Authorization = %q, want claude-a", first)
	}
	if second != first {
		t.Fatalf("same Claude session switched accounts: first %q, second %q", first, second)
	}
	if third != "Bearer claude-b-token" {
		t.Fatalf("new Claude session Authorization = %q, want claude-b", third)
	}
}

func TestHandlerReroutesClaudeWhenStickyAccountRefreshFails(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer good-token" {
			t.Fatalf("Authorization = %q, want good token", got)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()
	claudeUpstream, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}

	claudeStore := agentclaude.Store{Dir: t.TempDir()}
	badDir, err := claudeStore.CreateProfile("bad-profile")
	if err != nil {
		t.Fatal(err)
	}
	goodDir, err := claudeStore.CreateProfile("good-profile")
	if err != nil {
		t.Fatal(err)
	}
	writeClaudeCredential(t, badDir, agentclaude.CredentialInfo{
		AccessToken:  "bad-token",
		RefreshToken: "bad-refresh",
		ExpiresAt:    time.Now().Add(-time.Hour).UnixMilli(),
	})
	writeClaudeCredential(t, goodDir, agentclaude.CredentialInfo{
		AccessToken:  "good-token",
		RefreshToken: "good-refresh",
		ExpiresAt:    time.Now().Add(time.Hour).UnixMilli(),
	})

	refreshClient := &http.Client{Transport: proxyRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusBadRequest,
			Status:     "400 Bad Request",
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{"error":"invalid_grant","error_description":"Refresh token not found or invalid"}`)),
			Request:    req,
		}, nil
	})}
	accountRef := NewAccountRef(accounts.CodexStore{Dir: t.TempDir()}, nil, refreshClient)
	accountRef.claudeStore = claudeStore
	loaded, err := claudeStore.ListAccounts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	accountRef.accounts = loaded

	sessionStore, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sessionStore.Put("claude", "claude-session", "bad-profile", ""); err != nil {
		t.Fatal(err)
	}
	schedulerRef := selectacct.NewSchedulerRef(selectacct.NewScheduler([]selectacct.Score{
		{AccountID: "bad-profile", Headroom: 0.9, ShortHeadroom: 0.9},
		{AccountID: "good-profile", Headroom: 0.8, ShortHeadroom: 0.8},
	}))
	handler := Server{
		ClaudeUpstream: claudeUpstream,
		AccountRef:     accountRef,
		Sessions:       sessionStore,
		SchedulerRef:   schedulerRef,
		MaxBodyBytes:   1024,
	}.Handler()
	subrouter := httptest.NewServer(handler)
	defer subrouter.Close()

	req, err := http.NewRequest(http.MethodPost, subrouter.URL+"/v1/messages", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Subrouter-Agent", "claude")
	req.Header.Set("X-Claude-Code-Session-Id", "claude-session")
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if _, err := io.Copy(io.Discard, response.Body); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", response.StatusCode)
	}
	assignment, ok := sessionStore.Get("claude", "claude-session")
	if !ok {
		t.Fatal("missing Claude session assignment")
	}
	if assignment.AccountID != "good-profile" {
		t.Fatalf("sticky AccountID = %q, want good-profile", assignment.AccountID)
	}
}

func TestHandlerRoutesCodexAndClaudeProvidersSeparately(t *testing.T) {
	seen := make(chan string, 2)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/backend-api/codex/responses":
			if got := r.Header.Get("Authorization"); got != "Bearer codex-token" {
				t.Fatalf("Codex Authorization = %q, want codex token", got)
			}
			seen <- "codex"
		case "/v1/messages":
			if got := r.Header.Get("Authorization"); got != "Bearer claude-token" {
				t.Fatalf("Claude Authorization = %q, want claude token", got)
			}
			if got := r.Header.Get("ChatGPT-Account-ID"); got != "" {
				t.Fatalf("Claude ChatGPT-Account-ID = %q, want empty", got)
			}
			seen <- "claude"
		default:
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()

	codexUpstream, err := url.Parse(upstream.URL + "/backend-api/codex")
	if err != nil {
		t.Fatal(err)
	}
	claudeUpstream, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	handler := Server{
		CodexUpstream:  codexUpstream,
		ClaudeUpstream: claudeUpstream,
		Accounts: []accounts.Account{
			{ID: "codex-account", Provider: accounts.ProviderCodex, AuthMode: accounts.AuthModeOAuth, Token: "codex-token"},
			{ID: "claude-account", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "claude-token"},
		},
		Sessions:     store,
		Scheduler:    selectacct.NewScheduler(nil),
		MaxBodyBytes: 1024,
	}.Handler()
	subrouter := httptest.NewServer(handler)
	defer subrouter.Close()

	codexReq, err := http.NewRequest(http.MethodPost, subrouter.URL+"/v1/responses", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	codexReq.Header.Set("X-Codex-Session-ID", "codex-session")
	codexResp, err := http.DefaultClient.Do(codexReq)
	if err != nil {
		t.Fatal(err)
	}
	defer codexResp.Body.Close()
	if _, err := io.Copy(io.Discard, codexResp.Body); err != nil {
		t.Fatal(err)
	}
	if codexResp.StatusCode != http.StatusNoContent {
		t.Fatalf("Codex status = %d, want 204", codexResp.StatusCode)
	}

	claudeReq, err := http.NewRequest(http.MethodPost, subrouter.URL+"/v1/messages", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	claudeReq.Header.Set("X-Claude-Code-Session-Id", "claude-session")
	claudeResp, err := http.DefaultClient.Do(claudeReq)
	if err != nil {
		t.Fatal(err)
	}
	defer claudeResp.Body.Close()
	if _, err := io.Copy(io.Discard, claudeResp.Body); err != nil {
		t.Fatal(err)
	}
	if claudeResp.StatusCode != http.StatusNoContent {
		t.Fatalf("Claude status = %d, want 204", claudeResp.StatusCode)
	}

	got := []string{<-seen, <-seen}
	sort.Strings(got)
	if strings.Join(got, ",") != "claude,codex" {
		t.Fatalf("seen = %v, want claude and codex", got)
	}
}

func TestHandlerHandlesBaseURLHeadProbeLocally(t *testing.T) {
	upstreamCalled := false
	codexUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalled = true
		w.WriteHeader(http.StatusNoContent)
	}))
	defer codexUpstream.Close()
	claudeUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalled = true
		w.WriteHeader(http.StatusNoContent)
	}))
	defer claudeUpstream.Close()

	codexURL, err := url.Parse(codexUpstream.URL + "/backend-api/codex")
	if err != nil {
		t.Fatal(err)
	}
	claudeURL, err := url.Parse(claudeUpstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	handler := Server{
		CodexUpstream:  codexURL,
		ClaudeUpstream: claudeURL,
		Accounts: []accounts.Account{
			{ID: "codex-account", Provider: accounts.ProviderCodex, AuthMode: accounts.AuthModeOAuth, Token: "codex-token"},
			{ID: "claude-account", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "claude-token"},
		},
		Sessions:     store,
		Scheduler:    selectacct.NewScheduler(nil),
		MaxBodyBytes: 1024,
	}.Handler()
	subrouter := httptest.NewServer(handler)
	defer subrouter.Close()

	req, err := http.NewRequest(http.MethodHead, subrouter.URL+"/", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("User-Agent", "claude-cli/2.1.141 (external, sdk-cli)")
	req.Header.Set("Authorization", "Bearer client-token")
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", response.StatusCode)
	}
	if upstreamCalled {
		t.Fatal("base URL HEAD probe was proxied upstream")
	}
}

func TestHandlerMapsDesktopCodexBackendPathsWithoutDuplicatingPrefix(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Path; got != "/backend-api/codex/remote/control/client/enroll/start" {
			t.Fatalf("path = %q, want /backend-api/codex/remote/control/client/enroll/start", got)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer oauth-token" {
			t.Fatalf("Authorization = %q, want oauth token", got)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()

	codexUpstream, err := url.Parse(upstream.URL + "/backend-api/codex")
	if err != nil {
		t.Fatal(err)
	}
	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	handler := Server{
		CodexUpstream: codexUpstream,
		Accounts: []accounts.Account{{
			ID:       "codex-account",
			AuthMode: accounts.AuthModeOAuth,
			Token:    "oauth-token",
		}},
		Sessions:     store,
		Scheduler:    selectacct.NewScheduler(nil),
		MaxBodyBytes: 1024,
	}.Handler()
	subrouter := httptest.NewServer(handler)
	defer subrouter.Close()

	response, err := http.Post(subrouter.URL+"/backend-api/codex/remote/control/client/enroll/start", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if _, err := io.Copy(io.Discard, response.Body); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", response.StatusCode)
	}
}

func TestHandlerMapsChatGPTBackendUsagePathsWithoutDuplicatingPrefix(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Path; got != "/backend-api/wham/usage" {
			t.Fatalf("path = %q, want /backend-api/wham/usage", got)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer oauth-token" {
			t.Fatalf("Authorization = %q, want oauth token", got)
		}
		if got := r.Header.Get("ChatGPT-Account-ID"); got != "acct-1" {
			t.Fatalf("ChatGPT-Account-ID = %q, want acct-1", got)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()

	codexUpstream, err := url.Parse(upstream.URL + "/backend-api/codex")
	if err != nil {
		t.Fatal(err)
	}
	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	handler := Server{
		CodexUpstream: codexUpstream,
		Accounts: []accounts.Account{{
			ID:        "codex-account",
			AuthMode:  accounts.AuthModeOAuth,
			Token:     "oauth-token",
			AccountID: "acct-1",
		}},
		Sessions:     store,
		Scheduler:    selectacct.NewScheduler(nil),
		MaxBodyBytes: 1024,
	}.Handler()
	subrouter := httptest.NewServer(handler)
	defer subrouter.Close()

	response, err := http.Get(subrouter.URL + "/backend-api/wham/usage")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if _, err := io.Copy(io.Discard, response.Body); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", response.StatusCode)
	}
}

func TestHandlerDoesNotUseAPIKeyAccountsForDesktopCodexBackendPaths(t *testing.T) {
	upstreamCalled := false
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalled = true
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()

	codexUpstream, err := url.Parse(upstream.URL + "/backend-api/codex")
	if err != nil {
		t.Fatal(err)
	}
	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	handler := Server{
		CodexUpstream: codexUpstream,
		Accounts: []accounts.Account{{
			ID:       "apikey:team",
			AuthMode: accounts.AuthModeAPIKey,
			Token:    "api-token",
		}},
		Sessions:     store,
		Scheduler:    selectacct.NewScheduler(nil),
		MaxBodyBytes: 1024,
	}.Handler()
	subrouter := httptest.NewServer(handler)
	defer subrouter.Close()

	response, err := http.Post(subrouter.URL+"/backend-api/codex/remote/control/client/enroll/start", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if _, err := io.Copy(io.Discard, response.Body); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", response.StatusCode)
	}
	if upstreamCalled {
		t.Fatal("API-key account was used for a Codex backend path")
	}
}

func TestHandlerDoesNotUseAPIKeyAccountsForChatGPTBackendUsagePaths(t *testing.T) {
	upstreamCalled := false
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalled = true
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()

	codexUpstream, err := url.Parse(upstream.URL + "/backend-api/codex")
	if err != nil {
		t.Fatal(err)
	}
	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	handler := Server{
		CodexUpstream: codexUpstream,
		Accounts: []accounts.Account{{
			ID:       "apikey:team",
			AuthMode: accounts.AuthModeAPIKey,
			Token:    "api-token",
		}},
		Sessions:     store,
		Scheduler:    selectacct.NewScheduler(nil),
		MaxBodyBytes: 1024,
	}.Handler()
	subrouter := httptest.NewServer(handler)
	defer subrouter.Close()

	response, err := http.Get(subrouter.URL + "/backend-api/wham/usage")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if _, err := io.Copy(io.Discard, response.Body); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", response.StatusCode)
	}
	if upstreamCalled {
		t.Fatal("API-key account was used for a ChatGPT backend path")
	}
}

func TestHandlerDoesNotProxyUnknownInternalSubrouterPaths(t *testing.T) {
	upstreamCalled := false
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalled = true
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()

	codexUpstream, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	handler := Server{
		CodexUpstream: codexUpstream,
		Accounts: []accounts.Account{{
			ID:       "codex-account",
			AuthMode: accounts.AuthModeOAuth,
			Token:    "oauth-token",
		}},
		Sessions:     store,
		Scheduler:    selectacct.NewScheduler(nil),
		MaxBodyBytes: 1024,
	}.Handler()

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/_subrouter/newer-cli-endpoint", nil))
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", recorder.Code)
	}
	if upstreamCalled {
		t.Fatal("unknown internal path was proxied upstream")
	}
}

func TestHandlerRefreshesExpiredOAuthBeforeProxying(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	store := accounts.CodexStore{Dir: t.TempDir()}
	stale := proxyStoredOAuthAccount("codex@example.com", "old", time.Now().Add(-time.Hour))
	fresh := proxyStoredOAuthAccount("codex@example.com", "new", time.Now().Add(time.Hour))
	if err := store.SaveStored(stale); err != nil {
		t.Fatal(err)
	}
	staleAccount, ok := stale.Account(stale.SourcePath(store))
	if !ok {
		t.Fatal("stale account did not convert")
	}
	freshAccount, ok := fresh.Account(fresh.SourcePath(store))
	if !ok {
		t.Fatal("fresh account did not convert")
	}

	refreshClient := &http.Client{Transport: proxyRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.Method != http.MethodPost {
			t.Fatalf("refresh method = %s", req.Method)
		}
		body, _ := json.Marshal(map[string]string{
			"access_token":  fresh.Auth.Tokens.AccessToken,
			"refresh_token": fresh.Auth.Tokens.RefreshToken,
			"id_token":      fresh.Auth.Tokens.IDToken,
		})
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(bytes.NewReader(body)),
		}, nil
	})}

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != freshAccount.AuthorizationHeader() {
			t.Fatalf("Authorization = %q, want refreshed token", got)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()
	codexUpstream, err := url.Parse(upstream.URL + "/backend-api/codex")
	if err != nil {
		t.Fatal(err)
	}
	sessionStore, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}

	handler := Server{
		CodexUpstream: codexUpstream,
		Accounts:      []accounts.Account{staleAccount},
		AccountRef:    NewAccountRef(store, []accounts.Account{staleAccount}, refreshClient),
		Sessions:      sessionStore,
		Scheduler:     selectacct.NewScheduler(nil),
		MaxBodyBytes:  1024,
	}.Handler()
	subrouter := httptest.NewServer(handler)
	defer subrouter.Close()

	response, err := http.Post(subrouter.URL+"/v1/responses", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if _, err := io.Copy(io.Discard, response.Body); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", response.StatusCode)
	}

	stored, ok, err := store.FindStored("codex@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if !ok || stored.Auth.Tokens.RefreshToken != fresh.Auth.Tokens.RefreshToken {
		t.Fatalf("stored refresh token was not updated")
	}
}

func TestAccountStatusEndpointValidatesRefreshToken(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	store := accounts.CodexStore{Dir: t.TempDir()}
	stale := proxyStoredOAuthAccount("codex@example.com", "old", time.Now().Add(-time.Hour))
	fresh := proxyStoredOAuthAccount("codex@example.com", "new", time.Now().Add(time.Hour))
	if err := store.SaveStored(stale); err != nil {
		t.Fatal(err)
	}
	staleAccount, ok := stale.Account(stale.SourcePath(store))
	if !ok {
		t.Fatal("stale account did not convert")
	}
	refreshClient := &http.Client{Transport: proxyRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		body, _ := json.Marshal(map[string]string{
			"access_token":  fresh.Auth.Tokens.AccessToken,
			"refresh_token": fresh.Auth.Tokens.RefreshToken,
			"id_token":      fresh.Auth.Tokens.IDToken,
		})
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(bytes.NewReader(body)),
		}, nil
	})}
	sessionStore, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	handler := Server{
		Accounts:     []accounts.Account{staleAccount},
		AccountRef:   NewAccountRef(store, []accounts.Account{staleAccount}, refreshClient),
		Sessions:     sessionStore,
		Scheduler:    selectacct.NewScheduler(nil),
		MaxBodyBytes: 1024,
	}.Handler()

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/_subrouter/account-status", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var statuses []AccountStatus
	if err := json.Unmarshal(recorder.Body.Bytes(), &statuses); err != nil {
		t.Fatal(err)
	}
	if len(statuses) != 1 || !statuses[0].AuthValid || !statuses[0].Refreshed {
		t.Fatalf("unexpected statuses: %+v", statuses)
	}
}

func TestUsageStatusEndpointFetchesUsageWithoutForcingFreshRefresh(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	store := accounts.CodexStore{Dir: t.TempDir()}
	fresh := proxyStoredOAuthAccount("codex@example.com", "fresh", time.Now().Add(time.Hour))
	if err := store.SaveStored(fresh); err != nil {
		t.Fatal(err)
	}
	if err := accounts.WriteActiveCodexAuth(fresh.Auth); err != nil {
		t.Fatal(err)
	}
	account, ok := fresh.Account(fresh.SourcePath(store))
	if !ok {
		t.Fatal("fresh account did not convert")
	}
	client := &http.Client{Transport: proxyRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Host == "auth.openai.com" {
			t.Fatalf("usage-status should not force OAuth refresh for a fresh access token")
		}
		if req.URL.Path != "/backend-api/wham/usage" {
			t.Fatalf("unexpected path: %s", req.URL.Path)
		}
		body, _ := json.Marshal(map[string]any{
			"plan_type": "pro",
			"rate_limit": map[string]any{
				"primary_window": map[string]any{
					"used_percent":         float64(20),
					"limit_window_seconds": int64((5 * time.Hour) / time.Second),
					"reset_after_seconds":  int64(time.Hour / time.Second),
				},
			},
			"complimentary_session_reset": map[string]any{
				"available": true,
			},
		})
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(bytes.NewReader(body)),
		}, nil
	})}
	sessionStore, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	handler := Server{
		Accounts:     []accounts.Account{account},
		AccountRef:   NewAccountRef(store, []accounts.Account{account}, client),
		Sessions:     sessionStore,
		Scheduler:    selectacct.NewScheduler(nil),
		MaxBodyBytes: 1024,
	}.Handler()

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/_subrouter/usage-status", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var statuses []AccountUsageStatus
	if err := json.Unmarshal(recorder.Body.Bytes(), &statuses); err != nil {
		t.Fatal(err)
	}
	if len(statuses) != 1 || !statuses[0].AuthValid || statuses[0].Refreshed || statuses[0].PlanType != "pro" || !statuses[0].Active {
		t.Fatalf("unexpected statuses: %+v", statuses)
	}
	if statuses[0].ComplimentaryReset == nil || !statuses[0].ComplimentaryReset.Available {
		t.Fatalf("missing complimentary reset status: %+v", statuses[0].ComplimentaryReset)
	}
}

func TestUsageStatusEndpointIncludesClaudeRequestTimeExhaustion(t *testing.T) {
	ref := selectacct.NewSchedulerRef(selectacct.NewScheduler(nil))
	ref.MarkExhaustedUntil(accounts.ProviderClaude, "claude@example.com", "", time.Now().Add(2*time.Hour))
	handler := Server{
		Accounts: []accounts.Account{{
			ID:       "claude@example.com",
			Provider: accounts.ProviderClaude,
			AuthMode: accounts.AuthModeOAuth,
			Email:    "claude@example.com",
		}},
		SchedulerRef: ref,
		MaxBodyBytes: 1024,
	}.Handler()

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/_subrouter/usage-status", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var statuses []AccountUsageStatus
	if err := json.Unmarshal(recorder.Body.Bytes(), &statuses); err != nil {
		t.Fatal(err)
	}
	if len(statuses) != 1 {
		t.Fatalf("statuses = %+v, want one", statuses)
	}
	for _, window := range statuses[0].Windows {
		if window.Name == agentclaude.FableWindowName {
			if window.UsedPercent != 100 || window.ResetAfterSeconds <= 0 {
				t.Fatalf("Fable window = %+v, want saturated with reset", window)
			}
			if window.Feature != agentclaude.FableModel {
				t.Fatalf("Fable Feature = %q, want %q", window.Feature, agentclaude.FableModel)
			}
			return
		}
	}
	t.Fatalf("missing Fable window: %+v", statuses[0].Windows)
}

func TestReloadAccountsHotLoadsNewAccountWithoutRestart(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	accountStore := accounts.CodexStore{Dir: t.TempDir()}
	initial := proxyStoredOAuthAccount("old@example.com", "old", time.Now().Add(time.Hour))
	added := proxyStoredOAuthAccount("new@example.com", "new", time.Now().Add(time.Hour))
	if err := accountStore.SaveStored(initial); err != nil {
		t.Fatal(err)
	}
	initialAccount, ok := initial.Account(initial.SourcePath(accountStore))
	if !ok {
		t.Fatal("initial account did not convert")
	}
	upstreamAuthorizations := make(chan string, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamAuthorizations <- r.Header.Get("Authorization")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()
	codexUpstream, err := url.Parse(upstream.URL + "/backend-api/codex")
	if err != nil {
		t.Fatal(err)
	}
	sessionStore, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	accountRef := NewAccountRef(accountStore, []accounts.Account{initialAccount}, nil)
	schedulerRef := selectacct.NewSchedulerRef(selectacct.NewScheduler([]selectacct.Score{
		{AccountID: "old@example.com", Headroom: 0.90, ShortHeadroom: 0.90},
	}))
	handler := Server{
		CodexUpstream: codexUpstream,
		AccountRef:    accountRef,
		Sessions:      sessionStore,
		SchedulerRef:  schedulerRef,
		MaxBodyBytes:  1024,
	}.Handler()
	subrouter := httptest.NewServer(handler)
	defer subrouter.Close()

	if err := accountStore.SaveStored(added); err != nil {
		t.Fatal(err)
	}
	reloadReq, err := http.NewRequest(http.MethodPost, subrouter.URL+"/_subrouter/reload-accounts", nil)
	if err != nil {
		t.Fatal(err)
	}
	reloadResp, err := http.DefaultClient.Do(reloadReq)
	if err != nil {
		t.Fatal(err)
	}
	if reloadResp.StatusCode != http.StatusOK {
		defer reloadResp.Body.Close()
		body, _ := io.ReadAll(reloadResp.Body)
		t.Fatalf("reload status = %d, body = %s", reloadResp.StatusCode, string(body))
	}
	var payload struct {
		OK       bool `json:"ok"`
		Accounts int  `json:"accounts"`
	}
	if err := json.NewDecoder(reloadResp.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if err := reloadResp.Body.Close(); err != nil {
		t.Fatal(err)
	}
	if !payload.OK || payload.Accounts != 2 {
		t.Fatalf("reload payload = %+v, want 2 accounts", payload)
	}

	req, err := http.NewRequest(http.MethodPost, subrouter.URL+"/v1/responses", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Subrouter-Session", "new-session")
	req.Header.Set("X-Subrouter-Account-ID", "new@example.com")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", resp.StatusCode)
	}
	if got := <-upstreamAuthorizations; got != "Bearer "+added.Auth.Tokens.AccessToken {
		t.Fatalf("Authorization = %q, want new account token", got)
	}
}

func TestHandlerMapsV1WebSocketRequestsToCodexBackendPaths(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(_ *http.Request) bool { return true }}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !websocket.IsWebSocketUpgrade(r) {
			t.Fatalf("expected websocket upgrade")
		}
		if got := r.URL.Path; got != "/backend-api/codex/responses" {
			t.Fatalf("path = %q, want /backend-api/codex/responses", got)
		}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Fatalf("upgrade: %v", err)
		}
		defer conn.Close()
		if err := conn.WriteMessage(websocket.TextMessage, []byte("ok")); err != nil {
			t.Fatalf("write message: %v", err)
		}
	}))
	defer upstream.Close()

	codexUpstream, err := url.Parse(upstream.URL + "/backend-api/codex")
	if err != nil {
		t.Fatal(err)
	}
	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	handler := Server{
		CodexUpstream: codexUpstream,
		Accounts: []accounts.Account{{
			ID:       "codex-account",
			AuthMode: accounts.AuthModeOAuth,
			Token:    "oauth-token",
		}},
		Sessions:     store,
		Scheduler:    selectacct.NewScheduler(nil),
		MaxBodyBytes: 1024,
	}.Handler()
	subrouter := httptest.NewServer(handler)
	defer subrouter.Close()

	wsURL := "ws" + strings.TrimPrefix(subrouter.URL, "http") + "/v1/responses"
	conn, response, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer response.Body.Close()
	defer conn.Close()
	_, body, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read message: %v", err)
	}
	if string(body) != "ok" {
		t.Fatalf("message = %q, want ok", string(body))
	}
}

func TestHandlerMapsRealtimeWebSocketRequestsToCodexBackendPaths(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(_ *http.Request) bool { return true }}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !websocket.IsWebSocketUpgrade(r) {
			t.Fatalf("expected websocket upgrade")
		}
		if got := r.URL.Path; got != "/backend-api/codex/realtime" {
			t.Fatalf("path = %q, want /backend-api/codex/realtime", got)
		}
		if got := r.URL.Query().Get("intent"); got != "quicksilver" {
			t.Fatalf("intent = %q, want quicksilver", got)
		}
		if got := r.URL.Query().Get("model"); got != "snapshot" {
			t.Fatalf("model = %q, want snapshot", got)
		}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Fatalf("upgrade: %v", err)
		}
		defer conn.Close()
		if err := conn.WriteMessage(websocket.TextMessage, []byte("ok")); err != nil {
			t.Fatalf("write message: %v", err)
		}
	}))
	defer upstream.Close()

	codexUpstream, err := url.Parse(upstream.URL + "/backend-api/codex")
	if err != nil {
		t.Fatal(err)
	}
	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	handler := Server{
		CodexUpstream: codexUpstream,
		Accounts: []accounts.Account{{
			ID:       "codex-account",
			AuthMode: accounts.AuthModeOAuth,
			Token:    "oauth-token",
		}},
		Sessions:     store,
		Scheduler:    selectacct.NewScheduler(nil),
		MaxBodyBytes: 1024,
	}.Handler()
	subrouter := httptest.NewServer(handler)
	defer subrouter.Close()

	wsURL := "ws" + strings.TrimPrefix(subrouter.URL, "http") + "/v1/realtime?intent=quicksilver&model=snapshot"
	conn, response, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer response.Body.Close()
	defer conn.Close()
	_, body, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read message: %v", err)
	}
	if string(body) != "ok" {
		t.Fatalf("message = %q, want ok", string(body))
	}
}

func TestHandlerMapsDesktopCodexBackendWebSocketPathsWithoutDuplicatingPrefix(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(_ *http.Request) bool { return true }}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !websocket.IsWebSocketUpgrade(r) {
			t.Fatalf("expected websocket upgrade")
		}
		if got := r.URL.Path; got != "/backend-api/codex/realtime" {
			t.Fatalf("path = %q, want /backend-api/codex/realtime", got)
		}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Fatalf("upgrade: %v", err)
		}
		defer conn.Close()
		if err := conn.WriteMessage(websocket.TextMessage, []byte("ok")); err != nil {
			t.Fatalf("write message: %v", err)
		}
	}))
	defer upstream.Close()

	codexUpstream, err := url.Parse(upstream.URL + "/backend-api/codex")
	if err != nil {
		t.Fatal(err)
	}
	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	handler := Server{
		CodexUpstream: codexUpstream,
		Accounts: []accounts.Account{{
			ID:       "codex-account",
			AuthMode: accounts.AuthModeOAuth,
			Token:    "oauth-token",
		}},
		Sessions:     store,
		Scheduler:    selectacct.NewScheduler(nil),
		MaxBodyBytes: 1024,
	}.Handler()
	subrouter := httptest.NewServer(handler)
	defer subrouter.Close()

	wsURL := "ws" + strings.TrimPrefix(subrouter.URL, "http") + "/backend-api/codex/realtime"
	conn, response, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer response.Body.Close()
	defer conn.Close()
	_, body, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read message: %v", err)
	}
	if string(body) != "ok" {
		t.Fatalf("message = %q, want ok", string(body))
	}
}

type proxyRoundTripFunc func(*http.Request) (*http.Response, error)

func (f proxyRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func writeClaudeCredential(t *testing.T, dir string, credential agentclaude.CredentialInfo) {
	t.Helper()
	body, err := json.Marshal(map[string]agentclaude.CredentialInfo{"claudeAiOauth": credential})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".credentials.json"), body, 0o600); err != nil {
		t.Fatal(err)
	}
}

func proxyStoredOAuthAccount(email, tokenPrefix string, exp time.Time) accounts.StoredCodexAccount {
	return accounts.StoredCodexAccount{
		Email:   email,
		AddedAt: time.Now().UTC().Format(time.RFC3339),
		Auth: accounts.CodexAuthFile{AuthMode: "chatgpt", Tokens: &accounts.CodexTokens{
			AccessToken:  proxyTestCodexJWT(email, tokenPrefix+"-access", exp),
			RefreshToken: tokenPrefix + "-refresh",
			IDToken:      proxyTestCodexJWT(email, tokenPrefix+"-id", exp),
		}},
	}
}

func proxyTestCodexJWT(email, jwtID string, exp time.Time) string {
	header, _ := json.Marshal(map[string]string{"alg": "none", "typ": "JWT"})
	payload, _ := json.Marshal(map[string]any{
		"exp": exp.Unix(),
		"iat": time.Now().Add(-time.Minute).Unix(),
		"jti": jwtID,
		"https://api.openai.com/profile": map[string]any{
			"email": email,
		},
	})
	return base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(payload) + ".sig"
}

func TestHandlerMapsBareRequestsToOpenAIV1Paths(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Path; got != "/v1/responses" {
			t.Fatalf("path = %q, want /v1/responses", got)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer api-token" {
			t.Fatalf("Authorization = %q, want API token", got)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()

	apiUpstream, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	handler := Server{
		APIUpstream: apiUpstream,
		Accounts: []accounts.Account{{
			ID:       "api-account",
			AuthMode: accounts.AuthModeAPIKey,
			Token:    "api-token",
		}},
		Sessions:     store,
		Scheduler:    selectacct.NewScheduler(nil),
		MaxBodyBytes: 1024,
	}.Handler()
	subrouter := httptest.NewServer(handler)
	defer subrouter.Close()

	response, err := http.Post(subrouter.URL+"/responses", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if _, err := io.Copy(io.Discard, response.Body); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", response.StatusCode)
	}
}

func TestHandlerUsesExplicitSubrouterAccountID(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Path; got != "/v1/responses" {
			t.Fatalf("path = %q, want /v1/responses", got)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer api-token" {
			t.Fatalf("Authorization = %q, want API token", got)
		}
		if got := r.Header.Get("X-Subrouter-Account-ID"); got != "" {
			t.Fatalf("X-Subrouter-Account-ID leaked upstream: %q", got)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()

	apiUpstream, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	handler := Server{
		APIUpstream: apiUpstream,
		Accounts: []accounts.Account{
			{ID: "oauth@example.com", AuthMode: accounts.AuthModeOAuth, Token: "oauth-token"},
			{ID: "apikey:team-codex-1", AuthMode: accounts.AuthModeAPIKey, Token: "api-token"},
		},
		Sessions: store,
		Scheduler: selectacct.NewScheduler([]selectacct.Score{
			{AccountID: "oauth@example.com", Headroom: 1, ShortHeadroom: 1},
			{AccountID: "apikey:team-codex-1", Headroom: 0.01, ShortHeadroom: 0.01},
		}),
		MaxBodyBytes: 1024,
	}.Handler()
	subrouter := httptest.NewServer(handler)
	defer subrouter.Close()

	req, err := http.NewRequest(http.MethodPost, subrouter.URL+"/responses", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Subrouter-Session", "session-1")
	req.Header.Set("X-Subrouter-Account-ID", "team-codex-1")
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if _, err := io.Copy(io.Discard, response.Body); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", response.StatusCode)
	}

	assignment, ok := store.Get("codex", "session-1")
	if !ok {
		t.Fatal("missing assignment")
	}
	if assignment.AccountID != "apikey:team-codex-1" {
		t.Fatalf("AccountID = %q, want apikey:team-codex-1", assignment.AccountID)
	}
}

func TestHandlerUsesClaudeAPIKeyHeaders(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Path; got != "/v1/messages" {
			t.Fatalf("path = %q, want /v1/messages", got)
		}
		if got := r.Header.Get("X-Api-Key"); got != "anthropic-key" {
			t.Fatalf("X-Api-Key = %q, want API key", got)
		}
		if got := r.Header.Get("Authorization"); got != "" {
			t.Fatalf("Authorization leaked upstream: %q", got)
		}
		if strings.Contains(r.Header.Get("Anthropic-Beta"), "oauth-2025-04-20") {
			t.Fatalf("OAuth beta sent for API-key Claude account: %q", r.Header.Get("Anthropic-Beta"))
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()

	claudeUpstream, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	handler := Server{
		ClaudeUpstream: claudeUpstream,
		Accounts: []accounts.Account{{
			ID:       "claude:team-key",
			Provider: accounts.ProviderClaude,
			AuthMode: accounts.AuthModeAPIKey,
			Token:    "anthropic-key",
		}},
		Sessions:     store,
		Scheduler:    selectacct.NewScheduler(nil),
		MaxBodyBytes: 1024,
	}.Handler()
	subrouter := httptest.NewServer(handler)
	defer subrouter.Close()

	req, err := http.NewRequest(http.MethodPost, subrouter.URL+"/v1/messages", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer subrouter-client-token")
	req.Header.Set("X-Subrouter-Agent", "claude")
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if _, err := io.Copy(io.Discard, response.Body); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", response.StatusCode)
	}
}

func TestHandlerPreservesRequestBodyBytesAfterJSONSessionExtraction(t *testing.T) {
	body := []byte("{\n  \"input\": \"keep bytes: \\u2603\",\n  \"metadata\": {\"session_id\": \"json-session\"},\n  \"array\": [1, true, null]\n}")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, body) {
			t.Fatalf("body changed:\n got: %q\nwant: %q", got, body)
		}
		if r.ContentLength != int64(len(body)) {
			t.Fatalf("ContentLength = %d, want %d", r.ContentLength, len(body))
		}
		if got := r.Header.Get("X-Client-Trace-ID"); got != "trace-preserved" {
			t.Fatalf("X-Client-Trace-ID = %q, want trace-preserved", got)
		}
		w.WriteHeader(http.StatusNoContent)
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
	handler := Server{
		Upstream: upstreamURL,
		Accounts: []accounts.Account{{
			ID:       "a@example.com",
			AuthMode: accounts.AuthModeOAuth,
			Token:    "a-token",
		}},
		Sessions:     store,
		Scheduler:    selectacct.NewScheduler(nil),
		MaxBodyBytes: 1024,
	}.Handler()
	subrouter := httptest.NewServer(handler)
	defer subrouter.Close()

	req, err := http.NewRequest(http.MethodPost, subrouter.URL+"/v1/responses", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Client-Trace-ID", "trace-preserved")
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if _, err := io.Copy(io.Discard, response.Body); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", response.StatusCode)
	}

	assignment, ok := store.Get("codex", "json-session")
	if !ok {
		t.Fatal("missing json-session assignment")
	}
	if assignment.AccountID != "a@example.com" {
		t.Fatalf("AccountID = %q, want a@example.com", assignment.AccountID)
	}
}

func TestHandlerPreservesResponseBodyBytes(t *testing.T) {
	body := []byte("event: response.output_text.delta\ndata: {\"delta\":\"hello\"}\n\nevent: response.completed\ndata: {}\n\n")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusAccepted)
		if _, err := w.Write(body); err != nil {
			t.Fatal(err)
		}
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
	handler := Server{
		Upstream: upstreamURL,
		Accounts: []accounts.Account{{
			ID:       "a@example.com",
			AuthMode: accounts.AuthModeOAuth,
			Token:    "a-token",
		}},
		Sessions:     store,
		Scheduler:    selectacct.NewScheduler(nil),
		MaxBodyBytes: 1024,
	}.Handler()
	subrouter := httptest.NewServer(handler)
	defer subrouter.Close()

	req, err := http.NewRequest(http.MethodPost, subrouter.URL+"/v1/responses", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Subrouter-Session", "response-session")
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	got, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", response.StatusCode)
	}
	if !bytes.Equal(got, body) {
		t.Fatalf("body changed:\n got: %q\nwant: %q", got, body)
	}
}

func TestNewOutboundTransportUsesIPv4AndPooledHTTP1(t *testing.T) {
	transport := NewOutboundTransport()
	if transport.DisableKeepAlives {
		t.Fatal("DisableKeepAlives = true, want pooled connections")
	}
	if transport.DialContext == nil {
		t.Fatal("DialContext = nil, want IPv4-only dialer")
	}
	if transport.ForceAttemptHTTP2 {
		t.Fatal("ForceAttemptHTTP2 = true, want false")
	}
	if transport.TLSNextProto == nil {
		t.Fatal("TLSNextProto = nil, want empty map to disable HTTP/2")
	}
	if len(transport.TLSNextProto) != 0 {
		t.Fatalf("TLSNextProto = %#v, want empty map", transport.TLSNextProto)
	}
	if transport.TLSClientConfig == nil {
		t.Fatal("TLSClientConfig = nil, want ALPN constrained to HTTP/1.1")
	}
	if got := strings.Join(transport.TLSClientConfig.NextProtos, ","); got != "http/1.1" {
		t.Fatalf("NextProtos = %q, want http/1.1", got)
	}
}

func TestNewOutboundTransportDialsIPv4(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		conn, err := listener.Accept()
		if err == nil {
			accepted <- conn
		}
		close(accepted)
	}()

	transport := NewOutboundTransport()
	conn, err := transport.DialContext(context.Background(), "tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if got := conn.RemoteAddr().(*net.TCPAddr).IP.String(); got != "127.0.0.1" {
		t.Fatalf("remote IP = %q, want 127.0.0.1", got)
	}
	if acceptedConn := <-accepted; acceptedConn != nil {
		_ = acceptedConn.Close()
	}
}

func TestHandlerRetriesReplayableResponsesPostOnTransientTransportError(t *testing.T) {
	for _, path := range []string{"/v1/responses", "/v1/responses/compact"} {
		t.Run(path, func(t *testing.T) {
			upstreamURL, err := url.Parse("https://chatgpt.com/backend-api/codex")
			if err != nil {
				t.Fatal(err)
			}
			store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
			if err != nil {
				t.Fatal(err)
			}
			attempts := 0
			var bodies []string
			transport := proxyRoundTripFunc(func(req *http.Request) (*http.Response, error) {
				attempts++
				body, err := io.ReadAll(req.Body)
				if err != nil {
					t.Fatal(err)
				}
				bodies = append(bodies, string(body))
				if attempts == 1 {
					return nil, errors.New("write tcp: broken pipe")
				}
				return &http.Response{
					StatusCode: http.StatusAccepted,
					Header:     make(http.Header),
					Body:       io.NopCloser(strings.NewReader("ok")),
					Request:    req,
				}, nil
			})
			handler := Server{
				CodexUpstream: upstreamURL,
				Accounts: []accounts.Account{{
					ID:       "codex-account",
					AuthMode: accounts.AuthModeOAuth,
					Token:    "oauth-token",
				}},
				Sessions:     store,
				Scheduler:    selectacct.NewScheduler(nil),
				Transport:    transport,
				MaxBodyBytes: 1024,
			}.Handler()
			subrouter := httptest.NewServer(handler)
			defer subrouter.Close()

			body := `{"model":"gpt-5.5","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}]}`
			response, err := http.Post(subrouter.URL+path, "application/json", strings.NewReader(body))
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			responseBody, err := io.ReadAll(response.Body)
			if err != nil {
				t.Fatal(err)
			}
			if response.StatusCode != http.StatusAccepted {
				t.Fatalf("status = %d, want 202", response.StatusCode)
			}
			if string(responseBody) != "ok" {
				t.Fatalf("body = %q, want ok", string(responseBody))
			}
			if attempts != 2 {
				t.Fatalf("attempts = %d, want 2", attempts)
			}
			if strings.Join(bodies, "\x00") != body+"\x00"+body {
				t.Fatalf("bodies = %#v, want replayed body", bodies)
			}
		})
	}
}

func TestHandlerRetriesReplayableResponsesPostOnUpstreamRequestTimeout(t *testing.T) {
	for _, path := range []string{"/v1/responses", "/v1/responses/compact"} {
		t.Run(path, func(t *testing.T) {
			upstreamURL, err := url.Parse("https://chatgpt.com/backend-api/codex")
			if err != nil {
				t.Fatal(err)
			}
			store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
			if err != nil {
				t.Fatal(err)
			}
			attempts := 0
			var bodies []string
			transport := proxyRoundTripFunc(func(req *http.Request) (*http.Response, error) {
				attempts++
				body, err := io.ReadAll(req.Body)
				if err != nil {
					t.Fatal(err)
				}
				bodies = append(bodies, string(body))
				if attempts == 1 {
					return &http.Response{
						StatusCode: http.StatusRequestTimeout,
						Header: http.Header{
							"Cf-Ray":       []string{"a01b6497ca8ddad6-DFW"},
							"X-Request-ID": []string{"034a8436-31b9-49b4-9308-fcd5e751cb91"},
						},
						Body:    io.NopCloser(strings.NewReader(`{"detail":"Request body read timed out"}`)),
						Request: req,
					}, nil
				}
				return &http.Response{
					StatusCode: http.StatusAccepted,
					Header:     make(http.Header),
					Body:       io.NopCloser(strings.NewReader("ok")),
					Request:    req,
				}, nil
			})
			handler := Server{
				CodexUpstream: upstreamURL,
				Accounts: []accounts.Account{{
					ID:       "codex-account",
					AuthMode: accounts.AuthModeOAuth,
					Token:    "oauth-token",
				}},
				Sessions:     store,
				Scheduler:    selectacct.NewScheduler(nil),
				Transport:    transport,
				MaxBodyBytes: 1024,
			}.Handler()
			subrouter := httptest.NewServer(handler)
			defer subrouter.Close()

			body := `{"model":"gpt-5.5","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}]}`
			response, err := http.Post(subrouter.URL+path, "application/json", strings.NewReader(body))
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			responseBody, err := io.ReadAll(response.Body)
			if err != nil {
				t.Fatal(err)
			}
			if response.StatusCode != http.StatusAccepted {
				t.Fatalf("status = %d, want 202", response.StatusCode)
			}
			if string(responseBody) != "ok" {
				t.Fatalf("body = %q, want ok", string(responseBody))
			}
			if attempts != 2 {
				t.Fatalf("attempts = %d, want 2", attempts)
			}
			if strings.Join(bodies, "\x00") != body+"\x00"+body {
				t.Fatalf("bodies = %#v, want replayed body", bodies)
			}
		})
	}
}

func TestReplayablePostMaxBodyBytesCoversLargeDesktopCompacts(t *testing.T) {
	const desktopCompactBytes = 45_994_179
	if replayablePostMaxBodyBytes < desktopCompactBytes {
		t.Fatalf("replayablePostMaxBodyBytes = %d, want at least %d", replayablePostMaxBodyBytes, desktopCompactBytes)
	}
}

func TestHandlerLogsUpstreamResponseStreamReadErrors(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hijacker, ok := w.(http.Hijacker)
		if !ok {
			t.Fatal("response writer cannot hijack")
		}
		conn, rw, err := hijacker.Hijack()
		if err != nil {
			t.Fatalf("hijack: %v", err)
		}
		defer conn.Close()
		if _, err := rw.WriteString("HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n5\r\nhello\r\n"); err != nil {
			t.Fatalf("write partial chunked response: %v", err)
		}
		if err := rw.Flush(); err != nil {
			t.Fatalf("flush partial chunked response: %v", err)
		}
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
	var logs bytes.Buffer
	handler := Server{
		Upstream: upstreamURL,
		Accounts: []accounts.Account{{
			ID:       "a@example.com",
			AuthMode: accounts.AuthModeOAuth,
			Token:    "a-token",
		}},
		Sessions:     store,
		Scheduler:    selectacct.NewScheduler(nil),
		Logger:       slog.New(slog.NewTextHandler(&logs, nil)),
		MaxBodyBytes: 1024,
	}.Handler()
	subrouter := httptest.NewServer(handler)
	defer subrouter.Close()

	req, err := http.NewRequest(http.MethodPost, subrouter.URL+"/v1/responses/compact", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Subrouter-Session", "codex-session:7")
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, copyErr := io.Copy(io.Discard, response.Body)
	if err := response.Body.Close(); err != nil {
		t.Fatal(err)
	}
	if copyErr == nil {
		t.Fatal("copy error = nil, want truncated upstream response error")
	}

	gotLogs := logs.String()
	if !strings.Contains(gotLogs, "proxy response stream read failed") {
		t.Fatalf("logs missing stream read failure:\n%s", gotLogs)
	}
	if !strings.Contains(gotLogs, "path=/v1/responses/compact") {
		t.Fatalf("logs missing compact path:\n%s", gotLogs)
	}
	if strings.Contains(gotLogs, "a-token") {
		t.Fatalf("logs leaked token:\n%s", gotLogs)
	}
}

func TestHandlerPreservesWebSocketMessageBytes(t *testing.T) {
	payload := []byte{0x00, 0x01, 0x02, 0xfe, 0xff, 'c', 'm', 'u', 'x'}
	upgrader := websocket.Upgrader{CheckOrigin: func(_ *http.Request) bool { return true }}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Fatalf("upgrade: %v", err)
		}
		defer conn.Close()
		messageType, got, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("read message: %v", err)
		}
		if messageType != websocket.BinaryMessage {
			t.Fatalf("message type = %d, want binary", messageType)
		}
		if !bytes.Equal(got, payload) {
			t.Fatalf("websocket payload changed:\n got: %x\nwant: %x", got, payload)
		}
		if got := r.Header.Get("X-Codex-Window-ID"); got != "ws-window" {
			t.Fatalf("X-Codex-Window-ID = %q, want ws-window", got)
		}
		if err := conn.WriteMessage(websocket.BinaryMessage, got); err != nil {
			t.Fatalf("write message: %v", err)
		}
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
	handler := Server{
		Upstream: upstreamURL,
		Accounts: []accounts.Account{{
			ID:       "a@example.com",
			AuthMode: accounts.AuthModeOAuth,
			Token:    "a-token",
		}},
		Sessions:     store,
		Scheduler:    selectacct.NewScheduler(nil),
		MaxBodyBytes: 1024,
	}.Handler()
	subrouter := httptest.NewServer(handler)
	defer subrouter.Close()

	wsURL := "ws" + strings.TrimPrefix(subrouter.URL, "http") + "/v1/responses"
	header := http.Header{"X-Codex-Window-ID": []string{"ws-window"}}
	conn, response, err := websocket.DefaultDialer.Dial(wsURL, header)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer response.Body.Close()
	defer conn.Close()
	if err := conn.WriteMessage(websocket.BinaryMessage, payload); err != nil {
		t.Fatalf("write message: %v", err)
	}
	messageType, got, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read message: %v", err)
	}
	if messageType != websocket.BinaryMessage {
		t.Fatalf("message type = %d, want binary", messageType)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("websocket echo changed:\n got: %x\nwant: %x", got, payload)
	}
}

func TestHandlerRecordsHTTPTranscriptBodies(t *testing.T) {
	requestBody := []byte(`{"session_id":"codex-session:0","input":"hello"}`)
	responseBody := []byte("event: done\ndata: {}\n\n")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.Copy(io.Discard, r.Body); err != nil {
			t.Fatal(err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		if _, err := w.Write(responseBody); err != nil {
			t.Fatal(err)
		}
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
	transcripts := transcript.NewRecorder(filepath.Join(t.TempDir(), "transcripts"))
	handler := Server{
		Upstream: upstreamURL,
		Accounts: []accounts.Account{{
			ID:       "a@example.com",
			AuthMode: accounts.AuthModeOAuth,
			Token:    "a-token",
		}},
		Sessions:     store,
		Scheduler:    selectacct.NewScheduler(nil),
		MaxBodyBytes: 1024,
		Transcripts:  transcripts,
	}.Handler()
	subrouter := httptest.NewServer(handler)
	defer subrouter.Close()

	req, err := http.NewRequest(http.MethodPost, subrouter.URL+"/v1/responses", bytes.NewReader(requestBody))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer client-token")
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(io.Discard, response.Body); err != nil {
		t.Fatal(err)
	}
	if err := response.Body.Close(); err != nil {
		t.Fatal(err)
	}

	events := readTranscriptEventsEventually(t, transcripts.PathForSession("codex", "codex-session:0"), 3)
	assertTranscriptPayload(t, events, "http_body", "client_to_upstream", requestBody)
	assertTranscriptPayload(t, events, "http_body", "upstream_to_client", responseBody)
	if got := events[0]["payload"].(map[string]any)["agent_type"]; got != "codex" {
		t.Fatalf("agent_type = %v, want codex", got)
	}
	if got := events[0]["payload"].(map[string]any)["agent_session_id"]; got != "codex-session" {
		t.Fatalf("agent_session_id = %v, want codex-session", got)
	}
	if got := events[0]["payload"].(map[string]any)["codex_session_id"]; got != "codex-session" {
		t.Fatalf("codex_session_id = %v, want codex-session", got)
	}
	headers := events[0]["payload"].(map[string]any)["headers"].(map[string]any)
	if got := headers["Authorization"].([]any)[0]; got != "<redacted>" {
		t.Fatalf("Authorization header = %v, want redacted", got)
	}
}

func TestHandlerRecordsWebSocketTranscriptMessages(t *testing.T) {
	clientPayload := []byte(`{"encrypted_content":"client-ciphertext","prompt_cache_key":"cache-key"}`)
	upstreamPayload := []byte(`{"encrypted_content":"upstream-ciphertext"}`)
	upgrader := websocket.Upgrader{CheckOrigin: func(_ *http.Request) bool { return true }}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Fatalf("upgrade: %v", err)
		}
		defer conn.Close()
		messageType, body, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("read message: %v", err)
		}
		if messageType != websocket.TextMessage {
			t.Fatalf("message type = %d, want text", messageType)
		}
		if !bytes.Equal(body, clientPayload) {
			t.Fatalf("client payload = %q, want %q", body, clientPayload)
		}
		if err := conn.WriteMessage(websocket.TextMessage, upstreamPayload); err != nil {
			t.Fatalf("write message: %v", err)
		}
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
	transcripts := transcript.NewRecorder(filepath.Join(t.TempDir(), "transcripts"))
	handler := Server{
		Upstream: upstreamURL,
		Accounts: []accounts.Account{{
			ID:       "a@example.com",
			AuthMode: accounts.AuthModeOAuth,
			Token:    "a-token",
		}},
		Sessions:     store,
		Scheduler:    selectacct.NewScheduler(nil),
		MaxBodyBytes: 1024,
		Transcripts:  transcripts,
	}.Handler()
	subrouter := httptest.NewServer(handler)
	defer subrouter.Close()

	wsURL := "ws" + strings.TrimPrefix(subrouter.URL, "http") + "/v1/responses"
	conn, response, err := websocket.DefaultDialer.Dial(wsURL, http.Header{"X-Codex-Window-ID": []string{"codex-ws:0"}})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer response.Body.Close()
	defer conn.Close()
	if err := conn.WriteMessage(websocket.TextMessage, clientPayload); err != nil {
		t.Fatalf("write message: %v", err)
	}
	if _, _, err := conn.ReadMessage(); err != nil {
		t.Fatalf("read message: %v", err)
	}

	events := readTranscriptEventsEventually(t, transcripts.PathForSession("codex", "codex-ws:0"), 3)
	assertTranscriptPayload(t, events, "websocket_message", "client_to_upstream", clientPayload)
	assertTranscriptPayload(t, events, "websocket_message", "upstream_to_client", upstreamPayload)
}

func TestHandlerStoresUserEmailAndStripsSubrouterHeaders(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("X-Subrouter-Session"); got != "" {
			t.Fatalf("X-Subrouter-Session = %q, want empty", got)
		}
		if got := r.Header.Get("X-Subrouter-Agent"); got != "" {
			t.Fatalf("X-Subrouter-Agent = %q, want empty", got)
		}
		if got := r.Header.Get("X-Subrouter-User-Email"); got != "" {
			t.Fatalf("X-Subrouter-User-Email = %q, want empty", got)
		}
		if got := r.Header.Get("X-Subrouter-User"); got != "" {
			t.Fatalf("X-Subrouter-User = %q, want empty", got)
		}
		if got := r.Header.Get("X-User-Email"); got != "" {
			t.Fatalf("X-User-Email = %q, want empty", got)
		}
		if got := r.Header.Get("X-Trace-ID"); got != "trace-1" {
			t.Fatalf("X-Trace-ID = %q, want trace-1", got)
		}
		w.WriteHeader(http.StatusNoContent)
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
	handler := Server{
		Upstream: upstreamURL,
		Accounts: []accounts.Account{{
			ID:       "a@example.com",
			AuthMode: accounts.AuthModeOAuth,
			Token:    "a-token",
		}},
		Sessions:     store,
		Scheduler:    selectacct.NewScheduler(nil),
		MaxBodyBytes: 1024,
	}.Handler()
	subrouter := httptest.NewServer(handler)
	defer subrouter.Close()

	req, err := http.NewRequest(http.MethodPost, subrouter.URL+"/v1/responses", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Subrouter-Session", "session-1")
	req.Header.Set("X-Subrouter-Agent", "claude")
	req.Header.Set("X-Subrouter-User-Email", "Alice <Alice@Example.COM>")
	req.Header.Set("X-Subrouter-User", "bob@example.com")
	req.Header.Set("X-User-Email", "carol@example.com")
	req.Header.Set("X-Trace-ID", "trace-1")
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if _, err := io.Copy(io.Discard, response.Body); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", response.StatusCode)
	}

	assignments := store.All()
	if len(assignments) != 1 {
		t.Fatalf("len(assignments) = %d, want 1", len(assignments))
	}
	assignment := assignments[0]
	if assignment.AgentType != "claude" {
		t.Fatalf("AgentType = %q, want claude", assignment.AgentType)
	}
	if assignment.SessionID != "session-1" {
		t.Fatalf("SessionID = %q, want session-1", assignment.SessionID)
	}
	if assignment.AccountID != "a@example.com" {
		t.Fatalf("AccountID = %q, want a@example.com", assignment.AccountID)
	}
	if assignment.UserEmail != "alice@example.com" {
		t.Fatalf("UserEmail = %q, want alice@example.com", assignment.UserEmail)
	}
}

func TestHandlerReroutesStickySessionWhenAssignedAccountExhausted(t *testing.T) {
	var auths []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auths = append(auths, r.Header.Get("Authorization"))
		w.WriteHeader(http.StatusNoContent)
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
	if _, err := store.Put("codex", "session-1", "empty@example.com", ""); err != nil {
		t.Fatal(err)
	}
	handler := Server{
		Upstream: upstreamURL,
		Accounts: []accounts.Account{
			{ID: "empty@example.com", AuthMode: accounts.AuthModeOAuth, Token: "empty-token"},
			{ID: "healthy@example.com", AuthMode: accounts.AuthModeOAuth, Token: "healthy-token"},
		},
		Sessions: store,
		Scheduler: selectacct.NewScheduler([]selectacct.Score{
			{AccountID: "empty@example.com", Headroom: 0, ShortHeadroom: 0},
			{AccountID: "healthy@example.com", Headroom: 0.90, ShortHeadroom: 0.90},
		}),
		MaxBodyBytes: 1024,
	}.Handler()
	subrouter := httptest.NewServer(handler)
	defer subrouter.Close()

	req, err := http.NewRequest(http.MethodPost, subrouter.URL+"/v1/responses", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Subrouter-Session", "session-1")
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if _, err := io.Copy(io.Discard, response.Body); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", response.StatusCode)
	}

	if strings.Join(auths, "\x00") != "Bearer healthy-token" {
		t.Fatalf("auths = %#v, want healthy account", auths)
	}
	assignment, ok := store.Get("codex", "session-1")
	if !ok {
		t.Fatal("missing session-1 assignment")
	}
	if assignment.AccountID != "healthy@example.com" {
		t.Fatalf("AccountID = %q, want healthy@example.com", assignment.AccountID)
	}
}

func TestHandlerReroutesColdStickySessionWhenAssignedAccountBelowHeadroom(t *testing.T) {
	var auths []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auths = append(auths, r.Header.Get("Authorization"))
		w.WriteHeader(http.StatusNoContent)
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
	if _, err := store.Put("codex", "session-1", "low@example.com", ""); err != nil {
		t.Fatal(err)
	}
	handler := Server{
		Upstream: upstreamURL,
		Accounts: []accounts.Account{
			{ID: "low@example.com", AuthMode: accounts.AuthModeOAuth, Token: "low-token"},
			{ID: "healthy@example.com", AuthMode: accounts.AuthModeOAuth, Token: "healthy-token"},
		},
		Sessions: store,
		Scheduler: selectacct.NewScheduler([]selectacct.Score{
			{AccountID: "low@example.com", Headroom: 0.10, ShortHeadroom: 0.10},
			{AccountID: "healthy@example.com", Headroom: 0.90, ShortHeadroom: 0.90},
		}),
		MaxBodyBytes: 1024,
	}.Handler()
	subrouter := httptest.NewServer(handler)
	defer subrouter.Close()

	req, err := http.NewRequest(http.MethodPost, subrouter.URL+"/v1/responses", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Subrouter-Session", "session-1")
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if _, err := io.Copy(io.Discard, response.Body); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", response.StatusCode)
	}

	if strings.Join(auths, "\x00") != "Bearer healthy-token" {
		t.Fatalf("auths = %#v, want healthy account", auths)
	}
	assignment, ok := store.Get("codex", "session-1")
	if !ok {
		t.Fatal("missing session-1 assignment")
	}
	if assignment.AccountID != "healthy@example.com" {
		t.Fatalf("AccountID = %q, want healthy@example.com", assignment.AccountID)
	}
}

func TestHandlerRoutesSparkModelUsingSparkQuota(t *testing.T) {
	var auths []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auths = append(auths, r.Header.Get("Authorization"))
		w.WriteHeader(http.StatusNoContent)
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
	handler := Server{
		Upstream: upstreamURL,
		Accounts: []accounts.Account{
			{ID: "normal-healthy@example.com", AuthMode: accounts.AuthModeOAuth, Token: "normal-token"},
			{ID: "spark-healthy@example.com", AuthMode: accounts.AuthModeOAuth, Token: "spark-token"},
		},
		Sessions: store,
		Scheduler: selectacct.NewScheduler([]selectacct.Score{
			selectacct.ScoreFromLimitWindows("normal-healthy@example.com", 0, []selectacct.LimitWindow{
				{Name: "primary", UsedPercent: 1, LimitWindowSeconds: 5 * 60 * 60},
				{Name: "secondary", UsedPercent: 2, LimitWindowSeconds: 7 * 24 * 60 * 60},
				{Name: "GPT-5.3-Codex-Spark/primary", Feature: "GPT-5.3-Codex-Spark", UsedPercent: 100, LimitWindowSeconds: 5 * 60 * 60},
				{Name: "GPT-5.3-Codex-Spark/secondary", Feature: "GPT-5.3-Codex-Spark", UsedPercent: 100, LimitWindowSeconds: 7 * 24 * 60 * 60},
			}),
			selectacct.ScoreFromLimitWindows("spark-healthy@example.com", 0, []selectacct.LimitWindow{
				{Name: "primary", UsedPercent: 100, LimitWindowSeconds: 5 * 60 * 60},
				{Name: "secondary", UsedPercent: 100, LimitWindowSeconds: 7 * 24 * 60 * 60},
				{Name: "GPT-5.3-Codex-Spark/primary", Feature: "GPT-5.3-Codex-Spark", UsedPercent: 1, LimitWindowSeconds: 5 * 60 * 60},
				{Name: "GPT-5.3-Codex-Spark/secondary", Feature: "GPT-5.3-Codex-Spark", UsedPercent: 2, LimitWindowSeconds: 7 * 24 * 60 * 60},
			}),
		}),
		MaxBodyBytes: 1024,
	}.Handler()
	subrouter := httptest.NewServer(handler)
	defer subrouter.Close()

	req, err := http.NewRequest(http.MethodPost, subrouter.URL+"/v1/responses", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Subrouter-Session", "spark-session")
	req.Header.Set("X-Subrouter-Model", "GPT-5.3-Codex-Spark")
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if _, err := io.Copy(io.Discard, response.Body); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", response.StatusCode)
	}
	if strings.Join(auths, "\x00") != "Bearer spark-token" {
		t.Fatalf("auths = %#v, want Spark account", auths)
	}
}

func TestHandlerKeepsActiveStickySessionWhenAssignedAccountBelowHeadroom(t *testing.T) {
	var mu sync.Mutex
	var auths []string
	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	var firstOnce sync.Once
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		auths = append(auths, r.Header.Get("Authorization"))
		mu.Unlock()

		block := false
		if r.URL.Path == "/v1/responses" {
			firstOnce.Do(func() {
				block = true
				close(firstStarted)
			})
		}
		if block {
			<-releaseFirst
		}
		w.WriteHeader(http.StatusNoContent)
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
	if _, err := store.Put("codex", "session-1", "low@example.com", ""); err != nil {
		t.Fatal(err)
	}
	schedulerRef := selectacct.NewSchedulerRef(selectacct.NewScheduler([]selectacct.Score{
		{AccountID: "low@example.com", Headroom: 0.90, ShortHeadroom: 0.90},
		{AccountID: "healthy@example.com", Headroom: 0.80, ShortHeadroom: 0.80},
	}))
	handler := Server{
		Upstream: upstreamURL,
		Accounts: []accounts.Account{
			{ID: "low@example.com", AuthMode: accounts.AuthModeOAuth, Token: "low-token"},
			{ID: "healthy@example.com", AuthMode: accounts.AuthModeOAuth, Token: "healthy-token"},
		},
		Sessions:     store,
		SchedulerRef: schedulerRef,
		MaxBodyBytes: 1024,
	}.Handler()
	subrouter := httptest.NewServer(handler)
	defer subrouter.Close()

	firstReq, err := http.NewRequest(http.MethodPost, subrouter.URL+"/v1/responses", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	firstReq.Header.Set("X-Subrouter-Session", "session-1")
	firstErr := make(chan error, 1)
	go func() {
		response, err := http.DefaultClient.Do(firstReq)
		if err != nil {
			firstErr <- err
			return
		}
		defer response.Body.Close()
		if _, err := io.Copy(io.Discard, response.Body); err != nil {
			firstErr <- err
			return
		}
		if response.StatusCode != http.StatusNoContent {
			firstErr <- fmt.Errorf("first status = %d, want 204", response.StatusCode)
			return
		}
		firstErr <- nil
	}()

	select {
	case <-firstStarted:
	case err := <-firstErr:
		t.Fatalf("first request finished before becoming active: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for first request to become active")
	}

	schedulerRef.Set(selectacct.NewScheduler([]selectacct.Score{
		{AccountID: "low@example.com", Headroom: 0.10, ShortHeadroom: 0.10},
		{AccountID: "healthy@example.com", Headroom: 0.90, ShortHeadroom: 0.90},
	}))

	secondReq, err := http.NewRequest(http.MethodGet, subrouter.URL+"/backend-api/codex/analytics-events/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	secondReq.Header.Set("X-Subrouter-Session", "session-1")
	secondResp, err := http.DefaultClient.Do(secondReq)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(io.Discard, secondResp.Body); err != nil {
		t.Fatal(err)
	}
	if err := secondResp.Body.Close(); err != nil {
		t.Fatal(err)
	}
	if secondResp.StatusCode != http.StatusNoContent {
		t.Fatalf("second status = %d, want 204", secondResp.StatusCode)
	}

	close(releaseFirst)
	if err := <-firstErr; err != nil {
		t.Fatal(err)
	}

	thirdReq, err := http.NewRequest(http.MethodPost, subrouter.URL+"/v1/responses/compact", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	thirdReq.Header.Set("X-Subrouter-Session", "session-1")
	thirdResp, err := http.DefaultClient.Do(thirdReq)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(io.Discard, thirdResp.Body); err != nil {
		t.Fatal(err)
	}
	if err := thirdResp.Body.Close(); err != nil {
		t.Fatal(err)
	}
	if thirdResp.StatusCode != http.StatusNoContent {
		t.Fatalf("third status = %d, want 204", thirdResp.StatusCode)
	}

	mu.Lock()
	got := append([]string(nil), auths...)
	mu.Unlock()
	want := []string{"Bearer low-token", "Bearer low-token", "Bearer healthy-token"}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("auths = %#v, want %#v", got, want)
	}
	assignment, ok := store.Get("codex", "session-1")
	if !ok {
		t.Fatal("missing session-1 assignment")
	}
	if assignment.AccountID != "healthy@example.com" {
		t.Fatalf("AccountID = %q, want healthy@example.com", assignment.AccountID)
	}
}

func TestHandlerReroutesActiveStickySessionWhenAssignedAccountExhausted(t *testing.T) {
	var mu sync.Mutex
	var auths []string
	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	var firstOnce sync.Once
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		auths = append(auths, r.Header.Get("Authorization"))
		mu.Unlock()

		block := false
		if r.URL.Path == "/v1/responses" {
			firstOnce.Do(func() {
				block = true
				close(firstStarted)
			})
		}
		if block {
			<-releaseFirst
		}
		w.WriteHeader(http.StatusNoContent)
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
	if _, err := store.Put("codex", "session-1", "empty@example.com", ""); err != nil {
		t.Fatal(err)
	}
	schedulerRef := selectacct.NewSchedulerRef(selectacct.NewScheduler([]selectacct.Score{
		{AccountID: "empty@example.com", Headroom: 0.90, ShortHeadroom: 0.90},
		{AccountID: "healthy@example.com", Headroom: 0.80, ShortHeadroom: 0.80},
	}))
	handler := Server{
		Upstream: upstreamURL,
		Accounts: []accounts.Account{
			{ID: "empty@example.com", AuthMode: accounts.AuthModeOAuth, Token: "empty-token"},
			{ID: "healthy@example.com", AuthMode: accounts.AuthModeOAuth, Token: "healthy-token"},
		},
		Sessions:     store,
		SchedulerRef: schedulerRef,
		MaxBodyBytes: 1024,
	}.Handler()
	subrouter := httptest.NewServer(handler)
	defer subrouter.Close()

	firstReq, err := http.NewRequest(http.MethodPost, subrouter.URL+"/v1/responses", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	firstReq.Header.Set("X-Subrouter-Session", "session-1")
	firstErr := make(chan error, 1)
	go func() {
		response, err := http.DefaultClient.Do(firstReq)
		if err != nil {
			firstErr <- err
			return
		}
		defer response.Body.Close()
		if _, err := io.Copy(io.Discard, response.Body); err != nil {
			firstErr <- err
			return
		}
		if response.StatusCode != http.StatusNoContent {
			firstErr <- fmt.Errorf("first status = %d, want 204", response.StatusCode)
			return
		}
		firstErr <- nil
	}()

	select {
	case <-firstStarted:
	case err := <-firstErr:
		t.Fatalf("first request finished before becoming active: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for first request to become active")
	}

	schedulerRef.MarkExhausted(accounts.ProviderCodex, "empty@example.com", "")

	secondReq, err := http.NewRequest(http.MethodGet, subrouter.URL+"/backend-api/codex/analytics-events/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	secondReq.Header.Set("X-Subrouter-Session", "session-1")
	secondResp, err := http.DefaultClient.Do(secondReq)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(io.Discard, secondResp.Body); err != nil {
		t.Fatal(err)
	}
	if err := secondResp.Body.Close(); err != nil {
		t.Fatal(err)
	}
	if secondResp.StatusCode != http.StatusNoContent {
		t.Fatalf("second status = %d, want 204", secondResp.StatusCode)
	}

	close(releaseFirst)
	if err := <-firstErr; err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	got := append([]string(nil), auths...)
	mu.Unlock()
	want := []string{"Bearer empty-token", "Bearer healthy-token"}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("auths = %#v, want %#v", got, want)
	}
	assignment, ok := store.Get("codex", "session-1")
	if !ok {
		t.Fatal("missing session-1 assignment")
	}
	if assignment.AccountID != "healthy@example.com" {
		t.Fatalf("AccountID = %q, want healthy@example.com", assignment.AccountID)
	}
}

func TestHandlerRefreshesStaleUsageScoresBeforeReusingStickySession(t *testing.T) {
	var auths []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auths = append(auths, r.Header.Get("Authorization"))
		w.WriteHeader(http.StatusNoContent)
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
	if _, err := store.Put("codex", "session-1", "empty@example.com", ""); err != nil {
		t.Fatal(err)
	}
	schedulerRef := selectacct.NewSchedulerRef(selectacct.NewScheduler([]selectacct.Score{
		{AccountID: "empty@example.com", Headroom: 0.80, ShortHeadroom: 0.80},
		{AccountID: "healthy@example.com", Headroom: 0.80, ShortHeadroom: 0.80},
	}))
	schedulerRef.SetUpdatedAt(time.Now().Add(-time.Hour))
	refreshed := false
	handler := Server{
		Upstream: upstreamURL,
		Accounts: []accounts.Account{
			{ID: "empty@example.com", AuthMode: accounts.AuthModeOAuth, Token: "empty-token"},
			{ID: "healthy@example.com", AuthMode: accounts.AuthModeOAuth, Token: "healthy-token"},
		},
		Sessions:      store,
		SchedulerRef:  schedulerRef,
		UsageScoreTTL: time.Minute,
		ScoreAccounts: func(_ context.Context, candidates []accounts.Account) ([]selectacct.Score, int) {
			refreshed = true
			if len(candidates) != 2 {
				t.Fatalf("candidates = %#v, want both OAuth accounts", candidates)
			}
			return []selectacct.Score{
				{AccountID: "empty@example.com", Headroom: 0, ShortHeadroom: 0},
				{AccountID: "healthy@example.com", Headroom: 0.80, ShortHeadroom: 0.80},
			}, 2
		},
		MaxBodyBytes: 1024,
	}.Handler()
	subrouter := httptest.NewServer(handler)
	defer subrouter.Close()

	req, err := http.NewRequest(http.MethodPost, subrouter.URL+"/v1/responses", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Subrouter-Session", "session-1")
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if _, err := io.Copy(io.Discard, response.Body); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", response.StatusCode)
	}
	if !refreshed {
		t.Fatal("usage scores were not refreshed before account selection")
	}
	if strings.Join(auths, "\x00") != "Bearer healthy-token" {
		t.Fatalf("auths = %#v, want healthy account", auths)
	}
	assignment, ok := store.Get("codex", "session-1")
	if !ok {
		t.Fatal("missing session-1 assignment")
	}
	if assignment.AccountID != "healthy@example.com" {
		t.Fatalf("AccountID = %q, want healthy@example.com", assignment.AccountID)
	}
}

func TestHandlerMarksWebSocketUsageLimitAccountExhausted(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(_ *http.Request) bool { return true }}
	var auths []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auths = append(auths, r.Header.Get("Authorization"))
		if !websocket.IsWebSocketUpgrade(r) {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Fatalf("upgrade: %v", err)
		}
		defer conn.Close()
		if _, _, err := conn.ReadMessage(); err != nil {
			t.Fatalf("read client message: %v", err)
		}
		if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"error","error":{"type":"usage_limit_reached","message":"The usage limit has been reached"}}`)); err != nil {
			t.Fatalf("write usage error: %v", err)
		}
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
	if _, err := store.Put("codex", "session-1", "empty@example.com", ""); err != nil {
		t.Fatal(err)
	}
	schedulerRef := selectacct.NewSchedulerRef(selectacct.NewScheduler([]selectacct.Score{
		{AccountID: "empty@example.com", Headroom: 0.80, ShortHeadroom: 0.80},
		{AccountID: "healthy@example.com", Headroom: 0.80, ShortHeadroom: 0.80},
	}))
	handler := Server{
		Upstream: upstreamURL,
		Accounts: []accounts.Account{
			{ID: "empty@example.com", AuthMode: accounts.AuthModeOAuth, Token: "empty-token"},
			{ID: "healthy@example.com", AuthMode: accounts.AuthModeOAuth, Token: "healthy-token"},
		},
		Sessions:     store,
		SchedulerRef: schedulerRef,
		MaxBodyBytes: 1024,
	}.Handler()
	subrouter := httptest.NewServer(handler)
	defer subrouter.Close()

	wsURL := "ws" + strings.TrimPrefix(subrouter.URL, "http") + "/v1/responses"
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, http.Header{
		"X-Subrouter-Session": []string{"session-1"},
		"X-Subrouter-Model":   []string{"gpt-5.6-sol"},
	})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create"}`)); err != nil {
		t.Fatalf("write create: %v", err)
	}
	_, body, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read usage error: %v", err)
	}
	if !strings.Contains(string(body), "usage_limit_reached") {
		t.Fatalf("websocket body = %q, want usage limit error", string(body))
	}
	_ = conn.Close()
	if _, accountMarked := schedulerRef.ExhaustedUntilFor(accounts.ProviderCodex, "empty@example.com", ""); !accountMarked {
		t.Fatal("Codex usage limit must mark the whole account exhausted")
	}
	if _, modelMarked := schedulerRef.ModelIncompatibleUntilFor(accounts.ProviderCodex, "empty@example.com", "gpt-5.6-sol"); modelMarked {
		t.Fatal("Codex usage limit must not be scoped to the request model")
	}

	req, err := http.NewRequest(http.MethodPost, subrouter.URL+"/v1/responses", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Subrouter-Session", "session-1")
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if _, err := io.Copy(io.Discard, response.Body); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", response.StatusCode)
	}
	want := []string{"Bearer empty-token", "Bearer healthy-token"}
	if strings.Join(auths, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("auths = %#v, want %#v", auths, want)
	}
	assignment, ok := store.Get("codex", "session-1")
	if !ok {
		t.Fatal("missing session-1 assignment")
	}
	if assignment.AccountID != "healthy@example.com" {
		t.Fatalf("AccountID = %q, want healthy@example.com", assignment.AccountID)
	}
}

func TestHandlerRetriesHTTPUsageLimitOnAlternateOAuthAccount(t *testing.T) {
	var auths []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		auths = append(auths, auth)
		if auth == "Bearer empty-token" {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"type":"usage_limit_reached","message":"The usage limit has been reached"}}`))
			return
		}
		w.WriteHeader(http.StatusNoContent)
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
	if _, err := store.Put("codex", "session-1", "empty@example.com", ""); err != nil {
		t.Fatal(err)
	}
	schedulerRef := selectacct.NewSchedulerRef(selectacct.NewScheduler([]selectacct.Score{
		{AccountID: "empty@example.com", Headroom: 0.80, ShortHeadroom: 0.80},
		{AccountID: "healthy@example.com", Headroom: 0.80, ShortHeadroom: 0.80},
	}))
	handler := Server{
		Upstream: upstreamURL,
		Accounts: []accounts.Account{
			{ID: "empty@example.com", AuthMode: accounts.AuthModeOAuth, Token: "empty-token"},
			{ID: "healthy@example.com", AuthMode: accounts.AuthModeOAuth, Token: "healthy-token"},
		},
		Sessions:     store,
		SchedulerRef: schedulerRef,
		MaxBodyBytes: 1024,
	}.Handler()
	subrouter := httptest.NewServer(handler)
	defer subrouter.Close()

	firstReq, err := http.NewRequest(http.MethodPost, subrouter.URL+"/v1/responses/compact", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	firstReq.Header.Set("X-Subrouter-Session", "session-1")
	firstResp, err := http.DefaultClient.Do(firstReq)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(io.Discard, firstResp.Body); err != nil {
		t.Fatal(err)
	}
	if err := firstResp.Body.Close(); err != nil {
		t.Fatal(err)
	}
	if firstResp.StatusCode != http.StatusNoContent {
		t.Fatalf("first status = %d, want 204", firstResp.StatusCode)
	}
	want := []string{"Bearer empty-token", "Bearer healthy-token"}
	if strings.Join(auths, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("auths = %#v, want %#v", auths, want)
	}
	assignment, ok := store.Get("codex", "session-1")
	if !ok {
		t.Fatal("missing session-1 assignment")
	}
	if assignment.AccountID != "healthy@example.com" {
		t.Fatalf("AccountID = %q, want healthy@example.com", assignment.AccountID)
	}
}

func TestHandlerRetriesCodexModelCompatibilityErrorOnAlternateOAuthAccount(t *testing.T) {
	var auths []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		auths = append(auths, auth)
		if auth == "Bearer incompatible-token" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"type":"invalid_request_error","message":"The 'gpt-5.6-sol' model is not supported when using Codex with a ChatGPT account."}}`))
			return
		}
		w.WriteHeader(http.StatusNoContent)
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
	if _, err := store.Put("codex", "session-1", "incompatible@example.com", ""); err != nil {
		t.Fatal(err)
	}
	schedulerRef := selectacct.NewSchedulerRef(selectacct.NewScheduler([]selectacct.Score{
		{AccountID: "incompatible@example.com", Headroom: 0.80, ShortHeadroom: 0.80},
		{AccountID: "compatible@example.com", Headroom: 0.80, ShortHeadroom: 0.80},
	}))
	handler := Server{
		Upstream: upstreamURL,
		Accounts: []accounts.Account{
			{ID: "incompatible@example.com", AuthMode: accounts.AuthModeOAuth, Token: "incompatible-token"},
			{ID: "compatible@example.com", AuthMode: accounts.AuthModeOAuth, Token: "compatible-token"},
		},
		Sessions:     store,
		SchedulerRef: schedulerRef,
		MaxBodyBytes: 1024,
	}.Handler()
	subrouter := httptest.NewServer(handler)
	defer subrouter.Close()

	req, err := http.NewRequest(http.MethodPost, subrouter.URL+"/v1/responses", strings.NewReader(`{"model":"gpt-5.6-sol","input":"hello"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Subrouter-Session", "session-1")
	req.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(io.Discard, response.Body); err != nil {
		t.Fatal(err)
	}
	if err := response.Body.Close(); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", response.StatusCode)
	}
	want := []string{"Bearer incompatible-token", "Bearer compatible-token"}
	if strings.Join(auths, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("auths = %#v, want %#v", auths, want)
	}
	assignment, ok := store.Get("codex", "session-1")
	if !ok {
		t.Fatal("missing session-1 assignment")
	}
	if assignment.AccountID != "compatible@example.com" {
		t.Fatalf("AccountID = %q, want compatible@example.com", assignment.AccountID)
	}
	_, modelMarked := schedulerRef.ModelIncompatibleUntilFor(accounts.ProviderCodex, "incompatible@example.com", "gpt-5.6-sol")
	_, accountMarked := schedulerRef.ExhaustedUntilFor(accounts.ProviderCodex, "incompatible@example.com", "")
	if !modelMarked {
		t.Fatalf("missing model-scoped incompatibility mark (account_marked=%t)", accountMarked)
	}
	if accountMarked {
		t.Fatal("model incompatibility must not mark the whole account exhausted")
	}
}

func TestHandlerDoesNotRetryCodexModelCompatibilityErrorOnAPIKeyAccount(t *testing.T) {
	var auths []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auths = append(auths, r.Header.Get("Authorization"))
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"The 'gpt-5.6-sol' model is not supported when using Codex with a ChatGPT account."}}`))
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
	if _, err := store.Put("codex", "session-1", "incompatible@example.com", ""); err != nil {
		t.Fatal(err)
	}
	handler := Server{
		Upstream: upstreamURL,
		Accounts: []accounts.Account{
			{ID: "incompatible@example.com", AuthMode: accounts.AuthModeOAuth, Token: "incompatible-token"},
			{ID: "apikey:team-codex-1", AuthMode: accounts.AuthModeAPIKey, Token: "api-token"},
		},
		Sessions: store,
		SchedulerRef: selectacct.NewSchedulerRef(selectacct.NewScheduler([]selectacct.Score{
			{AccountID: "incompatible@example.com", Headroom: 0.80, ShortHeadroom: 0.80},
			{AccountID: "apikey:team-codex-1", Headroom: 0.01, ShortHeadroom: 0.01},
		})),
		MaxBodyBytes: 1024,
	}.Handler()
	subrouter := httptest.NewServer(handler)
	defer subrouter.Close()

	req, err := http.NewRequest(http.MethodPost, subrouter.URL+"/v1/responses", strings.NewReader(`{"model":"gpt-5.6-sol","input":"hello"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Subrouter-Session", "session-1")
	req.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if _, err := io.Copy(io.Discard, response.Body); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", response.StatusCode)
	}
	want := []string{"Bearer incompatible-token"}
	if strings.Join(auths, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("auths = %#v, want %#v", auths, want)
	}
}

func TestHandlerDoesNotMarkCodexAccountWideWhenCompatibilityModelIsUnknown(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer incompatible-token" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"The 'gpt-5.6-sol' model is not supported when using Codex with a ChatGPT account."}}`))
			return
		}
		w.WriteHeader(http.StatusNoContent)
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
	if _, err := store.Put("codex", "session-1", "incompatible@example.com", ""); err != nil {
		t.Fatal(err)
	}
	schedulerRef := selectacct.NewSchedulerRef(selectacct.NewScheduler([]selectacct.Score{
		{AccountID: "incompatible@example.com", Headroom: 0.80, ShortHeadroom: 0.80},
		{AccountID: "compatible@example.com", Headroom: 0.80, ShortHeadroom: 0.80},
	}))
	handler := Server{
		Upstream: upstreamURL,
		Accounts: []accounts.Account{
			{ID: "incompatible@example.com", AuthMode: accounts.AuthModeOAuth, Token: "incompatible-token"},
			{ID: "compatible@example.com", AuthMode: accounts.AuthModeOAuth, Token: "compatible-token"},
		},
		Sessions:     store,
		SchedulerRef: schedulerRef,
		MaxBodyBytes: 1024,
	}.Handler()
	subrouter := httptest.NewServer(handler)
	defer subrouter.Close()

	requestBody := `{"input":"` + strings.Repeat("x", (8<<20)+1) + `","model":"gpt-5.6-sol"}`
	req, err := http.NewRequest(http.MethodPost, subrouter.URL+"/v1/responses", strings.NewReader(requestBody))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Subrouter-Session", "session-1")
	req.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if _, err := io.Copy(io.Discard, response.Body); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", response.StatusCode)
	}
	if _, accountMarked := schedulerRef.ExhaustedUntilFor(accounts.ProviderCodex, "incompatible@example.com", ""); accountMarked {
		t.Fatal("unknown compatibility model must not mark the whole account exhausted")
	}
}

func TestCaptureResponseBodyMarksCodexModelCompatibility(t *testing.T) {
	schedulerRef := selectacct.NewSchedulerRef(selectacct.NewScheduler([]selectacct.Score{{
		AccountID:     "incompatible@example.com",
		Provider:      accounts.ProviderCodex,
		Headroom:      0.8,
		ShortHeadroom: 0.8,
	}}))
	server := Server{SchedulerRef: schedulerRef}
	response := &http.Response{
		StatusCode: http.StatusBadRequest,
		Header:     http.Header{},
		Body:       io.NopCloser(strings.NewReader(`{"error":{"message":"The 'gpt-5.6-sol' model is not supported when using Codex with a ChatGPT account."}}`)),
	}
	server.captureResponseBody(response, "codex", "session-1", "incompatible@example.com", "", "", "gpt-5.6-sol", "/v1/responses")
	if _, err := io.Copy(io.Discard, response.Body); err != nil {
		t.Fatal(err)
	}
	if err := response.Body.Close(); err != nil {
		t.Fatal(err)
	}

	if _, modelMarked := schedulerRef.ModelIncompatibleUntilFor(accounts.ProviderCodex, "incompatible@example.com", "gpt-5.6-sol"); !modelMarked {
		t.Fatal("passive compatibility inspection must mark the rejected model")
	}
	if _, accountMarked := schedulerRef.ExhaustedUntilFor(accounts.ProviderCodex, "incompatible@example.com", ""); accountMarked {
		t.Fatal("passive compatibility inspection must not mark the whole account")
	}
}

func TestHandlerMarksWebSocketModelCompatibilityAndReroutesReconnect(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(_ *http.Request) bool { return true }}
	var auths []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		auths = append(auths, auth)
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Fatalf("upgrade: %v", err)
		}
		defer conn.Close()
		if _, _, err := conn.ReadMessage(); err != nil {
			t.Fatalf("read client message: %v", err)
		}
		if auth == "Bearer incompatible-token" {
			if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"error","error":{"message":"The 'gpt-5.6-sol' model is not supported when using Codex with a ChatGPT account."}}`)); err != nil {
				t.Fatalf("write compatibility error: %v", err)
			}
			return
		}
		if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.done"}`)); err != nil {
			t.Fatalf("write success: %v", err)
		}
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
	if _, err := store.Put("codex", "session-1", "incompatible@example.com", ""); err != nil {
		t.Fatal(err)
	}
	schedulerRef := selectacct.NewSchedulerRef(selectacct.NewScheduler([]selectacct.Score{
		{AccountID: "incompatible@example.com", Headroom: 0.80, ShortHeadroom: 0.80},
		{AccountID: "compatible@example.com", Headroom: 0.80, ShortHeadroom: 0.80},
	}))
	handler := Server{
		Upstream: upstreamURL,
		Accounts: []accounts.Account{
			{ID: "incompatible@example.com", AuthMode: accounts.AuthModeOAuth, Token: "incompatible-token"},
			{ID: "compatible@example.com", AuthMode: accounts.AuthModeOAuth, Token: "compatible-token"},
		},
		Sessions:     store,
		SchedulerRef: schedulerRef,
		MaxBodyBytes: 1024,
	}.Handler()
	subrouter := httptest.NewServer(handler)
	defer subrouter.Close()

	wsURL := "ws" + strings.TrimPrefix(subrouter.URL, "http") + "/v1/responses"
	header := http.Header{
		"X-Subrouter-Session": []string{"session-1"},
	}
	for attempt := 0; attempt < 2; attempt++ {
		conn, _, err := websocket.DefaultDialer.Dial(wsURL, header)
		if err != nil {
			t.Fatalf("dial attempt %d: %v", attempt+1, err)
		}
		if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","response":{"model":"gpt-5.6-sol"}}`)); err != nil {
			t.Fatalf("write create attempt %d: %v", attempt+1, err)
		}
		_, body, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("read attempt %d: %v", attempt+1, err)
		}
		_ = conn.Close()
		if attempt == 0 && !strings.Contains(string(body), "not supported") {
			t.Fatalf("first body = %q, want compatibility error", body)
		}
		if attempt == 1 && !strings.Contains(string(body), "response.done") {
			t.Fatalf("second body = %q, want success", body)
		}
	}

	want := []string{"Bearer incompatible-token", "Bearer compatible-token"}
	if strings.Join(auths, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("auths = %#v, want %#v", auths, want)
	}
	if _, modelMarked := schedulerRef.ModelIncompatibleUntilFor(accounts.ProviderCodex, "incompatible@example.com", "gpt-5.6-sol"); !modelMarked {
		t.Fatal("WebSocket compatibility error must mark the rejected model")
	}
	if _, accountMarked := schedulerRef.ExhaustedUntilFor(accounts.ProviderCodex, "incompatible@example.com", ""); accountMarked {
		t.Fatal("WebSocket compatibility error must not mark the whole account")
	}
}

func TestCodexWebSocketRequestModel(t *testing.T) {
	for _, test := range []struct {
		name string
		body string
		want string
	}{
		{name: "nested response", body: `{"type":"response.create","response":{"model":"gpt-5.6-sol"}}`, want: "gpt-5.6-sol"},
		{name: "top level", body: `{"type":"response.create","model":"gpt-5.6-sol"}`, want: "gpt-5.6-sol"},
		{name: "other event", body: `{"type":"response.done","response":{"model":"gpt-5.6-sol"}}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := codexWebSocketRequestModel([]byte(test.body)); got != test.want {
				t.Fatalf("model = %q, want %q", got, test.want)
			}
		})
	}
}

func TestCodexWebSocketResponseFinished(t *testing.T) {
	for _, eventType := range []string{"response.completed", "response.failed", "response.incomplete", "response.done", "error"} {
		t.Run(eventType, func(t *testing.T) {
			if !codexWebSocketResponseFinished([]byte(`{"type":"` + eventType + `"}`)) {
				t.Fatalf("%s should finish the current WebSocket response", eventType)
			}
		})
	}
	if codexWebSocketResponseFinished([]byte(`{"type":"response.output_text.delta"}`)) {
		t.Fatal("streaming delta must not finish the current WebSocket response")
	}
}

func TestWebSocketModelStateTracksSequentialResponses(t *testing.T) {
	state := &webSocketModelState{model: "default-model"}
	state.observe([]byte(`{"type":"response.create","model":"model-a"}`))
	state.observe([]byte(`{"type":"response.create","model":"model-b"}`))

	if got := state.current(); got != "model-a" {
		t.Fatalf("first current model = %q, want model-a", got)
	}
	state.complete()
	if got := state.current(); got != "model-b" {
		t.Fatalf("second current model = %q, want model-b", got)
	}
}

func TestHandlerRetriesHTTPUsageLimitToBelowThresholdFallback(t *testing.T) {
	var auths []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		auths = append(auths, auth)
		if auth == "Bearer empty-token" {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"type":"usage_limit_reached","message":"The usage limit has been reached"}}`))
			return
		}
		w.WriteHeader(http.StatusNoContent)
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
	if _, err := store.Put("codex", "session-1", "empty@example.com", ""); err != nil {
		t.Fatal(err)
	}
	schedulerRef := selectacct.NewSchedulerRef(selectacct.NewScheduler([]selectacct.Score{
		{AccountID: "empty@example.com", Headroom: 0.80, ShortHeadroom: 0.80},
		{AccountID: "fallback@example.com", Headroom: 0.01, ShortHeadroom: 0.80},
	}))
	handler := Server{
		Upstream: upstreamURL,
		Accounts: []accounts.Account{
			{ID: "empty@example.com", AuthMode: accounts.AuthModeOAuth, Token: "empty-token"},
			{ID: "fallback@example.com", AuthMode: accounts.AuthModeOAuth, Token: "fallback-token"},
		},
		Sessions:     store,
		SchedulerRef: schedulerRef,
		MaxBodyBytes: 1024,
	}.Handler()
	subrouter := httptest.NewServer(handler)
	defer subrouter.Close()

	req, err := http.NewRequest(http.MethodPost, subrouter.URL+"/v1/responses", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Subrouter-Session", "session-1")
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(io.Discard, response.Body); err != nil {
		t.Fatal(err)
	}
	if err := response.Body.Close(); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", response.StatusCode)
	}
	want := []string{"Bearer empty-token", "Bearer fallback-token"}
	if strings.Join(auths, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("auths = %#v, want %#v", auths, want)
	}
	assignment, ok := store.Get("codex", "session-1")
	if !ok {
		t.Fatal("missing session-1 assignment")
	}
	if assignment.AccountID != "fallback@example.com" {
		t.Fatalf("AccountID = %q, want fallback@example.com", assignment.AccountID)
	}
}

// When every OAuth account is scored exhausted, the router must still route
// optimistically and let the upstream decide, rather than refusing with a 503.
// Scores can be stale (cached usage behind a rate-limited upstream), and a hard
// refusal here caused real outages where healthy accounts looked exhausted.
func TestHandlerRoutesOptimisticallyWhenAllOAuthScoredExhausted(t *testing.T) {
	var auths []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auths = append(auths, r.Header.Get("Authorization"))
		w.WriteHeader(http.StatusNoContent)
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
	handler := Server{
		Upstream: upstreamURL,
		Accounts: []accounts.Account{
			{ID: "short-empty@example.com", AuthMode: accounts.AuthModeOAuth, Token: "short-empty-token"},
			{ID: "weekly-empty@example.com", AuthMode: accounts.AuthModeOAuth, Token: "weekly-empty-token"},
		},
		Sessions: store,
		Scheduler: selectacct.NewScheduler([]selectacct.Score{
			{AccountID: "short-empty@example.com", Headroom: 0, ShortHeadroom: 0},
			{AccountID: "weekly-empty@example.com", Headroom: 0, ShortHeadroom: 1},
		}),
		MaxBodyBytes: 1024,
	}.Handler()
	subrouter := httptest.NewServer(handler)
	defer subrouter.Close()

	req, err := http.NewRequest(http.MethodPost, subrouter.URL+"/v1/responses", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Subrouter-Session", "session-1")
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if _, err := io.Copy(io.Discard, response.Body); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode == http.StatusServiceUnavailable {
		t.Fatal("router refused with 503 instead of routing optimistically on stale-exhausted scores")
	}
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204 (forwarded upstream)", response.StatusCode)
	}
	if len(auths) != 1 {
		t.Fatalf("upstream auths = %#v, want exactly one forwarded request", auths)
	}
	if _, ok := store.Get("codex", "session-1"); !ok {
		t.Fatal("session should be assigned after optimistic routing")
	}
}

func TestHandlerUsesConstrainedOAuthInsteadOfExhaustedOAuth(t *testing.T) {
	var auths []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auths = append(auths, r.Header.Get("Authorization"))
		w.WriteHeader(http.StatusNoContent)
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
	handler := Server{
		Upstream: upstreamURL,
		Accounts: []accounts.Account{
			{ID: "short-empty@example.com", AuthMode: accounts.AuthModeOAuth, Token: "short-empty-token"},
			{ID: "near-threshold@example.com", AuthMode: accounts.AuthModeOAuth, Token: "near-threshold-token"},
		},
		Sessions: store,
		Scheduler: selectacct.NewScheduler([]selectacct.Score{
			{AccountID: "short-empty@example.com", Headroom: 0.79, ShortHeadroom: 0},
			{AccountID: "near-threshold@example.com", Headroom: 0.39, ShortHeadroom: 0.39},
		}),
		MaxBodyBytes: 1024,
	}.Handler()
	subrouter := httptest.NewServer(handler)
	defer subrouter.Close()

	req, err := http.NewRequest(http.MethodPost, subrouter.URL+"/v1/responses", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Subrouter-Session", "session-1")
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if _, err := io.Copy(io.Discard, response.Body); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", response.StatusCode)
	}
	if len(auths) != 1 || auths[0] != "Bearer near-threshold-token" {
		t.Fatalf("upstream auths = %#v, want constrained OAuth account", auths)
	}
	assignment, ok := store.Get("codex", "session-1")
	if !ok {
		t.Fatal("missing session assignment")
	}
	if assignment.AccountID != "near-threshold@example.com" {
		t.Fatalf("AccountID = %q, want near-threshold@example.com", assignment.AccountID)
	}
}

func TestHandlerScopesStickySessionsByAgentType(t *testing.T) {
	var auths []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auths = append(auths, r.Header.Get("Authorization"))
		w.WriteHeader(http.StatusNoContent)
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
	handler := Server{
		Upstream: upstreamURL,
		Accounts: []accounts.Account{
			{ID: "a@example.com", AuthMode: accounts.AuthModeOAuth, Token: "a-token"},
			{ID: "b@example.com", AuthMode: accounts.AuthModeOAuth, Token: "b-token"},
		},
		Sessions: store,
		Scheduler: selectacct.NewScheduler([]selectacct.Score{
			{AccountID: "a@example.com", Headroom: 0.80, ShortHeadroom: 0.80},
			{AccountID: "b@example.com", Headroom: 0.80, ShortHeadroom: 0.80},
		}),
		MaxBodyBytes: 1024,
	}.Handler()
	subrouter := httptest.NewServer(handler)
	defer subrouter.Close()

	for _, agentType := range []string{"codex", "claude"} {
		req, err := http.NewRequest(http.MethodPost, subrouter.URL+"/v1/responses", strings.NewReader(`{}`))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("X-Subrouter-Session", "same-session")
		req.Header.Set("X-Subrouter-Agent", agentType)
		response, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.Copy(io.Discard, response.Body); err != nil {
			t.Fatal(err)
		}
		if err := response.Body.Close(); err != nil {
			t.Fatal(err)
		}
	}

	want := []string{"Bearer a-token", "Bearer b-token"}
	if strings.Join(auths, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("auths = %#v, want %#v", auths, want)
	}
	if _, ok := store.Get("codex", "same-session"); !ok {
		t.Fatal("missing codex assignment")
	}
	if _, ok := store.Get("claude", "same-session"); !ok {
		t.Fatal("missing claude assignment")
	}
}

func TestHandlerKeepsCodexTurnIDsOnSameAccount(t *testing.T) {
	var auths []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auths = append(auths, r.Header.Get("Authorization"))
		w.WriteHeader(http.StatusNoContent)
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
	handler := Server{
		Upstream: upstreamURL,
		Accounts: []accounts.Account{
			{ID: "a@example.com", AuthMode: accounts.AuthModeOAuth, Token: "a-token"},
			{ID: "b@example.com", AuthMode: accounts.AuthModeOAuth, Token: "b-token"},
		},
		Sessions: store,
		Scheduler: selectacct.NewScheduler([]selectacct.Score{
			{AccountID: "a@example.com", Headroom: 0.80, ShortHeadroom: 0.80},
			{AccountID: "b@example.com", Headroom: 0.80, ShortHeadroom: 0.80},
		}),
		MaxBodyBytes: 1024,
	}.Handler()
	subrouter := httptest.NewServer(handler)
	defer subrouter.Close()

	for _, sessionID := range []string{"codex-session:4", "codex-session:7"} {
		req, err := http.NewRequest(http.MethodPost, subrouter.URL+"/v1/responses/compact", strings.NewReader(`{}`))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("X-Subrouter-Session", sessionID)
		response, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.Copy(io.Discard, response.Body); err != nil {
			t.Fatal(err)
		}
		if err := response.Body.Close(); err != nil {
			t.Fatal(err)
		}
	}

	want := []string{"Bearer a-token", "Bearer a-token"}
	if strings.Join(auths, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("auths = %#v, want %#v", auths, want)
	}
	assignment, ok := store.Get("codex", "codex-session:9")
	if !ok {
		t.Fatal("missing base codex assignment")
	}
	if assignment.SessionID != "codex-session" {
		t.Fatalf("SessionID = %q, want codex-session", assignment.SessionID)
	}
	if assignment.AccountID != "a@example.com" {
		t.Fatalf("AccountID = %q, want a@example.com", assignment.AccountID)
	}
}

func readTranscriptEvents(t *testing.T, path string) []map[string]any {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(body)), "\n")
	events := make([]map[string]any, 0, len(lines))
	for _, line := range lines {
		var event map[string]any
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatal(err)
		}
		events = append(events, event)
	}
	return events
}

func readTranscriptEventsEventually(t *testing.T, path string, wantAtLeast int) []map[string]any {
	t.Helper()
	// Transcript events are written server-side as the request/response bodies
	// stream and close, which races the client-side body close the caller does
	// before polling. The events also arrive incrementally, so returning the
	// instant len >= wantAtLeast could return before a still-pending event (e.g.
	// upstream_to_client) is written, producing the flaky "missing http_body
	// upstream_to_client transcript event". Wait until the count both reaches
	// wantAtLeast AND stops growing for a short grace window, so all pending
	// events are captured. 10s deadline absorbs CI load.
	deadline := time.Now().Add(10 * time.Second)
	const stableFor = 200 * time.Millisecond
	var lastEvents []map[string]any
	var stableSince time.Time
	for {
		body, err := os.ReadFile(path)
		if err == nil {
			lines := strings.Split(strings.TrimSpace(string(body)), "\n")
			events := make([]map[string]any, 0, len(lines))
			parseOK := true
			for _, line := range lines {
				if strings.TrimSpace(line) == "" {
					continue
				}
				var event map[string]any
				if err := json.Unmarshal([]byte(line), &event); err != nil {
					parseOK = false
					break
				}
				events = append(events, event)
			}
			if parseOK {
				if len(events) != len(lastEvents) {
					stableSince = time.Time{} // count changed; reset the grace window
				} else if len(events) >= wantAtLeast {
					if stableSince.IsZero() {
						stableSince = time.Now()
					} else if time.Since(stableSince) >= stableFor {
						return events
					}
				}
				lastEvents = events
			}
		}
		if time.Now().After(deadline) {
			if len(lastEvents) >= wantAtLeast {
				return lastEvents
			}
			t.Fatalf("transcript %s did not reach %d events", path, wantAtLeast)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func assertTranscriptPayload(t *testing.T, events []map[string]any, eventType, direction string, want []byte) {
	t.Helper()
	for _, event := range events {
		if event["type"] != eventType {
			continue
		}
		payload := event["payload"].(map[string]any)
		if payload["direction"] != direction {
			continue
		}
		var got []byte
		if encoded, ok := payload["body_base64"].(string); ok {
			decoded, err := base64.StdEncoding.DecodeString(encoded)
			if err != nil {
				t.Fatal(err)
			}
			got = decoded
		} else if streamID, ok := payload["stream_id"].(string); ok && payload["body_chunked"] == true {
			got = transcriptChunks(t, events, eventType+"_chunk", direction, streamID)
		} else {
			continue
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("%s %s payload = %q, want %q", eventType, direction, got, want)
		}
		return
	}
	t.Fatalf("missing %s %s transcript event", eventType, direction)
}

func transcriptChunks(t *testing.T, events []map[string]any, eventType, direction, streamID string) []byte {
	t.Helper()
	chunks := map[int][]byte{}
	var indexes []int
	for _, event := range events {
		if event["type"] != eventType {
			continue
		}
		payload := event["payload"].(map[string]any)
		if payload["direction"] != direction || payload["stream_id"] != streamID {
			continue
		}
		index := int(payload["chunk_index"].(float64))
		encoded := payload["body_base64"].(string)
		body, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := chunks[index]; !ok {
			indexes = append(indexes, index)
		}
		chunks[index] = body
	}
	sort.Ints(indexes)
	var body []byte
	for _, index := range indexes {
		body = append(body, chunks[index]...)
	}
	return body
}

func TestHandlerBalancesEquivalentNewSessionsByStoredCounts(t *testing.T) {
	var auths []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auths = append(auths, r.Header.Get("Authorization"))
		w.WriteHeader(http.StatusNoContent)
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
	handler := Server{
		Upstream: upstreamURL,
		Accounts: []accounts.Account{
			{ID: "a@example.com", AuthMode: accounts.AuthModeOAuth, Token: "a-token"},
			{ID: "b@example.com", AuthMode: accounts.AuthModeOAuth, Token: "b-token"},
		},
		Sessions: store,
		Scheduler: selectacct.NewScheduler([]selectacct.Score{
			{AccountID: "a@example.com", Headroom: 0.80, ShortHeadroom: 0.80},
			{AccountID: "b@example.com", Headroom: 0.80, ShortHeadroom: 0.80},
		}),
		MaxBodyBytes: 1024,
	}.Handler()
	subrouter := httptest.NewServer(handler)
	defer subrouter.Close()

	for _, sessionID := range []string{"session-1", "session-2"} {
		req, err := http.NewRequest(http.MethodPost, subrouter.URL+"/v1/responses", strings.NewReader(`{}`))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("X-Subrouter-Session", sessionID)
		response, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.Copy(io.Discard, response.Body); err != nil {
			t.Fatal(err)
		}
		if err := response.Body.Close(); err != nil {
			t.Fatal(err)
		}
	}

	want := []string{"Bearer a-token", "Bearer b-token"}
	if strings.Join(auths, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("auths = %#v, want %#v", auths, want)
	}
}
