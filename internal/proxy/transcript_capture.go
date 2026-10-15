package proxy

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/manaflow-ai/subrouter/internal/transcript"
	"github.com/manaflow-ai/subrouter/session"
)

// transcriptProcessToken makes request and stream ids unique across worker
// processes, which share one spool and one archive.
var transcriptProcessToken = func() string {
	var token [4]byte
	_, _ = rand.Read(token[:])
	return hex.EncodeToString(token[:])
}()

var transcriptRequestCounter atomic.Uint64

// nextTranscriptRequestID names one client request (or one websocket
// connection) so its metadata, request body and response can be joined.
func nextTranscriptRequestID() string {
	return fmt.Sprintf("req-%s-%d", transcriptProcessToken, transcriptRequestCounter.Add(1))
}

type transcriptRequestIDKey struct{}

func withTranscriptRequestID(ctx context.Context, requestID string) context.Context {
	return context.WithValue(ctx, transcriptRequestIDKey{}, requestID)
}

func transcriptRequestIDFrom(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	id, _ := ctx.Value(transcriptRequestIDKey{}).(string)
	return id
}

// transcriptClientName is the client name the caller declared, if any. The
// machine itself is named from the connection by the transcript writer.
func transcriptClientName(r *http.Request) string {
	if r == nil {
		return ""
	}
	return NormalizeClientName(r.Header.Get(ClientNameHeader))
}

func addTranscriptClient(metadata map[string]any, clientName, userEmail string) {
	if clientName != "" {
		metadata["client_name"] = clientName
	}
	if hash := userEmailHash(userEmail); hash != "" {
		metadata["user_hash"] = hash
	}
}

// requestBodyMaxWhole bounds how much of a streamed request body is held to
// be recorded whole (and delta-encoded). A larger body is recorded as chunks.
const requestBodyMaxWhole = 64 << 20

// requestBodyCapture records a request body that cannot be replayed (so it
// is read exactly once, by the transport) as one whole body in segment mode.
type requestBodyCapture struct {
	io.ReadCloser
	recorder  *transcript.Recorder
	requestID string
	agentType string
	sessionID string
	encoding  string
	streamID  string

	mu       sync.Mutex
	buf      []byte
	hasher   hash.Hash
	offset   int64
	chunked  bool
	sawEOF   bool
	finished sync.Once
}

func newRequestBodyCapture(body io.ReadCloser, recorder *transcript.Recorder, requestID, agentType, sessionID, encoding string) *requestBodyCapture {
	return &requestBodyCapture{
		ReadCloser: body, recorder: recorder, requestID: requestID,
		agentType: agentType, sessionID: sessionID, encoding: encoding,
		streamID: nextTranscriptStreamID(), hasher: sha256.New(),
	}
}

func (c *requestBodyCapture) payload() map[string]any {
	payload := map[string]any{"request_id": c.requestID}
	if c.encoding != "" {
		payload["content_encoding"] = c.encoding
	}
	return payload
}

func (c *requestBodyCapture) Read(p []byte) (int, error) {
	n, err := c.ReadCloser.Read(p)
	// The transport may Close the body while a Read is still running.
	c.mu.Lock()
	defer c.mu.Unlock()
	if n > 0 {
		chunk := p[:n]
		_, _ = c.hasher.Write(chunk)
		if !c.chunked && len(c.buf)+n > requestBodyMaxWhole {
			c.chunked = true
			c.recorder.RecordPayloadChunk(c.agentType, c.sessionID, "http_body", "client_to_upstream", c.streamID, 0, 0, c.buf, c.payload())
			c.offset = int64(len(c.buf))
			c.buf = nil
		}
		if c.chunked {
			c.recorder.RecordPayloadChunk(c.agentType, c.sessionID, "http_body", "client_to_upstream", c.streamID, 0, c.offset, chunk, c.payload())
			c.offset += int64(n)
		} else {
			c.buf = append(c.buf, chunk...)
		}
	}
	if err == io.EOF {
		c.sawEOF = true
		c.finishLocked()
	}
	return n, err
}

func (c *requestBodyCapture) Close() error {
	err := c.ReadCloser.Close()
	c.mu.Lock()
	c.finishLocked()
	c.mu.Unlock()
	return err
}

func (c *requestBodyCapture) finishLocked() {
	c.finished.Do(func() {
		payload := c.payload()
		if !c.sawEOF {
			// The transport stopped reading early (an upstream error or a
			// cancelled request): what was read is recorded, marked partial.
			payload["partial"] = true
		}
		if !c.chunked {
			c.recorder.RecordRequestBody(c.agentType, c.sessionID, "http_body", "client_to_upstream", c.streamID, c.buf, c.encoding, payload)
			c.buf = nil
			return
		}
		c.recorder.RecordPayloadSummary(c.agentType, c.sessionID, "http_body", "client_to_upstream", c.streamID, c.offset, hex.EncodeToString(c.hasher.Sum(nil)), 0, payload)
	})
}

// recordBedrockGatewayRequest records a /bedrock/* gateway call, which
// bypasses the account pool and so never reached the pool's recording.
// It returns the request id for the response, or "" when nothing is
// recorded.
func (s Server) recordBedrockGatewayRequest(r *http.Request, upstreamPath string, body []byte) (agentType, sessionID, requestID string) {
	if !s.Transcripts.SegmentMode() {
		return "", "", ""
	}
	agentType = "claude"
	sessionID = session.ExtractRoutingID(r)
	if sessionID == "" {
		sessionID = "bedrock-gateway"
	}
	if !transcriptWorthRecording(sessionID) {
		return "", "", ""
	}
	requestID = nextTranscriptRequestID()
	metadata := map[string]any{
		"transport":  "http",
		"request_id": requestID,
		"provider":   "bedrock",
		"account":    "bedrock-gateway",
		"method":     r.Method,
		"path":       r.URL.Path,
		"upstream":   "bedrock-runtime" + upstreamPath,
		"headers":    transcript.RedactedHeaders(r.Header),
	}
	addTranscriptClient(metadata, transcriptClientName(r), "")
	s.Transcripts.RecordRequestMeta(agentType, sessionID, r.RemoteAddr, metadata)
	s.Transcripts.RecordRequestBody(agentType, sessionID, "http_body", "client_to_upstream", nextTranscriptStreamID(), append([]byte(nil), body...), r.Header.Get("Content-Encoding"), map[string]any{"request_id": requestID})
	return agentType, sessionID, requestID
}

// recordBedrockGatewayResponse wraps a gateway response body so the bytes
// the client receives are recorded as they stream.
func (s Server) recordBedrockGatewayResponse(resp *http.Response, agentType, sessionID, requestID, sourceName, region string) {
	if requestID == "" || resp == nil || resp.Body == nil {
		return
	}
	payload := map[string]any{
		"status":           resp.StatusCode,
		"request_id":       requestID,
		"provider":         "bedrock",
		"account":          "bedrock-gateway",
		"bedrock_source":   sourceName,
		"bedrock_region":   region,
		"response_headers": transcript.RedactedHeaders(resp.Header),
	}
	resp.Body = newStreamingTranscriptReadCloser(streamingTranscriptConfig{
		ReadCloser: resp.Body,
		Recorder:   s.Transcripts,
		AgentType:  agentType,
		SessionID:  sessionID,
		EventType:  "http_body",
		Direction:  "upstream_to_client",
		StreamID:   nextTranscriptStreamID(),
		Payload:    payload,
		Started:    time.Now(),
	})
}

// handleTranscriptStats reports segment-mode drops, upload progress and spool
// size, so a full spool or a failing upload is visible without the logs.
func (s Server) handleTranscriptStats(w http.ResponseWriter, _ *http.Request) {
	stats, ok := s.Transcripts.SegmentStats()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"segment_mode": ok, "stats": stats, "dropped_events": s.Transcripts.DroppedEvents()})
}

// serveRecorded runs a route that answers the client itself instead of
// through the pool's reverse proxy (Bedrock-primary, the Fable fallback, the
// Azure Codex fallback), recording the request and exactly the bytes the
// client receives. Nothing is recorded when the route declines and writes
// nothing, because the pool path that follows records the request itself.
func (s Server) serveRecorded(w http.ResponseWriter, r *http.Request, agentType, sessionID, route string, serve func(http.ResponseWriter, *http.Request) bool) bool {
	if !s.Transcripts.SegmentMode() || !transcriptWorthRecording(sessionID) || r.Body == nil {
		return serve(w, r)
	}
	var body []byte
	var err error
	if r.GetBody != nil {
		var reader io.ReadCloser
		if reader, err = r.GetBody(); err == nil {
			body, err = io.ReadAll(io.LimitReader(reader, replayablePostMaxBodyBytes+1))
			_ = reader.Close()
		}
	} else {
		body, err = io.ReadAll(io.LimitReader(r.Body, replayablePostMaxBodyBytes+1))
		// Put back exactly what was read, followed by anything past the
		// limit, so the route sees the body it would have seen.
		rest := r.Body
		restored := body
		r.Body = struct {
			io.Reader
			io.Closer
		}{io.MultiReader(bytes.NewReader(restored), rest), rest}
		if int64(len(restored)) <= replayablePostMaxBodyBytes && err == nil {
			r.ContentLength = int64(len(restored))
			r.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(restored)), nil }
		}
	}
	if err != nil || int64(len(body)) > replayablePostMaxBodyBytes {
		return serve(w, r)
	}
	capture := &transcriptResponseWriter{
		ResponseWriter: w, server: s, request: r, body: body, route: route,
		agentType: agentType, sessionID: sessionID, requestID: nextTranscriptRequestID(),
		streamID: nextTranscriptStreamID(), hasher: sha256.New(), started: time.Now(),
	}
	served := serve(capture, r)
	if served || capture.began {
		capture.finish()
	}
	return served
}

// transcriptResponseWriter records what a directly served route writes.
type transcriptResponseWriter struct {
	http.ResponseWriter
	server    Server
	request   *http.Request
	body      []byte
	route     string
	agentType string
	sessionID string
	requestID string
	streamID  string
	status    int
	began     bool
	hasher    hash.Hash
	written   int64
	started   time.Time
}

func (c *transcriptResponseWriter) begin() {
	if c.began {
		return
	}
	c.began = true
	if c.status == 0 {
		c.status = http.StatusOK
	}
	metadata := map[string]any{
		"transport":  "http",
		"request_id": c.requestID,
		"route":      c.route,
		"method":     c.request.Method,
		"path":       c.request.URL.Path,
		"headers":    transcript.RedactedHeaders(c.request.Header),
	}
	addTranscriptClient(metadata, transcriptClientName(c.request), session.ExtractUserEmail(c.request))
	c.server.Transcripts.RecordRequestMeta(c.agentType, c.sessionID, c.request.RemoteAddr, metadata)
	c.server.Transcripts.RecordRequestBody(c.agentType, c.sessionID, "http_body", "client_to_upstream", nextTranscriptStreamID(), c.body, c.request.Header.Get("Content-Encoding"), map[string]any{"request_id": c.requestID})
	c.body = nil
}

func (c *transcriptResponseWriter) WriteHeader(status int) {
	if !c.began {
		c.status = status
		c.begin()
	}
	c.ResponseWriter.WriteHeader(status)
}

func (c *transcriptResponseWriter) Write(p []byte) (int, error) {
	c.begin()
	n, err := c.ResponseWriter.Write(p)
	if n > 0 {
		_, _ = c.hasher.Write(p[:n])
		c.server.Transcripts.RecordPayloadChunk(c.agentType, c.sessionID, "http_body", "upstream_to_client", c.streamID, 0, c.written, p[:n], map[string]any{"request_id": c.requestID})
		c.written += int64(n)
	}
	return n, err
}

func (c *transcriptResponseWriter) Flush() {
	if flusher, ok := c.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (c *transcriptResponseWriter) Unwrap() http.ResponseWriter { return c.ResponseWriter }

func (c *transcriptResponseWriter) finish() {
	c.begin()
	c.server.Transcripts.RecordPayloadSummary(c.agentType, c.sessionID, "http_body", "upstream_to_client", c.streamID, c.written, hex.EncodeToString(c.hasher.Sum(nil)), 0, map[string]any{
		"request_id":       c.requestID,
		"status":           c.status,
		"route":            c.route,
		"provider":         c.route,
		"duration_ms":      time.Since(c.started).Milliseconds(),
		"response_headers": transcript.RedactedHeaders(c.ResponseWriter.Header()),
	})
}
