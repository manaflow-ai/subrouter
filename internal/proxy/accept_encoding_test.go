package proxy

import (
	"bufio"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/accounts"
	"github.com/manaflow-ai/subrouter/selectacct"
	"github.com/manaflow-ai/subrouter/session"
)

// Claude Code sends this Accept-Encoding; api.anthropic.com answers br to it.
const claudeCodeAcceptEncoding = "gzip, deflate, br, zstd"

// gzipSSEWriter writes SSE events gzip-compressed, flushing the compressor and
// the connection after each one, the way a compressing CDN streams.
type gzipSSEWriter struct {
	gz      *gzip.Writer
	flusher http.Flusher
}

func startGzipSSE(t *testing.T, w http.ResponseWriter, r *http.Request) *gzipSSEWriter {
	t.Helper()
	// A real upstream only compresses when asked; subrouter must now leave
	// the asking to Go's transport, which offers exactly gzip.
	if got := r.Header.Get("Accept-Encoding"); got != "gzip" {
		t.Errorf("upstream Accept-Encoding = %q, want the transport's own gzip", got)
	}
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Content-Encoding", "gzip")
	w.WriteHeader(http.StatusOK)
	return &gzipSSEWriter{gz: gzip.NewWriter(w), flusher: w.(http.Flusher)}
}

func (g *gzipSSEWriter) event(body string) {
	_, _ = io.WriteString(g.gz, body)
	_ = g.gz.Flush()
	g.flusher.Flush()
}

func (g *gzipSSEWriter) close() {
	_ = g.gz.Close()
	g.flusher.Flush()
}

func claudeAcceptEncodingServer(t *testing.T, upstream *httptest.Server, recorder *TokenUsageRecorder) *httptest.Server {
	t.Helper()
	upstreamURL, _ := url.Parse(upstream.URL)
	store, err := session.NewStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	subrouter := httptest.NewServer(Server{
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
	}.Handler())
	t.Cleanup(subrouter.Close)
	return subrouter
}

// claudeCodeRequest is a streamed /v1/messages POST as Claude Code sends it.
// The client transport does not add or strip encodings, so the test sees the
// exact bytes and headers subrouter returns.
func claudeCodeRequest(t *testing.T, ctx context.Context, subrouterURL string) *http.Response {
	t.Helper()
	request, _ := http.NewRequestWithContext(ctx, http.MethodPost, subrouterURL+"/v1/messages?beta=true",
		strings.NewReader(`{"model":"claude-opus-5-5","max_tokens":32000,"stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept-Encoding", claudeCodeAcceptEncoding)
	request.Header.Set("X-Subrouter-Agent", "claude")
	request.Header.Set("X-Subrouter-Session", "session-accept-encoding")
	client := &http.Client{Transport: &http.Transport{DisableCompression: true}}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = response.Body.Close() })
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("status = %d, body %q", response.StatusCode, body)
	}
	if got := response.Header.Get("Content-Encoding"); got != "" {
		t.Fatalf("client Content-Encoding = %q, want an identity body", got)
	}
	return response
}

// readSSEEvent reads one blank-line-terminated SSE event.
func readSSEEvent(t *testing.T, reader *bufio.Reader) string {
	t.Helper()
	var event strings.Builder
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("read event: %v (partial %q)", err, event.String())
		}
		event.WriteString(line)
		if line == "\n" {
			return event.String()
		}
	}
}

// The first byte of a compressed Claude stream must not wait for the overload
// peek's timeout, and each gzip-flushed event must reach the client before
// upstream sends the next one. On main-19bb33b a streamed reply took 3.7s to
// first byte with Claude Code's Accept-Encoding and 0.95s with identity.
func TestProxyStreamsCompressedClaudeSSEEventByEvent(t *testing.T) {
	// Long enough that a peek which cannot parse the stream fails this test
	// by its context deadline instead of passing after the timeout.
	previous := claudeSSEOverloadPeekTimeout
	claudeSSEOverloadPeekTimeout = 30 * time.Second
	t.Cleanup(func() { claudeSSEOverloadPeekTimeout = previous })

	deltas := []string{"one", "two", "three"}
	next := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		sse := startGzipSSE(t, w, r)
		sse.event(claudeSSEMessageStart)
		sse.event("event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n")
		for _, delta := range deltas {
			select {
			case <-next:
			case <-r.Context().Done():
				return
			}
			sse.event(fmt.Sprintf("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":%q}}\n\n", delta))
		}
		sse.event(claudeSSETail)
		sse.close()
	}))
	defer upstream.Close()
	subrouter := claudeAcceptEncodingServer(t, upstream, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	start := time.Now()
	response := claudeCodeRequest(t, ctx, subrouter.URL)
	reader := bufio.NewReader(response.Body)
	if first := readSSEEvent(t, reader); !strings.Contains(first, "message_start") {
		t.Fatalf("first event = %q", first)
	}
	if ttfb := time.Since(start); ttfb > 2*time.Second {
		t.Fatalf("time to first event %v, want it unheld by the overload peek", ttfb)
	}
	readSSEEvent(t, reader) // ping
	if got := readSSEEvent(t, reader); !strings.Contains(got, "content_block_start") {
		t.Fatalf("event = %q", got)
	}
	// Upstream sends each delta only after the client has the previous
	// event, so a proxy that buffers would deadlock here until the deadline.
	for _, delta := range deltas {
		next <- struct{}{}
		if got := readSSEEvent(t, reader); !strings.Contains(got, `"text":"`+delta+`"`) {
			t.Fatalf("event = %q, want delta %q", got, delta)
		}
	}
	rest, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if string(rest) != claudeSSETail {
		t.Fatalf("tail = %q", rest)
	}
}

// Anthropic can answer 200 and then send overloaded_error before any content.
// With a compressed stream the peek used to see gzip bytes, miss the error,
// and hand the client a dead stream instead of retrying.
func TestProxyRetriesCompressedClaudeStreamOverload(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		sse := startGzipSSE(t, w, r)
		if calls.Add(1) == 1 {
			sse.event(claudeSSEMessageStart)
			sse.event(claudeSSEOverloadedEvent)
		} else {
			sse.event(claudeSSEMessageStart + claudeSSEContent + claudeSSETail)
		}
		sse.close()
	}))
	defer upstream.Close()
	subrouter := claudeAcceptEncodingServer(t, upstream, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	response := claudeCodeRequest(t, ctx, subrouter.URL)
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if want := claudeSSEMessageStart + claudeSSEContent + claudeSSETail; string(body) != want {
		t.Fatalf("client body = %q, want the retried stream", body)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("upstream calls = %d, want the overload retried once", got)
	}
}

// Token usage is read from a compressed Claude stream when the client asks for
// Claude Code's encodings.
func TestProxyRecordsTokenUsageWithClaudeCodeAcceptEncoding(t *testing.T) {
	const stream = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"model\":\"claude-opus-5-5\",\"id\":\"msg_01\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"usage\":{\"input_tokens\":12,\"cache_creation_input_tokens\":3000,\"cache_read_input_tokens\":90000,\"output_tokens\":1}}}\n\n" +
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hello\"}}\n\n" +
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":4321}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		sse := startGzipSSE(t, w, r)
		for _, event := range strings.SplitAfter(stream, "\n\n") {
			if event != "" {
				sse.event(event)
			}
		}
		sse.close()
	}))
	defer upstream.Close()
	recorder := NewTokenUsageRecorder("", nil)
	subrouter := claudeAcceptEncodingServer(t, upstream, recorder)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	response := claudeCodeRequest(t, ctx, subrouter.URL)
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != stream {
		t.Fatalf("client body = %q", body)
	}
	_ = response.Body.Close()

	var rows []TokenUsageRow
	deadline := time.Now().Add(5 * time.Second)
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
	if row.Model != "claude-opus-5-5" || row.Requests != 1 || row.RequestsWithoutUsage != 0 ||
		row.InputTokens != 12+3000+90000 || row.CachedInputTokens != 90000 || row.CacheWriteInputTokens != 3000 ||
		row.OutputTokens != 4321 {
		t.Fatalf("row = %+v", row)
	}
}
