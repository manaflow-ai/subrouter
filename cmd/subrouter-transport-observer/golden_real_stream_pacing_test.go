package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
)

type accumulatingObserverDelay struct {
	elapsed time.Duration
}

func (delay *accumulatingObserverDelay) wait(
	_ context.Context,
	_ <-chan struct{},
	_ <-chan struct{},
	duration time.Duration,
) error {
	delay.elapsed += duration
	return nil
}

func TestGoldenObserverDefaultPacingOutlastsDeploymentGate(t *testing.T) {
	gate := newGoldenResponseGate()
	pacer := gate.newResponsePacer()
	delay := &accumulatingObserverDelay{}
	pacer.delay = delay

	var payload bytes.Buffer
	for line := 1; line <= 4000; line++ {
		if _, err := fmt.Fprintf(&payload, "%d x\n", line); err != nil {
			t.Fatal(err)
		}
	}
	var delivered bytes.Buffer
	if _, err := pacer.write(context.Background(), payload.Bytes(), delivered.Write); err != nil {
		t.Fatal(err)
	}
	if delay.elapsed < 20*time.Minute {
		t.Fatalf("finite golden response pacing runway = %s, want at least 20m", delay.elapsed)
	}
	if delivered.Len() >= payload.Len() {
		t.Fatal("finite golden response completed before gate release")
	}

	gate.releasePacing()
	if err := pacer.waitAndFlush(); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(delivered.Bytes(), payload.Bytes()) {
		t.Fatal("released golden response did not preserve its payload")
	}
}

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

func TestGoldenObserverSupersedesAbandonedResponseAttempt(t *testing.T) {
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
	results := map[string]chan responseResult{
		"first-response":  make(chan responseResult, 1),
		"second-response": make(chan responseResult, 1),
	}
	startRequest := func(identifier string) {
		go func(identifier string) {
			response, err := http.Post(observation.baseURL+"/v1/responses?id="+identifier, "application/json", bytes.NewReader([]byte("{}")))
			if err != nil {
				results[identifier] <- responseResult{identifier: identifier, err: err}
				return
			}
			body, readErr := io.ReadAll(response.Body)
			closeErr := response.Body.Close()
			if readErr == nil {
				readErr = closeErr
			}
			results[identifier] <- responseResult{identifier: identifier, body: string(body), err: readErr}
		}(identifier)
	}
	waitForUpstream := func(identifier string) {
		t.Helper()
		select {
		case got := <-upstreamWritten:
			if got != identifier {
				t.Fatalf("upstream response = %q, want %q", got, identifier)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s upstream response was not written", identifier)
		}
	}

	startRequest("first-response")
	waitForUpstream("first-response")
	startRequest("second-response")
	waitForUpstream("second-response")

	select {
	case result := <-results["first-response"]:
		if result.err != nil {
			t.Fatalf("superseded response failed: %v", result.err)
		}
		if result.body != "" {
			t.Fatalf("superseded response body = %q, want empty", result.body)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("superseded response remained held")
	}
	select {
	case result := <-results["second-response"]:
		t.Fatalf("current response completed before gate release: body=%q err=%v", result.body, result.err)
	case <-time.After(100 * time.Millisecond):
	}

	if err := releaseGoldenTestSessions([]*goldenSession{{observer: observation}}); err != nil {
		t.Fatal(err)
	}
	select {
	case result := <-results["second-response"]:
		if result.err != nil {
			t.Fatalf("current response failed: %v", result.err)
		}
		if result.body != result.identifier {
			t.Fatalf("current response body = %q, want %q", result.body, result.identifier)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("current response did not drain")
	}
}
