package proxy

import (
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/zstd"
	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/selectacct"
	"github.com/manaflow-ai/subrouter/session"
)

// realisticAnthropicStream is the shape api.anthropic.com streams to Claude
// Code: input and cache usage on message_start, cumulative output usage on
// message_delta, and a long run of content deltas in between. One delta line
// is longer than the scanner's per-line head window, and the whole stream is
// far longer than head plus tail.
func realisticAnthropicStream() string {
	var b strings.Builder
	b.WriteString("event: message_start\n")
	b.WriteString(`data: {"type":"message_start","message":{"model":"claude-opus-5-5","id":"msg_01","type":"message","role":"assistant","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":12,"cache_creation_input_tokens":3000,"cache_read_input_tokens":90000,"cache_creation":{"ephemeral_5m_input_tokens":3000,"ephemeral_1h_input_tokens":0},"output_tokens":1,"service_tier":"standard"}}}` + "\n\n")
	b.WriteString("event: content_block_start\n")
	b.WriteString(`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` + "\n\n")
	b.WriteString("event: ping\ndata: {\"type\": \"ping\"}\n\n")
	for i := 0; i < 4000; i++ {
		fmt.Fprintf(&b, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"token %d with \\\"usage\\\": quoted text \"}}\n\n", i)
	}
	b.WriteString("event: content_block_delta\n")
	b.WriteString(`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"` + strings.Repeat("x", 100<<10) + `"}}` + "\n\n")
	b.WriteString("event: content_block_stop\n")
	b.WriteString(`data: {"type":"content_block_stop","index":0}` + "\n\n")
	b.WriteString("event: message_delta\n")
	b.WriteString(`data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"input_tokens":12,"cache_creation_input_tokens":3000,"cache_read_input_tokens":90000,"output_tokens":4321}}` + "\n\n")
	b.WriteString("event: message_stop\n")
	b.WriteString(`data: {"type":"message_stop"}` + "\n\n")
	return b.String()
}

type flushWriteCloser interface {
	io.WriteCloser
	Flush() error
}

type nopFlushWriter struct{ io.Writer }

func (nopFlushWriter) Close() error { return nil }
func (nopFlushWriter) Flush() error { return nil }

func newTestEncoder(t *testing.T, encoding string, w io.Writer) flushWriteCloser {
	t.Helper()
	switch encoding {
	case "":
		return nopFlushWriter{w}
	case "gzip":
		return gzip.NewWriter(w)
	case "deflate":
		return zlib.NewWriter(w)
	case "br":
		return brotli.NewWriter(w)
	case "zstd":
		encoder, err := zstd.NewWriter(w)
		if err != nil {
			t.Fatal(err)
		}
		return encoder
	}
	t.Fatalf("unknown encoding %q", encoding)
	return nil
}

func encodeForTest(t *testing.T, encoding, body string) []byte {
	t.Helper()
	var buf bytes.Buffer
	encoder := newTestEncoder(t, encoding, &buf)
	if _, err := io.WriteString(encoder, body); err != nil {
		t.Fatal(err)
	}
	if err := encoder.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// TestProxyRecordsTokenUsageForCompressedAnthropicStream reproduces the
// production traffic that landed every Claude turn in requests_without_usage:
// Claude Code sends Accept-Encoding, the proxy forwards it, and Anthropic
// answers with a compressed event stream. The client must still receive the
// exact compressed bytes upstream sent.
func TestProxyRecordsTokenUsageForCompressedAnthropicStream(t *testing.T) {
	// The upstream sends the whole stream in one burst, so the decoder can be
	// well behind at EOF, especially under -race; this test is about what is
	// decoded, not the wait bound.
	setTokenUsageDecodeLimits(t, 8<<20, 256<<20, 30*time.Second)
	stream := realisticAnthropicStream()
	for _, encoding := range []string{"", "gzip", "deflate", "br", "zstd"} {
		t.Run("encoding="+encoding, func(t *testing.T) {
			want := encodeForTest(t, encoding, stream)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				if got := r.Header.Get("Accept-Encoding"); !strings.Contains(got, "br") {
					t.Errorf("upstream Accept-Encoding = %q, want the client's value", got)
				}
				w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
				if encoding != "" {
					w.Header().Set("Content-Encoding", encoding)
				}
				w.WriteHeader(http.StatusOK)
				flusher, _ := w.(http.Flusher)
				// Odd write sizes so compressed frames and SSE lines both
				// straddle read boundaries.
				for data := want; len(data) > 0; {
					n := 1777
					if n > len(data) {
						n = len(data)
					}
					_, _ = w.Write(data[:n])
					if flusher != nil {
						flusher.Flush()
					}
					data = data[n:]
				}
			}))
			defer upstream.Close()
			upstreamURL, _ := url.Parse(upstream.URL)
			store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
			if err != nil {
				t.Fatal(err)
			}
			recorder := NewTokenUsageRecorder("", nil)
			handler := Server{
				ClaudeUpstream: upstreamURL,
				Accounts: []accounts.Account{{
					ID: "claude-a@example.com", Provider: accounts.ProviderClaude, AuthMode: accounts.AuthModeOAuth, Token: "tok-a",
				}},
				Sessions: store,
				Scheduler: selectacct.NewScheduler([]selectacct.Score{
					{AccountID: "claude-a@example.com", Provider: accounts.ProviderClaude, Headroom: 1, ShortHeadroom: 1},
				}),
				MaxBodyBytes: 1 << 20,
				TokenUsage:   recorder,
			}.Handler()
			subrouter := httptest.NewServer(handler)
			defer subrouter.Close()

			request, _ := http.NewRequest(http.MethodPost, subrouter.URL+"/v1/messages?beta=true",
				strings.NewReader(`{"model":"claude-opus-5-5","max_tokens":32000,"stream":true,"messages":[{"role":"user","content":"hi"}]}`))
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("Accept-Encoding", "gzip, deflate, br, zstd")
			request.Header.Set("X-Subrouter-Agent", "claude")
			request.Header.Set("X-Subrouter-Session", "session-usage")
			request.Header.Set(ClientNameHeader, "leos-mbp")
			// A client that sets Accept-Encoding itself gets the raw bytes.
			client := &http.Client{Transport: &http.Transport{DisableCompression: true}}
			response, err := client.Do(request)
			if err != nil {
				t.Fatal(err)
			}
			got, err := io.ReadAll(response.Body)
			response.Body.Close()
			if err != nil {
				t.Fatal(err)
			}
			if response.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, body %q", response.StatusCode, got)
			}
			if response.Header.Get("Content-Encoding") != encoding {
				t.Fatalf("client Content-Encoding = %q, want %q", response.Header.Get("Content-Encoding"), encoding)
			}
			if !bytes.Equal(got, want) {
				t.Fatalf("client body changed: got %d bytes, want %d", len(got), len(want))
			}

			var rows []TokenUsageRow
			deadline := time.Now().Add(2 * time.Second)
			for {
				rows, err = recorder.Rows(time.Now().Add(-time.Hour))
				if err != nil {
					t.Fatal(err)
				}
				if len(rows) > 0 || time.Now().After(deadline) {
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			if len(rows) != 1 {
				t.Fatalf("rows = %+v, want one", rows)
			}
			row := rows[0]
			if row.Provider != "claude" || row.AccountID != "claude-a@example.com" || row.Model != "claude-opus-5-5" ||
				row.Requests != 1 || row.RequestsWithoutUsage != 0 ||
				row.InputTokens != 12+3000+90000 || row.CachedInputTokens != 90000 || row.CacheWriteInputTokens != 3000 ||
				row.OutputTokens != 4321 {
				t.Fatalf("row = %+v", row)
			}
		})
	}
}

// TestTokenUsageBodyUndecodableEncodingCountsWithoutUsage keeps the contract
// that a turn whose usage cannot be read is still counted, and that a body in
// an encoding the scanner cannot decode never blocks or alters the stream.
func TestTokenUsageBodyUndecodableEncodingCountsWithoutUsage(t *testing.T) {
	for _, tc := range []struct {
		name, encoding string
		body           []byte
	}{
		{"unknown encoding", "compress", []byte("opaque bytes")},
		{"corrupt gzip", "gzip", []byte("not gzip at all, but long enough to be read in several pieces")},
		{"corrupt deflate", "deflate", []byte("not deflate at all, but long enough to be read in several pieces")},
		{"corrupt zlib deflate", "deflate", append([]byte{0x78, 0x9c}, []byte("broken zlib payload that is not a deflate stream")...)},
		{"corrupt zstd", "zstd", []byte("not zstd at all, but long enough to be read in several pieces")},
		{"corrupt br", "br", []byte("not brotli at all, but long enough to be read in several pieces")},
		{"zstd window over 8 MiB", "zstd", zstdWithWindow(t, 32<<20, realisticAnthropicStream())},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var recorded, ok bool
			body := newTokenUsageBody(io.NopCloser(bytes.NewReader(tc.body)), "text/event-stream", tc.encoding,
				func(_ tokenUsage, _ string, gotUsage bool) { recorded, ok = true, gotUsage })
			got, err := io.ReadAll(iotestHalfReader{body})
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, tc.body) {
				t.Fatalf("body changed: %q", got)
			}
			_ = body.Close()
			if !recorded || ok {
				t.Fatalf("recorded=%v ok=%v, want counted without usage", recorded, ok)
			}
		})
	}
}

// iotestHalfReader reads at most a few bytes per call, like a slow stream.
type iotestHalfReader struct{ r io.Reader }

func (h iotestHalfReader) Read(p []byte) (int, error) {
	if len(p) > 5 {
		p = p[:5]
	}
	return h.r.Read(p)
}

// zstdWithWindow encodes body as a streaming frame that declares window.
func zstdWithWindow(t *testing.T, window int, body string) []byte {
	t.Helper()
	var buf bytes.Buffer
	encoder, err := zstd.NewWriter(&buf, zstd.WithWindowSize(window), zstd.WithSingleSegment(false))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(encoder, body); err != nil {
		t.Fatal(err)
	}
	if err := encoder.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func setTokenUsageDecodeLimits(t *testing.T, maxPending int, maxDecoded int64, wait time.Duration) {
	t.Helper()
	oldPending, oldDecoded, oldWait := tokenUsageMaxPendingBytes, tokenUsageMaxDecodedBytes, tokenUsageDecodeWait
	tokenUsageMaxPendingBytes, tokenUsageMaxDecodedBytes, tokenUsageDecodeWait = maxPending, maxDecoded, wait
	t.Cleanup(func() {
		tokenUsageMaxPendingBytes, tokenUsageMaxDecodedBytes, tokenUsageDecodeWait = oldPending, oldDecoded, oldWait
	})
}

// blockTokenUsageDecoder makes the next decoders stall until release is
// closed, as a decoder that has fallen behind would.
func blockTokenUsageDecoder(t *testing.T) (release chan struct{}) {
	t.Helper()
	release = make(chan struct{})
	old := openTokenUsageDecoderFunc
	openTokenUsageDecoderFunc = func(encoding string, r io.Reader) (io.Reader, func(), error) {
		<-release
		return old(encoding, r)
	}
	t.Cleanup(func() { openTokenUsageDecoderFunc = old })
	return release
}

func decodingSinkOf(t *testing.T, body io.ReadCloser) *tokenUsageDecodingSink {
	t.Helper()
	sink, ok := body.(*tokenUsageBody).sink.(*tokenUsageDecodingSink)
	if !ok {
		t.Fatalf("sink = %T, want a decoding sink", body.(*tokenUsageBody).sink)
	}
	return sink
}

func waitDecoderExit(t *testing.T, sink *tokenUsageDecodingSink) {
	t.Helper()
	select {
	case <-sink.done:
	case <-time.After(5 * time.Second):
		t.Fatal("decoder goroutine did not exit")
	}
}

type usageResult struct {
	calls int
	usage tokenUsage
	ok    bool
}

func (r *usageResult) record(usage tokenUsage, _ string, ok bool) {
	r.calls++
	r.usage, r.ok = usage, ok
}

// A decoder that falls behind must not let queued compressed bytes grow
// without bound, and must not hold the client's EOF.
func TestTokenUsageDecoderBehindCapsPendingAndDoesNotHoldEOF(t *testing.T) {
	setTokenUsageDecodeLimits(t, 4<<10, 256<<20, 50*time.Millisecond)
	release := blockTokenUsageDecoder(t)
	wire := encodeForTest(t, "gzip", realisticAnthropicStream())
	if len(wire) <= 4<<10 {
		t.Fatalf("compressed stream is %d bytes, want more than the pending cap", len(wire))
	}
	var result usageResult
	body := newTokenUsageBody(io.NopCloser(bytes.NewReader(wire)), "text/event-stream", "gzip", result.record)
	sink := decodingSinkOf(t, body)
	start := time.Now()
	got, err := io.ReadAll(body)
	if err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("EOF took %v with a stalled decoder", elapsed)
	}
	if !bytes.Equal(got, wire) {
		t.Fatal("client body changed")
	}
	sink.queue.mu.Lock()
	pending, abandoned := len(sink.queue.pending), sink.queue.abandoned
	sink.queue.mu.Unlock()
	if !abandoned || pending != 0 {
		t.Fatalf("queue abandoned=%v pending=%d, want abandoned and empty", abandoned, pending)
	}
	if !sink.failed.Load() {
		t.Fatal("overflow did not mark the turn as without usage")
	}
	if result.calls != 1 || result.ok {
		t.Fatalf("result = %+v, want one call without usage", result)
	}
	close(release)
	waitDecoderExit(t, sink)
	_ = body.Close()
	if result.calls != 1 {
		t.Fatalf("recorded %d times", result.calls)
	}
}

// Under the pending cap, a decoder still busy at EOF is abandoned after the
// wait instead of delaying the client.
func TestTokenUsageDecoderStillBusyAtEOFIsAbandoned(t *testing.T) {
	setTokenUsageDecodeLimits(t, 8<<20, 256<<20, 30*time.Millisecond)
	release := blockTokenUsageDecoder(t)
	wire := encodeForTest(t, "br", realisticAnthropicStream())
	var result usageResult
	body := newTokenUsageBody(io.NopCloser(bytes.NewReader(wire)), "text/event-stream", "br", result.record)
	sink := decodingSinkOf(t, body)
	start := time.Now()
	if _, err := io.ReadAll(body); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("EOF took %v", elapsed)
	}
	if result.calls != 1 || result.ok {
		t.Fatalf("result = %+v, want one call without usage", result)
	}
	close(release)
	waitDecoderExit(t, sink)
}

// A body that inflates past the decoded cap stops decoding and counts
// without usage.
func TestTokenUsageDecoderStopsPastDecodedCap(t *testing.T) {
	setTokenUsageDecodeLimits(t, 8<<20, 1<<20, 5*time.Second)
	stream := realisticAnthropicStream() // about 280 KiB decoded
	var padded strings.Builder
	for padded.Len() < 4<<20 {
		padded.WriteString(": keepalive padding line\n")
	}
	padded.WriteString(stream)
	for _, tc := range []struct {
		name   string
		body   string
		wantOK bool
	}{
		{"under the cap", stream, true},
		{"over the cap", padded.String(), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wire := encodeForTest(t, "gzip", tc.body)
			var result usageResult
			body := newTokenUsageBody(io.NopCloser(bytes.NewReader(wire)), "text/event-stream", "gzip", result.record)
			sink := decodingSinkOf(t, body)
			if _, err := io.ReadAll(body); err != nil {
				t.Fatal(err)
			}
			waitDecoderExit(t, sink)
			if result.calls != 1 || result.ok != tc.wantOK {
				t.Fatalf("result = %+v, want ok=%v", result, tc.wantOK)
			}
			if tc.wantOK && result.usage.OutputTokens != 4321 {
				t.Fatalf("usage = %+v", result.usage)
			}
		})
	}
}

// blockingHalfBody serves the first half of wire, then blocks until closed,
// like an upstream stream the client abandons midway.
type blockingHalfBody struct {
	r      *bytes.Reader
	closed chan struct{}
	once   sync.Once
}

func (b *blockingHalfBody) Read(p []byte) (int, error) {
	if b.r.Len() > 0 {
		return b.r.Read(p)
	}
	<-b.closed
	return 0, io.ErrUnexpectedEOF
}

func (b *blockingHalfBody) Close() error {
	b.once.Do(func() { close(b.closed) })
	return nil
}

// Closing a body mid-stream returns promptly, records once, and leaves no
// decoder goroutine behind, for every encoding.
func TestTokenUsageBodyEarlyCloseEveryEncoding(t *testing.T) {
	stream := realisticAnthropicStream()
	for _, encoding := range []string{"", "gzip", "deflate", "br", "zstd"} {
		t.Run("encoding="+encoding, func(t *testing.T) {
			wire := encodeForTest(t, encoding, stream)
			inner := &blockingHalfBody{r: bytes.NewReader(wire[:len(wire)/2]), closed: make(chan struct{})}
			var result usageResult
			body := newTokenUsageBody(inner, "text/event-stream", encoding, result.record)
			buf := make([]byte, 4096)
			for read := 0; read < len(wire)/4; {
				n, err := body.Read(buf)
				if err != nil {
					t.Fatal(err)
				}
				read += n
			}
			closed := make(chan struct{})
			go func() {
				_ = body.Close()
				close(closed)
			}()
			select {
			case <-closed:
			case <-time.After(2 * time.Second):
				t.Fatal("Close did not return")
			}
			if result.calls != 1 {
				t.Fatalf("recorded %d times, want once", result.calls)
			}
			if encoding != "" {
				waitDecoderExit(t, decodingSinkOf(t, body))
			}
		})
	}
}

// A client that cancels mid-stream through the proxy still gets its turn
// counted, for every encoding. This drives the Codex HTTP route: the Claude
// route holds a stream it cannot parse in claudeStreamOverloaded until the
// in-flight upstream read returns, so a half-sent Claude stream never reaches
// the client at all.
func TestProxyCountsCompressedTurnWhenClientCancels(t *testing.T) {
	stream := realisticResponsesStream()
	for _, encoding := range []string{"", "gzip", "deflate", "br", "zstd"} {
		t.Run("encoding="+encoding, func(t *testing.T) {
			wire := encodeForTest(t, encoding, stream)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				w.Header().Set("Content-Type", "text/event-stream")
				if encoding != "" {
					w.Header().Set("Content-Encoding", encoding)
				}
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write(wire[:len(wire)/2])
				w.(http.Flusher).Flush()
				<-r.Context().Done()
			}))
			defer upstream.Close()
			upstreamURL, _ := url.Parse(upstream.URL)
			store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
			if err != nil {
				t.Fatal(err)
			}
			recorder := NewTokenUsageRecorder("", nil)
			subrouter := httptest.NewServer(Server{
				Upstream: upstreamURL,
				Accounts: []accounts.Account{{
					ID: "codex-a", AuthMode: accounts.AuthModeOAuth, Token: "token-a", AccountID: "acct-a",
				}},
				Sessions:     store,
				Scheduler:    selectacct.NewScheduler(nil),
				MaxBodyBytes: 1 << 20,
				TokenUsage:   recorder,
			}.Handler())
			defer subrouter.Close()

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			request, _ := http.NewRequestWithContext(ctx, http.MethodPost, subrouter.URL+"/v1/responses",
				strings.NewReader(`{"model":"gpt-6-astra","stream":true,"input":"hello"}`))
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("Accept-Encoding", "gzip, deflate, br, zstd")
			client := &http.Client{Transport: &http.Transport{DisableCompression: true}}
			response, err := client.Do(request)
			if err != nil {
				t.Fatal(err)
			}
			if response.StatusCode != http.StatusOK {
				got, _ := io.ReadAll(response.Body)
				t.Fatalf("status = %d, body %q", response.StatusCode, got)
			}
			if _, err := io.ReadFull(response.Body, make([]byte, len(wire)/4)); err != nil {
				t.Fatal(err)
			}
			cancel()
			_ = response.Body.Close()

			deadline := time.Now().Add(5 * time.Second)
			for {
				rows, err := recorder.Rows(time.Now().Add(-time.Hour))
				if err != nil {
					t.Fatal(err)
				}
				if len(rows) == 1 && rows[0].Requests == 1 {
					return
				}
				if time.Now().After(deadline) {
					t.Fatalf("rows = %+v, want the canceled turn counted", rows)
				}
				time.Sleep(10 * time.Millisecond)
			}
		})
	}
}

// realisticResponsesStream is an OpenAI Responses stream as chatgpt.com sends
// it to codex-tui: many output deltas, then response.completed with usage.
func realisticResponsesStream() string {
	var b strings.Builder
	b.WriteString("event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"model\":\"gpt-6-astra\"}}\n\n")
	for i := 0; i < 4000; i++ {
		fmt.Fprintf(&b, "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"token %d \"}\n\n", i)
	}
	b.WriteString("event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"model\":\"gpt-6-astra\",\"usage\":{\"input_tokens\":40,\"output_tokens\":6}}}\n\n")
	return b.String()
}
