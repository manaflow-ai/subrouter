package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/session"
)

// A redial refused with 401 means the credential is dead: the retry must stop
// and hand the session back with 1012 instead of redialing it forever.
func TestAutonomousCodexWebSocketRedialStopsOnRejectedCredential(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	var dials atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if dials.Add(1) > 1 {
			http.Error(w, "token expired", http.StatusUnauthorized)
			return
		}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		if _, _, err := conn.ReadMessage(); err != nil {
			return
		}
		// Fail the turn and hang up so the retry has to redial.
		_ = conn.WriteMessage(websocket.TextMessage, []byte(turnRetryCapacityFailure))
	}))
	defer upstream.Close()

	client := turnRetryProxy(t, upstream, noTurnRetryDelay)
	if err := client.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","input":[]}`)); err != nil {
		t.Fatal(err)
	}
	_ = client.SetReadDeadline(time.Now().Add(10 * time.Second))
	_, body, err := client.ReadMessage()
	var closeErr *websocket.CloseError
	if err == nil || !errors.As(err, &closeErr) || closeErr.Code != websocket.CloseServiceRestart {
		t.Fatalf("client read = %q, %v; want 1012 so Codex reconnects on another account", body, err)
	}
	if got := dials.Load(); got != 2 {
		t.Fatalf("upstream dials = %d, want 2 (no redial after the 401)", got)
	}
}

// failingReader returns a partial payload together with a read error, as
// gorilla does when an upstream frame is cut short.
type failingReader struct {
	payload []byte
	sent    bool
}

func (r *failingReader) Read(p []byte) (int, error) {
	if r.sent {
		return 0, io.ErrUnexpectedEOF
	}
	r.sent = true
	return copy(p, r.payload), io.ErrUnexpectedEOF
}

func TestStreamWebSocketMessageDropsHeldMessageOnReadError(t *testing.T) {
	upgrader := websocket.Upgrader{}
	received := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		_, body, err := conn.ReadMessage()
		if err != nil {
			received <- ""
			return
		}
		received <- string(body)
	}))
	defer server.Close()
	conn, response, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	defer conn.Close()

	_, _, streamErr := streamWebSocketMessage(
		context.Background(),
		&failingReader{payload: []byte(`{"type":"response.output_text.del`)},
		func() (io.WriteCloser, error) {
			return &bufferedWebSocketWriter{conn: conn, messageType: websocket.TextMessage}, nil
		},
		newWebSocketMessageObserver(nil, "codex", "session", "upstream_to_client", websocket.TextMessage),
		newWebSocketCopyBufferPool(newWebSocketByteBudget(webSocketCopyChunkBytes), webSocketCopyChunkBytes),
		nil,
	)
	if !errors.Is(streamErr, io.ErrUnexpectedEOF) {
		t.Fatalf("stream error = %v, want the read error", streamErr)
	}
	// Send a sentinel so the reader sees exactly one message either way.
	if err := conn.WriteMessage(websocket.TextMessage, []byte("sentinel")); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-received:
		if got != "sentinel" {
			t.Fatalf("client received %q first, want the truncated message dropped", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no message received")
	}
}

func TestAutonomousRetryReturnsNilResponseOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	transport := autonomousAgentRetryTransport{
		base: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusServiceUnavailable,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"error":{"code":"server_is_overloaded"}}`)),
				Request:    req,
			}, nil
		}),
		provider: accounts.ProviderCodex,
		budget:   newAttemptBudget(5),
		// The client cancels while the retry is backing off.
		sleep: func(context.Context, time.Duration) bool {
			cancel()
			return false
		},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://upstream/v1/responses", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	response, err := transport.RoundTrip(req)
	if err == nil {
		t.Fatal("round trip succeeded, want the cancellation error")
	}
	if response != nil {
		t.Fatalf("response = %+v, want nil alongside the cancellation error", response)
	}
}

func TestSessionsAPIIncludesActiveRetry(t *testing.T) {
	store, err := session.NewStore(t.TempDir() + "/sessions.json")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put("codex", "waiting", "codex-account", ""); err != nil {
		t.Fatal(err)
	}
	server := Server{Sessions: store, retryStatuses: newRetryStatusRegistry()}
	release := server.beginRetryWait("codex", "waiting", RetryStatus{
		Provider: accounts.ProviderCodex, Model: "gpt-6.1-sol", AccountID: "codex-account",
		Attempt: 3, Reason: "capacity", NextRetryAt: time.Now().Add(time.Second),
	})
	defer release()
	handler := server.Handler()

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/_subrouter/sessions", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("sessions = %d: %s", response.Code, response.Body.String())
	}
	var rows []sessionAdminView
	if err := json.Unmarshal(response.Body.Bytes(), &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Retry == nil || rows[0].Retry.Model != "gpt-6.1-sol" || rows[0].Retry.AccountID != "codex-account" {
		t.Fatalf("sessions body = %s, want the active retry with model and account", response.Body.String())
	}

	health := httptest.NewRecorder()
	handler.ServeHTTP(health, httptest.NewRequest(http.MethodGet, "/_subrouter/health", nil))
	if strings.Contains(health.Body.String(), "gpt-6.1-sol") || strings.Contains(health.Body.String(), "codex-account") {
		t.Fatalf("health leaked retry details: %s", health.Body.String())
	}
}
