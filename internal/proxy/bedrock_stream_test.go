package proxy

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
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

func TestServeClaudeFableBedrockPrimaryFallsThroughOnExceptionFirstStream(t *testing.T) {
	bodyStr := `{"model":"claude-fable-5","stream":true,"max_tokens":8,"messages":[]}`
	var logBuf bytes.Buffer
	rt := bedrockRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(bytes.NewReader(buildBedrockExceptionFrame(t, "modelStreamErrorException", `{"message":"boom"}`))),
			Header:     http.Header{"Content-Type": []string{"application/vnd.amazon.eventstream"}},
		}, nil
	})
	s := Server{
		MaxBodyBytes:        1 << 20,
		FableBedrockPrimary: true,
		Logger:              slog.New(slog.NewTextHandler(&logBuf, nil)),
		Bedrock:             &BedrockConfig{Regions: []string{"us-east-1"}, Credentials: staticBedrockCreds(), Transport: rt},
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(bodyStr))
	rec := httptest.NewRecorder()
	if s.serveClaudeFableBedrockPrimary(rec, req) {
		t.Fatal("expected exception-first stream to fall through")
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("recorder body = %q, want untouched", rec.Body.String())
	}
	restored, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatal(err)
	}
	if string(restored) != bodyStr {
		t.Fatalf("restored body = %q, want %q", string(restored), bodyStr)
	}
	if logs := logBuf.String(); !strings.Contains(logs, "claude-fable bedrock stream failed before content") || !strings.Contains(logs, "exception_type=modelStreamErrorException") {
		t.Fatalf("missing first-event failure log:\n%s", logs)
	}
}

func TestServeClaudeFableBedrockPrimaryFallsThroughOnEmptyStream(t *testing.T) {
	bodyStr := `{"model":"claude-fable-5","stream":true,"max_tokens":8,"messages":[]}`
	rt := bedrockRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(bytes.NewReader(nil)),
			Header:     http.Header{"Content-Type": []string{"application/vnd.amazon.eventstream"}},
		}, nil
	})
	s := Server{
		MaxBodyBytes:        1 << 20,
		FableBedrockPrimary: true,
		Bedrock:             &BedrockConfig{Regions: []string{"us-east-1"}, Credentials: staticBedrockCreds(), Transport: rt},
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(bodyStr))
	rec := httptest.NewRecorder()
	if s.serveClaudeFableBedrockPrimary(rec, req) {
		t.Fatal("expected empty stream to fall through")
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("recorder body = %q, want untouched", rec.Body.String())
	}
	restored, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatal(err)
	}
	if string(restored) != bodyStr {
		t.Fatalf("restored body = %q, want %q", string(restored), bodyStr)
	}
}

// A committed stream that goes silent used to block inside Read until the
// client's stall detector canceled the request minutes later ("The response
// stopped arriving"). The idle watchdog must close the body and surface a
// retryable in-band error instead.
func TestTranscodeBedrockIdleWatchdogAbortsSilentStream(t *testing.T) {
	frames := append(
		buildEventStreamFrame(t, `{"type":"message_start","message":{"usage":{"input_tokens":3}}}`),
		buildEventStreamFrame(t, `{"type":"content_block_delta","delta":{"type":"text_delta","text":"hi"}}`)...,
	)
	pr, pw := io.Pipe()
	go func() {
		_, _ = pw.Write(frames)
		// Silence forever: only the watchdog closing pr can end the stream.
	}()
	watchdog := newBedrockIdleWatchdogReader(pr, pr, 50*time.Millisecond, 50*time.Millisecond)
	defer watchdog.stop()

	var logBuf bytes.Buffer
	rec := httptest.NewRecorder()
	result := transcodeBedrockToSSE(rec, watchdog, slog.New(slog.NewTextHandler(&logBuf, nil)), "aw0", "us-east-1")

	if !errors.Is(result.ReadErr, errBedrockStreamIdle) {
		t.Fatalf("read err = %v, want errBedrockStreamIdle", result.ReadErr)
	}
	out := rec.Body.String()
	if !strings.Contains(out, "event: content_block_delta") {
		t.Fatalf("committed content missing from output:\n%s", out)
	}
	if !strings.Contains(out, "event: error") || !strings.Contains(out, `"type":"overloaded_error"`) || !strings.Contains(out, "went idle mid-response") {
		t.Fatalf("missing retryable idle SSE error:\n%s", out)
	}
	logs := logBuf.String()
	if !strings.Contains(logs, "claude-fable bedrock stream idle, aborted by watchdog") ||
		!strings.Contains(logs, "content_deltas_forwarded=1") {
		t.Fatalf("missing idle watchdog log:\n%s", logs)
	}
}

// Frames arriving within the timeout must keep resetting the watchdog: a slow
// but alive stream reaches message_stop with no synthetic error.
func TestBedrockIdleWatchdogSparesSlowButAliveStream(t *testing.T) {
	var frames [][]byte
	for i := 0; i < 5; i++ {
		frames = append(frames, buildEventStreamFrame(t, `{"type":"content_block_delta","delta":{"type":"text_delta","text":"x"}}`))
	}
	frames = append(frames, buildEventStreamFrame(t, `{"type":"message_stop"}`))
	pr, pw := io.Pipe()
	go func() {
		for _, frame := range frames {
			time.Sleep(60 * time.Millisecond)
			_, _ = pw.Write(frame)
		}
		_ = pw.Close()
	}()
	watchdog := newBedrockIdleWatchdogReader(pr, pr, 250*time.Millisecond, 250*time.Millisecond)
	defer watchdog.stop()

	rec := httptest.NewRecorder()
	result := transcodeBedrockToSSE(rec, watchdog, nil, "aw0", "us-east-1")
	if !result.SawMessageStop || result.ReadErr != nil {
		t.Fatalf("result = %+v, want clean message_stop", result)
	}
	if out := rec.Body.String(); strings.Contains(out, "event: error") {
		t.Fatalf("live stream got a synthetic error:\n%s", out)
	}
}

// End-to-end: the committed-stream goroutine in claudeFableBedrockResponse
// must arm the watchdog, so a client reading the returned SSE body sees the
// retryable error instead of a hang.
func TestClaudeFableBedrockIdleWatchdogEndToEnd(t *testing.T) {
	oldTimeout := claudeFableBedrockStreamIdleTimeout
	claudeFableBedrockStreamIdleTimeout = 50 * time.Millisecond
	defer func() { claudeFableBedrockStreamIdleTimeout = oldTimeout }()

	frames := append(
		buildEventStreamFrame(t, `{"type":"message_start","message":{"usage":{"input_tokens":3}}}`),
		buildEventStreamFrame(t, `{"type":"content_block_delta","delta":{"type":"text_delta","text":"hi"}}`)...,
	)
	rt := bedrockRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		pr, pw := io.Pipe()
		go func() {
			_, _ = pw.Write(frames)
			// Silence forever after commit.
		}()
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       pr,
			Header:     http.Header{"Content-Type": []string{"application/vnd.amazon.eventstream"}},
		}, nil
	})
	s := fableStreamTestServer(rt)
	resp, err := s.claudeFableBedrockResponse(t.Context(), fableStreamTestBody)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want committed 200", resp.StatusCode)
	}
	done := make(chan []byte, 1)
	go func() {
		sse, _ := io.ReadAll(resp.Body)
		done <- sse
	}()
	select {
	case sse := <-done:
		if !strings.Contains(string(sse), `"type":"overloaded_error"`) || !strings.Contains(string(sse), "went idle mid-response") {
			t.Fatalf("missing idle error event: %q", sse)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("stream still hanging: watchdog never fired")
	}
}

// The stream path must carry the cache_creation TTL split into the cost
// record; dropping it prices 1h writes at the 5m rate.
func TestTranscodeCarriesCacheCreationDetail(t *testing.T) {
	start := `{"type":"message_start","message":{"usage":{"input_tokens":5,"cache_creation_input_tokens":100,"cache_read_input_tokens":7,"cache_creation":{"ephemeral_5m_input_tokens":25,"ephemeral_1h_input_tokens":75}}}}`
	stream := append(buildEventStreamFrame(t, start), buildEventStreamFrame(t, `{"type":"message_stop"}`)...)
	var out bytes.Buffer
	result := transcodeBedrockToSSE(&out, bytes.NewReader(stream), nil, "src", "region")
	if result.Usage.CacheCreation.Ephemeral5m != 25 || result.Usage.CacheCreation.Ephemeral1h != 75 {
		t.Fatalf("cache_creation detail lost: %+v", result.Usage.CacheCreation)
	}
	if result.Usage.CacheWriteTokens != 100 {
		t.Fatalf("cache write total = %d, want 100", result.Usage.CacheWriteTokens)
	}
}

// The watchdog's two timeouts must switch on first bytes: before any byte the
// long initial deadline governs (prefill is silent by nature), after first
// bytes the short idle deadline governs (an answered stream that goes silent
// is stalled).
func TestBedrockIdleWatchdogSplitsInitialAndIdleTimeouts(t *testing.T) {
	// Answered then silent: killed by the short idle deadline, far before
	// the long initial one.
	pr, pw := io.Pipe()
	go func() { _, _ = pw.Write([]byte("x")) }()
	watchdog := newBedrockIdleWatchdogReader(pr, pr, 10*time.Second, 40*time.Millisecond)
	buf := make([]byte, 4)
	start := time.Now()
	if n, err := watchdog.Read(buf); n != 1 || err != nil {
		t.Fatalf("first read = (%d, %v), want the answered byte", n, err)
	}
	_, err := watchdog.Read(buf) // silence: only the idle deadline can end this
	elapsed := time.Since(start)
	watchdog.stop()
	if !errors.Is(err, errBedrockStreamIdle) {
		t.Fatalf("read err = %v, want errBedrockStreamIdle", err)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("answered-then-silent stream killed after %v, want the short idle deadline", elapsed)
	}

	// Never answered: the initial deadline governs even when idle is shorter.
	pr2, _ := io.Pipe()
	watchdog2 := newBedrockIdleWatchdogReader(pr2, pr2, 200*time.Millisecond, 10*time.Millisecond)
	start = time.Now()
	_, err = watchdog2.Read(buf)
	elapsed = time.Since(start)
	watchdog2.stop()
	if !errors.Is(err, errBedrockStreamIdle) {
		t.Fatalf("read err = %v, want errBedrockStreamIdle", err)
	}
	if elapsed < 150*time.Millisecond {
		t.Fatalf("silent stream killed after %v, want the longer initial deadline to govern before first bytes", elapsed)
	}
}

// Steady-state traffic must always start on the primary (first configured)
// region so per-region prompt caches stay hot; only retries escalate.
func TestOrderedAttemptsPrimaryFirstAndRetryEscalation(t *testing.T) {
	cfg := &BedrockConfig{
		Regions: []string{"us-east-1", "us-west-2"},
		Sources: []BedrockCredentialSource{{Name: "aw1", Credentials: staticBedrockCreds()}},
	}
	for i := 0; i < 3; i++ {
		if got := cfg.orderedAttempts(0)[0].Region; got != "us-east-1" {
			t.Fatalf("first attempt %d starts at %s, want the primary region every time", i, got)
		}
	}
	if got := cfg.orderedAttempts(1)[0].Region; got != "us-west-2" {
		t.Fatalf("retry index 1 starts at %s, want us-west-2", got)
	}
	if got := cfg.orderedAttempts(2)[0].Region; got != "us-east-1" {
		t.Fatalf("retry index 2 starts at %s, want wraparound to us-east-1", got)
	}
}

// End to end: a fable stream that sheds on the primary region must be retried
// on the next region, and the client sees only the good stream.
func TestClaudeFableBedrockRetryEscalatesToNextRegion(t *testing.T) {
	overloaded := buildEventStreamFrame(t, `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`)
	good := append(
		buildEventStreamFrame(t, `{"type":"message_start","message":{"usage":{"input_tokens":4}}}`),
		append(
			buildEventStreamFrame(t, `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ok"}}`),
			buildEventStreamFrame(t, `{"type":"message_stop"}`)...,
		)...,
	)
	var hosts []string
	rt := bedrockRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		hosts = append(hosts, req.Host)
		body := overloaded
		if strings.Contains(req.Host, "us-west-2") {
			body = good
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(bytes.NewReader(body)),
			Header:     http.Header{"Content-Type": []string{"application/vnd.amazon.eventstream"}},
		}, nil
	})
	s := Server{Bedrock: &BedrockConfig{
		Regions:   []string{"us-east-1", "us-west-2"},
		Sources:   []BedrockCredentialSource{{Name: "aw1", Credentials: staticBedrockCreds()}},
		Transport: rt,
	}}
	resp, err := s.claudeFableBedrockResponse(t.Context(), fableStreamTestBody)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 after regional escalation", resp.StatusCode)
	}
	sse, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(sse), "event: message_stop") || strings.Contains(string(sse), "overloaded_error") {
		t.Fatalf("escalated stream wrong: %q", sse)
	}
	if len(hosts) != 2 || !strings.Contains(hosts[0], "us-east-1") || !strings.Contains(hosts[1], "us-west-2") {
		t.Fatalf("hosts = %v, want primary first then escalation", hosts)
	}
}

// Tool JSON buffers under its own longer window: a shed during slow tool-JSON
// emission past the thinking window but inside the JSON window must retry
// invisibly, and JSON-window expiry still commits.
func TestBedrockToolJSONWindowOutlivesThinkingWindow(t *testing.T) {
	jsonFrame := bedrockFrame{payload: []byte(`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"x"}}`)}
	thinkFrame := bedrockFrame{payload: []byte(`{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"hm"}}`)}

	past := claudeFableBedrockCommitWindow + time.Second
	if decisive, _, _ := bedrockFrameDecision(jsonFrame, past); decisive {
		t.Fatal("tool JSON committed at the thinking window; it must use its own longer window")
	}
	if decisive, _, _ := bedrockFrameDecision(thinkFrame, past); !decisive {
		t.Fatal("thinking must still commit at its own window")
	}
	decisive, failure, reason := bedrockFrameDecision(jsonFrame, claudeFableBedrockToolJSONCommitWindow)
	if !decisive || failure || reason != "window_expired_input_json_delta" {
		t.Fatalf("JSON window expiry: decisive=%v failure=%v reason=%q", decisive, failure, reason)
	}
}
