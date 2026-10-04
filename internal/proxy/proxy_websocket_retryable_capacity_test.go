package proxy

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/selectacct"
	"github.com/manaflow-ai/subrouter/session"
)

func TestHandlerRetriesRetryableCapacityWebSocket(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(_ *http.Request) bool { return true }}
	var connections atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Fatalf("upgrade: %v", err)
		}
		defer conn.Close()
		if _, _, err := conn.ReadMessage(); err != nil {
			t.Fatalf("read client message: %v", err)
		}
		if connections.Add(1) == 1 {
			if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.failed","response":{"error":{"code":"server_is_overloaded"}}}`)); err != nil {
				t.Fatalf("write capacity error: %v", err)
			}
			return
		}
		if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed","response":{"id":"resp_1"}}`)); err != nil {
			t.Fatalf("write completion: %v", err)
		}
	}))
	defer upstream.Close()

	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put("codex", "capacity-ws", "codex-account", ""); err != nil {
		t.Fatal(err)
	}
	server := Server{
		Upstream:              upstreamURL,
		Accounts:              []accounts.Account{{ID: "codex-account", AuthMode: accounts.AuthModeOAuth, Token: "codex-token"}},
		Sessions:              store,
		Scheduler:             selectacct.NewScheduler(nil),
		MaxBodyBytes:          1024,
		CodexOverloadFailover: &CodexOverloadFailoverConfig{CapacityRetryPersist: true, persistGap: func() time.Duration { return 0 }},
	}.Handler()
	subrouter := httptest.NewServer(server)
	defer subrouter.Close()

	wsURL := "ws" + strings.TrimPrefix(subrouter.URL, "http") + "/v1/responses"
	dial := func() (*websocket.Conn, error) {
		conn, _, err := websocket.DefaultDialer.Dial(wsURL, http.Header{
			"X-Subrouter-Session":        []string{"capacity-ws"},
			CodexCapacityRetryableHeader: []string{"1"},
			AgentRetryPolicyHeader:       []string{"bounded"},
		})
		if err != nil {
			return nil, err
		}
		if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create"}`)); err != nil {
			_ = conn.Close()
			return nil, err
		}
		return conn, nil
	}

	first, err := dial()
	if err != nil {
		t.Fatal(err)
	}
	_, body, err := first.ReadMessage()
	if err == nil {
		t.Fatalf("first response = %q, want a reconnect close", body)
	}
	var closeErr *websocket.CloseError
	if !errors.As(err, &closeErr) || closeErr.Code != websocket.CloseServiceRestart {
		t.Fatalf("first close = %v, want 1012 capacity retry", err)
	}
	_ = first.Close()

	second, err := dial()
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	_, body, err = second.ReadMessage()
	if err != nil || !strings.Contains(string(body), "response.completed") {
		t.Fatalf("second response = %q, err=%v; want completion after reconnect", body, err)
	}
	if got := connections.Load(); got != 2 {
		t.Fatalf("upstream connections = %d, want 2", got)
	}
}
