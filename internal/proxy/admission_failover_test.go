package proxy

import (
	"context"
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

// TestAdmissionRejectionFailsOverToAnotherAccount drives Server.Handler()
// with a session pinned to an account the admission limiter turns away: once
// because the account is paused after a rate limit (a Claude 429 pauses it
// until the quota reset), once because its queue is full. Before, the first
// case held the request until the client gave up and the second answered 503
// on every request while other accounts had room (2026-10-14). Both must now
// be served by the free account, and the rejection must reach the error log.
// Only the pause moves the session: a full queue means the account is busy,
// not out of quota, so the session keeps the account holding its prompt cache.
func TestAdmissionRejectionFailsOverToAnotherAccount(t *testing.T) {
	for _, tc := range []struct {
		name        string
		prepare     func(*admissionControl)
		wantClass   string
		wantSession string
	}{
		{
			name: "paused after a rate limit",
			prepare: func(control *admissionControl) {
				for _, model := range []string{"", "default", "claude-opus", "claude-sonnet", "claude-fable"} {
					control.pause(admissionKeyFor(accounts.ProviderClaude, "pinned@example.com", model), time.Now().Add(time.Hour))
				}
			},
			wantClass:   "admission_paused",
			wantSession: "free@example.com",
		},
		{
			name: "queue full",
			prepare: func(control *admissionControl) {
				control.limit, control.queueCap = 1, 0
				for _, model := range []string{"", "default", "claude-opus", "claude-sonnet", "claude-fable"} {
					// Names that normalize to one key find its only slot held.
					if _, err := control.acquire(context.Background(), admissionKeyFor(accounts.ProviderClaude, "pinned@example.com", model)); err != nil && !admissionRejected(err) {
						t.Fatal(err)
					}
				}
			},
			wantClass:   "admission_queue_full",
			wantSession: "pinned@example.com",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
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
			store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.Put("claude", "session-pinned", "pinned@example.com", ""); err != nil {
				t.Fatal(err)
			}
			control := testAdmissionControl(defaultAdmissionLimit, defaultAdmissionQueue)
			tc.prepare(control)
			events := NewErrorEventLog("", nil)
			handler := Server{
				ClaudeUpstream: upstreamURL,
				Accounts: []accounts.Account{
					{ID: "pinned@example.com", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "tok-pinned"},
					{ID: "free@example.com", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "tok-free"},
				},
				Sessions: store,
				SchedulerRef: selectacct.NewSchedulerRef(selectacct.NewScheduler([]selectacct.Score{
					{AccountID: "pinned@example.com", Provider: accounts.ProviderClaude, Headroom: 1, ShortHeadroom: 1},
					{AccountID: "free@example.com", Provider: accounts.ProviderClaude, Headroom: 1, ShortHeadroom: 1},
				})),
				MaxBodyBytes: 1 << 20,
				ErrorEvents:  events,
				admission:    control,
			}.Handler()
			subrouter := httptest.NewServer(handler)
			defer subrouter.Close()

			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, subrouter.URL+"/v1/messages", strings.NewReader(`{"model":"claude-opus-5-5","messages":[]}`))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-Subrouter-Agent", "claude")
			req.Header.Set("X-Subrouter-Session", "session-pinned")
			response, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("request did not complete: %v", err)
			}
			body, _ := io.ReadAll(response.Body)
			response.Body.Close()
			if response.StatusCode != http.StatusOK {
				t.Fatalf("status = %d body=%.200s, want 200 from the free account", response.StatusCode, body)
			}
			mu.Lock()
			if hits["pinned"] != 0 || hits["free"] != 1 {
				t.Fatalf("upstream hits = %v, want only the free account", hits)
			}
			mu.Unlock()
			report := events.Query(ErrorEventQuery{Provider: "claude"})
			if len(report.Events) == 0 || report.Events[0].ErrorClass != tc.wantClass || report.Events[0].Account != "pinned@example.com" {
				t.Fatalf("error log = %+v, want a %s event on pinned@example.com", report.Events, tc.wantClass)
			}
			if assignment, ok := store.Get("claude", "session-pinned"); !ok || assignment.AccountID != tc.wantSession {
				t.Fatalf("session assignment = %+v (found %v), want it on %s", assignment, ok, tc.wantSession)
			}
		})
	}
}

// TestQueueFullAfterPausedFailoverStillMovesSession drives Server.Handler()
// with a session pinned to a paused account. The failover lands on an account
// whose queue is full and then on a third account, which serves the request.
// The pause justified moving the session, so it must move to the account that
// served, not stay on the paused one because the queue-full hop overflowed
// only the request.
func TestQueueFullAfterPausedFailoverStillMovesSession(t *testing.T) {
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
	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put("claude", "session-pinned", "pinned@example.com", ""); err != nil {
		t.Fatal(err)
	}
	control := testAdmissionControl(1, 0)
	models := []string{"", "default", "claude-opus", "claude-sonnet", "claude-fable"}
	for _, model := range models {
		control.pause(admissionKeyFor(accounts.ProviderClaude, "pinned@example.com", model), time.Now().Add(time.Hour))
		// Names that normalize to one key find its only slot held.
		if _, err := control.acquire(context.Background(), admissionKeyFor(accounts.ProviderClaude, "busy@example.com", model)); err != nil && !admissionRejected(err) {
			t.Fatal(err)
		}
	}
	handler := Server{
		ClaudeUpstream: upstreamURL,
		Accounts: []accounts.Account{
			{ID: "pinned@example.com", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "tok-pinned"},
			{ID: "busy@example.com", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "tok-busy"},
			{ID: "last@example.com", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "tok-last"},
		},
		Sessions: store,
		// last sits under the new-session floor, so failover tries busy
		// first and reaches last only after busy's queue turns it away.
		SchedulerRef: selectacct.NewSchedulerRef(selectacct.NewScheduler([]selectacct.Score{
			{AccountID: "pinned@example.com", Provider: accounts.ProviderClaude, Headroom: 1, ShortHeadroom: 1},
			{AccountID: "busy@example.com", Provider: accounts.ProviderClaude, Headroom: 1, ShortHeadroom: 1},
			{AccountID: "last@example.com", Provider: accounts.ProviderClaude, Headroom: 0.2, ShortHeadroom: 0.2},
		})),
		MaxBodyBytes: 1 << 20,
		admission:    control,
	}.Handler()
	subrouter := httptest.NewServer(handler)
	defer subrouter.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, subrouter.URL+"/v1/messages", strings.NewReader(`{"model":"claude-opus-5-5","messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Subrouter-Agent", "claude")
	req.Header.Set("X-Subrouter-Session", "session-pinned")
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request did not complete: %v", err)
	}
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d body=%.200s, want 200 from the last account", response.StatusCode, body)
	}
	mu.Lock()
	if hits["last"] != 1 || hits["pinned"] != 0 || hits["busy"] != 0 {
		t.Fatalf("upstream hits = %v, want only the last account", hits)
	}
	mu.Unlock()
	if assignment, ok := store.Get("claude", "session-pinned"); !ok || assignment.AccountID != "last@example.com" {
		t.Fatalf("session assignment = %+v (found %v), want it moved off the paused account to last@example.com", assignment, ok)
	}
}
