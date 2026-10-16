package proxy

import (
	"context"
	"net"
	"net/http"

	"github.com/gorilla/websocket"
)

// dialWebSocketWithContext also interrupts the HTTP upgrade when the caller
// cancels after TCP connected. The dialer's context-aware TCP dial and fixed
// handshake deadline alone do not interrupt that in-progress response read.
func dialWebSocketWithContext(requestCtx context.Context, rawURL string, headers http.Header) (*websocket.Conn, *http.Response, error) {
	dialer := *outboundWebSocketDialer()
	dialTCP := dialer.NetDialContext
	var stopCancellation func() bool
	dialer.NetDialContext = func(dialCtx context.Context, network, address string) (net.Conn, error) {
		conn, err := dialTCP(dialCtx, network, address)
		if err == nil {
			// Gorilla cancels dialCtx on a successful handshake too. Watching
			// the caller's context keeps that cleanup from closing the socket.
			stopCancellation = context.AfterFunc(requestCtx, func() { _ = conn.Close() })
		}
		return conn, err
	}
	conn, response, err := dialer.DialContext(requestCtx, rawURL, headers)
	if stopCancellation != nil {
		stopCancellation()
	}
	return conn, response, err
}
