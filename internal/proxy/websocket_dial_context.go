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
func dialWebSocketWithContext(ctx context.Context, rawURL string, headers http.Header) (*websocket.Conn, *http.Response, error) {
	dialer := *outboundWebSocketDialer()
	dialTCP := dialer.NetDialContext
	var stopCancellation func() bool
	dialer.NetDialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		conn, err := dialTCP(ctx, network, address)
		if err == nil {
			stopCancellation = context.AfterFunc(ctx, func() { _ = conn.Close() })
		}
		return conn, err
	}
	conn, response, err := dialer.DialContext(ctx, rawURL, headers)
	if stopCancellation != nil {
		stopCancellation()
	}
	return conn, response, err
}
