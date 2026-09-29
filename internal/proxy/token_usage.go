package proxy

import (
	"bufio"
	"bytes"
	"container/list"
	"context"
	"encoding/json"
	"errors"
	"hash/maphash"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/internal/tailnet"
	"github.com/manaflow-ai/subrouter/session"
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
	tokenUsageLineTailBytes   = 32 << 10
	// tokenUsageLineKeepBytes is how much of an ordinary SSE line is kept;
	// a longer one keeps only this head and its tail. It is also the buffer
	// a scanner keeps between lines.
	tokenUsageLineKeepBytes = 64 << 10
	// tokenUsageWholeLineBytes bounds how much of a terminal SSE event (or a
	// JSON body) is kept whole and decoded as JSON. A Codex
	// response.completed event echoes the whole response object (output
	// items with encrypted reasoning, tool calls, instructions, tools), so on
	// long agentic turns it runs to hundreds of KiB, and nothing fixes where
	// its usage sits relative to the end. Past this bound the event falls
	// back to its head and tail.
	tokenUsageWholeLineBytes = 4 << 20
	tokenUsageClientTTL      = 10 * time.Minute
	tokenUsageClientCacheN   = 1024
	tokenUsageWhoIsTimeout   = 3 * time.Second
	// tokenUsageMaxLabelKeys bounds the distinct stop reasons and error
	// statuses one row keeps.
	tokenUsageMaxLabelKeys = 16
	// tokenUsageSwitchWindow is how recent a session's previous turn must be
	// for a move to another account to count as a switch. Prompt caches
	// live minutes to an hour, so a session idle longer had nothing to lose.
	tokenUsageSwitchWindow = time.Hour
	// tokenUsageMaxSessions bounds the per-session account memory; past it
	// the least recently seen session is forgotten.
	tokenUsageMaxSessions = 20000
	// tokenUsageSessionAccounts is how many recent serving accounts each
	// (session, model) remembers. Concurrent requests of one session may
	// finish on different accounts; each account with a warm cache is kept
	// so that finishing out of order is not counted as a switch again.
	tokenUsageSessionAccounts = 4
	// tokenUsageMaxLabelLength bounds a stop reason label.
	tokenUsageMaxLabelLength = 48
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
	Type  string          `json:"type"`
	Model string          `json:"model"`
	Usage *tokenUsageWire `json:"usage"`
	// Object, Status and IncompleteDetails describe a non-streamed Responses
	// body; StopReason a non-streamed Anthropic one.
	Object            string                     `json:"object"`
	Status            string                     `json:"status"`
	IncompleteDetails *tokenUsageIncomplete      `json:"incomplete_details"`
	StopReason        string                     `json:"stop_reason"`
	Delta             json.RawMessage            `json:"delta"`
	Choices           []tokenUsageFinishedChoice `json:"choices"`
	Response          *struct {
		Model             string                `json:"model"`
		Status            string                `json:"status"`
		IncompleteDetails *tokenUsageIncomplete `json:"incomplete_details"`
		Usage             *tokenUsageWire       `json:"usage"`
	} `json:"response"`
	Message *struct {
		Model      string          `json:"model"`
		StopReason string          `json:"stop_reason"`
		Usage      *tokenUsageWire `json:"usage"`
	} `json:"message"`
}

type tokenUsageIncomplete struct {
	Reason string `json:"reason"`
}

type tokenUsageFinishedChoice struct {
	FinishReason string `json:"finish_reason"`
}

// stopReason is why the response ended, as the provider names it: an
// Anthropic stop_reason, a chat completion finish_reason, or a Responses
// status ("incomplete:<reason>" when the status is incomplete). It is "" for
// an event that does not end a response.
func (e tokenUsageEvent) stopReason() string {
	switch strings.ToLower(e.Type) {
	case "response.completed", "response.done", "response.incomplete", "response.failed":
		status := strings.TrimPrefix(strings.ToLower(e.Type), "response.")
		var details *tokenUsageIncomplete
		if e.Response != nil {
			if e.Response.Status != "" {
				status = e.Response.Status
			}
			details = e.Response.IncompleteDetails
		}
		return responsesStopReason(status, details)
	case "message_delta":
		if len(e.Delta) > 0 && e.Delta[0] == '{' {
			var delta struct {
				StopReason string `json:"stop_reason"`
			}
			if json.Unmarshal(e.Delta, &delta) == nil {
				return delta.StopReason
			}
		}
		return ""
	}
	if e.StopReason != "" {
		return e.StopReason
	}
	if e.Message != nil && e.Message.StopReason != "" {
		return e.Message.StopReason
	}
	for _, choice := range e.Choices {
		if choice.FinishReason != "" {
			return choice.FinishReason
		}
	}
	if e.Object == "response" && e.Status != "" {
		return responsesStopReason(e.Status, e.IncompleteDetails)
	}
	return ""
}

func responsesStopReason(status string, details *tokenUsageIncomplete) string {
	status = strings.ToLower(strings.TrimSpace(status))
	if status == "incomplete" && details != nil && strings.TrimSpace(details.Reason) != "" {
		return status + ":" + strings.TrimSpace(details.Reason)
	}
	return status
}

// tokenUsageMayCarryStop is a byte scan for a non-null stop_reason or
// finish_reason key. A chat completion stream names its finish reason in a
// chunk that carries no usage.
func tokenUsageMayCarryStop(payload []byte) bool {
	return bytes.Contains(payload, []byte(`_reason":"`)) || bytes.Contains(payload, []byte(`_reason": "`))
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
	// modelHint is the first model named by an event that was skipped
	// without decoding; it labels a response whose usage never arrives.
	modelHint string
	// stop is the last stop or finish reason seen.
	stop string
}

func (a *tokenUsageAccumulator) noteStop(reason string) {
	if reason = strings.TrimSpace(reason); reason != "" {
		a.stop = reason
	}
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

// observeJSON decodes one complete JSON payload. A payload that never names
// usage is skipped without decoding: most SSE events are deltas, and the
// events that echo the request (instructions, tools) are the largest.
func (a *tokenUsageAccumulator) observeJSON(payload []byte) {
	if !bytes.Contains(payload, []byte(`"usage"`)) && !tokenUsageMayCarryStop(payload) {
		if a.modelHint == "" {
			a.modelHint = firstJSONStringField(payload, "model")
		}
		return
	}
	var event tokenUsageEvent
	if json.Unmarshal(payload, &event) != nil {
		return
	}
	a.noteStop(event.stopReason())
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
	a.noteStop(truncatedStopReason(eventType, head))
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

// truncatedStopReason reads a Responses terminal event's status from its
// head, where the response object names it before any output.
func truncatedStopReason(eventType string, head []byte) string {
	switch strings.ToLower(eventType) {
	case "response.completed", "response.done", "response.incomplete", "response.failed":
	default:
		return ""
	}
	status := strings.TrimPrefix(strings.ToLower(eventType), "response.")
	if index := bytes.Index(head, []byte(`"response":`)); index >= 0 {
		if found := firstJSONStringField(head[index:], "status"); found != "" {
			status = found
		}
	}
	var details *tokenUsageIncomplete
	if index := bytes.Index(head, []byte(`"incomplete_details":{`)); index >= 0 {
		details = &tokenUsageIncomplete{Reason: firstJSONStringField(head[index:], "reason")}
	}
	return responsesStopReason(status, details)
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
	model := a.model
	if model == "" {
		model = a.modelHint
	}
	return a.usage, model, a.got
}

// tokenUsageScanner watches a response body as it streams past. An SSE line
// keeps at most its first tokenUsageLineKeepBytes and a tail, except a
// terminal event, which is kept whole up to wholeMax because that is where
// the usage is; a JSON body is kept whole up to wholeMax. A long stream costs
// a bounded amount of memory.
//
// Write runs before the bytes reach the client, so it only decodes lines
// that fit the keep size. A terminal event larger than that is decoded in
// Finish, at the end of the body.
type tokenUsageScanner struct {
	sse bool
	// sniff is set while the body kind is still open: the Content-Type was
	// absent or neither JSON nor an event stream (chatgpt.com sends Codex
	// streams with no Content-Type), so the first non-whitespace byte
	// decides.
	sniff    bool
	wholeMax int
	head     []byte
	tail     []byte
	lineLen  int
	// decided and whole record, once a line outgrows the keep size, whether
	// it is a terminal event to keep whole. Past the head, a whole line's
	// bytes are copied into pieces, so it costs its own size rather than a
	// growing buffer's.
	decided   bool
	whole     bool
	pieces    [][]byte
	piecesLen int
	// pending is a large terminal event waiting for Finish.
	pending []byte
	acc     tokenUsageAccumulator
}

func newTokenUsageScanner(contentType string) *tokenUsageScanner {
	return newTokenUsageScannerWithLimit(contentType, tokenUsageWholeLineBytes)
}

func newTokenUsageScannerWithLimit(contentType string, wholeMax int) *tokenUsageScanner {
	if wholeMax < tokenUsageLineKeepBytes {
		wholeMax = tokenUsageLineKeepBytes
	}
	return &tokenUsageScanner{
		sse:      strings.Contains(strings.ToLower(contentType), "text/event-stream"),
		sniff:    contentTypeNeedsSniff(contentType),
		wholeMax: wholeMax,
	}
}

// tokenUsageTerminalEvent reports whether an SSE event type carries a
// response's final usage.
func tokenUsageTerminalEvent(eventType string) bool {
	switch eventType {
	case "response.completed", "response.incomplete", "response.failed", "response.done", "message_delta":
		return true
	}
	return false
}

func (s *tokenUsageScanner) Write(chunk []byte) {
	if s.sniff {
		// Leading whitespace means nothing to either kind.
		chunk = bytes.TrimLeft(chunk, " \t\r\n")
		if len(chunk) == 0 {
			return
		}
		s.sniff = false
		s.sse = chunk[0] != '{' && chunk[0] != '['
	}
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
	limit := tokenUsageLineKeepBytes
	if !s.sse {
		limit = s.wholeMax
	}
	if room := limit - len(s.head); room > 0 {
		take := min(room, len(chunk))
		if s.sse && len(s.head)+take > cap(s.head) && len(s.head)+take > 4<<10 {
			// A long line: size the head once instead of doubling up to it.
			s.head = append(make([]byte, 0, limit), s.head...)
		}
		s.head = append(s.head, chunk[:take]...)
		chunk = chunk[take:]
	}
	if len(chunk) == 0 {
		return
	}
	if s.sse && !s.decided {
		// The line outgrew the keep size: its head names the event type.
		s.decided = true
		s.whole = bytes.HasPrefix(s.head, []byte("data:")) &&
			tokenUsageTerminalEvent(firstJSONStringField(s.head, "type"))
	}
	if s.whole {
		if len(s.head)+s.piecesLen+len(chunk) <= s.wholeMax {
			s.pieces = append(s.pieces, bytes.Clone(chunk))
			s.piecesLen += len(chunk)
			return
		}
		// Too large to keep whole: fall back to the head and a tail seeded
		// from the pieces' end.
		s.whole = false
		for _, piece := range s.pieces {
			s.appendTail(piece)
		}
		s.pieces, s.piecesLen = nil, 0
	}
	s.appendTail(chunk)
}

// appendTail keeps a rolling tail of the line's last bytes, seeded from the
// end of the head so it is contiguous. It may grow to twice its size before
// being cut back, so trimming is amortized rather than a copy per chunk.
func (s *tokenUsageScanner) appendTail(chunk []byte) {
	if s.tail == nil {
		s.tail = make([]byte, 0, 2*tokenUsageLineTailBytes)
	}
	if len(s.tail) == 0 {
		seed := s.head
		if len(seed) > tokenUsageLineTailBytes {
			seed = seed[len(seed)-tokenUsageLineTailBytes:]
		}
		s.tail = append(s.tail, seed...)
	}
	if len(chunk) > tokenUsageLineTailBytes {
		chunk = chunk[len(chunk)-tokenUsageLineTailBytes:]
		s.tail = s.tail[:0]
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
	if !bytes.HasPrefix(s.head, []byte("data:")) {
		return
	}
	if s.whole {
		// A large terminal event, complete: decode it at the end of the
		// body, off the client's path. One copy joins it, since the head
		// buffer is reused for the next line.
		line := make([]byte, 0, len(s.head)+s.piecesLen)
		line = append(line, s.head...)
		for _, piece := range s.pieces {
			line = append(line, piece...)
		}
		payload := bytes.TrimSpace(bytes.TrimRight(line, "\r")[len("data:"):])
		s.flushPending()
		s.pending = payload
		return
	}
	line := bytes.TrimRight(s.head, "\r")
	if s.lineLen > len(s.head) {
		s.acc.observeTruncated(line, bytes.TrimRight(s.tailBytes(), "\r"))
		return
	}
	payload := bytes.TrimSpace(line[len("data:"):])
	if len(payload) == 0 || payload[0] != '{' {
		return
	}
	s.acc.observeJSON(payload)
}

func (s *tokenUsageScanner) flushPending() {
	if s.pending != nil {
		s.acc.observeJSON(s.pending)
		s.pending = nil
	}
}

func (s *tokenUsageScanner) resetLine() {
	if cap(s.head) > 2*tokenUsageLineKeepBytes {
		// Do not pin a large JSON body's buffer.
		s.head = nil
	}
	s.head = s.head[:0]
	s.tail = s.tail[:0]
	s.lineLen = 0
	s.decided, s.whole = false, false
	s.pieces, s.piecesLen = nil, 0
}

// Finish returns the usage seen, draining any unterminated SSE line, a
// pending terminal event, or the buffered JSON body.
func (s *tokenUsageScanner) Finish() (tokenUsage, string, bool) {
	if s.sse {
		if s.lineLen > 0 {
			s.finishLine()
		}
		s.flushPending()
		return s.acc.result()
	}
	if s.lineLen <= len(s.head) {
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

// StopReason is the last stop or finish reason the body named. Call it after
// Finish.
func (s *tokenUsageScanner) StopReason() string {
	return s.acc.stop
}

// tokenUsageFromWebSocketMessage reads a Codex WebSocket event. It reports
// whether the event ends a turn, since that is when a turn is counted.
func tokenUsageFromWebSocketMessage(body []byte) (usage tokenUsage, model string, ok bool, terminal bool) {
	usage, model, _, ok, terminal = tokenUsageTurnFromWebSocketMessage(body)
	return usage, model, ok, terminal
}

// tokenUsageTurnFromWebSocketMessage is tokenUsageFromWebSocketMessage plus
// the turn's stop reason.
func tokenUsageTurnFromWebSocketMessage(body []byte) (usage tokenUsage, model, stop string, ok bool, terminal bool) {
	if len(body) == 0 || !bytes.Contains(body, []byte(`"response.`)) {
		return tokenUsage{}, "", "", false, false
	}
	var event tokenUsageEvent
	if json.Unmarshal(body, &event) != nil {
		return tokenUsage{}, "", "", false, false
	}
	switch strings.ToLower(event.Type) {
	case "response.completed", "response.incomplete", "response.failed", "response.done":
	default:
		return tokenUsage{}, "", "", false, false
	}
	var acc tokenUsageAccumulator
	wire, eventModel := event.usageAndModel()
	acc.observe(event.Type, wire, eventModel)
	usage, model, ok = acc.result()
	return usage, model, event.stopReason(), ok, true
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

// tokenUsageBody wraps a response body and records its usage once, when the
// body ends or is closed. Reads pass through untouched; the body notes when
// its first byte arrived and when it ended.
type tokenUsageBody struct {
	io.ReadCloser
	scanner   *tokenUsageScanner
	record    func(tokenUsageBodyResult)
	firstByte time.Time
	once      sync.Once
}

// tokenUsageBodyResult is what a finished body reports.
type tokenUsageBodyResult struct {
	usage     tokenUsage
	model     string
	ok        bool
	stop      string
	firstByte time.Time
	finished  time.Time
}

func newTokenUsageBody(inner io.ReadCloser, contentType string, record func(tokenUsageBodyResult)) io.ReadCloser {
	return &tokenUsageBody{ReadCloser: inner, scanner: newTokenUsageScanner(contentType), record: record}
}

func (b *tokenUsageBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if n > 0 {
		if b.firstByte.IsZero() {
			b.firstByte = time.Now()
		}
		b.scanner.Write(p[:n])
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
		usage, model, ok := b.scanner.Finish()
		b.record(tokenUsageBodyResult{
			usage: usage, model: model, ok: ok, stop: b.scanner.StopReason(),
			firstByte: b.firstByte, finished: time.Now(),
		})
	})
}

// TokenUsageLatencyBoundsMs are the upper bounds, in milliseconds, of the
// latency histogram buckets in a row's ttfb_ms_buckets and
// duration_ms_buckets. The last bucket, past the final bound, has no upper
// bound. A row's bucket slice drops trailing empty buckets.
var TokenUsageLatencyBoundsMs = [...]int64{250, 500, 1000, 2000, 4000, 8000, 16000, 32000, 64000, 128000, 256000}

const tokenUsageLatencyBuckets = len(TokenUsageLatencyBoundsMs) + 1

// tokenUsageLatency aggregates one latency: a count, a sum and a max, and
// coarse buckets from which a percentile can be estimated. Every part sums
// (or, for the max, maxes) across delta rows, so it aggregates safely.
type tokenUsageLatency struct {
	Count   int64
	SumMs   int64
	MaxMs   int64
	Buckets [tokenUsageLatencyBuckets]int64
}

func (l *tokenUsageLatency) observe(elapsed time.Duration) {
	if elapsed < 0 {
		elapsed = 0
	}
	ms := elapsed.Milliseconds()
	l.Count++
	l.SumMs += ms
	l.MaxMs = max(l.MaxMs, ms)
	bucket := len(TokenUsageLatencyBoundsMs)
	for index, bound := range TokenUsageLatencyBoundsMs {
		if ms <= bound {
			bucket = index
			break
		}
	}
	l.Buckets[bucket]++
}

func (l *tokenUsageLatency) add(other tokenUsageLatency) {
	l.Count += other.Count
	l.SumMs += other.SumMs
	l.MaxMs = max(l.MaxMs, other.MaxMs)
	for index := range l.Buckets {
		l.Buckets[index] += other.Buckets[index]
	}
}

// bucketSlice is the row form: nil when empty, trailing empty buckets cut.
func (l tokenUsageLatency) bucketSlice() []int64 {
	end := len(l.Buckets)
	for end > 0 && l.Buckets[end-1] == 0 {
		end--
	}
	if end == 0 {
		return nil
	}
	return append([]int64(nil), l.Buckets[:end]...)
}

func tokenUsageLatencyFromRow(count, sumMs, maxMs int64, buckets []int64) tokenUsageLatency {
	latency := tokenUsageLatency{Count: count, SumMs: sumMs, MaxMs: maxMs}
	for index, value := range buckets {
		// A longer slice, from a build with more buckets, folds into the
		// open-ended last one.
		latency.Buckets[min(index, tokenUsageLatencyBuckets-1)] += value
	}
	return latency
}

// TokenUsageLatencyQuantileMs estimates a latency percentile (q in (0, 1])
// from a row's buckets: the upper bound of the bucket that holds it, capped
// at the observed max. It reports false when the buckets are empty.
func TokenUsageLatencyQuantileMs(buckets []int64, maxMs int64, q float64) (int64, bool) {
	var total int64
	for _, value := range buckets {
		total += value
	}
	if total <= 0 {
		return 0, false
	}
	rank := int64(q*float64(total) + 0.999999)
	rank = min(max(rank, 1), total)
	var seen int64
	for index, value := range buckets {
		seen += value
		if seen < rank {
			continue
		}
		if index < len(TokenUsageLatencyBoundsMs) && (maxMs <= 0 || TokenUsageLatencyBoundsMs[index] < maxMs) {
			return TokenUsageLatencyBoundsMs[index], true
		}
		return maxMs, true
	}
	return maxMs, true
}

// TokenUsageRow is one aggregated row, both on disk and in the endpoint.
// Fields after reasoning_output_tokens are omitted when zero, so rows from
// before they existed and rows that never saw them read the same.
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
	// AccountSwitches counts turns served by an account outside the set of
	// accounts that served the same (session, model) within
	// tokenUsageSwitchWindow, so they could not read a warm prompt cache.
	// AccountSwitchInputTokens is the cold part of those turns' input (input
	// minus cache reads). AccountSwitchesInRequest is the part a
	// retry layer caused mid-request (failover after the placed account
	// failed); the rest were moved at placement (eviction, rebalance, a
	// reconnect after a failed turn).
	AccountSwitches          int64 `json:"account_switches,omitempty"`
	AccountSwitchInputTokens int64 `json:"account_switch_input_tokens,omitempty"`
	AccountSwitchesInRequest int64 `json:"account_switches_in_request,omitempty"`
	// Time to the first response body byte, and to the end of the body (or
	// of the WebSocket turn), from when the request arrived.
	TTFBCount         int64   `json:"ttfb_count,omitempty"`
	TTFBMsSum         int64   `json:"ttfb_ms_sum,omitempty"`
	TTFBMsMax         int64   `json:"ttfb_ms_max,omitempty"`
	TTFBMsBuckets     []int64 `json:"ttfb_ms_buckets,omitempty"`
	DurationCount     int64   `json:"duration_count,omitempty"`
	DurationMsSum     int64   `json:"duration_ms_sum,omitempty"`
	DurationMsMax     int64   `json:"duration_ms_max,omitempty"`
	DurationMsBuckets []int64 `json:"duration_ms_buckets,omitempty"`
	// StopReasons counts turns by the provider's stop or finish reason.
	StopReasons map[string]int64 `json:"stop_reasons,omitempty"`
	// UpstreamErrors counts model requests whose final upstream response was
	// not 2xx, by status. Those requests are not in requests.
	UpstreamErrors map[string]int64 `json:"upstream_errors,omitempty"`
}

type tokenUsageKey struct {
	Hour      int64
	Provider  string
	AccountID string
	Model     string
	Client    string
}

type tokenUsageCounts struct {
	Requests                 int64
	RequestsWithoutUsage     int64
	InputTokens              int64
	CachedInputTokens        int64
	CacheWriteInputTokens    int64
	OutputTokens             int64
	ReasoningOutputTokens    int64
	AccountSwitches          int64
	AccountSwitchInputTokens int64
	AccountSwitchesInRequest int64
	TTFB                     tokenUsageLatency
	Duration                 tokenUsageLatency
	StopReasons              map[string]int64
	UpstreamErrors           map[string]int64
}

func (c *tokenUsageCounts) add(other tokenUsageCounts) {
	c.Requests += other.Requests
	c.RequestsWithoutUsage += other.RequestsWithoutUsage
	c.InputTokens += other.InputTokens
	c.CachedInputTokens += other.CachedInputTokens
	c.CacheWriteInputTokens += other.CacheWriteInputTokens
	c.OutputTokens += other.OutputTokens
	c.ReasoningOutputTokens += other.ReasoningOutputTokens
	c.AccountSwitches += other.AccountSwitches
	c.AccountSwitchInputTokens += other.AccountSwitchInputTokens
	c.AccountSwitchesInRequest += other.AccountSwitchesInRequest
	c.TTFB.add(other.TTFB)
	c.Duration.add(other.Duration)
	c.StopReasons = addTokenUsageLabelCounts(c.StopReasons, other.StopReasons)
	c.UpstreamErrors = addTokenUsageLabelCounts(c.UpstreamErrors, other.UpstreamErrors)
}

// addTokenUsageLabelCounts adds src into dst, which it allocates when needed
// and never shares with src. Past tokenUsageMaxLabelKeys distinct labels,
// new ones count under "other", so a row stays small.
func addTokenUsageLabelCounts(dst, src map[string]int64) map[string]int64 {
	for label, count := range src {
		if count == 0 {
			continue
		}
		if dst == nil {
			dst = map[string]int64{}
		}
		if _, exists := dst[label]; !exists {
			// Reserve one slot for overflow. Older snapshots may already contain
			// the full label budget without an "other" bucket; fold one label
			// into that bucket before admitting another key.
			if _, hasOther := dst[tokenUsageOverflowLabel]; !hasOther && len(dst) >= tokenUsageMaxLabelKeys {
				for existing, existingCount := range dst {
					if existing != tokenUsageOverflowLabel {
						dst[tokenUsageOverflowLabel] = existingCount
						delete(dst, existing)
						break
					}
				}
			}
			if len(dst) >= tokenUsageMaxLabelKeys-1 {
				label = tokenUsageOverflowLabel
			}
		}
		dst[label] += count
	}
	return dst
}

func (c tokenUsageCounts) zero() bool {
	return c.Requests == 0 && c.RequestsWithoutUsage == 0 && c.InputTokens == 0 && c.CachedInputTokens == 0 &&
		c.CacheWriteInputTokens == 0 && c.OutputTokens == 0 && c.ReasoningOutputTokens == 0 &&
		c.AccountSwitches == 0 && c.AccountSwitchInputTokens == 0 && c.AccountSwitchesInRequest == 0 &&
		c.TTFB == (tokenUsageLatency{}) && c.Duration == (tokenUsageLatency{}) &&
		len(c.StopReasons) == 0 && len(c.UpstreamErrors) == 0
}

func tokenUsageRowFor(key tokenUsageKey, counts tokenUsageCounts) TokenUsageRow {
	return TokenUsageRow{
		Hour:                     time.Unix(key.Hour, 0).UTC().Format(time.RFC3339),
		Provider:                 key.Provider,
		AccountID:                key.AccountID,
		Model:                    key.Model,
		Client:                   key.Client,
		Requests:                 counts.Requests,
		RequestsWithoutUsage:     counts.RequestsWithoutUsage,
		InputTokens:              counts.InputTokens,
		CachedInputTokens:        counts.CachedInputTokens,
		CacheWriteInputTokens:    counts.CacheWriteInputTokens,
		OutputTokens:             counts.OutputTokens,
		ReasoningOutputTokens:    counts.ReasoningOutputTokens,
		AccountSwitches:          counts.AccountSwitches,
		AccountSwitchInputTokens: counts.AccountSwitchInputTokens,
		AccountSwitchesInRequest: counts.AccountSwitchesInRequest,
		TTFBCount:                counts.TTFB.Count,
		TTFBMsSum:                counts.TTFB.SumMs,
		TTFBMsMax:                counts.TTFB.MaxMs,
		TTFBMsBuckets:            counts.TTFB.bucketSlice(),
		DurationCount:            counts.Duration.Count,
		DurationMsSum:            counts.Duration.SumMs,
		DurationMsMax:            counts.Duration.MaxMs,
		DurationMsBuckets:        counts.Duration.bucketSlice(),
		StopReasons:              addTokenUsageLabelCounts(nil, counts.StopReasons),
		UpstreamErrors:           addTokenUsageLabelCounts(nil, counts.UpstreamErrors),
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
			Requests:                 row.Requests,
			RequestsWithoutUsage:     row.RequestsWithoutUsage,
			InputTokens:              row.InputTokens,
			CachedInputTokens:        row.CachedInputTokens,
			CacheWriteInputTokens:    row.CacheWriteInputTokens,
			OutputTokens:             row.OutputTokens,
			ReasoningOutputTokens:    row.ReasoningOutputTokens,
			AccountSwitches:          row.AccountSwitches,
			AccountSwitchInputTokens: row.AccountSwitchInputTokens,
			AccountSwitchesInRequest: row.AccountSwitchesInRequest,
			TTFB:                     tokenUsageLatencyFromRow(row.TTFBCount, row.TTFBMsSum, row.TTFBMsMax, row.TTFBMsBuckets),
			Duration:                 tokenUsageLatencyFromRow(row.DurationCount, row.DurationMsSum, row.DurationMsMax, row.DurationMsBuckets),
			StopReasons:              addTokenUsageLabelCounts(nil, row.StopReasons),
			UpstreamErrors:           addTokenUsageLabelCounts(nil, row.UpstreamErrors),
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

	// sessions remembers, under mu, the accounts that recently served each
	// (session, model), so a turn on an account outside that set counts as
	// a switch. sessionOrder keeps them least recently seen last, so
	// eviction is constant time. It is process memory only: the first turn
	// after a restart is never a switch.
	sessionSeed  maphash.Seed
	sessions     map[uint64]*list.Element
	sessionOrder *list.List
}

// tokenUsageSessionEntry is one (session, model)'s recent serving accounts.
type tokenUsageSessionEntry struct {
	hash     uint64
	accounts [tokenUsageSessionAccounts]tokenUsageServedAccount
	count    int
}

type tokenUsageServedAccount struct {
	account string
	seen    time.Time
}

// TokenUsageRecorderOption configures NewTokenUsageRecorder.
type TokenUsageRecorderOption func(*TokenUsageRecorder)

// WithTokenUsageClock sets the recorder's clock, including for the
// compaction the constructor runs.
func WithTokenUsageClock(now func() time.Time) TokenUsageRecorderOption {
	return func(t *TokenUsageRecorder) {
		if now != nil {
			t.now = now
		}
	}
}

// tokenUsageTurn is what one finished request adds besides its token counts.
type tokenUsageTurn struct {
	// sessionKey names the session across turns; empty skips switch
	// accounting.
	sessionKey string
	// sessionModel is the pool model the turn's prompt cache belongs to;
	// the row's model when empty.
	sessionModel string
	// placedAccountID is the account placement picked for the request,
	// before any retry layer ran. A served account that differs from it
	// means the switch happened mid-request.
	placedAccountID string
	ttfb            time.Duration
	ttfbOK          bool
	duration        time.Duration
	durationOK      bool
	stopReason      string
	// errorStatus, when non-zero, records an upstream error response
	// instead of a turn.
	errorStatus int
}

// tokenUsageSessionKey joins what identifies a session across turns.
func tokenUsageSessionKey(provider accounts.Provider, agentType, sessionID string) string {
	if strings.TrimSpace(sessionID) == "" {
		return ""
	}
	if provider == "" {
		// Legacy Codex accounts carry no provider; the HTTP path names it.
		provider = accounts.ProviderCodex
	}
	return string(provider) + "\x00" + agentType + "\x00" + sessionID
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
func NewTokenUsageRecorder(path string, whois TokenUsageWhoIs, options ...TokenUsageRecorderOption) *TokenUsageRecorder {
	recorder := &TokenUsageRecorder{
		path:         strings.TrimSpace(path),
		now:          time.Now,
		whois:        whois,
		hours:        map[int64]*tokenUsageHour{},
		clientCache:  map[string]tokenUsageClientEntry{},
		sessionSeed:  maphash.MakeSeed(),
		sessions:     map[uint64]*list.Element{},
		sessionOrder: list.New(),
	}
	for _, option := range options {
		option(recorder)
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
	t.recordTurn(provider, accountID, model, client, usage, ok, tokenUsageTurn{})
}

func (t *TokenUsageRecorder) recordTurn(provider, accountID, model, client string, usage tokenUsage, ok bool, turn tokenUsageTurn) {
	if t == nil {
		return
	}
	var counts tokenUsageCounts
	if turn.errorStatus != 0 {
		counts.UpstreamErrors = map[string]int64{strconv.Itoa(turn.errorStatus): 1}
	} else {
		counts.Requests = 1
		if ok {
			counts.InputTokens = usage.InputTokens
			counts.CachedInputTokens = usage.CachedInputTokens
			counts.CacheWriteInputTokens = usage.CacheWriteInputTokens
			counts.OutputTokens = usage.OutputTokens
			counts.ReasoningOutputTokens = usage.ReasoningOutputTokens
		} else {
			counts.RequestsWithoutUsage = 1
		}
		if turn.ttfbOK {
			counts.TTFB.observe(turn.ttfb)
		}
		if turn.durationOK {
			counts.Duration.observe(turn.duration)
		}
		if reason := tokenUsageReasonLabel(turn.stopReason); reason != "" {
			counts.StopReasons = map[string]int64{reason: 1}
		}
	}
	now := t.clock()
	key := tokenUsageKey{
		Hour:      now.UTC().Truncate(time.Hour).Unix(),
		Provider:  tokenUsageLabel(provider, "unknown"),
		AccountID: tokenUsageLabel(accountID, "unknown"),
		Model:     tokenUsageLabel(strings.ToLower(model), "unknown"),
		Client:    tokenUsageLabel(client, tokenUsageUnknownClient),
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	// A turn the fallback chain answered has no subscription account and no
	// cache on one, so it neither counts nor joins the session's set.
	if turn.errorStatus == 0 && turn.sessionKey != "" && key.AccountID != tokenUsageFallbackAccount &&
		t.noteSessionAccountLocked(tokenUsageSessionModelKey(turn.sessionKey, turn.sessionModel, key.Model), key.AccountID, now) {
		counts.AccountSwitches = 1
		// The cold part: what the new account could not read from cache.
		counts.AccountSwitchInputTokens = max(counts.InputTokens-counts.CachedInputTokens, 0)
		if placed := strings.TrimSpace(turn.placedAccountID); placed != "" && tokenUsageLabel(placed, "unknown") != key.AccountID {
			counts.AccountSwitchesInRequest = 1
		}
	}
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

// tokenUsageSessionModelKey scopes a session key to one model, since each
// model keeps its own prompt cache.
func tokenUsageSessionModelKey(sessionKey, sessionModel, rowModel string) string {
	model := strings.ToLower(strings.TrimSpace(sessionModel))
	if model == "" {
		model = rowModel
	}
	return sessionKey + "\x00" + model
}

// noteSessionAccountLocked records that account served a turn of the
// (session, model) and reports whether that is a switch: the key has other
// accounts that served it within tokenUsageSwitchWindow, and this account is
// not among them. The caller holds mu. Every step is constant time.
func (t *TokenUsageRecorder) noteSessionAccountLocked(key, account string, now time.Time) bool {
	if t.sessions == nil || t.sessionOrder == nil {
		t.sessionSeed = maphash.MakeSeed()
		t.sessions = map[uint64]*list.Element{}
		t.sessionOrder = list.New()
	}
	hash := maphash.String(t.sessionSeed, key)
	element, found := t.sessions[hash]
	if !found {
		for t.sessionOrder.Len() >= tokenUsageMaxSessions {
			oldest := t.sessionOrder.Back()
			delete(t.sessions, oldest.Value.(*tokenUsageSessionEntry).hash)
			t.sessionOrder.Remove(oldest)
		}
		entry := &tokenUsageSessionEntry{hash: hash}
		entry.accounts[0] = tokenUsageServedAccount{account: account, seen: now}
		entry.count = 1
		t.sessions[hash] = t.sessionOrder.PushFront(entry)
		return false
	}
	t.sessionOrder.MoveToFront(element)
	entry := element.Value.(*tokenUsageSessionEntry)
	warm, known, oldest := false, -1, 0
	for index := range entry.count {
		served := entry.accounts[index]
		if now.Sub(served.seen) <= tokenUsageSwitchWindow {
			warm = true
		}
		if served.account == account {
			known = index
		}
		if served.seen.Before(entry.accounts[oldest].seen) {
			oldest = index
		}
	}
	switched := warm && (known < 0 || now.Sub(entry.accounts[known].seen) > tokenUsageSwitchWindow)
	switch {
	case known >= 0:
		// Out-of-order finishes must not move the time backwards.
		if now.After(entry.accounts[known].seen) {
			entry.accounts[known].seen = now
		}
	case entry.count < tokenUsageSessionAccounts:
		entry.accounts[entry.count] = tokenUsageServedAccount{account: account, seen: now}
		entry.count++
	default:
		entry.accounts[oldest] = tokenUsageServedAccount{account: account, seen: now}
	}
	return switched
}

// tokenUsageTrackedSession reports whether a request's session id names a
// session that lasts across turns. An id taken from Idempotency-Key, or the
// connection hash session.ExtractID falls back to, is new on every request,
// so tracking it would only fill the session memory.
func tokenUsageTrackedSession(r *http.Request, sessionID string) bool {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" || strings.HasPrefix(sessionID, "fallback:") {
		return false
	}
	if r != nil {
		// ExtractID reads Idempotency-Key ahead of the query and the body, so
		// an id equal to it is the one-shot key, not a session.
		if key := strings.TrimSpace(r.Header.Get("Idempotency-Key")); key != "" && (key == sessionID || session.CanonicalThreadID(key) == sessionID) {
			return false
		}
	}
	return true
}

// tokenUsageReasonLabel normalizes a provider stop reason for a row: lower
// case, at most tokenUsageMaxLabelLength characters from [a-z0-9._:-], and
// "other" for anything else, so a row never carries free text.
func tokenUsageReasonLabel(reason string) string {
	reason = strings.ToLower(strings.TrimSpace(reason))
	if reason == "" {
		return ""
	}
	if len(reason) > tokenUsageMaxLabelLength {
		return tokenUsageOverflowLabel
	}
	for _, r := range reason {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.', r == '_', r == ':', r == '-':
		default:
			return tokenUsageOverflowLabel
		}
	}
	return reason
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
func (t *TokenUsageRecorder) recordWithClient(provider, accountID, model string, resolve func() string, blocking bool, usage tokenUsage, ok bool, turn tokenUsageTurn) {
	if t == nil {
		return
	}
	if !blocking {
		t.recordTurn(provider, accountID, model, resolve(), usage, ok, turn)
		return
	}
	go t.recordTurn(provider, accountID, model, resolve(), usage, ok, turn)
}

// tokenUsageRequest is what the proxy knows about a model request before its
// response arrives.
type tokenUsageRequest struct {
	userEmail    string
	requestModel string
	// sessionKey is tokenUsageSessionKey for the request's session, or ""
	// when the session id is one-shot (tokenUsageTrackedSession).
	sessionKey string
	// sessionModel is the pool model the request's prompt cache belongs to.
	sessionModel string
	// placedAccountID is the account placement picked.
	placedAccountID string
	// started is when the request arrived.
	started time.Time
}

// wrapTokenUsageBody installs usage accounting on a final HTTP response.
// Successful model turns are counted in full; a model request that ends in
// an upstream error response counts only in upstream_errors.
func (s Server) wrapTokenUsageBody(response *http.Response, r *http.Request, request tokenUsageRequest, account accounts.Account) {
	if s.TokenUsage == nil || response == nil || r == nil || !tokenUsageCountedRequest(r.Method, r.URL.Path) {
		return
	}
	provider := account.Provider
	if provider == "" {
		provider = accounts.ProviderCodex
	}
	resolve, blocking := s.TokenUsage.tokenUsageClient(r, request.userEmail)
	recorder := s.TokenUsage
	accountID := account.ID
	if accountID == "" {
		// A retry layer answered from outside the pool (the Azure or Fable
		// fallback) and tagged the response with no account.
		accountID = tokenUsageFallbackAccount
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		recorder.recordWithClient(string(provider), accountID, request.requestModel, resolve, blocking, tokenUsage{}, false,
			tokenUsageTurn{errorStatus: response.StatusCode})
		return
	}
	if response.Body == nil {
		return
	}
	response.Body = newTokenUsageBody(response.Body, response.Header.Get("Content-Type"), func(result tokenUsageBodyResult) {
		model := result.model
		if model == "" {
			model = request.requestModel
		}
		turn := tokenUsageTurn{
			sessionKey:      request.sessionKey,
			sessionModel:    request.sessionModel,
			placedAccountID: request.placedAccountID,
			stopReason:      result.stop,
		}
		if !request.started.IsZero() {
			if !result.firstByte.IsZero() {
				turn.ttfb, turn.ttfbOK = result.firstByte.Sub(request.started), true
			}
			turn.duration, turn.durationOK = result.finished.Sub(request.started), true
		}
		recorder.recordWithClient(string(provider), accountID, model, resolve, blocking, result.usage, result.ok, turn)
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
// codexWebSocketTerminalTypes are the event types whose usage the failover's
// size tracker reads.
var codexWebSocketTerminalTypes = [][]byte{
	[]byte(`"response.completed"`), []byte(`"response.incomplete"`),
	[]byte(`"response.failed"`), []byte(`"response.done"`),
}

// codexWebSocketUsageCandidate is a byte scan for a terminal event that
// reports input tokens. The size tracker alone needs nothing else, so it
// skips unmarshaling the stream's many deltas (every one of which names a
// "response." type). A delta that merely quotes these strings is parsed and
// then ignored as non-terminal.
func codexWebSocketUsageCandidate(body []byte) bool {
	if !bytes.Contains(body, []byte(`"input_tokens"`)) {
		return false
	}
	for _, eventType := range codexWebSocketTerminalTypes {
		if bytes.Contains(body, eventType) {
			return true
		}
	}
	return false
}

func (s Server) recordWebSocketTokenUsage(provider accounts.Provider, accountID, sessionKey string, modelState *webSocketModelState, poolModel string, body []byte) {
	if modelState == nil {
		return
	}
	record := s.TokenUsage != nil && modelState.usageClient != nil
	// The failover's size cap reads the turn's input tokens too.
	track := s.CodexOverloadFailover.enabled() && s.CodexOverloadFailover.failoverMaxInput() > 0
	if !record && (!track || !codexWebSocketUsageCandidate(body)) {
		return
	}
	usage, responseModel, stop, ok, terminal := tokenUsageTurnFromWebSocketMessage(body)
	if !terminal {
		return
	}
	if ok {
		modelState.noteInputTokens(usage.InputTokens)
	}
	if !record {
		return
	}
	model := responseModel
	if model == "" {
		model = webSocketTurnModel(modelState, poolModel)
	}
	turn := tokenUsageTurn{sessionKey: sessionKey, sessionModel: webSocketTurnModel(modelState, poolModel), placedAccountID: accountID, stopReason: stop}
	turn.ttfb, turn.ttfbOK, turn.duration, turn.durationOK = modelState.turnTiming(time.Now())
	resolve, blocking := modelState.usageClient, modelState.usageClientBlocking
	s.TokenUsage.recordWithClient(string(provider), accountID, model, resolve, blocking, usage, ok, turn)
}
