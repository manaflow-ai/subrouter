package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/session"
)

// TestRootHealthProbesAnswerLocally drives Server.Handler() with the health
// probes agents send to the router root. They must get this worker's health
// without reaching the upstream (before, chatgpt.com answered them with a
// Cloudflare 403 page on a pooled account), while other methods on the same
// paths still forward.
func TestRootHealthProbesAnswerLocally(t *testing.T) {
	var upstreamHits atomic.Int32
	codex := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHits.Add(1)
		w.WriteHeader(http.StatusTeapot)
	}))
	defer codex.Close()
	codexURL, err := url.Parse(codex.URL)
	if err != nil {
		t.Fatal(err)
	}
	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	subrouter := httptest.NewServer(Server{
		CodexUpstream: codexURL,
		Sessions:      store,
		Accounts: []accounts.Account{
			{ID: "codex@example.com", Provider: accounts.ProviderCodex, AuthMode: accounts.AuthModeOAuth, Token: "tok-codex"},
		},
		MaxBodyBytes: 1 << 20,
	}.Handler())
	defer subrouter.Close()

	for _, probe := range []struct{ method, path string }{
		{http.MethodGet, "/"}, {http.MethodHead, "/"}, {http.MethodGet, "/health"},
		{http.MethodGet, "/healthz"}, {http.MethodGet, "/v1/health"}, {http.MethodGet, "/status"},
	} {
		req, err := http.NewRequest(probe.method, subrouter.URL+probe.path, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("User-Agent", "curl/8.20.0")
		response, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(response.Body)
		response.Body.Close()
		want := http.StatusOK
		if probe.method == http.MethodHead && probe.path == "/" {
			// The base-URL probe keeps its existing answer.
			want = http.StatusNoContent
		}
		if response.StatusCode != want {
			t.Fatalf("%s %s = %d %.200s, want %d", probe.method, probe.path, response.StatusCode, body, want)
		}
		if probe.method == http.MethodGet {
			var health struct {
				OK bool `json:"ok"`
			}
			if err := json.Unmarshal(body, &health); err != nil || !health.OK {
				t.Fatalf("%s %s body = %.200s, want health JSON with ok=true", probe.method, probe.path, body)
			}
		}
	}
	if hits := upstreamHits.Load(); hits != 0 {
		t.Fatalf("health probes reached the upstream %d times, want 0", hits)
	}

	response, err := http.Post(subrouter.URL+"/v1/health", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if upstreamHits.Load() != 1 || response.StatusCode != http.StatusTeapot {
		t.Fatalf("POST /v1/health = %d with %d upstream hits, want it forwarded once", response.StatusCode, upstreamHits.Load())
	}
}

// TestRootHealthProbesStayBehindProxyAuth keeps the probe answer behind the
// local proxy token and out of session-lease-only mode: a reorder of the
// checks in proxyHandler must not turn these paths into an open endpoint.
func TestRootHealthProbesStayBehindProxyAuth(t *testing.T) {
	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		server Server
	}{
		{"local proxy token", Server{Sessions: store, LocalProxyToken: "proxy-secret", MaxBodyBytes: 1 << 20}},
		{"session lease required", Server{Sessions: store, RequireSessionLease: true, MaxBodyBytes: 1 << 20}},
	} {
		subrouter := httptest.NewServer(tc.server.Handler())
		response, err := http.Get(subrouter.URL + "/healthz")
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(response.Body)
		response.Body.Close()
		subrouter.Close()
		if response.StatusCode == http.StatusOK {
			t.Fatalf("%s: GET /healthz without credentials = 200 %.200s, want it refused", tc.name, body)
		}
	}
}
