package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"sync/atomic"
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

func TestGoldenProbeLoopDoesNotOverlapSlowSamplesForSameTarget(t *testing.T) {
	firstStarted := make(chan time.Time, 1)
	secondStarted := make(chan time.Time, 1)
	releaseFirst := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseFirst) }) }
	var readyRequests atomic.Int64
	publicServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/_subrouter/ready" {
			w.WriteHeader(http.StatusOK)
			return
		}
		switch readyRequests.Add(1) {
		case 1:
			firstStarted <- time.Now()
			select {
			case <-releaseFirst:
			case <-request.Context().Done():
				return
			}
		case 2:
			secondStarted <- time.Now()
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(publicServer.Close)
	localServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(localServer.Close)

	publicOrigin, err := url.Parse(publicServer.URL)
	if err != nil {
		t.Fatal(err)
	}
	localOrigin, err := url.Parse(localServer.URL)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	runner := &goldenRunner{artifactDir: t.TempDir()}
	stats, err := runner.startProbes(ctx, publicOrigin, localOrigin)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		release()
		cancel()
		stats.wait()
	})

	var first time.Time
	select {
	case first = <-firstStarted:
	case <-time.After(time.Second):
		t.Fatal("first ready probe did not start")
	}
	select {
	case second := <-secondStarted:
		t.Fatalf("second ready probe overlapped the blocked first probe after %s", second.Sub(first))
	case <-time.After(3 * goldenProbeInterval):
	}
	release()
	select {
	case second := <-secondStarted:
		if gap := second.Sub(first); gap < 3*goldenProbeInterval {
			t.Fatalf("slow probe start gap = %s, want at least %s", gap, 3*goldenProbeInterval)
		}
	case <-time.After(time.Second):
		t.Fatal("second ready probe did not start after the first completed")
	}
}
