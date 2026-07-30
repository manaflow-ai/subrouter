package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestStreamDropStatsCountsByAttribution(t *testing.T) {
	stats := &StreamDropStats{}
	now := time.Unix(1770000000, 0)

	for range 940 {
		stats.Observe("client", now)
	}
	stats.Observe("proxy", now.Add(time.Minute))
	stats.Observe("upstream", now)
	stats.Observe("", now)

	got := stats.Snapshot()
	if got.Client != 940 || got.Proxy != 1 || got.Upstream != 1 || got.Unknown != 1 {
		t.Fatalf("unexpected counts: %+v", got)
	}
	if got.Total != 943 {
		t.Fatalf("Total = %d, want 943", got.Total)
	}
	if got.Since != now.UTC().Format(time.RFC3339) {
		t.Fatalf("Since = %q, want first observation time", got.Since)
	}
	if got.LastProxy != now.Add(time.Minute).UTC().Format(time.RFC3339) {
		t.Fatalf("LastProxy = %q, want the proxy drop time", got.LastProxy)
	}
}

func TestStreamDropStatsNilSafe(t *testing.T) {
	var stats *StreamDropStats
	stats.Observe("client", time.Now()) // must not panic
	if got := stats.Snapshot(); got.Total != 0 {
		t.Fatalf("nil snapshot Total = %d, want 0", got.Total)
	}
}

func TestHandleStreamStatsServesSnapshot(t *testing.T) {
	stats := &StreamDropStats{}
	stats.Observe("proxy", time.Unix(1770000000, 0))
	server := Server{StreamDrops: stats}

	recorder := httptest.NewRecorder()
	server.handleStreamStats(recorder, httptest.NewRequest(http.MethodGet, "/_subrouter/stream-stats", nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	var got StreamDropSnapshot
	if err := json.Unmarshal(recorder.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Proxy != 1 || got.Total != 1 {
		t.Fatalf("unexpected snapshot: %+v", got)
	}
}
