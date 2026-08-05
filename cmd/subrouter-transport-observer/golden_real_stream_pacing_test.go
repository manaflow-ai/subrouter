package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
)

func TestGoldenObserverHoldsRealResponseUntilGateRelease(t *testing.T) {
	previousHooks := goldenTestHooks
	goldenTestHooks.enabled = false
	t.Cleanup(func() { goldenTestHooks = previousHooks })

	payload := bytes.Repeat([]byte("continuity-payload\n"), 32<<10)
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = writer.Write(payload)
	}))
	t.Cleanup(upstream.Close)
	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}

	runner := &goldenRunner{artifactDir: t.TempDir()}
	observation, err := runner.startObserver("real-stream-pacing", upstreamURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := observation.stop(ctx); err != nil {
			t.Errorf("stop observer: %v", err)
		}
	})

	request, err := http.NewRequest(http.MethodPost, observation.baseURL+"/v1/responses", bytes.NewReader([]byte("{}")))
	if err != nil {
		t.Fatal(err)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	readDone := make(chan error, 1)
	go func() {
		_, readErr := io.Copy(io.Discard, response.Body)
		closeErr := response.Body.Close()
		if readErr != nil {
			readDone <- readErr
			return
		}
		readDone <- closeErr
	}()

	select {
	case err := <-readDone:
		t.Fatalf("real response completed before gate release: %v", err)
	case <-time.After(250 * time.Millisecond):
	}

	if err := releaseGoldenTestSessions([]*goldenSession{{observer: observation}}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-readDone:
		if err != nil {
			t.Fatalf("released response failed: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("released response did not drain")
	}
}
