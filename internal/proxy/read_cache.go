package proxy

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
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

// cacheKey identifies everything the cached response depends on: the method,
// the path, every query parameter, and the calling identity. Anything left out
// here is a request that gets served somebody else's body.
func cacheKey(r *http.Request) string {
	var b strings.Builder
	b.WriteString(r.Method)
	b.WriteByte('\n')
	b.WriteString(r.URL.Path)
	b.WriteByte('\n')
	// Encode() sorts by key, so parameter order does not fragment the cache.
	b.WriteString(r.URL.Query().Encode())
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
	// session, so hashing the bearer gives every new session its own cache and
	// makes concurrent sessions each pay for the same upstream work.
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

func (c *readCache) len() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.entries)
}

func (c *readCache) get(r *http.Request) (*cacheEntry, bool) {
	key := cacheKey(r)
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok || time.Now().After(e.expiresAt) {
		return nil, false
	}
	// Loop guard: a caller repeating one request this many times inside a
	// single TTL window is not polling, it is stuck. Serving the cached body
	// again is what keeps it stuck, so drop the entry and send it upstream.
	e.hits++
	if e.hits > readCacheLoopThreshold {
		delete(c.entries, key)
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
	defer c.mu.Unlock()
	if len(c.entries) >= readCacheMaxEntries {
		c.evictLocked()
	}
	c.entries[key] = e
}

// evictLocked drops expired entries first, then the soonest-to-expire entries
// until the map is back under the cap. Callers hold c.mu.
func (c *readCache) evictLocked() {
	now := time.Now()
	for k, e := range c.entries {
		if now.After(e.expiresAt) {
			delete(c.entries, k)
		}
	}
	for len(c.entries) >= readCacheMaxEntries {
		var oldestKey string
		var oldest time.Time
		for k, e := range c.entries {
			if oldestKey == "" || e.expiresAt.Before(oldest) {
				oldestKey, oldest = k, e.expiresAt
			}
		}
		if oldestKey == "" {
			return
		}
		delete(c.entries, oldestKey)
	}
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
	case strings.HasPrefix(p, "/ps/plugins/list"):
		// The catalog is large, changes slowly, and each miss costs a full
		// multi-page walk upstream. A short TTL here is what turns a handful of
		// sessions starting together into a burst of upstream traffic.
		return 10 * time.Minute
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
