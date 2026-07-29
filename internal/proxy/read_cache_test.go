package proxy

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestReadCacheHitSkipsUpstream(t *testing.T) {
	upstreamHits := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHits++
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	cache := newReadCache()
	cache.set(
		httptest.NewRequest(http.MethodGet, "/backend-api/ps/plugins/installed", nil),
		http.StatusOK,
		http.Header{"Content-Type": []string{"application/json"}},
		[]byte(`{"cached":true}`),
		60*time.Second,
	)

	s := Server{ReadCache: cache}
	handler := s.Handler()

	req := httptest.NewRequest(http.MethodGet, "/backend-api/ps/plugins/installed", nil)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if upstreamHits != 0 {
		t.Fatalf("expected 0 upstream hits on cache hit, got %d", upstreamHits)
	}
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "cached") {
		t.Fatalf("body = %q, want cached payload", rr.Body.String())
	}
	if rr.Header().Get("X-Subrouter-Cache") != "HIT" {
		t.Fatal("missing X-Subrouter-Cache: HIT header")
	}
}

func TestCacheablePathPatterns(t *testing.T) {
	cases := []struct {
		path    string
		wantTTL bool
	}{
		{"/backend-api/ps/plugins/installed", true},
		{"/backend-api/plugins/installed", true},
		{"/backend-api/plugins/featured", true},
		{"/backend-api/ps/plugins/search", true},
		// Non-cacheable
		{"/backend-api/codex/responses", false},
		{"/v1/messages", false},
		{"/backend-api/conversation", false},
		{"/v1/responses", false},
	}
	for _, tc := range cases {
		ttl := cacheablePath(tc.path)
		got := ttl > 0
		if got != tc.wantTTL {
			t.Errorf("cacheablePath(%q) cacheable=%v, want %v", tc.path, got, tc.wantTTL)
		}
	}
}

func TestReadCacheExpiry(t *testing.T) {
	cache := newReadCache()
	req := httptest.NewRequest(http.MethodGet, "/backend-api/ps/plugins/installed", nil)
	cache.set(req, 200, nil, []byte("body"), 1*time.Millisecond)
	time.Sleep(5 * time.Millisecond)
	_, ok := cache.get(req)
	if ok {
		t.Fatal("expected cache miss after TTL expiry")
	}
}
