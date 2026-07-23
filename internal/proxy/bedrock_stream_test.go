package proxy

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
)

type bedrockHeaderKV struct {
	name  string
	value string
}

func buildEventStreamFrameWithHeaders(t *testing.T, headers []bedrockHeaderKV, payload []byte) []byte {
	t.Helper()
	var headerBlock bytes.Buffer
	for _, h := range headers {
		if len(h.name) > 255 {
			t.Fatalf("header name too long: %s", h.name)
		}
		headerBlock.WriteByte(byte(len(h.name)))
		headerBlock.WriteString(h.name)
		headerBlock.WriteByte(7)
		if len(h.value) > 65535 {
			t.Fatalf("header value too long: %s", h.name)
		}
		var lenBuf [2]byte
		binary.BigEndian.PutUint16(lenBuf[:], uint16(len(h.value)))
		headerBlock.Write(lenBuf[:])
		headerBlock.WriteString(h.value)
	}
	headersBytes := headerBlock.Bytes()
	total := 16 + len(headersBytes) + len(payload)
	frame := make([]byte, total)
	binary.BigEndian.PutUint32(frame[0:4], uint32(total))
	binary.BigEndian.PutUint32(frame[4:8], uint32(len(headersBytes)))
	copy(frame[12:12+len(headersBytes)], headersBytes)
	copy(frame[12+len(headersBytes):12+len(headersBytes)+len(payload)], payload)
	return frame
}

func buildBedrockExceptionFrame(t *testing.T, exceptionType, body string) []byte {
	t.Helper()
	return buildEventStreamFrameWithHeaders(t, []bedrockHeaderKV{
		{name: ":message-type", value: "exception"},
		{name: ":exception-type", value: exceptionType},
		{name: ":event-type", value: exceptionType},
	}, []byte(body))
}

func buildMalformedHeaderEventFrame(t *testing.T, eventJSON string) []byte {
	t.Helper()
	payload, err := json.Marshal(map[string]string{"bytes": base64.StdEncoding.EncodeToString([]byte(eventJSON))})
	if err != nil {
		t.Fatal(err)
	}
	headers := []byte{5, 'x', 'y'}
	total := 16 + len(headers) + len(payload)
	frame := make([]byte, total)
	binary.BigEndian.PutUint32(frame[0:4], uint32(total))
	binary.BigEndian.PutUint32(frame[4:8], uint32(len(headers)))
	copy(frame[12:12+len(headers)], headers)
	copy(frame[12+len(headers):12+len(headers)+len(payload)], payload)
	return frame
}

func TestTranscodeBedrockToSSEHappyPathByteIdentical(t *testing.T) {
	startJSON := `{"type":"message_start","message":{"usage":{"input_tokens":10}}}`
	deltaJSON := `{"type":"message_delta","usage":{"output_tokens":7}}`
	stopJSON := `{"type":"message_stop"}`
	stream := append(append(append([]byte{}, buildEventStreamFrame(t, startJSON)...), buildEventStreamFrame(t, deltaJSON)...), buildEventStreamFrame(t, stopJSON)...)

	var logBuf bytes.Buffer
	rec := httptest.NewRecorder()
	result := transcodeBedrockToSSE(rec, bytes.NewReader(stream), slog.New(slog.NewTextHandler(&logBuf, nil)), "aw0", "us-east-1")

	want := "event: message_start\ndata: " + startJSON + "\n\n" +
		"event: message_delta\ndata: " + deltaJSON + "\n\n" +
		"event: message_stop\ndata: " + stopJSON + "\n\n"
	if rec.Body.String() != want {
		t.Fatalf("SSE output changed:\ngot  %q\nwant %q", rec.Body.String(), want)
	}
	if logBuf.Len() != 0 {
		t.Fatalf("clean stream logged unexpectedly:\n%s", logBuf.String())
	}
	if !result.HaveUsage || result.Usage.InputTokens != 10 || result.Usage.OutputTokens != 7 || !result.SawMessageStop || result.ReadErr != nil || result.ExceptionType != "" {
		t.Fatalf("result = %+v, want clean usage result", result)
	}
}

func TestTranscodeBedrockToSSEMidstreamException(t *testing.T) {
	stream := append(
		buildEventStreamFrame(t, `{"type":"message_start","message":{"usage":{"input_tokens":3}}}`),
		buildBedrockExceptionFrame(t, "throttlingException", `{"message":"Rate exceeded"}`)...,
	)
	var logBuf bytes.Buffer
	rec := httptest.NewRecorder()
	result := transcodeBedrockToSSE(rec, bytes.NewReader(stream), slog.New(slog.NewTextHandler(&logBuf, nil)), "aw0", "us-west-2")

	out := rec.Body.String()
	if !strings.Contains(out, "event: message_start") {
		t.Fatalf("missing forwarded first event:\n%s", out)
	}
	if !strings.Contains(out, "event: error") || !strings.Contains(out, `"type":"overloaded_error"`) || !strings.Contains(out, "Bedrock throttled mid-stream") {
		t.Fatalf("missing throttling SSE error:\n%s", out)
	}
	if result.ExceptionType != "throttlingException" {
		t.Fatalf("exception type = %q, want throttlingException", result.ExceptionType)
	}
	logs := logBuf.String()
	if !strings.Contains(logs, "claude-fable bedrock mid-stream exception") ||
		!strings.Contains(logs, "exception_type=throttlingException") ||
		!strings.Contains(logs, "bedrock_source=aw0") ||
		!strings.Contains(logs, "region=us-west-2") ||
		!strings.Contains(logs, "Rate exceeded") {
		t.Fatalf("missing diagnostic exception log:\n%s", logs)
	}
}

func TestTranscodeBedrockToSSETruncatedStream(t *testing.T) {
	stream := buildEventStreamFrame(t, `{"type":"message_start","message":{"usage":{"input_tokens":3}}}`)
	var logBuf bytes.Buffer
	rec := httptest.NewRecorder()
	result := transcodeBedrockToSSE(rec, bytes.NewReader(stream), slog.New(slog.NewTextHandler(&logBuf, nil)), "aw0", "us-east-1")

	if result.ReadErr != io.EOF || result.SawMessageStop {
		t.Fatalf("result = %+v, want EOF without message_stop", result)
	}
	out := rec.Body.String()
	if !strings.Contains(out, "event: error") || !strings.Contains(out, `"type":"api_error"`) || !strings.Contains(out, "Bedrock stream interrupted") {
		t.Fatalf("missing synthetic interruption error:\n%s", out)
	}
	logs := logBuf.String()
	if !strings.Contains(logs, "level=ERROR") ||
		!strings.Contains(logs, "claude-fable bedrock stream truncated") ||
		!strings.Contains(logs, "bedrock_source=aw0") ||
		!strings.Contains(logs, "region=us-east-1") ||
		!strings.Contains(logs, "saw_message_stop=false") {
		t.Fatalf("missing truncation log:\n%s", logs)
	}
}

func TestTranscodeBedrockToSSEMalformedHeadersDoNotBreakPayloadExtraction(t *testing.T) {
	stopJSON := `{"type":"message_stop"}`
	stream := buildMalformedHeaderEventFrame(t, stopJSON)
	rec := httptest.NewRecorder()
	result := transcodeBedrockToSSE(rec, bytes.NewReader(stream), nil, "", "")
	if !result.SawMessageStop {
		t.Fatalf("result = %+v, want message_stop despite malformed headers", result)
	}
	if got := rec.Body.String(); got != "event: message_stop\ndata: "+stopJSON+"\n\n" {
		t.Fatalf("output = %q, want decoded message_stop", got)
	}

	garbageHeaderLen := buildEventStreamFrame(t, stopJSON)
	binary.BigEndian.PutUint32(garbageHeaderLen[4:8], 999)
	rec = httptest.NewRecorder()
	result = transcodeBedrockToSSE(rec, bytes.NewReader(garbageHeaderLen), nil, "", "")
	if !result.SawMessageStop {
		t.Fatalf("result = %+v, want message_stop despite garbage headersLen", result)
	}
}
