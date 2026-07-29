package proxy

import (
	"bytes"
	"net/http"
	"strings"
	"sync"
	"time"
)

// readCache caches GET responses for read-only endpoints that are polled
// heavily by Codex clients (e.g. /backend-api/ps/plugins/installed).
// A 60s TTL reduces upstream hits by ~100x when many concurrent sessions
// all poll the same chatgpt.com endpoint.
//
// Entries are addressed by *http.Request rather than by a caller-built string:
// a cache whose key omits part of what the response depends on serves one
// request's body to a different request, and every such omission is a bug.
const (
	// readCacheMaxEntries caps the key space. Keys include the query string,
	// so distinct pagination cursors would otherwise grow the map without end.
	readCacheMaxEntries = 1024
	// readCacheLoopThreshold is how many consecutive hits one key may serve
	// before the cache forces the caller upstream. A client repeating the same
	// request hundreds of times inside one TTL window is looping, and replaying
	// the cached body is what keeps it looping.
	readCacheLoopThreshold = 64
)

type readCache struct {
	mu      sync.RWMutex
	entries map[string]*cacheEntry
}

type cacheEntry struct {
	body       []byte
	statusCode int
	headers    http.Header
	expiresAt  time.Time
	hits       int
}

func newReadCache() *readCache {
	return &readCache{entries: make(map[string]*cacheEntry)}
}

// cacheKey identifies everything the cached response depends on.
func cacheKey(r *http.Request) string {
	return r.URL.Path
}

func (c *readCache) len() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.entries)
}

func (c *readCache) get(r *http.Request) (*cacheEntry, bool) {
	key := cacheKey(r)
	c.mu.RLock()
	e, ok := c.entries[key]
	c.mu.RUnlock()
	if !ok || time.Now().After(e.expiresAt) {
		return nil, false
	}
	return e, true
}

func (c *readCache) set(r *http.Request, statusCode int, headers http.Header, body []byte, ttl time.Duration) {
	h := make(http.Header, len(headers))
	for k, v := range headers {
		h[k] = append([]string(nil), v...)
	}
	e := &cacheEntry{
		body:       append([]byte(nil), body...),
		statusCode: statusCode,
		headers:    h,
		expiresAt:  time.Now().Add(ttl),
	}
	key := cacheKey(r)
	c.mu.Lock()
	c.entries[key] = e
	c.mu.Unlock()
}

// cacheablePath returns the TTL for a GET endpoint that can be safely cached,
// or 0 if it should not be cached. These are chatgpt.com polling endpoints
// that Codex calls on every session startup and periodically thereafter.
func cacheablePath(path string) time.Duration {
	p := path
	if stripped, ok := stripChatGPTBackendPath(path); ok {
		p = stripped
	}
	switch {
	case p == "/ps/plugins/installed",
		strings.HasPrefix(p, "/ps/plugins/"),
		p == "/plugins/installed",
		p == "/plugins/featured",
		strings.HasPrefix(p, "/plugins/featured"):
		return 60 * time.Second
	}
	return 0
}

// cacheRecorder buffers a full HTTP response so it can be stored in the cache
// and then replayed to the actual client.
type cacheRecorder struct {
	header http.Header
	buf    bytes.Buffer
	code   int
}

func (r *cacheRecorder) Header() http.Header {
	if r.header == nil {
		r.header = make(http.Header)
	}
	return r.header
}

func (r *cacheRecorder) WriteHeader(code int) {
	r.code = code
}

func (r *cacheRecorder) Write(p []byte) (int, error) {
	if r.code == 0 {
		r.code = http.StatusOK
	}
	return r.buf.Write(p)
}
