package proxy

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"

	"github.com/gorilla/websocket"
)

// The plain HTTP path pins http/1.1 and IPv4 because that is what the upstream
// edge accepts; probing the same URL returned a bot challenge over HTTP/2 and a
// real origin response over HTTP/1.1. The websocket dialer has to match, or the
// upgrade closes with EOF before any response headers arrive.
func TestOutboundWebSocketDialerPinsHTTP11(t *testing.T) {
	dialer := outboundWebSocketDialer()

	if dialer == websocket.DefaultDialer {
		t.Fatal("websocket dials use DefaultDialer, which offers no ALPN and dials dual-stack")
	}
	if dialer.TLSClientConfig == nil {
		t.Fatal("dialer has no TLS config, so ALPN is left to the default")
	}
	if !slices.Contains(dialer.TLSClientConfig.NextProtos, "http/1.1") {
		t.Fatalf("NextProtos = %v, want it to advertise http/1.1; a websocket upgrade cannot proceed over h2",
			dialer.TLSClientConfig.NextProtos)
	}
	if slices.Contains(dialer.TLSClientConfig.NextProtos, "h2") {
		t.Fatalf("NextProtos = %v, must not offer h2 for an upgrade", dialer.TLSClientConfig.NextProtos)
	}
	if dialer.NetDialContext == nil {
		t.Fatal("dialer has no NetDialContext, so it will not pin IPv4 like the HTTP transport does")
	}
	if dialer.HandshakeTimeout <= 0 {
		t.Fatal("HandshakeTimeout is unset; a silent upstream would hang the upgrade indefinitely")
	}
}

// The negotiated protocol is what actually matters, so assert it end to end
// against a TLS server that records ALPN.
func TestOutboundWebSocketDialerNegotiatesHTTP11(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(_ *http.Request) bool { return true }}

	var mu sync.Mutex
	var negotiated string

	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS != nil {
			mu.Lock()
			negotiated = r.TLS.NegotiatedProtocol
			mu.Unlock()
		}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		_ = conn.WriteMessage(websocket.TextMessage, []byte("ok"))
	}))
	// Offer both, so the client's advertisement decides.
	server.TLS = &tls.Config{NextProtos: []string{"h2", "http/1.1"}}
	server.StartTLS()
	defer server.Close()

	dialer := *outboundWebSocketDialer()
	// Trust the test server's cert while keeping the ALPN pin under test.
	dialer.TLSClientConfig = &tls.Config{
		NextProtos:         outboundWebSocketDialer().TLSClientConfig.NextProtos,
		InsecureSkipVerify: true,
	}
	// httptest listens on IPv4 loopback, matching the pinned dial family.

	url := "wss" + server.URL[len("https"):]
	conn, response, err := dialer.Dial(url, nil)
	if err != nil {
		status := 0
		if response != nil {
			status = response.StatusCode
		}
		t.Fatalf("websocket dial failed: %v (status %d)", err, status)
	}
	defer conn.Close()

	if _, _, err := conn.ReadMessage(); err != nil {
		t.Fatalf("read after upgrade: %v", err)
	}

	mu.Lock()
	got := negotiated
	mu.Unlock()
	if got != "http/1.1" {
		t.Fatalf("negotiated ALPN = %q, want http/1.1; the upgrade must not land on h2", got)
	}
}
