package main

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// The supervisor's drain waits for a retired worker's connections to close and
// never times out, so an idle keep-alive connection can pin an obsolete worker
// indefinitely. IdleTimeout is what bounds that.
func TestWorkerServerBoundsIdleConnections(t *testing.T) {
	if workerIdleTimeout <= 0 {
		t.Fatal("workerIdleTimeout must be positive, otherwise an idle client pins a retired worker forever")
	}

	// Same server construction as the worker, with a short timeout so the test
	// observes the real behavior rather than asserting on a constant.
	const idle = 150 * time.Millisecond
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintln(w, "ok")
	}))
	server.Config.ReadHeaderTimeout = 10 * time.Second
	server.Config.IdleTimeout = idle

	closed := make(chan struct{}, 1)
	server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateClosed {
			select {
			case closed <- struct{}{}:
			default:
			}
		}
	}
	server.Start()
	defer server.Close()

	conn, err := net.Dial("tcp", server.Listener.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	// One request, then go quiet and hold the connection open like a client
	// between turns.
	if _, err := fmt.Fprintf(conn, "GET / HTTP/1.1\r\nHost: x\r\n\r\n"); err != nil {
		t.Fatalf("write request: %v", err)
	}
	buf := make([]byte, 256)
	if _, err := conn.Read(buf); err != nil {
		t.Fatalf("read response: %v", err)
	}

	select {
	case <-closed:
	case <-time.After(10 * time.Second):
		t.Fatal("idle connection was never closed; a retired worker would stay pinned by it")
	}
}
