package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"runtime"
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
	pacer := gate.newResponsePacer("")
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

func TestGoldenResumeWaitReleasesResponsePacing(t *testing.T) {
	previousHooks := goldenTestHooks
	goldenTestHooks.enabled = false
	t.Cleanup(func() { goldenTestHooks = previousHooks })

	payload := bytes.Repeat([]byte("resume-response-"), 64)
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
	observation, err := runner.startObserver("resume-stream-pacing", upstreamURL)
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

	session := &goldenSession{
		observer: observation,
		done:     make(chan struct{}),
	}
	result := make(chan struct {
		body []byte
		err  error
	}, 1)
	go func() {
		defer close(session.done)
		request, requestErr := http.NewRequest(
			http.MethodPost,
			observation.baseURL+"/v1/responses",
			bytes.NewReader([]byte("{}")),
		)
		if requestErr != nil {
			result <- struct {
				body []byte
				err  error
			}{err: requestErr}
			return
		}
		request.Header.Set(goldenResponseAttemptTokenHeader, "0123456789abcdef0123456789abcdef")
		response, requestErr := http.DefaultClient.Do(request)
		if requestErr != nil {
			result <- struct {
				body []byte
				err  error
			}{err: requestErr}
			return
		}
		body, readErr := io.ReadAll(response.Body)
		closeErr := response.Body.Close()
		if readErr == nil {
			readErr = closeErr
		}
		result <- struct {
			body []byte
			err  error
		}{body: body, err: readErr}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := waitGoldenResumeSessions(ctx, []*goldenSession{session}); err != nil {
		t.Fatal(err)
	}
	response := <-result
	if response.err != nil {
		t.Fatal(response.err)
	}
	if !bytes.Equal(response.body, payload) {
		t.Fatalf("released resume payload = %q, want %q", response.body, payload)
	}
}

func TestGoldenResumeWaitReleasesEveryGateAfterChunkFailure(t *testing.T) {
	previousHooks := goldenTestHooks
	goldenTestHooks.enabled = false
	t.Cleanup(func() { goldenTestHooks = previousHooks })

	failedDone := make(chan struct{})
	close(failedDone)
	failed := &goldenSession{
		observer: &runningGoldenObserver{
			gate:  newGoldenResponseGate(),
			stats: newObserverStats(),
		},
		done: failedDone,
	}

	blockedGate := newGoldenResponseGate()
	blockedPacer := blockedGate.newResponsePacer("0123456789abcdef0123456789abcdef")
	blockedDone := make(chan struct{})
	blockedReleased := make(chan struct{})
	allowBlockedFinish := make(chan struct{})
	go func() {
		_, _ = blockedPacer.write(context.Background(), []byte("held resume"), io.Discard.Write)
		_ = blockedPacer.waitAndFlush()
		close(blockedReleased)
		<-allowBlockedFinish
		close(blockedDone)
	}()
	blocked := &goldenSession{
		observer: &runningGoldenObserver{gate: blockedGate, stats: newObserverStats()},
		done:     blockedDone,
	}

	waitResult := make(chan error, 1)
	go func() {
		waitResult <- waitGoldenResumeSessions(context.Background(), []*goldenSession{failed, blocked})
	}()
	select {
	case <-blockedReleased:
	case <-time.After(5 * time.Second):
		t.Fatal("another resume gate remained held after chunk failure")
	}
	select {
	case err := <-waitResult:
		t.Fatalf("resume wait returned before every released session finished: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(allowBlockedFinish)
	err := <-waitResult
	if got := fixedGoldenFailure(err); got != "stream_baseline_ended_early" {
		t.Fatalf("resume wait failure = %q, want stream_baseline_ended_early", got)
	}
	select {
	case <-blockedDone:
	case <-time.After(5 * time.Second):
		t.Fatal("another resume gate remained held after chunk failure")
	}
}

func TestGoldenObserverLeavesPostReleaseResponsesIndependent(t *testing.T) {
	gate := newGoldenResponseGate()
	gate.releasePacing()
	first := gate.newResponsePacer("")
	second := gate.newResponsePacer("")

	var firstDelivered bytes.Buffer
	if _, err := first.write(context.Background(), []byte("first"), firstDelivered.Write); err != nil {
		t.Fatal(err)
	}
	var secondDelivered bytes.Buffer
	if _, err := second.write(context.Background(), []byte("second"), secondDelivered.Write); err != nil {
		t.Fatal(err)
	}

	if firstDelivered.String() != "first" {
		t.Fatalf("first post-release response = %q, want first", firstDelivered.String())
	}
	if secondDelivered.String() != "second" {
		t.Fatalf("second post-release response = %q, want second", secondDelivered.String())
	}
}

func TestGoldenObserverSerializesSupersessionWithGateRelease(t *testing.T) {
	gate := newGoldenResponseGate()
	token := "0123456789abcdef0123456789abcdef"
	previous := gate.newResponsePacer(token)

	onceEntered := make(chan struct{})
	onceRelease := make(chan struct{})
	go previous.releaseRequestOnce.Do(func() {
		close(onceEntered)
		<-onceRelease
	})
	<-onceEntered

	newPacerDone := make(chan struct{})
	go func() {
		gate.newResponsePacer(token)
		close(newPacerDone)
	}()
	deadline := time.After(5 * time.Second)
	for !previous.wasSuperseded() {
		select {
		case <-deadline:
			t.Fatal("new response did not begin superseding the previous attempt")
		default:
			runtime.Gosched()
		}
	}

	releaseDone := make(chan struct{})
	go func() {
		gate.releasePacing()
		close(releaseDone)
	}()
	select {
	case <-releaseDone:
		t.Fatal("gate release overtook an in-progress supersession")
	case <-time.After(100 * time.Millisecond):
	}

	close(onceRelease)
	select {
	case <-newPacerDone:
	case <-time.After(5 * time.Second):
		t.Fatal("supersession did not finish")
	}
	select {
	case <-releaseDone:
	case <-time.After(5 * time.Second):
		t.Fatal("gate release did not finish after supersession")
	}
}

func TestGoldenObserverSupersedesAbandonedResponseAttempt(t *testing.T) {
	previousHooks := goldenTestHooks
	goldenTestHooks.enabled = false
	t.Cleanup(func() { goldenTestHooks = previousHooks })

	upstreamWritten := make(chan string, 2)
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get(goldenResponseAttemptTokenHeader) != "" {
			t.Error("golden response request token escaped the observer")
		}
		identifier := request.URL.Query().Get("id")
		writer.Header().Set("Content-Type", "text/event-stream")
		writer.(http.Flusher).Flush()
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
			request, err := http.NewRequest(
				http.MethodPost,
				observation.baseURL+"/v1/responses?id="+identifier,
				bytes.NewReader([]byte("{}")),
			)
			if err != nil {
				results[identifier] <- responseResult{identifier: identifier, err: err}
				return
			}
			request.Header.Set(goldenResponseAttemptTokenHeader, "0123456789abcdef0123456789abcdef")
			response, err := http.DefaultClient.Do(request)
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

func TestGoldenObserverKeepsDistinctConcurrentResponsesIndependent(t *testing.T) {
	previousHooks := goldenTestHooks
	goldenTestHooks.enabled = false
	t.Cleanup(func() { goldenTestHooks = previousHooks })

	upstreamWritten := make(chan string, 2)
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		identifier := request.URL.Query().Get("id")
		writer.Header().Set("Content-Type", "text/event-stream")
		writer.(http.Flusher).Flush()
		_, _ = writer.Write([]byte(identifier))
		upstreamWritten <- identifier
	}))
	t.Cleanup(upstream.Close)
	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}

	runner := &goldenRunner{artifactDir: t.TempDir()}
	observation, err := runner.startObserver("distinct-concurrent-real-stream-pacing", upstreamURL)
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
	startRequest := func(identifier, token string) {
		go func() {
			request, requestErr := http.NewRequest(
				http.MethodPost,
				observation.baseURL+"/v1/responses?id="+identifier,
				bytes.NewReader([]byte("{}")),
			)
			if requestErr != nil {
				results <- responseResult{identifier: identifier, err: requestErr}
				return
			}
			request.Header.Set(goldenResponseAttemptTokenHeader, token)
			response, requestErr := http.DefaultClient.Do(request)
			if requestErr != nil {
				results <- responseResult{identifier: identifier, err: requestErr}
				return
			}
			body, readErr := io.ReadAll(response.Body)
			closeErr := response.Body.Close()
			if readErr == nil {
				readErr = closeErr
			}
			results <- responseResult{identifier: identifier, body: string(body), err: readErr}
		}()
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

	startRequest("first-response", "0123456789abcdef0123456789abcdef")
	waitForUpstream("first-response")
	startRequest("second-response", "fedcba9876543210fedcba9876543210")
	waitForUpstream("second-response")

	select {
	case result := <-results:
		t.Fatalf("distinct response completed before gate release: id=%s body=%q err=%v", result.identifier, result.body, result.err)
	case <-time.After(100 * time.Millisecond):
	}

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
				t.Fatalf("%s body = %q, want %q", result.identifier, result.body, result.identifier)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("distinct concurrent response did not drain")
		}
	}
}
