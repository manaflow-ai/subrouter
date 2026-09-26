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

// New model families and metadata must traverse the existing protocol without
// a router release, a restart, or a matching entry in a local registry.
func TestClaudeDiscoveryForwardsFutureModelsAndUpstreamFailures(t *testing.T) {
	var catalog atomic.Value
	catalog.Store(`{"data":[],"has_more":false}`)
	var status atomic.Int32
	status.Store(http.StatusOK)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer upstream-token" {
			t.Error("wrong upstream credential")
		}
		if r.Header.Get("Anthropic-Version") != "2023-06-01" {
			t.Error("missing API version")
		}
		switch r.URL.Path {
		case "/v1/models":
			if r.Method != http.MethodGet || r.URL.RawQuery != "limit=1000&after_id=cursor" {
				t.Errorf("discovery request changed: %s %s", r.Method, r.URL.RequestURI())
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(int(status.Load()))
			fmt.Fprint(w, catalog.Load().(string))
		case "/v1/messages":
			body, _ := io.ReadAll(r.Body)
			if !strings.Contains(string(body), `"model":"claude-future-family-99"`) {
				t.Error("future model was rewritten")
			}
			fmt.Fprint(w, `{"model":"claude-future-family-99","content":[]}`)
		default:
			t.Errorf("wrong upstream path %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer upstream.Close()
	u, _ := url.Parse(upstream.URL)
	sessions, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	handler := Server{
		ClaudeUpstream: u, Accounts: []accounts.Account{{ID: "claude-test", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "upstream-token"}},
		Sessions: sessions, Scheduler: selectacct.NewScheduler(nil), MaxBodyBytes: 1 << 20,
	}.Handler()
	for _, body := range []string{
		`{"data":[],"has_more":false}`,
		`{"data":[{"id":"claude-future-family-99","display_name":"Future family 99","capabilities":{"future_feature":true}}],"has_more":true,"last_id":"cursor2"}`,
	} {
		catalog.Store(body)
		req := httptest.NewRequest(http.MethodGet, "/v1/models?limit=1000&after_id=cursor", nil)
		req.Header.Set("X-Subrouter-Agent", "claude")
		req.Header.Set("Anthropic-Version", "2023-06-01")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != 200 || rec.Body.String() != body {
			t.Fatalf("catalog = %d %s", rec.Code, rec.Body.String())
		}
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-future-family-99","messages":[]}`))
	req.Header.Set("X-Subrouter-Agent", "claude")
	req.Header.Set("Anthropic-Version", "2023-06-01")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "claude-future-family-99") {
		t.Fatalf("future inference = %d %s", rec.Code, rec.Body.String())
	}
	status.Store(503)
	catalog.Store(`{"type":"error","error":{"type":"api_error","message":"unavailable"}}`)
	req = httptest.NewRequest(http.MethodGet, "/v1/models?limit=1000&after_id=cursor", nil)
	req.Header.Set("X-Subrouter-Agent", "claude")
	req.Header.Set("Anthropic-Version", "2023-06-01")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != 503 || rec.Body.String() != catalog.Load().(string) {
		t.Fatalf("upstream failure replaced with catalog: %d %s", rec.Code, rec.Body.String())
	}
}
