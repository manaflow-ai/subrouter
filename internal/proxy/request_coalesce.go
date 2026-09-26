package proxy

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"
)

// Subrouter holds no response bodies between requests. Read-heavy polling
// endpoints get exactly one mitigation: identical concurrent requests share a
// single upstream fetch (see singleFlight). A stored-response cache used to
// live here; its key once omitted the query string, which replayed page 1 of
// the plugin catalog to a page-2 request and fed codex an infinite pagination
// loop (3,598 requests, 15 GB client RSS, kernel panics). Coalescing cannot
// serve a stale body because it stores nothing: a wrong-body bug now requires
// two different requests to collide on flightKey at the same instant, which
// the key-completeness test below makes structurally hard to reintroduce.

// coalescablePath reports whether a GET endpoint's responses should be
// buffered and shared across identical concurrent requests. These are the
// chatgpt.com polling endpoints codex hits on every session startup; the
// catalog list additionally needs buffering so aggregateCatalogPages can walk
// its pagination server-side.
func coalescablePath(path string) bool {
	p := path
	if stripped, ok := stripChatGPTBackendPath(path); ok {
		p = stripped
	}
	switch {
	case strings.HasPrefix(p, "/ps/plugins/"),
		p == "/plugins/installed",
		strings.HasPrefix(p, "/plugins/featured"):
		return true
	}
	return false
}

// flightKey identifies everything a shared response depends on: the method,
// the path, every query parameter, the calling identity, and the response
// encoding the caller can accept. Anything left out here is a request that
// gets served somebody else's body.
func flightKey(r *http.Request) string {
	var b strings.Builder
	b.WriteString(r.Method)
	b.WriteByte('\n')
	b.WriteString(r.URL.Path)
	b.WriteByte('\n')
	// Encode() sorts by key, so parameter order does not fragment the key space.
	b.WriteString(r.URL.Query().Encode())
	b.WriteByte('\n')
	// The reverse proxy forwards Accept-Encoding upstream, so the buffered
	// body carries whatever Content-Encoding the leader negotiated. A gzip
	// body handed to a waiter that never offered gzip is garbage bytes.
	b.WriteString(r.Header.Get("Accept-Encoding"))
	b.WriteByte('\n')
	b.WriteString(callerIdentity(r))
	return b.String()
}

// callerIdentity fingerprints whose data this response is. Hashed so that
// bearer tokens never sit in a map key, and truncated because collision
// resistance beyond 128 bits buys nothing here.
func callerIdentity(r *http.Request) string {
	h := sha256.New()
	// Identity is the account, not the credential. Access tokens rotate per
	// session, so hashing the bearer would give every session its own key and
	// make concurrent sessions each pay for the same upstream work.
	account := r.Header.Get("chatgpt-account-id")
	user := r.Header.Get("chatgpt-user-id")
	if account != "" || user != "" {
		h.Write([]byte("account\x00"))
		h.Write([]byte(account))
		h.Write([]byte{0})
		h.Write([]byte(user))
		return hex.EncodeToString(h.Sum(nil)[:16])
	}
	// With no account headers the credential is all we have to separate
	// callers by, and separating them is what matters.
	h.Write([]byte("bearer\x00"))
	h.Write([]byte(r.Header.Get("Authorization")))
	return hex.EncodeToString(h.Sum(nil)[:16])
}

// responseRecorder buffers a full HTTP response so it can be shared across a
// flight and replayed to each waiting client.
type responseRecorder struct {
	header http.Header
	buf    bytes.Buffer
	code   int
}

func (r *responseRecorder) Header() http.Header {
	if r.header == nil {
		r.header = make(http.Header)
	}
	return r.header
}

func (r *responseRecorder) WriteHeader(code int) {
	r.code = code
}

func (r *responseRecorder) Write(p []byte) (int, error) {
	if r.code == 0 {
		r.code = http.StatusOK
	}
	return r.buf.Write(p)
}
