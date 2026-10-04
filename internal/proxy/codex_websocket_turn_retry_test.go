package proxy

import (
	"errors"
	"io"
	"log/slog"
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

const (
	turnRetryCapacityFailure = `{"type":"response.failed","response":{"error":{"code":"server_is_overloaded","message":"Selected model is at capacity."}}}`
	turnRetryDelta           = `{"type":"response.output_text.delta","delta":"hello"}`
	turnRetryCompleted       = `{"type":"response.completed","response":{"id":"resp_ok"}}`
)

// turnRetryProxy fronts upstream with an autonomous-capable relay and dials
// it as a pooled sr codex launch would.
func turnRetryProxy(t *testing.T, upstream *httptest.Server, delay func(int) time.Duration) *websocket.Conn {
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
	}
	proxy := httptest.NewServer(server.Handler())
	t.Cleanup(proxy.Close)
	wsURL := "ws" + strings.TrimPrefix(proxy.URL, "http") + "/v1/responses"
	conn, response, err := websocket.DefaultDialer.Dial(wsURL, http.Header{
		"X-Subrouter-Session":  []string{"turn-retry"},
		AgentRetryPolicyHeader: []string{"autonomous"},
	})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	_ = response.Body.Close()
	t.Cleanup(func() { _ = conn.Close() })
	return conn
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
