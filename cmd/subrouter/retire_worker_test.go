package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// A retired generation may keep serving an in-flight stream, but it no longer
// owns shared background mutations such as the active Codex account sweep.
func TestRetireServerStopsActiveGenerationTasks(t *testing.T) {
	ctx, stopActiveGenerationTasks := context.WithCancel(context.Background())
	server := &http.Server{}

	retireServer(server, nil, stopActiveGenerationTasks)

	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("retired worker kept active-generation background tasks running")
	}
}

// A retired worker must stop connection reuse so clients migrate to the new
// generation. Without this, the supervisor's drain never completes (WaitIdle
// has no timeout) and a keep-alive client pins an obsolete worker forever.
func TestRetireServerStopsConnectionReuse(t *testing.T) {
	var mu sync.Mutex
	conns := map[string]struct{}{}

	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintln(w, "ok")
	}))
	server.Config.ConnState = func(c net.Conn, state http.ConnState) {
		if state != http.StateNew {
			return
		}
		mu.Lock()
		conns[c.RemoteAddr().String()] = struct{}{}
		mu.Unlock()
	}
	server.Start()
	defer server.Close()

	client := &http.Client{Timeout: 10 * time.Second}
	get := func() *http.Response {
		t.Helper()
		response, err := client.Get(server.URL)
		if err != nil {
			t.Fatalf("request: %v", err)
		}
		_, _ = io.Copy(io.Discard, response.Body)
		_ = response.Body.Close()
		return response
	}

	// Before retirement, a second request reuses the first connection.
	first := get()
	if first.Close {
		t.Fatal("server asked the client to close before retirement")
	}
	get()
	mu.Lock()
	beforeRetire := len(conns)
	mu.Unlock()
	if beforeRetire != 1 {
		t.Fatalf("opened %d connections for two sequential requests, want 1; keep-alive is not working", beforeRetire)
	}

	retireServer(server.Config, nil)

	// After retirement the response tells the client to close, and the next
	// request must therefore land on a new connection.
	afterFirst := get()
	if !afterFirst.Close {
		t.Fatal("retired server did not send Connection: close, so clients would stay pinned to it")
	}
	get()

	mu.Lock()
	afterRetire := len(conns)
	mu.Unlock()
	if afterRetire <= beforeRetire {
		t.Fatalf("connection count stayed at %d after retirement; clients are still reusing the retired worker", afterRetire)
	}
}

// Retirement must not disturb a request that is already streaming.
func TestRetireServerDoesNotInterruptInFlightRequest(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})

	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		close(started)
		<-release
		fmt.Fprint(w, "tail")
	}))
	server.Start()
	defer server.Close()

	type result struct {
		body string
		err  error
	}
	done := make(chan result, 1)
	go func() {
		response, err := (&http.Client{Timeout: 15 * time.Second}).Get(server.URL)
		if err != nil {
			done <- result{err: err}
			return
		}
		defer response.Body.Close()
		body, err := io.ReadAll(response.Body)
		done <- result{body: string(body), err: err}
	}()

	<-started
	retireServer(server.Config, nil) // retire mid-stream
	close(release)

	select {
	case got := <-done:
		if got.err != nil {
			t.Fatalf("in-flight request failed after retirement: %v", got.err)
		}
		if got.body != "tail" {
			t.Fatalf("in-flight response body = %q, want %q; retirement truncated it", got.body, "tail")
		}
	case <-time.After(15 * time.Second):
		t.Fatal("in-flight request never completed after retirement")
	}
}
