package proxy

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// A cached page must never be served to a request that differs in its query
// string. Codex paginates /ps/plugins/list with an opaque pageToken; replaying
// page 1 for a page-2 request hands the client back the same continuation
// cursor it just used, so it requests that page forever. Observed in the wild:
// 3,598 identical requests in one turn, 5.3 GB streamed to a single client,
// which drove the client to 15 GB RSS and panicked the host machine.
func TestReadCacheDoesNotServeAcrossQueryStrings(t *testing.T) {
	cache := newReadCache()
	page1 := httptest.NewRequest(http.MethodGet, "/backend-api/ps/plugins/list?scope=GLOBAL&limit=200", nil)
	cache.set(
		page1,
		http.StatusOK,
		http.Header{"Content-Type": []string{"application/json"}},
		[]byte(`{"page":1,"nextPageToken":"TOKEN_B"}`),
		60*time.Second,
	)

	page2 := httptest.NewRequest(http.MethodGet, "/backend-api/ps/plugins/list?scope=GLOBAL&limit=200&pageToken=TOKEN_B", nil)
	entry, ok := cache.get(page2)
	if ok {
		t.Fatal("page-2 request was served from the page-1 cache entry: infinite pagination loop")
	}
	if entry != nil && strings.Contains(string(entry.body), `"page":1`) {
		t.Fatalf("page-2 request got page-1 body: %q", entry.body)
	}
}

// Cached responses are per-account data (installed plugins, entitlements).
// Subrouter multiplexes many accounts, so a key that ignores caller identity
// serves one user's data to another.
func TestReadCacheDoesNotServeAcrossAccounts(t *testing.T) {
	cache := newReadCache()
	alice := httptest.NewRequest(http.MethodGet, "/backend-api/ps/plugins/installed?scope=USER", nil)
	alice.Header.Set("chatgpt-account-id", "acct-alice")
	alice.Header.Set("Authorization", "Bearer alice-token")
	cache.set(alice, http.StatusOK, nil, []byte(`{"owner":"alice"}`), 60*time.Second)

	bob := httptest.NewRequest(http.MethodGet, "/backend-api/ps/plugins/installed?scope=USER", nil)
	bob.Header.Set("chatgpt-account-id", "acct-bob")
	bob.Header.Set("Authorization", "Bearer bob-token")

	if _, ok := cache.get(bob); ok {
		t.Fatal("bob was served alice's cached response")
	}
}

// Identical requests must still hit, including when query parameters arrive in
// a different order, or the cache is useless.
func TestReadCacheHitsOnEquivalentRequests(t *testing.T) {
	cache := newReadCache()
	first := httptest.NewRequest(http.MethodGet, "/backend-api/ps/plugins/list?scope=GLOBAL&limit=200", nil)
	first.Header.Set("chatgpt-account-id", "acct-alice")
	cache.set(first, http.StatusOK, nil, []byte(`{"page":1}`), 60*time.Second)

	reordered := httptest.NewRequest(http.MethodGet, "/backend-api/ps/plugins/list?limit=200&scope=GLOBAL", nil)
	reordered.Header.Set("chatgpt-account-id", "acct-alice")
	if _, ok := cache.get(reordered); !ok {
		t.Fatal("equivalent request missed the cache")
	}
}

// Keying on the full request URI makes the key space unbounded (every distinct
// pageToken is a new key), so the map needs a ceiling.
func TestReadCacheBoundsEntryCount(t *testing.T) {
	cache := newReadCache()
	for i := 0; i < readCacheMaxEntries*3; i++ {
		r := httptest.NewRequest(http.MethodGet, "/backend-api/ps/plugins/list?pageToken=tok"+strings.Repeat("x", i%17)+string(rune('a'+i%26))+itoa(i), nil)
		cache.set(r, http.StatusOK, nil, []byte("body"), 60*time.Second)
	}
	if n := cache.len(); n > readCacheMaxEntries {
		t.Fatalf("cache holds %d entries, want <= %d", n, readCacheMaxEntries)
	}
}

// A client stuck in a request loop must not be fed from cache indefinitely.
// Tripping the detector forces upstream, which is what breaks the cycle.
func TestReadCacheLoopDetectorForcesUpstream(t *testing.T) {
	cache := newReadCache()
	r := httptest.NewRequest(http.MethodGet, "/backend-api/ps/plugins/list?scope=GLOBAL", nil)
	cache.set(r, http.StatusOK, nil, []byte(`{"page":1}`), 60*time.Second)

	served := 0
	for i := 0; i < readCacheLoopThreshold*2; i++ {
		if _, ok := cache.get(r); ok {
			served++
		}
	}
	if served > readCacheLoopThreshold {
		t.Fatalf("served %d consecutive cache hits for one key, want <= %d", served, readCacheLoopThreshold)
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}

// Key completeness. Every dimension a cached response can depend on must
// change the key. Add a dimension to the cache's contract, add it here, or the
// next omission repeats the pagination loop above.
func TestCacheKeyCoversEveryRequestDimension(t *testing.T) {
	base := func() *http.Request {
		r := httptest.NewRequest(http.MethodGet, "/backend-api/ps/plugins/list?scope=GLOBAL&limit=200", nil)
		r.Header.Set("chatgpt-account-id", "acct-alice")
		r.Header.Set("Authorization", "Bearer alice-token")
		r.Header.Set("chatgpt-user-id", "user-alice")
		return r
	}
	variants := map[string]func(*http.Request){
		"path": func(r *http.Request) { r.URL.Path = "/backend-api/ps/plugins/installed" },
		"query value": func(r *http.Request) {
			r.URL.RawQuery = "scope=GLOBAL&limit=200&pageToken=TOKEN_B"
		},
		"query scope": func(r *http.Request) { r.URL.RawQuery = "scope=WORKSPACE&limit=200" },
		"method":      func(r *http.Request) { r.Method = http.MethodPost },
		"account":     func(r *http.Request) { r.Header.Set("chatgpt-account-id", "acct-bob") },
		"bearer":      func(r *http.Request) { r.Header.Set("Authorization", "Bearer bob-token") },
		"user":        func(r *http.Request) { r.Header.Set("chatgpt-user-id", "user-bob") },
	}
	baseKey := cacheKey(base())
	for name, mutate := range variants {
		r := base()
		mutate(r)
		if cacheKey(r) == baseKey {
			t.Errorf("cacheKey ignores %s: two different requests share one cache entry", name)
		}
	}
}
