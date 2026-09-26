package proxy

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

// catalogUpstream serves `pages` pages of `perPage` entries under the given
// path prefix, and records the paths it was asked for.
func catalogUpstream(t *testing.T, prefix string, pages, perPage int) (*httptest.Server, *[]string) {
	t.Helper()
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.URL.Path)
		if r.URL.Path != prefix+"/ps/plugins/list" {
			http.NotFound(w, r)
			return
		}
		page := 1
		if tok := r.URL.Query().Get("pageToken"); tok != "" {
			fmt.Sscanf(tok, "page-%d", &page)
		}
		plugins := make([]map[string]string, 0, perPage)
		for i := 0; i < perPage; i++ {
			plugins = append(plugins, map[string]string{"id": fmt.Sprintf("p%d-%d", page, i)})
		}
		body := map[string]any{"plugins": plugins, "pagination": map[string]any{"limit": perPage}}
		if page < pages {
			body["pagination"] = map[string]any{
				"limit":           perPage,
				"next_page_token": fmt.Sprintf("page-%d", page+1),
			}
		}
		_ = json.NewEncoder(w).Encode(body)
	}))
	t.Cleanup(srv.Close)
	return srv, &seen
}

func firstPage(t *testing.T, srv *httptest.Server, prefix string) []byte {
	t.Helper()
	resp, err := srv.Client().Get(srv.URL + prefix + "/ps/plugins/list?scope=GLOBAL&limit=200")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	buf, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return buf
}

func requestFor(t *testing.T, srv *httptest.Server, prefix string) (*http.Request, *url.URL) {
	t.Helper()
	// The reverse proxy hands us a request whose path has already been mapped
	// for the upstream; the upstream URL carries its own prefix.
	req := httptest.NewRequest(http.MethodGet, "/ps/plugins/list?scope=GLOBAL&limit=200", nil)
	up, err := url.Parse(srv.URL + prefix)
	if err != nil {
		t.Fatal(err)
	}
	return req, up
}

func pluginIDs(t *testing.T, body []byte) []string {
	t.Helper()
	var decoded struct {
		Plugins    []map[string]string `json:"plugins"`
		Pagination map[string]any      `json:"pagination"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("decode merged body: %v", err)
	}
	if _, ok := decoded.Pagination["next_page_token"]; ok {
		t.Fatal("merged body still carries a continuation token")
	}
	ids := make([]string, 0, len(decoded.Plugins))
	for _, p := range decoded.Plugins {
		ids = append(ids, p["id"])
	}
	return ids
}

// The whole point: the client gets every entry in one page and never paginates.
func TestAggregatesEveryCatalogPage(t *testing.T) {
	srv, _ := catalogUpstream(t, "/backend-api", 5, 10)
	req, up := requestFor(t, srv, "/backend-api")

	merged, pages, entries, ok, err := aggregateCatalogPages(srv.Client().Transport, req, up, firstPage(t, srv, "/backend-api"))
	if err != nil || !ok {
		t.Fatalf("aggregation did not run: ok=%v err=%v", ok, err)
	}
	if pages != 5 || entries != 50 {
		t.Fatalf("pages=%d entries=%d, want 5 and 50", pages, entries)
	}
	if got := pluginIDs(t, merged); len(got) != 50 || got[0] != "p1-0" || got[49] != "p5-9" {
		t.Fatalf("merged %d entries, first=%q last=%q", len(got), got[0], got[len(got)-1])
	}
}

// Regression: the upstream URL carries a path prefix that the hand-issued
// requests must reproduce, or every page after the first 404s and the client
// silently receives only page one.
func TestAggregatePreservesUpstreamPathPrefix(t *testing.T) {
	srv, seen := catalogUpstream(t, "/backend-api", 3, 4)
	req, up := requestFor(t, srv, "/backend-api")

	_, pages, entries, ok, err := aggregateCatalogPages(srv.Client().Transport, req, up, firstPage(t, srv, "/backend-api"))
	if err != nil || !ok || pages != 3 || entries != 12 {
		t.Fatalf("ok=%v err=%v pages=%d entries=%d, want 3 pages and 12 entries", ok, err, pages, entries)
	}
	for _, path := range *seen {
		if path != "/backend-api/ps/plugins/list" {
			t.Fatalf("requested %q, want the prefixed path", path)
		}
	}
}

// A catalog bigger than the walk's bounds must error, not be silently
// truncated: a merged body carries no continuation token, so a client cannot
// tell a capped walk from a complete catalog and would cache the partial one.
func TestAggregateErrorsAtPageCap(t *testing.T) {
	srv, _ := catalogUpstream(t, "", catalogAggregateMaxPages+20, 2)
	req, up := requestFor(t, srv, "")

	_, pages, _, ok, err := aggregateCatalogPages(srv.Client().Transport, req, up, firstPage(t, srv, ""))
	if err == nil || ok {
		t.Fatalf("capped walk returned ok=%v err=%v, want an error", ok, err)
	}
	if pages != catalogAggregateMaxPages {
		t.Fatalf("walked %d pages, want the %d cap", pages, catalogAggregateMaxPages)
	}
}

// A mid-walk upstream failure must be an error, never the pages collected so
// far: codex pins whatever body it receives in a 3-hour disk cache, and a
// partial catalog is indistinguishable from a complete one. Observed in the
// wild before this rule: clients held 173- and 209-entry catalogs (of 2,285)
// for three hours after upstream throttled a walk.
func TestAggregateErrorsWhenUpstreamFailsMidWalk(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls > 2 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"plugins":    []map[string]string{{"id": fmt.Sprintf("p%d", calls)}},
			"pagination": map[string]any{"next_page_token": "more"},
		})
	}))
	defer srv.Close()
	req, up := requestFor(t, srv, "")

	_, _, _, ok, err := aggregateCatalogPages(srv.Client().Transport, req, up, firstPage(t, srv, ""))
	if err == nil || ok {
		t.Fatalf("mid-walk failure returned ok=%v err=%v, want an error", ok, err)
	}
}

func TestAggregateIgnoresUnrelatedResponses(t *testing.T) {
	srv, _ := catalogUpstream(t, "", 2, 1)
	up, _ := url.Parse(srv.URL)

	cases := []struct {
		name string
		path string
		body string
	}{
		{"non-catalog path", "/codex/responses", `{"plugins":[],"pagination":{"next_page_token":"x"}}`},
		{"no continuation", "/ps/plugins/list", `{"plugins":[{"id":"a"}],"pagination":{"limit":200}}`},
		{"not json", "/ps/plugins/list", `nonsense`},
		{"no plugins field", "/ps/plugins/list", `{"pagination":{"next_page_token":"x"}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tc.path, nil)
			out, _, _, ok, err := aggregateCatalogPages(srv.Client().Transport, req, up, []byte(tc.body))
			if ok || err != nil {
				t.Fatalf("unexpectedly aggregated: ok=%v err=%v", ok, err)
			}
			if string(out) != tc.body {
				t.Fatalf("body was modified: %s", out)
			}
		})
	}
}

// Only the plugin polling endpoints are buffered and coalesced; inference
// traffic must stream through untouched.
func TestCoalescablePathPatterns(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		{"/backend-api/ps/plugins/list", true},
		{"/backend-api/ps/plugins/installed", true},
		{"/backend-api/plugins/installed", true},
		{"/backend-api/plugins/featured", true},
		{"/backend-api/codex/responses", false},
		{"/v1/messages", false},
		{"/backend-api/conversation", false},
		{"/v1/responses", false},
	}
	for _, tc := range cases {
		if got := coalescablePath(tc.path); got != tc.want {
			t.Errorf("coalescablePath(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}
}
