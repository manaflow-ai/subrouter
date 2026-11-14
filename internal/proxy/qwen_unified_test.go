package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/selectacct"
	"github.com/manaflow-ai/subrouter/session"
)

func TestQwenUnifiedRouteFailover(t *testing.T) {
	for _, failure := range []int{429, 401, 404, 502, 503, 504} {
		t.Run(http.StatusText(failure), func(t *testing.T) {
			var oauthCalls, tokenCalls atomic.Int32
			oauth := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				oauthCalls.Add(1)
				if r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer oauth-key" {
					t.Errorf("wrong subscription target: %s", r.URL.Path)
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(failure)
				if failure == 404 {
					io.WriteString(w, `{"error":{"code":"model_not_found","message":"Model unavailable"}}`)
				} else {
					io.WriteString(w, `{"error":{"message":"quota or credential rejected"}}`)
				}
			}))
			defer oauth.Close()
			token := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				tokenCalls.Add(1)
				if r.URL.Path != "/compatible-mode/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer plan-key" {
					t.Errorf("wrong Token Plan target: %s", r.URL.Path)
				}
				w.Header().Set("Content-Type", "application/json")
				io.WriteString(w, `{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":1}}`)
			}))
			defer token.Close()
			sessions, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
			if err != nil {
				t.Fatal(err)
			}
			rows := []accounts.Account{{ID: "qwen-subscription:member", Provider: accounts.ProviderQwenOAuth, AuthMode: accounts.AuthModeOAuth, Token: "oauth-key"}, {ID: "qwen-token:plan", Provider: accounts.ProviderQwenToken, AuthMode: accounts.AuthModeAPIKey, Token: "plan-key"}}
			s := Server{Accounts: rows, Sessions: sessions, SchedulerRef: selectacct.NewSchedulerRef(selectacct.NewScheduler(nil)), QwenOAuthUpstream: mustParseURL(t, oauth.URL+"/v1"), QwenTokenUpstream: mustParseURL(t, token.URL+"/compatible-mode/v1"), MaxBodyBytes: 4096}
			handler := s.Handler()
			for range 2 {
				req := httptest.NewRequest("POST", "/qwen/v1/chat/completions", strings.NewReader(`{"model":"qwen3.8-max","messages":[]}`))
				req.Header.Set("X-Subrouter-Session", "sticky")
				rec := httptest.NewRecorder()
				handler.ServeHTTP(rec, req)
				if rec.Code != 200 || !strings.Contains(rec.Body.String(), "ok") {
					t.Fatalf("HTTP %d: %s", rec.Code, rec.Body.String())
				}
			}
			if oauthCalls.Load() != 1 || tokenCalls.Load() != 2 {
				t.Fatalf("attempts OAuth=%d Token=%d; want 1/2 with subscription first and sticky fallback", oauthCalls.Load(), tokenCalls.Load())
			}
			assignment, ok := sessions.Get("qwen", "sticky")
			if !ok || assignment.AccountID != rows[1].ID {
				t.Fatalf("assignment=%+v, found=%v", assignment, ok)
			}
			scheduler := s.SchedulerRef.Get()
			if (failure == 401 || failure == 429) && !scheduler.Exhausted(accounts.ProviderQwenOAuth, rows[0].ID) {
				t.Fatal("failure was not charged to subscription backend")
			}
			if scheduler.Exhausted(accounts.ProviderQwenToken, rows[1].ID) {
				t.Fatal("healthy plan inherited subscription exhaustion")
			}
		})
	}
}

func TestQwenUnifiedPinAndExplicitRoutes(t *testing.T) {
	var oauthCalls, tokenCalls atomic.Int32
	oauth := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { oauthCalls.Add(1); w.WriteHeader(429) }))
	defer oauth.Close()
	token := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tokenCalls.Add(1)
		io.WriteString(w, `{"choices":[{"message":{"content":"ok"}}]}`)
	}))
	defer token.Close()
	rows := []accounts.Account{{ID: "qwen-subscription:member", Provider: accounts.ProviderQwenOAuth, AuthMode: accounts.AuthModeOAuth, Token: "oauth-key"}, {ID: "qwen-token:plan", Provider: accounts.ProviderQwenToken, AuthMode: accounts.AuthModeAPIKey, Token: "plan-key"}}
	sessions, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	s := Server{Accounts: rows, Sessions: sessions, QwenOAuthUpstream: mustParseURL(t, oauth.URL+"/v1"), QwenTokenUpstream: mustParseURL(t, token.URL+"/v1"), MaxBodyBytes: 4096}
	for _, tt := range []struct {
		path, pin string
		want      int
	}{{"/qwen/v1/chat/completions", rows[0].ID, 429}, {"/qwen-oauth/v1/chat/completions", "", 429}, {"/qwen-token/v1/chat/completions", "", 200}} {
		req := httptest.NewRequest("POST", tt.path, strings.NewReader(`{"model":"qwen3.8-max","messages":[]}`))
		if tt.pin != "" {
			req.Header.Set("X-Subrouter-Account-ID", tt.pin)
		}
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		if rec.Code != tt.want {
			t.Fatalf("%s pinned %q: HTTP %d, want %d", tt.path, tt.pin, rec.Code, tt.want)
		}
	}
	if tokenCalls.Load() != 1 {
		t.Fatal("pinned or explicit subscription request leaked to Token Plan")
	}
}

func TestQwenUnifiedStreamAndUsageAttribution(t *testing.T) {
	var oauthCalls, tokenCalls atomic.Int32
	oauth := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		oauthCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(429)
		io.WriteString(w, `{"error":{"code":"insufficient_quota"}}`)
	}))
	defer oauth.Close()
	token := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tokenCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":null}]}\n\ndata: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":12,\"completion_tokens\":3}}\n\ndata: [DONE]\n\n")
	}))
	defer token.Close()
	sessions, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	ledger := NewTokenUsageRecorder("", nil)
	s := Server{Accounts: []accounts.Account{{ID: "qwen-subscription:member", Provider: accounts.ProviderQwenOAuth, AuthMode: accounts.AuthModeOAuth, Token: "oauth-key"}, {ID: "qwen-token:plan", Provider: accounts.ProviderQwenToken, AuthMode: accounts.AuthModeAPIKey, Token: "plan-key"}}, Sessions: sessions, TokenUsage: ledger, SchedulerRef: selectacct.NewSchedulerRef(selectacct.NewScheduler(nil)), QwenOAuthUpstream: mustParseURL(t, oauth.URL+"/v1"), QwenTokenUpstream: mustParseURL(t, token.URL+"/v1"), MaxBodyBytes: 4096}
	req := httptest.NewRequest("POST", "/qwen/v1/chat/completions", strings.NewReader(`{"model":"qwen3.8-max","stream":true,"messages":[]}`))
	req.Header.Set("X-Subrouter-Session", "stream")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "[DONE]") || strings.Contains(rec.Body.String(), "insufficient_quota") {
		t.Fatalf("bad stream: HTTP %d %s", rec.Code, rec.Body.String())
	}
	rows, err := ledger.Rows(time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	var outputs int64
	for _, row := range rows {
		if row.OutputTokens > 0 {
			if row.Provider != "qwen-token" || row.AccountID != "qwen-token:plan" {
				t.Fatalf("usage charged to wrong backend: %+v", row)
			}
			outputs += row.OutputTokens
		}
	}
	if outputs != 3 || oauthCalls.Load() != 1 || tokenCalls.Load() != 1 {
		t.Fatalf("output=%d calls=%d/%d", outputs, oauthCalls.Load(), tokenCalls.Load())
	}
}

func TestQwenUnifiedDoesNotReplayClientErrorsOrPartialStreams(t *testing.T) {
	for _, tt := range []struct {
		name              string
		status            int
		contentType, body string
	}{
		{"parameters", 400, "application/json", `{"error":{"code":"invalid_parameter","message":"invalid temperature"}}`},
		{"context", 400, "application/json", `{"error":{"code":"context_length_exceeded"}}`},
		{"partial stream", 200, "text/event-stream", "data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\ndata: {\"error\":{\"code\":\"insufficient_quota\"}}\n\n"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var fallback atomic.Int32
			oauth := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", tt.contentType)
				w.WriteHeader(tt.status)
				io.WriteString(w, tt.body)
			}))
			defer oauth.Close()
			token := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fallback.Add(1); w.WriteHeader(204) }))
			defer token.Close()
			sessions, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
			if err != nil {
				t.Fatal(err)
			}
			s := Server{Accounts: []accounts.Account{{ID: "qwen-subscription:member", Provider: accounts.ProviderQwenOAuth, AuthMode: accounts.AuthModeOAuth, Token: "oauth-key"}, {ID: "qwen-token:plan", Provider: accounts.ProviderQwenToken, AuthMode: accounts.AuthModeAPIKey, Token: "plan-key"}}, Sessions: sessions, QwenOAuthUpstream: mustParseURL(t, oauth.URL+"/v1"), QwenTokenUpstream: mustParseURL(t, token.URL+"/v1"), MaxBodyBytes: 4096}
			rec := httptest.NewRecorder()
			s.Handler().ServeHTTP(rec, httptest.NewRequest("POST", "/qwen/v1/chat/completions", strings.NewReader(`{"model":"qwen3.8-max","messages":[]}`)))
			if rec.Code != tt.status || rec.Body.String() != tt.body || fallback.Load() != 0 {
				t.Fatalf("client response changed or replayed: HTTP %d, fallback=%d, body=%s", rec.Code, fallback.Load(), rec.Body.String())
			}
		})
	}
}

func TestQwenUnifiedLeaseBindsActualBackend(t *testing.T) {
	for _, provider := range []accounts.Provider{accounts.ProviderQwenOAuth, accounts.ProviderQwenToken} {
		t.Run(string(provider), func(t *testing.T) {
			var calls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer selected-key" {
					t.Errorf("lease used wrong target %s", r.URL.Path)
				}
				io.WriteString(w, `{"choices":[{"message":{"content":"ok"}}]}`)
			}))
			defer upstream.Close()
			mode := accounts.AuthModeAPIKey
			id := "qwen-token:plan"
			if provider == accounts.ProviderQwenOAuth {
				mode = accounts.AuthModeOAuth
				id = "qwen-subscription:member"
			}
			s := Server{Accounts: []accounts.Account{{ID: id, Provider: provider, AuthMode: mode, Token: "selected-key"}}, Sessions: newSessionStore(t), Scheduler: selectacct.NewScheduler(nil), sessionLeases: newSessionLeaseStore(), AdminToken: "service-admin-token", QwenOAuthUpstream: mustParseURL(t, upstream.URL+"/v1"), QwenTokenUpstream: mustParseURL(t, upstream.URL+"/v1"), MaxBodyBytes: 4096}
			handler := s.Handler()
			lease, token := issueSessionLease(t, handler, "qwen", "qwen3.8-max")
			if lease.Assignment.Provider != "qwen" || lease.Assignment.AccountID != id {
				t.Fatalf("wrong lease assignment: %+v", lease.Assignment)
			}
			req := httptest.NewRequest("POST", "/qwen/chat/completions", strings.NewReader(`{"model":"qwen3.8-max","messages":[]}`))
			req.Header.Set("Authorization", "Bearer "+token)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != 200 || calls.Load() != 1 {
				t.Fatalf("bound lease HTTP=%d calls=%d body=%s", rec.Code, calls.Load(), rec.Body.String())
			}
		})
	}
}
