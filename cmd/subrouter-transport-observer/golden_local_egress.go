package main

import (
	"context"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	goldenLocalEgressMonitorInterval = 50 * time.Millisecond
	goldenLocalEgressMonitorMaxGap   = 100 * time.Millisecond
)

type goldenRemoteSocket struct {
	SocketID      string
	LocalID       string
	DestinationID string
}

func goldenLocalEgressMaxGapForRun() time.Duration {
	if goldenTestHooks.enabled && goldenTestHooks.localEgressMaxGap > 0 {
		return goldenTestHooks.localEgressMaxGap
	}
	return goldenLocalEgressMonitorMaxGap
}

func newGoldenRemoteSocket(name string) (goldenRemoteSocket, bool) {
	local, destination, found := strings.Cut(strings.TrimSpace(name), "->")
	local = strings.TrimSpace(local)
	destination = strings.TrimSpace(destination)
	if !found || local == "" || destination == "" || !socketDestinationIsRemote(name) {
		return goldenRemoteSocket{}, false
	}
	result := goldenRemoteSocket{
		SocketID:      goldenSocketEndpointID(name),
		LocalID:       goldenSocketEndpointID(local),
		DestinationID: goldenSocketEndpointID(destination),
	}
	return result, result.SocketID != "" && result.LocalID != "" && result.DestinationID != ""
}

func deduplicateGoldenRemoteSockets(values []goldenRemoteSocket) []goldenRemoteSocket {
	if len(values) == 0 {
		return nil
	}
	result := values[:1]
	for _, value := range values[1:] {
		if value.SocketID != result[len(result)-1].SocketID {
			result = append(result, value)
		}
	}
	return result
}

type goldenLocalEgressBinding struct {
	SessionLabel      string
	RequestID         string
	LeaseRequestID    string
	LeaseConnectionID string
	DestinationID     string
	Transport         string
	SocketID          string
	LocalUpstreamID   string

	leaseStats *observerStats
	session    *goldenSession
}

func goldenRemoteSocketsByID(evidence goldenProcessEvidence) map[string]goldenRemoteSocket {
	result := make(map[string]goldenRemoteSocket, len(evidence.remoteSockets))
	for _, socket := range evidence.remoteSockets {
		if socket.SocketID != "" {
			result[socket.SocketID] = socket
		}
	}
	return result
}

// goldenLeaseRequestPath recognizes both the legacy broker route and the
// tenant-scoped hosted route. Team-mode local egress uses the latter, while
// older clients can still use the former.
func goldenLeaseRequestPath(path string) bool {
	return path == "/api/subrouter/leases" || path == "/_subrouter/leases"
}

func goldenLeaseRequests(stats *observerStats) []transportEvent {
	if stats == nil {
		return nil
	}
	requests, _, _ := stats.snapshot()
	result := make([]transportEvent, 0, len(requests))
	for _, request := range requests {
		if goldenLeaseRequestPath(request.Path) {
			result = append(result, request)
		}
	}
	return result
}

func goldenHostedLeaseRequests(stats *observerStats) []transportEvent {
	if stats == nil {
		return nil
	}
	requests, _, _ := stats.snapshot()
	result := make([]transportEvent, 0, len(requests))
	for _, request := range requests {
		if request.Path == "/_subrouter/leases" {
			result = append(result, request)
		}
	}
	return result
}

func goldenResponseMethodMatchesTransport(method, transport string) bool {
	switch transport {
	case "websocket":
		return method == http.MethodGet
	case "http":
		return method == http.MethodPost
	default:
		return false
	}
}

func (r *goldenRunner) prepareGoldenLocalEgressBinding(
	session *goldenSession,
	leaseObserver *runningGoldenObserver,
	leaseBefore int,
	before, after goldenProcessEvidence,
) (*goldenLocalEgressBinding, bool, error) {
	if session == nil || session.route != "local-egress" || session.observer == nil ||
		leaseObserver == nil || leaseObserver.stats == nil || leaseBefore < 0 {
		return nil, false, failGolden("local_egress_binding_invalid")
	}
	request, requestStarted, err := goldenSessionRequestWindow(session)
	if err != nil || request.RequestID == "" || request.Transport != session.transport ||
		!goldenResponseMethodMatchesTransport(request.Method, request.Transport) ||
		(request.Path != "/v1/responses" && request.Path != "/responses") {
		return nil, false, failGolden("local_egress_binding_invalid")
	}
	session.mu.Lock()
	localUpstreamID := session.localUpstreamSocket
	session.mu.Unlock()
	if len(localUpstreamID) != 64 {
		return nil, false, failGolden("local_egress_binding_invalid")
	}
	afterCaptured, afterErr := parseGoldenEvidenceTime(after.Timestamp)
	if afterErr != nil {
		return nil, false, afterErr
	}
	leases := goldenHostedLeaseRequests(leaseObserver.stats)
	if leaseBefore > len(leases) {
		return nil, false, nil
	}
	// A proxy can acquire leases for metadata requests before it acquires the
	// lease used by the response. Bind to the earliest new hosted lease that
	// starts at or after the observed response request. The process snapshot is
	// the upper bound, so a lease observed after it is still incomplete. More
	// than one eligible lease is ambiguous and must fail closed.
	var lease transportEvent
	var leaseStarted time.Time
	eligibleLeases := 0
	for _, candidate := range leases[leaseBefore:] {
		if candidate.Method != http.MethodPost || candidate.RequestID == "" || candidate.ConnectionID == "" {
			return nil, false, failGolden("local_egress_lease_binding_invalid")
		}
		candidateStarted, parseErr := parseGoldenEvidenceTime(candidate.Timestamp)
		if parseErr != nil {
			return nil, false, failGolden("local_egress_lease_binding_invalid")
		}
		if candidateStarted.Before(requestStarted) {
			continue
		}
		if candidateStarted.After(afterCaptured) {
			continue
		}
		eligibleLeases++
		if eligibleLeases > 1 {
			return nil, false, failGolden("local_egress_lease_binding_invalid")
		}
		if lease.RequestID == "" || candidateStarted.Before(leaseStarted) {
			lease, leaseStarted = candidate, candidateStarted
		}
	}
	if lease.RequestID == "" {
		return nil, false, nil
	}
	left := goldenRemoteSocketsByID(before)
	right := goldenRemoteSocketsByID(after)
	if len(left) != len(before.RemoteSocketIDs) || len(right) != len(after.RemoteSocketIDs) {
		return nil, false, failGolden("local_egress_destination_missing")
	}
	missing := 0
	for socketID := range left {
		if _, ok := right[socketID]; !ok {
			missing++
		}
	}
	added := make([]goldenRemoteSocket, 0, 1)
	for socketID, socket := range right {
		if _, ok := left[socketID]; !ok {
			added = append(added, socket)
		}
	}
	if missing != 0 && len(added) != 0 {
		return nil, false, failGolden("local_egress_socket_reconnected")
	}
	if missing != 0 {
		return nil, false, failGolden("local_egress_socket_disappeared")
	}
	if len(added) > 1 {
		return nil, false, failGolden("local_egress_unrelated_socket")
	}
	var socket goldenRemoteSocket
	if len(added) == 1 {
		socket = added[0]
	} else if request.Transport == "http" {
		var reuseErr error
		socket, reuseErr = r.reusableGoldenHTTPSocket(right, requestStarted)
		if reuseErr != nil {
			return nil, false, reuseErr
		}
	}
	if socket.SocketID == "" {
		return nil, false, nil
	}
	if len(socket.DestinationID) != 64 {
		return nil, false, failGolden("local_egress_destination_missing")
	}
	binding := &goldenLocalEgressBinding{
		SessionLabel: session.label, RequestID: request.RequestID,
		LeaseRequestID: lease.RequestID, LeaseConnectionID: lease.ConnectionID,
		DestinationID: socket.DestinationID, Transport: request.Transport,
		SocketID: socket.SocketID, LocalUpstreamID: localUpstreamID,
		leaseStats: leaseObserver.stats, session: session,
	}
	return binding, true, nil
}

func (r *goldenRunner) reusableGoldenHTTPSocket(
	sockets map[string]goldenRemoteSocket,
	requestStarted time.Time,
) (goldenRemoteSocket, error) {
	r.localEgressMu.Lock()
	bindings := append([]*goldenLocalEgressBinding(nil), r.localEgressBindings...)
	r.localEgressMu.Unlock()
	candidates := make(map[string]goldenRemoteSocket)
	blocked := make(map[string]bool)
	for _, binding := range bindings {
		if binding == nil {
			continue
		}
		socket, present := sockets[binding.SocketID]
		if !present {
			continue
		}
		if binding.Transport != "http" || binding.session == nil ||
			socket.DestinationID != binding.DestinationID || !sessionDone(binding.session) {
			blocked[socket.SocketID] = true
			continue
		}
		binding.session.mu.Lock()
		finishedAt := binding.session.finishedAt
		binding.session.mu.Unlock()
		if finishedAt.IsZero() || finishedAt.After(requestStarted) {
			blocked[socket.SocketID] = true
			continue
		}
		candidates[socket.SocketID] = socket
	}
	for socketID := range blocked {
		delete(candidates, socketID)
	}
	if len(candidates) > 1 {
		return goldenRemoteSocket{}, failGolden("local_egress_binding_invalid")
	}
	for _, socket := range candidates {
		return socket, nil
	}
	return goldenRemoteSocket{}, nil
}

func (r *goldenRunner) commitGoldenLocalEgressBinding(
	session *goldenSession,
	binding *goldenLocalEgressBinding,
	timestamp string,
) error {
	if err := validateGoldenLocalEgressBinding(session, binding); err != nil {
		return err
	}
	session.mu.Lock()
	session.localEgressSocket = binding.SocketID
	session.localEgressCorrelated = true
	session.localEgressBinding = binding
	session.mu.Unlock()
	r.localEgressMu.Lock()
	r.localEgressBindings = append(r.localEgressBindings, binding)
	r.localEgressMu.Unlock()
	_ = r.evidence.write(map[string]any{
		"kind": "local_egress_bound", "timestamp": timestamp,
		"label": session.label, "request_id": binding.RequestID,
		"lease_request_id": binding.LeaseRequestID, "transport": binding.Transport,
		"socket_id": binding.SocketID, "destination_id": binding.DestinationID,
	})
	return nil
}

func (r *goldenRunner) bindGoldenLocalEgress(
	session *goldenSession,
	leaseObserver *runningGoldenObserver,
	leaseBefore int,
	before, after goldenProcessEvidence,
) error {
	binding, complete, err := r.prepareGoldenLocalEgressBinding(session, leaseObserver, leaseBefore, before, after)
	if err != nil {
		return err
	}
	if !complete {
		return failGolden("local_egress_correlation_missing")
	}
	return r.commitGoldenLocalEgressBinding(session, binding, after.Timestamp)
}

func (r *goldenRunner) waitAndBindGoldenLocalEgress(
	ctx context.Context,
	session *goldenSession,
	leaseObserver *runningGoldenObserver,
	leaseBefore int,
	baseline goldenProcessEvidence,
	pid int,
	phase string,
) (goldenProcessEvidence, error) {
	waitCtx, cancel := context.WithTimeout(ctx, goldenLocalEgressBindTimeout)
	defer cancel()
	ticker := time.NewTicker(goldenProcessSampleInterval)
	defer ticker.Stop()
	var previous *goldenLocalEgressBinding
	for {
		if err := r.requireGoldenLocalDaemonTransportClean(); err != nil {
			return goldenProcessEvidence{}, err
		}
		evidence, err := captureProcessEvidence(phase, "local-daemon", pid)
		if err != nil {
			return goldenProcessEvidence{}, err
		}
		binding, complete, err := r.prepareGoldenLocalEgressBinding(
			session, leaseObserver, leaseBefore, baseline, evidence,
		)
		if err != nil {
			return goldenProcessEvidence{}, err
		}
		if complete {
			if previous != nil {
				if binding.SocketID != previous.SocketID || binding.DestinationID != previous.DestinationID {
					return goldenProcessEvidence{}, failGolden("local_egress_socket_reconnected")
				}
				if err := r.commitGoldenLocalEgressBinding(session, binding, evidence.Timestamp); err != nil {
					return goldenProcessEvidence{}, err
				}
				return evidence, nil
			}
			previous = binding
		} else if previous != nil {
			if _, present := goldenRemoteSocketsByID(evidence)[previous.SocketID]; !present {
				return goldenProcessEvidence{}, failGolden("local_egress_socket_disappeared")
			}
		}
		select {
		case <-ctx.Done():
			return goldenProcessEvidence{}, ctx.Err()
		case <-waitCtx.Done():
			return goldenProcessEvidence{}, failGolden("local_egress_correlation_missing")
		case <-ticker.C:
		}
	}
}

func validateGoldenLocalEgressBinding(session *goldenSession, binding *goldenLocalEgressBinding) error {
	if session == nil || binding == nil || binding.SessionLabel != session.label ||
		binding.Transport != session.transport || len(binding.SocketID) != 64 ||
		len(binding.DestinationID) != 64 || len(binding.LocalUpstreamID) != 64 ||
		binding.leaseStats == nil || binding.session != session {
		return failGolden("local_egress_binding_invalid")
	}
	request, _, err := goldenSessionRequestWindow(session)
	if err != nil || request.RequestID != binding.RequestID || request.Transport != binding.Transport ||
		!goldenResponseMethodMatchesTransport(request.Method, request.Transport) {
		return failGolden("local_egress_binding_invalid")
	}
	foundLease := false
	for _, lease := range goldenLeaseRequests(binding.leaseStats) {
		if lease.RequestID == binding.LeaseRequestID && lease.ConnectionID == binding.LeaseConnectionID &&
			lease.Method == http.MethodPost {
			foundLease = true
			break
		}
	}
	if !foundLease {
		return failGolden("local_egress_lease_binding_invalid")
	}
	return nil
}

func requireBoundLocalEgress(sessions []*goldenSession, evidence map[string]goldenProcessEvidence) error {
	daemon, ok := evidence["local-daemon"]
	if !ok {
		return failGolden("local_daemon_evidence_missing")
	}
	actual := goldenRemoteSocketsByID(daemon)
	if len(actual) != len(daemon.RemoteSocketIDs) {
		return failGolden("local_egress_destination_missing")
	}
	expected := make(map[string]string)
	for _, session := range sessions {
		if session == nil || session.route != "local-egress" {
			continue
		}
		session.mu.Lock()
		binding := session.localEgressBinding
		localUpstreamID := session.localUpstreamSocket
		localEgressSocket := session.localEgressSocket
		localEgressCorrelated := session.localEgressCorrelated
		session.mu.Unlock()
		if err := validateGoldenLocalEgressBinding(session, binding); err != nil {
			return err
		}
		if !localEgressCorrelated || localEgressSocket != binding.SocketID ||
			localUpstreamID != binding.LocalUpstreamID {
			return failGolden("local_egress_binding_invalid")
		}
		if _, duplicate := expected[binding.SocketID]; duplicate {
			return failGolden("local_egress_binding_invalid")
		}
		expected[binding.SocketID] = binding.DestinationID
	}
	missing, unrelated := 0, 0
	for socketID, destinationID := range expected {
		socket, present := actual[socketID]
		if !present {
			missing++
			continue
		}
		if socket.DestinationID != destinationID {
			return failGolden("local_egress_destination_changed")
		}
	}
	for socketID := range actual {
		if _, present := expected[socketID]; !present {
			unrelated++
		}
	}
	if missing != 0 && unrelated != 0 {
		return failGolden("local_egress_socket_reconnected")
	}
	if missing != 0 {
		return failGolden("local_egress_socket_disappeared")
	}
	if unrelated != 0 {
		return failGolden("local_egress_unrelated_socket")
	}
	if len(expected) == 0 {
		return failGolden("local_egress_continuity_missing")
	}
	return nil
}

func requireStableLocalEgress(before, after map[string]goldenProcessEvidence) error {
	left, leftOK := before["local-daemon"]
	right, rightOK := after["local-daemon"]
	if !leftOK || !rightOK || len(left.RemoteSocketIDs) == 0 || len(right.RemoteSocketIDs) == 0 {
		return failGolden("local_egress_continuity_missing")
	}
	seen := make(map[string]bool, len(left.RemoteSocketIDs))
	for _, id := range left.RemoteSocketIDs {
		seen[id] = true
	}
	remaining := make(map[string]bool, len(seen))
	for id := range seen {
		remaining[id] = true
	}
	unrelated := 0
	for _, id := range right.RemoteSocketIDs {
		if !seen[id] {
			unrelated++
			continue
		}
		delete(remaining, id)
	}
	missing := len(remaining)
	if missing != 0 && unrelated != 0 {
		return failGolden("local_egress_socket_reconnected")
	}
	if missing != 0 {
		return failGolden("local_egress_socket_disappeared")
	}
	if unrelated != 0 {
		return failGolden("local_egress_unrelated_socket")
	}
	return nil
}

type goldenLocalEgressMonitor struct {
	runner   *goldenRunner
	pid      int
	phase    string
	expected goldenProcessEvidence
	capture  func(string, string, int) (goldenProcessEvidence, error)
	stop     chan struct{}
	done     chan struct{}
	ready    chan struct{}
	stopOnce sync.Once

	mu          sync.Mutex
	liveErr     error
	samples     int
	lastStarted time.Time
	maxStartGap time.Duration
}

func validateGoldenLocalEgressSocketSet(expected, actual goldenProcessEvidence) error {
	want := goldenRemoteSocketsByID(expected)
	got := goldenRemoteSocketsByID(actual)
	if len(want) == 0 {
		return failGolden("local_egress_continuity_missing")
	}
	if len(want) != len(expected.RemoteSocketIDs) || len(got) != len(actual.RemoteSocketIDs) {
		return failGolden("local_egress_destination_missing")
	}
	missing, unrelated := 0, 0
	for socketID, expectedSocket := range want {
		actualSocket, present := got[socketID]
		if !present {
			missing++
			continue
		}
		if actualSocket.DestinationID != expectedSocket.DestinationID || actualSocket.LocalID != expectedSocket.LocalID {
			return failGolden("local_egress_destination_changed")
		}
	}
	for socketID := range got {
		if _, present := want[socketID]; !present {
			unrelated++
		}
	}
	if missing != 0 && unrelated != 0 {
		return failGolden("local_egress_socket_reconnected")
	}
	if missing != 0 {
		return failGolden("local_egress_socket_disappeared")
	}
	if unrelated != 0 {
		return failGolden("local_egress_unrelated_socket")
	}
	return nil
}

func startGoldenLocalEgressMonitor(
	ctx context.Context,
	runner *goldenRunner,
	pid int,
	phase string,
	expected goldenProcessEvidence,
) (*goldenLocalEgressMonitor, error) {
	capture := func(phase, label string, pid int) (goldenProcessEvidence, error) {
		return captureGoldenRemoteSocketEvidence(phase, label, pid, expected.DescendantPIDs)
	}
	return startGoldenLocalEgressMonitorWithCapture(ctx, runner, pid, phase, expected, capture)
}

func startGoldenLocalEgressMonitorWithCapture(
	ctx context.Context,
	runner *goldenRunner,
	pid int,
	phase string,
	expected goldenProcessEvidence,
	capture func(string, string, int) (goldenProcessEvidence, error),
) (*goldenLocalEgressMonitor, error) {
	if runner == nil || pid <= 0 || phase == "" || capture == nil {
		return nil, failGolden("local_egress_monitor_invalid")
	}
	if err := validateGoldenLocalEgressSocketSet(expected, expected); err != nil {
		return nil, err
	}
	monitor := &goldenLocalEgressMonitor{
		runner: runner, pid: pid, phase: phase, expected: expected, capture: capture,
		stop: make(chan struct{}), done: make(chan struct{}), ready: make(chan struct{}),
	}
	go monitor.run(ctx)
	select {
	case <-ctx.Done():
		_ = monitor.stopAndValidate()
		return nil, ctx.Err()
	case <-monitor.ready:
		if err := monitor.validate(); err != nil {
			_ = monitor.stopAndValidate()
			return nil, err
		}
		return monitor, nil
	}
}

func (monitor *goldenLocalEgressMonitor) run(ctx context.Context) {
	defer close(monitor.done)
	if !monitor.sample() {
		close(monitor.ready)
		return
	}
	close(monitor.ready)
	ticker := time.NewTicker(goldenLocalEgressMonitorInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			monitor.setError(ctx.Err())
			return
		case <-monitor.stop:
			monitor.sample()
			return
		case <-ticker.C:
			if !monitor.sample() {
				return
			}
		}
	}
}

func (monitor *goldenLocalEgressMonitor) sample() bool {
	started := time.Now().UTC()
	monitor.mu.Lock()
	if !monitor.lastStarted.IsZero() {
		gap := started.Sub(monitor.lastStarted)
		if gap > monitor.maxStartGap {
			monitor.maxStartGap = gap
		}
		if gap > goldenLocalEgressMaxGapForRun() {
			monitor.liveErr = failGolden("local_egress_monitor_gap")
			monitor.mu.Unlock()
			return false
		}
	}
	monitor.lastStarted = started
	monitor.mu.Unlock()
	evidence, err := monitor.capture(monitor.phase, "local-daemon", monitor.pid)
	if err == nil {
		err = validateGoldenLocalEgressSocketSet(monitor.expected, evidence)
	}
	if err != nil {
		monitor.setError(err)
		return false
	}
	monitor.mu.Lock()
	monitor.samples++
	monitor.mu.Unlock()
	monitor.runner.recordSamplingEvidence(map[string]any{
		"kind": "local_egress_continuity_sample", "timestamp": evidence.Timestamp,
		"phase": monitor.phase, "remote_socket_ids": evidence.RemoteSocketIDs,
	})
	return true
}

func (monitor *goldenLocalEgressMonitor) setError(err error) {
	monitor.mu.Lock()
	if monitor.liveErr == nil {
		monitor.liveErr = err
	}
	monitor.mu.Unlock()
}

func (monitor *goldenLocalEgressMonitor) validate() error {
	if monitor == nil {
		return failGolden("local_egress_monitor_invalid")
	}
	monitor.mu.Lock()
	defer monitor.mu.Unlock()
	return monitor.liveErr
}

func (monitor *goldenLocalEgressMonitor) stopAndValidate() error {
	if monitor == nil {
		return failGolden("local_egress_monitor_invalid")
	}
	monitor.stopOnce.Do(func() { close(monitor.stop) })
	<-monitor.done
	monitor.mu.Lock()
	liveErr := monitor.liveErr
	samples := monitor.samples
	maxStartGap := monitor.maxStartGap
	monitor.mu.Unlock()
	_ = monitor.runner.evidence.write(map[string]any{
		"kind": "local_egress_continuity_complete", "timestamp": time.Now().UTC().Format(time.RFC3339Nano),
		"phase": monitor.phase, "samples": samples, "max_start_gap_ms": maxStartGap.Milliseconds(),
		"passed": liveErr == nil,
	})
	if liveErr != nil {
		return liveErr
	}
	if samples < 2 {
		return failGolden("local_egress_monitor_incomplete")
	}
	return nil
}

func captureGoldenRemoteSocketEvidence(
	phase, label string,
	pid int,
	pids []int,
) (goldenProcessEvidence, error) {
	if pid <= 0 {
		return goldenProcessEvidence{}, failGolden("process_id_missing")
	}
	if len(pids) == 0 {
		pids = []int{pid}
	}
	sampleCtx, cancel := context.WithTimeout(context.Background(), goldenLocalEgressMaxGapForRun())
	defer cancel()
	remoteSockets := make([]goldenRemoteSocket, 0)
	for _, processID := range pids {
		output, err := goldenSocketSnapshot(sampleCtx, processID)
		if sampleCtx.Err() != nil {
			return goldenProcessEvidence{}, failGolden("local_egress_monitor_gap")
		}
		if err != nil {
			return goldenProcessEvidence{}, failGolden("socket_snapshot_failed")
		}
		for _, line := range strings.Split(string(output), "\n") {
			if len(line) < 2 || line[0] != 'n' {
				continue
			}
			if socket, ok := newGoldenRemoteSocket(strings.TrimSpace(line[1:])); ok {
				remoteSockets = append(remoteSockets, socket)
			}
		}
	}
	sort.Slice(remoteSockets, func(i, j int) bool { return remoteSockets[i].SocketID < remoteSockets[j].SocketID })
	remoteSockets = deduplicateGoldenRemoteSockets(remoteSockets)
	remoteIDs := make([]string, 0, len(remoteSockets))
	for _, socket := range remoteSockets {
		remoteIDs = append(remoteIDs, socket.SocketID)
	}
	return goldenProcessEvidence{
		Timestamp: time.Now().UTC().Format(time.RFC3339Nano), Phase: phase, Label: label,
		ProcessID: pid, DescendantPIDs: append([]int(nil), pids...),
		RemoteSocketIDs: remoteIDs, remoteSockets: remoteSockets,
	}, nil
}
