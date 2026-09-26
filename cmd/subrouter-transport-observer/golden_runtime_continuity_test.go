package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/iotest"
	"time"
)

func TestGoldenObserverStopWaitsForConnectionClosureBeforeClosingEvidence(t *testing.T) {
	events, err := os.OpenFile(filepath.Join(t.TempDir(), "transport.jsonl"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	stats := newObserverStats()
	observation := newObserver(events, stats)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closeStarted := make(chan struct{})
	closeReleased := make(chan struct{})
	closeFinished := make(chan struct{})
	shutdownStarted := make(chan struct{})
	var closeOnce, shutdownOnce sync.Once
	lifecycle := newGoldenObserverConnectionLifecycle()
	server := &http.Server{
		Handler: http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			finishRequest := observation.requests.begin()
			defer finishRequest()
			meta := requestEvidence{
				transport: "http", method: request.Method, path: observedPath(request.URL.Path),
				requestID: observation.requestID(), connectionID: observation.connectionID(request),
			}
			observation.emit(meta.event("request_started"))
			writer.WriteHeader(http.StatusNoContent)
		}),
		ConnState: func(connection net.Conn, state http.ConnState) {
			finish := lifecycle.begin(connection, state)
			defer finish()
			if state != http.StateClosed {
				return
			}
			closeOnce.Do(func() {
				close(closeStarted)
				<-closeReleased
				observation.closeConnection(connection.RemoteAddr().String())
				close(closeFinished)
			})
		},
	}
	server.RegisterOnShutdown(func() { shutdownOnce.Do(func() { close(shutdownStarted) }) })
	running := &runningGoldenObserver{
		label: "lifecycle", baseURL: "http://" + listener.Addr().String(), server: server,
		listener: listener, events: events, stats: stats, observation: observation, lifecycle: lifecycle, done: make(chan struct{}),
	}
	go func() {
		running.serveErr = server.Serve(listener)
		close(running.done)
	}()

	client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
	response, err := client.Get(running.baseURL + "/responses")
	if err != nil {
		t.Fatal(err)
	}
	if err := response.Body.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-closeStarted:
	case <-time.After(time.Second):
		t.Fatal("connection close callback did not start")
	}

	stopDone := make(chan error, 1)
	go func() {
		stopDone <- running.stop(context.Background())
	}()
	select {
	case <-shutdownStarted:
	case <-time.After(time.Second):
		t.Fatal("observer shutdown did not start")
	}
	prematureStop := false
	var stopErr error
	select {
	case stopErr = <-stopDone:
		prematureStop = true
	case <-time.After(100 * time.Millisecond):
	}
	if _, err := events.Stat(); err != nil {
		t.Errorf("evidence writer closed while connection callback was live: %v", err)
	}
	close(closeReleased)
	select {
	case <-closeFinished:
	case <-time.After(time.Second):
		t.Fatal("connection close callback did not finish")
	}
	if !prematureStop {
		select {
		case stopErr = <-stopDone:
		case <-time.After(time.Second):
			t.Fatal("observer stop did not finish after connection callback")
		}
	}
	if stopErr != nil {
		t.Errorf("observer stop failed: %v", stopErr)
	}
	if err := running.stop(context.Background()); err != nil {
		t.Errorf("repeated observer stop failed: %v", err)
	}
	if prematureStop {
		t.Error("observer stop returned before connection close callback finished")
	}
	closed := stats.closedSnapshot()
	if len(closed) != 1 || closed[0].ConnectionID == "" {
		t.Errorf("recorded connection closures = %#v, want one closure", closed)
	}
	_, _, recordingErrors := stats.snapshot()
	if recordingErrors != 0 {
		t.Errorf("observer errors = %d, want zero", recordingErrors)
	}
	if err := running.finalize(context.Background()); err != nil {
		t.Fatalf("observer finalize failed: %v", err)
	}
	if err := running.finalize(context.Background()); err != nil {
		t.Fatalf("repeated observer finalize failed: %v", err)
	}
	if _, err := events.Stat(); err == nil {
		t.Error("observer finalize left evidence writer open")
	}
}

func TestGoldenObserverAttributesUpstreamEvidenceToEachRequest(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "text/plain")
		_, _ = writer.Write([]byte("ok"))
	}))
	t.Cleanup(upstream.Close)
	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	stats := newObserverStats()
	observation := newObserver(io.Discard, stats)
	proxy := httptest.NewServer(newObserverHandlerWithObserverAndGate(upstreamURL, observation, nil))
	t.Cleanup(proxy.Close)

	request := func(method, path string) {
		t.Helper()
		var body io.Reader
		if method != http.MethodGet {
			body = strings.NewReader("request")
		}
		request, err := http.NewRequest(method, proxy.URL+path, body)
		if err != nil {
			t.Fatal(err)
		}
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.Copy(io.Discard, response.Body); err != nil {
			response.Body.Close()
			t.Fatal(err)
		}
		if err := response.Body.Close(); err != nil {
			t.Fatal(err)
		}
	}
	request(http.MethodGet, "/metadata")
	request(http.MethodPost, "/v1/responses")

	requests, _, _ := stats.snapshot()
	var responseRequest transportEvent
	for _, candidate := range requests {
		if candidate.Path == "/v1/responses" {
			responseRequest = candidate
			break
		}
	}
	if responseRequest.RequestID == "" {
		t.Fatal("response request was not observed")
	}
	opened, requestBytes, responseBytes := 0, int64(0), int64(0)
	for _, event := range stats.upstreamSnapshot() {
		if event.RequestID != responseRequest.RequestID {
			continue
		}
		if event.Path != "/v1/responses" {
			t.Fatalf("response upstream event path = %q, want /v1/responses", event.Path)
		}
		switch event.Kind {
		case "upstream_connection_used":
			opened++
		case "upstream_request_chunk":
			requestBytes += event.Bytes
		case "upstream_response_chunk":
			responseBytes += event.Bytes
		}
	}
	if opened != 1 || requestBytes <= 0 || responseBytes <= 0 {
		t.Fatalf("response upstream evidence = opened %d, request bytes %d, response bytes %d; want one complete upstream request", opened, requestBytes, responseBytes)
	}
}

func TestGoldenObserverKeepsUpstreamEvidenceRequestScopedOnPooledConnections(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "text/plain")
		_, _ = writer.Write([]byte("ok"))
	}))
	t.Cleanup(upstream.Close)
	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	stats := newObserverStats()
	observation := newObserver(io.Discard, stats)
	proxy := httptest.NewServer(newObserverHandlerWithObserverAndGate(upstreamURL, observation, nil))
	t.Cleanup(proxy.Close)

	request := func(method, path string) {
		t.Helper()
		var body io.Reader
		if method != http.MethodGet {
			body = strings.NewReader("request")
		}
		request, err := http.NewRequest(method, proxy.URL+path, body)
		if err != nil {
			t.Fatal(err)
		}
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.Copy(io.Discard, response.Body); err != nil {
			response.Body.Close()
			t.Fatal(err)
		}
		if err := response.Body.Close(); err != nil {
			t.Fatal(err)
		}
	}
	request(http.MethodGet, "/metadata")
	request(http.MethodGet, "/metadata")
	request(http.MethodPost, "/v1/responses")
	request(http.MethodPost, "/v1/responses")

	requests, _, _ := stats.snapshot()
	requestIDs := make(map[string]bool)
	for _, candidate := range requests {
		switch candidate.Path {
		case "/other", "/v1/responses":
			requestIDs[candidate.RequestID] = true
		}
	}
	if len(requestIDs) != 4 {
		t.Fatalf("request IDs = %d, want four", len(requestIDs))
	}
	physicalConnections := make(map[string]bool)
	connectionsByRequest := make(map[string]map[string]bool)
	requestBytesByRequest := make(map[string]int64)
	responseBytesByRequest := make(map[string]int64)
	for _, event := range stats.upstreamSnapshot() {
		if event.Kind == "upstream_connection_opened" {
			physicalConnections[event.ConnectionID] = true
		}
		if !requestIDs[event.RequestID] {
			continue
		}
		switch event.Kind {
		case "upstream_connection_used":
			if connectionsByRequest[event.RequestID] == nil {
				connectionsByRequest[event.RequestID] = make(map[string]bool)
			}
			connectionsByRequest[event.RequestID][event.ConnectionID] = true
		case "upstream_request_chunk":
			requestBytesByRequest[event.RequestID] += event.Bytes
		case "upstream_response_chunk":
			responseBytesByRequest[event.RequestID] += event.Bytes
		}
	}
	if len(physicalConnections) == 0 {
		t.Fatal("no physical upstream connection was observed")
	}
	for requestID := range requestIDs {
		connections := connectionsByRequest[requestID]
		if len(connections) != 1 || requestBytesByRequest[requestID] <= 0 || responseBytesByRequest[requestID] <= 0 {
			t.Fatalf("request %s upstream evidence = connections %d, request bytes %d, response bytes %d; want one complete exchange", requestID, len(connections), requestBytesByRequest[requestID], responseBytesByRequest[requestID])
		}
		for connectionID := range connections {
			if !physicalConnections[connectionID] {
				t.Fatalf("request %s used unknown connection %q", requestID, connectionID)
			}
		}
	}
}

func TestGoldenObserverFinalizeLeavesEvidenceOpenWhenRequestClosureIsMissing(t *testing.T) {
	events, err := os.OpenFile(filepath.Join(t.TempDir(), "transport.jsonl"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	stats := newObserverStats()
	stats.observe(transportEvent{Kind: "connection_opened", ConnectionID: "connection-000001"})
	stats.observe(transportEvent{Kind: "request_started", ConnectionID: "connection-000001"})
	running := &runningGoldenObserver{events: events, stats: stats}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got := fixedGoldenFailure(running.finalize(ctx)); got != "observer_request_connections_incomplete" {
		t.Fatalf("failure = %q, want observer_request_connections_incomplete", got)
	}
	if _, err := events.Stat(); err != nil {
		t.Fatalf("finalize closed evidence after incomplete request closure: %v", err)
	}
	if got := fixedGoldenFailure(running.finalize(context.Background())); got != "observer_request_connections_incomplete" {
		t.Fatalf("repeated finalize failure = %q, want observer_request_connections_incomplete", got)
	}
	if err := events.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestGoldenObserverFinalizationCountsRepeatedConnectionIdentifiers(t *testing.T) {
	stats := newObserverStats()
	for index := 0; index < 2; index++ {
		stats.observe(transportEvent{Kind: "connection_opened", ConnectionID: "reused-connection"})
		stats.observe(transportEvent{Kind: "request_started", ConnectionID: "reused-connection"})
	}
	stats.observe(transportEvent{Kind: "connection_closed", ConnectionID: "reused-connection"})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got := fixedGoldenFailure(waitGoldenObserverRequestConnectionsClosed(ctx, stats)); got != "observer_request_connections_incomplete" {
		t.Fatalf("failure = %q, want observer_request_connections_incomplete", got)
	}
	stats.observe(transportEvent{Kind: "connection_closed", ConnectionID: "reused-connection"})
	if err := waitGoldenObserverRequestConnectionsClosed(context.Background(), stats); err != nil {
		t.Fatalf("two recorded closures did not satisfy two connection lifecycles: %v", err)
	}
}

func TestGoldenObserverFinalizeWaitsForHijackedRequestCompletion(t *testing.T) {
	events, err := os.OpenFile(filepath.Join(t.TempDir(), "transport.jsonl"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	stats := newObserverStats()
	observation := newObserver(events, stats)
	finishRequest := observation.requests.begin()
	stats.observe(transportEvent{Kind: "connection_opened", ConnectionID: "websocket-connection"})
	stats.observe(transportEvent{Kind: "request_started", ConnectionID: "websocket-connection"})
	stats.observe(transportEvent{Kind: "connection_closed", ConnectionID: "websocket-connection"})
	running := &runningGoldenObserver{events: events, stats: stats, observation: observation}
	finalizeStarted := make(chan struct{})
	finalizeDone := make(chan error, 1)
	go func() {
		close(finalizeStarted)
		finalizeDone <- running.finalize(context.Background())
	}()
	<-finalizeStarted
	select {
	case err := <-finalizeDone:
		t.Fatalf("finalize returned before hijacked request completion: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	if _, err := events.Stat(); err != nil {
		t.Fatalf("finalize closed evidence while hijacked request was active: %v", err)
	}
	finishRequest()
	select {
	case err := <-finalizeDone:
		if err != nil {
			t.Fatalf("finalize failed after hijacked request completion: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("finalize did not finish after hijacked request completion")
	}
	if _, err := events.Stat(); err == nil {
		t.Error("finalize left evidence writer open")
	}
}

func TestGoldenFreshResumeConnectionUsesObserverScopeAndCleanupBoundary(t *testing.T) {
	cutoff := time.Now().UTC()
	connectionID := strings.Repeat("a", 64)
	newSession := func(observer *runningGoldenObserver, stamp time.Time) *goldenSession {
		if observer.stats == nil {
			observer.stats = newObserverStats()
		}
		observer.stats.observe(transportEvent{
			Kind: "request_started", Timestamp: stamp.Format(time.RFC3339Nano),
			Method: http.MethodPost, Path: "/responses", RequestID: "request-1", ConnectionID: connectionID,
		})
		return &goldenSession{baseURL: observer.baseURL + "/v1", observer: observer}
	}

	originalObserver := &runningGoldenObserver{baseURL: "http://127.0.0.1:41000", stats: newObserverStats()}
	resumeObserver := &runningGoldenObserver{baseURL: originalObserver.baseURL, stats: newObserverStats()}
	original := newSession(originalObserver, cutoff.Add(-time.Second))
	resume := newSession(resumeObserver, cutoff.Add(time.Millisecond))
	if err := requireGoldenFreshResumeConnection(original, resume, cutoff); err != nil {
		t.Fatalf("distinct sequential observer scopes should accept endpoint and opaque-ID reuse: %v", err)
	}

	sameObserver := &runningGoldenObserver{baseURL: "http://127.0.0.1:42000", stats: newObserverStats()}
	sameScopeOriginal := &goldenSession{baseURL: sameObserver.baseURL + "/v1", observer: sameObserver}
	sameScopeResume := newSession(sameObserver, cutoff.Add(time.Millisecond))
	if got := fixedGoldenFailure(requireGoldenFreshResumeConnection(sameScopeOriginal, sameScopeResume, cutoff)); got != "resume_connection_not_fresh" {
		t.Fatalf("same-observer failure = %q, want resume_connection_not_fresh", got)
	}

	preCutoffObserver := &runningGoldenObserver{baseURL: originalObserver.baseURL, stats: newObserverStats()}
	preCutoff := newSession(preCutoffObserver, cutoff.Add(-time.Nanosecond))
	if got := fixedGoldenFailure(requireGoldenFreshResumeConnection(original, preCutoff, cutoff)); got != "resume_connection_not_fresh" {
		t.Fatalf("pre-cutoff failure = %q, want resume_connection_not_fresh", got)
	}
}

func TestGoldenStableLocalEgressRejectsSocketSetChanges(t *testing.T) {
	socketA := strings.Repeat("a", 64)
	socketB := strings.Repeat("b", 64)
	socketC := strings.Repeat("c", 64)
	tests := []struct {
		name   string
		before []string
		after  []string
		want   string
	}{
		{name: "unrelated new socket", before: []string{socketA}, after: []string{socketA, socketB}, want: "local_egress_unrelated_socket"},
		{name: "one bound socket disappears", before: []string{socketA, socketB}, after: []string{socketA}, want: "local_egress_socket_disappeared"},
		{name: "reconnected socket", before: []string{socketA, socketB}, after: []string{socketA, socketC}, want: "local_egress_socket_reconnected"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			before := map[string]goldenProcessEvidence{
				"local-daemon": {Label: "local-daemon", RemoteSocketIDs: test.before},
			}
			after := map[string]goldenProcessEvidence{
				"local-daemon": {Label: "local-daemon", RemoteSocketIDs: test.after},
			}
			if got := fixedGoldenFailure(requireStableLocalEgress(before, after)); got != test.want {
				t.Fatalf("failure = %q, want %q", got, test.want)
			}
		})
	}
}

func TestGoldenLocalEgressMonitorRejectsTransientSocketChanges(t *testing.T) {
	socketA, _ := newGoldenRemoteSocket("127.0.0.1:42001->203.0.113.10:443")
	socketB, _ := newGoldenRemoteSocket("127.0.0.1:42002->203.0.113.10:443")
	evidence := func(sockets ...goldenRemoteSocket) goldenProcessEvidence {
		result := goldenProcessEvidence{remoteSockets: sockets}
		for _, socket := range sockets {
			result.RemoteSocketIDs = append(result.RemoteSocketIDs, socket.SocketID)
		}
		return result
	}
	expected := evidence(socketA)
	destinationChanged := socketA
	destinationChanged.DestinationID = strings.Repeat("f", 64)
	for _, test := range []struct {
		name   string
		actual goldenProcessEvidence
		want   string
	}{
		{name: "disappeared", actual: evidence(), want: "local_egress_socket_disappeared"},
		{name: "reconnected", actual: evidence(socketB), want: "local_egress_socket_reconnected"},
		{name: "unrelated", actual: evidence(socketA, socketB), want: "local_egress_unrelated_socket"},
		{name: "destination changed", actual: evidence(destinationChanged), want: "local_egress_destination_changed"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := fixedGoldenFailure(validateGoldenLocalEgressSocketSet(expected, test.actual)); got != test.want {
				t.Fatalf("failure = %q, want %q", got, test.want)
			}
		})
	}
}

func TestGoldenLocalEgressMonitorRejectsMissedSampleWindow(t *testing.T) {
	monitor := &goldenLocalEgressMonitor{lastStarted: time.Now().Add(-goldenLocalEgressMonitorMaxGap - time.Millisecond)}
	if monitor.sample() {
		t.Fatal("monitor accepted a missed sample window")
	}
	if got := fixedGoldenFailure(monitor.validate()); got != "local_egress_monitor_gap" {
		t.Fatalf("failure = %q, want local_egress_monitor_gap", got)
	}
}

func TestGoldenLocalEgressMonitorCatchesReplaceAndReturnBetweenPhaseSnapshots(t *testing.T) {
	socketA, _ := newGoldenRemoteSocket("127.0.0.1:42001->203.0.113.10:443")
	socketB, _ := newGoldenRemoteSocket("127.0.0.1:42002->203.0.113.10:443")
	evidence := func(socket goldenRemoteSocket) goldenProcessEvidence {
		return goldenProcessEvidence{
			Timestamp:       time.Now().UTC().Format(time.RFC3339Nano),
			RemoteSocketIDs: []string{socket.SocketID}, remoteSockets: []goldenRemoteSocket{socket},
		}
	}
	before := map[string]goldenProcessEvidence{"local-daemon": evidence(socketA)}
	after := map[string]goldenProcessEvidence{"local-daemon": evidence(socketA)}
	if err := requireStableLocalEgress(before, after); err != nil {
		t.Fatalf("phase snapshots should miss the transient replacement: %v", err)
	}
	var calls atomic.Int32
	capture := func(_, _ string, _ int) (goldenProcessEvidence, error) {
		if calls.Add(1) == 2 {
			return evidence(socketB), nil
		}
		return evidence(socketA), nil
	}
	runner := &goldenRunner{evidence: &jsonlRecorder{writer: io.Discard}}
	monitor, err := startGoldenLocalEgressMonitorWithCapture(
		context.Background(), runner, 1, "replace-and-return", before["local-daemon"], capture,
	)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-monitor.done:
	case <-time.After(time.Second):
		t.Fatal("monitor did not sample the transient replacement")
	}
	if got := fixedGoldenFailure(monitor.stopAndValidate()); got != "local_egress_socket_reconnected" {
		t.Fatalf("failure = %q, want local_egress_socket_reconnected", got)
	}
}

func TestGoldenLocalEgressBindingPinsRequestLeaseDestinationAndTransport(t *testing.T) {
	now := time.Now().UTC()
	requestStats := newObserverStats()
	requestStats.observe(transportEvent{
		Kind: "request_started", Timestamp: now.Format(time.RFC3339Nano), Transport: "websocket",
		Method: http.MethodGet, Path: "/responses", RequestID: "request-1", ConnectionID: strings.Repeat("a", 64),
	})
	leaseStats := newObserverStats()
	leaseStats.observe(transportEvent{
		Kind: "request_started", Timestamp: now.Add(time.Millisecond).Format(time.RFC3339Nano), Transport: "http",
		Method: http.MethodPost, Path: "/_subrouter/leases", RequestID: "lease-1", ConnectionID: strings.Repeat("b", 64),
	})
	socket, ok := newGoldenRemoteSocket("127.0.0.1:42001->203.0.113.10:443")
	if !ok {
		t.Fatal("test socket was not remote")
	}
	session := &goldenSession{
		label: "rehearsal-local-websocket", route: "local-egress", transport: "websocket",
		observer: &runningGoldenObserver{stats: requestStats}, localUpstreamSocket: strings.Repeat("c", 64),
	}
	before := goldenProcessEvidence{Timestamp: now.Add(-time.Millisecond).Format(time.RFC3339Nano), Label: "local-daemon"}
	after := goldenProcessEvidence{
		Timestamp: now.Add(2 * time.Millisecond).Format(time.RFC3339Nano), Label: "local-daemon",
		RemoteSocketIDs: []string{socket.SocketID}, remoteSockets: []goldenRemoteSocket{socket},
	}
	runner := &goldenRunner{evidence: &jsonlRecorder{writer: io.Discard}}
	if err := runner.bindGoldenLocalEgress(session, &runningGoldenObserver{stats: leaseStats}, 0, before, after); err != nil {
		t.Fatal(err)
	}
	if err := requireBoundLocalEgress([]*goldenSession{session}, map[string]goldenProcessEvidence{"local-daemon": after}); err != nil {
		t.Fatal(err)
	}
	reconnected, _ := newGoldenRemoteSocket("127.0.0.1:42002->203.0.113.10:443")
	reconnectedEvidence := after
	reconnectedEvidence.RemoteSocketIDs = []string{reconnected.SocketID}
	reconnectedEvidence.remoteSockets = []goldenRemoteSocket{reconnected}
	if got := fixedGoldenFailure(requireBoundLocalEgress(
		[]*goldenSession{session},
		map[string]goldenProcessEvidence{"local-daemon": reconnectedEvidence},
	)); got != "local_egress_socket_reconnected" {
		t.Fatalf("failure = %q, want local_egress_socket_reconnected", got)
	}
}

func TestGoldenLocalEgressBindingSelectsResponseLeaseAfterMetadataLeases(t *testing.T) {
	now := time.Now().UTC()
	requestStarted := now.Add(10 * time.Millisecond)
	requestStats := newObserverStats()
	requestStats.observe(transportEvent{
		Kind: "request_started", Timestamp: requestStarted.Format(time.RFC3339Nano), Transport: "websocket",
		Method: http.MethodGet, Path: "/responses", RequestID: "response-1", ConnectionID: strings.Repeat("a", 64),
	})
	leaseStats := newObserverStats()
	for index, stamp := range []time.Time{now, now.Add(5 * time.Millisecond), requestStarted.Add(time.Microsecond)} {
		leaseStats.observe(transportEvent{
			Kind: "request_started", Timestamp: stamp.Format(time.RFC3339Nano), Transport: "http",
			Method: http.MethodPost, Path: "/_subrouter/leases", RequestID: fmt.Sprintf("lease-%d", index+1),
			ConnectionID: strings.Repeat(string(rune('b'+index)), 64),
		})
	}
	socket, ok := newGoldenRemoteSocket("127.0.0.1:42001->203.0.113.10:443")
	if !ok {
		t.Fatal("test socket was not remote")
	}
	session := &goldenSession{
		label: "rehearsal-local-websocket", route: "local-egress", transport: "websocket",
		observer: &runningGoldenObserver{stats: requestStats}, localUpstreamSocket: strings.Repeat("c", 64),
	}
	before := goldenProcessEvidence{Timestamp: now.Add(-time.Millisecond).Format(time.RFC3339Nano), Label: "local-daemon"}
	after := goldenProcessEvidence{
		Timestamp: now.Add(20 * time.Millisecond).Format(time.RFC3339Nano), Label: "local-daemon",
		RemoteSocketIDs: []string{socket.SocketID}, remoteSockets: []goldenRemoteSocket{socket},
	}
	runner := &goldenRunner{evidence: &jsonlRecorder{writer: io.Discard}}
	if err := runner.bindGoldenLocalEgress(session, &runningGoldenObserver{stats: leaseStats}, 0, before, after); err != nil {
		t.Fatalf("metadata leases before the response lease should not invalidate binding: %v", err)
	}
	if got := session.localEgressBinding.LeaseRequestID; got != "lease-3" {
		t.Fatalf("bound lease = %q, want response lease", got)
	}
}

func TestGoldenLocalEgressBindingRejectsMultipleResponseLeases(t *testing.T) {
	now := time.Now().UTC()
	requestStarted := now.Add(10 * time.Millisecond)
	requestStats := newObserverStats()
	requestStats.observe(transportEvent{
		Kind: "request_started", Timestamp: requestStarted.Format(time.RFC3339Nano), Transport: "websocket",
		Method: http.MethodGet, Path: "/responses", RequestID: "response-1", ConnectionID: strings.Repeat("a", 64),
	})
	leaseStats := newObserverStats()
	for index, stamp := range []time.Time{requestStarted.Add(time.Microsecond), requestStarted.Add(2 * time.Microsecond)} {
		leaseStats.observe(transportEvent{
			Kind: "request_started", Timestamp: stamp.Format(time.RFC3339Nano), Transport: "http",
			Method: http.MethodPost, Path: "/_subrouter/leases", RequestID: fmt.Sprintf("lease-%d", index+1),
			ConnectionID: strings.Repeat(string(rune('b'+index)), 64),
		})
	}
	socket, ok := newGoldenRemoteSocket("127.0.0.1:42001->203.0.113.10:443")
	if !ok {
		t.Fatal("test socket was not remote")
	}
	session := &goldenSession{
		label: "rehearsal-local-websocket", route: "local-egress", transport: "websocket",
		observer: &runningGoldenObserver{stats: requestStats}, localUpstreamSocket: strings.Repeat("c", 64),
	}
	before := goldenProcessEvidence{Timestamp: now.Add(-time.Millisecond).Format(time.RFC3339Nano), Label: "local-daemon"}
	after := goldenProcessEvidence{
		Timestamp: now.Add(20 * time.Millisecond).Format(time.RFC3339Nano), Label: "local-daemon",
		RemoteSocketIDs: []string{socket.SocketID}, remoteSockets: []goldenRemoteSocket{socket},
	}
	runner := &goldenRunner{evidence: &jsonlRecorder{writer: io.Discard}}
	if got := fixedGoldenFailure(runner.bindGoldenLocalEgress(
		session, &runningGoldenObserver{stats: leaseStats}, 0, before, after,
	)); got != "local_egress_lease_binding_invalid" {
		t.Fatalf("failure = %q, want local_egress_lease_binding_invalid", got)
	}
}

func TestGoldenLocalEgressBindingAllowsExactHTTPConnectionReuse(t *testing.T) {
	now := time.Now().UTC()
	leaseStats := newObserverStats()
	leaseStats.observe(transportEvent{
		Kind: "request_started", Timestamp: now.Add(time.Millisecond).Format(time.RFC3339Nano), Transport: "http",
		Method: http.MethodPost, Path: "/_subrouter/leases", RequestID: "lease-1",
		ConnectionID: strings.Repeat("d", 64),
	})
	socket, ok := newGoldenRemoteSocket("127.0.0.1:42001->203.0.113.10:443")
	if !ok {
		t.Fatal("test socket was not remote")
	}
	newSession := func(label, requestID string, started time.Time, upstreamID string) *goldenSession {
		stats := newObserverStats()
		stats.observe(transportEvent{
			Kind: "request_started", Timestamp: started.Format(time.RFC3339Nano), Transport: "http",
			Method: http.MethodPost, Path: "/responses", RequestID: requestID, ConnectionID: strings.Repeat("a", 64),
		})
		return &goldenSession{
			label: label, route: "local-egress", transport: "http",
			observer: &runningGoldenObserver{stats: stats}, localUpstreamSocket: upstreamID,
		}
	}
	first := newSession("first-local-http", "request-1", now, strings.Repeat("b", 64))
	second := newSession("second-local-http", "request-2", now.Add(3*time.Millisecond), strings.Repeat("c", 64))
	third := newSession("third-local-http", "request-3", now.Add(6*time.Millisecond), strings.Repeat("f", 64))
	before := goldenProcessEvidence{Timestamp: now.Add(-time.Millisecond).Format(time.RFC3339Nano), Label: "local-daemon"}
	bound := goldenProcessEvidence{
		Timestamp: now.Add(2 * time.Millisecond).Format(time.RFC3339Nano), Label: "local-daemon",
		RemoteSocketIDs: []string{socket.SocketID}, remoteSockets: []goldenRemoteSocket{socket},
	}
	reused := bound
	reused.Timestamp = now.Add(5 * time.Millisecond).Format(time.RFC3339Nano)
	runner := &goldenRunner{evidence: &jsonlRecorder{writer: io.Discard}}
	leaseObserver := &runningGoldenObserver{stats: leaseStats}
	if err := runner.bindGoldenLocalEgress(first, leaseObserver, 0, before, bound); err != nil {
		t.Fatal(err)
	}
	leaseStats.observe(transportEvent{
		Kind: "request_started", Timestamp: now.Add(4 * time.Millisecond).Format(time.RFC3339Nano), Transport: "http",
		Method: http.MethodPost, Path: "/_subrouter/leases", RequestID: "lease-2",
		ConnectionID: strings.Repeat("e", 64),
	})
	if got := fixedGoldenFailure(runner.bindGoldenLocalEgress(second, leaseObserver, 1, bound, reused)); got != "local_egress_correlation_missing" {
		t.Fatalf("overlapping reuse failure = %q, want local_egress_correlation_missing", got)
	}
	first.done = make(chan struct{})
	close(first.done)
	first.finishedAt = now.Add(2500 * time.Microsecond)
	if err := runner.bindGoldenLocalEgress(second, leaseObserver, 1, bound, reused); err != nil {
		t.Fatal(err)
	}
	leaseStats.observe(transportEvent{
		Kind: "request_started", Timestamp: now.Add(7 * time.Millisecond).Format(time.RFC3339Nano), Transport: "http",
		Method: http.MethodPost, Path: "/_subrouter/leases", RequestID: "lease-3",
		ConnectionID: strings.Repeat("f", 64),
	})
	thirdReuse := reused
	thirdReuse.Timestamp = now.Add(8 * time.Millisecond).Format(time.RFC3339Nano)
	if got := fixedGoldenFailure(runner.bindGoldenLocalEgress(third, leaseObserver, 2, reused, thirdReuse)); got != "local_egress_correlation_missing" {
		t.Fatalf("chained overlapping reuse failure = %q, want local_egress_correlation_missing", got)
	}
	second.done = make(chan struct{})
	close(second.done)
	second.finishedAt = now.Add(5500 * time.Microsecond)
	if err := runner.bindGoldenLocalEgress(third, leaseObserver, 2, reused, thirdReuse); err != nil {
		t.Fatal(err)
	}
	if err := requireBoundLocalEgress([]*goldenSession{second}, map[string]goldenProcessEvidence{"local-daemon": reused}); err != nil {
		t.Fatal(err)
	}
}

func TestGoldenLocalDaemonStderrIsClassifiedWithoutPersistingText(t *testing.T) {
	var evidence bytes.Buffer
	runner := &goldenRunner{evidence: &jsonlRecorder{writer: &evidence}}
	runner.consumeGoldenLocalDaemonStderr(strings.NewReader("retrying upstream SECRET_DIAGNOSTIC\n"))
	if got := fixedGoldenFailure(runner.requireGoldenLocalDaemonTransportClean()); got != "local_daemon_transport_issue_retry" {
		t.Fatalf("failure = %q, want local_daemon_transport_issue_retry", got)
	}
	if strings.Contains(evidence.String(), "SECRET_DIAGNOSTIC") || !strings.Contains(evidence.String(), `"category":"retry"`) {
		t.Fatalf("non-content-blind daemon evidence: %s", evidence.String())
	}
}

func TestGoldenLocalDaemonStderrIgnoresShutdownTimeoutConfiguration(t *testing.T) {
	var evidence bytes.Buffer
	runner := &goldenRunner{evidence: &jsonlRecorder{writer: &evidence}}
	runner.consumeGoldenLocalDaemonStderr(strings.NewReader(
		"2026-08-07T04:49:35Z INFO subrouter shutdown signal received signal=terminated timeout=10m0s\n",
	))
	if err := runner.requireGoldenLocalDaemonTransportClean(); err != nil {
		t.Fatalf("normal shutdown metadata was treated as a transport failure: %v", err)
	}
	if evidence.Len() != 0 {
		t.Fatalf("normal shutdown metadata emitted issue evidence: %s", evidence.String())
	}

	evidence.Reset()
	runner = &goldenRunner{evidence: &jsonlRecorder{writer: &evidence}}
	runner.consumeGoldenLocalDaemonStderr(strings.NewReader("upstream request timeout while reading response\n"))
	if got := fixedGoldenFailure(runner.requireGoldenLocalDaemonTransportClean()); got != "local_daemon_transport_issue_timeout" {
		t.Fatalf("real timeout failure = %q, want local_daemon_transport_issue_timeout", got)
	}

	evidence.Reset()
	runner = &goldenRunner{evidence: &jsonlRecorder{writer: &evidence}}
	runner.consumeGoldenLocalDaemonStderr(strings.NewReader("repeated timeouts while connecting upstream\n"))
	if got := fixedGoldenFailure(runner.requireGoldenLocalDaemonTransportClean()); got != "local_daemon_transport_issue_timeout" {
		t.Fatalf("plural timeout failure = %q, want local_daemon_transport_issue_timeout", got)
	}

	evidence.Reset()
	runner = &goldenRunner{evidence: &jsonlRecorder{writer: &evidence}}
	runner.consumeGoldenLocalDaemonStderr(strings.NewReader("upstream_request timeout=true\n"))
	if got := fixedGoldenFailure(runner.requireGoldenLocalDaemonTransportClean()); got != "local_daemon_transport_issue_timeout" {
		t.Fatalf("structured timeout failure = %q, want local_daemon_transport_issue_timeout", got)
	}
}

func TestGoldenLocalDaemonStderrIgnoresStructuredRequestMetadata(t *testing.T) {
	var evidence bytes.Buffer
	runner := &goldenRunner{evidence: &jsonlRecorder{writer: &evidence}}
	runner.consumeGoldenLocalDaemonStderr(strings.NewReader(
		"2026/08/07 10:04:57 INFO proxy request agent=codex session=fallback:0123456789abcdef user=\"\" account=account-timeout method=POST path=/responses upstream=example.test remote_addr=127.0.0.1:1234 user_agent=\"client msg=retry-client\"\n",
	))
	if err := runner.requireGoldenLocalDaemonTransportClean(); err != nil {
		t.Fatalf("ordinary structured request metadata was treated as a transport failure: %v", err)
	}
	if evidence.Len() != 0 {
		t.Fatalf("ordinary structured request metadata emitted issue evidence: %s", evidence.String())
	}
}

func TestGoldenLocalDaemonStderrClassifiesMessagesAcrossFragmentedReads(t *testing.T) {
	var evidence bytes.Buffer
	runner := &goldenRunner{evidence: &jsonlRecorder{writer: &evidence}}
	runner.consumeGoldenLocalDaemonStderr(iotest.OneByteReader(strings.NewReader(
		"time=2026-08-07T10:04:57Z level=INFO msg=\"proxy request\" session=fallback:0123456789abcdef\n" +
			"time=2026-08-07T10:04:58Z level=WARN msg=\"retrying replayable upstream request after transport failure\" session=ordinary\n",
	)))
	if runner.localIssues["fallback"] != 0 {
		t.Fatalf("structured fallback metadata was classified: %+v", runner.localIssues)
	}
	if runner.localIssues["retry"] != 1 || runner.localIssues["error"] != 1 {
		t.Fatalf("transport message categories = %+v, want one retry and one error", runner.localIssues)
	}
}

func TestGoldenLocalDaemonStderrRejectsOversizedRecord(t *testing.T) {
	var evidence bytes.Buffer
	runner := &goldenRunner{evidence: &jsonlRecorder{writer: &evidence}}
	runner.consumeGoldenLocalDaemonStderr(strings.NewReader(
		strings.Repeat("x", goldenLocalDaemonMaxLogRecordBytes+1),
	))
	if got := fixedGoldenFailure(runner.requireGoldenLocalDaemonTransportClean()); got != "local_daemon_transport_issue_error" {
		t.Fatalf("oversized record failure = %q, want local_daemon_transport_issue_error", got)
	}
	if strings.Contains(evidence.String(), strings.Repeat("x", 32)) {
		t.Fatal("oversized record contents leaked into evidence")
	}
}

func TestGoldenCounterContinuityRejectsActionRebaselining(t *testing.T) {
	tests := []struct {
		name string
		edit func(*goldenSummary)
	}{
		{
			name: "slot restart between activation and rollback",
			edit: func(summary *goldenSummary) {
				summary.Rollback.canonical.Metrics.RetiringSlot.NRestarts = goldenDeployCounter{
					Before: goldenInt64(1), After: goldenInt64(1),
				}
			},
		},
		{
			name: "slot oom between rollback and retirement",
			edit: func(summary *goldenSummary) {
				summary.OldGenerationCleanup.canonical.Metrics.OldSlot = validGoldenServiceMetrics(goldenRSSLimitBytes)
				summary.OldGenerationCleanup.canonical.Metrics.OldSlot.OOMKill = goldenDeployCounter{
					Before: goldenInt64(1), After: goldenInt64(1),
				}
			},
		},
		{
			name: "front oom between migration and slot activation",
			edit: func(summary *goldenSummary) {
				summary.Activation.canonical.Metrics.Front.OOMKill = goldenDeployCounter{
					Before: goldenInt64(1), After: goldenInt64(1),
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			summary := validGoldenAcceptanceSummary()
			test.edit(&summary)
			if got := fixedGoldenFailure(validateGoldenSummary(summary, true)); got != "server_counter_continuity_invalid" {
				t.Fatalf("failure = %q, want server_counter_continuity_invalid", got)
			}
		})
	}
}

func TestGoldenAgentPayloadRequiresExactOrderedNumberedLines(t *testing.T) {
	nonce := "nonce_0123456789abcdef"
	marker := "SR_GOLDEN_COMPLETE_0123456789abcdef"
	tests := []struct {
		name string
		text string
		ok   bool
	}{
		{name: "valid", text: nonce + "\n1 x\n2 x\n3 x\n" + marker, ok: true},
		{name: "duplicate nonce", text: nonce + "\n" + nonce + "\n1 x\n2 x\n3 x\n" + marker},
		{name: "missing numbered line", text: nonce + "\n1 x\n3 x\n" + marker},
		{name: "duplicate numbered line", text: nonce + "\n1 x\n2 x\n2 x\n3 x\n" + marker},
		{name: "reordered numbered lines", text: nonce + "\n2 x\n1 x\n3 x\n" + marker},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			evidence, err := validateGoldenAgentMessagePayload(test.text, nonce, marker, 3)
			if test.ok {
				if err != nil {
					t.Fatal(err)
				}
				if evidence.NumberedLineCount != 3 || len(evidence.NumberedLinesSHA256) != 64 {
					t.Fatalf("evidence = %#v", evidence)
				}
				return
			}
			if err == nil {
				t.Fatalf("accepted malformed payload: %#v", evidence)
			}
		})
	}
}

func TestGoldenAgentPayloadAcceptsOneOrMultipleAgentMessages(t *testing.T) {
	nonce := "nonce_0123456789abcdef"
	marker := "SR_GOLDEN_COMPLETE_0123456789abcdef"
	for _, test := range []struct {
		name     string
		messages []string
	}{
		{name: "one message", messages: []string{nonce + "\n1 x\n2 x\n3 x\n" + marker}},
		{name: "multiple messages", messages: []string{nonce + "\n1 x", "2 x\n3 x\n" + marker}},
	} {
		t.Run(test.name, func(t *testing.T) {
			session := &goldenSession{nonce: nonce, marker: marker, payloadExpectedLines: 3}
			for _, message := range test.messages {
				observeGoldenAgentMessage(session, message)
			}
			session.mu.Lock()
			defer session.mu.Unlock()
			if session.payloadInvalid || session.nonceCount != 1 || session.markerCount != 1 ||
				session.payloadNumberedLines != 3 || len(session.payloadSHA256) != 64 {
				t.Fatalf("payload state = %#v", session)
			}
		})
	}
}
