package proxy

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/selectacct"
	"github.com/manaflow-ai/subrouter/session"
)

func TestClaudeBillingFailureSkipsAccountAcrossSessions(t *testing.T) {
	for _, message := range []string{"Your credit balance is too low to access the Anthropic API.", "Credit balance too low", "Insufficient credits"} {
		t.Run(message, func(t *testing.T) {
			var badCalls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.Header.Get("X-Api-Key") == "empty-key" {
					badCalls.Add(1)
					w.WriteHeader(http.StatusBadRequest)
					fmt.Fprintf(w, `{"type":"error","error":{"type":"invalid_request_error","message":%q}}`, message)
					return
				}
				io.WriteString(w, `{"type":"message","content":[{"type":"text","text":"ok"}]}`)
			}))
			defer upstream.Close()
			u, _ := url.Parse(upstream.URL)
			store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
			if err != nil {
				t.Fatal(err)
			}
			for _, id := range []string{"stuck", "other-stuck"} {
				if _, err := store.Put("claude", id, "empty", ""); err != nil {
					t.Fatal(err)
				}
			}
			ref := selectacct.NewSchedulerRef(selectacct.NewScheduler(nil))
			proxy := httptest.NewServer(Server{
				ClaudeUpstream: u, Sessions: store, SchedulerRef: ref, MaxBodyBytes: 4096,
				Accounts: []accounts.Account{
					{ID: "empty", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeAPIKey, Token: "empty-key"},
					{ID: "funded", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeAPIKey, Token: "funded-key"},
				},
			}.Handler())
			defer proxy.Close()
			for _, id := range []string{"stuck", "other-stuck", "new"} {
				if id == "other-stuck" {
					ref.Set(selectacct.NewScheduler(nil))
				}
				request, _ := http.NewRequest(http.MethodPost, proxy.URL+"/v1/messages", strings.NewReader(`{"model":"claude-sonnet-4","max_tokens":1,"messages":[{"role":"user","content":"ping"}]}`))
				request.Header.Set("Content-Type", "application/json")
				request.Header.Set("X-Subrouter-Agent", "claude")
				request.Header.Set("X-Subrouter-Session", id)
				response, err := proxy.Client().Do(request)
				if err != nil {
					t.Fatal(err)
				}
				body, _ := io.ReadAll(response.Body)
				response.Body.Close()
				if response.StatusCode != 200 {
					t.Fatalf("session %s: status %d: %s", id, response.StatusCode, body)
				}
				assignment, _ := store.Get("claude", id)
				if assignment.AccountID != "funded" {
					t.Fatalf("session %s remained on %s", id, assignment.AccountID)
				}
			}
			if got := badCalls.Load(); got != 1 {
				t.Fatalf("empty account calls=%d, want 1", got)
			}
		})
	}
}

func TestClaudePaidStickyMovesToSubscription(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Api-Key") != "" {
			http.Error(w, "paid key selected", 400)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()
	u, _ := url.Parse(upstream.URL)
	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put("claude", "paid-sticky", "paid", ""); err != nil {
		t.Fatal(err)
	}
	proxy := httptest.NewServer(Server{ClaudeUpstream: u, Sessions: store, MaxBodyBytes: 4096, Scheduler: selectacct.NewScheduler(nil), Accounts: []accounts.Account{
		{ID: "paid", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeAPIKey, Token: "key"},
		{ID: "subscription", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "subscription-token"},
	}}.Handler())
	defer proxy.Close()
	req, _ := http.NewRequest(http.MethodPost, proxy.URL+"/v1/messages", strings.NewReader(`{"model":"claude-sonnet-4","max_tokens":1,"messages":[{"role":"user","content":"ping"}]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Subrouter-Agent", "claude")
	req.Header.Set("X-Subrouter-Session", "paid-sticky")
	resp, err := proxy.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 204 {
		t.Fatalf("status=%d, want subscription success", resp.StatusCode)
	}
}
