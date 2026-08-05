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
	"net/http/httptrace"
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

const (
	goldenRequestTokenHeader = "X-Subrouter-Golden-Request-Token"
	goldenRequestStateEnv    = "SUBROUTER_GOLDEN_FAKE_REQUEST_STATE"
	goldenPacedChunkBytes    = 256
	goldenPacedChunkInterval = 100 * time.Millisecond
	goldenPacedReadBuffer    = 64 << 10
	goldenPacedHoldbackBytes = 256
)

type observerDelay interface {
	wait(context.Context, <-chan struct{}, time.Duration) error
}

type timerObserverDelay struct{}

func (timerObserverDelay) wait(ctx context.Context, released <-chan struct{}, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-released:
		return nil
	case <-timer.C:
		return nil
	}
}

// goldenResponsePacer applies backpressure from the local continuity observer
// and retains a response tail so a finite real Codex response cannot complete
// before the deployment gate explicitly releases it.
type goldenResponsePacer struct {
	chunkBytes    int
	holdbackBytes int
	interval      time.Duration
	delay         observerDelay
	released      chan struct{}
	release       sync.Once
	mu            sync.Mutex
	started       bool
	pending       []byte
	sink          func([]byte) (int, error)
}

func newGoldenResponsePacer() *goldenResponsePacer {
	return &goldenResponsePacer{
		chunkBytes:    goldenPacedChunkBytes,
		holdbackBytes: goldenPacedHoldbackBytes,
		interval:      goldenPacedChunkInterval,
		delay:         timerObserverDelay{},
		released:      make(chan struct{}),
	}
}

func (p *goldenResponsePacer) releasePacing() {
	if p == nil {
		return
	}
	p.release.Do(func() { close(p.released) })
}

func (p *goldenResponsePacer) isReleased() bool {
	if p == nil {
		return true
	}
	select {
	case <-p.released:
		return true
	default:
		return false
	}
}

func (p *goldenResponsePacer) write(ctx context.Context, payload []byte, write func([]byte) (int, error)) (int, error) {
	if p == nil || len(payload) == 0 {
		return write(payload)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sink = write
	if p.isReleased() {
		if err := p.flushPendingLocked(); err != nil {
			return 0, err
		}
		return write(payload)
	}
	p.pending = append(p.pending, payload...)
	flushBytes := len(p.pending) - p.holdbackBytes
	if flushBytes <= 0 {
		return len(payload), nil
	}
	if err := p.writePacedLocked(ctx, p.pending[:flushBytes]); err != nil {
		return 0, err
	}
	copy(p.pending, p.pending[flushBytes:])
	p.pending = p.pending[:len(p.pending)-flushBytes]
	return len(payload), nil
}

func (p *goldenResponsePacer) writePacedLocked(ctx context.Context, payload []byte) error {
	total := 0
	for total < len(payload) {
		if p.isReleased() {
			n, err := p.sink(payload[total:])
			total += n
			if err != nil {
				return err
			}
			if total != len(payload) {
				return io.ErrShortWrite
			}
			return nil
		}
		if p.started {
			if err := p.delay.wait(ctx, p.released, p.interval); err != nil {
				return err
			}
			if p.isReleased() {
				continue
			}
		} else {
			p.started = true
		}
		end := total + p.chunkBytes
		if end > len(payload) {
			end = len(payload)
		}
		n, err := p.sink(payload[total:end])
		total += n
		if err != nil {
			return err
		}
		if total != end {
			return io.ErrShortWrite
		}
	}
	return nil
}

func (p *goldenResponsePacer) flushPendingLocked() error {
	if len(p.pending) == 0 {
		return nil
	}
	if p.sink == nil {
		return errors.New("golden response pacing sink is unavailable")
	}
	n, err := p.sink(p.pending)
	p.pending = p.pending[n:]
	if err != nil {
		return err
	}
	if len(p.pending) != 0 {
		return io.ErrShortWrite
	}
	return nil
}

func (p *goldenResponsePacer) hasPayload() bool {
	if p == nil {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.started || len(p.pending) > 0
}

func (p *goldenResponsePacer) waitAndFlush() error {
	if p == nil {
		return nil
	}
	<-p.released
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.flushPendingLocked()
}

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
	StatusCode   int    `json:"status_code,omitempty"`
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
	opened   []transportEvent
	requests []transportEvent
	chunks   []transportEvent
	upstream []transportEvent
	closed   []transportEvent
	errors   int
	notify   chan struct{}
}

func newObserverStats() *observerStats {
	return &observerStats{notify: make(chan struct{}, 1)}
}

func (s *observerStats) observe(event transportEvent) {
	s.mu.Lock()
	switch event.Kind {
	case "connection_opened":
		s.opened = append(s.opened, event)
	case "request_started":
		s.requests = append(s.requests, event)
	case "request_chunk", "response_chunk":
		s.chunks = append(s.chunks, event)
	case "upstream_connection_opened", "upstream_request_chunk", "upstream_response_chunk", "upstream_connection_closed":
		s.upstream = append(s.upstream, event)
	case "connection_closed":
		s.closed = append(s.closed, event)
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

func (s *observerStats) openedSnapshot() []transportEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]transportEvent(nil), s.opened...)
}

func (s *observerStats) snapshot() (requests, chunks []transportEvent, proxyErrors int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]transportEvent(nil), s.requests...), append([]transportEvent(nil), s.chunks...), s.errors
}

func (s *observerStats) closedSnapshot() []transportEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]transportEvent(nil), s.closed...)
}

func (s *observerStats) upstreamSnapshot() []transportEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]transportEvent(nil), s.upstream...)
}

type observer struct {
	recorder      *eventRecorder
	stats         *observerStats
	requests      *observerRequestLifecycle
	requestSeq    atomic.Uint64
	connectionSeq atomic.Uint64
	connectionsMu sync.Mutex
	connections   map[string]string
}

type observerRequestLifecycle struct {
	mu     sync.Mutex
	active int
	notify chan struct{}
}

func newObserverRequestLifecycle() *observerRequestLifecycle {
	return &observerRequestLifecycle{notify: make(chan struct{}, 1)}
}

func (l *observerRequestLifecycle) begin() func() {
	l.mu.Lock()
	l.active++
	l.signalLocked()
	l.mu.Unlock()
	return func() {
		l.mu.Lock()
		l.active--
		l.signalLocked()
		l.mu.Unlock()
	}
}

func (l *observerRequestLifecycle) signalLocked() {
	select {
	case l.notify <- struct{}{}:
	default:
	}
}

func (l *observerRequestLifecycle) wait(ctx context.Context) error {
	for {
		l.mu.Lock()
		complete := l.active == 0
		l.mu.Unlock()
		if complete {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-l.notify:
		}
	}
}

func newObserver(events io.Writer, stats *observerStats) *observer {
	if stats == nil {
		stats = newObserverStats()
	}
	return &observer{
		recorder:    &eventRecorder{writer: events},
		stats:       stats,
		requests:    newObserverRequestLifecycle(),
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
	endpoint := key
	if goldenTestHooks.enabled && goldenTestHooks.socketEndpoint != "" {
		endpoint = goldenTestHooks.socketEndpoint
	}
	id := goldenSocketEndpointID(endpoint)
	if id == "" {
		id = fmt.Sprintf("connection-%06d", o.connectionSeq.Add(1))
	}
	o.connections[key] = id
	o.emit(transportEvent{Kind: "connection_opened", ConnectionID: id})
	return id
}

func (o *observer) closeConnection(remoteAddress string) {
	key := strings.TrimSpace(remoteAddress)
	if key == "" {
		return
	}
	o.connectionsMu.Lock()
	id := o.connections[key]
	delete(o.connections, key)
	o.connectionsMu.Unlock()
	if id != "" {
		o.emit(transportEvent{Kind: "connection_closed", ConnectionID: id})
	}
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
	case "/v1/responses", "/responses", "/api/subrouter/leases", "/_subrouter/leases", "/_subrouter/health", "/_subrouter/ready":
		return path
	}
	if strings.HasPrefix(path, "/api/subrouter/leases/") && strings.HasSuffix(path, "/events") {
		return "/api/subrouter/leases/:id/events"
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
	observer   *observer
	meta       requestEvidence
	statusCode int
	context    context.Context
	pacer      *goldenResponsePacer
}

func (w *countingResponseWriter) WriteHeader(statusCode int) {
	if w.pacer != nil && statusCode != http.StatusSwitchingProtocols &&
		(statusCode < http.StatusOK || statusCode >= http.StatusMultipleChoices) {
		w.pacer.releasePacing()
	}
	w.ResponseWriter.WriteHeader(statusCode)
	if statusCode >= http.StatusOK || statusCode == http.StatusSwitchingProtocols {
		w.recordFinalStatus(statusCode)
	}
}

func (w *countingResponseWriter) Write(p []byte) (int, error) {
	if w.statusCode == 0 {
		w.recordFinalStatus(http.StatusOK)
	}
	return w.pacer.write(w.context, p, func(chunk []byte) (int, error) {
		n, err := w.ResponseWriter.Write(chunk)
		if n > 0 {
			event := w.meta.event("response_chunk")
			event.Direction = "upstream_to_client"
			event.Bytes = int64(n)
			w.observer.emit(event)
		}
		return n, err
	})
}

func (w *countingResponseWriter) recordFinalStatus(statusCode int) {
	if w.statusCode != 0 {
		return
	}
	w.statusCode = statusCode
	event := w.meta.event("response_status")
	event.StatusCode = statusCode
	w.observer.emit(event)
}

func (w *countingResponseWriter) finish() {
	if w.statusCode == 0 {
		w.recordFinalStatus(http.StatusOK)
	}
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
	w.recordFinalStatus(http.StatusSwitchingProtocols)
	// ReverseProxy writes the HTTP 101 control response through buffered. The
	// wrapped connection sees only post-handshake WebSocket frame bytes.
	return &countingConn{Conn: connection, observer: w.observer, meta: w.meta, context: w.context, pacer: w.pacer}, buffered, nil
}

type countingConn struct {
	net.Conn
	observer *observer
	meta     requestEvidence
	context  context.Context
	pacer    *goldenResponsePacer
	closed   atomic.Bool
}

type countingUpstreamConn struct {
	net.Conn
	observer *observer
	meta     requestEvidence
	id       string
	closed   atomic.Bool
}

func (c *countingUpstreamConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if n > 0 {
		event := c.meta.event("upstream_response_chunk")
		event.ConnectionID = c.id
		event.Direction = "upstream_to_observer"
		event.Bytes = int64(n)
		c.observer.emit(event)
	}
	return n, err
}

func (c *countingUpstreamConn) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	if n > 0 {
		event := c.meta.event("upstream_request_chunk")
		event.ConnectionID = c.id
		event.Direction = "observer_to_upstream"
		event.Bytes = int64(n)
		c.observer.emit(event)
	}
	return n, err
}

func (c *countingUpstreamConn) Close() error {
	c.closed.CompareAndSwap(false, true)
	return c.Conn.Close()
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
	return c.pacer.write(c.context, p, func(chunk []byte) (int, error) {
		n, err := c.Conn.Write(chunk)
		if n > 0 {
			event := c.meta.event("response_chunk")
			event.Direction = "upstream_to_client"
			event.Bytes = int64(n)
			c.observer.emit(event)
		}
		return n, err
	})
}

func (c *countingConn) Close() error {
	if c.pacer != nil {
		if !c.pacer.hasPayload() {
			c.pacer.releasePacing()
		}
		if err := c.pacer.waitAndFlush(); err != nil {
			_ = c.Conn.Close()
			return err
		}
	}
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
	return newObserverHandlerWithObserver(upstream, observation)
}

func newObserverHandlerWithObserver(upstream *url.URL, observation *observer) http.Handler {
	return newObserverHandlerWithObserverAndPacer(upstream, observation, nil)
}

func newObserverHandlerWithObserverAndPacer(upstream *url.URL, observation *observer, pacer *goldenResponsePacer) http.Handler {
	proxy := httputil.NewSingleHostReverseProxy(upstream)
	transport := http.DefaultTransport.(*http.Transport).Clone()
	dialer := &net.Dialer{}
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		connection, err := dialer.DialContext(ctx, network, address)
		if err != nil {
			return nil, err
		}
		meta, ok := ctx.Value(requestEvidenceContextKey{}).(requestEvidence)
		if !ok {
			return connection, nil
		}
		if pacer != nil && (meta.path == "/v1/responses" || meta.path == "/responses") {
			if tcp, ok := connection.(*net.TCPConn); ok {
				if err := tcp.SetReadBuffer(goldenPacedReadBuffer); err != nil {
					_ = connection.Close()
					return nil, err
				}
			}
		}
		id := goldenSocketEndpointID(connection.LocalAddr().String())
		event := meta.event("upstream_connection_opened")
		event.ConnectionID = id
		observation.emit(event)
		return &countingUpstreamConn{Conn: connection, observer: observation, meta: meta, id: id}, nil
	}
	proxy.Transport = transport
	if goldenTestHooks.enabled {
		proxy.Transport = &goldenRequestWriteTransport{
			base:   transport,
			signal: goldenTestHooks.outboundRequestWritten,
		}
	}
	originalDirector := proxy.Director
	proxy.Director = func(request *http.Request) {
		originalDirector(request)
		request.Host = upstream.Host
		if !headerHasToken(request.Header, "Connection", "upgrade") {
			request.Close = true
		}
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
		finishRequest := observation.requests.begin()
		defer finishRequest()
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
		responsePacer := pacer
		if meta.path != "/v1/responses" && meta.path != "/responses" {
			responsePacer = nil
		}
		responseWriter := &countingResponseWriter{
			ResponseWriter: w, observer: observation, meta: meta,
			context: request.Context(), pacer: responsePacer,
		}
		proxy.ServeHTTP(responseWriter, request)
		if responsePacer != nil {
			if !responsePacer.hasPayload() {
				responsePacer.releasePacing()
			}
			if err := responsePacer.waitAndFlush(); err != nil {
				observation.emit(meta.event("proxy_error"))
			}
		}
		responseWriter.finish()
		observation.emit(meta.event("request_completed"))
	})
}

type goldenRequestWriteTransport struct {
	base   http.RoundTripper
	signal func(string) error
}

func (transport *goldenRequestWriteTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if !strings.HasSuffix(request.URL.Path, "/responses") {
		return transport.base.RoundTrip(request)
	}
	token, err := goldenRequestToken(request.Header)
	if err != nil {
		return nil, err
	}
	if transport.signal == nil {
		return nil, errors.New("golden request completion signal is unavailable")
	}

	requestContext, cancel := context.WithCancelCause(request.Context())
	var callbackOnce sync.Once
	callbackDone := make(chan struct{})
	var callbackErr error
	trace := &httptrace.ClientTrace{WroteRequest: func(info httptrace.WroteRequestInfo) {
		callbackOnce.Do(func() {
			err := info.Err
			if err == nil {
				err = transport.signal(token)
			}
			callbackErr = err
			close(callbackDone)
			if err != nil {
				cancel(err)
			}
		})
	}}
	request = request.WithContext(httptrace.WithClientTrace(requestContext, trace))
	response, roundTripErr := transport.base.RoundTrip(request)
	if roundTripErr != nil {
		cancel(roundTripErr)
		select {
		case <-callbackDone:
			if callbackErr != nil {
				return nil, fmt.Errorf("golden request completion signal failed: %w", callbackErr)
			}
		default:
		}
		return nil, roundTripErr
	}
	select {
	case <-callbackDone:
	case <-request.Context().Done():
		if response != nil && response.Body != nil {
			_ = response.Body.Close()
		}
		cancel(request.Context().Err())
		return nil, request.Context().Err()
	}
	if callbackErr != nil {
		if response != nil && response.Body != nil {
			_ = response.Body.Close()
		}
		return nil, fmt.Errorf("golden request completion signal failed: %w", callbackErr)
	}
	// The child context stays live for the streaming response and is released
	// when ReverseProxy completes and cancels the parent request context.
	return response, nil
}

func goldenRequestToken(header http.Header) (string, error) {
	values := header.Values(goldenRequestTokenHeader)
	if len(values) != 1 || !validGoldenRequestToken(values[0]) {
		return "", errors.New("invalid golden request token")
	}
	return values[0], nil
}

func validGoldenRequestToken(token string) bool {
	if len(token) != 32 {
		return false
	}
	for index := range len(token) {
		character := token[index]
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return false
		}
	}
	return true
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
