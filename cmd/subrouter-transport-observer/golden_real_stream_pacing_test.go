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

	payload := []byte("finite response that fits inside one paced transport chunk")
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
	case <-time.After(500 * time.Millisecond):
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

func TestGoldenObserverIsolatesConcurrentResponseHoldbacks(t *testing.T) {
	previousHooks := goldenTestHooks
	goldenTestHooks.enabled = false
	t.Cleanup(func() { goldenTestHooks = previousHooks })

	upstreamWritten := make(chan string, 2)
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		identifier := request.URL.Query().Get("id")
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = writer.Write([]byte(identifier))
		upstreamWritten <- identifier
	}))
	t.Cleanup(upstream.Close)
	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}

	runner := &goldenRunner{artifactDir: t.TempDir()}
	observation, err := runner.startObserver("concurrent-real-stream-pacing", upstreamURL)
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

	type responseResult struct {
		identifier string
		body       string
		err        error
	}
	results := make(chan responseResult, 2)
	for _, identifier := range []string{"first-response", "second-response"} {
		go func(identifier string) {
			response, err := http.Post(observation.baseURL+"/v1/responses?id="+identifier, "application/json", bytes.NewReader([]byte("{}")))
			if err != nil {
				results <- responseResult{identifier: identifier, err: err}
				return
			}
			body, readErr := io.ReadAll(response.Body)
			closeErr := response.Body.Close()
			if readErr == nil {
				readErr = closeErr
			}
			results <- responseResult{identifier: identifier, body: string(body), err: readErr}
		}(identifier)
	}
	for range 2 {
		select {
		case <-upstreamWritten:
		case <-time.After(5 * time.Second):
			t.Fatal("concurrent upstream response was not written")
		}
	}
	time.Sleep(100 * time.Millisecond)
	if err := releaseGoldenTestSessions([]*goldenSession{{observer: observation}}); err != nil {
		t.Fatal(err)
	}

	for range 2 {
		select {
		case result := <-results:
			if result.err != nil {
				t.Fatalf("%s failed: %v", result.identifier, result.err)
			}
			if result.body != result.identifier {
				t.Fatalf("%s body = %q", result.identifier, result.body)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("concurrent response did not drain")
		}
	}
}
