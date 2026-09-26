package proxy

import (
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
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
