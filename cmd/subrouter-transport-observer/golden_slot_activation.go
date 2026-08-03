package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	goldenSlotActivationRequestSchema = "subrouter.gcp.slot-activation-ack-request/v1"
	goldenSlotActivationAckSchema     = "subrouter.gcp.slot-activation-ack/v1"
)

type goldenSlotActivationRequest struct {
	Schema    string          `json:"schema"`
	Challenge string          `json:"challenge"`
	Run       goldenDeployRun `json:"run"`
	Slots     struct {
		Old                 string `json:"old"`
		Candidate           string `json:"candidate"`
		OldGeneration       string `json:"old_generation"`
		CandidateGeneration string `json:"candidate_generation"`
	} `json:"slots"`
	ConfiguredOriginalClients               int64  `json:"configured_original_clients"`
	ExpectedOriginalSlotConnections         int64  `json:"expected_original_slot_connections"`
	ExpectedFreshCandidateDirectConnections int64  `json:"expected_fresh_candidate_direct_connections"`
	UpgradeRequestedAt                      string `json:"upgrade_requested_at"`
	ProvisionalSwitchAt                     string `json:"provisional_switch_at"`
}

type goldenSlotActivationAck struct {
	Schema                              string `json:"schema"`
	Challenge                           string `json:"challenge"`
	CandidateSlot                       string `json:"candidate_slot"`
	CandidateGeneration                 string `json:"candidate_generation"`
	ConfiguredOriginalClients           int64  `json:"configured_original_clients"`
	OriginalStreamsCrossed              int64  `json:"original_streams_crossed"`
	DirectOriginalConnectionsVerified   int64  `json:"direct_original_connections_verified"`
	LocalEgressClientsVerified          int64  `json:"local_egress_clients_verified"`
	AllOriginalStreamsCrossedActivation bool   `json:"all_original_streams_crossed_activation"`
	ProcessesStable                     bool   `json:"processes_stable"`
	SocketsStable                       bool   `json:"sockets_stable"`
	LocalEgressVerified                 bool   `json:"local_egress_verified"`
	FreshCandidateDirectConnection      bool   `json:"fresh_candidate_direct_connection"`
	FreshCandidateConnectionID          string `json:"fresh_candidate_connection_id"`
	ActivatedAt                         string `json:"activated_at"`
}

type runningGoldenEvidenceCommand struct {
	command  *exec.Cmd
	started  time.Time
	done     chan struct{}
	mu       sync.Mutex
	waitErr  error
	finished time.Time
}

func (r *goldenRunner) runSlotActivationWithAck(
	ctx context.Context,
	intent string,
	inputs goldenCycleInputs,
	initial []*goldenSession,
	spanningLocal *goldenSession,
	before map[string]goldenProcessEvidence,
	spanningBefore map[string]goldenProcessEvidence,
	monitors []*goldenContinuityMonitor,
	localEgressMonitor *goldenLocalEgressMonitor,
) (goldenActionSummary, *goldenSession, map[string]goldenProcessEvidence, error) {
	if (intent != "rehearsal" && intent != "final") || localEgressMonitor == nil {
		return goldenActionSummary{}, nil, nil, failGolden("slot_activation_intent_invalid")
	}
	requestPath := filepath.Join(r.privateRoot, inputs.name+"-slot-activation-ack-request.json")
	ackPath := filepath.Join(r.privateRoot, inputs.name+"-slot-activation-ack.json")
	evidenceName := inputs.name + "-slot-activation.json"
	evidencePath := filepath.Join(r.artifactDir, evidenceName)
	for _, path := range []string{requestPath, ackPath, evidencePath} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			return goldenActionSummary{}, nil, nil, failGolden("slot_activation_handshake_path_exists")
		}
	}
	command, err := r.startGoldenEvidenceCommand(ctx, r.options.activation, []string{
		"--intent", intent,
		"--golden-ack-request", requestPath,
		"--golden-ack", ackPath,
		"--evidence-json", evidencePath,
	}, map[string]string{
		"SUBROUTER_CONFIGURED_ORIGINAL_CLIENTS":   "4",
		"SUBROUTER_EXPECTED_ORIGINAL_CONNECTIONS": "2",
	})
	if err != nil {
		return goldenActionSummary{}, nil, nil, err
	}
	waited := false
	defer func() {
		if !waited {
			killProcessGroup(command.command)
			<-command.done
		}
	}()
	requestData, err := waitGoldenHandshakeFile(ctx, requestPath, command.done)
	if err != nil {
		return goldenActionSummary{}, nil, nil, err
	}
	if err := localEgressMonitor.validate(); err != nil {
		return goldenActionSummary{}, nil, nil, err
	}
	var request goldenSlotActivationRequest
	if err := json.Unmarshal(requestData, &request); err != nil ||
		request.Schema != goldenSlotActivationRequestSchema || !validGoldenChallenge(request.Challenge) ||
		request.ConfiguredOriginalClients != 4 || request.ExpectedOriginalSlotConnections != 2 ||
		request.ExpectedFreshCandidateDirectConnections != 1 || request.Slots.Old == request.Slots.Candidate ||
		!validGoldenOpaqueID(request.Slots.OldGeneration) || !validGoldenOpaqueID(request.Slots.CandidateGeneration) ||
		request.Slots.OldGeneration == request.Slots.CandidateGeneration {
		return goldenActionSummary{}, nil, nil, failGolden("slot_activation_ack_request_invalid")
	}
	upgradeRequested, parseErr := parseGoldenEvidenceTime(request.UpgradeRequestedAt)
	provisionalSwitch, switchErr := parseGoldenEvidenceTime(request.ProvisionalSwitchAt)
	if parseErr != nil || switchErr != nil || provisionalSwitch.Before(upgradeRequested) ||
		time.Since(upgradeRequested) >= goldenActivationLimit {
		return goldenActionSummary{}, nil, nil, failGolden("slot_activation_ack_request_invalid")
	}
	if err := waitGoldenContinuityBoundary(ctx, monitors, provisionalSwitch); err != nil {
		return goldenActionSummary{}, nil, nil, err
	}
	if err := requireGoldenSessionSpans(spanningLocal, provisionalSwitch); err != nil {
		return goldenActionSummary{}, nil, nil, err
	}
	provisionalSessions := append(append([]*goldenSession{}, initial...), spanningLocal)
	provisionalEvidence, err := r.capturePhase(inputs.name+"-provisional-activation", provisionalSessions, inputs.localDaemonPID)
	if err != nil {
		return goldenActionSummary{}, nil, nil, err
	}
	provisionalByLabel := evidenceByLabel(provisionalEvidence)
	if err := localEgressMonitor.validate(); err != nil {
		return goldenActionSummary{}, nil, nil, err
	}
	if err := requireStableSessionSockets(initial, before, provisionalByLabel); err != nil {
		return goldenActionSummary{}, nil, nil, err
	}
	if err := requireStableSessionSockets([]*goldenSession{spanningLocal}, spanningBefore, provisionalByLabel); err != nil {
		return goldenActionSummary{}, nil, nil, err
	}
	if err := requireStableLocalEgress(spanningBefore, provisionalByLabel); err != nil {
		return goldenActionSummary{}, nil, nil, err
	}
	if err := requireBoundLocalEgress(provisionalSessions, spanningBefore); err != nil {
		return goldenActionSummary{}, nil, nil, err
	}
	if err := requireBoundLocalEgress(provisionalSessions, provisionalByLabel); err != nil {
		return goldenActionSummary{}, nil, nil, err
	}
	if err := r.requireGoldenSamplingStable(provisionalSessions); err != nil {
		return goldenActionSummary{}, nil, nil, err
	}
	activatedAt := time.Now().UTC()
	postDirect, err := r.startActivationSession(
		ctx, inputs.name+"-candidate-direct", "direct-hosted", inputs.clientPath, inputs.authData,
		inputs.cloud, inputs.directConfigPath, inputs.teamConfigPath, inputs.hostedOrigin, inputs.localOrigin,
	)
	if err != nil {
		return goldenActionSummary{}, nil, nil, err
	}
	if err := waitGoldenSessionChunks(ctx, postDirect, goldenBaselineChunkSamples); err != nil {
		return goldenActionSummary{}, nil, nil, err
	}
	if err := requireGoldenSessionStartsAfter(postDirect, activatedAt); err != nil {
		return goldenActionSummary{}, nil, nil, err
	}
	if err := waitGoldenContinuityBoundary(ctx, monitors, activatedAt); err != nil {
		return goldenActionSummary{}, nil, nil, err
	}
	all := append(append([]*goldenSession{}, provisionalSessions...), postDirect)
	afterEvidence, err := r.capturePhase(inputs.name+"-after-activation", all, inputs.localDaemonPID)
	if err != nil {
		return goldenActionSummary{}, nil, nil, err
	}
	after := evidenceByLabel(afterEvidence)
	if err := localEgressMonitor.validate(); err != nil {
		return goldenActionSummary{}, nil, nil, err
	}
	if err := requireStableSessionSockets(initial, before, after); err != nil {
		return goldenActionSummary{}, nil, nil, err
	}
	if err := requireStableSessionSockets([]*goldenSession{spanningLocal}, spanningBefore, after); err != nil {
		return goldenActionSummary{}, nil, nil, err
	}
	if err := requireStableLocalEgress(spanningBefore, after); err != nil {
		return goldenActionSummary{}, nil, nil, err
	}
	if err := requireBoundLocalEgress(provisionalSessions, after); err != nil {
		return goldenActionSummary{}, nil, nil, err
	}
	if err := r.requireGoldenLocalDaemonTransportClean(); err != nil {
		return goldenActionSummary{}, nil, nil, err
	}
	requestEvent, _, err := goldenSessionRequestWindow(postDirect)
	if err != nil || requestEvent.ConnectionID == "" {
		return goldenActionSummary{}, nil, nil, failGolden("fresh_candidate_connection_missing")
	}
	if err := r.requireGoldenSamplingStable(all); err != nil {
		return goldenActionSummary{}, nil, nil, err
	}
	if err := localEgressMonitor.validate(); err != nil {
		return goldenActionSummary{}, nil, nil, err
	}
	ack := goldenSlotActivationAck{
		Schema: goldenSlotActivationAckSchema, Challenge: request.Challenge,
		CandidateSlot: request.Slots.Candidate, CandidateGeneration: request.Slots.CandidateGeneration,
		ConfiguredOriginalClients: 4, OriginalStreamsCrossed: 4,
		DirectOriginalConnectionsVerified: 2, LocalEgressClientsVerified: 2,
		AllOriginalStreamsCrossedActivation: true, ProcessesStable: true, SocketsStable: true,
		LocalEgressVerified: true, FreshCandidateDirectConnection: true,
		FreshCandidateConnectionID: requestEvent.ConnectionID,
		ActivatedAt:                activatedAt.Format(time.RFC3339Nano),
	}
	ackData, err := json.Marshal(ack)
	ackFileData := append(ackData, '\n')
	ackDigest := sha256.Sum256(ackFileData)
	if err != nil || time.Since(upgradeRequested) >= goldenActivationLimit ||
		writePrivateFile(ackPath, ackFileData) != nil {
		return goldenActionSummary{}, nil, nil, failGolden("slot_activation_ack_write_failed")
	}
	<-command.done
	waited = true
	result := command.result()
	result.EvidenceType = "slot-activation"
	result.EvidenceFile = evidenceName
	if result.ExitCode != 0 {
		return result, nil, nil, failGolden("slot_activation_command_failed")
	}
	evidence, digest, err := loadGoldenDeployEvidence(ctx, r.options.evidenceValidator, "slot-activation", evidencePath)
	if err != nil {
		return result, nil, nil, err
	}
	result.EvidenceSHA256 = digest
	result.EvidenceValid = true
	result.canonical = evidence
	populateGoldenActionSummary(&result, evidence)
	if evidence.Intent != intent || evidence.Slots.Before != request.Slots.Old || evidence.Slots.Candidate != request.Slots.Candidate ||
		evidence.Slots.OldGeneration != request.Slots.OldGeneration ||
		evidence.Slots.CandidateGeneration != request.Slots.CandidateGeneration ||
		evidence.Timestamps.UpgradeRequestedAt != request.UpgradeRequestedAt ||
		evidence.Timestamps.ProvisionalSwitchAt != request.ProvisionalSwitchAt ||
		evidence.Timestamps.ActivatedAt != ack.ActivatedAt ||
		evidence.GoldenAck.SHA256 != fmt.Sprintf("%x", ackDigest[:]) ||
		evidence.GoldenAck.Challenge != ack.Challenge ||
		evidence.GoldenAck.FreshCandidateConnectionID != ack.FreshCandidateConnectionID {
		return result, nil, nil, failGolden("slot_activation_ack_evidence_mismatch")
	}
	if sessionDone(postDirect) {
		return result, nil, nil, failGolden("fresh_candidate_connection_not_held")
	}
	return result, postDirect, after, nil
}

func (r *goldenRunner) startGoldenEvidenceCommand(
	ctx context.Context,
	base, controlled []string,
	environment map[string]string,
) (*runningGoldenEvidenceCommand, error) {
	if len(base) == 0 {
		return nil, failGolden("deployment_action_missing")
	}
	for _, argument := range base[1:] {
		switch argument {
		case "--intent", "--operation", "--evidence-json", "--activation-evidence", "--transition-evidence",
			"--golden-ack-request", "--golden-ack", "--prior-evidence", "--destination-proof-request", "--destination-proof":
			return nil, failGolden("deployment_action_controls_reserved_argument")
		}
	}
	argv := append(append([]string(nil), base...), controlled...)
	command := exec.CommandContext(ctx, argv[0], argv[1:]...)
	configureProcessGroup(command)
	command.Stdin = os.Stdin
	if r.testMode {
		command.Stdout, command.Stderr = io.Discard, io.Discard
	} else {
		command.Stdout, command.Stderr = os.Stdout, os.Stderr
	}
	command.Env = goldenActionEnvironment(environment)
	started := time.Now().UTC()
	if err := command.Start(); err != nil {
		return nil, failGolden("deployment_action_start_failed")
	}
	running := &runningGoldenEvidenceCommand{command: command, started: started, done: make(chan struct{})}
	go func() {
		err := command.Wait()
		running.mu.Lock()
		running.waitErr = err
		running.finished = time.Now().UTC()
		running.mu.Unlock()
		close(running.done)
	}()
	return running, nil
}

func (r *runningGoldenEvidenceCommand) result() goldenActionSummary {
	r.mu.Lock()
	defer r.mu.Unlock()
	return goldenActionSummary{
		StartedAt: r.started.Format(time.RFC3339Nano), FinishedAt: r.finished.Format(time.RFC3339Nano),
		DurationMillis: r.finished.Sub(r.started).Milliseconds(), ExitCode: commandExitCode(r.waitErr),
	}
}

func waitGoldenHandshakeFile(ctx context.Context, path string, commandDone <-chan struct{}) ([]byte, error) {
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		info, err := os.Lstat(path)
		if err == nil {
			if !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > goldenActionEvidenceLimit {
				return nil, failGolden("deployment_handshake_file_invalid")
			}
			return readGoldenEvidenceFile(path)
		}
		if !errors.Is(err, os.ErrNotExist) {
			return nil, failGolden("deployment_handshake_file_invalid")
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-commandDone:
			return nil, failGolden("deployment_action_ended_before_handshake")
		case <-ticker.C:
		}
	}
}

func (r *goldenRunner) requireGoldenSamplingStable(sessions []*goldenSession) error {
	r.localRSSMu.Lock()
	localMissing := r.localRSSSamples == 0
	localExceeded := r.localRSSExceeded
	localPaused := r.localPausedSamples != 0
	localFailed := r.localSampleFailures != 0
	localGap := r.localMaxSampleGap > goldenProcessSampleMaxGap
	r.localRSSMu.Unlock()
	if localMissing {
		return failGolden("local_daemon_rss_missing")
	}
	if localExceeded {
		return failGolden("rss_limit_exceeded")
	}
	if localPaused {
		return failGolden("paused_process_detected")
	}
	if localFailed {
		return failGolden("process_sampling_failed")
	}
	if localGap {
		return failGolden("process_sampling_gap")
	}
	for _, session := range sessions {
		session.mu.Lock()
		missing := session.rssSamples == 0
		exceeded := session.rssExceeded
		paused := session.pausedProcessSamples != 0
		failed := session.processSampleFailures != 0
		gap := session.maxProcessSampleGap > goldenProcessSampleMaxGap
		session.mu.Unlock()
		if missing {
			return failGolden("process_rss_missing")
		}
		if exceeded {
			return failGolden("rss_limit_exceeded")
		}
		if paused {
			return failGolden("paused_process_detected")
		}
		if failed {
			return failGolden("process_sampling_failed")
		}
		if gap {
			return failGolden("process_sampling_gap")
		}
	}
	return nil
}

func validGoldenOpaqueID(value string) bool {
	return value != "" && len(value) <= 256 && !strings.ContainsAny(value, "\r\n\x00")
}

func validGoldenChallenge(value string) bool {
	return len(value) == 32 && validGoldenSHA256(value+value)
}
