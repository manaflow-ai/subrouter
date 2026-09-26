package proxy

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/klauspost/compress/zstd"
)

func forwardingTestBody(sessionID string, padBytes int) []byte {
	return []byte(`{"model":"gpt-5.3-codex","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"` +
		strings.Repeat("x", padBytes) + `"}]}],"client_metadata":{"session_id":"` + sessionID + `"}}`)
}

func forwardingTestEncode(t *testing.T, encoding string, body []byte) []byte {
	t.Helper()
	switch encoding {
	case "":
		return body
	case "gzip":
		var buf bytes.Buffer
		writer := gzip.NewWriter(&buf)
		if _, err := writer.Write(body); err != nil {
			t.Fatal(err)
		}
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
		return buf.Bytes()
	case "zstd":
		encoder, err := zstd.NewWriter(nil)
		if err != nil {
			t.Fatal(err)
		}
		defer encoder.Close()
		return encoder.EncodeAll(body, nil)
	default:
		t.Fatalf("unknown encoding %q", encoding)
		return nil
	}
}

// TestProxyForwardsRequestBodyByteIdentical sends Codex requests through the
// whole handler, compressed and not, with a known length and chunked, below
// and above MaxBodyBytes, and checks that upstream receives exactly the bytes
// the client sent while the session id in the body still routes the request.
func TestProxyForwardsRequestBodyByteIdentical(t *testing.T) {
	var mu sync.Mutex
	var gotBody []byte
	var gotEncoding string
	pool := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("upstream read body: %v", err)
		}
		mu.Lock()
		gotBody = body
		gotEncoding = r.Header.Get("Content-Encoding")
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"resp_1"}`)
	}))
	defer pool.Close()
	poolURL, err := url.Parse(pool.URL)
	if err != nil {
		t.Fatal(err)
	}
	server := codexEgressServer(t, poolURL, nil, 1)
	proxy := httptest.NewServer(server.Handler())
	defer proxy.Close()

	sizes := map[string]int{"small": 1 << 10, "large": 3 << 20}
	for _, encoding := range []string{"", "gzip", "zstd"} {
		for sizeName, pad := range sizes {
			for _, chunked := range []bool{false, true} {
				label := encoding
				if label == "" {
					label = "identity"
				}
				name := fmt.Sprintf("%s/%s/chunked=%t", label, sizeName, chunked)
				t.Run(name, func(t *testing.T) {
					sessionID := "body-session-" + strings.ReplaceAll(name, "/", "-")
					wire := forwardingTestEncode(t, encoding, forwardingTestBody(sessionID, pad))
					var reader io.Reader = bytes.NewReader(wire)
					if chunked {
						// Hide the length so the client sends chunked encoding.
						reader = io.MultiReader(reader)
					}
					req, err := http.NewRequest(http.MethodPost, proxy.URL+"/responses", reader)
					if err != nil {
						t.Fatal(err)
					}
					req.Header.Set("Content-Type", "application/json")
					if encoding != "" {
						req.Header.Set("Content-Encoding", encoding)
					}
					response, err := http.DefaultClient.Do(req)
					if err != nil {
						t.Fatal(err)
					}
					responseBody, _ := io.ReadAll(response.Body)
					_ = response.Body.Close()
					if response.StatusCode != http.StatusOK {
						t.Fatalf("status = %d body = %s", response.StatusCode, responseBody)
					}
					mu.Lock()
					forwarded, forwardedEncoding := gotBody, gotEncoding
					mu.Unlock()
					if !bytes.Equal(forwarded, wire) {
						t.Fatalf("upstream body changed: got %d bytes, want %d", len(forwarded), len(wire))
					}
					if forwardedEncoding != encoding {
						t.Fatalf("upstream Content-Encoding = %q, want %q", forwardedEncoding, encoding)
					}
					// Bodies past MaxBodyBytes and chunked bodies keep their
					// session id too, so long conversations stay sticky.
					if _, ok := server.Sessions.Get("codex", sessionID); !ok {
						t.Fatalf("session %q from the body was not recorded", sessionID)
					}
				})
			}
		}
	}
}
