package proxy

import (
	"bufio"
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/zstd"
	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/internal/tailnet"
)

// Token usage accounting counts, per hour, how many tokens each client spent
// on each account and model. It reads only the usage block a provider reports
// at the end of a response; no prompt or completion text is kept, and the
// bytes delivered to the client are never changed or delayed.

// ClientNameHeader is the self-reported client name `sr` sends with every
// proxied request. It only labels usage rows; it grants nothing.
const ClientNameHeader = "X-Subrouter-Client"

const (
	tokenUsageRetention       = 30 * 24 * time.Hour
	tokenUsageDefaultSince    = 24 * time.Hour
	tokenUsageFlushInterval   = 3 * time.Minute
	tokenUsageMaxKeysPerHour  = 2000
	tokenUsageOverflowLabel   = "other"
	tokenUsageUnknownClient   = "unknown"
	tokenUsageFallbackAccount = "fallback"
	tokenUsageMaxClientLength = 64
	// tokenUsageLineHeadBytes is how much of one SSE line (or a JSON body) is
	// kept whole. Anything that fits is decoded as JSON; a longer line keeps
	// only its head and tail, which is where the event type, model, and usage
	// sit.
	tokenUsageLineHeadBytes = 64 << 10
	tokenUsageLineTailBytes = 32 << 10
	tokenUsageClientTTL     = 10 * time.Minute
	tokenUsageClientCacheN  = 1024
	tokenUsageWhoIsTimeout  = 3 * time.Second
	// tokenUsageMaxZstdWindow is the largest zstd window an HTTP recipient
	// must support (RFC 9659); a frame asking for more is rejected.
	tokenUsageMaxZstdWindow = 8 << 20
)

// tokenUsage is the usage one response reported, normalized to OpenAI
// semantics: InputTokens is every prompt token, and cached reads and cache
// writes are subsets of it. ReasoningOutputTokens is a subset of OutputTokens.
type tokenUsage struct {
	InputTokens           int64
	CachedInputTokens     int64
	CacheWriteInputTokens int64
	OutputTokens          int64
	ReasoningOutputTokens int64
}

// tokenUsageWire accepts the three usage shapes the proxy sees: OpenAI
// Responses, OpenAI chat completions, and Anthropic messages.
type tokenUsageWire struct {
	InputTokens              *int64 `json:"input_tokens"`
	OutputTokens             *int64 `json:"output_tokens"`
	CacheReadInputTokens     *int64 `json:"cache_read_input_tokens"`
	CacheCreationInputTokens *int64 `json:"cache_creation_input_tokens"`
	InputTokensDetails       *struct {
		CachedTokens     int64 `json:"cached_tokens"`
		CacheWriteTokens int64 `json:"cache_write_tokens"`
	} `json:"input_tokens_details"`
	OutputTokensDetails *struct {
		ReasoningTokens int64 `json:"reasoning_tokens"`
	} `json:"output_tokens_details"`
	PromptTokens        *int64 `json:"prompt_tokens"`
	CompletionTokens    *int64 `json:"completion_tokens"`
	PromptTokensDetails *struct {
		CachedTokens int64 `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
	CompletionTokensDetails *struct {
		ReasoningTokens int64 `json:"reasoning_tokens"`
	} `json:"completion_tokens_details"`
}

func (w *tokenUsageWire) anthropic() bool {
	return w.CacheReadInputTokens != nil || w.CacheCreationInputTokens != nil
}

func (w *tokenUsageWire) any() bool {
	return w != nil && (w.InputTokens != nil || w.OutputTokens != nil || w.PromptTokens != nil ||
		w.CompletionTokens != nil || w.anthropic())
}

func int64Value(value *int64) int64 {
	if value == nil || *value < 0 {
		return 0
	}
	return *value
}

// normalize converts one wire usage block. Anthropic reports input_tokens
// without cache reads and writes, so they are added back to make the input
// total comparable with OpenAI's.
func (w *tokenUsageWire) normalize() tokenUsage {
	if w.PromptTokens != nil || w.CompletionTokens != nil {
		usage := tokenUsage{InputTokens: int64Value(w.PromptTokens), OutputTokens: int64Value(w.CompletionTokens)}
		if w.PromptTokensDetails != nil {
			usage.CachedInputTokens = w.PromptTokensDetails.CachedTokens
		}
		if w.CompletionTokensDetails != nil {
			usage.ReasoningOutputTokens = w.CompletionTokensDetails.ReasoningTokens
		}
		return usage
	}
	if w.anthropic() {
		read := int64Value(w.CacheReadInputTokens)
		write := int64Value(w.CacheCreationInputTokens)
		return tokenUsage{
			InputTokens:           int64Value(w.InputTokens) + read + write,
			CachedInputTokens:     read,
			CacheWriteInputTokens: write,
			OutputTokens:          int64Value(w.OutputTokens),
		}
	}
	usage := tokenUsage{InputTokens: int64Value(w.InputTokens), OutputTokens: int64Value(w.OutputTokens)}
	if w.InputTokensDetails != nil {
		usage.CachedInputTokens = w.InputTokensDetails.CachedTokens
		usage.CacheWriteInputTokens = w.InputTokensDetails.CacheWriteTokens
	}
	if w.OutputTokensDetails != nil {
		usage.ReasoningOutputTokens = w.OutputTokensDetails.ReasoningTokens
	}
	return usage
}

// tokenUsageEvent is the subset of an SSE event, WebSocket message, or JSON
// body that carries usage. Everything else in the payload is ignored.
type tokenUsageEvent struct {
	Type     string          `json:"type"`
	Model    string          `json:"model"`
	Usage    *tokenUsageWire `json:"usage"`
	Response *struct {
		Model string          `json:"model"`
		Usage *tokenUsageWire `json:"usage"`
	} `json:"response"`
	Message *struct {
		Model string          `json:"model"`
		Usage *tokenUsageWire `json:"usage"`
	} `json:"message"`
}

func (e tokenUsageEvent) usageAndModel() (*tokenUsageWire, string) {
	switch {
	case e.Response != nil && e.Response.Usage.any():
		return e.Response.Usage, e.Response.Model
	case e.Message != nil && e.Message.Usage.any():
		return e.Message.Usage, e.Message.Model
	case e.Usage.any():
		return e.Usage, e.Model
	}
	model := e.Model
	if e.Response != nil && e.Response.Model != "" {
		model = e.Response.Model
	}
	if e.Message != nil && e.Message.Model != "" {
		model = e.Message.Model
	}
	return nil, model
}

// tokenUsageAccumulator folds the events of one response into its usage.
// Anthropic streams split usage: message_start carries the input side and
// message_delta the cumulative output. Every other shape reports one final
// block, and the last one wins.
type tokenUsageAccumulator struct {
	usage tokenUsage
	model string
	got   bool
}

func (a *tokenUsageAccumulator) observe(eventType string, wire *tokenUsageWire, model string) {
	if model != "" && a.model == "" {
		a.model = model
	}
	if !wire.any() {
		return
	}
	next := wire.normalize()
	switch eventType {
	case "message_delta":
		// Output is cumulative; input fields appear here only on newer API
		// versions and, when present, are the final totals.
		if wire.OutputTokens != nil {
			a.usage.OutputTokens = next.OutputTokens
		}
		if wire.InputTokens != nil || wire.anthropic() {
			a.usage.InputTokens = next.InputTokens
			a.usage.CachedInputTokens = next.CachedInputTokens
			a.usage.CacheWriteInputTokens = next.CacheWriteInputTokens
		}
	default:
		a.usage = next
	}
	a.got = true
}

// observeJSON decodes one complete JSON payload.
func (a *tokenUsageAccumulator) observeJSON(payload []byte) {
	var event tokenUsageEvent
	if json.Unmarshal(payload, &event) != nil {
		return
	}
	wire, model := event.usageAndModel()
	a.observe(event.Type, wire, model)
}

// observeTruncated handles a payload too large to keep whole: the event type
// and model come from its head, the usage object from its tail. Inside JSON a
// quote in string content is always escaped, so an unescaped `"usage":{` can
// only be a key, and the last one is the response's own usage because the
// usage block follows the output in every provider's layout.
func (a *tokenUsageAccumulator) observeTruncated(head, tail []byte) {
	eventType := firstJSONStringField(head, "type")
	model := firstJSONStringField(head, "model")
	marker := []byte(`"usage":`)
	index := bytes.LastIndex(tail, marker)
	if index < 0 {
		a.observe(eventType, nil, model)
		return
	}
	rest := bytes.TrimLeft(tail[index+len(marker):], " \t\r\n")
	if len(rest) == 0 || rest[0] != '{' {
		a.observe(eventType, nil, model)
		return
	}
	var wire tokenUsageWire
	if json.NewDecoder(bytes.NewReader(rest)).Decode(&wire) != nil {
		a.observe(eventType, nil, model)
		return
	}
	a.observe(eventType, &wire, model)
}

// firstJSONStringField returns the value of the first `"name":"value"` pair in
// a JSON fragment, or "" when it is absent or not a short plain string.
func firstJSONStringField(fragment []byte, name string) string {
	for _, marker := range [][]byte{[]byte(`"` + name + `":"`), []byte(`"` + name + `": "`)} {
		index := bytes.Index(fragment, marker)
		if index < 0 {
			continue
		}
		rest := fragment[index+len(marker):]
		end := bytes.IndexByte(rest, '"')
		if end <= 0 || end > 256 || bytes.IndexByte(rest[:end], '\\') >= 0 {
			return ""
		}
		return string(rest[:end])
	}
	return ""
}

func (a *tokenUsageAccumulator) result() (tokenUsage, string, bool) {
	return a.usage, a.model, a.got
}

// tokenUsageScanner watches a response body as it streams past. It keeps at
// most one line's head and tail (SSE) or the body's head and tail (JSON), so a
// long stream costs a bounded amount of memory.
type tokenUsageScanner struct {
	sse     bool
	head    []byte
	tail    []byte
	lineLen int
	acc     tokenUsageAccumulator
}

func newTokenUsageScanner(contentType string) *tokenUsageScanner {
	return &tokenUsageScanner{sse: strings.Contains(strings.ToLower(contentType), "text/event-stream")}
}

func (s *tokenUsageScanner) Write(chunk []byte) {
	if !s.sse {
		s.appendBytes(chunk)
		return
	}
	for len(chunk) > 0 {
		newline := bytes.IndexByte(chunk, '\n')
		if newline < 0 {
			s.appendBytes(chunk)
			return
		}
		s.appendBytes(chunk[:newline])
		s.finishLine()
		chunk = chunk[newline+1:]
	}
}

func (s *tokenUsageScanner) appendBytes(chunk []byte) {
	if len(chunk) == 0 {
		return
	}
	s.lineLen += len(chunk)
	if room := tokenUsageLineHeadBytes - len(s.head); room > 0 {
		take := chunk
		if len(take) > room {
			take = take[:room]
		}
		s.head = append(s.head, take...)
	}
	if s.lineLen <= tokenUsageLineHeadBytes {
		return
	}
	// Past the head: keep a rolling tail of the most recent bytes. It may
	// grow to twice its size before being cut back, so trimming is amortized
	// rather than a copy per chunk.
	if len(chunk) > tokenUsageLineTailBytes {
		chunk = chunk[len(chunk)-tokenUsageLineTailBytes:]
	}
	s.tail = append(s.tail, chunk...)
	if len(s.tail) > 2*tokenUsageLineTailBytes {
		s.tail = append(s.tail[:0], s.tail[len(s.tail)-tokenUsageLineTailBytes:]...)
	}
}

func (s *tokenUsageScanner) tailBytes() []byte {
	if len(s.tail) > tokenUsageLineTailBytes {
		return s.tail[len(s.tail)-tokenUsageLineTailBytes:]
	}
	return s.tail
}

func (s *tokenUsageScanner) finishLine() {
	defer s.resetLine()
	line := bytes.TrimRight(s.head, "\r")
	if s.lineLen <= tokenUsageLineHeadBytes {
		if !bytes.HasPrefix(line, []byte("data:")) {
			return
		}
		payload := bytes.TrimSpace(line[len("data:"):])
		if len(payload) == 0 || payload[0] != '{' {
			return
		}
		s.acc.observeJSON(payload)
		return
	}
	if !bytes.HasPrefix(line, []byte("data:")) {
		return
	}
	s.acc.observeTruncated(line, bytes.TrimRight(s.tailBytes(), "\r"))
}

func (s *tokenUsageScanner) resetLine() {
	s.head = s.head[:0]
	s.tail = s.tail[:0]
	s.lineLen = 0
}

// Finish returns the usage seen, draining any unterminated SSE line or the
// buffered JSON body.
func (s *tokenUsageScanner) Finish() (tokenUsage, string, bool) {
	if s.sse {
		if s.lineLen > 0 {
			s.finishLine()
		}
		return s.acc.result()
	}
	if s.lineLen <= tokenUsageLineHeadBytes {
		payload := bytes.TrimSpace(s.head)
		if len(payload) > 0 && payload[0] == '{' {
			s.acc.observeJSON(payload)
		}
	} else {
		s.acc.observeTruncated(s.head, s.tailBytes())
	}
	s.resetLine()
	return s.acc.result()
}

// tokenUsageFromWebSocketMessage reads a Codex WebSocket event. It reports
// whether the event ends a turn, since that is when a turn is counted.
func tokenUsageFromWebSocketMessage(body []byte) (usage tokenUsage, model string, ok bool, terminal bool) {
	if len(body) == 0 || !bytes.Contains(body, []byte(`"response.`)) {
		return tokenUsage{}, "", false, false
	}
	var event tokenUsageEvent
	if json.Unmarshal(body, &event) != nil {
		return tokenUsage{}, "", false, false
	}
	switch strings.ToLower(event.Type) {
	case "response.completed", "response.incomplete", "response.failed", "response.done":
	default:
		return tokenUsage{}, "", false, false
	}
	var acc tokenUsageAccumulator
	wire, eventModel := event.usageAndModel()
	acc.observe(event.Type, wire, eventModel)
	usage, model, ok = acc.result()
	return usage, model, ok, true
}

// tokenUsageCountedRequest reports whether a request is a model turn worth
// counting. Catalog polls, token counting, and other side endpoints would
// otherwise swamp the request counts.
func tokenUsageCountedRequest(method, path string) bool {
	if method != http.MethodPost {
		return false
	}
	path = strings.TrimRight(path, "/")
	for _, suffix := range []string{"/responses", "/responses/compact", "/messages", "/chat/completions"} {
		if strings.HasSuffix(path, suffix) {
			return true
		}
	}
	return false
}

// tokenUsageSink receives the raw response bytes and yields the usage once the
// body is done.
type tokenUsageSink interface {
	Write(chunk []byte)
	Finish() (tokenUsage, string, bool)
}

// tokenUsageBody wraps a response body and records its usage once, when the
// body ends or is closed. Reads pass through untouched.
type tokenUsageBody struct {
	io.ReadCloser
	sink   tokenUsageSink
	record func(tokenUsage, string, bool)
	once   sync.Once
}

// newTokenUsageBody wraps inner. The proxy forwards the client's
// Accept-Encoding, so upstream usually answers compressed (Anthropic sends
// br, chatgpt.com gzip or zstd); the scanner then reads a decoded copy while
// the client still gets the original bytes.
func newTokenUsageBody(inner io.ReadCloser, contentType, contentEncoding string, record func(tokenUsage, string, bool)) io.ReadCloser {
	return &tokenUsageBody{ReadCloser: inner, sink: newTokenUsageSink(contentType, contentEncoding), record: record}
}

func (b *tokenUsageBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if n > 0 {
		b.sink.Write(p[:n])
	}
	if err == io.EOF {
		b.finish()
	}
	return n, err
}

func (b *tokenUsageBody) Close() error {
	err := b.ReadCloser.Close()
	b.finish()
	return err
}

func (b *tokenUsageBody) finish() {
	b.once.Do(func() {
		usage, model, ok := b.sink.Finish()
		b.record(usage, model, ok)
	})
}

func newTokenUsageSink(contentType, contentEncoding string) tokenUsageSink {
	scanner := newTokenUsageScanner(contentType)
	encoding := strings.ToLower(strings.TrimSpace(contentEncoding))
	switch encoding {
	case "", "identity":
		return scanner
	case "gzip", "x-gzip", "deflate", "br", "zstd":
		return newTokenUsageDecodingSink(scanner, encoding)
	}
	// Stacked or unknown encodings: the turn still counts, without usage.
	return tokenUsageDiscardSink{}
}

type tokenUsageDiscardSink struct{}

func (tokenUsageDiscardSink) Write([]byte) {}

func (tokenUsageDiscardSink) Finish() (tokenUsage, string, bool) { return tokenUsage{}, "", false }

// Limits on decoding a compressed body for accounting. Past any of them the
// turn counts without usage; the client's bytes are never affected. They are
// variables so tests can shrink them.
var (
	// tokenUsageMaxPendingBytes caps compressed bytes queued ahead of the
	// decoder.
	tokenUsageMaxPendingBytes = 8 << 20
	// tokenUsageMaxDecodedBytes stops decoding a body that inflates past it.
	tokenUsageMaxDecodedBytes int64 = 256 << 20
	// tokenUsageDecodeWait bounds how long the body's EOF or Close waits for
	// the decoder to catch up.
	tokenUsageDecodeWait = 250 * time.Millisecond
	// openTokenUsageDecoderFunc is a seam for tests.
	openTokenUsageDecoderFunc = openTokenUsageDecoder
)

// tokenUsageDecodingSink decompresses a copy of the body on its own goroutine.
// Write only queues bytes and never waits on the decoder, and Finish waits at
// most tokenUsageDecodeWait, so decoding cannot delay the client.
type tokenUsageDecodingSink struct {
	scanner *tokenUsageScanner
	queue   *tokenUsageByteQueue
	done    chan struct{}
	// failed is set when a limit gave up on the body; its usage is not read.
	failed atomic.Bool
	// Limits are captured at construction so a running decoder never reads
	// the package variables.
	maxDecoded int64
	wait       time.Duration
	open       func(string, io.Reader) (io.Reader, func(), error)
}

func newTokenUsageDecodingSink(scanner *tokenUsageScanner, encoding string) *tokenUsageDecodingSink {
	sink := &tokenUsageDecodingSink{
		scanner:    scanner,
		done:       make(chan struct{}),
		maxDecoded: tokenUsageMaxDecodedBytes,
		wait:       tokenUsageDecodeWait,
		open:       openTokenUsageDecoderFunc,
	}
	sink.queue = newTokenUsageByteQueue(tokenUsageMaxPendingBytes, func() { sink.failed.Store(true) })
	go sink.decode(encoding)
	return sink
}

func (s *tokenUsageDecodingSink) decode(encoding string) {
	defer close(s.done)
	// Whatever ends decoding, stop buffering bytes nobody will read.
	defer s.queue.abandon()
	decoded, closeDecoder, err := s.open(encoding, s.queue)
	if err != nil {
		return
	}
	defer closeDecoder()
	buf := make([]byte, 32<<10)
	var total int64
	for {
		n, err := decoded.Read(buf)
		if n > 0 {
			total += int64(n)
			if total > s.maxDecoded {
				s.failed.Store(true)
				return
			}
			s.scanner.Write(buf[:n])
		}
		if err != nil || s.failed.Load() {
			return
		}
	}
}

func (s *tokenUsageDecodingSink) Write(chunk []byte) { s.queue.write(chunk) }

func (s *tokenUsageDecodingSink) Finish() (tokenUsage, string, bool) {
	s.queue.close()
	timer := time.NewTimer(s.wait)
	defer timer.Stop()
	select {
	case <-s.done:
	case <-timer.C:
		// The decoder still owns the scanner; leave it to exit on its own
		// once it sees the abandoned queue.
		s.failed.Store(true)
		s.queue.abandon()
		return tokenUsage{}, "", false
	}
	if s.failed.Load() {
		return tokenUsage{}, "", false
	}
	return s.scanner.Finish()
}

func openTokenUsageDecoder(encoding string, r io.Reader) (io.Reader, func(), error) {
	switch encoding {
	case "gzip", "x-gzip":
		decoder, err := gzip.NewReader(r)
		if err != nil {
			return nil, nil, err
		}
		return decoder, func() { _ = decoder.Close() }, nil
	case "deflate":
		// HTTP deflate is zlib-wrapped, but some servers send raw DEFLATE.
		buffered := bufio.NewReader(r)
		header, err := buffered.Peek(2)
		if err != nil {
			return nil, nil, err
		}
		if header[0]&0x0f == 8 && (uint16(header[0])<<8|uint16(header[1]))%31 == 0 {
			decoder, err := zlib.NewReader(buffered)
			if err != nil {
				return nil, nil, err
			}
			return decoder, func() { _ = decoder.Close() }, nil
		}
		decoder := flate.NewReader(buffered)
		return decoder, func() { _ = decoder.Close() }, nil
	case "br":
		return brotli.NewReader(r), func() {}, nil
	case "zstd":
		decoder, err := zstd.NewReader(r,
			zstd.WithDecoderConcurrency(1),
			zstd.WithDecoderMaxWindow(tokenUsageMaxZstdWindow),
			zstd.WithDecoderMaxMemory(tokenUsageMaxZstdWindow))
		if err != nil {
			return nil, nil, err
		}
		return decoder, decoder.Close, nil
	}
	return nil, nil, errors.New("unsupported content encoding")
}

var errTokenUsageQueueAbandoned = errors.New("token usage queue abandoned")

// tokenUsageByteQueue is a bounded in-memory pipe: write never blocks, and
// Read waits for data or close. Past maxPending queued bytes it gives up and
// calls onOverflow; once abandoned, writes are dropped and Read fails.
type tokenUsageByteQueue struct {
	mu         sync.Mutex
	ready      *sync.Cond
	pending    []byte
	maxPending int
	onOverflow func()
	closed     bool
	abandoned  bool
}

func newTokenUsageByteQueue(maxPending int, onOverflow func()) *tokenUsageByteQueue {
	q := &tokenUsageByteQueue{maxPending: maxPending, onOverflow: onOverflow}
	q.ready = sync.NewCond(&q.mu)
	return q
}

func (q *tokenUsageByteQueue) write(chunk []byte) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed || q.abandoned {
		return
	}
	if len(q.pending)+len(chunk) > q.maxPending {
		q.abandonLocked()
		if q.onOverflow != nil {
			q.onOverflow()
		}
		return
	}
	q.pending = append(q.pending, chunk...)
	q.ready.Signal()
}

func (q *tokenUsageByteQueue) close() {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.closed = true
	q.ready.Signal()
}

func (q *tokenUsageByteQueue) abandon() {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.abandonLocked()
}

func (q *tokenUsageByteQueue) abandonLocked() {
	q.abandoned = true
	q.pending = nil
	q.ready.Signal()
}

func (q *tokenUsageByteQueue) Read(p []byte) (int, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for len(q.pending) == 0 && !q.closed && !q.abandoned {
		q.ready.Wait()
	}
	if q.abandoned {
		return 0, errTokenUsageQueueAbandoned
	}
	if len(q.pending) == 0 {
		return 0, io.EOF
	}
	n := copy(p, q.pending)
	q.pending = q.pending[n:]
	if len(q.pending) == 0 {
		// Drop the backing array so a long stream does not pin its peak.
		q.pending = nil
	}
	return n, nil
}

// TokenUsageRow is one aggregated row, both on disk and in the endpoint.
type TokenUsageRow struct {
	Hour                  string `json:"hour"`
	Provider              string `json:"provider"`
	AccountID             string `json:"account_id"`
	Model                 string `json:"model"`
	Client                string `json:"client"`
	Requests              int64  `json:"requests"`
	RequestsWithoutUsage  int64  `json:"requests_without_usage"`
	InputTokens           int64  `json:"input_tokens"`
	CachedInputTokens     int64  `json:"cached_input_tokens"`
	CacheWriteInputTokens int64  `json:"cache_write_input_tokens"`
	OutputTokens          int64  `json:"output_tokens"`
	ReasoningOutputTokens int64  `json:"reasoning_output_tokens"`
}

type tokenUsageKey struct {
	Hour      int64
	Provider  string
	AccountID string
	Model     string
	Client    string
}

type tokenUsageCounts struct {
	Requests              int64
	RequestsWithoutUsage  int64
	InputTokens           int64
	CachedInputTokens     int64
	CacheWriteInputTokens int64
	OutputTokens          int64
	ReasoningOutputTokens int64
}

func (c *tokenUsageCounts) add(other tokenUsageCounts) {
	c.Requests += other.Requests
	c.RequestsWithoutUsage += other.RequestsWithoutUsage
	c.InputTokens += other.InputTokens
	c.CachedInputTokens += other.CachedInputTokens
	c.CacheWriteInputTokens += other.CacheWriteInputTokens
	c.OutputTokens += other.OutputTokens
	c.ReasoningOutputTokens += other.ReasoningOutputTokens
}

func (c tokenUsageCounts) zero() bool {
	return c == tokenUsageCounts{}
}

func tokenUsageRowFor(key tokenUsageKey, counts tokenUsageCounts) TokenUsageRow {
	return TokenUsageRow{
		Hour:                  time.Unix(key.Hour, 0).UTC().Format(time.RFC3339),
		Provider:              key.Provider,
		AccountID:             key.AccountID,
		Model:                 key.Model,
		Client:                key.Client,
		Requests:              counts.Requests,
		RequestsWithoutUsage:  counts.RequestsWithoutUsage,
		InputTokens:           counts.InputTokens,
		CachedInputTokens:     counts.CachedInputTokens,
		CacheWriteInputTokens: counts.CacheWriteInputTokens,
		OutputTokens:          counts.OutputTokens,
		ReasoningOutputTokens: counts.ReasoningOutputTokens,
	}
}

func tokenUsageKeyForRow(row TokenUsageRow) (tokenUsageKey, tokenUsageCounts, bool) {
	hour, err := time.Parse(time.RFC3339, row.Hour)
	if err != nil {
		return tokenUsageKey{}, tokenUsageCounts{}, false
	}
	return tokenUsageKey{
			Hour:      hour.UTC().Truncate(time.Hour).Unix(),
			Provider:  row.Provider,
			AccountID: row.AccountID,
			Model:     row.Model,
			Client:    row.Client,
		}, tokenUsageCounts{
			Requests:              row.Requests,
			RequestsWithoutUsage:  row.RequestsWithoutUsage,
			InputTokens:           row.InputTokens,
			CachedInputTokens:     row.CachedInputTokens,
			CacheWriteInputTokens: row.CacheWriteInputTokens,
			OutputTokens:          row.OutputTokens,
			ReasoningOutputTokens: row.ReasoningOutputTokens,
		}, true
}

// TokenUsageWhoIs resolves a tailnet peer; *tailnet.Resolver satisfies it.
type TokenUsageWhoIs interface {
	Lookup(ctx context.Context, remoteAddr string) (tailnet.Identity, bool)
}

// TokenUsageRecorder aggregates usage per hour and persists it.
//
// The file holds delta rows: each flush appends what changed since the last
// one, and loading sums them. Appending deltas, rather than rewriting totals,
// keeps two overlapping workers (an old one draining while a new one starts)
// from overwriting each other. The file is compacted to one row per key, and
// pruned to 30 days, at startup and on every hour rollover.
type TokenUsageRecorder struct {
	path  string
	now   func() time.Time
	whois TokenUsageWhoIs

	mu    sync.Mutex
	hours map[int64]*tokenUsageHour

	fileMu           sync.Mutex
	lastCompactedHr  int64
	clientCacheMu    sync.Mutex
	clientCache      map[string]tokenUsageClientEntry
	flushLoopStarted sync.Once
}

type tokenUsageHour struct {
	// total is everything this process counted in the hour; its size is
	// what the per-hour key cap bounds. pending is the part not yet written.
	total   map[tokenUsageKey]tokenUsageCounts
	pending map[tokenUsageKey]tokenUsageCounts
}

type tokenUsageClientEntry struct {
	name    string
	expires time.Time
}

// NewTokenUsageRecorder loads (and compacts) the usage log at path. An empty
// path keeps usage in memory only.
func NewTokenUsageRecorder(path string, whois TokenUsageWhoIs) *TokenUsageRecorder {
	recorder := &TokenUsageRecorder{
		path:        strings.TrimSpace(path),
		now:         time.Now,
		whois:       whois,
		hours:       map[int64]*tokenUsageHour{},
		clientCache: map[string]tokenUsageClientEntry{},
	}
	if recorder.path != "" {
		if err := recorder.compact(); err != nil && !errors.Is(err, os.ErrNotExist) {
			// A damaged log must not keep the proxy from starting; usage is
			// observability, not routing state.
			slog.Warn("token usage log could not be compacted", "path", recorder.path, "error", err)
		}
	}
	return recorder
}

func (t *TokenUsageRecorder) clock() time.Time {
	if t.now != nil {
		return t.now()
	}
	return time.Now()
}

// Record counts one finished request. A request whose usage could not be read
// still counts, in requests and in requests_without_usage, so missing data is
// visible instead of looking like a zero-token request.
func (t *TokenUsageRecorder) Record(provider, accountID, model, client string, usage tokenUsage, ok bool) {
	if t == nil {
		return
	}
	counts := tokenUsageCounts{Requests: 1}
	if ok {
		counts.InputTokens = usage.InputTokens
		counts.CachedInputTokens = usage.CachedInputTokens
		counts.CacheWriteInputTokens = usage.CacheWriteInputTokens
		counts.OutputTokens = usage.OutputTokens
		counts.ReasoningOutputTokens = usage.ReasoningOutputTokens
	} else {
		counts.RequestsWithoutUsage = 1
	}
	key := tokenUsageKey{
		Hour:      t.clock().UTC().Truncate(time.Hour).Unix(),
		Provider:  tokenUsageLabel(provider, "unknown"),
		AccountID: tokenUsageLabel(accountID, "unknown"),
		Model:     tokenUsageLabel(strings.ToLower(model), "unknown"),
		Client:    tokenUsageLabel(client, tokenUsageUnknownClient),
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	hour := t.hours[key.Hour]
	if hour == nil {
		hour = &tokenUsageHour{total: map[tokenUsageKey]tokenUsageCounts{}, pending: map[tokenUsageKey]tokenUsageCounts{}}
		t.hours[key.Hour] = hour
		if t.path == "" {
			// Without a log, memory is the only store: keep it to the
			// retention window.
			cutoff := key.Hour - int64(tokenUsageRetention/time.Second)
			for hourStart := range t.hours {
				if hourStart < cutoff {
					delete(t.hours, hourStart)
				}
			}
		}
	}
	if _, exists := hour.total[key]; !exists && len(hour.total) >= tokenUsageMaxKeysPerHour {
		// Past the cap, new model/client combinations share one row per
		// account, so a flood of distinct labels cannot grow memory.
		key.Model = tokenUsageOverflowLabel
		key.Client = tokenUsageOverflowLabel
	}
	total := hour.total[key]
	total.add(counts)
	hour.total[key] = total
	pending := hour.pending[key]
	pending.add(counts)
	hour.pending[key] = pending
}

func tokenUsageLabel(value, fallback string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return fallback
	}
	if len(value) > 128 {
		value = value[:128]
	}
	return value
}

// Flush appends pending deltas to the log, then compacts once per hour. It is
// safe to call concurrently and from shutdown.
func (t *TokenUsageRecorder) Flush() error {
	if t == nil || t.path == "" {
		return nil
	}
	now := t.clock().UTC()
	currentHour := now.Truncate(time.Hour).Unix()
	t.mu.Lock()
	var rows []TokenUsageRow
	for hourStart, hour := range t.hours {
		for key, counts := range hour.pending {
			if !counts.zero() {
				rows = append(rows, tokenUsageRowFor(key, counts))
			}
		}
		hour.pending = map[tokenUsageKey]tokenUsageCounts{}
		// Keep the previous hour's key set so the cap still applies to
		// requests that finish just after the rollover.
		if hourStart < currentHour-int64(time.Hour/time.Second) {
			delete(t.hours, hourStart)
		}
	}
	t.mu.Unlock()
	if err := t.appendRows(rows); err != nil {
		t.restorePending(rows)
		return err
	}
	t.fileMu.Lock()
	needsCompaction := t.lastCompactedHr != currentHour
	t.fileMu.Unlock()
	if needsCompaction {
		return t.compact()
	}
	return nil
}

// restorePending puts rows that failed to write back into pending so the
// next flush retries them.
func (t *TokenUsageRecorder) restorePending(rows []TokenUsageRow) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, row := range rows {
		key, counts, ok := tokenUsageKeyForRow(row)
		if !ok {
			continue
		}
		hour := t.hours[key.Hour]
		if hour == nil {
			hour = &tokenUsageHour{total: map[tokenUsageKey]tokenUsageCounts{}, pending: map[tokenUsageKey]tokenUsageCounts{}}
			t.hours[key.Hour] = hour
		}
		pending := hour.pending[key]
		pending.add(counts)
		hour.pending[key] = pending
	}
}

func (t *TokenUsageRecorder) appendRows(rows []TokenUsageRow) error {
	if len(rows) == 0 {
		return nil
	}
	var buffer bytes.Buffer
	for _, row := range rows {
		line, err := json.Marshal(row)
		if err != nil {
			return err
		}
		buffer.Write(line)
		buffer.WriteByte('\n')
	}
	t.fileMu.Lock()
	defer t.fileMu.Unlock()
	if err := os.MkdirAll(filepath.Dir(t.path), 0o700); err != nil {
		return err
	}
	file, err := os.OpenFile(t.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := file.Write(buffer.Bytes()); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

// readFileLocked sums the log into one count per key, dropping rows older
// than the cutoff. The caller holds fileMu.
func (t *TokenUsageRecorder) readFileLocked(cutoff int64) (map[tokenUsageKey]tokenUsageCounts, error) {
	merged := map[tokenUsageKey]tokenUsageCounts{}
	file, err := os.Open(t.path)
	if err != nil {
		return merged, err
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var row TokenUsageRow
		if json.Unmarshal(line, &row) != nil {
			continue
		}
		key, counts, ok := tokenUsageKeyForRow(row)
		if !ok || key.Hour < cutoff {
			continue
		}
		existing := merged[key]
		existing.add(counts)
		merged[key] = existing
	}
	return merged, scanner.Err()
}

// compact rewrites the log as one row per key within the retention window.
func (t *TokenUsageRecorder) compact() error {
	now := t.clock().UTC()
	cutoff := now.Add(-tokenUsageRetention).Truncate(time.Hour).Unix()
	t.fileMu.Lock()
	defer t.fileMu.Unlock()
	merged, err := t.readFileLocked(cutoff)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			t.lastCompactedHr = now.Truncate(time.Hour).Unix()
		}
		return err
	}
	rows := sortedTokenUsageRows(merged, 0)
	var buffer bytes.Buffer
	for _, row := range rows {
		line, err := json.Marshal(row)
		if err != nil {
			return err
		}
		buffer.Write(line)
		buffer.WriteByte('\n')
	}
	temp, err := os.CreateTemp(filepath.Dir(t.path), ".token-usage-*.jsonl")
	if err != nil {
		return err
	}
	tempName := temp.Name()
	if _, err := temp.Write(buffer.Bytes()); err != nil {
		_ = temp.Close()
		_ = os.Remove(tempName)
		return err
	}
	if err := temp.Chmod(0o600); err != nil {
		_ = temp.Close()
		_ = os.Remove(tempName)
		return err
	}
	if err := temp.Close(); err != nil {
		_ = os.Remove(tempName)
		return err
	}
	if err := os.Rename(tempName, t.path); err != nil {
		_ = os.Remove(tempName)
		return err
	}
	t.lastCompactedHr = now.Truncate(time.Hour).Unix()
	return nil
}

// RunFlushLoop flushes on a timer for the life of the process. Shutdown calls
// Flush directly so the last few minutes are written as well.
func (t *TokenUsageRecorder) RunFlushLoop(ctx context.Context) {
	if t == nil || t.path == "" {
		return
	}
	t.flushLoopStarted.Do(func() {
		go func() {
			ticker := time.NewTicker(tokenUsageFlushInterval)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					_ = t.Flush()
					return
				case <-ticker.C:
					_ = t.Flush()
				}
			}
		}()
	})
}

// Rows returns every row whose hour starts at or after since, merging the
// log with what this process has not written yet.
func (t *TokenUsageRecorder) Rows(since time.Time) ([]TokenUsageRow, error) {
	if t == nil {
		return []TokenUsageRow{}, nil
	}
	cutoff := since.UTC().Truncate(time.Hour).Unix()
	merged := map[tokenUsageKey]tokenUsageCounts{}
	if t.path != "" {
		t.fileMu.Lock()
		fromFile, err := t.readFileLocked(cutoff)
		t.fileMu.Unlock()
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		merged = fromFile
	}
	t.mu.Lock()
	for hourStart, hour := range t.hours {
		if hourStart < cutoff {
			continue
		}
		source := hour.pending
		if t.path == "" {
			source = hour.total
		}
		for key, counts := range source {
			existing := merged[key]
			existing.add(counts)
			merged[key] = existing
		}
	}
	t.mu.Unlock()
	return sortedTokenUsageRows(merged, cutoff), nil
}

func sortedTokenUsageRows(merged map[tokenUsageKey]tokenUsageCounts, cutoff int64) []TokenUsageRow {
	keys := make([]tokenUsageKey, 0, len(merged))
	for key, counts := range merged {
		if key.Hour >= cutoff && !counts.zero() {
			keys = append(keys, key)
		}
	}
	sort.Slice(keys, func(i, j int) bool {
		a, b := keys[i], keys[j]
		if a.Hour != b.Hour {
			return a.Hour < b.Hour
		}
		if a.Provider != b.Provider {
			return a.Provider < b.Provider
		}
		if a.AccountID != b.AccountID {
			return a.AccountID < b.AccountID
		}
		if a.Model != b.Model {
			return a.Model < b.Model
		}
		return a.Client < b.Client
	})
	rows := make([]TokenUsageRow, 0, len(keys))
	for _, key := range keys {
		rows = append(rows, tokenUsageRowFor(key, merged[key]))
	}
	return rows
}

// NormalizeClientName validates a self-reported client name: 1-64 characters
// from [A-Za-z0-9._-]. Anything else is ignored rather than cleaned up, so a
// label on the dashboard is always exactly what a client sent.
func NormalizeClientName(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > tokenUsageMaxClientLength {
		return ""
	}
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
		default:
			return ""
		}
	}
	return value
}

var tailnetPrefixes = []netip.Prefix{
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("fd7a:115c:a1e0::/48"),
}

func tailnetRemoteAddr(remoteAddr string) bool {
	host := clientRemoteIPFromAddr(remoteAddr)
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return false
	}
	addr = addr.Unmap()
	for _, prefix := range tailnetPrefixes {
		if prefix.Contains(addr) {
			return true
		}
	}
	return false
}

func clientRemoteIPFromAddr(remoteAddr string) string {
	remoteAddr = strings.TrimSpace(remoteAddr)
	if host, _, err := net.SplitHostPort(remoteAddr); err == nil {
		return host
	}
	return strings.Trim(remoteAddr, "[]")
}

// tokenUsageClient resolves who sent a request, in order: the X-Subrouter-Client
// header, a hash of the user email header, the tailnet node name behind the
// connection, then "unknown". The returned function may block on a tailnet
// lookup, so callers run it off the response path.
func (t *TokenUsageRecorder) tokenUsageClient(r *http.Request, userEmail string) (resolve func() string, blocking bool) {
	if r != nil {
		if name := NormalizeClientName(r.Header.Get(ClientNameHeader)); name != "" {
			return func() string { return name }, false
		}
	}
	if hash := userEmailHash(userEmail); hash != "" {
		return func() string { return "user:" + hash }, false
	}
	if t == nil || t.whois == nil || r == nil || !tailnetRemoteAddr(r.RemoteAddr) {
		return func() string { return tokenUsageUnknownClient }, false
	}
	host := clientRemoteIPFromAddr(r.RemoteAddr)
	if name, found := t.cachedClient(host); found {
		return func() string { return name }, false
	}
	remoteAddr := r.RemoteAddr
	return func() string {
		// A long-lived connection may resolve more than once; the first
		// answer is cached for the rest.
		if name, found := t.cachedClient(host); found {
			return name
		}
		ctx, cancel := context.WithTimeout(context.Background(), tokenUsageWhoIsTimeout)
		defer cancel()
		name := tokenUsageUnknownClient
		if identity, ok := t.whois.Lookup(ctx, remoteAddr); ok {
			if node := tailnetNodeLabel(identity.NodeName); node != "" {
				name = node
			}
		}
		t.storeClient(host, name)
		return name
	}, true
}

// tailnetNodeLabel reduces a MagicDNS name to its first label, which is the
// machine name and matches what `sr` reports by default.
func tailnetNodeLabel(nodeName string) string {
	nodeName = strings.TrimSuffix(strings.TrimSpace(nodeName), ".")
	if index := strings.IndexByte(nodeName, '.'); index > 0 {
		nodeName = nodeName[:index]
	}
	return NormalizeClientName(nodeName)
}

func (t *TokenUsageRecorder) cachedClient(host string) (string, bool) {
	t.clientCacheMu.Lock()
	defer t.clientCacheMu.Unlock()
	entry, found := t.clientCache[host]
	if !found || t.clock().After(entry.expires) {
		return "", false
	}
	return entry.name, true
}

func (t *TokenUsageRecorder) storeClient(host, name string) {
	t.clientCacheMu.Lock()
	defer t.clientCacheMu.Unlock()
	if len(t.clientCache) >= tokenUsageClientCacheN {
		t.clientCache = map[string]tokenUsageClientEntry{}
	}
	t.clientCache[host] = tokenUsageClientEntry{name: name, expires: t.clock().Add(tokenUsageClientTTL)}
}

// recordWithClient records once the client is known, off the caller's
// goroutine when that needs a tailnet lookup.
func (t *TokenUsageRecorder) recordWithClient(provider, accountID, model string, resolve func() string, blocking bool, usage tokenUsage, ok bool) {
	if t == nil {
		return
	}
	if !blocking {
		t.Record(provider, accountID, model, resolve(), usage, ok)
		return
	}
	go t.Record(provider, accountID, model, resolve(), usage, ok)
}

// wrapTokenUsageBody installs usage accounting on a final HTTP response.
// Only successful model turns are counted.
func (s Server) wrapTokenUsageBody(response *http.Response, r *http.Request, userEmail, requestModel string, account accounts.Account) {
	if s.TokenUsage == nil || response == nil || response.Body == nil || r == nil ||
		response.StatusCode < 200 || response.StatusCode >= 300 ||
		!tokenUsageCountedRequest(r.Method, r.URL.Path) {
		return
	}
	provider := account.Provider
	if provider == "" {
		provider = accounts.ProviderCodex
	}
	resolve, blocking := s.TokenUsage.tokenUsageClient(r, userEmail)
	recorder := s.TokenUsage
	accountID := account.ID
	if accountID == "" {
		// A retry layer answered from outside the pool (the Azure or Fable
		// fallback) and tagged the response with no account.
		accountID = tokenUsageFallbackAccount
	}
	response.Body = newTokenUsageBody(response.Body, response.Header.Get("Content-Type"), response.Header.Get("Content-Encoding"), func(usage tokenUsage, responseModel string, ok bool) {
		model := responseModel
		if model == "" {
			model = requestModel
		}
		recorder.recordWithClient(string(provider), accountID, model, resolve, blocking, usage, ok)
	})
}

// handleTokenUsage serves GET /_subrouter/token-usage?since=24h|RFC3339.
func (s Server) handleTokenUsage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	now := time.Now().UTC()
	if s.TokenUsage != nil {
		now = s.TokenUsage.clock().UTC()
	}
	since, err := parseTokenUsageSince(r.URL.Query().Get("since"), now)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	rows, err := s.TokenUsage.Rows(since)
	if err != nil {
		http.Error(w, "read token usage: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, struct {
		Since       string          `json:"since"`
		GeneratedAt string          `json:"generated_at"`
		Rows        []TokenUsageRow `json:"rows"`
	}{
		Since:       since.Format(time.RFC3339),
		GeneratedAt: now.Format(time.RFC3339),
		Rows:        rows,
	})
}

// parseTokenUsageSince accepts a Go duration ("24h", "90m") or an RFC3339
// time, defaults to 24h, and clamps to the 30-day retention window.
func parseTokenUsageSince(value string, now time.Time) (time.Time, error) {
	value = strings.TrimSpace(value)
	oldest := now.Add(-tokenUsageRetention)
	since := now.Add(-tokenUsageDefaultSince)
	if value != "" {
		if duration, err := time.ParseDuration(value); err == nil {
			if duration < 0 {
				return time.Time{}, errors.New("since must be a positive duration or an RFC3339 time")
			}
			since = now.Add(-duration)
		} else if parsed, err := time.Parse(time.RFC3339, value); err == nil {
			since = parsed.UTC()
		} else {
			return time.Time{}, errors.New("since must be a duration like 24h or an RFC3339 time")
		}
	}
	if since.Before(oldest) {
		since = oldest
	}
	return since.UTC().Truncate(time.Hour), nil
}

// recordWebSocketTokenUsage counts a Codex WebSocket turn when its terminal
// event arrives. A turn is one response.create, so a connection that runs many
// turns counts each of them.
func (s Server) recordWebSocketTokenUsage(provider accounts.Provider, accountID string, modelState *webSocketModelState, poolModel string, body []byte) {
	if s.TokenUsage == nil || modelState == nil || modelState.usageClient == nil {
		return
	}
	usage, responseModel, ok, terminal := tokenUsageFromWebSocketMessage(body)
	if !terminal {
		return
	}
	model := responseModel
	if model == "" {
		model = webSocketTurnModel(modelState, poolModel)
	}
	resolve, blocking := modelState.usageClient, modelState.usageClientBlocking
	s.TokenUsage.recordWithClient(string(provider), accountID, model, resolve, blocking, usage, ok)
}
