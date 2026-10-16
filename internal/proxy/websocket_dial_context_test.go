package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestWebSocketHandshakeDoesNotCancelEstablishedConnection(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		messageType, payload, err := conn.ReadMessage()
		if err == nil {
			_ = conn.WriteMessage(messageType, payload)
		}
	}))
	defer upstream.Close()
	for i := 0; i < 25; i++ {
		requestCtx, cancel := context.WithCancel(t.Context())
		conn, response, err := dialWebSocketWithContext(requestCtx, "ws"+strings.TrimPrefix(upstream.URL, "http"), nil)
		if response != nil && response.Body != nil {
			_ = response.Body.Close()
		}
		if err != nil {
			cancel()
			t.Fatalf("handshake %d: %v", i, err)
		}
		runtime.Gosched()
		time.Sleep(2 * time.Millisecond)
		_ = conn.SetReadDeadline(time.Now().Add(time.Second))
		err = conn.WriteMessage(websocket.TextMessage, []byte("after-handshake"))
		if err == nil {
			var payload []byte
			_, payload, err = conn.ReadMessage()
			if err == nil && string(payload) != "after-handshake" {
				t.Errorf("echo = %q", payload)
			}
		}
		_ = conn.Close()
		cancel()
		if err != nil {
			t.Fatalf("established connection %d closed after successful handshake: %v", i, err)
		}
	}
}
