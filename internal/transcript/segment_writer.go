package transcript

import (
	"bufio"
	"bytes"
	"compress/flate"
	"compress/gzip"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/klauspost/compress/zstd"
)

// WhoIsFunc names the machine and user behind a client address. It may block
// (the tailnet resolver shells out to `tailscale whois`), so the segment
// writer calls it from its own goroutine and caches the answer per address;
// a proxied request never waits on it.
type WhoIsFunc func(ctx context.Context, remoteAddr string) (node, login string, ok bool)

// SegmentConfig configures a segment-mode Recorder.
type SegmentConfig struct {
	// Dir is the spool root. Live, sealed and compressed segments live under
	// Dir/segments.
	Dir string
	// Host labels the machine that recorded the segment; it is part of the
	// segment id and the blob path.
	Host string
	// MaxSegmentBytes, MaxSegmentAge and IdleSeal decide when the live
	// segment is sealed.
	MaxSegmentBytes int64
	MaxSegmentAge   time.Duration
	IdleSeal        time.Duration
	// QueueBytes caps bytes waiting for the writer goroutine. Events past the
	// cap are dropped and counted, never waited for.
	QueueBytes int64
	// DeltaBaseBytes caps the earlier bodies kept in memory as delta bases.
	DeltaBaseBytes int64
	// MaxLocalBytes caps the spool (live, sealed and compressed segments not
	// yet uploaded). Past it new events are dropped and counted. Zero means
	// no cap.
	MaxLocalBytes int64
	// Compression settings for sealed segments.
	CompressLevel zstd.EncoderLevel
	WindowSize    int
	// Uploader receives compressed segments. Nil keeps them in the spool.
	Uploader SegmentUploader
	WhoIs    WhoIsFunc
	Logger   *slog.Logger
	Now      func() time.Time
	// Version is recorded in each segment header.
	Version string
	// BlobPrefix, when set, replaces the date and host partition in the blob
	// name (used for converted legacy transcripts).
	BlobPrefix string
}

const (
	defaultSegmentMaxBytes   = 256 << 20
	defaultSegmentMaxAge     = time.Hour
	defaultSegmentIdleSeal   = 10 * time.Minute
	defaultSegmentQueueBytes = 256 << 20
	defaultDeltaBaseBytes    = 256 << 20
	// DefaultSegmentWindowSize is the zstd window for sealed segments. 128 MiB
	// is the largest window the stock zstd CLI decodes without --long or
	// --memory, so `zstd -d` works on a segment as is.
	DefaultSegmentWindowSize = 128 << 20
	// requestBodyDecodeLimit bounds how far a compressed request body is
	// expanded for recording.
	requestBodyDecodeLimit = 256 << 20
	// chunkCoalesceBytes is how much of one response stream is gathered into
	// a single chunk event.
	chunkCoalesceBytes = 1 << 20
	whoisPositiveTTL   = 15 * time.Minute
	whoisNegativeTTL   = time.Minute
	whoisTimeout       = 2 * time.Second
)

// rawSegmentExt names live and sealed (uncompressed) segments. It is not
// .jsonl on purpose: the jsonl-format syncers of an older worker sharing the
// directory upload and prune every *.jsonl file they find.
const rawSegmentExt = ".segment"

// requestBodyZstd decodes zstd request bodies; DecodeAll is safe for
// concurrent use and the memory cap bounds a decompression bomb.
var requestBodyZstd, _ = zstd.NewReader(nil, zstd.WithDecoderConcurrency(1), zstd.WithDecoderMaxMemory(requestBodyDecodeLimit))

// UnknownAttribution is recorded when a client cannot be named.
const UnknownAttribution = "unknown"

type segmentItemKind int

const (
	itemEvent segmentItemKind = iota
	itemMeta
	itemBody
	itemChunk
	itemSummary
	itemSync
)

type segmentItem struct {
	kind      segmentItemKind
	at        time.Time
	eventType string
	payload   map[string]any
	body      []byte
	// delta asks for the body to be stored against earlier bodies of its
	// chain when that is smaller.
	delta bool
	// contentEncoding is the wire encoding of body; the writer decodes it.
	contentEncoding string
	remoteAddr      string
	streamID        string
	offset          int64
	done            chan struct{}
	seal            bool
}

func (i *segmentItem) size() int64 { return int64(len(i.body)) + 512 }

// segmentWriter turns recorder calls into one JSONL stream per process,
// rolled into sealed segments.
type segmentWriter struct {
	cfg   SegmentConfig
	root  string
	live  string
	store *segmentStore

	mu      sync.Mutex
	queue   []*segmentItem
	queued  int64
	closed  bool
	kick    chan struct{}
	stopped chan struct{}

	droppedQueue atomic.Int64
	droppedSpool atomic.Int64
	writeErrors  atomic.Int64
	lastDropLog  atomic.Int64

	// The fields below belong to the writer goroutine.
	file          *os.File
	buf           *bufio.Writer
	segID         string
	segStarted    time.Time
	segLastWrite  time.Time
	segBytes      int64
	segEvents     int64
	segBodyBytes  int64
	segSeq        int64
	encodeBuf     bytes.Buffer
	deltas        map[string]*deltaChain
	deltaBytes    int64
	deltaUse      int64
	pending       map[string]*pendingStream
	whois         map[string]whoisEntry
	processSerial int

	// offline writers are driven by their caller (legacy conversion) and
	// hand each sealed segment to onSealed instead of the background store.
	offline  bool
	onSealed func(path string) error
	lastErr  error
}

func (w *segmentWriter) maybeRollSize() {
	if w.file != nil && w.segBytes >= w.cfg.MaxSegmentBytes {
		w.seal("size")
	}
}

type deltaChain struct {
	bodies  []deltaBase
	lastUse int64
}

type deltaBase struct {
	seq  int64
	body []byte
}

type pendingStream struct {
	eventType string
	payload   map[string]any
	offset    int64
	data      []byte
	at        time.Time
}

type whoisEntry struct {
	node, login string
	expires     time.Time
}

func withSegmentDefaults(cfg SegmentConfig) SegmentConfig {
	if cfg.MaxSegmentBytes <= 0 {
		cfg.MaxSegmentBytes = defaultSegmentMaxBytes
	}
	if cfg.MaxSegmentAge <= 0 {
		cfg.MaxSegmentAge = defaultSegmentMaxAge
	}
	if cfg.IdleSeal <= 0 {
		cfg.IdleSeal = defaultSegmentIdleSeal
	}
	if cfg.QueueBytes <= 0 {
		cfg.QueueBytes = defaultSegmentQueueBytes
	}
	if cfg.DeltaBaseBytes <= 0 {
		cfg.DeltaBaseBytes = defaultDeltaBaseBytes
	}
	if cfg.WindowSize <= 0 {
		cfg.WindowSize = DefaultSegmentWindowSize
	}
	if cfg.CompressLevel == 0 {
		cfg.CompressLevel = zstd.SpeedBestCompression
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if strings.TrimSpace(cfg.Host) == "" {
		host, _ := os.Hostname()
		cfg.Host = host
	}
	cfg.Host = safeFilename(strings.Split(cfg.Host, ".")[0])
	return cfg
}

func newSegmentWriter(cfg SegmentConfig) (*segmentWriter, error) {
	if strings.TrimSpace(cfg.Dir) == "" {
		return nil, errors.New("segment spool directory is empty")
	}
	cfg = withSegmentDefaults(cfg)
	root := filepath.Join(cfg.Dir, "segments")
	live := filepath.Join(root, "live", fmt.Sprintf("%d-%d", os.Getpid(), cfg.Now().UnixNano()))
	for _, dir := range []string{live, filepath.Join(root, "sealed"), filepath.Join(root, "upload")} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, err
		}
	}
	if err := lockLiveDir(live); err != nil {
		return nil, err
	}
	w := &segmentWriter{
		cfg:     cfg,
		root:    root,
		live:    live,
		kick:    make(chan struct{}, 1),
		stopped: make(chan struct{}),
		deltas:  map[string]*deltaChain{},
		pending: map[string]*pendingStream{},
		whois:   map[string]whoisEntry{},
	}
	w.store = newSegmentStore(w)
	go w.run()
	go w.store.run()
	return w, nil
}

// enqueue hands an item to the writer goroutine without waiting for it.
func (w *segmentWriter) enqueue(item *segmentItem) {
	if item.at.IsZero() {
		item.at = w.cfg.Now()
	}
	if w.store.full.Load() {
		w.drop(&w.droppedSpool, "transcript spool is full")
		return
	}
	size := item.size()
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		w.drop(&w.droppedQueue, "transcript writer is closed")
		return
	}
	if w.queued+size > w.cfg.QueueBytes {
		w.mu.Unlock()
		w.drop(&w.droppedQueue, "transcript writer queue is full")
		return
	}
	w.queue = append(w.queue, item)
	w.queued += size
	w.mu.Unlock()
	select {
	case w.kick <- struct{}{}:
	default:
	}
}

func (w *segmentWriter) drop(counter *atomic.Int64, reason string) {
	total := counter.Add(1)
	now := time.Now().UnixNano()
	last := w.lastDropLog.Load()
	if now-last < int64(dropLogInterval) || !w.lastDropLog.CompareAndSwap(last, now) {
		return
	}
	w.cfg.Logger.Warn("transcript events dropped", "reason", reason, "dropped_total", total,
		"dropped_queue_full", w.droppedQueue.Load(), "dropped_spool_full", w.droppedSpool.Load())
}

// sync waits until everything enqueued so far is written to the live
// segment, and seals it when seal is set.
func (w *segmentWriter) sync(seal bool) {
	done := make(chan struct{})
	item := &segmentItem{kind: itemSync, done: done, seal: seal}
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return
	}
	w.queue = append(w.queue, item)
	w.mu.Unlock()
	select {
	case w.kick <- struct{}{}:
	default:
	}
	select {
	case <-done:
	case <-w.stopped:
	}
}

func (w *segmentWriter) close() error {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		<-w.stopped
		return nil
	}
	w.closed = true
	w.mu.Unlock()
	select {
	case w.kick <- struct{}{}:
	default:
	}
	<-w.stopped
	w.store.stop()
	return nil
}

func (w *segmentWriter) run() {
	defer close(w.stopped)
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-w.kick:
		case <-ticker.C:
		}
		for {
			w.mu.Lock()
			batch := w.queue
			w.queue = nil
			w.queued = 0
			closed := w.closed
			w.mu.Unlock()
			if len(batch) == 0 {
				if closed {
					w.seal("close")
					return
				}
				break
			}
			for _, item := range batch {
				w.process(item)
			}
			w.flushBuffer()
		}
		w.maybeRoll()
	}
}

func (w *segmentWriter) flushBuffer() {
	if w.buf == nil {
		return
	}
	if err := w.buf.Flush(); err != nil {
		w.writeErrors.Add(1)
		w.cfg.Logger.Warn("transcript segment write failed", "segment", w.segID, "error", err)
	}
}

func (w *segmentWriter) maybeRoll() {
	if w.file == nil {
		return
	}
	now := w.cfg.Now()
	switch {
	case w.segBytes >= w.cfg.MaxSegmentBytes:
		w.seal("size")
	case now.Sub(w.segStarted) >= w.cfg.MaxSegmentAge:
		w.seal("age")
	case now.Sub(w.segLastWrite) >= w.cfg.IdleSeal:
		w.seal("idle")
	}
}

func (w *segmentWriter) process(item *segmentItem) {
	switch item.kind {
	case itemSync:
		w.flushBuffer()
		if item.seal {
			w.seal("sync")
		}
		close(item.done)
		return
	case itemChunk:
		w.addChunk(item)
		return
	case itemSummary:
		w.flushStream(item.streamID, true)
		w.writeEvent(item.at, item.eventType, item.payload)
		return
	case itemMeta:
		w.attribute(item)
	case itemBody:
		// Open first: a body's sequence number and delta chain belong to the
		// segment it is written to.
		if w.file == nil {
			if err := w.openSegment(item.at); err != nil {
				w.writeErrors.Add(1)
				w.cfg.Logger.Warn("transcript segment open failed", "error", err)
				return
			}
		}
		w.encodeBody(item)
	}
	w.writeEvent(item.at, item.eventType, item.payload)
	if w.segBytes >= w.cfg.MaxSegmentBytes {
		w.seal("size")
	}
}

// attribute names the client machine. The answer is cached per address, so
// the tailnet is asked once per client, not once per request.
func (w *segmentWriter) attribute(item *segmentItem) {
	node, login := UnknownAttribution, UnknownAttribution
	host := addressHost(item.remoteAddr)
	if host != "" {
		item.payload["client_ip"] = host
	}
	if host != "" && w.cfg.WhoIs != nil {
		now := w.cfg.Now()
		entry, cached := w.whois[host]
		if !cached || now.After(entry.expires) {
			entry = whoisEntry{node: UnknownAttribution, login: UnknownAttribution, expires: now.Add(whoisNegativeTTL)}
			ctx, cancel := context.WithTimeout(context.Background(), whoisTimeout)
			if gotNode, gotLogin, ok := w.cfg.WhoIs(ctx, item.remoteAddr); ok {
				entry.node = nonEmpty(gotNode)
				entry.login = nonEmpty(gotLogin)
				entry.expires = now.Add(whoisPositiveTTL)
			}
			cancel()
			if len(w.whois) > 4096 {
				w.whois = map[string]whoisEntry{}
			}
			w.whois[host] = entry
		}
		node, login = entry.node, entry.login
	}
	item.payload["client_node"] = node
	item.payload["client_user"] = login
}

func nonEmpty(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return UnknownAttribution
	}
	return value
}

func addressHost(remoteAddr string) string {
	remoteAddr = strings.TrimSpace(remoteAddr)
	if remoteAddr == "" {
		return ""
	}
	if host, _, err := net.SplitHostPort(remoteAddr); err == nil {
		return strings.Trim(host, "[]")
	}
	return strings.Trim(remoteAddr, "[]")
}

// encodeBody decodes a compressed body, then stores it whole or as a delta
// against an earlier body of its chain, whichever is smaller.
func (w *segmentWriter) encodeBody(item *segmentItem) {
	body := item.body
	payload := item.payload
	if encoding := strings.ToLower(strings.TrimSpace(item.contentEncoding)); encoding != "" && encoding != "identity" {
		payload["content_encoding"] = encoding
		wireSum := sha256.Sum256(body)
		payload["wire_bytes"] = len(body)
		payload["wire_sha256"] = hex.EncodeToString(wireSum[:])
		decoded, err := decodeContentEncoding(body, encoding)
		if err != nil {
			// Keep the exact wire bytes; nothing is lost, it is just not
			// readable as text.
			payload["decode_error"] = err.Error()
			payload["body_is_wire"] = true
		} else {
			body = decoded
		}
	}
	sum := sha256.Sum256(body)
	payload["bytes"] = len(body)
	payload["sha256"] = hex.EncodeToString(sum[:])
	w.segSeq++
	seq := w.segSeq
	payload["body_seq"] = seq
	w.segBodyBytes += int64(len(body))

	if item.delta {
		key := deltaKeyFor(item.eventType, payload)
		chain := w.deltas[key]
		if chain != nil {
			if ops, baseSeq, ok := w.bestDelta(chain, body); ok {
				payload["delta"] = map[string]any{"base": baseSeq, "ops": marshalDeltaOps(ops)}
			}
		}
		if _, ok := payload["delta"]; !ok {
			putBody(payload, body)
		}
		w.keepBase(key, seq, body)
		return
	}
	putBody(payload, body)
}

// bestDelta picks the base sharing the longest prefix plus suffix with body
// and encodes against it. A delta is used only when copies cover most of the
// body; otherwise the whole body is cheaper to store and to read.
func (w *segmentWriter) bestDelta(chain *deltaChain, body []byte) ([]deltaOp, int64, bool) {
	bestScore := -1
	var best deltaBase
	for _, base := range chain.bodies {
		prefix := commonPrefixLen(base.body, body)
		score := prefix + commonSuffixLen(base.body[prefix:], body[prefix:])
		if score > bestScore {
			bestScore, best = score, base
		}
	}
	if best.body == nil || len(body) < deltaMinMatch {
		return nil, 0, false
	}
	ops := encodeDelta(best.body, body)
	if deltaSavings(ops)*2 < len(body) {
		return nil, 0, false
	}
	return ops, best.seq, true
}

func (w *segmentWriter) keepBase(key string, seq int64, body []byte) {
	chain := w.deltas[key]
	if chain == nil {
		chain = &deltaChain{}
		w.deltas[key] = chain
	}
	chain.bodies = append(chain.bodies, deltaBase{seq: seq, body: body})
	w.deltaBytes += int64(len(body))
	if len(chain.bodies) > deltaBasesPerKey {
		w.deltaBytes -= int64(len(chain.bodies[0].body))
		chain.bodies = append(chain.bodies[:0:0], chain.bodies[1:]...)
	}
	w.deltaUse++
	chain.lastUse = w.deltaUse
	for w.deltaBytes > w.cfg.DeltaBaseBytes && len(w.deltas) > 1 {
		w.evictOldestChain(key)
	}
}

// evictOldestChain drops the least recently used chain other than keep. A
// later body of an evicted chain is stored whole, so eviction only costs
// space, never correctness.
func (w *segmentWriter) evictOldestChain(keep string) {
	var oldestKey string
	var oldest int64 = -1
	for key, chain := range w.deltas {
		if key == keep {
			continue
		}
		if oldest < 0 || chain.lastUse < oldest {
			oldestKey, oldest = key, chain.lastUse
		}
	}
	if oldestKey == "" {
		return
	}
	for _, base := range w.deltas[oldestKey].bodies {
		w.deltaBytes -= int64(len(base.body))
	}
	delete(w.deltas, oldestKey)
}

func decodeContentEncoding(body []byte, encoding string) ([]byte, error) {
	var reader io.Reader
	switch encoding {
	case "zstd":
		decoded, err := requestBodyZstd.DecodeAll(body, nil)
		if err != nil {
			return nil, err
		}
		return decoded, nil
	case "gzip", "x-gzip":
		gz, err := gzip.NewReader(bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		defer gz.Close()
		reader = gz
	case "deflate":
		reader = flate.NewReader(bytes.NewReader(body))
	default:
		return nil, fmt.Errorf("unsupported content encoding %q", encoding)
	}
	decoded, err := io.ReadAll(io.LimitReader(reader, requestBodyDecodeLimit+1))
	if err != nil {
		return nil, err
	}
	if len(decoded) > requestBodyDecodeLimit {
		return nil, fmt.Errorf("decoded body exceeds %d bytes", requestBodyDecodeLimit)
	}
	return decoded, nil
}

// addChunk gathers one stream's chunks so a response is a few large text
// events instead of thousands of tiny ones.
func (w *segmentWriter) addChunk(item *segmentItem) {
	stream := w.pending[item.streamID]
	if stream == nil {
		stream = &pendingStream{eventType: item.eventType, payload: item.payload, offset: item.offset, at: item.at}
		w.pending[item.streamID] = stream
	}
	stream.data = append(stream.data, item.body...)
	w.segBodyBytes += int64(len(item.body))
	if len(stream.data) >= chunkCoalesceBytes {
		w.flushStream(item.streamID, false)
	}
}

// flushStream writes what a stream has gathered. Unless final, a trailing
// partial UTF-8 rune stays pending so the chunk is stored as text.
func (w *segmentWriter) flushStream(streamID string, final bool) {
	stream := w.pending[streamID]
	if stream == nil {
		return
	}
	data := stream.data
	cut := len(data)
	if !final {
		cut = utf8SafeCut(data)
	}
	if cut > 0 {
		payload := make(map[string]any, len(stream.payload)+4)
		for key, value := range stream.payload {
			payload[key] = value
		}
		payload["body_chunk"] = true
		payload["offset"] = stream.offset
		payload["chunk_bytes"] = cut
		putBody(payload, data[:cut])
		w.writeEvent(stream.at, stream.eventType+"_chunk", payload)
		stream.offset += int64(cut)
	}
	if final || cut == len(data) {
		if final {
			delete(w.pending, streamID)
			return
		}
		stream.data = stream.data[:0]
		return
	}
	rest := append([]byte(nil), data[cut:]...)
	stream.data = rest
	stream.at = w.cfg.Now()
}

// utf8SafeCut returns the length of data without a trailing incomplete rune.
func utf8SafeCut(data []byte) int {
	for back := 1; back <= utf8.UTFMax && back <= len(data); back++ {
		start := len(data) - back
		if utf8.RuneStart(data[start]) {
			if utf8.FullRune(data[start:]) {
				return len(data)
			}
			return start
		}
	}
	return len(data)
}

func (w *segmentWriter) writeEvent(at time.Time, eventType string, payload map[string]any) {
	if w.file == nil {
		if err := w.openSegment(at); err != nil {
			w.writeErrors.Add(1)
			w.cfg.Logger.Warn("transcript segment open failed", "error", err)
			return
		}
	}
	w.encodeBuf.Reset()
	encoder := json.NewEncoder(&w.encodeBuf)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(Event{Timestamp: at.UTC().Format(time.RFC3339Nano), Type: eventType, Payload: payload}); err != nil {
		w.writeErrors.Add(1)
		return
	}
	n, err := w.buf.Write(w.encodeBuf.Bytes())
	w.segBytes += int64(n)
	if err != nil {
		w.writeErrors.Add(1)
		return
	}
	w.segEvents++
	w.segLastWrite = w.cfg.Now()
}

func (w *segmentWriter) openSegment(at time.Time) error {
	now := w.cfg.Now()
	w.processSerial++
	var random [3]byte
	_, _ = rand.Read(random[:])
	w.segID = fmt.Sprintf("%s-%s-%d-%d-%s", now.UTC().Format("20060102T150405Z"), w.cfg.Host, os.Getpid(), w.processSerial, hex.EncodeToString(random[:]))
	if err := os.MkdirAll(w.live, 0o700); err != nil {
		return err
	}
	path := filepath.Join(w.live, w.segID+rawSegmentExt)
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	w.file = file
	w.buf = bufio.NewWriterSize(file, 1<<20)
	w.segStarted = now
	w.segLastWrite = now
	w.segBytes, w.segEvents, w.segBodyBytes, w.segSeq = 0, 0, 0, 0
	w.writeEvent(at, "segment_header", map[string]any{
		"format":     SegmentFormat,
		"segment_id": w.segID,
		"host":       w.cfg.Host,
		"pid":        os.Getpid(),
		"version":    w.cfg.Version,
		"started_at": now.UTC().Format(time.RFC3339Nano),
	})
	return nil
}

// seal ends the live segment and hands it to the store. Delta chains end with
// it, so every sealed segment decodes on its own.
func (w *segmentWriter) seal(reason string) {
	if w.file == nil {
		w.resetSegmentState()
		return
	}
	for streamID := range w.pending {
		w.flushStream(streamID, true)
	}
	w.writeEvent(w.cfg.Now(), "segment_footer", map[string]any{
		"segment_id":         w.segID,
		"sealed_at":          w.cfg.Now().UTC().Format(time.RFC3339Nano),
		"seal_reason":        reason,
		"events":             w.segEvents,
		"body_bytes":         w.segBodyBytes,
		"dropped_queue_full": w.droppedQueue.Load(),
		"dropped_spool_full": w.droppedSpool.Load(),
		"write_errors":       w.writeErrors.Load(),
	})
	w.flushBuffer()
	if err := w.file.Sync(); err != nil {
		w.writeErrors.Add(1)
	}
	_ = w.file.Close()
	livePath := filepath.Join(w.live, w.segID+rawSegmentExt)
	sealedPath := filepath.Join(w.root, "sealed", w.segID+rawSegmentExt)
	_ = os.MkdirAll(filepath.Dir(sealedPath), 0o700)
	if err := os.Rename(livePath, sealedPath); err != nil {
		w.writeErrors.Add(1)
		w.cfg.Logger.Warn("transcript segment seal failed", "segment", w.segID, "error", err)
	}
	w.file, w.buf = nil, nil
	w.resetSegmentState()
	if w.offline {
		if w.onSealed != nil {
			if err := w.onSealed(sealedPath); err != nil && w.lastErr == nil {
				w.lastErr = err
			}
		}
		return
	}
	w.store.notify()
}

func (w *segmentWriter) resetSegmentState() {
	w.segSeq = 0
	w.deltas = map[string]*deltaChain{}
	w.deltaBytes = 0
	w.pending = map[string]*pendingStream{}
}

// SegmentStats is what a segment-mode recorder reports about itself.
type SegmentStats struct {
	DroppedQueueFull int64 `json:"dropped_queue_full"`
	DroppedSpoolFull int64 `json:"dropped_spool_full"`
	WriteErrors      int64 `json:"write_errors"`
	UploadedSegments int64 `json:"uploaded_segments"`
	UploadedBytes    int64 `json:"uploaded_bytes"`
	UploadErrors     int64 `json:"upload_errors"`
	SpoolBytes       int64 `json:"spool_bytes"`
	SpoolFull        bool  `json:"spool_full"`
}

func (w *segmentWriter) stats() SegmentStats {
	return SegmentStats{
		DroppedQueueFull: w.droppedQueue.Load(),
		DroppedSpoolFull: w.droppedSpool.Load(),
		WriteErrors:      w.writeErrors.Load(),
		UploadedSegments: w.store.uploaded.Load(),
		UploadedBytes:    w.store.uploadedBytes.Load(),
		UploadErrors:     w.store.uploadErrors.Load(),
		SpoolBytes:       w.store.spoolBytes.Load(),
		SpoolFull:        w.store.full.Load(),
	}
}
