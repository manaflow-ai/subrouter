// Command subrouter-transport-observer is a content-blind reverse proxy used by
// deployment continuity gates. It records transport lifecycle metadata, never
// header values, URLs, credentials, or request and response contents.
package main

import (
	"bufio"
	"context"
	"crypto/tls"
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
	goldenRequestTokenHeader         = "X-Subrouter-Golden-Request-Token"
	goldenResponseAttemptTokenHeader = "X-Subrouter-Golden-Response-Attempt"
	goldenRequestStateEnv            = "SUBROUTER_GOLDEN_FAKE_REQUEST_STATE"
	goldenPacedChunkBytes            = 8
	goldenPacedChunkInterval         = 500 * time.Millisecond
	goldenPacedReadBuffer            = 64 << 10
	goldenPacedHoldbackBytes         = 256
)

type observerDelay interface {
	wait(context.Context, <-chan struct{}, <-chan struct{}, <-chan struct{}, time.Duration) (bool, error)
}

type timerObserverDelay struct{}

func (timerObserverDelay) wait(
	ctx context.Context,
	gateReleased <-chan struct{},
	requestReleased <-chan struct{},
	wake <-chan struct{},
	duration time.Duration,
) (bool, error) {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false, ctx.Err()
	case <-gateReleased:
		return false, nil
	case <-requestReleased:
		return false, nil
	case <-wake:
		return true, nil
	case <-timer.C:
		return false, nil
	}
}

type goldenResponseGate struct {
	released       chan struct{}
	release        sync.Once
	mu             sync.Mutex
	pacingReleased bool
	current        map[string]*goldenResponsePacer
}

func newGoldenResponseGate() *goldenResponseGate {
	return &goldenResponseGate{
		released: make(chan struct{}),
		current:  make(map[string]*goldenResponsePacer),
	}
}

func (g *goldenResponseGate) releasePacing() {
	if g == nil {
		return
	}
	g.release.Do(func() {
		g.mu.Lock()
		g.pacingReleased = true
		clear(g.current)
		close(g.released)
		g.mu.Unlock()
	})
}

// newResponsePacer treats matching non-empty tokens as attempts for one golden
// Codex turn. Untagged and differently tagged requests remain independent.
func (g *goldenResponseGate) newResponsePacer(requestToken string) *goldenResponsePacer {
	if g == nil {
		return nil
	}
	pacer := &goldenResponsePacer{
		chunkBytes:      goldenPacedChunkBytes,
		holdbackBytes:   goldenPacedHoldbackBytes,
		interval:        goldenPacedChunkInterval,
		delay:           timerObserverDelay{},
		gateReleased:    g.released,
		requestReleased: make(chan struct{}),
	}
	g.mu.Lock()
	if !g.pacingReleased && requestToken != "" {
		previous := g.current[requestToken]
		g.current[requestToken] = pacer
		previous.supersede()
	}
	g.mu.Unlock()
	return pacer
}

func (g *goldenResponseGate) finishResponsePacer(requestToken string, pacer *goldenResponsePacer) {
	if g == nil || requestToken == "" || pacer == nil {
		return
	}
	g.mu.Lock()
	if g.current[requestToken] == pacer {
		delete(g.current, requestToken)
	}
	g.mu.Unlock()
}

// goldenResponsePacer applies backpressure from the local continuity observer
// and retains a response tail so a finite real Codex response cannot complete
// before the deployment gate explicitly releases it.
type goldenResponsePacer struct {
	chunkBytes         int
	holdbackBytes      int
	interval           time.Duration
	delay              observerDelay
	gateReleased       <-chan struct{}
	requestReleased    chan struct{}
	releaseRequestOnce sync.Once
	superseded         atomic.Bool
	payloadSeen        atomic.Bool
	mu                 sync.Mutex
	started            bool
	pending            []byte
	sink               func([]byte) (int, error)
}

func (p *goldenResponsePacer) supersede() {
	if p == nil {
		return
	}
	p.superseded.Store(true)
	p.releaseRequest()
}

func (p *goldenResponsePacer) wasSuperseded() bool {
	return p != nil && p.superseded.Load()
}

func (p *goldenResponsePacer) releaseRequest() {
	if p == nil {
		return
	}
	p.releaseRequestOnce.Do(func() { close(p.requestReleased) })
}

func (p *goldenResponsePacer) isReleased() bool {
	if p == nil {
		return true
	}
	select {
	case <-p.gateReleased:
		return true
	case <-p.requestReleased:
		return true
	default:
		return false
	}
}

func (p *goldenResponsePacer) write(ctx context.Context, payload []byte, write func([]byte) (int, error)) (int, error) {
	if p == nil || len(payload) == 0 {
		return write(payload)
	}
	p.payloadSeen.Store(true)
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sink = write
	if p.wasSuperseded() {
		p.pending = nil
		return len(payload), nil
	}
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
	written, err := p.writePacedLocked(ctx, p.pending[:flushBytes])
	if written < 0 || written > len(p.pending) {
		return 0, errors.New("golden response pacing writer returned an invalid byte count")
	}
	copy(p.pending, p.pending[written:])
	p.pending = p.pending[:len(p.pending)-written]
	if err != nil {
		return 0, err
	}
	if written != flushBytes {
		return 0, io.ErrShortWrite
	}
	return len(payload), nil
}

func (p *goldenResponsePacer) writePacedLocked(ctx context.Context, payload []byte) (int, error) {
	total := 0
	for total < len(payload) {
		if p.wasSuperseded() {
			return len(payload), nil
		}
		if p.isReleased() {
			n, err := p.sink(payload[total:])
			total += n
			if err != nil {
				return total, err
			}
			if total != len(payload) {
				return total, io.ErrShortWrite
			}
			return total, nil
		}
		if p.started {
			if _, err := p.delay.wait(ctx, p.gateReleased, p.requestReleased, nil, p.interval); err != nil {
				return total, err
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
			return total, err
		}
		if total != end {
			return total, io.ErrShortWrite
		}
	}
	return total, nil
}

func (p *goldenResponsePacer) flushPendingLocked() error {
	if p.wasSuperseded() {
		p.pending = nil
		return nil
	}
	if len(p.pending) == 0 {
		return nil
	}
	if p.sink == nil {
		return errors.New("golden response pacing sink is unavailable")
	}
	n, err := p.sink(p.pending)
	if n < 0 || n > len(p.pending) {
		return errors.New("golden response pacing writer returned an invalid byte count")
	}
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
	return p.payloadSeen.Load() || p.started || len(p.pending) > 0
}

func (p *goldenResponsePacer) waitAndFlush() error {
	if p == nil {
		return nil
	}
	select {
	case <-p.gateReleased:
	case <-p.requestReleased:
	}
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
	errors   []transportEvent
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
	case "upstream_connection_opened", "upstream_connection_used", "upstream_request_chunk", "upstream_response_chunk", "upstream_connection_closed":
		s.upstream = append(s.upstream, event)
	case "connection_closed":
		s.closed = append(s.closed, event)
	case "proxy_error", "recording_error":
		s.errors = append(s.errors, event)
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
	return append([]transportEvent(nil), s.requests...), append([]transportEvent(nil), s.chunks...), len(s.errors)
}

func (s *observerStats) errorSnapshot() []transportEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]transportEvent(nil), s.errors...)
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
	recorder              *eventRecorder
	stats                 *observerStats
	requests              *observerRequestLifecycle
	requestSeq            atomic.Uint64
	connectionSeq         atomic.Uint64
	connectionsMu         sync.Mutex
	connections           map[string]string
	upstreamConnectionsMu sync.Mutex
	upstreamConnections   map[string]*countingUpstreamConn
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
		recorder:            &eventRecorder{writer: events},
		stats:               stats,
		requests:            newObserverRequestLifecycle(),
		connections:         make(map[string]string),
		upstreamConnections: make(map[string]*countingUpstreamConn),
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

func (o *observer) registerUpstreamConnection(connection net.Conn, meta requestEvidence) *countingUpstreamConn {
	if connection == nil || connection.LocalAddr() == nil {
		return nil
	}
	id := goldenSocketEndpointID(connection.LocalAddr().String())
	if id == "" {
		return nil
	}
	wrapped := &countingUpstreamConn{Conn: connection, observer: o, id: id}
	o.upstreamConnectionsMu.Lock()
	o.upstreamConnections[id] = wrapped
	o.upstreamConnectionsMu.Unlock()
	if meta.requestID != "" {
		event := meta.event("upstream_connection_opened")
		event.ConnectionID = id
		o.emit(event)
	}
	return wrapped
}

func (o *observer) bindUpstreamConnection(connection net.Conn, meta requestEvidence) *countingUpstreamConn {
	if connection == nil || connection.LocalAddr() == nil {
		return nil
	}
	id := goldenSocketEndpointID(connection.LocalAddr().String())
	if id == "" {
		return nil
	}
	o.upstreamConnectionsMu.Lock()
	wrapped := o.upstreamConnections[id]
	o.upstreamConnectionsMu.Unlock()
	if wrapped == nil {
		return nil
	}
	wrapped.bind(meta)
	event := meta.event("upstream_connection_used")
	event.ConnectionID = id
	o.emit(event)
	return wrapped
}

func (o *observer) forgetUpstreamConnection(id string, connection *countingUpstreamConn) {
	if id == "" || connection == nil {
		return
	}
	o.upstreamConnectionsMu.Lock()
	if o.upstreamConnections[id] == connection {
		delete(o.upstreamConnections, id)
	}
	o.upstreamConnectionsMu.Unlock()
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
	observer         *observer
	meta             requestEvidence
	statusCode       int
	context          context.Context
	pacer            *goldenResponsePacer
	websocketUpgrade bool
}

func (w *countingResponseWriter) WriteHeader(statusCode int) {
	if w.pacer != nil && statusCode != http.StatusSwitchingProtocols &&
		(statusCode < http.StatusOK || statusCode >= http.StatusMultipleChoices) {
		w.pacer.releaseRequest()
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
	var websocketPacer *goldenWebSocketPacer
	if w.websocketUpgrade {
		websocketPacer = newGoldenWebSocketPacer(w.pacer)
	}
	return &countingConn{
		Conn: connection, observer: w.observer, meta: w.meta, context: w.context,
		pacer: w.pacer, websocketPacer: websocketPacer,
	}, buffered, nil
}

type countingConn struct {
	net.Conn
	observer       *observer
	meta           requestEvidence
	context        context.Context
	pacer          *goldenResponsePacer
	websocketPacer *goldenWebSocketPacer
	closed         atomic.Bool
}

type countingUpstreamConn struct {
	net.Conn
	observer *observer
	id       string
	metaMu   sync.RWMutex
	meta     requestEvidence
	bound    bool
	closed   atomic.Bool
}

func (c *countingUpstreamConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if n > 0 {
		c.emitChunk("upstream_response_chunk", "upstream_to_observer", n)
	}
	return n, err
}

func (c *countingUpstreamConn) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	if n > 0 {
		c.emitChunk("upstream_request_chunk", "observer_to_upstream", n)
	}
	return n, err
}

func (c *countingUpstreamConn) Close() error {
	if c.closed.CompareAndSwap(false, true) {
		c.observer.forgetUpstreamConnection(c.id, c)
	}
	return c.Conn.Close()
}

func (c *countingUpstreamConn) bind(meta requestEvidence) {
	c.metaMu.Lock()
	c.meta = meta
	c.bound = true
	c.metaMu.Unlock()
}

func (c *countingUpstreamConn) unbind(requestID string) {
	c.metaMu.Lock()
	if c.bound && c.meta.requestID == requestID {
		c.meta = requestEvidence{}
		c.bound = false
	}
	c.metaMu.Unlock()
}

func (c *countingUpstreamConn) emitChunk(kind, direction string, bytes int) {
	c.metaMu.RLock()
	meta, bound := c.meta, c.bound
	c.metaMu.RUnlock()
	if !bound {
		return
	}
	event := meta.event(kind)
	event.ConnectionID = c.id
	event.Direction = direction
	event.Bytes = int64(bytes)
	c.observer.emit(event)
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
	write := func(chunk []byte) (int, error) {
		n, err := c.Conn.Write(chunk)
		if n > 0 {
			event := c.meta.event("response_chunk")
			event.Direction = "upstream_to_client"
			event.Bytes = int64(n)
			c.observer.emit(event)
		}
		return n, err
	}
	if c.websocketPacer != nil {
		return c.websocketPacer.write(c.context, p, write)
	}
	return c.pacer.write(c.context, p, write)
}

func (c *countingConn) Close() error {
	var pacingErr error
	if c.websocketPacer != nil {
		if !c.websocketPacer.hasPayload() {
			c.websocketPacer.releaseRequest()
		}
		pacingErr = c.websocketPacer.waitAndFlush()
	} else if c.pacer != nil {
		if !c.pacer.hasPayload() {
			c.pacer.releaseRequest()
		}
		pacingErr = c.pacer.waitAndFlush()
	}
	if c.closed.CompareAndSwap(false, true) {
		c.observer.emit(transportEvent{Kind: "connection_closed", ConnectionID: c.meta.connectionID})
	}
	closeErr := c.Conn.Close()
	if pacingErr != nil {
		return pacingErr
	}
	return closeErr
}

func newObserverHandler(upstream *url.URL, events io.Writer) http.Handler {
	return newObserverHandlerWithStats(upstream, events, nil)
}

func newObserverHandlerWithStats(upstream *url.URL, events io.Writer, stats *observerStats) http.Handler {
	observation := newObserver(events, stats)
	return newObserverHandlerWithObserver(upstream, observation)
}

func newObserverHandlerWithObserver(upstream *url.URL, observation *observer) http.Handler {
	return newObserverHandlerWithObserverAndGate(upstream, observation, nil)
}

func newGoldenObserverHTTPTransport() *http.Transport {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	// The observer records bytes on one physical connection. HTTP/2 can
	// multiplex unrelated requests on that connection, so constrain this
	// transport to pooled HTTP/1.1 and bind its metadata at GotConn time.
	transport.ForceAttemptHTTP2 = false
	transport.TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{}
	if transport.TLSClientConfig == nil {
		transport.TLSClientConfig = &tls.Config{}
	} else {
		transport.TLSClientConfig = transport.TLSClientConfig.Clone()
	}
	transport.TLSClientConfig.NextProtos = []string{"http/1.1"}
	return transport
}

func newObserverHandlerWithObserverAndGate(upstream *url.URL, observation *observer, gate *goldenResponseGate) http.Handler {
	proxy := httputil.NewSingleHostReverseProxy(upstream)
	transport := newGoldenObserverHTTPTransport()
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
		if gate != nil && (meta.path == "/v1/responses" || meta.path == "/responses") {
			if tcp, ok := connection.(*net.TCPConn); ok {
				if err := tcp.SetReadBuffer(goldenPacedReadBuffer); err != nil {
					_ = connection.Close()
					return nil, err
				}
			}
		}
		if wrapped := observation.registerUpstreamConnection(connection, meta); wrapped != nil {
			return wrapped, nil
		}
		return connection, nil
	}
	proxy.Transport = &goldenUpstreamEvidenceTransport{
		base:        &goldenResponseAttemptHeaderStripTransport{base: transport},
		observation: observation,
	}
	if goldenTestHooks.enabled {
		proxy.Transport = &goldenRequestWriteTransport{
			base:   proxy.Transport,
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
		requestToken := goldenResponseAttemptToken(request.Header)
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
		var responsePacer *goldenResponsePacer
		if meta.path == "/v1/responses" || meta.path == "/responses" {
			responsePacer = gate.newResponsePacer(requestToken)
		}
		responseWriter := &countingResponseWriter{
			ResponseWriter: w, observer: observation, meta: meta,
			context: request.Context(), pacer: responsePacer,
			websocketUpgrade: meta.transport == "websocket",
		}
		proxy.ServeHTTP(responseWriter, request)
		if responsePacer != nil {
			defer gate.finishResponsePacer(requestToken, responsePacer)
			if !responsePacer.hasPayload() {
				responsePacer.releaseRequest()
			}
			if err := responsePacer.waitAndFlush(); err != nil {
				observation.emit(meta.event("proxy_error"))
			}
			if responsePacer.wasSuperseded() {
				observation.emit(meta.event("response_superseded"))
			}
		}
		responseWriter.finish()
		observation.emit(meta.event("request_completed"))
	})
}

type goldenResponseAttemptHeaderStripTransport struct {
	base http.RoundTripper
}

func (transport *goldenResponseAttemptHeaderStripTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	request.Header.Del(goldenResponseAttemptTokenHeader)
	return transport.base.RoundTrip(request)
}

type goldenUpstreamEvidenceTransport struct {
	base        http.RoundTripper
	observation *observer
}

func (transport *goldenUpstreamEvidenceTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	meta, ok := request.Context().Value(requestEvidenceContextKey{}).(requestEvidence)
	if !ok || transport.observation == nil {
		return transport.base.RoundTrip(request)
	}
	var bound *countingUpstreamConn
	trace := &httptrace.ClientTrace{GotConn: func(info httptrace.GotConnInfo) {
		if bound != nil {
			bound.unbind(meta.requestID)
		}
		bound = transport.observation.bindUpstreamConnection(info.Conn, meta)
	}}
	request = request.WithContext(httptrace.WithClientTrace(request.Context(), trace))
	response, err := transport.base.RoundTrip(request)
	if err != nil {
		if bound != nil {
			bound.unbind(meta.requestID)
		}
		return nil, err
	}
	if bound == nil || response == nil || response.Body == nil {
		if bound != nil {
			bound.unbind(meta.requestID)
		}
		return response, nil
	}
	binding := &goldenUpstreamEvidenceBinding{connection: bound, requestID: meta.requestID}
	if body, ok := response.Body.(io.ReadWriteCloser); ok {
		response.Body = &goldenUpstreamReadWriteCloser{ReadWriteCloser: body, binding: binding}
	} else {
		response.Body = &goldenUpstreamReadCloser{ReadCloser: response.Body, binding: binding}
	}
	return response, nil
}

type goldenUpstreamEvidenceBinding struct {
	connection *countingUpstreamConn
	requestID  string
	once       sync.Once
}

func (binding *goldenUpstreamEvidenceBinding) release() {
	if binding == nil || binding.connection == nil {
		return
	}
	binding.once.Do(func() {
		binding.connection.unbind(binding.requestID)
	})
}

type goldenUpstreamReadCloser struct {
	io.ReadCloser
	binding *goldenUpstreamEvidenceBinding
}

func (body *goldenUpstreamReadCloser) Read(p []byte) (int, error) {
	n, err := body.ReadCloser.Read(p)
	if err != nil {
		body.binding.release()
	}
	return n, err
}

func (body *goldenUpstreamReadCloser) Close() error {
	err := body.ReadCloser.Close()
	body.binding.release()
	return err
}

type goldenUpstreamReadWriteCloser struct {
	io.ReadWriteCloser
	binding *goldenUpstreamEvidenceBinding
}

func (body *goldenUpstreamReadWriteCloser) Read(p []byte) (int, error) {
	n, err := body.ReadWriteCloser.Read(p)
	if err != nil {
		body.binding.release()
	}
	return n, err
}

func (body *goldenUpstreamReadWriteCloser) Close() error {
	err := body.ReadWriteCloser.Close()
	body.binding.release()
	return err
}

func goldenResponseAttemptToken(header http.Header) string {
	values := header.Values(goldenResponseAttemptTokenHeader)
	if len(values) != 1 || !validGoldenRequestToken(values[0]) {
		return ""
	}
	return values[0]
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
	case "golden-slot":
		err = runGoldenSlot(args)
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
