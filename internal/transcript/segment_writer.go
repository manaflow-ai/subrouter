package transcript

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/klauspost/compress/zstd"
)

// WhoIsFunc names the machine and user behind a client address. It may block
// (the tailnet resolver shells out to `tailscale whois`), so the writer calls
// it from its own goroutine and caches the answer per address; a proxied
// request never waits on it.
type WhoIsFunc func(ctx context.Context, remoteAddr string) (node, login string, ok bool)

// SegmentConfig configures a recorder that streams transcripts to blob
// storage. Nothing is written to local disk.
//
// Each agent session gets its own segment blob: a sequence of independent
// zstd frames (each a few MiB of events) followed by a seek table, uploaded
// as staged blocks while it grows and committed when it seals. Every model
// call also gets one row in an index blob, written every few minutes, that
// names the byte range of its request and its response.
type SegmentConfig struct {
	Uploader BlockUploader
	// Host labels the recording machine in blob names and rows.
	Host string
	// FrameBytes is the raw size at which a frame is compressed and closed.
	FrameBytes int
	// BlockBytes is the compressed size staged per Put Block.
	BlockBytes int
	// MaxSegmentBytes (compressed), MaxSegmentAge and IdleSeal decide when a
	// session's segment is committed.
	MaxSegmentBytes int64
	MaxSegmentAge   time.Duration
	IdleSeal        time.Duration
	// IndexFlush is how often index rows are written out.
	IndexFlush time.Duration
	// QueueBytes caps events waiting for the writer goroutine.
	QueueBytes int64
	// UploadBacklogBytes caps compressed bytes waiting to upload. Past it new
	// events are dropped and counted.
	UploadBacklogBytes int64
	// LiveBytes caps open frames and delta bases across sessions; past it the
	// least recently active sessions are sealed early.
	LiveBytes int64
	// CompressLevel is the zstd level for frames.
	CompressLevel zstd.EncoderLevel
	// UploadWorkers is the number of concurrent block uploads.
	UploadWorkers int
	WhoIs         WhoIsFunc
	Logger        *slog.Logger
	Now           func() time.Time
	Version       string
	// BodiesPrefix and IndexPrefix are the blob name prefixes.
	BodiesPrefix string
	IndexPrefix  string
	// CloseTimeout bounds how long Close waits for the last uploads.
	CloseTimeout time.Duration
}

// BlockUploader is the blob storage the recorder writes to. Names are never
// reused: the writer's credential can create and write but not read or
// delete.
type BlockUploader interface {
	PutBlock(ctx context.Context, blob, blockID string, data []byte) error
	PutBlockList(ctx context.Context, blob string, blockIDs []string, contentType string, metadata map[string]string) error
	PutBlob(ctx context.Context, blob string, data []byte, contentType string) error
	Destination() string
}

const (
	defaultFrameBytes         = 16 << 20
	defaultBlockBytes         = 4 << 20
	defaultSegmentMaxBytes    = 256 << 20
	defaultSegmentMaxAge      = time.Hour
	defaultSegmentIdleSeal    = 15 * time.Minute
	defaultIndexFlush         = 10 * time.Minute
	defaultSegmentQueueBytes  = 256 << 20
	defaultUploadBacklogBytes = 512 << 20
	defaultLiveBytes          = 1 << 30
	defaultUploadWorkers      = 4
	defaultCloseTimeout       = 2 * time.Minute
	// maxDeltaChain bounds how many bodies one body may depend on, so reading
	// a single call never replays more than this many earlier requests.
	maxDeltaChain = 48
	// requestBodyDecodeLimit bounds how far a compressed request body is
	// expanded for recording.
	requestBodyDecodeLimit = 256 << 20
	// chunkCoalesceBytes is how much of one response stream is gathered into
	// a single chunk event.
	chunkCoalesceBytes = 1 << 20
	whoisPositiveTTL   = 15 * time.Minute
	whoisNegativeTTL   = time.Minute
	whoisTimeout       = 2 * time.Second
	// staleCallAge closes a call that never finished (its client vanished).
	staleCallAge = 2 * time.Hour
)

// UnknownAttribution is recorded when a client cannot be named.
const UnknownAttribution = "unknown"

// requestBodyZstd decodes zstd request bodies; DecodeAll is safe for
// concurrent use and the memory cap bounds a decompression bomb.
var requestBodyZstd, _ = zstd.NewReader(nil, zstd.WithDecoderConcurrency(1), zstd.WithDecoderMaxMemory(requestBodyDecodeLimit))

// frameVerifier decodes each compressed frame once before it is staged.
var frameVerifier, _ = zstd.NewReader(nil, zstd.WithDecoderConcurrency(1), zstd.WithDecoderMaxMemory(1<<31))

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

// segmentWriter turns recorder calls into per-session segment blobs and
// index rows.
type segmentWriter struct {
	cfg SegmentConfig

	mu     sync.Mutex
	queue  []*segmentItem
	queued int64
	closed bool
	kick   chan struct{}
	done   chan struct{}

	droppedQueue   atomic.Int64
	droppedBacklog atomic.Int64
	writeErrors    atomic.Int64
	lastDropLog    atomic.Int64

	up *uploadPool

	// The fields below belong to the writer goroutine (or, offline, to the
	// caller driving process).
	sessions      map[string]*sessionSegment
	calls         map[string]*callRow
	wsCalls       map[string]*callRow
	whois         map[string]whoisEntry
	encoder       *zstd.Encoder
	encodeBuf     bytes.Buffer
	liveBytes     int64
	serial        int
	lastIndexTick time.Time
	// lastHousekeeping is when time-based work last ran.
	lastHousekeeping time.Time
	wsMeta           map[string]*callRow
	index            indexBuffer
	// source names the legacy file being converted (offline only).
	source  string
	offline bool
	// onSeal runs for every sealed segment before its commit is queued.
	onSeal func(*sessionSegment)
}

type whoisEntry struct {
	node, login string
	expires     time.Time
}

type deltaChain struct {
	bodies []deltaBase
}

type deltaBase struct {
	seq   int64
	body  []byte
	frame int
	depth int
}

type pendingStream struct {
	eventType string
	payload   map[string]any
	offset    int64
	data      []byte
	at        time.Time
}

type frameInfo struct {
	compOff, compLen, rawLen int64
}

// sessionSegment is the open segment of one session.
type sessionSegment struct {
	key, agent, session string
	blob                string
	started, lastWrite  time.Time

	raw          []byte
	frames       []frameInfo
	compressed   int64
	rawTotal     int64
	pendingBlock []byte
	blockIDs     []string
	upload       *segmentUpload

	deltas     map[string]*deltaChain
	deltaBytes int64
	seq        int64
	events     int64
	bodyBytes  int64
	streams    map[string]*pendingStream
	sources    map[string]bool
	// checkpointed is when blocks were last made durable by an interim
	// commit; final marks the last commit, which adds the seek table.
	checkpointed time.Time
	final        bool
	commitMu     sync.Mutex
	committedAll bool
}

func withSegmentDefaults(cfg SegmentConfig) SegmentConfig {
	if cfg.FrameBytes <= 0 {
		cfg.FrameBytes = defaultFrameBytes
	}
	if cfg.BlockBytes <= 0 {
		cfg.BlockBytes = defaultBlockBytes
	}
	if cfg.MaxSegmentBytes <= 0 {
		cfg.MaxSegmentBytes = defaultSegmentMaxBytes
	}
	if cfg.MaxSegmentAge <= 0 {
		cfg.MaxSegmentAge = defaultSegmentMaxAge
	}
	if cfg.IdleSeal <= 0 {
		cfg.IdleSeal = defaultSegmentIdleSeal
	}
	if cfg.IndexFlush <= 0 {
		cfg.IndexFlush = defaultIndexFlush
	}
	if cfg.QueueBytes <= 0 {
		cfg.QueueBytes = defaultSegmentQueueBytes
	}
	if cfg.UploadBacklogBytes <= 0 {
		cfg.UploadBacklogBytes = defaultUploadBacklogBytes
	}
	if cfg.LiveBytes <= 0 {
		cfg.LiveBytes = defaultLiveBytes
	}
	if cfg.UploadWorkers <= 0 {
		cfg.UploadWorkers = defaultUploadWorkers
	}
	if cfg.CloseTimeout <= 0 {
		cfg.CloseTimeout = defaultCloseTimeout
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
	if cfg.BodiesPrefix == "" {
		cfg.BodiesPrefix = "bodies/v1"
	}
	if cfg.IndexPrefix == "" {
		cfg.IndexPrefix = "index/v1"
	}
	return cfg
}

func newSegmentWriter(cfg SegmentConfig, offline bool) (*segmentWriter, error) {
	if cfg.Uploader == nil {
		return nil, errors.New("transcript segments need a blob uploader")
	}
	cfg = withSegmentDefaults(cfg)
	encoder, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(cfg.CompressLevel), zstd.WithEncoderConcurrency(1),
		zstd.WithWindowSize(frameWindow(cfg.FrameBytes)), zstd.WithEncoderCRC(true))
	if err != nil {
		return nil, err
	}
	w := &segmentWriter{
		cfg:      cfg,
		kick:     make(chan struct{}, 1),
		done:     make(chan struct{}),
		sessions: map[string]*sessionSegment{},
		calls:    map[string]*callRow{},
		wsCalls:  map[string]*callRow{},
		whois:    map[string]whoisEntry{},
		encoder:  encoder,
		offline:  offline,
	}
	w.lastIndexTick = cfg.Now()
	w.up = newUploadPool(w)
	if !offline {
		go w.run()
	}
	return w, nil
}

// frameWindow is the smallest power-of-two window that holds a whole frame.
func frameWindow(frameBytes int) int {
	window := zstd.MinWindowSize
	for window < frameBytes && window < zstd.MaxWindowSize {
		window <<= 1
	}
	return window
}

// enqueue hands an item to the writer goroutine without waiting for it.
func (w *segmentWriter) enqueue(item *segmentItem) {
	if item.at.IsZero() {
		item.at = w.cfg.Now()
	}
	if w.up.full() {
		w.drop(&w.droppedBacklog, "transcript upload backlog is full")
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
		"dropped_queue_full", w.droppedQueue.Load(), "dropped_upload_backlog", w.droppedBacklog.Load())
}

// sync waits until everything enqueued so far is processed. With seal it
// also commits every open segment, waits for the uploads, and writes the
// index.
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
	case <-w.done:
	}
	if seal {
		w.up.waitIdle(context.Background())
		w.flushIndexNow()
		w.up.waitIdle(context.Background())
	}
}

func (w *segmentWriter) close() error {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		<-w.done
		return nil
	}
	w.closed = true
	w.mu.Unlock()
	if w.offline {
		w.finishAll("close")
		close(w.done)
	} else {
		select {
		case w.kick <- struct{}{}:
		default:
		}
		<-w.done
	}
	ctx, cancel := context.WithTimeout(context.Background(), w.cfg.CloseTimeout)
	defer cancel()
	w.up.waitIdle(ctx)
	w.flushIndexNow()
	w.up.waitIdle(ctx)
	w.up.stop()
	if ctx.Err() != nil {
		return fmt.Errorf("transcript uploads still pending after %s", w.cfg.CloseTimeout)
	}
	return nil
}

func (w *segmentWriter) run() {
	defer close(w.done)
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
					w.finishAll("close")
					return
				}
				break
			}
			for _, item := range batch {
				w.process(item)
			}
			// Under steady traffic the queue never drains, so time-based
			// work (checkpoints, idle and age commits, the index) runs
			// between batches too, not only when the writer goes quiet.
			if w.cfg.Now().Sub(w.lastHousekeeping) >= housekeepingInterval {
				w.housekeeping()
			}
		}
		w.housekeeping()
	}
}

// housekeeping seals idle and old segments, closes abandoned calls, and
// writes the index on schedule.
// housekeepingInterval is the longest the writer goes between time-based
// checks while it is busy.
const housekeepingInterval = 5 * time.Second

func (w *segmentWriter) housekeeping() {
	now := w.cfg.Now()
	w.lastHousekeeping = now
	for _, seg := range w.sessions {
		switch {
		case now.Sub(seg.lastWrite) >= w.cfg.IdleSeal:
			w.sealSession(seg, "idle")
		case now.Sub(seg.started) >= w.cfg.MaxSegmentAge:
			w.sealSession(seg, "age")
		case now.Sub(seg.checkpointed) >= segmentCheckpoint && (len(seg.raw) > 0 || len(seg.pendingBlock) > 0):
			w.checkpoint(seg)
		}
	}
	for id, call := range w.calls {
		if now.Sub(call.touched) >= staleCallAge {
			w.closeCall(call, false)
			delete(w.calls, id)
		}
	}
	for id, call := range w.wsCalls {
		if now.Sub(call.touched) >= staleCallAge {
			w.closeCall(call, true)
			delete(w.wsCalls, id)
		}
	}
	for id, template := range w.wsMeta {
		if now.Sub(template.touched) >= staleCallAge {
			delete(w.wsMeta, id)
		}
	}
	if now.Sub(w.lastIndexTick) >= w.cfg.IndexFlush {
		w.lastIndexTick = now
		w.flushIndexNow()
	}
}

// finishAll seals every session and closes every open call.
func (w *segmentWriter) finishAll(reason string) {
	for _, seg := range w.sessions {
		w.sealSession(seg, reason)
	}
	for id, call := range w.calls {
		w.closeCall(call, false)
		delete(w.calls, id)
	}
	for id, call := range w.wsCalls {
		w.closeCall(call, true)
		delete(w.wsCalls, id)
	}
}

func (w *segmentWriter) process(item *segmentItem) {
	switch item.kind {
	case itemSync:
		if item.seal {
			w.finishAll("sync")
		}
		close(item.done)
		return
	case itemMeta:
		w.attribute(item)
	}
	seg := w.segmentFor(item)
	switch item.kind {
	case itemChunk:
		w.addChunk(seg, item)
	case itemSummary:
		w.flushStream(seg, item.streamID, true)
		frame := w.writeEvent(seg, item.at, item.eventType, item.payload)
		w.noteSummary(seg, item, frame)
	case itemBody:
		depFrame := w.encodeBody(seg, item)
		frame := w.writeEvent(seg, item.at, item.eventType, item.payload)
		w.noteBody(seg, item, depFrame, frame)
	default:
		w.writeEvent(seg, item.at, item.eventType, item.payload)
		if item.kind == itemMeta {
			w.noteMeta(seg, item)
		}
	}
	w.afterWrite(seg)
}

// afterWrite closes a full frame, seals an oversized segment, and keeps
// memory inside its cap.
func (w *segmentWriter) afterWrite(seg *sessionSegment) {
	if len(seg.raw) >= w.cfg.FrameBytes {
		w.closeFrame(seg)
	}
	if seg.compressed >= w.cfg.MaxSegmentBytes {
		w.sealSession(seg, "size")
	}
	for w.liveBytes > w.cfg.LiveBytes && len(w.sessions) > 0 {
		// Delta bases are only an optimization: drop the least recently
		// active session's bases first (its next body is stored whole), and
		// commit a session early only when no bases are left to drop.
		var oldest, oldestWithBases *sessionSegment
		for _, candidate := range w.sessions {
			if oldest == nil || candidate.lastWrite.Before(oldest.lastWrite) {
				oldest = candidate
			}
			if candidate.deltaBytes > 0 && (oldestWithBases == nil || candidate.lastWrite.Before(oldestWithBases.lastWrite)) {
				oldestWithBases = candidate
			}
		}
		if oldestWithBases != nil {
			w.liveBytes -= oldestWithBases.deltaBytes
			oldestWithBases.deltas, oldestWithBases.deltaBytes = map[string]*deltaChain{}, 0
			continue
		}
		w.sealSession(oldest, "memory")
	}
}

func sessionKey(payload map[string]any) (key, agent, session string) {
	agent, _ = payload["agent_type"].(string)
	session, _ = payload["session_id"].(string)
	base := BaseSessionID(session)
	return agent + "\x00" + base, agent, base
}

// segmentFor returns the session's open segment, starting one if needed.
func (w *segmentWriter) segmentFor(item *segmentItem) *sessionSegment {
	key, agent, session := sessionKey(item.payload)
	seg := w.sessions[key]
	if seg != nil {
		return seg
	}
	now := w.cfg.Now()
	w.serial++
	var random [4]byte
	_, _ = rand.Read(random[:])
	segID := fmt.Sprintf("%s-%s-%d-%d-%s", now.UTC().Format("20060102T150405Z"), w.cfg.Host, os.Getpid(), w.serial, hex.EncodeToString(random[:]))
	seg = &sessionSegment{
		key: key, agent: agent, session: session,
		blob:    fmt.Sprintf("%s/%s/%s/%s/%s/%s.zst", w.cfg.BodiesPrefix, now.UTC().Format("2006/01/02/15"), w.cfg.Host, safeFilename(normalizeAgentType(agent)), safeFilename(session), segID),
		started: now, lastWrite: now, checkpointed: now,
		deltas:  map[string]*deltaChain{},
		streams: map[string]*pendingStream{},
		sources: map[string]bool{},
		upload:  newSegmentUpload(),
	}
	w.sessions[key] = seg
	w.writeEvent(seg, item.at, "segment_header", map[string]any{
		"format":           SegmentFormat,
		"segment_id":       segID,
		"blob":             seg.blob,
		"host":             w.cfg.Host,
		"pid":              os.Getpid(),
		"version":          w.cfg.Version,
		"agent_type":       normalizeAgentType(agent),
		"agent_session_id": session,
		"started_at":       now.UTC().Format(time.RFC3339Nano),
	})
	return seg
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
// against an earlier body of its chain, whichever is smaller. It returns
// the frame holding the first body the result depends on.
func (w *segmentWriter) encodeBody(seg *sessionSegment, item *segmentItem) int {
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
	item.body = body
	sum := sha256.Sum256(body)
	payload["bytes"] = len(body)
	payload["sha256"] = hex.EncodeToString(sum[:])
	seg.seq++
	seq := seg.seq
	payload["body_seq"] = seq
	seg.bodyBytes += int64(len(body))
	frame := len(seg.frames)
	if !item.delta {
		putBody(payload, body)
		return frame
	}
	key := deltaKeyFor(item.eventType, payload)
	depFrame, depth := frame, 0
	if chain := seg.deltas[key]; chain != nil {
		if ops, base, ok := bestDelta(chain, body); ok {
			marshaled := marshalDeltaOps(ops)
			// Prove the delta rebuilds the body before relying on it.
			if rebuilt, err := applyDelta(base.body, jsonRoundTrip(marshaled)); err == nil && bytes.Equal(rebuilt, body) {
				payload["delta"] = map[string]any{"base": base.seq, "ops": marshaled}
				depFrame, depth = base.frame, base.depth+1
			} else {
				w.writeErrors.Add(1)
			}
		}
	}
	if _, ok := payload["delta"]; !ok {
		putBody(payload, body)
	}
	w.keepBase(seg, key, deltaBase{seq: seq, body: body, frame: depFrame, depth: depth})
	return depFrame
}

// jsonRoundTrip makes ops look the way a reader decodes them.
func jsonRoundTrip(ops []any) any {
	encoded, err := json.Marshal(ops)
	if err != nil {
		return nil
	}
	var decoded any
	_ = json.Unmarshal(encoded, &decoded)
	return decoded
}

// bestDelta picks the base sharing the longest prefix plus suffix with body
// and encodes against it. A delta is used only when copies cover most of the
// body and the chain stays short enough to read one call quickly.
func bestDelta(chain *deltaChain, body []byte) ([]deltaOp, deltaBase, bool) {
	bestScore := -1
	var best deltaBase
	for _, base := range chain.bodies {
		if base.depth >= maxDeltaChain {
			continue
		}
		prefix := commonPrefixLen(base.body, body)
		score := prefix + commonSuffixLen(base.body[prefix:], body[prefix:])
		if score > bestScore {
			bestScore, best = score, base
		}
	}
	if best.body == nil || len(body) < deltaMinMatch {
		return nil, deltaBase{}, false
	}
	ops := encodeDelta(best.body, body)
	if deltaSavings(ops)*2 < len(body) {
		return nil, deltaBase{}, false
	}
	return ops, best, true
}

func (w *segmentWriter) keepBase(seg *sessionSegment, key string, base deltaBase) {
	chain := seg.deltas[key]
	if chain == nil {
		chain = &deltaChain{}
		seg.deltas[key] = chain
	}
	chain.bodies = append(chain.bodies, base)
	added := int64(len(base.body))
	if len(chain.bodies) > deltaBasesPerKey {
		added -= int64(len(chain.bodies[0].body))
		chain.bodies = append(chain.bodies[:0:0], chain.bodies[1:]...)
	}
	seg.deltaBytes += added
	w.liveBytes += added
}

func decodeContentEncoding(body []byte, encoding string) ([]byte, error) {
	var reader io.Reader
	switch encoding {
	case "zstd":
		return requestBodyZstd.DecodeAll(body, nil)
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
func (w *segmentWriter) addChunk(seg *sessionSegment, item *segmentItem) {
	stream := seg.streams[item.streamID]
	if stream == nil {
		stream = &pendingStream{eventType: item.eventType, payload: item.payload, offset: item.offset, at: item.at}
		seg.streams[item.streamID] = stream
	}
	w.noteChunk(seg, item)
	seg.lastWrite = w.cfg.Now()
	stream.data = append(stream.data, item.body...)
	w.liveBytes += int64(len(item.body))
	seg.bodyBytes += int64(len(item.body))
	if len(stream.data) >= chunkCoalesceBytes {
		w.flushStream(seg, item.streamID, false)
	}
}

// flushStream writes what a stream has gathered. Unless final, a trailing
// partial UTF-8 rune stays pending so the chunk is stored as text.
func (w *segmentWriter) flushStream(seg *sessionSegment, streamID string, final bool) {
	stream := seg.streams[streamID]
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
		frame := w.writeEvent(seg, stream.at, stream.eventType+"_chunk", payload)
		w.noteChunkFrame(seg, payload, frame)
		stream.offset += int64(cut)
	}
	w.liveBytes -= int64(len(data))
	if final {
		delete(seg.streams, streamID)
		return
	}
	stream.data = append([]byte(nil), data[cut:]...)
	w.liveBytes += int64(len(stream.data))
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

// writeEvent appends one event line to the open frame and returns the
// frame's index.
func (w *segmentWriter) writeEvent(seg *sessionSegment, at time.Time, eventType string, payload map[string]any) int {
	w.encodeBuf.Reset()
	encoder := json.NewEncoder(&w.encodeBuf)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(Event{Timestamp: at.UTC().Format(time.RFC3339Nano), Type: eventType, Payload: payload}); err != nil {
		w.writeErrors.Add(1)
		return len(seg.frames)
	}
	seg.raw = append(seg.raw, w.encodeBuf.Bytes()...)
	w.liveBytes += int64(w.encodeBuf.Len())
	seg.events++
	seg.lastWrite = w.cfg.Now()
	if w.source != "" {
		seg.sources[w.source] = true
	}
	return len(seg.frames)
}

// closeFrame compresses the open frame into one independent zstd frame,
// proves it decodes to the same bytes, and stages full blocks.
func (w *segmentWriter) closeFrame(seg *sessionSegment) {
	if len(seg.raw) == 0 {
		return
	}
	compressed := w.encoder.EncodeAll(seg.raw, nil)
	if decoded, err := frameVerifier.DecodeAll(compressed, nil); err != nil || !bytes.Equal(decoded, seg.raw) {
		w.writeErrors.Add(1)
		w.cfg.Logger.Error("transcript frame did not decode to its source; storing it at the fastest level", "blob", seg.blob)
		compressed = zstdFallback(seg.raw)
	}
	seg.frames = append(seg.frames, frameInfo{compOff: seg.compressed, compLen: int64(len(compressed)), rawLen: int64(len(seg.raw))})
	seg.compressed += int64(len(compressed))
	seg.rawTotal += int64(len(seg.raw))
	w.liveBytes -= int64(len(seg.raw))
	w.liveBytes += int64(len(compressed))
	seg.raw = nil
	seg.pendingBlock = append(seg.pendingBlock, compressed...)
	if len(seg.pendingBlock) >= w.cfg.BlockBytes {
		w.stageBlock(seg)
	}
}

func zstdFallback(raw []byte) []byte {
	encoder, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedFastest), zstd.WithEncoderConcurrency(1))
	if err != nil {
		return nil
	}
	defer encoder.Close()
	return encoder.EncodeAll(raw, nil)
}

func (w *segmentWriter) stageBlock(seg *sessionSegment) {
	if len(seg.pendingBlock) == 0 {
		return
	}
	id := base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("blk%08d", len(seg.blockIDs))))
	seg.blockIDs = append(seg.blockIDs, id)
	data := seg.pendingBlock
	seg.pendingBlock = nil
	w.liveBytes -= int64(len(data))
	w.up.putBlock(seg.upload, seg.blob, id, data)
}

// sealSession ends a session's segment: it closes the last frame, appends
// the seek table, stages the rest and commits the block list. Delta chains
// end with it, so every segment decodes on its own.
func (w *segmentWriter) sealSession(seg *sessionSegment, reason string) {
	if w.sessions[seg.key] != seg {
		return
	}
	for streamID := range seg.streams {
		w.flushStream(seg, streamID, true)
	}
	w.writeEvent(seg, w.cfg.Now(), "segment_footer", map[string]any{
		"sealed_at":              w.cfg.Now().UTC().Format(time.RFC3339Nano),
		"seal_reason":            reason,
		"events":                 seg.events,
		"body_bytes":             seg.bodyBytes,
		"dropped_queue_full":     w.droppedQueue.Load(),
		"dropped_upload_backlog": w.droppedBacklog.Load(),
		"write_errors":           w.writeErrors.Load(),
	})
	w.closeFrame(seg)
	table := seekTable(seg.frames)
	seg.pendingBlock = append(seg.pendingBlock, table...)
	w.liveBytes += int64(len(table))
	seg.final = true
	w.stageBlock(seg)
	w.liveBytes -= seg.deltaBytes
	seg.deltas, seg.deltaBytes = nil, 0
	delete(w.sessions, seg.key)
	if reason == "idle" || reason == "close" || reason == "sync" {
		// The session went quiet: calls still open in it will not finish.
		for id, call := range w.calls {
			if call.sessionKey == seg.key {
				w.closeCall(call, false)
				delete(w.calls, id)
			}
		}
		for id, call := range w.wsCalls {
			if call.sessionKey == seg.key {
				w.closeCall(call, true)
				delete(w.wsCalls, id)
			}
		}
	}
	metadata := map[string]string{
		"subrouter_format": "segment-v1",
		"host":             w.cfg.Host,
		"frames":           fmt.Sprint(len(seg.frames)),
		"raw_bytes":        fmt.Sprint(seg.rawTotal),
		"body_bytes":       fmt.Sprint(seg.bodyBytes),
	}
	if w.onSeal != nil {
		w.onSeal(seg)
	}
	w.up.commit(seg, metadata)
}

// segmentCheckpoint is how often an open segment's events are made durable.
// Uncommitted blocks vanish if the process dies, and the writer's credential
// cannot read them back, so an open segment is committed as it stands (whole
// zstd frames, no seek table yet) at this interval.
const segmentCheckpoint = 5 * time.Minute

func (w *segmentWriter) checkpoint(seg *sessionSegment) {
	seg.checkpointed = w.cfg.Now()
	w.closeFrame(seg)
	w.stageBlock(seg)
	if len(seg.blockIDs) == 0 {
		return
	}
	w.up.checkpoint(seg, append([]string(nil), seg.blockIDs...))
}

// seekTable renders the Zstandard seekable format's seek table: a skippable
// frame listing each frame's compressed and decompressed size, so a reader
// can find any frame without decompressing the ones before it.
func seekTable(frames []frameInfo) []byte {
	const (
		skippableMagic = 0x184D2A5E
		seekableMagic  = 0x8F92EAB1
	)
	body := make([]byte, 0, len(frames)*8+9)
	for _, frame := range frames {
		body = binary.LittleEndian.AppendUint32(body, uint32(frame.compLen))
		body = binary.LittleEndian.AppendUint32(body, uint32(frame.rawLen))
	}
	body = binary.LittleEndian.AppendUint32(body, uint32(len(frames)))
	body = append(body, 0) // descriptor: no checksums
	body = binary.LittleEndian.AppendUint32(body, seekableMagic)
	out := make([]byte, 0, len(body)+8)
	out = binary.LittleEndian.AppendUint32(out, skippableMagic)
	out = binary.LittleEndian.AppendUint32(out, uint32(len(body)))
	return append(out, body...)
}

// SegmentStats is what a segment recorder reports about itself.
type SegmentStats struct {
	DroppedQueueFull     int64 `json:"dropped_queue_full"`
	DroppedUploadBacklog int64 `json:"dropped_upload_backlog"`
	WriteErrors          int64 `json:"write_errors"`
	CommittedSegments    int64 `json:"committed_segments"`
	CommittedBytes       int64 `json:"committed_bytes"`
	CommittedRawBytes    int64 `json:"committed_raw_bytes"`
	CommittedBodyBytes   int64 `json:"committed_body_bytes"`
	LostSegments         int64 `json:"lost_segments"`
	BlockRetries         int64 `json:"block_retries"`
	IndexParts           int64 `json:"index_parts"`
	IndexRows            int64 `json:"index_rows"`
	UploadBacklogBytes   int64 `json:"upload_backlog_bytes"`
}

func (w *segmentWriter) stats() SegmentStats {
	return SegmentStats{
		DroppedQueueFull:     w.droppedQueue.Load(),
		DroppedUploadBacklog: w.droppedBacklog.Load(),
		WriteErrors:          w.writeErrors.Load(),
		CommittedSegments:    w.up.committed.Load(),
		CommittedBytes:       w.up.committedBytes.Load(),
		CommittedRawBytes:    w.up.committedRaw.Load(),
		CommittedBodyBytes:   w.up.committedBody.Load(),
		LostSegments:         w.up.lost.Load(),
		BlockRetries:         w.up.retries.Load(),
		IndexParts:           w.up.indexParts.Load(),
		IndexRows:            w.up.indexRows.Load(),
		UploadBacklogBytes:   w.up.backlog.Load(),
	}
}
