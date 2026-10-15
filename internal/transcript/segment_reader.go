package transcript

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/klauspost/compress/zstd"
)

// ExportBody is one reconstructed body. JSON bodies are embedded as JSON,
// other UTF-8 as text, anything else as base64.
type ExportBody struct {
	Bytes           int64           `json:"bytes"`
	SHA256          string          `json:"sha256"`
	Verified        bool            `json:"verified"`
	ContentEncoding string          `json:"content_encoding,omitempty"`
	Partial         bool            `json:"partial,omitempty"`
	JSON            json.RawMessage `json:"json,omitempty"`
	Text            *string         `json:"text,omitempty"`
	Base64          string          `json:"base64,omitempty"`
}

// ExportClient is the machine a request came from.
type ExportClient struct {
	Node string `json:"node,omitempty"`
	User string `json:"user,omitempty"`
	IP   string `json:"ip,omitempty"`
	Name string `json:"name,omitempty"`
}

// ExportResponse is the upstream answer to one request.
type ExportResponse struct {
	Status     int                 `json:"status,omitempty"`
	Account    string              `json:"account,omitempty"`
	Provider   string              `json:"provider,omitempty"`
	DurationMs int64               `json:"duration_ms,omitempty"`
	Headers    map[string][]string `json:"headers,omitempty"`
	Body       *ExportBody         `json:"body,omitempty"`
}

// ExportRecord is one request with its response, or one websocket message,
// or one body that could not be joined to a request (legacy transcripts
// carry no request ids).
type ExportRecord struct {
	Kind           string              `json:"kind"`
	RequestID      string              `json:"request_id,omitempty"`
	StreamID       string              `json:"stream_id,omitempty"`
	AgentType      string              `json:"agent_type,omitempty"`
	SessionID      string              `json:"session_id,omitempty"`
	Timestamp      string              `json:"timestamp,omitempty"`
	Client         *ExportClient       `json:"client,omitempty"`
	Account        string              `json:"account,omitempty"`
	Provider       string              `json:"provider,omitempty"`
	Transport      string              `json:"transport,omitempty"`
	Method         string              `json:"method,omitempty"`
	Path           string              `json:"path,omitempty"`
	Upstream       string              `json:"upstream,omitempty"`
	RequestHeaders map[string][]string `json:"request_headers,omitempty"`
	Model          string              `json:"model,omitempty"`
	Direction      string              `json:"direction,omitempty"`
	Opcode         string              `json:"opcode,omitempty"`
	Request        *ExportBody         `json:"request,omitempty"`
	Response       *ExportResponse     `json:"response,omitempty"`
	Message        *ExportBody         `json:"message,omitempty"`
	Body           *ExportBody         `json:"body,omitempty"`
	Segment        string              `json:"segment,omitempty"`
	Host           string              `json:"host,omitempty"`
	Source         string              `json:"source,omitempty"`
}

// ExportStats summarizes a pass over segments.
type ExportStats struct {
	Segments         int   `json:"segments"`
	Events           int64 `json:"events"`
	Records          int64 `json:"records"`
	Bodies           int64 `json:"bodies"`
	BodyBytes        int64 `json:"body_bytes"`
	DeltaBodies      int64 `json:"delta_bodies"`
	Verified         int64 `json:"verified"`
	Mismatched       int64 `json:"mismatched"`
	DecodeErrors     int64 `json:"decode_errors"`
	TornLines        int64 `json:"torn_lines"`
	MissingFooters   int   `json:"missing_footers"`
	UnattributedMeta int64 `json:"unattributed_meta"`
	SegmentBytes     int64 `json:"segment_bytes"`
}

// Failed reports whether any body failed to reconstruct exactly.
func (s ExportStats) Failed() bool { return s.Mismatched > 0 || s.DecodeErrors > 0 }

// SegmentExporter rebuilds requests and responses from segment events.
// Segments are fed in order; request and stream state carries across
// segments (a stream can straddle a seal), delta bases do not.
type SegmentExporter struct {
	Emit func(ExportRecord) error
	// Filter, when set, drops records it returns false for.
	Filter func(ExportRecord) bool

	Stats ExportStats

	bases    map[string][]deltaBase
	streams  map[string]*exportStream
	requests map[string]*ExportRecord
	order    []string
	segment  string
	host     string
	source   string
	footer   bool
	err      error
}

type exportStream struct {
	data     []byte
	next     int64
	payload  map[string]any
	at       string
	gap      bool
	segment  string
	host     string
	eventTyp string
}

func NewSegmentExporter(emit func(ExportRecord) error) *SegmentExporter {
	return &SegmentExporter{
		Emit:     emit,
		bases:    map[string][]deltaBase{},
		streams:  map[string]*exportStream{},
		requests: map[string]*ExportRecord{},
	}
}

// ReadSegment processes one decompressed segment stream.
func (x *SegmentExporter) ReadSegment(name string, r io.Reader) error {
	x.Stats.Segments++
	x.segment, x.host, x.footer, x.source = name, "", false, ""
	x.bases = map[string][]deltaBase{}
	reader := bufio.NewReaderSize(r, 4<<20)
	for {
		line, err := reader.ReadBytes('\n')
		x.Stats.SegmentBytes += int64(len(line))
		if len(bytes.TrimSpace(line)) > 0 {
			if line[len(line)-1] != '\n' {
				// A torn last line from a process that died mid-write.
				x.Stats.TornLines++
			} else if decodeErr := x.handleLine(line); decodeErr != nil {
				x.Stats.TornLines++
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
	}
	if !x.footer {
		x.Stats.MissingFooters++
	}
	return x.err
}

func (x *SegmentExporter) handleLine(line []byte) error {
	var event Event
	decoder := json.NewDecoder(bytes.NewReader(line))
	decoder.UseNumber()
	if err := decoder.Decode(&event); err != nil {
		return err
	}
	x.Stats.Events++
	payload := event.Payload
	if payload == nil {
		payload = map[string]any{}
	}
	switch event.Type {
	case "segment_header":
		x.host, _ = payload["host"].(string)
		if id, ok := payload["segment_id"].(string); ok && id != "" {
			x.segment = id
		}
		return nil
	case "segment_footer":
		x.footer = true
		return nil
	case "legacy_source":
		x.source, _ = payload["source"].(string)
		return nil
	case "subrouter_meta":
		x.meta(event, payload)
		return nil
	}
	if boolField(payload, "body_chunk") || strings.HasSuffix(event.Type, "_chunk") {
		x.chunk(event, payload)
		return nil
	}
	if boolField(payload, "body_chunked") {
		x.summary(event, payload)
		return nil
	}
	if _, hasSeq := payload["body_seq"]; hasSeq || payload["body_text"] != nil || payload["body_base64"] != nil {
		x.wholeBody(event, payload)
	}
	return nil
}

func (x *SegmentExporter) meta(event Event, payload map[string]any) {
	requestID, _ := payload["request_id"].(string)
	record := &ExportRecord{
		Kind:           "http",
		RequestID:      requestID,
		AgentType:      str(payload["agent_type"]),
		SessionID:      str(payload["session_id"]),
		Timestamp:      event.Timestamp,
		Account:        str(payload["account"]),
		Provider:       str(payload["provider"]),
		Transport:      str(payload["transport"]),
		Method:         str(payload["method"]),
		Path:           str(payload["path"]),
		Upstream:       str(payload["upstream"]),
		RequestHeaders: headerMap(payload["headers"]),
		Segment:        x.segment,
		Host:           x.host,
		Source:         x.source,
	}
	client := &ExportClient{Node: str(payload["client_node"]), User: str(payload["client_user"]), IP: str(payload["client_ip"]), Name: str(payload["client_name"])}
	if client.Node == "" || client.Node == UnknownAttribution {
		x.Stats.UnattributedMeta++
	}
	if *client != (ExportClient{}) {
		record.Client = client
	}
	if record.Transport == "websocket" {
		record.Kind = "websocket_connection"
	}
	if requestID == "" {
		// Legacy metadata carries no request id; it stands alone.
		x.emit(*record)
		return
	}
	if _, exists := x.requests[requestID]; !exists {
		x.order = append(x.order, requestID)
	}
	x.requests[requestID] = record
	if record.Kind == "websocket_connection" {
		// Messages are emitted on their own; the connection record goes out
		// now so a reader sees it first.
		x.emit(*record)
		delete(x.requests, requestID)
	}
}

func (x *SegmentExporter) chunk(event Event, payload map[string]any) {
	streamID := str(payload["stream_id"])
	body, ok, err := payloadBody(payload)
	if err != nil || !ok {
		x.Stats.DecodeErrors++
		return
	}
	stream := x.streams[streamID]
	if stream == nil {
		stream = &exportStream{payload: payload, at: event.Timestamp, segment: x.segment, host: x.host, eventTyp: strings.TrimSuffix(event.Type, "_chunk")}
		x.streams[streamID] = stream
	}
	if offset, ok := jsonInt64(payload["offset"]); ok && offset != stream.next {
		stream.gap = true
	}
	stream.data = append(stream.data, body...)
	stream.next += int64(len(body))
}

func (x *SegmentExporter) summary(event Event, payload map[string]any) {
	streamID := str(payload["stream_id"])
	stream := x.streams[streamID]
	delete(x.streams, streamID)
	var data []byte
	gap := false
	if stream != nil {
		data, gap = stream.data, stream.gap
	}
	body := x.exportBody(data, payload)
	if gap {
		body.Verified = false
	}
	x.attach(event, payload, body, nil)
}

func (x *SegmentExporter) wholeBody(event Event, payload map[string]any) {
	var body []byte
	if delta, ok := payload["delta"].(map[string]any); ok {
		x.Stats.DeltaBodies++
		baseSeq, _ := jsonInt64(delta["base"])
		key := deltaKeyFor(event.Type, payload)
		var base []byte
		found := false
		for _, candidate := range x.bases[key] {
			if candidate.seq == baseSeq {
				base, found = candidate.body, true
				break
			}
		}
		if !found {
			x.Stats.DecodeErrors++
			return
		}
		rebuilt, err := applyDelta(base, normalizeNumbers(delta["ops"]))
		if err != nil {
			x.Stats.DecodeErrors++
			return
		}
		body = rebuilt
	} else {
		decoded, ok, err := payloadBody(payload)
		if err != nil || !ok {
			x.Stats.DecodeErrors++
			return
		}
		body = decoded
	}
	if seq, ok := jsonInt64(payload["body_seq"]); ok && str(payload["direction"]) == "client_to_upstream" {
		key := deltaKeyFor(event.Type, payload)
		chain := append(x.bases[key], deltaBase{seq: seq, body: body})
		if len(chain) > deltaBasesPerKey {
			chain = chain[len(chain)-deltaBasesPerKey:]
		}
		x.bases[key] = chain
	}
	exported := x.exportBody(body, payload)
	x.attach(event, payload, exported, body)
}

// attach joins a body to its request, or emits it as its own record.
func (x *SegmentExporter) attach(event Event, payload map[string]any, body *ExportBody, raw []byte) {
	requestID := str(payload["request_id"])
	direction := str(payload["direction"])
	eventType := strings.TrimSuffix(event.Type, "_chunk")
	if eventType == "websocket_message" {
		x.emit(ExportRecord{
			Kind: "websocket_message", RequestID: requestID, StreamID: str(payload["stream_id"]),
			AgentType: str(payload["agent_type"]), SessionID: str(payload["session_id"]), Timestamp: event.Timestamp,
			Direction: direction, Opcode: str(payload["opcode"]), Message: body,
			Segment: x.segment, Host: x.host, Source: x.source,
		})
		return
	}
	record := x.requests[requestID]
	if requestID == "" || record == nil {
		x.emit(ExportRecord{
			Kind: "body", RequestID: requestID, StreamID: str(payload["stream_id"]),
			AgentType: str(payload["agent_type"]), SessionID: str(payload["session_id"]), Timestamp: event.Timestamp,
			Direction: direction, Body: body, Model: modelOf(raw),
			Segment: x.segment, Host: x.host, Source: x.source,
		})
		return
	}
	if direction == "client_to_upstream" {
		record.Request = body
		if record.Model == "" {
			record.Model = modelOf(raw)
		}
		if record.Model == "" && body.JSON != nil {
			record.Model = modelOf(body.JSON)
		}
		return
	}
	duration, _ := jsonInt64(payload["duration_ms"])
	status, _ := jsonInt64(payload["status"])
	record.Response = &ExportResponse{
		Status: int(status), Account: str(payload["account"]), Provider: str(payload["provider"]),
		DurationMs: duration, Headers: headerMap(payload["response_headers"]), Body: body,
	}
	x.emit(*record)
	delete(x.requests, requestID)
}

func (x *SegmentExporter) exportBody(body []byte, payload map[string]any) *ExportBody {
	sum := sha256.Sum256(body)
	got := hex.EncodeToString(sum[:])
	want := str(payload["sha256"])
	exported := &ExportBody{
		Bytes:           int64(len(body)),
		SHA256:          got,
		ContentEncoding: str(payload["content_encoding"]),
		Partial:         boolField(payload, "partial"),
	}
	x.Stats.Bodies++
	x.Stats.BodyBytes += int64(len(body))
	switch {
	case want == "":
	case want == got:
		exported.Verified = true
		x.Stats.Verified++
	default:
		x.Stats.Mismatched++
	}
	switch {
	case len(bytes.TrimSpace(body)) > 0 && json.Valid(body) && (bytes.TrimSpace(body)[0] == '{' || bytes.TrimSpace(body)[0] == '['):
		exported.JSON = append(json.RawMessage(nil), body...)
	case utf8.Valid(body):
		text := string(body)
		exported.Text = &text
	default:
		exported.Base64 = base64.StdEncoding.EncodeToString(body)
	}
	return exported
}

func (x *SegmentExporter) emit(record ExportRecord) {
	if x.Filter != nil && !x.Filter(record) {
		return
	}
	x.Stats.Records++
	if x.Emit != nil && x.err == nil {
		x.err = x.Emit(record)
	}
}

// Finish emits requests whose response never arrived and streams that never
// got a summary, in the order they were seen.
func (x *SegmentExporter) Finish() {
	for _, id := range x.order {
		if record, ok := x.requests[id]; ok {
			x.emit(*record)
			delete(x.requests, id)
		}
	}
	x.order = nil
	ids := make([]string, 0, len(x.streams))
	for id := range x.streams {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		stream := x.streams[id]
		body := x.exportBody(stream.data, map[string]any{})
		body.Partial = true
		x.emit(ExportRecord{
			Kind: "body", StreamID: id, AgentType: str(stream.payload["agent_type"]), SessionID: str(stream.payload["session_id"]),
			Timestamp: stream.at, Direction: str(stream.payload["direction"]), RequestID: str(stream.payload["request_id"]),
			Body: body, Segment: stream.segment, Host: stream.host,
		})
	}
	x.streams = map[string]*exportStream{}
}

func str(value any) string {
	switch v := value.(type) {
	case string:
		return v
	case json.Number:
		return v.String()
	case nil:
		return ""
	default:
		return fmt.Sprint(v)
	}
}

func jsonInt64(value any) (int64, bool) {
	switch v := value.(type) {
	case json.Number:
		n, err := v.Int64()
		return n, err == nil
	case float64:
		return int64(v), true
	case int:
		return int64(v), true
	case int64:
		return v, true
	default:
		return 0, false
	}
}

// normalizeNumbers turns json.Number leaves into float64 for applyDelta.
func normalizeNumbers(value any) any {
	switch v := value.(type) {
	case json.Number:
		n, err := v.Int64()
		if err != nil {
			return v
		}
		return float64(n)
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = normalizeNumbers(item)
		}
		return out
	default:
		return v
	}
}

func headerMap(value any) map[string][]string {
	raw, ok := value.(map[string]any)
	if !ok {
		return nil
	}
	out := make(map[string][]string, len(raw))
	for key, values := range raw {
		list, _ := values.([]any)
		for _, item := range list {
			out[key] = append(out[key], str(item))
		}
	}
	return out
}

// modelOf reads the top-level "model" of a JSON request body.
func modelOf(body []byte) string {
	if len(body) == 0 || body[0] != '{' {
		return ""
	}
	var head struct {
		Model    string `json:"model"`
		Response struct {
			Model string `json:"model"`
		} `json:"response"`
	}
	if json.Unmarshal(body, &head) != nil {
		return ""
	}
	if head.Model != "" {
		return head.Model
	}
	return head.Response.Model
}

// NewSegmentDecoder reads a segment (any run of whole zstd frames; the seek
// table is a skippable frame and is skipped).
func NewSegmentDecoder(r io.Reader) (*zstd.Decoder, error) {
	return zstd.NewReader(r, zstd.WithDecoderMaxWindow(1<<31), zstd.WithDecoderConcurrency(1))
}

// ReadIndexRows parses one index blob (zstd-compressed JSON lines).
func ReadIndexRows(r io.Reader, visit func(IndexRow) error) error {
	decoder, err := NewSegmentDecoder(r)
	if err != nil {
		return err
	}
	defer decoder.Close()
	reader := bufio.NewReaderSize(decoder, 1<<20)
	for {
		line, err := reader.ReadBytes('\n')
		if len(bytes.TrimSpace(line)) > 0 {
			var row IndexRow
			if jsonErr := json.Unmarshal(line, &row); jsonErr == nil {
				if visitErr := visit(row); visitErr != nil {
					return visitErr
				}
			}
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// CallBodies is one call rebuilt from its index row and the byte ranges it
// names.
type CallBodies struct {
	Row      IndexRow       `json:"row"`
	Request  *ExportBody    `json:"request,omitempty"`
	Response *ExportBody    `json:"response,omitempty"`
	Messages []ExportRecord `json:"messages,omitempty"`
	Verified bool           `json:"verified"`
}

// RangeReader fetches a byte range of a blob.
type RangeReader func(blob string, offset, length int64) ([]byte, error)

// ExportCall rebuilds one call by reading only the frames its row names:
// from the first frame its request depends on to the end of its response.
func ExportCall(row IndexRow, read RangeReader) (CallBodies, error) {
	out := CallBodies{Row: row}
	collect := func(blob string, offset, length int64) ([]ExportRecord, ExportStats, error) {
		if blob == "" || length <= 0 {
			return nil, ExportStats{}, nil
		}
		data, err := read(blob, offset, length)
		if err != nil {
			return nil, ExportStats{}, err
		}
		decoder, err := NewSegmentDecoder(bytes.NewReader(data))
		if err != nil {
			return nil, ExportStats{}, err
		}
		defer decoder.Close()
		var records []ExportRecord
		exporter := NewSegmentExporter(func(record ExportRecord) error {
			records = append(records, record)
			return nil
		})
		if err := exporter.ReadSegment(blob, decoder); err != nil {
			return nil, exporter.Stats, err
		}
		exporter.Finish()
		return records, exporter.Stats, nil
	}
	var records []ExportRecord
	var failed bool
	same := row.RequestBlob == row.ResponseBlob && row.ResponseBlob != ""
	if same {
		start := min(row.RequestOffset, row.ResponseOffset)
		end := max(row.RequestOffset+row.RequestLength, row.ResponseOffset+row.ResponseLength)
		got, stats, err := collect(row.RequestBlob, start, end-start)
		if err != nil {
			return out, err
		}
		records, failed = got, stats.Failed()
	} else {
		for _, part := range [][3]any{{row.RequestBlob, row.RequestOffset, row.RequestLength}, {row.ResponseBlob, row.ResponseOffset, row.ResponseLength}} {
			got, stats, err := collect(part[0].(string), part[1].(int64), part[2].(int64))
			if err != nil {
				return out, err
			}
			records = append(records, got...)
			failed = failed || stats.Failed()
		}
	}
	connection, stream, websocket := row.RequestID, "", row.Kind == "websocket"
	if websocket {
		if index := strings.LastIndex(row.RequestID, "/"); index > 0 {
			connection, stream = row.RequestID[:index], row.RequestID[index+1:]
		}
	}
	inCall := false
	for _, record := range records {
		if websocket {
			if record.Kind != "websocket_message" || record.RequestID != connection {
				continue
			}
			if record.Direction == "client_to_upstream" {
				inCall = record.StreamID == stream
				if inCall {
					out.Request = record.Message
				}
				continue
			}
			if inCall {
				out.Messages = append(out.Messages, record)
			}
			continue
		}
		if record.RequestID != row.RequestID {
			continue
		}
		switch {
		case record.Request != nil:
			out.Request = record.Request
		case record.Body != nil && record.Direction == "client_to_upstream":
			out.Request = record.Body
		case record.Body != nil && record.Direction == "upstream_to_client":
			out.Response = record.Body
		}
		if record.Response != nil && record.Response.Body != nil {
			out.Response = record.Response.Body
		}
	}
	// Other calls in the range may cite bases before it; only this call's
	// bodies decide.
	_ = failed
	out.Verified = out.Request != nil && out.Request.Verified && out.Request.SHA256 == row.RequestSHA256
	if !websocket && row.ResponseSHA256 != "" && !row.ResponseSplit {
		out.Verified = out.Verified && out.Response != nil && out.Response.Verified && out.Response.SHA256 == row.ResponseSHA256
	}
	return out, nil
}
