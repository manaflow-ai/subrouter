package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/manaflow-ai/subrouter/internal/front"
	"github.com/manaflow-ai/subrouter/session"
)

type canaryTestHost struct {
	s            *supervisor
	control      *http.Client
	public       string
	workerConfig string
	releaseState string
	initialID    string
}

// startCanaryTestHost runs a real supervisor over fake workers (the test
// binary) with a control socket, a public TCP listener, a worker config file
// and a release-state file.
func startCanaryTestHost(t *testing.T) *canaryTestHost {
	t.Helper()
	t.Setenv("SUBROUTER_TEST_FAKE_WORKER", "1")
	directory, err := os.MkdirTemp("/tmp", "subrouter-canary-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	host := &canaryTestHost{
		public:       reserveSupervisorAddress(t),
		workerConfig: filepath.Join(directory, "worker.json"),
		releaseState: filepath.Join(directory, "state", "release-state.json"),
	}
	config := supervisorConfig{
		Addr:            host.public,
		ControlSocket:   filepath.Join(directory, "control.sock"),
		WorkerBin:       os.Args[0],
		WorkerConfig:    host.workerConfig,
		ReleaseState:    host.releaseState,
		ReadyTimeout:    10 * time.Second,
		DrainTimeout:    50 * time.Millisecond,
		WorkerStopGrace: time.Second,
	}
	initial, err := startWorkerGeneration(config, generationInitial)
	if err != nil {
		t.Fatal(err)
	}
	host.initialID = initial.id
	router, err := front.NewRouter(front.Backend{ID: initial.id, Network: initial.network, Address: initial.address})
	if err != nil {
		terminateWorker(initial, time.Second)
		t.Fatal(err)
	}
	router.SetSessionKey(session.ExtractRoutingID)
	host.s = &supervisor{
		config:   config,
		router:   router,
		fatal:    make(chan error, 1),
		retireCh: make(chan struct{}),
		workers:  map[string]*workerGeneration{initial.id: initial},
	}
	go host.s.monitorWorker(initial)
	runDone := make(chan error, 1)
	go func() { runDone <- host.s.run() }()
	t.Cleanup(func() {
		// Stop run the way a fatal error does: it closes the listeners,
		// unregisters its signal handler and signals every worker.
		host.s.fatal <- fmt.Errorf("test finished")
		select {
		case <-runDone:
		case <-time.After(10 * time.Second):
			t.Error("supervisor did not stop")
		}
		host.s.workersMu.Lock()
		workers := make([]*workerGeneration, 0, len(host.s.workers))
		for _, worker := range host.s.workers {
			workers = append(workers, worker)
		}
		host.s.workersMu.Unlock()
		for _, worker := range workers {
			terminateWorker(worker, time.Second)
		}
	})
	host.control = unixHTTPClient(config.ControlSocket)
	host.control.Timeout = 20 * time.Second
	waitForSupervisorStatus(t, host.control, runDone)
	return host
}

func (h *canaryTestHost) post(t *testing.T, path string) (int, string) {
	t.Helper()
	response, err := h.control.Post("http://supervisor"+path, "application/json", nil)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	return response.StatusCode, string(body)
}

func (h *canaryTestHost) status(t *testing.T) canaryStatus {
	t.Helper()
	response, err := h.control.Get("http://supervisor/_subrouter/canary")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var status canaryStatus
	if err := json.NewDecoder(response.Body).Decode(&status); err != nil {
		t.Fatal(err)
	}
	return status
}

// workerPID sends one request for session on a fresh public connection and
// returns the pid of the worker that served it.
func (h *canaryTestHost) workerPID(t *testing.T, session string) (string, int) {
	t.Helper()
	request, _ := http.NewRequest(http.MethodGet, "http://"+h.public+"/", nil)
	if session != "" {
		request.Header.Set("X-Claude-Code-Session-Id", session)
	}
	client := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("public request: %v", err)
	}
	_, _ = io.Copy(io.Discard, response.Body)
	_ = response.Body.Close()
	return response.Header.Get("X-Fake-Worker-Pid"), response.StatusCode
}

func (h *canaryTestHost) pidOf(t *testing.T, id string) string {
	t.Helper()
	h.s.workersMu.Lock()
	defer h.s.workersMu.Unlock()
	worker := h.s.workers[id]
	if worker == nil {
		t.Fatalf("generation %s is not supervised", id)
	}
	return fmt.Sprint(worker.command.Process.Pid)
}

func (h *canaryTestHost) waitGenerationGone(t *testing.T, id string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		h.s.workersMu.Lock()
		_, present := h.s.workers[id]
		h.s.workersMu.Unlock()
		if !present {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("generation %s was not drained and stopped", id)
}

func (h *canaryTestHost) readReleaseState(t *testing.T) map[string]any {
	t.Helper()
	data, err := os.ReadFile(h.releaseState)
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := json.Unmarshal(data, &document); err != nil {
		t.Fatal(err)
	}
	return document
}

// holdOnPublic opens an in-flight /hold request for session and returns a
// function that completes it and returns the response body and pid.
func holdOnPublic(t *testing.T, address, session string) func() (string, string) {
	t.Helper()
	connection, err := net.DialTimeout("tcp", address, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = connection.Close() })
	if _, err := fmt.Fprintf(connection,
		"POST /hold HTTP/1.1\r\nHost: worker\r\nX-Claude-Code-Session-Id: %s\r\nTransfer-Encoding: chunked\r\nExpect: 100-continue\r\n\r\n", session); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(connection)
	_ = connection.SetDeadline(time.Now().Add(10 * time.Second))
	line, err := reader.ReadString('\n')
	if err != nil || !strings.Contains(line, "100 Continue") {
		t.Fatalf("hold did not start: %q %v", line, err)
	}
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if line == "\r\n" {
			break
		}
	}
	if _, err := io.WriteString(connection, "1\r\nx\r\n"); err != nil {
		t.Fatal(err)
	}
	return func() (string, string) {
		if _, err := io.WriteString(connection, "0\r\n\r\n"); err != nil {
			t.Fatalf("in-flight request was cut: %v", err)
		}
		response, err := http.ReadResponse(reader, &http.Request{Method: http.MethodPost})
		if err != nil {
			t.Fatalf("in-flight request was cut: %v", err)
		}
		body, _ := io.ReadAll(response.Body)
		_ = response.Body.Close()
		// The client hangs up, as a real one does after Connection: close.
		_ = connection.Close()
		return string(body), response.Header.Get("X-Fake-Worker-Pid")
	}
}

func TestSupervisorCanaryAbortLeavesIncumbentServing(t *testing.T) {
	if retireSignal == nil {
		t.Skip("worker retirement signal is unavailable")
	}
	host := startCanaryTestHost(t)
	incumbentPID := host.pidOf(t, host.initialID)

	code, body := host.post(t, "/_subrouter/canary/start?weight=100")
	if code != http.StatusOK {
		t.Fatalf("start = %d %s", code, body)
	}
	status := host.status(t)
	if status.State != "canary" || status.Weight != 100 || status.Candidate == nil || status.Candidate.Traffic == nil || status.Incumbent.Traffic == nil {
		t.Fatalf("status after start = %+v", status)
	}
	candidateID := status.Candidate.ID
	candidatePID := host.pidOf(t, candidateID)
	if pid, _ := host.workerPID(t, "s-1"); pid != candidatePID {
		t.Fatalf("new session at weight 100 served by %s, want candidate %s", pid, candidatePID)
	}
	if state := host.readReleaseState(t); state["state"] != "canary" || state["weight"] != float64(100) {
		t.Fatalf("release state after start = %v", state)
	}
	finish := holdOnPublic(t, host.public, "s-1")

	if code, body := host.post(t, "/_subrouter/canary/abort?reason=manual+test"); code != http.StatusOK {
		t.Fatalf("abort = %d %s", code, body)
	}
	for _, session := range []string{"s-1", "s-2", ""} {
		if pid, code := host.workerPID(t, session); pid != incumbentPID || code != http.StatusOK {
			t.Fatalf("session %q after abort served by %s (%d), want incumbent %s", session, pid, code, incumbentPID)
		}
	}
	if got, pid := finish(); got != "released" || pid != candidatePID {
		t.Fatalf("in-flight candidate request finished with %q from %s", got, pid)
	}
	host.waitGenerationGone(t, candidateID)
	if host.s.router.Active().ID != host.initialID {
		t.Fatal("abort changed the active generation")
	}
	status = host.status(t)
	if status.State != "idle" || status.Last == nil || status.Last.State != "aborted" || status.Last.Weight != 100 {
		t.Fatalf("status after abort = %+v", status)
	}
	if state := host.readReleaseState(t); state["state"] != "aborted" || state["reason"] != "manual test" || state["weight"] != float64(100) {
		t.Fatalf("release state after abort = %v", state)
	}
	if code, _ := host.post(t, "/_subrouter/canary/abort"); code != http.StatusConflict {
		t.Fatalf("second abort = %d, want 409", code)
	}
}

func TestSupervisorCanaryPromoteDrainsIncumbent(t *testing.T) {
	if retireSignal == nil {
		t.Skip("worker retirement signal is unavailable")
	}
	host := startCanaryTestHost(t)
	incumbentPID := host.pidOf(t, host.initialID)
	if code, body := host.post(t, "/_subrouter/canary/start?weight=0"); code != http.StatusOK {
		t.Fatalf("start = %d %s", code, body)
	}
	candidateID := host.status(t).Candidate.ID
	candidatePID := host.pidOf(t, candidateID)
	if pid, _ := host.workerPID(t, "old"); pid != incumbentPID {
		t.Fatalf("weight 0 served by %s", pid)
	}
	finish := holdOnPublic(t, host.public, "old")
	if code, body := host.post(t, "/_subrouter/canary/weight?weight=5"); code != http.StatusOK || !strings.Contains(body, `"weight":5`) {
		t.Fatalf("weight = %d %s", code, body)
	}
	if code, body := host.post(t, "/_subrouter/canary/promote"); code != http.StatusOK {
		t.Fatalf("promote = %d %s", code, body)
	}
	if host.s.router.Active().ID != candidateID {
		t.Fatal("promote did not make the candidate active")
	}
	for _, session := range []string{"old", "new", ""} {
		if pid, _ := host.workerPID(t, session); pid != candidatePID {
			t.Fatalf("session %q after promote served by %s, want %s", session, pid, candidatePID)
		}
	}
	if got, pid := finish(); got != "released" || pid != incumbentPID {
		t.Fatalf("in-flight incumbent request finished with %q from %s", got, pid)
	}
	host.waitGenerationGone(t, host.initialID)
	if state := host.readReleaseState(t); state["state"] != "promoted" {
		t.Fatalf("release state after promote = %v", state)
	}
}

func TestSupervisorCanaryStepperAbortsRegressedCandidate(t *testing.T) {
	host := startCanaryTestHost(t)
	incumbentPID := host.pidOf(t, host.initialID)
	// The candidate generation reads this config; the incumbent already runs.
	if err := os.WriteFile(host.workerConfig, []byte(`{"args": [], "env": {"SUBROUTER_TEST_FAKE_WORKER_FAIL": "1"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, body := host.post(t, "/_subrouter/canary/start?steps=100&dwell=1m"); code != http.StatusOK {
		t.Fatalf("start = %d %s", code, body)
	}
	for i := 0; i < 60; i++ {
		if _, code := host.workerPID(t, fmt.Sprintf("regressed-%d", i)); code != http.StatusBadGateway {
			t.Fatalf("request %d = %d, want the failing candidate's 502", i, code)
		}
	}
	deadline := time.Now().Add(20 * time.Second)
	var status canaryStatus
	for time.Now().Before(deadline) {
		if status = host.status(t); status.State == "idle" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if status.Last == nil || status.Last.State != "aborted" || !strings.Contains(status.Last.Reason, "proxy 5xx 100.0% vs 0.0%") {
		t.Fatalf("stepper did not abort the regressed candidate: %+v", status)
	}
	if !strings.HasPrefix(status.Release, "fake-") || !strings.Contains(status.Release, "aborted at 100%: proxy 5xx") {
		t.Fatalf("release line = %q", status.Release)
	}
	if pid, code := host.workerPID(t, "regressed-1"); pid != incumbentPID || code != http.StatusOK {
		t.Fatalf("after abort served by %s (%d), want incumbent %s", pid, code, incumbentPID)
	}
}

// An operator pause must win over a stepper whose gate check was already in
// flight: the check read traffic before the pause, and its promote or next
// step lands after it.
func TestOperatorPauseStopsStepperWithCheckInFlight(t *testing.T) {
	host := startCanaryTestHost(t)
	if code, body := host.post(t, "/_subrouter/canary/start?weight=5"); code != http.StatusOK {
		t.Fatalf("start = %d %s", code, body)
	}
	// Attach a stepper driver exactly as startCanary does for steps.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	host.s.upgradeMu.Lock()
	rollout := host.s.canary
	rollout.cancel = cancel
	rollout.steps = []int{5, 25, 100}
	host.s.upgradeMu.Unlock()
	driver := supervisorRolloutDriver{s: host.s, rollout: rollout, ctx: ctx}

	if _, ok := driver.observe(); !ok {
		t.Fatal("stepper does not own its rollout before the pause")
	}
	// The operator pauses while that check's decision is still pending.
	if code, body := host.post(t, "/_subrouter/canary/weight?weight=0"); code != http.StatusOK {
		t.Fatalf("pause = %d %s", code, body)
	}
	if err := driver.setWeight(25, 1); !errors.Is(err, errStepperSuperseded) {
		t.Fatalf("stepper set the next step after a pause: %v", err)
	}
	if err := driver.promote("passed 3 steps"); !errors.Is(err, errStepperSuperseded) {
		t.Fatalf("stepper promoted after a pause: %v", err)
	}
	if err := driver.abort("late regression"); !errors.Is(err, errStepperSuperseded) {
		t.Fatalf("stepper aborted after a pause: %v", err)
	}
	driver.record(1, gateDecision{Action: gatePromote, Reason: "stale"})
	if _, ok := driver.observe(); ok {
		t.Fatal("stopped stepper still observes")
	}
	status := host.status(t)
	if status.State != "canary" || status.Weight != 0 || status.Gate != nil || len(status.Steps) != 0 {
		t.Fatalf("status after pause = %+v", status)
	}
	if _, weight, running := host.s.router.Canary(); !running || weight != 0 {
		t.Fatalf("router canary = %t at %d, want running at 0", running, weight)
	}
	if host.s.router.Active().ID != host.initialID {
		t.Fatal("the incumbent stopped being active")
	}
	// The operator still controls the rollout.
	if code, body := host.post(t, "/_subrouter/canary/abort"); code != http.StatusOK {
		t.Fatalf("operator abort = %d %s", code, body)
	}
}

func TestPlainUpgradeKeepsItsBehavior(t *testing.T) {
	host := startCanaryTestHost(t)
	code, body := host.post(t, "/_subrouter/upgrade")
	if code != http.StatusOK {
		t.Fatalf("upgrade = %d %s", code, body)
	}
	var response struct {
		Active front.Backend `json:"active"`
	}
	if err := json.Unmarshal([]byte(body), &response); err != nil || response.Active.ID == "" || response.Active.ID == host.initialID {
		t.Fatalf("upgrade response = %s (%v)", body, err)
	}
	host.waitGenerationGone(t, host.initialID)
	upgraded := response.Active.ID

	// During a canary a plain upgrade still means one new generation from
	// the binary on disk: it ends the canary instead of failing, so the
	// deploy scripts and the guard work unchanged.
	if code, body := host.post(t, "/_subrouter/canary/start?steps=default"); code != http.StatusOK {
		t.Fatalf("start = %d %s", code, body)
	}
	candidateID := host.status(t).Candidate.ID
	code, body = host.post(t, "/_subrouter/upgrade")
	if code != http.StatusOK {
		t.Fatalf("upgrade during canary = %d %s", code, body)
	}
	if err := json.Unmarshal([]byte(body), &response); err != nil {
		t.Fatal(err)
	}
	if response.Active.ID == upgraded || response.Active.ID == candidateID {
		t.Fatalf("upgrade during canary made %s active", response.Active.ID)
	}
	status := host.status(t)
	if status.State != "idle" || status.Last == nil || status.Last.Reason != "superseded by a plain upgrade" {
		t.Fatalf("status after upgrade = %+v", status)
	}
	host.waitGenerationGone(t, candidateID)
	host.waitGenerationGone(t, upgraded)
	if backends := host.s.router.Status(); len(backends) != 1 {
		t.Fatalf("backends after upgrade = %+v", backends)
	}
}

func TestCanaryStartValidatesParameters(t *testing.T) {
	host := startCanaryTestHost(t)
	for _, path := range []string{
		"/_subrouter/canary/start",
		"/_subrouter/canary/start?weight=101",
		"/_subrouter/canary/start?steps=5,25&weight=25",
		"/_subrouter/canary/start?weight=5&dwell=1m",
		"/_subrouter/canary/start?steps=default&dwell=-1m",
	} {
		if code, _ := host.post(t, path); code != http.StatusBadRequest {
			t.Errorf("%s = %d, want 400", path, code)
		}
	}
	for _, path := range []string{"/_subrouter/canary/weight?weight=5", "/_subrouter/canary/promote"} {
		if code, _ := host.post(t, path); code != http.StatusConflict {
			t.Errorf("%s without a canary = %d, want 409", path, code)
		}
	}
	if len(host.s.router.Status()) != 1 {
		t.Fatal("a rejected start left a generation behind")
	}
}

func TestWriteRolloutStateKeepsForeignFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "release-state.json")
	if err := os.WriteFile(path, []byte(`{"state":"baking","bake_until":"2026-01-01T00:00:00Z","baseline":{"requests":7}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	if err := writeRolloutState(path, "v0.1.141", "v0.1.140", "canary", 25, "step 2 of 3", now.Add(-9*time.Minute), now); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	var document map[string]any
	if err := json.Unmarshal(data, &document); err != nil {
		t.Fatal(err)
	}
	if document["state"] != "canary" || document["weight"] != float64(25) || document["since"] != "2026-09-27T11:51:00Z" {
		t.Fatalf("state = %v", document)
	}
	if _, ok := document["bake_until"]; ok {
		t.Fatal("bake_until survived a canary write")
	}
	if baseline, _ := document["baseline"].(map[string]any); baseline["requests"] != float64(7) {
		t.Fatalf("foreign field lost: %v", document)
	}
	var view releaseStateView
	_ = json.Unmarshal(data, &view)
	if got := releaseStatusText(&view, now); got != "v0.1.141 canary 25% (9m)" {
		t.Fatalf("release line = %q", got)
	}
}

// An incumbent that dies mid-rollout is replaced from --worker-bin, which is
// the candidate binary. The rollout must end as aborted so the host scripts
// put last-good back and switch to it.
func TestSupervisorCanaryIncumbentExitAbortsRollout(t *testing.T) {
	host := startCanaryTestHost(t)
	if code, body := host.post(t, "/_subrouter/canary/start?weight=5"); code != http.StatusOK {
		t.Fatalf("start = %d %s", code, body)
	}
	host.s.workersMu.Lock()
	incumbent := host.s.workers[host.initialID]
	host.s.workersMu.Unlock()
	if err := incumbent.command.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		status := host.status(t)
		if status.State == "idle" && status.Last != nil && host.s.router.Active().ID != host.initialID {
			if status.Last.State != "aborted" || !strings.Contains(status.Last.Reason, "incumbent worker exited during the rollout") {
				t.Fatalf("last rollout = %+v", status.Last)
			}
			if state := host.readReleaseState(t); state["state"] != "aborted" {
				t.Fatalf("release state = %v", state)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("status after the incumbent died = %+v", host.status(t))
}
