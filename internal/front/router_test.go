package front

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"
)

func TestSwitchKeepsExistingConnectionOnOldBackend(t *testing.T) {
	backendA := startLineBackend(t, "a")
	backendB := startLineBackend(t, "b")
	router, err := NewRouter(Backend{ID: "a", Address: backendA})
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() { _ = router.Serve(listener) }()

	oldConnection := dialLineClient(t, listener.Addr().String())
	defer oldConnection.Close()
	assertReply(t, oldConnection, "one", "a:one")

	if err := router.Switch(Backend{ID: "b", Address: backendB}); err != nil {
		t.Fatal(err)
	}
	assertReply(t, oldConnection, "two", "a:two")

	newConnection := dialLineClient(t, listener.Addr().String())
	defer newConnection.Close()
	assertReply(t, newConnection, "three", "b:three")
}

func TestWaitIdleDoesNotRaceWithConnectionSelection(t *testing.T) {
	backendA := startLineBackend(t, "a")
	backendB := startLineBackend(t, "b")
	router, err := NewRouter(Backend{ID: "a", Address: backendA})
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() { _ = router.Serve(listener) }()

	oldConnection := dialLineClient(t, listener.Addr().String())
	assertReply(t, oldConnection, "one", "a:one")
	if err := router.Switch(Backend{ID: "b", Address: backendB}); err != nil {
		t.Fatal(err)
	}

	idle := make(chan struct{})
	go func() {
		router.WaitIdle("a")
		close(idle)
	}()
	select {
	case <-idle:
		t.Fatal("old backend became idle while its connection was open")
	case <-time.After(50 * time.Millisecond):
	}
	_ = oldConnection.Close()
	select {
	case <-idle:
	case <-time.After(time.Second):
		t.Fatal("old backend did not become idle after its connection closed")
	}
}

func TestRetiredBackendCanBeDrainedWithABoundedDeadline(t *testing.T) {
	backendA := startLineBackend(t, "a")
	backendB := startLineBackend(t, "b")
	router, err := NewRouter(Backend{ID: "a", Address: backendA})
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() { _ = router.Serve(listener) }()

	oldConnection := dialLineClient(t, listener.Addr().String())
	defer oldConnection.Close()
	assertReply(t, oldConnection, "one", "a:one")
	if err := router.Switch(Backend{ID: "b", Address: backendB}); err != nil {
		t.Fatal(err)
	}

	type boundedRetirement interface {
		WaitIdleContext(context.Context, string) error
		CloseBackendConnections(string) int
	}
	bounded, ok := any(router).(boundedRetirement)
	if !ok {
		t.Fatal("router has no bounded retired-backend drain API")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	err = bounded.WaitIdleContext(ctx, "a")
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("WaitIdleContext error = %v, want deadline exceeded", err)
	}
	if closed := bounded.CloseBackendConnections("a"); closed != 1 {
		t.Fatalf("closed connections = %d, want 1", closed)
	}
	ctx, cancel = context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := bounded.WaitIdleContext(ctx, "a"); err != nil {
		t.Fatalf("retired backend stayed pinned after forced close: %v", err)
	}
}

func TestWaitAllIdleWaitsForAcceptedConnections(t *testing.T) {
	backend := startLineBackend(t, "a")
	router, err := NewRouter(Backend{ID: "a", Address: backend})
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() { _ = router.Serve(listener) }()

	connection := dialLineClient(t, listener.Addr().String())
	assertReply(t, connection, "one", "a:one")
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	idle := make(chan error, 1)
	go func() { idle <- router.WaitAllIdle(ctx) }()
	select {
	case err := <-idle:
		t.Fatalf("router became idle while connection was open: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	_ = connection.Close()
	if err := <-idle; err != nil {
		t.Fatal(err)
	}
}

func startLineBackend(t *testing.T, name string) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			connection, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer connection.Close()
				scanner := bufio.NewScanner(connection)
				if !scanner.Scan() || !strings.HasPrefix(scanner.Text(), "PROXY ") {
					return
				}
				for scanner.Scan() {
					_, _ = fmt.Fprintf(connection, "%s:%s\n", name, scanner.Text())
				}
			}()
		}
	}()
	return listener.Addr().String()
}

func dialLineClient(t *testing.T, address string) net.Conn {
	t.Helper()
	connection, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	return connection
}

func assertReply(t *testing.T, connection net.Conn, request, expected string) {
	t.Helper()
	_ = connection.SetDeadline(time.Now().Add(time.Second))
	if _, err := fmt.Fprintf(connection, "%s\n", request); err != nil {
		t.Fatal(err)
	}
	reply, err := bufio.NewReader(connection).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if reply != expected+"\n" {
		t.Fatalf("reply = %q, want %q", reply, expected+"\n")
	}
}
