package proxy

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCacheStatsClassifiesPartialAndFullHits(t *testing.T) {
	stats := NewCacheStats(filepath.Join(t.TempDir(), "cache-stats.json"))
	stats.ObserveUsage("gpt-6-astra", 100, 0)
	stats.ObserveUsage("gpt-6-astra", 100, 40)
	stats.ObserveUsage("gpt-6-astra", 100, 100)
	stats.ObserveColdAccountMove()

	got := stats.Snapshot()
	if got.Responses != 3 || got.MissResponses != 1 || got.PartialHitResponses != 1 || got.FullHitResponses != 1 {
		t.Fatalf("classification = %+v", got)
	}
	if got.InputTokens != 300 || got.CachedInputTokens != 140 || got.UncachedInputTokens != 160 || got.ColdAccountMoves != 1 {
		t.Fatalf("token counters = %+v", got)
	}
	if got.RequestHitRate != 2.0/3.0 || got.TokenHitRate != 140.0/300.0 {
		t.Fatalf("ratios = %+v", got)
	}
	if len(got.ByModel) != 1 || got.ByModel[0].Model != "gpt-6-astra" {
		t.Fatalf("model counters = %+v", got.ByModel)
	}
}

func TestCacheUsageObserverReadsJSONAndSSE(t *testing.T) {
	stats := NewCacheStats("")
	observer := newCacheUsageObserver(stats, "gpt-6-astra")
	observer.ObserveChunk([]byte("event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"model\":\"gpt-6-astra\",\"usage\":{\"input_tokens\":100,\"input_tokens_details\":{\"cached_tokens\":75}}}}\n"))
	observer.Finish()
	got := stats.Snapshot()
	if got.Responses != 1 || got.CachedInputTokens != 75 || got.PartialHitResponses != 1 {
		t.Fatalf("SSE counters = %+v", got)
	}

	observer = newCacheUsageObserver(stats, "fallback")
	observer.ObserveMessage([]byte(`{"model":"fallback","usage":{"input_tokens":20,"input_tokens_details":{"cached_tokens":20}}}`))
	if got := stats.Snapshot(); got.Responses != 2 || got.FullHitResponses != 1 {
		t.Fatalf("JSON counters = %+v", got)
	}
}

func TestCacheStatsFlushMergesWorkerDeltas(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cache-stats.json")
	first := NewCacheStats(path)
	second := NewCacheStats(path)
	first.ObserveUsage("a", 100, 100)
	second.ObserveUsage("b", 200, 0)
	if err := first.Flush(); err != nil {
		t.Fatal(err)
	}
	if err := second.Flush(); err != nil {
		t.Fatal(err)
	}

	file, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var got cacheStatsFile
	if err := json.Unmarshal(file, &got); err != nil {
		t.Fatal(err)
	}
	if got.Totals.Responses != 2 || got.Totals.CachedInputTokens != 100 || got.Totals.MissResponses != 1 {
		t.Fatalf("merged file = %+v", got)
	}
}

func TestCacheStatsEndpointDoesNotRequireAdminToken(t *testing.T) {
	stats := NewCacheStats("")
	stats.ObserveUsage("gpt-6-astra", 10, 10)
	handler := Server{CacheStats: stats}.Handler()
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/_subrouter/cache-stats", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d", response.Code)
	}
	var got CacheStatsSnapshot
	if err := json.Unmarshal(response.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Responses != 1 || got.TokenHitRate != 1 {
		t.Fatalf("body = %+v", got)
	}
}

func TestCacheStatsStartFlushesOnContextCancel(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cache-stats.json")
	stats := NewCacheStats(path)
	ctx, cancel := context.WithCancel(context.Background())
	stats.Start(ctx)
	stats.ObserveUsage("gpt-6-astra", 10, 5)
	cancel()
	for i := 0; i < 20; i++ {
		if _, err := os.Stat(path); err == nil {
			return
		}
		// The goroutine has no user-visible readiness channel; this short poll
		// keeps the test deterministic without adding one to the hot path.
		select {
		case <-ctx.Done():
		default:
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("cache stats were not flushed: %v", err)
	}
}
