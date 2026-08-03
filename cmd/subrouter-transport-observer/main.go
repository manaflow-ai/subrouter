// Command subrouter-transport-observer is a content-blind reverse proxy used by
// deployment continuity gates. It records transport lifecycle metadata, never
// header values, URLs, credentials, or request and response contents.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

type transportEvent struct {
	Kind         string `json:"kind"`
	Timestamp    string `json:"timestamp"`
	Transport    string `json:"transport,omitempty"`
	Method       string `json:"method,omitempty"`
	Path         string `json:"path,omitempty"`
	RequestID    string `json:"request_id,omitempty"`
	ConnectionID string `json:"connection_id,omitempty"`
	Direction    string `json:"direction,omitempty"`
	Bytes        int64  `json:"bytes,omitempty"`
}

type eventRecorder struct {
	mu     sync.Mutex
	writer io.Writer
	now    func() time.Time
}

func (r *eventRecorder) record(event transportEvent) error {
	if event.Timestamp == "" {
		now := time.Now
		if r.now != nil {
			now = r.now
		}
		event.Timestamp = now().UTC().Format(time.RFC3339Nano)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return json.NewEncoder(r.writer).Encode(event)
}

type observerStats struct {
	mu       sync.Mutex
	requests []transportEvent
	chunks   []transportEvent
	errors   int
	notify   chan struct{}
}

func newObserverStats() *observerStats {
	return &observerStats{notify: make(chan struct{}, 1)}
}

func (s *observerStats) observe(event transportEvent) {
	s.mu.Lock()
	switch event.Kind {
	case "request_started":
		s.requests = append(s.requests, event)
	case "request_chunk", "response_chunk":
		s.chunks = append(s.chunks, event)
	case "proxy_error":
		s.errors++
	case "recording_error":
		s.errors++
	}
	s.mu.Unlock()
	select {
	case s.notify <- struct{}{}:
	default:
	}
}

func (s *observerStats) snapshot() (requests, chunks []transportEvent, proxyErrors int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]transportEvent(nil), s.requests...), append([]transportEvent(nil), s.chunks...), s.errors
}

type observer struct {
	recorder      *eventRecorder
	stats         *observerStats
	requestSeq    atomic.Uint64
	connectionSeq atomic.Uint64
	connectionsMu sync.Mutex
	connections   map[string]string
}

func newObserver(events io.Writer, stats *observerStats) *observer {
	if stats == nil {
		stats = newObserverStats()
	}
	return &observer{
		recorder:    &eventRecorder{writer: events},
		stats:       stats,
		connections: make(map[string]string),
	}
}

func (o *observer) emit(event transportEvent) {
	if event.Timestamp == "" {
		event.Timestamp = time.Now().UTC().Format(time.RFC3339Nano)
	}
	if err := o.recorder.record(event); err != nil {
		o.stats.observe(transportEvent{Kind: "recording_error", Timestamp: event.Timestamp})
		return
	}
	o.stats.observe(event)
}

func (o *observer) requestID() string {
	return fmt.Sprintf("request-%06d", o.requestSeq.Add(1))
}

func (o *observer) connectionID(request *http.Request) string {
	// RemoteAddr is used only as an in-memory key. Evidence receives an opaque,
	// monotonically assigned identifier and never the address itself.
	key := request.RemoteAddr
	if key == "" {
		key = fmt.Sprintf("request:%p", request)
	}
	o.connectionsMu.Lock()
	defer o.connectionsMu.Unlock()
	if id := o.connections[key]; id != "" {
		return id
	}
	id := fmt.Sprintf("connection-%06d", o.connectionSeq.Add(1))
	o.connections[key] = id
	o.emit(transportEvent{Kind: "connection_opened", ConnectionID: id})
	return id
}

func observedTransport(request *http.Request) string {
	if headerHasToken(request.Header, "Connection", "upgrade") &&
		strings.EqualFold(strings.TrimSpace(request.Header.Get("Upgrade")), "websocket") {
		return "websocket"
	}
	return "http"
}

func headerHasToken(header http.Header, name, token string) bool {
	for _, value := range header.Values(name) {
		for _, candidate := range strings.Split(value, ",") {
			if strings.EqualFold(strings.TrimSpace(candidate), token) {
				return true
			}
		}
	}
	return false
}

// observedPath returns only a fixed route template. In particular, tenant and
// lease identifiers are removed even if a caller sends them to the observer.
func observedPath(raw string) string {
	parts := strings.Split(strings.TrimPrefix(raw, "/"), "/")
	if len(parts) >= 2 && parts[0] == "t" {
		parts = parts[2:]
	}
	path := "/" + strings.Join(parts, "/")
	switch path {
	case "/v1/responses", "/responses", "/_subrouter/leases", "/_subrouter/health", "/_subrouter/ready":
		return path
	}
	if strings.HasPrefix(path, "/_subrouter/leases/") && strings.HasSuffix(path, "/events") {
		return "/_subrouter/leases/:id/events"
	}
	return "/other"
}

type requestEvidence struct {
	transport    string
	method       string
	path         string
	requestID    string
	connectionID string
}

type requestEvidenceContextKey struct{}

func (m requestEvidence) event(kind string) transportEvent {
	return transportEvent{
		Kind:         kind,
		Transport:    m.transport,
		Method:       m.method,
		Path:         m.path,
		RequestID:    m.requestID,
		ConnectionID: m.connectionID,
	}
}

type countingReadCloser struct {
	io.ReadCloser
	observer *observer
	meta     requestEvidence
}

func (r *countingReadCloser) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)
	if n > 0 {
		event := r.meta.event("request_chunk")
		event.Direction = "client_to_upstream"
		event.Bytes = int64(n)
		r.observer.emit(event)
	}
	return n, err
}

type countingResponseWriter struct {
	http.ResponseWriter
	observer *observer
	meta     requestEvidence
}

func (w *countingResponseWriter) Write(p []byte) (int, error) {
	n, err := w.ResponseWriter.Write(p)
	if n > 0 {
		event := w.meta.event("response_chunk")
		event.Direction = "upstream_to_client"
		event.Bytes = int64(n)
		w.observer.emit(event)
	}
	return n, err
}

func (w *countingResponseWriter) Flush() {
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (w *countingResponseWriter) Push(target string, options *http.PushOptions) error {
	if pusher, ok := w.ResponseWriter.(http.Pusher); ok {
		return pusher.Push(target, options)
	}
	return http.ErrNotSupported
}

func (w *countingResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hijacker, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("response writer does not support hijacking")
	}
	connection, buffered, err := hijacker.Hijack()
	if err != nil {
		return nil, nil, err
	}
	// ReverseProxy writes the HTTP 101 control response through buffered. The
	// wrapped connection sees only post-handshake WebSocket frame bytes.
	return &countingConn{Conn: connection, observer: w.observer, meta: w.meta}, buffered, nil
}

type countingConn struct {
	net.Conn
	observer *observer
	meta     requestEvidence
	closed   atomic.Bool
}

func (c *countingConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if n > 0 {
		event := c.meta.event("request_chunk")
		event.Direction = "client_to_upstream"
		event.Bytes = int64(n)
		c.observer.emit(event)
	}
	return n, err
}

func (c *countingConn) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	if n > 0 {
		event := c.meta.event("response_chunk")
		event.Direction = "upstream_to_client"
		event.Bytes = int64(n)
		c.observer.emit(event)
	}
	return n, err
}

func (c *countingConn) Close() error {
	if c.closed.CompareAndSwap(false, true) {
		c.observer.emit(transportEvent{Kind: "connection_closed", ConnectionID: c.meta.connectionID})
	}
	return c.Conn.Close()
}

func newObserverHandler(upstream *url.URL, events io.Writer) http.Handler {
	return newObserverHandlerWithStats(upstream, events, nil)
}

func newObserverHandlerWithStats(upstream *url.URL, events io.Writer, stats *observerStats) http.Handler {
	observation := newObserver(events, stats)
	proxy := httputil.NewSingleHostReverseProxy(upstream)
	originalDirector := proxy.Director
	proxy.Director = func(request *http.Request) {
		originalDirector(request)
		request.Host = upstream.Host
	}
	proxy.FlushInterval = -1
	proxy.ErrorLog = log.New(io.Discard, "", 0)
	proxy.ErrorHandler = func(w http.ResponseWriter, request *http.Request, _ error) {
		if value := request.Context().Value(requestEvidenceContextKey{}); value != nil {
			meta := value.(requestEvidence)
			observation.emit(meta.event("proxy_error"))
		}
		http.Error(w, "upstream transport failed", http.StatusBadGateway)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/_observer/health" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		meta := requestEvidence{
			transport:    observedTransport(request),
			method:       request.Method,
			path:         observedPath(request.URL.EscapedPath()),
			requestID:    observation.requestID(),
			connectionID: observation.connectionID(request),
		}
		observation.emit(meta.event("request_started"))
		if request.Body != nil {
			request.Body = &countingReadCloser{ReadCloser: request.Body, observer: observation, meta: meta}
		}
		request = request.WithContext(context.WithValue(request.Context(), requestEvidenceContextKey{}, meta))
		proxy.ServeHTTP(&countingResponseWriter{ResponseWriter: w, observer: observation, meta: meta}, request)
		observation.emit(meta.event("request_completed"))
	})
}

func validateObserverUpstream(upstream *url.URL) error {
	if upstream == nil || upstream.Host == "" {
		return fmt.Errorf("--upstream must include a host")
	}
	if upstream.Scheme != "http" && upstream.Scheme != "https" {
		return fmt.Errorf("--upstream must be an http or https URL")
	}
	if upstream.User != nil {
		return fmt.Errorf("--upstream must not contain user information")
	}
	return nil
}

func readPrivateValue(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("private input is not a regular file")
	}
	if info.Mode().Perm()&0o077 != 0 {
		return "", fmt.Errorf("private input permissions must be 0600 or stricter")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}

func writePrivateFile(path string, contents []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".private-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(contents); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryPath, path)
}

func runProxy(args []string) error {
	flags := flag.NewFlagSet("proxy", flag.ContinueOnError)
	listenAddress := flags.String("listen", "127.0.0.1:0", "loopback address to listen on")
	upstreamValue := flags.String("upstream", "", "upstream HTTP URL")
	upstreamFile := flags.String("upstream-file", "", "0600 file containing the upstream HTTP URL")
	eventPath := flags.String("events", "", "JSONL evidence path")
	readyPath := flags.String("ready-file", "", "optional 0600 file receiving the bound address")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if strings.TrimSpace(*upstreamValue) != "" && strings.TrimSpace(*upstreamFile) != "" {
		return fmt.Errorf("use only one of --upstream and --upstream-file")
	}
	if strings.TrimSpace(*upstreamFile) != "" {
		value, err := readPrivateValue(*upstreamFile)
		if err != nil {
			return fmt.Errorf("read upstream file: %w", err)
		}
		*upstreamValue = value
	}
	if strings.TrimSpace(*upstreamValue) == "" {
		return fmt.Errorf("--upstream or --upstream-file is required")
	}
	if strings.TrimSpace(*eventPath) == "" {
		return fmt.Errorf("--events is required")
	}
	upstream, err := url.Parse(*upstreamValue)
	if err != nil {
		return fmt.Errorf("parse upstream URL")
	}
	if err := validateObserverUpstream(upstream); err != nil {
		return err
	}
	events, err := os.OpenFile(*eventPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("open evidence file: %w", err)
	}
	defer events.Close()
	if err := events.Chmod(0o600); err != nil {
		return fmt.Errorf("protect evidence file")
	}
	listener, err := net.Listen("tcp", *listenAddress)
	if err != nil {
		return err
	}
	defer listener.Close()
	if !listener.Addr().(*net.TCPAddr).IP.IsLoopback() {
		return fmt.Errorf("observer must listen on loopback")
	}
	if *readyPath != "" {
		if err := writePrivateFile(*readyPath, []byte(listener.Addr().String()+"\n")); err != nil {
			return fmt.Errorf("write ready file: %w", err)
		}
	}
	server := &http.Server{
		Handler:           newObserverHandler(upstream, events),
		ReadHeaderTimeout: 10 * time.Second,
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()
	if err := server.Serve(listener); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

func run() error {
	return runProxy(os.Args[1:])
}

func main() {
	args := os.Args[1:]
	command := "proxy"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		command = args[0]
		args = args[1:]
	}
	var err error
	switch command {
	case "proxy":
		err = runProxy(args)
	case "golden":
		err = runGolden(args)
	default:
		err = fmt.Errorf("unknown command %q", command)
	}
	if err != nil {
		// Errors returned by this command are designed to contain only fixed
		// labels. Never add upstream URLs or child output here.
		fmt.Fprintf(os.Stderr, "subrouter transport observer: %v\n", err)
		os.Exit(1)
	}
}
