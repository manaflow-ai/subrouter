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
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

// LegacyConverter rewrites raw per-session transcript JSONL (base64 body
// chunks, as the append-blob spool holds) into the segment format: bodies
// reassembled, request bodies delta-encoded, text stored as text, sealed
// segments compressed and uploaded like live ones.
type LegacyConverter struct {
	w   *segmentWriter
	ctx context.Context

	// SealedSources lists, per sealed segment, the sources that ended in it.
	// A source is safe to mark converted once the segment holding its last
	// event is stored.
	OnSealed func(info SegmentInfo, completedSources []string) error

	completed []string
	// SourceBytes and Sources count what was read.
	SourceBytes int64
	Sources     int
	Mismatched  int64
	Incomplete  int64
	seg         SegmentConfig
	sealedInfos []SegmentInfo
}

// NewLegacyConverter writes segments under cfg.Dir; cfg.Uploader may be nil
// to keep them local. Segment roll limits apply as in live recording.
func NewLegacyConverter(ctx context.Context, cfg SegmentConfig) (*LegacyConverter, error) {
	w, err := newOfflineSegmentWriter(cfg)
	if err != nil {
		return nil, err
	}
	converter := &LegacyConverter{w: w, ctx: ctx, seg: cfg}
	w.onSealed = converter.sealed
	return converter, nil
}

// Segments reports what was produced so far.
func (c *LegacyConverter) Segments() []SegmentInfo { return c.sealedInfos }

func (c *LegacyConverter) sealed(path string) error {
	info, compressedPath, err := c.w.store.compressSync(c.ctx, path)
	if err != nil {
		return err
	}
	if c.w.cfg.Uploader != nil {
		if err := c.w.cfg.Uploader.UploadSegment(c.ctx, compressedPath, info); err != nil {
			return err
		}
		_ = os.Remove(compressedPath)
		_ = os.Remove(compressedPath + ".json")
	}
	c.sealedInfos = append(c.sealedInfos, info)
	completed := c.completed
	c.completed = nil
	if c.OnSealed != nil {
		return c.OnSealed(info, completed)
	}
	return nil
}

type legacyStream struct {
	eventType string
	payload   map[string]any
	data      []byte
	at        time.Time
}

// Convert reads one legacy JSONL source to its end.
func (c *LegacyConverter) Convert(source string, r io.Reader) error {
	c.Sources++
	streams := map[string]*legacyStream{}
	var order []string
	c.w.process(&segmentItem{kind: itemEvent, at: time.Now(), eventType: "legacy_source", payload: map[string]any{"source": source}})
	reader := bufio.NewReaderSize(r, 4<<20)
	for {
		if err := c.ctx.Err(); err != nil {
			return err
		}
		line, readErr := reader.ReadBytes('\n')
		c.SourceBytes += int64(len(line))
		if len(bytes.TrimSpace(line)) > 0 {
			c.convertLine(line, streams, &order)
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
		c.emitStream(id, stream, summary)
	}
	c.completed = append(c.completed, source)
	c.w.flushBuffer()
	c.w.maybeRollSize()
	return c.w.lastErr
}

// Close seals the last segment.
func (c *LegacyConverter) Close() error {
	c.w.seal("close")
	c.w.flushBuffer()
	return c.w.lastErr
}

var legacyBodyFields = []string{"body_base64", "body_text", "chunk_sha256", "chunk_index", "chunk_bytes", "body_chunk", "offset"}

func copyPayload(payload map[string]any) map[string]any {
	out := make(map[string]any, len(payload))
	for key, value := range payload {
		out[key] = value
	}
	return out
}

func (c *LegacyConverter) convertLine(line []byte, streams map[string]*legacyStream, order *[]string) {
	var event Event
	if err := json.Unmarshal(line, &event); err != nil {
		// Keep an unparseable line verbatim rather than lose it.
		c.w.process(&segmentItem{kind: itemEvent, at: time.Now(), eventType: "legacy_unparsed_line", payload: map[string]any{"line": string(bytes.TrimRight(line, "\n"))}})
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
			c.Mismatched++
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
		}
		c.emitStream(streamID, stream, copyPayload(payload))
	case payload["body_base64"] != nil:
		encoded, _ := payload["body_base64"].(string)
		body, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			c.Mismatched++
			return
		}
		base := copyPayload(payload)
		for _, field := range legacyBodyFields {
			delete(base, field)
		}
		delete(base, "bytes")
		delete(base, "sha256")
		c.w.process(&segmentItem{kind: itemBody, at: at, eventType: event.Type, payload: base, body: body, delta: str(payload["direction"]) == "client_to_upstream"})
	default:
		c.w.process(&segmentItem{kind: itemEvent, at: at, eventType: event.Type, payload: payload})
	}
}

// emitStream writes a reassembled stream: a request body whole (so it can be
// delta-encoded), anything else as chunks plus its summary.
func (c *LegacyConverter) emitStream(streamID string, stream *legacyStream, summary map[string]any) {
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
	c.w.process(&segmentItem{kind: itemSummary, at: stream.at, eventType: stream.eventType, payload: summary, streamID: streamID})
}

func trimChunkSuffix(eventType string) string {
	const suffix = "_chunk"
	if len(eventType) > len(suffix) && eventType[len(eventType)-len(suffix):] == suffix {
		return eventType[:len(eventType)-len(suffix)]
	}
	return eventType
}

// newOfflineSegmentWriter is a writer driven synchronously by its caller,
// without the queue and background goroutines of live recording.
func newOfflineSegmentWriter(cfg SegmentConfig) (*segmentWriter, error) {
	if cfg.Dir == "" {
		return nil, errors.New("segment directory is empty")
	}
	cfg = withSegmentDefaults(cfg)
	root := filepath.Join(cfg.Dir, "segments")
	live := filepath.Join(root, "live", fmt.Sprintf("offline-%d-%d", os.Getpid(), cfg.Now().UnixNano()))
	for _, dir := range []string{live, filepath.Join(root, "sealed"), filepath.Join(root, "upload")} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, err
		}
	}
	w := &segmentWriter{
		cfg: cfg, root: root, live: live, offline: true,
		kick: make(chan struct{}, 1), stopped: make(chan struct{}),
		deltas: map[string]*deltaChain{}, pending: map[string]*pendingStream{}, whois: map[string]whoisEntry{},
	}
	w.store = newSegmentStore(w)
	return w, nil
}

// compressSync compresses one sealed file in the caller's goroutine.
func (s *segmentStore) compressSync(ctx context.Context, sealedPath string) (SegmentInfo, string, error) {
	segID := filepath.Base(sealedPath)
	segID = segID[:len(segID)-len(".jsonl")]
	target := filepath.Join(s.w.root, "upload", segID+".jsonl.zst")
	info, err := compressSegmentFile(ctx, sealedPath, target, s.w.cfg.CompressLevel, s.w.cfg.WindowSize)
	if err != nil {
		return SegmentInfo{}, "", err
	}
	info.SegmentID = segID
	info.Host = s.w.cfg.Host
	if s.w.cfg.BlobPrefix != "" {
		info.BlobName = s.w.cfg.BlobPrefix + "/" + segID + ".jsonl.zst"
	}
	if err := writeSidecar(target, info); err != nil {
		return SegmentInfo{}, "", err
	}
	if err := os.Remove(sealedPath); err != nil {
		return SegmentInfo{}, "", err
	}
	return info, target, nil
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
