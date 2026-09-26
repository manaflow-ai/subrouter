package transcript

import (
	"bytes"
	"container/list"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Recorder appends transcript events to one JSONL file per agent session.
//
// Events are buffered in memory per session and written by a background
// flusher, so the request path only marshals and appends to a byte slice. A
// buffer is written when it reaches a size threshold, on a short interval,
// when its file handle is evicted from the LRU or goes idle, on Flush, and on
// Close. Each flush writes whole lines in a single write call, so a reader
// never observes a line split across two writes.
type Recorder struct {
	dir  string
	opts recorderOptions

	// mu guards the fields below. It is held only for map and list updates,
	// never across file I/O.
	mu        sync.Mutex
	appenders map[string]*appender
	lru       *list.List // front is the most recently written session
	evicted   []*appender
	closing   map[string]*appender // detached appenders whose final flush has not finished
	running   bool
	closed    bool
	done      chan struct{}

	stop     chan struct{}
	kick     chan struct{}
	buffered atomic.Int64

	dropped     atomic.Int64
	lastDropLog atomic.Int64
	directMu    sync.Mutex
}

type recorderOptions struct {
	// maxOpenFiles bounds the LRU of open session files.
	maxOpenFiles int
	// flushBytes is the per-session buffer size that triggers an early flush.
	flushBytes int
	// flushInterval is how often every buffer is written out.
	flushInterval time.Duration
	// idleTicks is how many flush intervals without a write close a session file.
	idleTicks int
	// maxBufferedBytes caps buffered bytes across all sessions. Events that
	// would exceed it are dropped and counted rather than blocking the proxy.
	// It stays above the readers' 32 MiB line limit so any event a reader can
	// parse also fits in the buffer.
	maxBufferedBytes int64
}

var defaultRecorderOptions = recorderOptions{
	maxOpenFiles:     256,
	flushBytes:       256 << 10,
	flushInterval:    time.Second,
	idleTicks:        30,
	maxBufferedBytes: 64 << 20,
}

const (
	maxSpareBufferBytes = 1 << 20
	dropLogInterval     = 10 * time.Second
)

// appender buffers one session file's events and owns its open handle.
type appender struct {
	path string
	elem *list.Element // guarded by Recorder.mu

	// mu guards buf, detached, touched, and idle.
	mu       sync.Mutex
	buf      []byte
	detached bool
	touched  bool
	idle     int

	// ioMu serializes file I/O for this appender and guards the fields below.
	ioMu     sync.Mutex
	file     *os.File
	info     os.FileInfo
	spare    []byte
	prev     *appender // an older appender for the same path that must finish first
	finished bool
}

type Event struct {
	Timestamp string         `json:"timestamp"`
	Type      string         `json:"type"`
	Payload   map[string]any `json:"payload"`
}

func NewRecorder(dir string) *Recorder {
	return newRecorder(dir, defaultRecorderOptions)
}

func newRecorder(dir string, opts recorderOptions) *Recorder {
	if strings.TrimSpace(dir) == "" {
		return nil
	}
	return &Recorder{
		dir:       dir,
		opts:      opts,
		appenders: map[string]*appender{},
		lru:       list.New(),
		closing:   map[string]*appender{},
		stop:      make(chan struct{}),
		kick:      make(chan struct{}, 1),
	}
}

func (r *Recorder) Enabled() bool {
	return r != nil && r.dir != ""
}

func (r *Recorder) RecordMeta(agentType, sessionID string, payload map[string]any) {
	if !r.Enabled() {
		return
	}
	r.write(agentType, sessionID, Event{
		Timestamp: now(),
		Type:      "subrouter_meta",
		Payload:   withSession(agentType, sessionID, payload),
	})
}

func (r *Recorder) RecordPayload(agentType, sessionID, eventType, direction string, body []byte, payload map[string]any) {
	if !r.Enabled() {
		return
	}
	sum := sha256.Sum256(body)
	enriched := withSession(agentType, sessionID, payload)
	enriched["direction"] = direction
	enriched["bytes"] = len(body)
	enriched["sha256"] = hex.EncodeToString(sum[:])
	enriched["body_base64"] = base64.StdEncoding.EncodeToString(body)
	r.write(agentType, sessionID, Event{
		Timestamp: now(),
		Type:      eventType,
		Payload:   enriched,
	})
}

func (r *Recorder) RecordPayloadChunk(agentType, sessionID, eventType, direction, streamID string, chunkIndex int, offset int64, body []byte, payload map[string]any) {
	if !r.Enabled() {
		return
	}
	sum := sha256.Sum256(body)
	enriched := withSession(agentType, sessionID, payload)
	enriched["direction"] = direction
	enriched["stream_id"] = streamID
	enriched["body_chunk"] = true
	enriched["chunk_index"] = chunkIndex
	enriched["offset"] = offset
	enriched["chunk_bytes"] = len(body)
	enriched["chunk_sha256"] = hex.EncodeToString(sum[:])
	enriched["body_base64"] = base64.StdEncoding.EncodeToString(body)
	r.write(agentType, sessionID, Event{
		Timestamp: now(),
		Type:      eventType + "_chunk",
		Payload:   enriched,
	})
}

func (r *Recorder) RecordPayloadSummary(agentType, sessionID, eventType, direction, streamID string, bytesRead int64, sha256Hex string, chunks int, payload map[string]any) {
	if !r.Enabled() {
		return
	}
	enriched := withSession(agentType, sessionID, payload)
	enriched["direction"] = direction
	enriched["stream_id"] = streamID
	enriched["body_chunked"] = true
	enriched["bytes"] = bytesRead
	enriched["sha256"] = sha256Hex
	enriched["chunks"] = chunks
	r.write(agentType, sessionID, Event{
		Timestamp: now(),
		Type:      eventType,
		Payload:   enriched,
	})
}

func (r *Recorder) PathForSession(agentType, sessionID string) string {
	agent := safeFilename(normalizeAgentType(agentType))
	base := BaseSessionID(sessionID)
	return filepath.Join(r.dir, "by-agent", agent, "by-session", safeFilename(base)+".jsonl")
}

func BaseSessionID(sessionID string) string {
	if before, _, ok := strings.Cut(sessionID, ":"); ok {
		return before
	}
	return sessionID
}

func RedactedHeaders(headers http.Header) map[string][]string {
	out := make(map[string][]string, len(headers))
	for key, values := range headers {
		if isSensitiveHeader(key) {
			out[key] = []string{"<redacted>"}
			continue
		}
		out[key] = append([]string(nil), values...)
	}
	return out
}

// Flush writes every buffered event to disk. Readers of the transcript
// directory call it first so they see events recorded up to that moment.
func (r *Recorder) Flush() error {
	if !r.Enabled() {
		return nil
	}
	r.mu.Lock()
	finishing := r.evicted
	r.evicted = nil
	for _, a := range r.closing {
		finishing = append(finishing, a)
	}
	live := r.liveLocked()
	r.mu.Unlock()

	var errs []error
	for _, a := range finishing {
		errs = append(errs, r.finish(a))
	}
	for _, a := range live {
		a.ioMu.Lock()
		errs = append(errs, r.flushLocked(a))
		a.ioMu.Unlock()
	}
	return errors.Join(errs...)
}

// Close flushes every buffered event, closes all session files, and stops the
// background flusher. Events recorded after Close are written synchronously,
// one open, write, and close per event, so late writers lose nothing.
func (r *Recorder) Close() error {
	if !r.Enabled() {
		return nil
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil
	}
	r.closed = true
	done := r.done
	running := r.running
	live := r.liveLocked()
	for _, a := range live {
		r.detachLocked(a)
	}
	r.mu.Unlock()

	close(r.stop)
	if running {
		<-done
	}

	r.mu.Lock()
	finishing := r.evicted
	r.evicted = nil
	for _, a := range r.closing {
		finishing = append(finishing, a)
	}
	r.mu.Unlock()
	var errs []error
	for _, a := range finishing {
		errs = append(errs, r.finish(a))
	}
	return errors.Join(errs...)
}

// DroppedEvents reports how many events were dropped because the buffer cap
// was reached or a write failed.
func (r *Recorder) DroppedEvents() int64 {
	if r == nil {
		return 0
	}
	return r.dropped.Load()
}

func (r *Recorder) write(agentType, sessionID string, event Event) {
	body, err := json.Marshal(event)
	if err != nil {
		r.recordDrop(1, err)
		return
	}
	line := append(body, '\n')
	path := r.PathForSession(agentType, sessionID)
	size := int64(len(line))
	if r.buffered.Add(size) > r.opts.maxBufferedBytes {
		r.buffered.Add(-size)
		r.recordDrop(1, errTranscriptBufferFull)
		return
	}
	for {
		a := r.appenderFor(path)
		if a == nil {
			r.buffered.Add(-size)
			r.writeDirect(path, line)
			return
		}
		a.mu.Lock()
		if a.detached {
			// Evicted or idled out between the lookup and the append. Its final
			// flush is already scheduled; look up the replacement instead.
			a.mu.Unlock()
			continue
		}
		a.buf = append(a.buf, line...)
		a.touched = true
		full := len(a.buf) >= r.opts.flushBytes
		a.mu.Unlock()
		if full || r.buffered.Load() >= r.opts.maxBufferedBytes/2 {
			r.signal()
		}
		return
	}
}

var errTranscriptBufferFull = errors.New("transcript buffer full")

// appenderFor returns the live appender for path, creating it and evicting
// the least recently written one when the LRU is full. It returns nil once
// the recorder is closed.
func (r *Recorder) appenderFor(path string) *appender {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil
	}
	if a, ok := r.appenders[path]; ok {
		r.lru.MoveToFront(a.elem)
		return a
	}
	a := &appender{path: path, prev: r.closing[path]}
	a.elem = r.lru.PushFront(a)
	r.appenders[path] = a
	if r.lru.Len() > r.opts.maxOpenFiles {
		oldest := r.lru.Back().Value.(*appender)
		r.detachLocked(oldest)
		r.evicted = append(r.evicted, oldest)
		r.signal()
	}
	if !r.running {
		r.running = true
		r.done = make(chan struct{})
		go r.flushLoop(r.done)
	}
	return a
}

// detachLocked removes a from the live set so no new events reach it. Its
// remaining buffer is written by finish. A later appender for the same path
// finishes it before its own first write, which keeps lines in order.
func (r *Recorder) detachLocked(a *appender) {
	delete(r.appenders, a.path)
	r.lru.Remove(a.elem)
	a.mu.Lock()
	a.detached = true
	a.mu.Unlock()
	r.closing[a.path] = a
}

func (r *Recorder) liveLocked() []*appender {
	live := make([]*appender, 0, r.lru.Len())
	for e := r.lru.Front(); e != nil; e = e.Next() {
		live = append(live, e.Value.(*appender))
	}
	return live
}

func (r *Recorder) signal() {
	select {
	case r.kick <- struct{}{}:
	default:
	}
}

func (r *Recorder) flushLoop(done chan struct{}) {
	defer close(done)
	ticker := time.NewTicker(r.opts.flushInterval)
	defer ticker.Stop()
	for {
		select {
		case <-r.stop:
			return
		case <-r.kick:
			r.flushPending(false)
		case <-ticker.C:
			r.flushPending(true)
			r.mu.Lock()
			if len(r.appenders) == 0 && len(r.evicted) == 0 {
				// Nothing left to write: exit so an idle recorder holds no
				// goroutine. The next write starts a new flusher.
				r.running = false
				r.mu.Unlock()
				return
			}
			r.mu.Unlock()
		}
	}
}

// flushPending finishes evicted appenders and writes live buffers. On a tick
// it writes every non-empty buffer and closes files idle for idleTicks; on a
// kick it writes only buffers past the size threshold, or all of them when
// the recorder-wide buffer is half full.
func (r *Recorder) flushPending(tick bool) {
	r.mu.Lock()
	evicted := r.evicted
	r.evicted = nil
	live := r.liveLocked()
	r.mu.Unlock()

	for _, a := range evicted {
		_ = r.finish(a)
	}
	pressure := r.buffered.Load() >= r.opts.maxBufferedBytes/2
	for _, a := range live {
		a.mu.Lock()
		size := len(a.buf)
		idle := false
		if tick {
			if a.touched {
				a.touched = false
				a.idle = 0
			} else {
				a.idle++
				idle = a.idle >= r.opts.idleTicks
			}
		}
		a.mu.Unlock()

		if idle {
			r.mu.Lock()
			if r.appenders[a.path] == a {
				r.detachLocked(a)
			}
			r.mu.Unlock()
			_ = r.finish(a)
			continue
		}
		if size == 0 || (!tick && !pressure && size < r.opts.flushBytes) {
			continue
		}
		a.ioMu.Lock()
		_ = r.flushLocked(a)
		a.ioMu.Unlock()
	}
}

// finish writes a detached appender's remaining buffer and closes its file.
// It is idempotent, so the flusher, Flush, Close, and a successor appender
// can all call it.
func (r *Recorder) finish(a *appender) error {
	a.ioMu.Lock()
	defer a.ioMu.Unlock()
	if a.finished {
		return nil
	}
	a.finished = true
	err := r.flushLocked(a)
	a.closeFile()
	a.spare = nil
	r.mu.Lock()
	if r.closing[a.path] == a {
		delete(r.closing, a.path)
	}
	r.mu.Unlock()
	return err
}

// flushLocked writes a's buffer. The caller holds a.ioMu.
func (r *Recorder) flushLocked(a *appender) error {
	if a.prev != nil {
		_ = r.finish(a.prev)
		a.prev = nil
	}
	a.mu.Lock()
	data := a.buf
	if len(data) == 0 {
		a.mu.Unlock()
		return nil
	}
	a.buf = a.spare
	a.mu.Unlock()

	err := a.writeFile(data)
	r.buffered.Add(-int64(len(data)))
	if err != nil {
		r.recordDrop(int64(bytes.Count(data, []byte{'\n'})), err)
		a.closeFile()
	}
	if cap(data) <= maxSpareBufferBytes {
		a.spare = data[:0]
	} else {
		a.spare = nil
	}
	return err
}

func (a *appender) writeFile(data []byte) error {
	if a.file != nil {
		// The GCS and Azure syncers delete archived files and prune empty
		// directories. Reopen when the path no longer names the open file, so
		// events never land in an unlinked inode.
		if info, err := os.Stat(a.path); err != nil || !os.SameFile(info, a.info) {
			a.closeFile()
		}
	}
	if a.file == nil {
		file, err := openTranscriptFile(a.path)
		if err != nil {
			return err
		}
		info, err := file.Stat()
		if err != nil {
			_ = file.Close()
			return err
		}
		a.file, a.info = file, info
	}
	_, err := a.file.Write(data)
	return err
}

func (a *appender) closeFile() {
	if a.file != nil {
		_ = a.file.Close()
		a.file, a.info = nil, nil
	}
}

// writeDirect appends one line after Close, after finishing any buffered
// events for the same path so lines stay in order.
func (r *Recorder) writeDirect(path string, line []byte) {
	r.mu.Lock()
	prev := r.closing[path]
	r.mu.Unlock()
	if prev != nil {
		_ = r.finish(prev)
	}
	r.directMu.Lock()
	defer r.directMu.Unlock()
	file, err := openTranscriptFile(path)
	if err != nil {
		r.recordDrop(1, err)
		return
	}
	defer file.Close()
	if _, err := file.Write(line); err != nil {
		r.recordDrop(1, err)
	}
}

// openTranscriptFile opens path for appending, creating its directory only
// when it is missing.
func openTranscriptFile(path string) (*os.File, error) {
	const flags = os.O_CREATE | os.O_APPEND | os.O_WRONLY
	file, err := os.OpenFile(path, flags, 0o600)
	if errors.Is(err, fs.ErrNotExist) {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return nil, err
		}
		file, err = os.OpenFile(path, flags, 0o600)
	}
	return file, err
}

func (r *Recorder) recordDrop(events int64, reason error) {
	total := r.dropped.Add(events)
	now := time.Now().UnixNano()
	last := r.lastDropLog.Load()
	if now-last < int64(dropLogInterval) || !r.lastDropLog.CompareAndSwap(last, now) {
		return
	}
	slog.Warn("transcript events dropped", "dir", r.dir, "dropped_total", total, "reason", reason.Error())
}

func withSession(agentType, sessionID string, payload map[string]any) map[string]any {
	out := make(map[string]any, len(payload)+4)
	for key, value := range payload {
		out[key] = value
	}
	normalizedAgent := normalizeAgentType(agentType)
	agentSessionID := BaseSessionID(sessionID)
	out["agent_type"] = normalizedAgent
	out["session_id"] = sessionID
	out["agent_session_id"] = agentSessionID
	if normalizedAgent == "codex" {
		out["codex_session_id"] = agentSessionID
	}
	return out
}

func normalizeAgentType(agentType string) string {
	normalized := strings.ToLower(strings.TrimSpace(agentType))
	if normalized == "" {
		return "codex"
	}
	return normalized
}

func now() string {
	return time.Now().UTC().Format(time.RFC3339Nano)
}

func isSensitiveHeader(key string) bool {
	switch strings.ToLower(key) {
	case "authorization", "cookie", "set-cookie", "proxy-authorization", "x-api-key", "openai-api-key":
		return true
	default:
		return false
	}
}

var unsafeFilenameChars = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

func safeFilename(value string) string {
	if value == "" {
		return "unknown"
	}
	safe := unsafeFilenameChars.ReplaceAllString(value, "_")
	trimmed := strings.Trim(safe, "._-")
	if trimmed == "" {
		return "unknown"
	}
	return trimmed
}
