package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/manaflow-ai/subrouter/internal/front"
	"github.com/manaflow-ai/subrouter/internal/proxy"
)

// A canary rollout runs a candidate worker generation beside the incumbent
// and sends a weighted share of new sessions to it (see internal/front's
// canary.go for how sessions are kept on one generation). It is driven over
// the control socket:
//
//	POST /_subrouter/canary/start?weight=5              manual rollout
//	POST /_subrouter/canary/start?steps=default         5% -> 25% -> 100%, 10m each, gated
//	POST /_subrouter/canary/start?steps=5,50,100&dwell=20m
//	POST /_subrouter/canary/weight?weight=25            takes over from the stepper
//	POST /_subrouter/canary/promote                     candidate becomes the sole generation
//	POST /_subrouter/canary/abort[?reason=...]          weight 0, drain and stop the candidate
//	GET  /_subrouter/canary                             state, gate, both generations' traffic
//
// The candidate runs the binary at --worker-bin, exactly as an upgrade would.

var errCanaryConflict = errors.New("canary conflict")

type canaryRollout struct {
	candidate *workerGeneration
	startedAt time.Time
	stepSince time.Time
	weight    int
	steps     []int
	step      int
	dwell     time.Duration
	cancel    context.CancelFunc
	version   string
	previous  string
	baseID    string
	base      trafficWindow
	gate      *gateDecision
	gateAt    time.Time
	// healthFailures counts consecutive failed readiness checks of the
	// candidate, and healthError is the last one's error.
	healthFailures int
	healthError    string
}

type rolloutOutcome struct {
	State     string    `json:"state"`
	Candidate string    `json:"candidate"`
	Version   string    `json:"version,omitempty"`
	Weight    int       `json:"weight"`
	Reason    string    `json:"reason"`
	At        time.Time `json:"at"`
}

func (s *supervisor) now() time.Time {
	if s.clock != nil {
		return s.clock.Now()
	}
	return time.Now()
}

// generationTraffic reads one generation's /_subrouter/traffic.
func (s *supervisor) generationTraffic(id string) (*proxy.TrafficSnapshot, error) {
	s.workersMu.Lock()
	worker := s.workers[id]
	s.workersMu.Unlock()
	if worker == nil {
		return nil, fmt.Errorf("generation %q is not supervised", id)
	}
	transport := workerTransport(worker)
	defer transport.CloseIdleConnections()
	client := &http.Client{Timeout: 3 * time.Second, Transport: transport}
	response, err := client.Get("http://subrouter-worker/_subrouter/traffic")
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("traffic returned status %d", response.StatusCode)
	}
	var snapshot proxy.TrafficSnapshot
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&snapshot); err != nil {
		return nil, fmt.Errorf("decode traffic: %w", err)
	}
	return &snapshot, nil
}

// generationReady checks one generation's /_subrouter/ready, the probe the
// supervisor gates every new generation on.
func (s *supervisor) generationReady(id string) error {
	s.workersMu.Lock()
	worker := s.workers[id]
	s.workersMu.Unlock()
	if worker == nil {
		return fmt.Errorf("generation %q is not supervised", id)
	}
	transport := workerTransport(worker)
	defer transport.CloseIdleConnections()
	client := &http.Client{Timeout: 3 * time.Second, Transport: transport}
	response, err := client.Get("http://subrouter-worker/_subrouter/ready")
	if err != nil {
		return err
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4<<10))
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("ready returned status %d", response.StatusCode)
	}
	return nil
}

func snapshotVersion(snapshot *proxy.TrafficSnapshot) string {
	if snapshot == nil {
		return ""
	}
	return snapshot.Version
}

// startCanary starts a candidate generation at weight. With steps, the
// stepper then drives it through every step and promotes or aborts it.
func (s *supervisor) startCanary(weight int, steps []int, dwell time.Duration) error {
	s.upgradeMu.Lock()
	defer s.upgradeMu.Unlock()
	if err := s.generationChangeAllowed(); err != nil {
		return err
	}
	if s.canary != nil {
		return fmt.Errorf("%w: canary %s is already running; promote or abort it first", errCanaryConflict, s.canary.candidate.id)
	}
	next, err := startWorkerGeneration(s.config, generationReplacement)
	if err != nil {
		return err
	}
	s.workersMu.Lock()
	s.workers[next.id] = next
	s.workersMu.Unlock()
	incumbent := s.router.Active()
	incumbentTraffic, _ := s.generationTraffic(incumbent.ID)
	candidateTraffic, _ := s.generationTraffic(next.id)
	if err := s.router.StartCanary(front.Backend{ID: next.id, Network: next.network, Address: next.address}, weight); err != nil {
		terminateWorker(next, s.config.WorkerStopGrace)
		s.workersMu.Lock()
		delete(s.workers, next.id)
		s.workersMu.Unlock()
		return err
	}
	now := s.now()
	rollout := &canaryRollout{
		candidate: next,
		startedAt: now,
		stepSince: now,
		weight:    weight,
		steps:     steps,
		dwell:     dwell,
		version:   snapshotVersion(candidateTraffic),
		previous:  snapshotVersion(incumbentTraffic),
		baseID:    incumbent.ID,
	}
	if incumbentTraffic != nil {
		rollout.base = trafficWindowOf(*incumbentTraffic)
	}
	s.canary = rollout
	go s.monitorWorker(next)
	slog.Info("subrouter canary started", "candidate", next.id, "incumbent", incumbent.ID, "weight", weight, "steps", steps, "dwell", dwell)
	s.writeRolloutStateLocked(rollout, "canary", weight, canaryStepReason(rollout))
	if len(steps) > 0 {
		ctx, cancel := context.WithCancel(context.Background())
		rollout.cancel = cancel
		clock := s.clock
		if clock == nil {
			clock = realRolloutClock{}
		}
		go canaryStepper{
			driver:     supervisorRolloutDriver{s: s, rollout: rollout, ctx: ctx},
			steps:      steps,
			dwell:      dwell,
			tick:       canaryTick(dwell),
			clock:      clock,
			thresholds: processGateThresholds(),
		}.run(ctx)
	}
	return nil
}

func canaryStepReason(rollout *canaryRollout) string {
	if len(rollout.steps) == 0 {
		return "manual canary"
	}
	return fmt.Sprintf("step %d of %d (%s)", rollout.step+1, len(rollout.steps), joinWeights(rollout.steps))
}

func joinWeights(steps []int) string {
	parts := make([]string, len(steps))
	for i, step := range steps {
		parts[i] = strconv.Itoa(step) + "%"
	}
	return strings.Join(parts, " -> ")
}

func (s *supervisor) stopStepperLocked() {
	if s.canary != nil && s.canary.cancel != nil {
		s.canary.cancel()
		s.canary.cancel = nil
	}
}

// setCanaryWeight is the operator's weight change. It stops the stepper:
// once someone sets a weight by hand, the rollout is theirs.
func (s *supervisor) setCanaryWeight(weight int) error {
	s.upgradeMu.Lock()
	defer s.upgradeMu.Unlock()
	if s.canary == nil {
		return fmt.Errorf("%w: no canary is running", errCanaryConflict)
	}
	s.stopStepperLocked()
	s.canary.steps = nil
	return s.setCanaryWeightLocked(s.canary, weight)
}

func (s *supervisor) setCanaryWeightLocked(rollout *canaryRollout, weight int) error {
	if err := s.router.SetCanaryWeight(weight); err != nil {
		return err
	}
	rollout.weight = weight
	rollout.stepSince = s.now()
	slog.Info("subrouter canary weight", "candidate", rollout.candidate.id, "weight", weight)
	s.writeRolloutStateLocked(rollout, "canary", weight, canaryStepReason(rollout))
	return nil
}

func (s *supervisor) promoteCanary(reason string) error {
	s.upgradeMu.Lock()
	defer s.upgradeMu.Unlock()
	return s.promoteCanaryLocked(s.canary, reason)
}

// promoteCanaryLocked makes the candidate the sole generation and drains the
// incumbent exactly as an upgrade drains the generation it replaces.
func (s *supervisor) promoteCanaryLocked(rollout *canaryRollout, reason string) error {
	if rollout == nil || s.canary != rollout {
		return fmt.Errorf("%w: no canary is running", errCanaryConflict)
	}
	if err := s.generationChangeAllowed(); err != nil {
		return err
	}
	previous, err := s.router.PromoteCanary()
	if err != nil {
		return err
	}
	s.stopStepperLocked()
	s.canary = nil
	s.lastRollout = &rolloutOutcome{State: "promoted", Candidate: rollout.candidate.id, Version: rollout.version, Weight: 100, Reason: reason, At: s.now()}
	slog.Info("subrouter canary promoted", "from", previous.ID, "to", rollout.candidate.id, "reason", reason)
	s.writeRolloutStateLocked(rollout, "promoted", 100, reason)
	go s.reapWhenIdle(previous.ID)
	return nil
}

func (s *supervisor) abortCanary(reason string) error {
	s.upgradeMu.Lock()
	defer s.upgradeMu.Unlock()
	if s.canary == nil {
		return fmt.Errorf("%w: no canary is running", errCanaryConflict)
	}
	s.abortCanaryLocked(reason)
	return nil
}

// abortCanaryLocked stops sending anything new to the candidate, then drains
// and stops it. The incumbent is untouched. Abort is always allowed, even
// under an inhibit marker, because it only ever removes the candidate.
func (s *supervisor) abortCanaryLocked(reason string) {
	rollout := s.canary
	if rollout == nil {
		return
	}
	candidate, err := s.router.AbortCanary()
	if err != nil {
		slog.Warn("subrouter canary abort", "error", err)
	}
	s.stopStepperLocked()
	s.canary = nil
	s.lastRollout = &rolloutOutcome{State: "aborted", Candidate: rollout.candidate.id, Version: rollout.version, Weight: rollout.weight, Reason: reason, At: s.now()}
	slog.Warn("subrouter canary aborted", "candidate", rollout.candidate.id, "weight", rollout.weight, "reason", reason)
	s.writeRolloutStateLocked(rollout, "aborted", rollout.weight, reason)
	if candidate.ID != "" {
		go s.reapWhenIdle(candidate.ID)
	}
}

// supervisorRolloutDriver binds a stepper to the rollout it was started for
// and to its own context. stopStepperLocked cancels that context under
// upgradeMu, and every mutation below checks it under upgradeMu, so once an
// operator changes the weight, promotes or aborts, nothing the stepper had in
// flight (a traffic read can take seconds) can act on the rollout again.
type supervisorRolloutDriver struct {
	s       *supervisor
	rollout *canaryRollout
	ctx     context.Context
}

var errStepperSuperseded = errors.New("canary stepper was stopped or its rollout is over")

// ownsLocked reports whether this stepper may still act. Call with upgradeMu.
func (d supervisorRolloutDriver) ownsLocked() bool {
	return d.ctx.Err() == nil && d.s.canary == d.rollout
}

func (d supervisorRolloutDriver) observe() (gateInput, bool) {
	d.s.upgradeMu.Lock()
	if !d.ownsLocked() {
		d.s.upgradeMu.Unlock()
		return gateInput{}, false
	}
	candidateID := d.rollout.candidate.id
	baseID, base := d.rollout.baseID, d.rollout.base
	d.s.upgradeMu.Unlock()

	var input gateInput
	readyErr := d.s.generationReady(candidateID)
	d.s.upgradeMu.Lock()
	if d.ownsLocked() {
		if readyErr != nil {
			d.rollout.healthFailures++
			d.rollout.healthError = readyErr.Error()
		} else {
			d.rollout.healthFailures = 0
			d.rollout.healthError = ""
		}
		input.CandidateHealthFailures = d.rollout.healthFailures
		input.CandidateHealthError = d.rollout.healthError
	}
	d.s.upgradeMu.Unlock()
	if snapshot, err := d.s.generationTraffic(candidateID); err == nil {
		window := trafficWindowOf(*snapshot)
		input.Candidate = &window
	}
	incumbentID := d.s.router.Active().ID
	if snapshot, err := d.s.generationTraffic(incumbentID); err == nil {
		window := trafficWindowOf(*snapshot)
		if incumbentID == baseID {
			window = window.since(base)
		}
		input.Incumbent = &window
	}
	return input, true
}

func (d supervisorRolloutDriver) setWeight(weight, step int) error {
	d.s.upgradeMu.Lock()
	defer d.s.upgradeMu.Unlock()
	if !d.ownsLocked() {
		return errStepperSuperseded
	}
	d.rollout.step = step
	return d.s.setCanaryWeightLocked(d.rollout, weight)
}

func (d supervisorRolloutDriver) record(_ int, decision gateDecision) {
	d.s.upgradeMu.Lock()
	defer d.s.upgradeMu.Unlock()
	if !d.ownsLocked() {
		return
	}
	d.rollout.gate = &decision
	d.rollout.gateAt = d.s.now()
}

func (d supervisorRolloutDriver) promote(reason string) error {
	d.s.upgradeMu.Lock()
	defer d.s.upgradeMu.Unlock()
	if !d.ownsLocked() {
		return errStepperSuperseded
	}
	return d.s.promoteCanaryLocked(d.rollout, reason)
}

func (d supervisorRolloutDriver) abort(reason string) error {
	d.s.upgradeMu.Lock()
	defer d.s.upgradeMu.Unlock()
	if !d.ownsLocked() {
		return errStepperSuperseded
	}
	d.s.abortCanaryLocked(reason)
	return nil
}

// releaseStatePath is --release-state, else the worker config's
// SUBROUTER_RELEASE_STATE, so a host configured only for the worker still
// reports rollouts where the worker reads them.
func (s *supervisor) releaseStatePath() string {
	path := strings.TrimSpace(s.config.ReleaseState)
	if path == "" {
		if _, env, err := resolveWorkerLaunch(s.config); err == nil {
			path = strings.TrimSpace(env["SUBROUTER_RELEASE_STATE"])
		}
	}
	if path == "" || !filepath.IsAbs(path) {
		return ""
	}
	return path
}

// writeRolloutStateLocked records the rollout in release-state.json, the
// file the deploy scripts' bake gate writes and the worker reports as
// "release" in /_subrouter/health. It keeps the schema: version,
// previous_version, state, reason, since, plus weight. Fields it does not
// own are kept. The guard's bake only acts on state "baking", so a canary
// state never triggers it.
func (s *supervisor) writeRolloutStateLocked(rollout *canaryRollout, state string, weight int, reason string) {
	path := s.releaseStatePath()
	if path == "" {
		return
	}
	if err := writeRolloutState(path, rollout.version, rollout.previous, state, weight, reason, rollout.stepSince, s.now()); err != nil {
		slog.Warn("subrouter canary state not recorded", "path", path, "error", err)
	}
}

func writeRolloutState(path, version, previous, state string, weight int, reason string, stepSince, now time.Time) error {
	document := map[string]any{}
	if data, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(data, &document)
		if document == nil {
			document = map[string]any{}
		}
	}
	since := now
	if state == "canary" {
		since = stepSince
	}
	document["version"] = version
	document["previous_version"] = previous
	document["state"] = state
	document["reason"] = reason
	document["since"] = since.UTC().Format(time.RFC3339)
	document["weight"] = weight
	delete(document, "bake_until")
	data, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		return err
	}
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(directory, ".release-state.")
	if err != nil {
		return err
	}
	defer os.Remove(temporary.Name())
	if _, err := temporary.Write(append(data, '\n')); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Chmod(0o644); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporary.Name(), path)
}

type canaryGenerationStatus struct {
	ID           string                 `json:"id"`
	PID          int                    `json:"pid,omitempty"`
	Version      string                 `json:"version,omitempty"`
	Connections  int                    `json:"connections"`
	Traffic      *proxy.TrafficSnapshot `json:"traffic,omitempty"`
	TrafficError string                 `json:"traffic_error,omitempty"`
}

type canaryStatus struct {
	State         string                  `json:"state"`
	Weight        int                     `json:"weight"`
	Steps         []int                   `json:"steps,omitempty"`
	Step          int                     `json:"step,omitempty"`
	DwellSeconds  int64                   `json:"dwell_seconds,omitempty"`
	StartedAt     string                  `json:"started_at,omitempty"`
	StepSince     string                  `json:"step_since,omitempty"`
	Release       string                  `json:"release,omitempty"`
	Incumbent     *canaryGenerationStatus `json:"incumbent"`
	Candidate     *canaryGenerationStatus `json:"candidate,omitempty"`
	Gate          *gateDecision           `json:"gate,omitempty"`
	GateCheckedAt string                  `json:"gate_checked_at,omitempty"`
	Last          *rolloutOutcome         `json:"last,omitempty"`
}

func (s *supervisor) canaryStatus() canaryStatus {
	s.upgradeMu.Lock()
	status := canaryStatus{State: "idle", Last: s.lastRollout}
	candidateID := ""
	if rollout := s.canary; rollout != nil {
		candidateID = rollout.candidate.id
		status.State = "canary"
		status.Weight = rollout.weight
		status.Steps = append([]int(nil), rollout.steps...)
		if len(rollout.steps) > 0 {
			status.Step = rollout.step + 1
			status.DwellSeconds = int64(rollout.dwell / time.Second)
		}
		status.StartedAt = rollout.startedAt.UTC().Format(time.RFC3339)
		status.StepSince = rollout.stepSince.UTC().Format(time.RFC3339)
		status.Release = releaseStatusText(&releaseStateView{
			Version: rollout.version, State: "canary", Weight: rollout.weight,
			Since: status.StepSince,
		}, s.now())
		if rollout.gate != nil {
			gate := *rollout.gate
			status.Gate = &gate
			status.GateCheckedAt = rollout.gateAt.UTC().Format(time.RFC3339)
		}
	} else if last := s.lastRollout; last != nil {
		status.Release = releaseStatusText(&releaseStateView{
			Version: last.Version, State: last.State, Weight: last.Weight, Reason: last.Reason,
		}, s.now())
	}
	s.upgradeMu.Unlock()

	connections := map[string]int{}
	for _, backend := range s.router.Status() {
		connections[backend.ID] = backend.Connections
	}
	describe := func(id string) *canaryGenerationStatus {
		generation := &canaryGenerationStatus{ID: id, Connections: connections[id]}
		s.workersMu.Lock()
		if worker := s.workers[id]; worker != nil && worker.command != nil && worker.command.Process != nil {
			generation.PID = worker.command.Process.Pid
		}
		s.workersMu.Unlock()
		traffic, err := s.generationTraffic(id)
		if err != nil {
			generation.TrafficError = err.Error()
		} else {
			generation.Traffic = traffic
			generation.Version = traffic.Version
		}
		return generation
	}
	status.Incumbent = describe(s.router.Active().ID)
	if candidateID != "" {
		status.Candidate = describe(candidateID)
	}
	return status
}

// parseCanarySteps accepts "default" or ascending percents such as
// "5,25,100".
func parseCanarySteps(raw string) ([]int, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	if raw == "default" {
		return append([]int(nil), defaultCanarySteps...), nil
	}
	var steps []int
	for _, part := range strings.Split(raw, ",") {
		weight, err := strconv.Atoi(strings.TrimSuffix(strings.TrimSpace(part), "%"))
		if err != nil || weight < 1 || weight > 100 {
			return nil, fmt.Errorf("step %q must be a percent from 1 to 100", part)
		}
		if len(steps) > 0 && weight <= steps[len(steps)-1] {
			return nil, fmt.Errorf("steps must increase, got %q", raw)
		}
		steps = append(steps, weight)
	}
	return steps, nil
}

func parseCanaryWeight(raw string) (int, error) {
	weight, err := strconv.Atoi(strings.TrimSuffix(strings.TrimSpace(raw), "%"))
	if err != nil || weight < 0 || weight > 100 {
		return 0, fmt.Errorf("weight %q must be a percent from 0 to 100", raw)
	}
	return weight, nil
}

func (s *supervisor) registerCanaryHandlers(mux *http.ServeMux) {
	writeStatus := func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(s.canaryStatus())
	}
	fail := func(w http.ResponseWriter, err error, status int) {
		if errors.Is(err, errCanaryConflict) {
			status = http.StatusConflict
		}
		http.Error(w, err.Error(), status)
	}
	mux.HandleFunc("GET /_subrouter/canary", func(w http.ResponseWriter, _ *http.Request) { writeStatus(w) })
	mux.HandleFunc("POST /_subrouter/canary/start", func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query()
		steps, err := parseCanarySteps(query.Get("steps"))
		if err != nil {
			fail(w, err, http.StatusBadRequest)
			return
		}
		weight := -1
		if raw := query.Get("weight"); raw != "" {
			if weight, err = parseCanaryWeight(raw); err != nil {
				fail(w, err, http.StatusBadRequest)
				return
			}
		}
		switch {
		case len(steps) > 0 && weight >= 0 && weight != steps[0]:
			fail(w, fmt.Errorf("weight %d differs from the first step %d", weight, steps[0]), http.StatusBadRequest)
			return
		case len(steps) > 0:
			weight = steps[0]
		case weight < 0:
			fail(w, errors.New("start needs weight=<percent> or steps=default|<p1,p2,...>"), http.StatusBadRequest)
			return
		}
		dwell := defaultCanaryDwell
		if raw := query.Get("dwell"); raw != "" {
			if len(steps) == 0 {
				fail(w, errors.New("dwell applies only with steps"), http.StatusBadRequest)
				return
			}
			if dwell, err = time.ParseDuration(raw); err != nil || dwell <= 0 {
				fail(w, fmt.Errorf("dwell %q must be a positive duration such as 10m", raw), http.StatusBadRequest)
				return
			}
		}
		if len(steps) == 0 {
			dwell = 0
		}
		if err := s.startCanary(weight, steps, dwell); err != nil {
			fail(w, err, http.StatusBadGateway)
			return
		}
		writeStatus(w)
	})
	mux.HandleFunc("POST /_subrouter/canary/weight", func(w http.ResponseWriter, r *http.Request) {
		weight, err := parseCanaryWeight(r.URL.Query().Get("weight"))
		if err != nil {
			fail(w, err, http.StatusBadRequest)
			return
		}
		if err := s.setCanaryWeight(weight); err != nil {
			fail(w, err, http.StatusBadGateway)
			return
		}
		writeStatus(w)
	})
	mux.HandleFunc("POST /_subrouter/canary/promote", func(w http.ResponseWriter, _ *http.Request) {
		if err := s.promoteCanary("promoted by operator"); err != nil {
			fail(w, err, http.StatusBadGateway)
			return
		}
		writeStatus(w)
	})
	mux.HandleFunc("POST /_subrouter/canary/abort", func(w http.ResponseWriter, r *http.Request) {
		reason := strings.TrimSpace(r.URL.Query().Get("reason"))
		if reason == "" {
			reason = "aborted by operator"
		}
		if err := s.abortCanary(reason); err != nil {
			fail(w, err, http.StatusBadGateway)
			return
		}
		writeStatus(w)
	})
}
