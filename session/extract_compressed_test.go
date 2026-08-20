package session

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/klauspost/compress/zstd"
)

// codexResponsesBody mirrors the shape codex-cli sends to /v1/responses: the
// model first, the session id nested in client_metadata at the end.
const codexResponsesBody = `{"model":"gpt-5.3-codex","instructions":"be helpful","input":[],` +
	`"prompt_cache_key":"01a01336-3c47-7791-8883-ca794f5bd7c1",` +
	`"client_metadata":{"session_id":"01a01336-3c47-7791-8883-ca794f5bd7c1",` +
	`"thread_id":"01a01336-3c47-7791-8883-ca794f5bd7c1"}}`

func zstdBytes(t *testing.T, body string) []byte {
	t.Helper()
	var buf bytes.Buffer
	encoder, err := zstd.NewWriter(&buf)
	if err != nil {
		t.Fatalf("new zstd writer: %v", err)
	}
	if _, err := encoder.Write([]byte(body)); err != nil {
		t.Fatalf("write zstd body: %v", err)
	}
	if err := encoder.Close(); err != nil {
		t.Fatalf("close zstd writer: %v", err)
	}
	return buf.Bytes()
}

func zstdCodexRequest(t *testing.T) *http.Request {
	t.Helper()
	wire := zstdBytes(t, codexResponsesBody)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(wire))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Content-Encoding", "zstd")
	req.ContentLength = int64(len(wire))
	return req
}

func TestExtractIDReadsZstdCodexBody(t *testing.T) {
	req := zstdCodexRequest(t)
	if got := ExtractID(req, 1<<20); got != "01a01336-3c47-7791-8883-ca794f5bd7c1" {
		t.Fatalf("ExtractID = %q, want the codex session id", got)
	}
}

func TestExtractModelReadsZstdCodexBody(t *testing.T) {
	req := zstdCodexRequest(t)
	if got := ExtractModel(req, 1<<20); got != "gpt-5.3-codex" {
		t.Fatalf("ExtractModel = %q, want gpt-5.3-codex", got)
	}
}

func TestExtractLeavesCompressedBodyIntactForUpstream(t *testing.T) {
	wire := zstdBytes(t, codexResponsesBody)
	req := zstdCodexRequest(t)
	_ = ExtractID(req, 1<<20)
	_ = ExtractModel(req, 1<<20)
	forwarded, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatalf("read forwarded body: %v", err)
	}
	if !bytes.Equal(forwarded, wire) {
		t.Fatalf("forwarded body changed: got %d bytes, want %d", len(forwarded), len(wire))
	}
}

func TestExtractIDUsesBareSessionIDHeader(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(nil))
	req.Header.Set("session-id", "01a01336-3c47-7791-8883-ca794f5bd7c1")
	if got := ExtractID(req, 1<<20); got != "01a01336-3c47-7791-8883-ca794f5bd7c1" {
		t.Fatalf("ExtractID = %q, want the session-id header value", got)
	}
}

func TestExtractIDPrefersCodexWindowHeaderOverBareSessionID(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(nil))
	req.Header.Set("X-Codex-Window-ID", "01a01336-3c47-7791-8883-ca794f5bd7c1:0")
	req.Header.Set("session-id", "01a01336-3c47-7791-8883-ca794f5bd7c1")
	// The window header still wins as the source, but its window index is not
	// part of the identity: the same thread in a later window is the same
	// conversation, and treating it as a new session resets both the account it
	// is sticky to and the provider it was pinned to.
	if got := ExtractID(req, 1<<20); got != "01a01336-3c47-7791-8883-ca794f5bd7c1" {
		t.Fatalf("ExtractID = %q, want the canonical thread id", got)
	}
}
