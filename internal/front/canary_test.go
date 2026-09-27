package front

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

func startHTTPNameBackend(t *testing.T, name string) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, name)
	})}
	go func() { _ = server.Serve(NewProxyProtocolListener(listener)) }()
	t.Cleanup(func() { _ = server.Close() })
	return listener.Addr().String()
}

func startCanaryRouter(t *testing.T) (*Router, string) {
	t.Helper()
	router, err := NewRouter(Backend{ID: "incumbent", Address: startHTTPNameBackend(t, "incumbent")})
	if err != nil {
		t.Fatal(err)
	}
	router.SetSessionKey(func(request *http.Request) string { return request.Header.Get("X-Session") })
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() { _ = router.Serve(listener) }()
	return router, listener.Addr().String()
}

// sessionConn is one client connection that can carry several requests.
type sessionConn struct {
	conn   net.Conn
	reader *bufio.Reader
}

func dialSession(t *testing.T, address string) *sessionConn {
	t.Helper()
	conn, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return &sessionConn{conn: conn, reader: bufio.NewReader(conn)}
}

func (c *sessionConn) get(t *testing.T, session string) string {
	t.Helper()
	_ = c.conn.SetDeadline(time.Now().Add(3 * time.Second))
	header := ""
	if session != "" {
		header = "X-Session: " + session + "\r\n"
	}
	if _, err := fmt.Fprintf(c.conn, "GET / HTTP/1.1\r\nHost: subrouter\r\n%s\r\n", header); err != nil {
		t.Fatal(err)
	}
	response, err := http.ReadResponse(c.reader, nil)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	body, err := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

// oneShot sends one request for session on a fresh connection.
func oneShot(t *testing.T, address, session string) string {
	t.Helper()
	conn := dialSession(t, address)
	defer conn.conn.Close()
	return conn.get(t, session)
}

func TestCanaryWeightedSplitIsStickyPerSession(t *testing.T) {
	router, address := startCanaryRouter(t)
	if got := oneShot(t, address, "before"); got != "incumbent" {
		t.Fatalf("without a canary got %q", got)
	}
	if err := router.StartCanary(Backend{ID: "candidate", Address: startHTTPNameBackend(t, "candidate")}, 25); err != nil {
		t.Fatal(err)
	}

	const sessions = 400
	first := make(map[string]string, sessions)
	candidates := 0
	for i := 0; i < sessions; i++ {
		session := fmt.Sprintf("session-%d", i)
		first[session] = oneShot(t, address, session)
		if first[session] == "candidate" {
			candidates++
		}
	}
	if share := float64(candidates) / sessions; share < 0.15 || share > 0.35 {
		t.Fatalf("candidate share at weight 25 = %.2f (%d of %d)", share, candidates, sessions)
	}

	// Raising the weight must not move a session that already has a
	// generation, and every new session now goes to the candidate.
	if err := router.SetCanaryWeight(100); err != nil {
		t.Fatal(err)
	}
	for session, want := range first {
		if got := oneShot(t, address, session); got != want {
			t.Fatalf("session %s moved from %s to %s after a weight change", session, want, got)
		}
	}
	for i := 0; i < 20; i++ {
		if got := oneShot(t, address, fmt.Sprintf("new-%d", i)); got != "candidate" {
			t.Fatalf("new session at weight 100 went to %s", got)
		}
	}
	if err := router.SetCanaryWeight(0); err != nil {
		t.Fatal(err)
	}
	if got := oneShot(t, address, "new-0"); got != "candidate" {
		t.Fatalf("pinned session left the candidate at weight 0: %s", got)
	}
	if got := oneShot(t, address, "newest"); got != "incumbent" {
		t.Fatalf("new session at weight 0 went to %s", got)
	}
}

func TestCanaryPinsLongSessionKeysByDigest(t *testing.T) {
	router, address := startCanaryRouter(t)
	if err := router.StartCanary(Backend{ID: "candidate", Address: startHTTPNameBackend(t, "candidate")}, 100); err != nil {
		t.Fatal(err)
	}
	long := strings.Repeat("k", 32<<10)
	if got := oneShot(t, address, long); got != "candidate" {
		t.Fatalf("long session went to %q", got)
	}
	if err := router.SetCanaryWeight(0); err != nil {
		t.Fatal(err)
	}
	if got := oneShot(t, address, long); got != "candidate" {
		t.Fatalf("long session lost its pin: %q", got)
	}
	router.mu.Lock()
	pins := len(router.pins)
	router.mu.Unlock()
	if pins != 1 {
		t.Fatalf("pins = %d, want 1", pins)
	}
}

func TestCanarySplitsSessionlessConnectionsByConnection(t *testing.T) {
	router, address := startCanaryRouter(t)
	if err := router.StartCanary(Backend{ID: "candidate", Address: startHTTPNameBackend(t, "candidate")}, 50); err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	for i := 0; i < 200; i++ {
		counts[oneShot(t, address, "")]++
	}
	if counts["candidate"] == 0 || counts["incumbent"] == 0 {
		t.Fatalf("sessionless connections were not split: %v", counts)
	}
}

func TestCanaryDoesNotMoveExistingConnections(t *testing.T) {
	router, address := startCanaryRouter(t)
	onIncumbent := dialSession(t, address)
	if got := onIncumbent.get(t, "old"); got != "incumbent" {
		t.Fatalf("got %q", got)
	}
	if err := router.StartCanary(Backend{ID: "candidate", Address: startHTTPNameBackend(t, "candidate")}, 100); err != nil {
		t.Fatal(err)
	}
	// A keep-alive connection opened before the canary keeps its backend,
	// even for a session the canary would take.
	if got := onIncumbent.get(t, "new-session"); got != "incumbent" {
		t.Fatalf("existing connection moved to %q", got)
	}
	if got := oneShot(t, address, "new-session"); got != "candidate" {
		t.Fatalf("new connection went to %q", got)
	}
}

func TestCanaryAbortKeepsIncumbentAndDropsNoConnection(t *testing.T) {
	router, address := startCanaryRouter(t)
	if err := router.StartCanary(Backend{ID: "candidate", Address: startHTTPNameBackend(t, "candidate")}, 100); err != nil {
		t.Fatal(err)
	}
	onCandidate := dialSession(t, address)
	if got := onCandidate.get(t, "s1"); got != "candidate" {
		t.Fatalf("got %q", got)
	}
	candidate, err := router.AbortCanary()
	if err != nil || candidate.ID != "candidate" {
		t.Fatalf("abort = %v, %v", candidate, err)
	}
	// The open candidate connection is not cut: it keeps being served.
	if got := onCandidate.get(t, "s1"); got != "candidate" {
		t.Fatalf("open candidate connection after abort got %q", got)
	}
	// Everything new, including the session pinned to the candidate, goes
	// to the incumbent.
	for _, session := range []string{"s1", "s2", ""} {
		if got := oneShot(t, address, session); got != "incumbent" {
			t.Fatalf("session %q after abort went to %q", session, got)
		}
	}
	if router.Active().ID != "incumbent" {
		t.Fatalf("active after abort = %q", router.Active().ID)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	err = router.ForgetWhenIdleContext(ctx, "candidate")
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("candidate forgotten while a connection was open: %v", err)
	}
	_ = onCandidate.conn.Close()
	ctx, cancel = context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := router.ForgetWhenIdleContext(ctx, "candidate"); err != nil {
		t.Fatalf("drain aborted candidate: %v", err)
	}
}

func TestCanaryPromoteRetiresPreviousActive(t *testing.T) {
	router, address := startCanaryRouter(t)
	onIncumbent := dialSession(t, address)
	if got := onIncumbent.get(t, "old"); got != "incumbent" {
		t.Fatalf("got %q", got)
	}
	if err := router.StartCanary(Backend{ID: "candidate", Address: startHTTPNameBackend(t, "candidate")}, 5); err != nil {
		t.Fatal(err)
	}
	if err := router.ForgetWhenIdleContext(context.Background(), "candidate"); err == nil {
		t.Fatal("a running canary was forgettable")
	}
	previous, err := router.PromoteCanary()
	if err != nil || previous.ID != "incumbent" {
		t.Fatalf("promote = %v, %v", previous, err)
	}
	if _, _, running := router.Canary(); running {
		t.Fatal("canary still running after promote")
	}
	if router.Active().ID != "candidate" {
		t.Fatalf("active after promote = %q", router.Active().ID)
	}
	if got := onIncumbent.get(t, "old"); got != "incumbent" {
		t.Fatalf("existing incumbent connection after promote got %q", got)
	}
	if got := oneShot(t, address, "old"); got != "candidate" {
		t.Fatalf("new connection after promote got %q", got)
	}
	_ = onIncumbent.conn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := router.ForgetWhenIdleContext(ctx, "incumbent"); err != nil {
		t.Fatalf("drain previous active: %v", err)
	}
}

func TestCanaryRoutesNonHTTPConnectionWithoutWaiting(t *testing.T) {
	backendA := startLineBackend(t, "a")
	backendB := startLineBackend(t, "b")
	router, err := NewRouter(Backend{ID: "a", Address: backendA})
	if err != nil {
		t.Fatal(err)
	}
	router.SetCanaryPeekTimeout(time.Minute)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() { _ = router.Serve(listener) }()
	if err := router.StartCanary(Backend{ID: "b", Address: backendB}, 0); err != nil {
		t.Fatal(err)
	}
	connection := dialLineClient(t, listener.Addr().String())
	defer connection.Close()
	// assertReply allows one second; the peek timeout is a minute.
	assertReply(t, connection, "one", "a:one")
}

func TestPlausibleRequestStart(t *testing.T) {
	for input, want := range map[string]bool{
		"":                           true,
		"GE":                         true,
		"POST /v1/messages HTTP/1.1": true,
		"POST /v1 HTTP/1.1\r\nHost":  true,
		"one\n":                      false,
		"\x16\x03\x01":               false,
		"get / HTTP/1.1":             false,
		" GET":                       false,
	} {
		if got := plausibleRequestStart([]byte(input)); got != want {
			t.Errorf("plausibleRequestStart(%q) = %t, want %t", input, got, want)
		}
	}
}

func TestSwitchRefusesCanary(t *testing.T) {
	router, _ := startCanaryRouter(t)
	candidate := Backend{ID: "candidate", Address: "127.0.0.1:1"}
	if err := router.StartCanary(candidate, 5); err != nil {
		t.Fatal(err)
	}
	if err := router.Switch(candidate); err == nil {
		t.Fatal("Switch to the canary succeeded")
	}
	if err := router.StartCanary(Backend{ID: "other", Address: "127.0.0.1:1"}, 5); err == nil {
		t.Fatal("a second canary started")
	}
	if err := router.SetCanaryWeight(101); err == nil {
		t.Fatal("weight 101 accepted")
	}
}
