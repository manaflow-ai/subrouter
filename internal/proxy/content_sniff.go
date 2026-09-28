package proxy

import (
	"bufio"
	"bytes"
	"net/http"
	"strings"
)

// chatgpt.com's Codex Responses endpoint streams SSE with no Content-Type at
// all (only Cf-*, X-Codex-*, X-Oai-Request-Id, Transfer-Encoding: chunked).
// Everything that recognized a stream by its Content-Type then treated a
// Codex turn as an opaque body: token usage read it as JSON and found
// nothing, the capacity and stream-failure peek (codexEventStream) skipped
// it, and the session commit waited for EOF instead of the terminal event.

type sniffedBodyKind int

const (
	sniffedUnknown sniffedBodyKind = iota
	sniffedSSE
	sniffedJSON
)

// sseLinePrefixes are the field names an SSE stream can open with (a comment
// is a line starting with ':').
var sseLinePrefixes = [][]byte{[]byte("event:"), []byte("data:"), []byte("id:"), []byte("retry:"), []byte(":")}

// sniffBodyKind classifies a body from its first non-whitespace bytes. It
// reports decided=false while prefix is too short to tell (all whitespace,
// or a partial SSE field name).
func sniffBodyKind(prefix []byte) (kind sniffedBodyKind, decided bool) {
	rest := bytes.TrimLeft(prefix, " \t\r\n")
	if len(rest) == 0 {
		return sniffedUnknown, false
	}
	switch rest[0] {
	case '{', '[':
		return sniffedJSON, true
	}
	partial := false
	for _, field := range sseLinePrefixes {
		if bytes.HasPrefix(rest, field) {
			return sniffedSSE, true
		}
		if len(rest) < len(field) && bytes.HasPrefix(field, rest) {
			partial = true
		}
	}
	return sniffedUnknown, !partial
}

// contentTypeNeedsSniff reports whether a response's Content-Type leaves its
// body kind open: absent, or neither JSON nor an event stream.
func contentTypeNeedsSniff(contentType string) bool {
	contentType = strings.ToLower(contentType)
	return !strings.Contains(contentType, "json") && !strings.Contains(contentType, "text/event-stream")
}

// sniffContentTypeTransport labels an upstream response that arrives with no
// Content-Type, from its first body bytes, so every layer above it (stream
// peeks, session commit, token usage, the client) sees what it is. It only
// waits for the first bytes the client would have had to wait for anyway,
// and leaves the body byte for byte as upstream sent it.
type sniffContentTypeTransport struct {
	base http.RoundTripper
}

// sniffContentTypeMaxBytes bounds how far a body is read looking for its
// first non-whitespace bytes.
const sniffContentTypeMaxBytes = 64

func (t sniffContentTypeTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	response, err := base.RoundTrip(req)
	if err != nil || response == nil {
		return response, err
	}
	labelSniffedContentType(response)
	return response, nil
}

func labelSniffedContentType(response *http.Response) {
	if response.Header == nil || response.Header.Get("Content-Type") != "" ||
		response.Body == nil || response.Body == http.NoBody || response.ContentLength == 0 ||
		response.StatusCode == http.StatusNoContent || response.StatusCode == http.StatusNotModified ||
		(response.Request != nil && response.Request.Method == http.MethodHead) {
		return
	}
	body := response.Body
	reader := bufio.NewReaderSize(body, 512)
	kind := sniffedUnknown
	for reader.Buffered() < sniffContentTypeMaxBytes {
		// Peek never consumes; asking for one byte past what is buffered
		// waits for the next read from upstream.
		if _, err := reader.Peek(reader.Buffered() + 1); err != nil {
			buffered, _ := reader.Peek(reader.Buffered())
			kind, _ = sniffBodyKind(buffered)
			break
		}
		buffered, _ := reader.Peek(reader.Buffered())
		var decided bool
		if kind, decided = sniffBodyKind(buffered); decided {
			break
		}
	}
	response.Body = readCloser{Reader: reader, Closer: body}
	switch kind {
	case sniffedSSE:
		response.Header.Set("Content-Type", "text/event-stream")
	case sniffedJSON:
		response.Header.Set("Content-Type", "application/json")
	}
}
