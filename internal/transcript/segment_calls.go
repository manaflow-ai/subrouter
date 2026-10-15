package transcript

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
)

// IndexRow is one model call in the analysis index. Rows are JSON lines in
// zstd-compressed index blobs; the byte ranges point into segment blobs.
//
// Token counts are as the provider reports them, except input_tokens, which
// is always the whole prompt: for Claude it adds cache reads and cache
// writes to the uncached input, for OpenAI it already includes cached input.
type IndexRow struct {
	Timestamp  string `json:"ts"`
	RequestID  string `json:"request_id"`
	Kind       string `json:"kind"`
	RecordedBy string `json:"recorded_by"`
	ClientNode string `json:"client_node"`
	ClientUser string `json:"client_user"`
	ClientIP   string `json:"client_ip,omitempty"`
	ClientName string `json:"client_name,omitempty"`
	UserHash   string `json:"user_hash,omitempty"`
	Agent      string `json:"agent"`
	SessionID  string `json:"session_id"`
	// ClientSessionID is the session id the client sent in a header
	// (x-claude-code-session-id, Codex session or conversation id), for
	// joining to the client's own trajectory files; ClientTurnID is its
	// prompt or turn id when a header carries one.
	ClientSessionID string `json:"client_session_id,omitempty"`
	ClientTurnID    string `json:"client_turn_id,omitempty"`
	// SessionGrouping is "fallback" when the request named no session and
	// was grouped by the router's fallback key, else "client".
	SessionGrouping string `json:"session_grouping"`
	Provider        string `json:"provider,omitempty"`
	Upstream        string `json:"upstream,omitempty"`
	Route           string `json:"route,omitempty"`
	ServedBy        string `json:"served_by,omitempty"`
	Account         string `json:"account,omitempty"`
	ServingAccount  string `json:"serving_account,omitempty"`
	Model           string `json:"model,omitempty"`
	ResponseModel   string `json:"response_model,omitempty"`
	Method          string `json:"method,omitempty"`
	Path            string `json:"path,omitempty"`
	Status          int    `json:"status"`
	TTFBMs          int64  `json:"ttfb_ms"`
	DurationMs      int64  `json:"duration_ms"`

	InputTokens       int64  `json:"input_tokens"`
	CachedInputTokens int64  `json:"cached_input_tokens"`
	CacheWriteTokens  int64  `json:"cache_write_tokens"`
	OutputTokens      int64  `json:"output_tokens"`
	ReasoningTokens   int64  `json:"reasoning_tokens"`
	StopReason        string `json:"stop_reason,omitempty"`

	// Reasoning settings the request asked for, and where the response's
	// reasoning went: visible thinking text versus opaque signatures,
	// redacted blocks and encrypted reasoning items (sizes only; nothing is
	// decrypted).
	ThinkingType            string `json:"thinking_type,omitempty"`
	ThinkingDisplay         string `json:"thinking_display,omitempty"`
	ThinkingBudget          int64  `json:"thinking_budget_tokens,omitempty"`
	ReasoningEffort         string `json:"reasoning_effort,omitempty"`
	ReasoningSummary        string `json:"reasoning_summary,omitempty"`
	ThinkingBlocks          int64  `json:"thinking_blocks"`
	ThinkingChars           int64  `json:"thinking_chars"`
	ThinkingSignatureBytes  int64  `json:"thinking_signature_bytes"`
	RedactedThinkingBytes   int64  `json:"redacted_thinking_bytes"`
	ReasoningEncryptedBytes int64  `json:"reasoning_encrypted_bytes"`
	ReasoningSummaryChars   int64  `json:"reasoning_summary_chars"`

	RequestSHA256   string `json:"request_sha256,omitempty"`
	RequestBytes    int64  `json:"request_bytes"`
	RequestEncoding string `json:"request_encoding,omitempty"`
	ResponseSHA256  string `json:"response_sha256,omitempty"`
	ResponseBytes   int64  `json:"response_bytes"`

	RequestBlob    string `json:"request_blob,omitempty"`
	RequestOffset  int64  `json:"request_offset"`
	RequestLength  int64  `json:"request_length"`
	ResponseBlob   string `json:"response_blob,omitempty"`
	ResponseOffset int64  `json:"response_offset"`
	ResponseLength int64  `json:"response_length"`
	// ResponseSplit means the response continued into the session's next
	// segment; the range covers only the first part.
	ResponseSplit bool `json:"response_split,omitempty"`
	// Complete is false when the response never finished (client gone,
	// upstream error, or a shutdown).
	Complete bool `json:"complete"`
	// BodyLost means a segment holding this call's bodies failed to store.
	BodyLost bool `json:"body_lost,omitempty"`
	// LegacySource names the raw transcript file a converted call came from.
	LegacySource string `json:"legacy_source,omitempty"`
}

type partRef struct {
	seg      *sessionSegment
	from, to int
	set      bool
}

func (p *partRef) extend(seg *sessionSegment, from, to int) bool {
	if !p.set {
		*p = partRef{seg: seg, from: from, to: to, set: true}
		return true
	}
	if p.seg != seg {
		return false
	}
	if from < p.from {
		p.from = from
	}
	if to > p.to {
		p.to = to
	}
	return true
}

type callRow struct {
	row        IndexRow
	sessionKey string
	connection string
	streamID   string
	metaAt     time.Time
	touched    time.Time
	firstByte  time.Time
	req, resp  partRef
	usage      usageScanner
	waiting    int
	emitted    bool
}

func (w *segmentWriter) newCall(seg *sessionSegment, item *segmentItem) *callRow {
	payload := item.payload
	return &callRow{
		sessionKey: seg.key,
		metaAt:     item.at,
		touched:    item.at,
		row: IndexRow{
			Timestamp:       item.at.UTC().Format(time.RFC3339Nano),
			RequestID:       str(payload["request_id"]),
			Kind:            "http",
			RecordedBy:      w.cfg.Host,
			ClientNode:      nonEmpty(str(payload["client_node"])),
			ClientUser:      nonEmpty(str(payload["client_user"])),
			ClientIP:        str(payload["client_ip"]),
			ClientName:      str(payload["client_name"]),
			UserHash:        str(payload["user_hash"]),
			Agent:           normalizeAgentType(str(payload["agent_type"])),
			SessionID:       str(payload["session_id"]),
			SessionGrouping: sessionGrouping(str(payload["session_id"])),
			Provider:        str(payload["provider"]),
			Upstream:        str(payload["upstream"]),
			Route:           str(payload["route"]),
			Account:         str(payload["account"]),
			Method:          str(payload["method"]),
			Path:            str(payload["path"]),
			LegacySource:    w.source,
		},
	}
}

func (w *segmentWriter) noteMeta(seg *sessionSegment, item *segmentItem) {
	requestID := str(item.payload["request_id"])
	if requestID == "" {
		return
	}
	call := w.newCall(seg, item)
	headers := headerMap(item.payload["headers"])
	call.row.ClientSessionID = firstHeader(headers, clientSessionHeaders)
	call.row.ClientTurnID = firstHeader(headers, clientTurnHeaders)
	if str(item.payload["transport"]) == "websocket" {
		// A websocket connection carries many calls; this is their template.
		call.row.Kind = "websocket"
		w.wsConns()[requestID] = call
		return
	}
	w.calls[requestID] = call
}

// wsConns holds websocket connection metadata by connection request id.
func (w *segmentWriter) wsConns() map[string]*callRow {
	if w.wsMeta == nil {
		w.wsMeta = map[string]*callRow{}
	}
	return w.wsMeta
}

func (w *segmentWriter) noteBody(seg *sessionSegment, item *segmentItem, depFrame, frame int) {
	payload := item.payload
	requestID := str(payload["request_id"])
	if requestID == "" {
		return
	}
	direction := str(payload["direction"])
	if item.eventType == "websocket_message" {
		// depFrame: a delta-encoded client message needs its chain's first
		// body to rebuild.
		w.noteWebSocketMessage(seg, item, depFrame, frame)
		return
	}
	call := w.calls[requestID]
	if call == nil || direction != "client_to_upstream" {
		return
	}
	call.touched = item.at
	call.req.extend(seg, depFrame, frame)
	call.row.RequestSHA256 = str(payload["sha256"])
	call.row.RequestBytes = int64(len(item.body))
	call.row.RequestEncoding = str(payload["content_encoding"])
	applyRequestHead(&call.row, item.body)
}

// noteWebSocketMessage starts a call at each client message and adds the
// upstream messages that follow to it.
func (w *segmentWriter) noteWebSocketMessage(seg *sessionSegment, item *segmentItem, from, to int) {
	payload := item.payload
	connection := str(payload["request_id"])
	direction := str(payload["direction"])
	if direction == "client_to_upstream" {
		if previous := w.wsCalls[connection]; previous != nil && previous.streamID == str(payload["stream_id"]) {
			// The end of a client message whose chunks already started it.
			previous.req.extend(seg, from, to)
			previous.row.RequestSHA256 = str(payload["sha256"])
			previous.row.RequestBytes, _ = jsonInt64(payload["bytes"])
			return
		}
		if previous := w.wsCalls[connection]; previous != nil {
			w.closeCall(previous, true)
			delete(w.wsCalls, connection)
		}
		call := &callRow{sessionKey: seg.key, connection: connection, metaAt: item.at, touched: item.at}
		if template := w.wsConns()[connection]; template != nil {
			template.touched = item.at
			call.row = template.row
		} else {
			call.row = IndexRow{Kind: "websocket", RecordedBy: w.cfg.Host, Agent: normalizeAgentType(str(payload["agent_type"])), SessionID: str(payload["session_id"]), ClientNode: UnknownAttribution, ClientUser: UnknownAttribution}
		}
		call.streamID = str(payload["stream_id"])
		call.row.Timestamp = item.at.UTC().Format(time.RFC3339Nano)
		call.row.RequestID = connection + "/" + call.streamID
		call.row.SessionID = str(payload["session_id"])
		call.req.extend(seg, from, to)
		call.row.RequestSHA256 = str(payload["sha256"])
		call.row.RequestBytes += int64(len(item.body))
		applyRequestHead(&call.row, item.body)
		w.wsCalls[connection] = call
		return
	}
	call := w.wsCalls[connection]
	if call == nil {
		return
	}
	call.touched = item.at
	if call.firstByte.IsZero() {
		call.firstByte = item.at
	}
	if !call.resp.extend(seg, from, to) {
		call.row.ResponseSplit = true
	}
	switch item.kind {
	case itemBody:
		call.row.ResponseBytes += int64(len(item.body))
		call.usage.feedMessage(item.body)
	case itemSummary:
		// A chunked message ends here; its bytes went through feed.
		call.usage.endMessage()
	}
	if call.usage.done {
		w.closeCall(call, true)
		delete(w.wsCalls, connection)
	}
}

// noteChunk sees each response chunk as it arrives (before coalescing): the
// first one marks time to first byte, and usage is read from the stream.
func (w *segmentWriter) noteChunk(seg *sessionSegment, item *segmentItem) {
	requestID := str(item.payload["request_id"])
	if requestID == "" {
		return
	}
	if item.eventType == "websocket_message" {
		if str(item.payload["direction"]) == "client_to_upstream" {
			// A client message too large to hold whole starts its call at
			// its first chunk, so the chunks fall inside the call's range.
			if call := w.wsCalls[requestID]; call == nil || call.streamID != str(item.payload["stream_id"]) {
				w.noteWebSocketMessage(seg, &segmentItem{kind: itemChunk, at: item.at, eventType: item.eventType, payload: item.payload}, len(seg.frames), len(seg.frames))
			}
			return
		}
		if call := w.wsCalls[requestID]; call != nil && str(item.payload["direction"]) == "upstream_to_client" {
			call.touched = item.at
			if call.firstByte.IsZero() {
				call.firstByte = item.at
			}
			call.row.ResponseBytes += int64(len(item.body))
			call.usage.feed(item.body)
		}
		return
	}
	call := w.calls[requestID]
	if call == nil || str(item.payload["direction"]) != "upstream_to_client" {
		return
	}
	call.touched = item.at
	if call.firstByte.IsZero() {
		call.firstByte = item.at
	}
	call.usage.feed(item.body)
}

// noteChunkFrame records which frame a coalesced chunk landed in.
func (w *segmentWriter) noteChunkFrame(seg *sessionSegment, payload map[string]any, frame int) {
	requestID := str(payload["request_id"])
	if requestID == "" {
		return
	}
	direction := str(payload["direction"])
	if payload["opcode"] != nil {
		call := w.wsCalls[requestID]
		if call == nil {
			return
		}
		if direction == "client_to_upstream" {
			call.req.extend(seg, frame, frame)
		} else if !call.resp.extend(seg, frame, frame) {
			call.row.ResponseSplit = true
		}
		return
	}
	call := w.calls[requestID]
	if call == nil {
		return
	}
	if direction == "client_to_upstream" {
		call.req.extend(seg, frame, frame)
		return
	}
	if !call.resp.extend(seg, frame, frame) {
		call.row.ResponseSplit = true
	}
}

func (w *segmentWriter) noteSummary(seg *sessionSegment, item *segmentItem, frame int) {
	payload := item.payload
	requestID := str(payload["request_id"])
	if requestID == "" {
		return
	}
	direction := str(payload["direction"])
	if item.eventType == "websocket_message" {
		w.noteWebSocketMessage(seg, item, frame, frame)
		return
	}
	call := w.calls[requestID]
	if call == nil {
		return
	}
	call.touched = item.at
	if direction == "client_to_upstream" {
		// A request too large to hold whole was recorded as chunks.
		call.req.extend(seg, frame, frame)
		call.row.RequestSHA256 = str(payload["sha256"])
		call.row.RequestBytes, _ = jsonInt64(payload["bytes"])
		return
	}
	if !call.resp.extend(seg, frame, frame) {
		call.row.ResponseSplit = true
	}
	status, _ := jsonInt64(payload["status"])
	call.row.Status = int(status)
	call.row.ResponseSHA256 = str(payload["sha256"])
	call.row.ResponseBytes, _ = jsonInt64(payload["bytes"])
	if account := str(payload["account"]); account != "" {
		call.row.ServingAccount = account
	}
	if provider := str(payload["provider"]); provider != "" && call.row.Provider == "" {
		call.row.Provider = provider
	}
	if route := str(payload["route"]); route != "" && call.row.Route == "" {
		call.row.Route = route
	}
	call.row.ServedBy = servedBy(headerMap(payload["response_headers"]))
	call.row.Complete = true
	w.closeCall(call, false)
	delete(w.calls, requestID)
}

// servedBy names the service that produced a response, from its headers;
// Bedrock and Azure fallbacks answer through the same proxy path as the
// subscription pool.
func servedBy(headers map[string][]string) string {
	has := func(prefix string) bool {
		for key := range headers {
			if strings.HasPrefix(strings.ToLower(key), prefix) {
				return true
			}
		}
		return false
	}
	switch {
	case has("x-amzn-bedrock") || has("x-amzn-requestid"):
		return "bedrock"
	case has("apim-request-id") || has("x-ms-region") || has("azureml"):
		return "azure"
	case has("anthropic-"):
		return "anthropic"
	case has("openai-") || has("x-codex") || has("x-oai"):
		return "openai"
	default:
		return ""
	}
}

// closeCall finishes a row and hands it to the segments it points into; it
// is written to the index once they are all stored.
func (w *segmentWriter) closeCall(call *callRow, websocket bool) {
	if call.emitted {
		return
	}
	call.emitted = true
	usage := call.usage.result()
	call.row.InputTokens = usage.input
	call.row.CachedInputTokens = usage.cached
	call.row.CacheWriteTokens = usage.cacheWrite
	call.row.OutputTokens = usage.output
	call.row.ReasoningTokens = usage.reasoning
	call.row.StopReason = usage.stop
	call.row.ResponseModel = usage.model
	call.row.ThinkingBlocks = usage.thinkingBlocks
	call.row.ThinkingChars = usage.thinkingChars
	call.row.ThinkingSignatureBytes = usage.signatureBytes
	call.row.RedactedThinkingBytes = usage.redactedBytes
	call.row.ReasoningEncryptedBytes = usage.encryptedBytes
	call.row.ReasoningSummaryChars = usage.summaryChars
	if websocket {
		call.row.Complete = usage.done
	}
	end := call.touched
	if !call.firstByte.IsZero() {
		call.row.TTFBMs = call.firstByte.Sub(call.metaAt).Milliseconds()
	}
	call.row.DurationMs = end.Sub(call.metaAt).Milliseconds()
	w.up.attachRow(call)
}

// resolvePart fills a row's blob range once its segment is stored.
func resolvePart(call *callRow, seg *sessionSegment) {
	for _, part := range []*partRef{&call.req, &call.resp} {
		if !part.set || part.seg != seg || part.from < 0 || part.to >= len(seg.frames) {
			continue
		}
		offset := seg.frames[part.from].compOff
		end := seg.frames[part.to].compOff + seg.frames[part.to].compLen
		if part == &call.req {
			call.row.RequestBlob, call.row.RequestOffset, call.row.RequestLength = seg.blob, offset, end-offset
		} else {
			call.row.ResponseBlob, call.row.ResponseOffset, call.row.ResponseLength = seg.blob, offset, end-offset
		}
	}
}

// indexBuffer collects rows for the next index blob.
type indexBuffer struct {
	mu     sync.Mutex
	buf    bytes.Buffer
	rows   int64
	serial int
}

func (w *segmentWriter) emitRow(row IndexRow) {
	line, err := json.Marshal(row)
	if err != nil {
		return
	}
	w.index.mu.Lock()
	w.index.buf.Write(line)
	w.index.buf.WriteByte('\n')
	w.index.rows++
	full := w.index.buf.Len() >= indexPartBytes
	w.index.mu.Unlock()
	w.up.indexRows.Add(1)
	if full {
		w.flushIndexNow()
	}
}

// indexPartBytes caps one index blob's raw size.
const indexPartBytes = 16 << 20

// flushIndexNow writes the rows collected so far as one index blob.
func (w *segmentWriter) flushIndexNow() {
	w.index.mu.Lock()
	if w.index.buf.Len() == 0 {
		w.index.mu.Unlock()
		return
	}
	raw := append([]byte(nil), w.index.buf.Bytes()...)
	w.index.buf.Reset()
	w.index.serial++
	serial := w.index.serial
	w.index.mu.Unlock()
	encoder, err := newIndexEncoder()
	if err != nil {
		return
	}
	compressed := encoder.EncodeAll(raw, nil)
	_ = encoder.Close()
	now := w.cfg.Now().UTC()
	var random [4]byte
	_, _ = rand.Read(random[:])
	name := fmt.Sprintf("%s/%s/%s-%d-%d-%s.jsonl.zst", w.cfg.IndexPrefix, now.Format("2006/01/02/15"), w.cfg.Host, os.Getpid(), serial, hex.EncodeToString(random[:]))
	w.up.putBlob(name, compressed, "application/zstd")
}

// Headers that carry the client's own session and turn ids, most specific
// first.
var (
	clientSessionHeaders = []string{"X-Claude-Code-Session-Id", "X-Claude-Session-Id", "X-Codex-Session-Id", "Session_id", "Session-Id", "X-Session-Id", "Conversation_id", "X-Conversation-Id", "Openai-Conversation-Id", "Thread-Id", "X-Codex-Parent-Thread-Id", "X-Codex-Window-Id", "X-Subrouter-Session"}
	// Only ids that name a prompt or turn; per-request ids would mislabel.
	clientTurnHeaders = []string{"X-Claude-Code-Prompt-Id", "X-Prompt-Id", "X-Codex-Turn-Id", "X-Turn-Id"}
)

func firstHeader(headers map[string][]string, names []string) string {
	for _, name := range names {
		for key, values := range headers {
			if strings.EqualFold(key, name) && len(values) > 0 && strings.TrimSpace(values[0]) != "" {
				return strings.TrimSpace(values[0])
			}
		}
	}
	return ""
}

// sessionGrouping tells a client-named session from the router's fallback
// grouping of requests that named none.
func sessionGrouping(sessionID string) string {
	switch {
	case sessionID == "":
		return "none"
	case strings.HasPrefix(sessionID, "fallback:"):
		// session.ExtractID's grouping for requests that name no session.
		return "fallback"
	default:
		return "client"
	}
}

// applyRequestHead reads the model and reasoning settings of a request body
// in one parse.
func applyRequestHead(row *IndexRow, body []byte) {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return
	}
	// Each part decodes on its own, so an unexpected shape in one (a string
	// "reasoning", a boolean "thinking") never costs the others.
	var head struct {
		Model           json.RawMessage `json:"model"`
		Thinking        json.RawMessage `json:"thinking"`
		Reasoning       json.RawMessage `json:"reasoning"`
		ReasoningEffort json.RawMessage `json:"reasoning_effort"`
		Response        json.RawMessage `json:"response"`
	}
	if json.Unmarshal(trimmed, &head) != nil {
		return
	}
	_ = json.Unmarshal(head.Model, &row.Model)
	if row.Model == "" {
		var response struct {
			Model string `json:"model"`
		}
		if json.Unmarshal(head.Response, &response) == nil {
			row.Model = response.Model
		}
	}
	var thinking struct {
		Type         string `json:"type"`
		Display      string `json:"display"`
		BudgetTokens int64  `json:"budget_tokens"`
	}
	if json.Unmarshal(head.Thinking, &thinking) == nil {
		row.ThinkingType, row.ThinkingDisplay, row.ThinkingBudget = thinking.Type, thinking.Display, thinking.BudgetTokens
	}
	var reasoning struct {
		Effort  string `json:"effort"`
		Summary string `json:"summary"`
	}
	if json.Unmarshal(head.Reasoning, &reasoning) == nil {
		row.ReasoningEffort, row.ReasoningSummary = reasoning.Effort, reasoning.Summary
	}
	if row.ReasoningEffort == "" {
		_ = json.Unmarshal(head.ReasoningEffort, &row.ReasoningEffort)
	}
}
