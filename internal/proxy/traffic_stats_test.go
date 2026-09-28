package proxy

import (
	"bufio"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/manaflow-ai/subrouter/internal/accounts"
)

// unwrapOnlyTrafficWriter models middleware that exposes ResponseController
// unwrapping without implementing http.Hijacker itself.
type unwrapOnlyTrafficWriter struct {
	http.ResponseWriter
}

func (w unwrapOnlyTrafficWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

type failedTrafficHijacker struct {
	http.ResponseWriter
	err error
}

func (w failedTrafficHijacker) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return nil, nil, w.err
}

func TestTrafficCountedPreservesWebSocketUpgrade(t *testing.T) {
	for _, nested := range []bool{false, true} {
		name := "direct"
		if nested {
			name = "nested_unwrap_only_writers"
		}
		t.Run(name, func(t *testing.T) {
			stats := NewTrafficStats(time.Now())
			done := make(chan struct{})
			handler := stats.trafficCounted(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				defer close(done)
				upgrader := websocket.Upgrader{}
				conn, err := upgrader.Upgrade(w, r, nil)
				if err != nil {
					t.Errorf("upgrade through traffic middleware: %v", err)
					return
				}
				defer conn.Close()
				if err := conn.WriteMessage(websocket.TextMessage, []byte("OK")); err != nil {
					t.Errorf("write websocket message: %v", err)
				}
			}))
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if nested {
					w = unwrapOnlyTrafficWriter{unwrapOnlyTrafficWriter{w}}
				}
				handler.ServeHTTP(w, r)
			}))
			defer server.Close()
			conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/v1/responses", nil)
			if err != nil {
				t.Fatalf("dial through traffic middleware: %v", err)
			}
			defer conn.Close()
			_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
			_, message, err := conn.ReadMessage()
			if err != nil || string(message) != "OK" {
				t.Fatalf("websocket message = %q, error = %v", message, err)
			}
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("websocket handler did not finish")
			}
		})
	}
}

func TestTrafficResponseWriterHijackErrors(t *testing.T) {
	hijackErr := errors.New("underlying hijack failed")
	for _, test := range []struct {
		name   string
		writer http.ResponseWriter
		want   error
	}{
		{"unsupported", httptest.NewRecorder(), http.ErrNotSupported},
		{"underlying_error", failedTrafficHijacker{httptest.NewRecorder(), hijackErr}, hijackErr},
		{"nested_underlying_error", unwrapOnlyTrafficWriter{failedTrafficHijacker{httptest.NewRecorder(), hijackErr}}, hijackErr},
	} {
		t.Run(test.name, func(t *testing.T) {
			writer := &trafficResponseWriter{ResponseWriter: test.writer}
			conn, rw, err := writer.Hijack()
			if !errors.Is(err, test.want) {
				t.Fatalf("Hijack error = %v, want %v", err, test.want)
			}
			if conn != nil || rw != nil {
				t.Fatal("failed Hijack returned a connection or buffered reader/writer")
			}
			if writer.status != 0 {
				t.Fatalf("failed Hijack recorded status %d before caller handled error", writer.status)
			}
		})
	}
}

type failedTrafficFlusher struct {
	http.ResponseWriter
	err error
}

func (w failedTrafficFlusher) FlushError() error { return w.err }

// A writer with neither Flush nor FlushError, as opposed to ResponseRecorder.
type unsupportedTrafficFlusher struct{ http.ResponseWriter }

func TestTrafficResponseWriterPreservesFlushErrors(t *testing.T) {
	flushErr := errors.New("stream connection reset")
	for _, test := range []struct {
		name   string
		writer http.ResponseWriter
		want   error
	}{
		{"unsupported", unsupportedTrafficFlusher{httptest.NewRecorder()}, http.ErrNotSupported},
		{"underlying_error", failedTrafficFlusher{httptest.NewRecorder(), flushErr}, flushErr},
		{"nested_underlying_error", unwrapOnlyTrafficWriter{failedTrafficFlusher{httptest.NewRecorder(), flushErr}}, flushErr},
	} {
		t.Run(test.name, func(t *testing.T) {
			writer := &trafficResponseWriter{ResponseWriter: test.writer}
			if err := http.NewResponseController(writer).Flush(); !errors.Is(err, test.want) {
				t.Fatalf("ResponseController.Flush error = %v, want %v", err, test.want)
			}
		})
	}
	t.Run("legacy_flusher", func(t *testing.T) {
		recorder := httptest.NewRecorder()
		writer := &trafficResponseWriter{ResponseWriter: recorder}
		var flusher http.Flusher = writer
		flusher.Flush()
		if !recorder.Flushed || writer.status != http.StatusOK {
			t.Fatalf("legacy Flush: flushed=%v status=%d", recorder.Flushed, writer.status)
		}
	})
}

func TestTrafficWebSocketHandshakeSurvivesLaterPanic(t *testing.T) {
	stats := NewTrafficStats(time.Now())
	handler := stats.trafficCounted(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		markWebSocketUpgraded(r.Context())
		panic(http.ErrAbortHandler)
	}))
	func() {
		defer func() {
			if got := recover(); got != http.ErrAbortHandler {
				t.Errorf("panic = %v, want http.ErrAbortHandler", got)
			}
		}()
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/v1/responses", nil))
	}()
	got := stats.Snapshot(nil, time.Now())
	if got.Responses.Other != 1 || got.Responses.Class2xx != 0 || got.Proxy5xx != 0 {
		t.Fatalf("panic after completed handshake traffic = %+v", got)
	}
}

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
