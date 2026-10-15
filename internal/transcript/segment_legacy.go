package transcript

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"time"
)

// LegacyConverter rewrites raw per-session transcript JSONL (base64 body
// chunks, as the old local spool and the append blobs hold) into segments:
// bodies reassembled, request bodies delta-encoded, text stored as text,
// uploaded like live segments, with index rows for every call that can be
// paired without guessing.
type LegacyConverter struct {
	w   *segmentWriter
	ctx context.Context

	// OnSourceStored runs once every segment holding a source's events is
	// committed (ok) or one of them failed (not ok). It may run on an upload
	// goroutine.
	OnSourceStored func(source string, ok bool)

	mu       sync.Mutex
	pending  map[string]int
	failed   map[string]bool
	finished map[string]bool

	// pairing state per session: calls waiting for a request body, and
	// calls waiting for a response.
	needBody map[string][]string
	needResp map[string][]string
	serial   int

	SourceBytes int64
	Sources     int
	Mismatched  int64
	Incomplete  int64
	mismatchBy  map[string]bool
}

// NewLegacyConverter writes through cfg.Uploader. Convert runs in the
// caller's goroutine; uploads run in the background.
func NewLegacyConverter(ctx context.Context, cfg SegmentConfig) (*LegacyConverter, error) {
	w, err := newSegmentWriter(cfg, true)
	if err != nil {
		return nil, err
	}
	c := &LegacyConverter{
		w: w, ctx: ctx,
		pending: map[string]int{}, failed: map[string]bool{}, finished: map[string]bool{},
		needBody: map[string][]string{}, needResp: map[string][]string{}, mismatchBy: map[string]bool{},
	}
	w.onSeal = c.sealed
	return c, nil
}

// Stats reports what the upload side has done.
func (c *LegacyConverter) Stats() SegmentStats { return c.w.stats() }

func (c *LegacyConverter) sealed(seg *sessionSegment) {
	for source := range seg.sources {
		c.mu.Lock()
		c.pending[source]++
		c.mu.Unlock()
		source := source
		c.w.up.onSettled(seg, func(ok bool) { c.settle(source, ok) })
	}
}

func (c *LegacyConverter) settle(source string, ok bool) {
	c.mu.Lock()
	c.pending[source]--
	if !ok {
		c.failed[source] = true
	}
	ready := c.finished[source] && c.pending[source] == 0
	stored := !c.failed[source] && !c.mismatchBy[source]
	if ready {
		delete(c.pending, source)
		delete(c.finished, source)
	}
	c.mu.Unlock()
	if ready && c.OnSourceStored != nil {
		c.OnSourceStored(source, stored)
	}
}

type legacyStream struct {
	eventType string
	payload   map[string]any
	data      []byte
	at        time.Time
}

// Convert reads one legacy JSONL source to its end and seals the segments
// that hold it.
func (c *LegacyConverter) Convert(source string, r io.Reader) error {
	c.Sources++
	c.w.source = source
	defer func() { c.w.source = "" }()
	streams := map[string]*legacyStream{}
	var order []string
	reader := bufio.NewReaderSize(r, 4<<20)
	for {
		if err := c.ctx.Err(); err != nil {
			return err
		}
		c.w.up.waitRoom(c.ctx)
		line, readErr := reader.ReadBytes('\n')
		c.SourceBytes += int64(len(line))
		if len(bytes.TrimSpace(line)) > 0 {
			c.convertLine(source, line, streams, &order)
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return readErr
		}
	}
	// Streams the source never summarized (a cut-off response).
	for _, id := range order {
		stream, ok := streams[id]
		if !ok {
			continue
		}
		c.Incomplete++
		sum := sha256.Sum256(stream.data)
		summary := copyPayload(stream.payload)
		summary["incomplete"] = true
		summary["body_chunked"] = true
		summary["bytes"] = len(stream.data)
		summary["sha256"] = hex.EncodeToString(sum[:])
		c.emitStream(id, stream, summary, stream.at)
	}
	for _, seg := range c.w.sessions {
		if seg.sources[source] {
			c.w.sealSession(seg, "source")
		}
	}
	for id, call := range c.w.calls {
		if call.row.LegacySource == source {
			c.w.closeCall(call, false)
			delete(c.w.calls, id)
		}
	}
	c.needBody, c.needResp = map[string][]string{}, map[string][]string{}
	// Every segment holding the source is sealed (and counted in pending)
	// by now; from here settle may report it, or this check does when all
	// of them already settled.
	c.mu.Lock()
	c.finished[source] = true
	ready := c.pending[source] == 0
	stored := !c.failed[source] && !c.mismatchBy[source]
	if ready {
		delete(c.finished, source)
		delete(c.pending, source)
	}
	c.mu.Unlock()
	if ready && c.OnSourceStored != nil {
		c.OnSourceStored(source, stored)
	}
	return nil
}

// Close commits what is left, writes the index, and waits for the uploads.
func (c *LegacyConverter) Close() error { return c.w.close() }

func (c *LegacyConverter) nextID() string {
	c.serial++
	return fmt.Sprintf("legacy-%d-%d", time.Now().UnixNano(), c.serial)
}

// pair takes the one call waiting in queue, or nothing when several are
// waiting: interleaved calls cannot be paired without guessing.
func pair(queues map[string][]string, key string) string {
	queue := queues[key]
	if len(queue) != 1 {
		if len(queue) > 1 {
			queues[key] = nil
		}
		return ""
	}
	queues[key] = nil
	return queue[0]
}

var legacyBodyFields = []string{"body_base64", "body_text", "chunk_sha256", "chunk_index", "chunk_bytes", "body_chunk", "offset"}

func copyPayload(payload map[string]any) map[string]any {
	out := make(map[string]any, len(payload))
	for key, value := range payload {
		out[key] = value
	}
	return out
}

func (c *LegacyConverter) convertLine(source string, line []byte, streams map[string]*legacyStream, order *[]string) {
	var event Event
	if err := json.Unmarshal(line, &event); err != nil {
		c.keepUnparsed(source, line)
		return
	}
	at, err := time.Parse(time.RFC3339Nano, event.Timestamp)
	if err != nil {
		at = time.Now()
	}
	payload := event.Payload
	if payload == nil {
		payload = map[string]any{}
	}
	switch {
	case boolField(payload, "body_chunk"):
		streamID := str(payload["stream_id"])
		encoded, _ := payload["body_base64"].(string)
		chunk, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			c.keepUnparsed(source, line)
			return
		}
		stream := streams[streamID]
		if stream == nil {
			base := copyPayload(payload)
			for _, field := range legacyBodyFields {
				delete(base, field)
			}
			stream = &legacyStream{eventType: trimChunkSuffix(event.Type), payload: base, at: at}
			streams[streamID] = stream
			*order = append(*order, streamID)
		}
		stream.data = append(stream.data, chunk...)
	case boolField(payload, "body_chunked"):
		streamID := str(payload["stream_id"])
		stream := streams[streamID]
		if stream == nil {
			stream = &legacyStream{eventType: event.Type, payload: copyPayload(payload), at: at}
		}
		delete(streams, streamID)
		sum := sha256.Sum256(stream.data)
		if hex.EncodeToString(sum[:]) != str(payload["sha256"]) {
			c.Mismatched++
			c.mu.Lock()
			c.mismatchBy[source] = true
			c.mu.Unlock()
		}
		c.emitStream(streamID, stream, copyPayload(payload), at)
	case payload["body_base64"] != nil:
		encoded, _ := payload["body_base64"].(string)
		body, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			c.keepUnparsed(source, line)
			return
		}
		base := copyPayload(payload)
		for _, field := range legacyBodyFields {
			delete(base, field)
		}
		delete(base, "bytes")
		delete(base, "sha256")
		if event.Type == "http_body" {
			c.assignRequestID(base)
		}
		c.w.process(&segmentItem{kind: itemBody, at: at, eventType: event.Type, payload: base, body: body, delta: str(payload["direction"]) == "client_to_upstream"})
	case event.Type == "subrouter_meta" && str(payload["request_id"]) == "" && str(payload["transport"]) != "websocket":
		key, _, _ := sessionKey(payload)
		id := c.nextID()
		payload["request_id"] = id
		c.needBody[key] = append(c.needBody[key], id)
		c.w.process(&segmentItem{kind: itemMeta, at: at, eventType: event.Type, payload: payload})
	default:
		c.w.process(&segmentItem{kind: itemEvent, at: at, eventType: event.Type, payload: payload})
	}
}

// emitStream writes a reassembled stream: a request body whole (so it can be
// delta-encoded), anything else as chunks plus its summary.
// assignRequestID pairs a legacy body with its call when exactly one call
// is waiting for it.
func (c *LegacyConverter) assignRequestID(payload map[string]any) {
	if str(payload["request_id"]) != "" {
		return
	}
	key, _, _ := sessionKey(payload)
	switch str(payload["direction"]) {
	case "client_to_upstream":
		if id := pair(c.needBody, key); id != "" {
			payload["request_id"] = id
			c.needResp[key] = append(c.needResp[key], id)
		}
	case "upstream_to_client":
		if id := pair(c.needResp, key); id != "" {
			payload["request_id"] = id
		}
	}
}

func (c *LegacyConverter) emitStream(streamID string, stream *legacyStream, summary map[string]any, end time.Time) {
	if stream.eventType == "http_body" {
		c.assignRequestID(stream.payload)
		if id := str(stream.payload["request_id"]); id != "" {
			summary["request_id"] = id
		}
	}
	if str(stream.payload["direction"]) == "client_to_upstream" && !boolField(summary, "incomplete") {
		payload := copyPayload(stream.payload)
		payload["stream_id"] = streamID
		delete(payload, "chunks")
		c.w.process(&segmentItem{kind: itemBody, at: stream.at, eventType: stream.eventType, payload: payload, body: stream.data, delta: true})
		return
	}
	payload := copyPayload(stream.payload)
	payload["stream_id"] = streamID
	if decoded, ok := gunzipComplete(stream.data); ok {
		// Before the proxy stopped forwarding Accept-Encoding, upstreams
		// answered with gzip, which the old spool kept as opaque base64.
		// Store the text the client decoded, and the wire digest.
		wireSum := sha256.Sum256(stream.data)
		for _, target := range []map[string]any{payload, summary} {
			target["content_encoding"] = "gzip"
			target["wire_bytes"] = len(stream.data)
			target["wire_sha256"] = hex.EncodeToString(wireSum[:])
		}
		sum := sha256.Sum256(decoded)
		summary["bytes"] = len(decoded)
		summary["sha256"] = hex.EncodeToString(sum[:])
		stream.data = decoded
	}
	if len(stream.data) > 0 {
		c.w.process(&segmentItem{kind: itemChunk, at: stream.at, eventType: stream.eventType, payload: payload, body: stream.data, streamID: streamID})
	}
	summary["stream_id"] = streamID
	c.w.process(&segmentItem{kind: itemSummary, at: end, eventType: stream.eventType, payload: summary, streamID: streamID})
}

func trimChunkSuffix(eventType string) string {
	const suffix = "_chunk"
	if len(eventType) > len(suffix) && eventType[len(eventType)-len(suffix):] == suffix {
		return eventType[:len(eventType)-len(suffix)]
	}
	return eventType
}

// gunzipComplete decodes data when it is a complete gzip stream.
func gunzipComplete(data []byte) ([]byte, bool) {
	if len(data) < 18 || data[0] != 0x1f || data[1] != 0x8b {
		return nil, false
	}
	reader, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, false
	}
	defer reader.Close()
	decoded, err := io.ReadAll(io.LimitReader(reader, requestBodyDecodeLimit+1))
	if err != nil || len(decoded) > requestBodyDecodeLimit {
		return nil, false
	}
	return decoded, true
}

// keepUnparsed stores a line the converter cannot read, verbatim (as text
// when it is UTF-8), in the source's session segment, and marks the source
// so its file is kept.
func (c *LegacyConverter) keepUnparsed(source string, line []byte) {
	c.Mismatched++
	c.mu.Lock()
	c.mismatchBy[source] = true
	c.mu.Unlock()
	payload := map[string]any{"agent_type": "legacy", "session_id": source}
	putBody(payload, bytes.TrimRight(line, "\n"))
	c.w.process(&segmentItem{kind: itemEvent, at: time.Now(), eventType: "legacy_unparsed_line", payload: payload})
}
