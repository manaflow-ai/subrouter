package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/selectacct"
	"github.com/manaflow-ai/subrouter/session"
)

// These tests pin the plugin-endpoint contract at the handler level, end to
// end through ServeHTTP, because the original incident lived in the gap
// between correct units: the read cache passed its own tests while the
// handler replayed page 1 of the plugin catalog to page-2 requests. Codex
// re-sent the continuation cursor it had just been handed back, forever:
// 3,598 requests in one turn, 5.3 GB streamed, 15 GB client RSS, kernel
// panics. The cache is gone; anyone reintroducing stored responses, or any
// other change that makes two different plugin requests share one body, must
// turn one of these red.

func pluginTestServer(t *testing.T, upstream *httptest.Server) http.Handler {
	t.Helper()
	codexUpstream, err := url.Parse(upstream.URL + "/backend-api/codex")
	if err != nil {
		t.Fatal(err)
	}
	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	return Server{
		CodexUpstream: codexUpstream,
		Accounts: []accounts.Account{{
			ID:       "codex-account",
			AuthMode: accounts.AuthModeOAuth,
			Token:    "oauth-token",
		}},
		Sessions:     store,
		Scheduler:    selectacct.NewScheduler(nil),
		MaxBodyBytes: 1 << 20,
	}.Handler()
}

// The incident, replayed: request page 1, then request page 2 with the cursor
// page 1 returned. The page-2 response must be page 2. Any stored-response
// layer keyed on less than the full request serves page 1 again, and the
// client loops on its own cursor.
func TestPluginListPageTwoIsNeverPageOne(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/backend-api/ps/plugins/installed" {
			http.NotFound(w, r)
			return
		}
		// Distinct bodies per query string, echoing the shape that broke:
		// same path, different query.
		fmt.Fprintf(w, `{"scope":%q}`, r.URL.Query().Get("scope"))
	}))
	defer upstream.Close()
	handler := pluginTestServer(t, upstream)

	for _, scope := range []string{"USER", "GLOBAL", "WORKSPACE", "USER"} {
		req := httptest.NewRequest(http.MethodGet, "/backend-api/ps/plugins/installed?scope="+scope, nil)
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("scope=%s status = %d, want 200; body %s", scope, rr.Code, rr.Body.String())
		}
		want := fmt.Sprintf(`{"scope":%q}`, scope)
		if strings.TrimSpace(rr.Body.String()) != want {
			t.Fatalf("scope=%s got body %q, want %q: a response for a different request was replayed", scope, rr.Body.String(), want)
		}
	}
}

// Nothing is stored between requests: two sequential identical GETs both
// reach upstream. This is the "no response cache" contract itself. If this
// fails because someone added a cache to cut upstream load, read the
// request_coalesce.go comment first: the previous cache shipped a
// cross-account data leak and the pagination-loop incident, and coalescing
// plus codex's own 3-hour disk cache already bound the traffic.
func TestSequentialIdenticalPluginRequestsEachReachUpstream(t *testing.T) {
	var hits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = io.WriteString(w, `{"plugins":[]}`)
	}))
	defer upstream.Close()
	handler := pluginTestServer(t, upstream)

	for i := 0; i < 3; i++ {
		req := httptest.NewRequest(http.MethodGet, "/backend-api/ps/plugins/installed?scope=USER", nil)
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("request %d status = %d", i, rr.Code)
		}
		if h := rr.Header().Get("X-Subrouter-Cache"); h != "" {
			t.Fatalf("X-Subrouter-Cache header %q resurfaced: a response cache is back", h)
		}
	}
	if got := hits.Load(); got != 3 {
		t.Fatalf("upstream saw %d requests for 3 sequential client requests, want 3: a stored response was replayed", got)
	}
}

// Identical CONCURRENT requests are the one case that shares an upstream
// fetch, and every waiter gets the complete merged catalog. This pins both
// halves of the coalescing contract: one walk, full results for everyone.
// (Truncated catalogs for waiters was a real failure: clients cached 173- and
// 209-entry catalogs of 2,285 for 3 hours.)
func TestConcurrentCatalogRequestsShareOneCompleteWalk(t *testing.T) {
	var listHits atomic.Int32
	const pages, perPage = 4, 5
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/backend-api/ps/plugins/list" {
			http.NotFound(w, r)
			return
		}
		listHits.Add(1)
		page := 1
		if tok := r.URL.Query().Get("pageToken"); tok != "" {
			fmt.Sscanf(tok, "page-%d", &page)
		}
		plugins := make([]map[string]string, 0, perPage)
		for i := 0; i < perPage; i++ {
			plugins = append(plugins, map[string]string{"id": fmt.Sprintf("p%d-%d", page, i)})
		}
		body := map[string]any{"plugins": plugins, "pagination": map[string]any{}}
		if page < pages {
			body["pagination"] = map[string]any{"next_page_token": fmt.Sprintf("page-%d", page+1)}
		}
		_ = json.NewEncoder(w).Encode(body)
	}))
	defer upstream.Close()
	handler := pluginTestServer(t, upstream)
	proxy := httptest.NewServer(handler)
	defer proxy.Close()

	const clients = 6
	bodies := make([][]byte, clients)
	var wg sync.WaitGroup
	for i := 0; i < clients; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			resp, err := http.Get(proxy.URL + "/backend-api/ps/plugins/list?scope=GLOBAL&limit=5")
			if err != nil {
				t.Errorf("client %d: %v", i, err)
				return
			}
			defer resp.Body.Close()
			bodies[i], _ = io.ReadAll(resp.Body)
		}(i)
	}
	wg.Wait()

	// Exactly one walk: page-1 fetches equal 1 even though 6 clients asked.
	// (Total hits = one initial page + the walk's follow-up pages.)
	if got := listHits.Load(); got != pages {
		t.Fatalf("upstream saw %d list requests for %d concurrent clients, want %d (one shared walk)", got, clients, pages)
	}
	for i, body := range bodies {
		var decoded struct {
			Plugins    []map[string]string `json:"plugins"`
			Pagination map[string]any      `json:"pagination"`
		}
		if err := json.Unmarshal(body, &decoded); err != nil {
			t.Fatalf("client %d body: %v", i, err)
		}
		if len(decoded.Plugins) != pages*perPage {
			t.Fatalf("client %d got %d entries, want %d: a waiter received a partial catalog", i, len(decoded.Plugins), pages*perPage)
		}
		if _, ok := decoded.Pagination["next_page_token"]; ok {
			t.Fatalf("client %d body still carries a continuation token", i)
		}
	}
}

// A walk that fails midway must surface as an error status, never a 2xx with
// a partial body. A merged catalog carries no continuation token, so a
// partial one is indistinguishable from a complete one and codex pins it in a
// 3-hour disk cache.
func TestCatalogWalkFailureIsAnErrorNotAPartialBody(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) > 2 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"plugins":    []map[string]string{{"id": "p"}},
			"pagination": map[string]any{"next_page_token": "more"},
		})
	}))
	defer upstream.Close()
	handler := pluginTestServer(t, upstream)

	req := httptest.NewRequest(http.MethodGet, "/backend-api/ps/plugins/list?scope=GLOBAL&limit=1", nil)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	if rr.Code < 500 {
		t.Fatalf("mid-walk failure returned status %d with body %q, want 5xx: a partial catalog would be cached as complete", rr.Code, rr.Body.String())
	}
}

// The flight's work is shared, so the leader's disconnect must not cancel it:
// the walk runs on a context detached from the initiating request. Cancel the
// leader mid-walk and assert the walk still reaches the final page.
func TestLeaderDisconnectDoesNotCancelSharedWalk(t *testing.T) {
	leaderCtx, cancelLeader := context.WithCancel(context.Background())
	var pagesServed atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page := 1
		if tok := r.URL.Query().Get("pageToken"); tok != "" {
			fmt.Sscanf(tok, "page-%d", &page)
		}
		pagesServed.Add(1)
		if page == 2 {
			// Kill the initiating client while its walk is mid-flight.
			cancelLeader()
			time.Sleep(50 * time.Millisecond)
		}
		body := map[string]any{"plugins": []map[string]string{{"id": fmt.Sprintf("p%d", page)}}, "pagination": map[string]any{}}
		if page < 3 {
			body["pagination"] = map[string]any{"next_page_token": fmt.Sprintf("page-%d", page+1)}
		}
		_ = json.NewEncoder(w).Encode(body)
	}))
	defer upstream.Close()
	handler := pluginTestServer(t, upstream)

	req := httptest.NewRequest(http.MethodGet, "/backend-api/ps/plugins/list?scope=GLOBAL&limit=1", nil).WithContext(leaderCtx)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if got := pagesServed.Load(); got != 3 {
		t.Fatalf("walk served %d pages after the leader disconnected, want all 3: the shared flight died with its leader", got)
	}
}

// Inference traffic must never enter the buffering/coalescing path: streamed
// responses through a body-buffering recorder would break SSE. Pin that the
// gate is the path allowlist.
func TestInferencePathsAreNotCoalesced(t *testing.T) {
	if coalescablePath("/backend-api/codex/responses") || coalescablePath("/v1/responses") || coalescablePath("/v1/messages") {
		t.Fatal("an inference path became coalescable: streaming responses would be buffered")
	}
}
