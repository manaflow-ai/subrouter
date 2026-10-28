package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/selectacct"
	"github.com/manaflow-ai/subrouter/session"
)

type blockedReplayConn struct {
	net.Conn
	block   atomic.Bool
	entered chan struct{}
	closed  chan struct{}
	once    sync.Once
}

func (c *blockedReplayConn) Write(body []byte) (int, error) {
	if !c.block.Load() {
		return c.Conn.Write(body)
	}
	close(c.entered)
	<-c.closed
	return 0, net.ErrClosed
}

func (c *blockedReplayConn) Close() error {
	c.once.Do(func() { close(c.closed) })
	return c.Conn.Close()
}

func TestCodexWebSocketReplayWriteCancels(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		conn.ReadMessage()
	}))
	t.Cleanup(upstream.Close)
	var transport *blockedReplayConn
	dialer := websocket.Dialer{NetDial: func(network, address string) (net.Conn, error) {
		conn, err := net.Dial(network, address)
		if err != nil {
			return nil, err
		}
		transport = &blockedReplayConn{Conn: conn, entered: make(chan struct{}), closed: make(chan struct{})}
		return transport, nil
	}}
	conn, _, err := dialer.Dial("ws"+strings.TrimPrefix(upstream.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	transport.block.Store(true)
	link := newWebSocketUpstreamLink(conn)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- link.sendContext(ctx, []byte("replayed request")) }()
	select {
	case <-transport.entered:
	case <-time.After(time.Second):
		t.Fatal("replay write did not start")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("blocked replay = %v, want cancellation", err)
		}
	case <-time.After(time.Second):
		t.Fatal("replay write ignored cancellation")
	}
	link.close()
}

const (
	turnRetryCapacityFailure = `{"type":"response.failed","response":{"error":{"code":"server_is_overloaded","message":"Selected model is at capacity."}}}`
	turnRetryDelta           = `{"type":"response.output_text.delta","delta":"hello"}`
	turnRetryCompleted       = `{"type":"response.completed","response":{"id":"resp_ok"}}`
)

// turnRetryProxy fronts upstream with an autonomous-capable relay and dials
// it as a pooled sr codex launch would.
func turnRetryProxy(t *testing.T, upstream *httptest.Server, delay func(int) time.Duration) *websocket.Conn {
	return turnRetryProxyWithHeaders(t, upstream, delay, http.Header{AgentRetryPolicyHeader: {"autonomous"}}, nil)
}

func turnRetryProxyWithHeaders(t *testing.T, upstream *httptest.Server, delay func(int) time.Duration, headers http.Header, config *CodexOverloadFailoverConfig) *websocket.Conn {
	t.Helper()
	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put("codex", "turn-retry", "codex-account", ""); err != nil {
		t.Fatal(err)
	}
	server := Server{
		Upstream:                     upstreamURL,
		Accounts:                     []accounts.Account{{ID: "codex-account", AuthMode: accounts.AuthModeOAuth, Token: "codex-token"}},
		Sessions:                     store,
		Scheduler:                    selectacct.NewScheduler(nil),
		MaxBodyBytes:                 1 << 20,
		Logger:                       slog.New(slog.NewTextHandler(io.Discard, nil)),
		codexWebSocketTurnRetryDelay: delay,
		CodexOverloadFailover:        config,
	}
	proxy := httptest.NewServer(server.Handler())
	t.Cleanup(proxy.Close)
	wsURL := "ws" + strings.TrimPrefix(proxy.URL, "http") + "/v1/responses"
	headers = headers.Clone()
	if headers == nil {
		headers = make(http.Header)
	}
	headers.Set("X-Subrouter-Session", "turn-retry")
	conn, response, err := websocket.DefaultDialer.Dial(wsURL, headers)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	_ = response.Body.Close()
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func TestCodexWebSocketRetriesCapacityWithoutLauncherHeaders(t *testing.T) {
	for _, chained := range []bool{false, true} {
		t.Run(fmt.Sprint("chained=", chained), func(t *testing.T) {
			var attempts atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
				if err != nil {
					return
				}
				defer conn.Close()
				for {
					if _, _, err := conn.ReadMessage(); err != nil {
						return
					}
					conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.created"}`))
					if attempts.Add(1) <= 2 {
						conn.WriteMessage(websocket.TextMessage, []byte(turnRetryCapacityFailure))
					} else {
						conn.WriteMessage(websocket.TextMessage, []byte(turnRetryDelta))
						conn.WriteMessage(websocket.TextMessage, []byte(turnRetryCompleted))
					}
				}
			}))
			t.Cleanup(upstream.Close)
			client := turnRetryProxyWithHeaders(t, upstream, noTurnRetryDelay, nil, nil)
			create := `{"type":"response.create","model":"gpt-6.1-sol","input":[]}`
			if chained {
				create = `{"type":"response.create","model":"gpt-6.1-sol","previous_response_id":"resp_previous","input":[]}`
			}
			if err := client.WriteMessage(websocket.TextMessage, []byte(create)); err != nil {
				t.Fatal(err)
			}
			readTurn(t, client)
			if attempts.Load() != 3 {
				t.Fatalf("upstream attempts = %d, want 3", attempts.Load())
			}
		})
	}
}

func TestCodexWebSocketDefaultRetryBoundaries(t *testing.T) {
	for _, mode := range []string{"no_retry", "partial_output", "partial_tool_output", "unknown_failure", "server_error", "budget", "disconnect"} {
		t.Run(mode, func(t *testing.T) {
			failure := turnRetryCapacityFailure
			partial := turnRetryDelta
			if mode == "partial_tool_output" {
				partial = `{"type":"response.output_item.done","item":{"type":"function_call","id":"tool_item","call_id":"tool_call","name":"exec_command","arguments":"{}"}}`
			}
			if mode == "unknown_failure" {
				failure = `{"type":"response.failed","response":{"error":{"code":"request_not_supported","message":"unsupported operation"}}}`
			} else if mode == "server_error" {
				failure = `{"type":"response.failed","response":{"error":{"code":"server_error","message":"request failed"}}}`
			}
			var creates atomic.Int32
			closed := make(chan struct{})
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
				if err != nil {
					return
				}
				defer conn.Close()
				defer close(closed)
				for {
					if _, _, err := conn.ReadMessage(); err != nil {
						return
					}
					creates.Add(1)
					if mode == "partial_output" || mode == "partial_tool_output" {
						conn.WriteMessage(websocket.TextMessage, []byte(partial))
					}
					conn.WriteMessage(websocket.TextMessage, []byte(failure))
				}
			}))
			t.Cleanup(upstream.Close)
			headers := http.Header{}
			if mode == "no_retry" {
				headers.Set(subrouterNoRetryHeader, "1")
			}
			var config *CodexOverloadFailoverConfig
			if mode == "budget" {
				config = &CodexOverloadFailoverConfig{StayMaxWait: 5 * time.Millisecond}
			}
			retrying := make(chan struct{})
			client := turnRetryProxyWithHeaders(t, upstream, func(int) time.Duration {
				close(retrying)
				return time.Hour
			}, headers, config)
			if err := client.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"gpt-6.1-sol","input":[]}`)); err != nil {
				t.Fatal(err)
			}
			client.SetReadDeadline(time.Now().Add(time.Second))
			if mode == "disconnect" {
				select {
				case <-retrying:
				case <-time.After(time.Second):
					t.Fatal("retry did not start")
				}
				client.Close()
				select {
				case <-closed:
				case <-time.After(time.Second):
					t.Fatal("retry ignored client disconnect")
				}
			} else if mode == "budget" {
				_, _, err := client.ReadMessage()
				var closeErr *websocket.CloseError
				if !errors.As(err, &closeErr) || closeErr.Code != websocket.CloseServiceRestart {
					t.Fatalf("retry budget did not terminate: %v", err)
				}
			} else {
				if mode == "partial_output" || mode == "partial_tool_output" {
					if _, body, err := client.ReadMessage(); err != nil || string(body) != partial {
						t.Fatalf("partial output = %q, %v", body, err)
					}
				}
				_, body, err := client.ReadMessage()
				if err != nil {
					t.Fatal(err)
				}
				if mode == "partial_output" || mode == "partial_tool_output" {
					var event struct {
						Response struct {
							Error struct {
								Code    string
								Headers map[string]string
							}
						}
					}
					if json.Unmarshal(body, &event) != nil || event.Response.Error.Code != "server_is_overloaded" || event.Response.Error.Headers["Retry-After"] != "1" {
						t.Fatalf("forwarded capacity failure lacks native client retry advice: %s", body)
					}
				} else if string(body) != failure {
					t.Fatalf("non-capacity/no-retry failure changed: %s", body)
				}
			}
			if creates.Load() != 1 {
				t.Fatalf("unexpected replay past boundary: %d creates", creates.Load())
			}
		})
	}
}

func TestCodexWebSocketDefaultReplayMemory(t *testing.T) {
	create := []byte(`{"type":"response.create","model":"gpt-6.1-sol","input":[]}`)
	budget := newWebSocketByteBudget(int64(len(create)))
	state := &webSocketModelState{sameAccountRetry: &overloadRetryPolicy{}, replayBudget: budget}
	state.observe(create)
	if !state.turnRetryable() || budget.used != int64(len(create)) {
		t.Fatal("first turn was not retained within its replay budget")
	}
	state.observe(create)
	state.complete()
	if budget.used != 0 || state.turnRetryable() {
		t.Fatal("overflowed turn retained memory or was considered replayable")
	}
	state.complete()
	state.observe(create)
	state.releaseReplayBodies()
	if budget.used != 0 || state.turnRetryable() {
		t.Fatal("disconnect retained replay memory")
	}
}

func noTurnRetryDelay(int) time.Duration { return 0 }

// readTurn reads client messages until the completion and fails on anything
// a user would see besides the successful turn: an error event or a close.
func readTurn(t *testing.T, conn *websocket.Conn) []string {
	t.Helper()
	var got []string
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	for {
		_, body, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("client read after %q: %v (want the turn delivered on the open socket)", got, err)
		}
		got = append(got, string(body))
		if strings.Contains(string(body), "failed") || strings.Contains(string(body), `"error"`) {
			t.Fatalf("client received failure event %q", body)
		}
		if strings.Contains(string(body), "response.completed") {
			return got
		}
	}
}

func TestAutonomousCodexWebSocketRetriesCapacityTurnInPlace(t *testing.T) {
	const failures = 25
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	var connections atomic.Int32
	var mu sync.Mutex
	var creates []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade: %v", err)
			return
		}
		defer conn.Close()
		connections.Add(1)
		for {
			_, body, err := conn.ReadMessage()
			if err != nil {
				return
			}
			mu.Lock()
			creates = append(creates, string(body))
			n := len(creates)
			mu.Unlock()
			if n <= failures {
				_ = conn.WriteMessage(websocket.TextMessage, []byte(turnRetryCapacityFailure))
				continue
			}
			_ = conn.WriteMessage(websocket.TextMessage, []byte(turnRetryDelta))
			_ = conn.WriteMessage(websocket.TextMessage, []byte(turnRetryCompleted))
		}
	}))
	defer upstream.Close()

	client := turnRetryProxy(t, upstream, noTurnRetryDelay)
	create := `{"type":"response.create","model":"gpt-5.6-sol","previous_response_id":"resp_prev","input":[]}`
	if err := client.WriteMessage(websocket.TextMessage, []byte(create)); err != nil {
		t.Fatal(err)
	}
	got := readTurn(t, client)
	if len(got) != 2 || got[0] != turnRetryDelta {
		t.Fatalf("client messages = %q, want exactly the successful turn", got)
	}

	// The socket is still usable for the next turn.
	if err := client.WriteMessage(websocket.TextMessage, []byte(create)); err != nil {
		t.Fatal(err)
	}
	readTurn(t, client)

	mu.Lock()
	defer mu.Unlock()
	if len(creates) != failures+2 {
		t.Fatalf("upstream creates = %d, want %d", len(creates), failures+2)
	}
	for i, body := range creates {
		if body != create {
			t.Fatalf("create %d = %q, want the client's create resent unchanged", i, body)
		}
	}
	if got := connections.Load(); got != 1 {
		t.Fatalf("upstream connections = %d, want the chained turn kept on one socket", got)
	}
}

func TestAutonomousCodexWebSocketRedialsWhenUpstreamDrops(t *testing.T) {
	const drops = 5
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	var connections atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade: %v", err)
			return
		}
		defer conn.Close()
		n := connections.Add(1)
		if _, _, err := conn.ReadMessage(); err != nil {
			return
		}
		if n <= drops {
			// Fail the turn and hang up, as an overloaded edge may.
			_ = conn.WriteMessage(websocket.TextMessage, []byte(turnRetryCapacityFailure))
			return
		}
		_ = conn.WriteMessage(websocket.TextMessage, []byte(turnRetryDelta))
		_ = conn.WriteMessage(websocket.TextMessage, []byte(turnRetryCompleted))
		_, _, _ = conn.ReadMessage()
	}))
	defer upstream.Close()

	client := turnRetryProxy(t, upstream, noTurnRetryDelay)
	if err := client.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"gpt-5.6-sol","input":[]}`)); err != nil {
		t.Fatal(err)
	}
	if got := readTurn(t, client); len(got) != 2 {
		t.Fatalf("client messages = %q, want exactly the successful turn", got)
	}
	if got := connections.Load(); got != drops+1 {
		t.Fatalf("upstream connections = %d, want %d", got, drops+1)
	}
}

func TestAutonomousCodexWebSocketChainedTurnClosesWhenUpstreamDrops(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		if _, _, err := conn.ReadMessage(); err != nil {
			return
		}
		_ = conn.WriteMessage(websocket.TextMessage, []byte(turnRetryCapacityFailure))
	}))
	defer upstream.Close()

	client := turnRetryProxy(t, upstream, noTurnRetryDelay)
	if err := client.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","previous_response_id":"resp_prev"}`)); err != nil {
		t.Fatal(err)
	}
	_ = client.SetReadDeadline(time.Now().Add(10 * time.Second))
	_, body, err := client.ReadMessage()
	var closeErr *websocket.CloseError
	if err == nil || !errors.As(err, &closeErr) || closeErr.Code != websocket.CloseServiceRestart {
		t.Fatalf("client read = %q, %v; want 1012 so Codex resends the full context", body, err)
	}
}

func TestAutonomousCodexWebSocketRetryStopsWhenClientLeaves(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	upstreamClosed := make(chan struct{})
	var creates atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		defer close(upstreamClosed)
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
			creates.Add(1)
			_ = conn.WriteMessage(websocket.TextMessage, []byte(turnRetryCapacityFailure))
		}
	}))
	defer upstream.Close()

	client := turnRetryProxy(t, upstream, func(int) time.Duration { return time.Hour })
	if err := client.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create"}`)); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	_ = client.Close()
	select {
	case <-upstreamClosed:
	case <-time.After(3 * time.Second):
		t.Fatal("upstream still open 3s after the client left; the retry wait ignored the disconnect")
	}
	if got := creates.Load(); got != 1 {
		t.Fatalf("upstream creates = %d, want 1 (no replay after the client left)", got)
	}
}

func TestAutonomousCodexWebSocketFailureAfterOutputPassesThrough(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	var creates atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
			creates.Add(1)
			_ = conn.WriteMessage(websocket.TextMessage, []byte(turnRetryDelta))
			_ = conn.WriteMessage(websocket.TextMessage, []byte(turnRetryCapacityFailure))
		}
	}))
	defer upstream.Close()

	client := turnRetryProxy(t, upstream, noTurnRetryDelay)
	if err := client.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create"}`)); err != nil {
		t.Fatal(err)
	}
	_ = client.SetReadDeadline(time.Now().Add(10 * time.Second))
	for _, want := range []string{turnRetryDelta, turnRetryCapacityFailure} {
		_, body, err := client.ReadMessage()
		if err != nil || string(body) != want {
			t.Fatalf("client read = %q, %v; want %q (a failure after output must not be replayed)", body, err, want)
		}
	}
	if got := creates.Load(); got != 1 {
		t.Fatalf("upstream creates = %d, want 1", got)
	}
}
