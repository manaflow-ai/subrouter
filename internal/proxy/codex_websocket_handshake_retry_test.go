package proxy

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/manaflow-ai/subrouter/internal/accounts"
)

type webSocketHandshakeArrival struct {
	authorization string
	accountID     string
	requestURI    string
}

func TestAutonomousCodexWebSocketHandshakeRetries503OnSameRoute(t *testing.T) {
	var attempts atomic.Int32
	var mu sync.Mutex
	var arrivals []webSocketHandshakeArrival
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		arrivals = append(arrivals, webSocketHandshakeArrival{
			authorization: r.Header.Get("Authorization"),
			accountID:     r.Header.Get("ChatGPT-Account-ID"),
			requestURI:    r.URL.RequestURI(),
		})
		mu.Unlock()
		if attempts.Add(1) <= 2 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, `{"error":{"type":"server_error","message":"overloaded"}}`)
			return
		}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		_, create, err := conn.ReadMessage()
		if err != nil {
			return
		}
		_ = conn.WriteMessage(websocket.TextMessage, create)
	}))
	defer upstream.Close()

	server, proxyURL := newCodexWebSocketHandshakeTestServer(t, upstream.URL, time.Millisecond)
	header := http.Header{
		"Session-Id":            []string{"ws-handshake-503"},
		"X-Subrouter-Model":     []string{"gpt-6-astra"},
		AgentRetryPolicyHeader:  []string{"autonomous"},
		"X-Test-Route-Affinity": []string{"same-route"},
	}
	conn, response, err := websocket.DefaultDialer.Dial(proxyURL+"?affinity=stable", header)
	if err != nil {
		closeWebSocketResponse(response)
		t.Fatalf("dial after retries: %v", err)
	}
	defer conn.Close()
	closeWebSocketResponse(response)
	create := `{"type":"response.create","model":"gpt-6-astra","input":"same prompt"}`
	if err := conn.WriteMessage(websocket.TextMessage, []byte(create)); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, echoed, err := conn.ReadMessage()
	if err != nil || string(echoed) != create {
		t.Fatalf("echo = %q, err = %v", echoed, err)
	}

	if got := attempts.Load(); got != 3 {
		t.Fatalf("attempts = %d, want 3", got)
	}
	mu.Lock()
	gotArrivals := append([]webSocketHandshakeArrival(nil), arrivals...)
	mu.Unlock()
	if len(gotArrivals) != 3 {
		t.Fatalf("arrivals = %d, want 3", len(gotArrivals))
	}
	for index, got := range gotArrivals {
		if got.authorization != "Bearer oauth-token-0" || got.accountID != "" ||
			got.requestURI != "/codex/responses?affinity=stable" {
			t.Fatalf("arrival %d = %+v; route/account auth changed", index+1, got)
		}
	}
	if status := server.retryStatuses.forSession("codex", "ws-handshake-503"); status != nil {
		t.Fatalf("retry status leaked after successful handshake: %+v", status)
	}
}

func TestAutonomousCodexWebSocketHandshakeRetries429AndReset(t *testing.T) {
	tests := []struct {
		name string
		fail func(http.ResponseWriter)
	}{
		{
			name: "429",
			fail: func(w http.ResponseWriter) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = io.WriteString(w, `{"error":{"type":"server_error","message":"rate limited"}}`)
			},
		},
		{
			name: "connection reset",
			fail: func(w http.ResponseWriter) {
				conn, _, err := w.(http.Hijacker).Hijack()
				if err == nil {
					_ = conn.Close()
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var attempts atomic.Int32
			upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if attempts.Add(1) == 1 {
					test.fail(w)
					return
				}
				conn, err := upgrader.Upgrade(w, r, nil)
				if err == nil {
					_ = conn.Close()
				}
			}))
			defer upstream.Close()
			_, proxyURL := newCodexWebSocketHandshakeTestServer(t, upstream.URL, time.Millisecond)
			conn, response, err := websocket.DefaultDialer.Dial(proxyURL, http.Header{
				"Session-Id":           []string{"ws-handshake-" + strings.ReplaceAll(test.name, " ", "-")},
				AgentRetryPolicyHeader: []string{"autonomous"},
			})
			closeWebSocketResponse(response)
			if err != nil {
				t.Fatalf("dial after %s: %v", test.name, err)
			}
			_ = conn.Close()
			if got := attempts.Load(); got != 2 {
				t.Fatalf("attempts = %d, want 2", got)
			}
		})
	}
}

func TestAutonomousCodexWebSocketHandshakePublishesRetryStatus(t *testing.T) {
	registry := newRetryStatusRegistry()
	server := Server{
		retryStatuses:                registry,
		codexOverloadRerouteCounts:   newCodexOverloadReroutes(),
		webSocketHandshakeRetryDelay: func(int) time.Duration { return time.Hour },
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, _, err := server.dialAutonomousCodexWebSocket(
			ctx, "ws://upstream.invalid/responses", http.Header{},
			"codex", "ws-handshake-status", "codex-account-0", "gpt-6-astra",
			func(context.Context, string, http.Header) (*websocket.Conn, *http.Response, error) {
				return nil, &http.Response{StatusCode: http.StatusServiceUnavailable, Body: http.NoBody}, websocket.ErrBadHandshake
			},
		)
		done <- err
	}()

	deadline := time.Now().Add(2 * time.Second)
	var status *RetryStatus
	for status == nil && time.Now().Before(deadline) {
		status = registry.forSession("codex", "ws-handshake-status")
		if status == nil {
			time.Sleep(time.Millisecond)
		}
	}
	if status == nil || status.Provider != accounts.ProviderCodex ||
		status.Model != "gpt-6-astra" || status.AccountID != "codex-account-0" ||
		status.Attempt != 2 || status.Reason != "http_503" ||
		!status.NextRetryAt.After(time.Now()) {
		t.Fatalf("retry status = %+v", status)
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("retry cancellation = %v, want context canceled", err)
	}
	deadline = time.Now().Add(2 * time.Second)
	for registry.forSession("codex", "ws-handshake-status") != nil && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if status := registry.forSession("codex", "ws-handshake-status"); status != nil {
		t.Fatalf("retry status leaked after cancellation: %+v", status)
	}
}

func TestCodexWebSocketHandshakeRetryEligibilityAndTerminal4xx(t *testing.T) {
	tests := []struct {
		name   string
		status int
		header http.Header
	}{
		{name: "bounded 503", status: http.StatusServiceUnavailable},
		{name: "autonomous no retry", status: http.StatusServiceUnavailable, header: http.Header{
			AgentRetryPolicyHeader: []string{"autonomous"}, "X-Subrouter-No-Retry": []string{"1"},
		}},
		{name: "autonomous forced account", status: http.StatusServiceUnavailable, header: http.Header{
			AgentRetryPolicyHeader: []string{"autonomous"}, "X-Subrouter-Account-ID": []string{"codex-account-0"},
		}},
		{name: "autonomous 400", status: http.StatusBadRequest, header: http.Header{
			AgentRetryPolicyHeader: []string{"autonomous"},
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var attempts atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				attempts.Add(1)
				w.WriteHeader(test.status)
			}))
			defer upstream.Close()
			_, proxyURL := newCodexWebSocketHandshakeTestServer(t, upstream.URL, time.Millisecond)
			_, response, err := websocket.DefaultDialer.Dial(proxyURL, test.header)
			closeWebSocketResponse(response)
			if err == nil {
				t.Fatal("dial unexpectedly succeeded")
			}
			if got := attempts.Load(); got != 1 {
				t.Fatalf("attempts = %d, want 1", got)
			}
		})
	}
}

func TestAutonomousCodexWebSocketHandshakeCancellationAndBodyClose(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	registry := newRetryStatusRegistry()
	server := Server{
		retryStatuses:                registry,
		codexOverloadRerouteCounts:   newCodexOverloadReroutes(),
		webSocketHandshakeRetryDelay: func(int) time.Duration { return time.Hour },
	}
	body := &trackingReadCloser{Reader: strings.NewReader("overloaded")}
	dialed := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, _, err := server.dialAutonomousCodexWebSocket(
			ctx, "ws://upstream.invalid/responses", http.Header{},
			"codex", "ws-handshake-cancel", "codex-account-0", "gpt-6-astra",
			func(context.Context, string, http.Header) (*websocket.Conn, *http.Response, error) {
				close(dialed)
				return nil, &http.Response{StatusCode: http.StatusServiceUnavailable, Body: body}, websocket.ErrBadHandshake
			},
		)
		done <- err
	}()
	<-dialed
	deadline := time.Now().Add(time.Second)
	for registry.forSession("codex", "ws-handshake-cancel") == nil && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if registry.forSession("codex", "ws-handshake-cancel") == nil {
		t.Fatal("retry status was not published")
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context canceled", err)
	}
	if !body.closed.Load() {
		t.Fatal("failed handshake response body was not closed before retry wait")
	}
	if status := registry.forSession("codex", "ws-handshake-cancel"); status != nil {
		t.Fatalf("retry status leaked after cancellation: %+v", status)
	}
}

type trackingReadCloser struct {
	io.Reader
	closed atomic.Bool
}

func (b *trackingReadCloser) Close() error {
	b.closed.Store(true)
	return nil
}

func newCodexWebSocketHandshakeTestServer(t *testing.T, rawUpstream string, retryDelay time.Duration) (Server, string) {
	t.Helper()
	upstreamURL, err := url.Parse(rawUpstream)
	if err != nil {
		t.Fatal(err)
	}
	server := codexEgressServer(t, upstreamURL, nil, 1)
	server.retryStatuses = newRetryStatusRegistry()
	server.codexOverloadRerouteCounts = newCodexOverloadReroutes()
	server.webSocketHandshakeRetryDelay = func(int) time.Duration { return retryDelay }
	proxy := httptest.NewServer(server.Handler())
	t.Cleanup(proxy.Close)
	return server, "ws" + strings.TrimPrefix(proxy.URL, "http") + "/backend-api/codex/responses"
}

func closeWebSocketResponse(response *http.Response) {
	if response != nil && response.Body != nil {
		_ = response.Body.Close()
	}
}
