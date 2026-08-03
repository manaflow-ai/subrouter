package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

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
	if err := requireGoldenFreshResumeConnection(original, resume, cutoff, false); err != nil {
		t.Fatalf("distinct sequential observer scopes should accept endpoint and opaque-ID reuse: %v", err)
	}

	sameObserver := &runningGoldenObserver{baseURL: "http://127.0.0.1:42000", stats: newObserverStats()}
	sameScopeOriginal := &goldenSession{baseURL: sameObserver.baseURL + "/v1", observer: sameObserver}
	sameScopeResume := newSession(sameObserver, cutoff.Add(time.Millisecond))
	if got := fixedGoldenFailure(requireGoldenFreshResumeConnection(sameScopeOriginal, sameScopeResume, cutoff, false)); got != "resume_connection_not_fresh" {
		t.Fatalf("same-observer failure = %q, want resume_connection_not_fresh", got)
	}

	preCutoffObserver := &runningGoldenObserver{baseURL: originalObserver.baseURL, stats: newObserverStats()}
	preCutoff := newSession(preCutoffObserver, cutoff.Add(-time.Nanosecond))
	if got := fixedGoldenFailure(requireGoldenFreshResumeConnection(original, preCutoff, cutoff, false)); got != "resume_connection_not_fresh" {
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
		Method: http.MethodPost, Path: "/api/subrouter/leases", RequestID: "lease-1", ConnectionID: strings.Repeat("b", 64),
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

func TestGoldenLocalEgressBindingAllowsExactHTTPConnectionReuse(t *testing.T) {
	now := time.Now().UTC()
	leaseStats := newObserverStats()
	leaseStats.observe(transportEvent{
		Kind: "request_started", Timestamp: now.Add(time.Millisecond).Format(time.RFC3339Nano), Transport: "http",
		Method: http.MethodPost, Path: "/api/subrouter/leases", RequestID: "lease-1",
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
		Method: http.MethodPost, Path: "/api/subrouter/leases", RequestID: "lease-2",
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
		Method: http.MethodPost, Path: "/api/subrouter/leases", RequestID: "lease-3",
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
			name: "front oom between migration transitions",
			edit: func(summary *goldenSummary) {
				summary.MigrationRollback.migrationCanonical.Metrics.Front.OOMKill = goldenDeployCounter{
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
