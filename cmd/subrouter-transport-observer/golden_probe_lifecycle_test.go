package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestGoldenProbeOrderlyCancellationDoesNotRecordFailure(t *testing.T) {
	requestStarted := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
		close(requestStarted)
		<-request.Context().Done()
	}))
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	stats := &goldenProbeStats{record: &jsonlRecorder{writer: io.Discard}}
	stats.launchProbe(ctx, "public-ready", server.URL)
	select {
	case <-requestStarted:
	case <-time.After(time.Second):
		t.Fatal("probe request did not start")
	}
	cancel()
	stats.samples.Wait()

	stats.mu.Lock()
	events := append([]goldenProbeEvent(nil), stats.events...)
	stats.mu.Unlock()
	if len(events) != 0 {
		t.Fatalf("orderly cancellation recorded %d probe event(s), want 0", len(events))
	}
}

func TestGoldenProbeUncancelledRequestFailureRemainsEvidence(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	rawURL := server.URL
	server.Close()

	stats := &goldenProbeStats{record: &jsonlRecorder{writer: io.Discard}}
	stats.launchProbe(context.Background(), "public-ready", rawURL)
	stats.samples.Wait()

	stats.mu.Lock()
	events := append([]goldenProbeEvent(nil), stats.events...)
	stats.mu.Unlock()
	if len(events) != 1 || events[0].OK {
		t.Fatalf("uncancelled request failure events = %+v, want one failed event", events)
	}
}
