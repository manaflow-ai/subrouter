package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
)

func trafficSnapshotFrom(t *testing.T, handler http.Handler) TrafficSnapshot {
	t.Helper()
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/_subrouter/traffic", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("traffic status = %d: %s", recorder.Code, recorder.Body.String())
	}
	var snapshot TrafficSnapshot
	if err := json.Unmarshal(recorder.Body.Bytes(), &snapshot); err != nil {
		t.Fatalf("decode traffic: %v (%s)", err, recorder.Body.String())
	}
	return snapshot
}

func TestTrafficCountsClassifyUpstreamAndProxyFailures(t *testing.T) {
	var status int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer upstream.Close()

	server := opencodeProviderTestServer(t, accounts.Account{
		ID:       "kimi:main",
		Provider: accounts.ProviderKimi,
		AuthMode: accounts.AuthModeAPIKey,
		Token:    "kimi-token",
	}, accounts.ProviderKimi, upstream.URL+"/coding/v1")
	server.Traffic = NewTrafficStats(time.Now().Add(-time.Minute))
	server.StreamDrops = &StreamDropStats{}
	server.StreamDrops.Observe("proxy", time.Now())
	handler := server.Handler()

	send := func() int {
		t.Helper()
		request := httptest.NewRequest(http.MethodGet, "/kimi/models", nil)
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		return recorder.Code
	}
	for _, want := range []int{http.StatusOK, http.StatusBadRequest, http.StatusServiceUnavailable} {
		status = want
		if got := send(); got != want {
			t.Fatalf("status = %d, want upstream %d passed through", got, want)
		}
	}
	// Control traffic is not client traffic.
	for _, path := range []string{"/_subrouter/health", "/_subrouter/stream-stats"} {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodGet, path, nil)
		request.RemoteAddr = "127.0.0.1:4321"
		handler.ServeHTTP(recorder, request)
	}
	// A dead upstream is subrouter's own 502.
	upstream.Close()
	if got := send(); got < 500 {
		t.Fatalf("dead upstream status = %d, want a 5xx", got)
	}

	got := trafficSnapshotFrom(t, handler)
	if got.Requests != 4 {
		t.Fatalf("requests = %d, want 4 (control paths excluded): %+v", got.Requests, got)
	}
	if got.Responses.Class2xx != 1 || got.Responses.Class4xx != 1 || got.Responses.Class5xx != 2 {
		t.Fatalf("responses = %+v, want 1/1/2", got.Responses)
	}
	if got.Upstream5xx != 1 || got.Proxy5xx != 1 {
		t.Fatalf("upstream_5xx = %d proxy_5xx = %d, want 1 and 1", got.Upstream5xx, got.Proxy5xx)
	}
	if got.Streams.Proxy != 1 {
		t.Fatalf("stream proxy drops = %d, want 1", got.Streams.Proxy)
	}
	if got.StartedAt == "" || got.UptimeSeconds < 59 || got.Version == "" {
		t.Fatalf("started_at/uptime/version missing: %+v", got)
	}
}

func TestTrafficCountsHandlerGeneratedFailuresAsProxy(t *testing.T) {
	stats := NewTrafficStats(time.Now())
	handler := stats.trafficCounted(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/no-account":
			http.Error(w, "no usable account", http.StatusServiceUnavailable)
		case "/upstream":
			markUpstreamResponse(r.Context(), true)
			w.WriteHeader(http.StatusBadGateway)
		case "/implicit":
			_, _ = w.Write([]byte("ok"))
		case "/stream":
			w.(http.Flusher).Flush()
		}
	}))
	for _, path := range []string{"/no-account", "/upstream", "/implicit", "/stream", "/_subrouter/health"} {
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, path, nil))
	}
	got := stats.Snapshot(nil, time.Now())
	if got.Requests != 4 || got.Responses.Class2xx != 2 || got.Proxy5xx != 1 || got.Upstream5xx != 1 {
		t.Fatalf("snapshot = %+v", got)
	}
}

func TestTrafficCountsAPanickingHandlerAsProxy5xx(t *testing.T) {
	stats := NewTrafficStats(time.Now())
	handler := stats.trafficCounted(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic(http.ErrAbortHandler)
	}))
	func() {
		defer func() { _ = recover() }()
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/x", nil))
	}()
	if got := stats.Snapshot(nil, time.Now()); got.Proxy5xx != 1 {
		t.Fatalf("proxy_5xx = %d, want 1", got.Proxy5xx)
	}
}

func TestTrafficNilStatsIsSafe(t *testing.T) {
	var stats *TrafficStats
	next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	if handler := stats.trafficCounted(next); handler == nil {
		t.Fatal("nil stats must pass the handler through")
	}
	if got := stats.Snapshot(nil, time.Now()); got.Requests != 0 {
		t.Fatalf("nil snapshot = %+v", got)
	}
}

func TestHealthReportsReleaseStateOnlyWhenConfigured(t *testing.T) {
	path := filepath.Join(t.TempDir(), "release-state.json")
	state := `{"version":"v0.1.150","previous_version":"v0.1.149","state":"baking","since":"2026-09-26T10:00:00Z","bake_until":"2026-09-26T10:20:00Z","baseline":{"requests":10}}`
	if err := os.WriteFile(path, []byte(state), 0o644); err != nil {
		t.Fatal(err)
	}
	health := func(server Server) map[string]json.RawMessage {
		t.Helper()
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodGet, "/_subrouter/health", nil)
		request.RemoteAddr = "127.0.0.1:4321"
		server.handleHealth(recorder, request)
		var body map[string]json.RawMessage
		if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode health: %v", err)
		}
		return body
	}
	if _, ok := health(Server{})["release"]; ok {
		t.Fatal("health reported release without a configured path")
	}
	body := health(Server{ReleaseStatePath: path})
	var release ReleaseState
	if err := json.Unmarshal(body["release"], &release); err != nil {
		t.Fatalf("decode release: %v (%s)", err, body["release"])
	}
	if release.Version != "v0.1.150" || release.State != "baking" || release.BakeUntil == "" || release.PreviousVersion != "v0.1.149" {
		t.Fatalf("release = %+v", release)
	}
	if strings.Contains(string(body["release"]), "baseline") {
		t.Fatalf("release leaks the bake baseline: %s", body["release"])
	}
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, ok := health(Server{ReleaseStatePath: path})["release"]; ok {
		t.Fatal("a malformed release file must be omitted, not reported")
	}
	if _, ok := health(Server{ReleaseStatePath: path + ".missing"})["release"]; ok {
		t.Fatal("a missing release file must be omitted")
	}
}
