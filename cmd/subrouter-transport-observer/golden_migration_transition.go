package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

const (
	goldenDestinationProofRequestSchema    = "subrouter.gcp.destination-proof-request/v1"
	goldenDestinationProofSchema           = "subrouter.gcp.destination-proof/v1"
	goldenDestinationLivenessRequestSchema = "subrouter.gcp.destination-liveness-request/v1"
	goldenDestinationLivenessProofSchema   = "subrouter.gcp.destination-liveness/v1"
	goldenDestinationProofMaxAttempts      = 64
)

type goldenDestinationProofRequest struct {
	Schema                    string `json:"schema"`
	Challenge                 string `json:"challenge"`
	Operation                 string `json:"operation"`
	Destination               string `json:"destination"`
	DestinationGeneration     string `json:"destination_generation"`
	Source                    string `json:"source"`
	SourceGeneration          string `json:"source_generation"`
	SourceSnapshotSHA256      string `json:"source_snapshot_sha256"`
	ExpectedSourceConnections int64  `json:"expected_source_connections"`
	TransitionRequestedAt     string `json:"transition_requested_at"`
}

type goldenDestinationProof struct {
	Schema                     string `json:"schema"`
	Challenge                  string `json:"challenge"`
	Operation                  string `json:"operation"`
	Destination                string `json:"destination"`
	DestinationGeneration      string `json:"destination_generation"`
	Source                     string `json:"source"`
	SourceGeneration           string `json:"source_generation"`
	SourceSnapshotSHA256       string `json:"source_snapshot_sha256"`
	ExpectedSourceConnections  int64  `json:"expected_source_connections"`
	OriginalContinuityVerified bool   `json:"original_continuity_verified"`
	FreshPublicConnection      bool   `json:"fresh_public_connection"`
	ConnectionID               string `json:"connection_id"`
	SessionID                  string `json:"session_id"`
	ObservedAt                 string `json:"observed_at"`
}

type goldenDestinationLivenessRequest struct {
	Schema                    string `json:"schema"`
	Challenge                 string `json:"challenge"`
	Operation                 string `json:"operation"`
	Destination               string `json:"destination"`
	DestinationGeneration     string `json:"destination_generation"`
	ConnectionID              string `json:"connection_id"`
	SessionID                 string `json:"session_id"`
	DestinationSnapshotSHA256 string `json:"destination_snapshot_sha256"`
	RequestedAt               string `json:"requested_at"`
}

type goldenDestinationLivenessProof struct {
	Schema                    string `json:"schema"`
	Challenge                 string `json:"challenge"`
	Operation                 string `json:"operation"`
	Destination               string `json:"destination"`
	DestinationGeneration     string `json:"destination_generation"`
	ConnectionID              string `json:"connection_id"`
	SessionID                 string `json:"session_id"`
	DestinationSnapshotSHA256 string `json:"destination_snapshot_sha256"`
	RequestedAt               string `json:"requested_at"`
	ResponseChunkAt           string `json:"response_chunk_at"`
}

func (r *goldenRunner) runMigrationTransitionWithProof(
	ctx context.Context,
	operation string,
	label string,
	prior goldenActionSummary,
	inputs goldenCycleInputs,
	sourceSessions []*goldenSession,
	monitors []*goldenContinuityMonitor,
) (goldenActionSummary, *goldenSession, error) {
	source, destination, expectedConnections, expectedEvidence := "legacy", "front", int64(2), "front-migration-cutover"
	if operation == "rollback" {
		source, destination, expectedConnections, expectedEvidence = "front", "legacy", 1, "front-migration-rollback"
	} else if operation != "rehearsal-cutover" && operation != "final-cutover" {
		return goldenActionSummary{}, nil, failGolden("migration_operation_invalid")
	}
	if len(sourceSessions) == 0 || len(monitors) < len(sourceSessions) || !prior.EvidenceValid || !validGoldenSHA256(prior.EvidenceSHA256) {
		return goldenActionSummary{}, nil, failGolden("migration_source_continuity_missing")
	}
	requestPath := filepath.Join(r.privateRoot, label+"-destination-proof-request.json")
	proofPath := filepath.Join(r.privateRoot, label+"-destination-proof.json")
	evidenceName := label + ".json"
	evidencePath := filepath.Join(r.artifactDir, evidenceName)
	for _, path := range []string{requestPath, proofPath, evidencePath} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			return goldenActionSummary{}, nil, failGolden("migration_handshake_path_exists")
		}
	}
	actionEnvironment := map[string]string{
		"SUBROUTER_EXPECTED_MIGRATION_CONNECTIONS": strconv.FormatInt(expectedConnections, 10),
	}
	command, err := r.startGoldenEvidenceCommand(ctx, r.options.migrationSwitch, []string{
		"--operation", operation,
		"--prior-evidence", filepath.Join(r.artifactDir, prior.EvidenceFile),
		"--destination-proof-request", requestPath,
		"--destination-proof", proofPath,
		"--evidence-json", evidencePath,
	}, actionEnvironment)
	if err != nil {
		return goldenActionSummary{}, nil, err
	}
	waited := false
	defer func() {
		if !waited {
			command.stop()
		}
	}()
	var request goldenDestinationProofRequest
	var proof goldenDestinationProof
	var proofDigest [sha256.Size]byte
	var livenessRequest goldenDestinationLivenessRequest
	var livenessProof goldenDestinationLivenessProof
	var livenessProofDigest [sha256.Size]byte
	var destinationSession *goldenSession
	for attempt := 1; attempt <= goldenDestinationProofMaxAttempts; attempt++ {
		attemptRequestPath := goldenMigrationAttemptPath(requestPath, attempt)
		attemptProofPath := goldenMigrationAttemptPath(proofPath, attempt)
		attemptLivenessRequestPath := attemptProofPath + ".liveness-request"
		attemptLivenessProofPath := attemptProofPath + ".liveness-proof"
		requestData, err := waitGoldenHandshakeFile(ctx, attemptRequestPath, command.done)
		if err != nil {
			return goldenActionSummary{}, nil, err
		}
		request = goldenDestinationProofRequest{}
		if err := json.Unmarshal(requestData, &request); err != nil || request.Schema != goldenDestinationProofRequestSchema ||
			!validGoldenChallenge(request.Challenge) || request.Operation != operation || request.Source != source ||
			request.Destination != destination || !validGoldenOpaqueID(request.SourceGeneration) ||
			!validGoldenOpaqueID(request.DestinationGeneration) || request.SourceGeneration == request.DestinationGeneration ||
			!validGoldenSHA256(request.SourceSnapshotSHA256) || request.ExpectedSourceConnections != expectedConnections {
			return goldenActionSummary{}, nil, failGolden("migration_destination_proof_request_invalid")
		}
		requested, err := parseGoldenEvidenceTime(request.TransitionRequestedAt)
		if err != nil || time.Since(requested) >= goldenMigrationPropagationLimit {
			return goldenActionSummary{}, nil, failGolden("migration_destination_proof_request_invalid")
		}
		if err := waitGoldenContinuityBoundary(ctx, monitors, requested); err != nil {
			return goldenActionSummary{}, nil, err
		}
		if err := requireSessionsRunning(sourceSessions, "migration_"+operation); err != nil {
			return goldenActionSummary{}, nil, err
		}
		if err := r.requireGoldenSamplingStable(sourceSessions); err != nil {
			return goldenActionSummary{}, nil, err
		}
		observedAt := time.Now().UTC()
		sessionLabel := label + "-destination-direct"
		if attempt > 1 {
			sessionLabel = fmt.Sprintf("%s-attempt-%d", sessionLabel, attempt)
		}
		candidateSession, err := r.startActivationSessionWithTransport(
			ctx, sessionLabel, "direct-hosted", "http", inputs.clientPath, inputs.authData,
			inputs.cloud, inputs.directConfigPath, inputs.teamConfigPath, inputs.hostedOrigin, inputs.localOrigin,
		)
		if err != nil {
			return goldenActionSummary{}, nil, err
		}
		if err := waitGoldenSessionChunks(ctx, candidateSession, goldenBaselineChunkSamples); err != nil {
			return goldenActionSummary{}, nil, err
		}
		if err := requireGoldenSessionStartsAfter(candidateSession, observedAt); err != nil {
			return goldenActionSummary{}, nil, err
		}
		if err := waitGoldenContinuityBoundary(ctx, monitors, observedAt); err != nil {
			return goldenActionSummary{}, nil, err
		}
		requestEvent, _, err := goldenSessionRequestWindow(candidateSession)
		sessionID := goldenSessionThreadID(candidateSession)
		if err != nil || !validGoldenSHA256(requestEvent.ConnectionID) || !validGoldenOpaqueID(sessionID) {
			return goldenActionSummary{}, nil, failGolden("migration_destination_connection_missing")
		}
		proof = goldenDestinationProof{
			Schema: goldenDestinationProofSchema, Challenge: request.Challenge, Operation: operation,
			Destination: request.Destination, DestinationGeneration: request.DestinationGeneration,
			Source: request.Source, SourceGeneration: request.SourceGeneration,
			SourceSnapshotSHA256: request.SourceSnapshotSHA256, ExpectedSourceConnections: expectedConnections,
			OriginalContinuityVerified: true, FreshPublicConnection: true,
			ConnectionID: requestEvent.ConnectionID, SessionID: sessionID, ObservedAt: observedAt.Format(time.RFC3339Nano),
		}
		proofData, err := json.Marshal(proof)
		proofFileData := append(proofData, '\n')
		proofDigest = sha256.Sum256(proofFileData)
		if err != nil || time.Since(requested) >= goldenMigrationPropagationLimit || writePrivateFile(attemptProofPath, proofFileData) != nil {
			return goldenActionSummary{}, nil, failGolden("migration_destination_proof_write_failed")
		}
		retry, livenessData, err := waitGoldenMigrationAttemptOutcome(
			ctx, goldenMigrationAttemptPath(requestPath, attempt+1), attemptLivenessRequestPath, command.done,
		)
		if err != nil {
			return goldenActionSummary{}, nil, err
		}
		if retry {
			_ = r.evidence.write(map[string]any{
				"kind": "migration_destination_retry", "timestamp": time.Now().UTC().Format(time.RFC3339Nano),
				"operation": operation, "attempt": attempt, "session_id": sessionID,
			})
			if err := r.stopGoldenMigrationRejectedSession(candidateSession); err != nil {
				return goldenActionSummary{}, nil, err
			}
			continue
		}
		livenessRequest = goldenDestinationLivenessRequest{}
		if err := json.Unmarshal(livenessData, &livenessRequest); err != nil ||
			livenessRequest.Schema != goldenDestinationLivenessRequestSchema ||
			!validGoldenChallenge(livenessRequest.Challenge) || livenessRequest.Operation != operation ||
			livenessRequest.Destination != destination ||
			livenessRequest.DestinationGeneration != request.DestinationGeneration ||
			livenessRequest.ConnectionID != proof.ConnectionID || livenessRequest.SessionID != proof.SessionID ||
			!validGoldenSHA256(livenessRequest.DestinationSnapshotSHA256) {
			return goldenActionSummary{}, nil, failGolden("migration_destination_liveness_request_invalid")
		}
		livenessRequested, err := parseGoldenEvidenceTime(livenessRequest.RequestedAt)
		proofObserved, proofTimeErr := parseGoldenEvidenceTime(proof.ObservedAt)
		if err != nil || proofTimeErr != nil || livenessRequested.Before(proofObserved) ||
			time.Since(livenessRequested) >= goldenDestinationLivenessLimit {
			return goldenActionSummary{}, nil, failGolden("migration_destination_liveness_request_invalid")
		}
		responseChunkAt, err := waitGoldenConnectionResponseChunkAfter(
			ctx, candidateSession, proof.ConnectionID, livenessRequested,
		)
		if err != nil {
			return goldenActionSummary{}, nil, err
		}
		livenessProof = goldenDestinationLivenessProof{
			Schema: goldenDestinationLivenessProofSchema, Challenge: livenessRequest.Challenge,
			Operation: operation, Destination: destination,
			DestinationGeneration: request.DestinationGeneration,
			ConnectionID:          proof.ConnectionID, SessionID: proof.SessionID,
			DestinationSnapshotSHA256: livenessRequest.DestinationSnapshotSHA256,
			RequestedAt:               livenessRequest.RequestedAt, ResponseChunkAt: responseChunkAt.Format(time.RFC3339Nano),
		}
		livenessProofData, err := json.Marshal(livenessProof)
		livenessProofFileData := append(livenessProofData, '\n')
		livenessProofDigest = sha256.Sum256(livenessProofFileData)
		if err != nil || writePrivateFile(attemptLivenessProofPath, livenessProofFileData) != nil {
			return goldenActionSummary{}, nil, failGolden("migration_destination_liveness_proof_write_failed")
		}
		select {
		case <-ctx.Done():
			return goldenActionSummary{}, nil, ctx.Err()
		case <-command.done:
		}
		destinationSession = candidateSession
		break
	}
	if destinationSession == nil {
		return goldenActionSummary{}, nil, failGolden("migration_destination_retry_limit")
	}
	waited = true
	result := command.result()
	result.EvidenceType, result.EvidenceFile = expectedEvidence, evidenceName
	if result.ExitCode != 0 {
		return result, nil, failGolden("migration_transition_command_failed")
	}
	evidence, digest, err := loadGoldenMigrationEvidence(ctx, r.options.evidenceValidator, expectedEvidence, evidencePath)
	if err != nil {
		return result, nil, err
	}
	result.EvidenceSHA256, result.EvidenceValid = digest, true
	result.migrationCanonical = evidence
	if err := populateGoldenMigrationActionSummary(&result, evidence); err != nil {
		return result, nil, err
	}
	if evidence.PriorEvidenceSHA256 != prior.EvidenceSHA256 || evidence.Routing.Before != source ||
		evidence.Routing.After != destination || evidence.Timestamps.TransitionRequestedAt != request.TransitionRequestedAt ||
		evidence.Timestamps.ActivatedAt != proof.ObservedAt || evidence.DestinationProof.SHA256 != fmt.Sprintf("%x", proofDigest[:]) ||
		evidence.DestinationProof.Challenge != proof.Challenge || evidence.DestinationProof.ConnectionID != proof.ConnectionID ||
		evidence.DestinationProof.SessionID != proof.SessionID ||
		evidence.DestinationProof.PostSnapshotLiveness.SHA256 != fmt.Sprintf("%x", livenessProofDigest[:]) ||
		evidence.DestinationProof.PostSnapshotLiveness.Challenge != livenessProof.Challenge ||
		evidence.DestinationProof.PostSnapshotLiveness.ConnectionID != livenessProof.ConnectionID ||
		evidence.DestinationProof.PostSnapshotLiveness.SessionID != livenessProof.SessionID ||
		evidence.DestinationProof.PostSnapshotLiveness.DestinationSnapshotSHA256 != livenessProof.DestinationSnapshotSHA256 ||
		evidence.DestinationProof.PostSnapshotLiveness.RequestedAt != livenessProof.RequestedAt ||
		evidence.DestinationProof.PostSnapshotLiveness.ResponseChunkAt != livenessProof.ResponseChunkAt {
		return result, nil, failGolden("migration_destination_proof_evidence_mismatch")
	}
	if sessionDone(destinationSession) {
		return result, nil, failGolden("migration_destination_connection_not_held")
	}
	return result, destinationSession, nil
}

func goldenMigrationAttemptPath(base string, attempt int) string {
	if attempt <= 1 {
		return base
	}
	return fmt.Sprintf("%s.attempt-%d", base, attempt)
}

func goldenSessionThreadID(session *goldenSession) string {
	if session == nil {
		return ""
	}
	session.mu.Lock()
	defer session.mu.Unlock()
	return session.threadID
}

func waitGoldenConnectionResponseChunkAfter(
	ctx context.Context,
	session *goldenSession,
	connectionID string,
	boundary time.Time,
) (time.Time, error) {
	if session == nil || session.observer == nil || session.observer.stats == nil ||
		!validGoldenSHA256(connectionID) || boundary.IsZero() {
		return time.Time{}, failGolden("migration_destination_connection_not_held")
	}
	waitContext, cancel := context.WithTimeout(ctx, goldenDestinationLivenessLimit)
	defer cancel()
	for {
		requests, chunks, _ := session.observer.stats.snapshot()
		requestIDs := make(map[string]bool)
		for _, request := range requests {
			if request.ConnectionID == connectionID &&
				(request.Path == "/v1/responses" || request.Path == "/responses") {
				requestIDs[request.RequestID] = true
			}
		}
		if observerScopedErrorCount(session.observer.stats, func(event transportEvent) bool {
			return event.ConnectionID == connectionID && requestIDs[event.RequestID] &&
				(event.Path == "/v1/responses" || event.Path == "/responses")
		}) != 0 {
			return time.Time{}, failGolden("migration_destination_connection_not_held")
		}
		for _, chunk := range chunks {
			if chunk.Kind != "response_chunk" || chunk.ConnectionID != connectionID || chunk.Bytes <= 0 ||
				!requestIDs[chunk.RequestID] || (chunk.Path != "/v1/responses" && chunk.Path != "/responses") {
				continue
			}
			stamp, err := time.Parse(time.RFC3339Nano, chunk.Timestamp)
			if err != nil || stamp.IsZero() {
				return time.Time{}, failGolden("migration_destination_connection_not_held")
			}
			if !stamp.Before(boundary) {
				return stamp, nil
			}
		}
		if sessionDone(session) {
			return time.Time{}, failGolden("migration_destination_connection_not_held")
		}
		select {
		case <-waitContext.Done():
			return time.Time{}, failGolden("migration_destination_connection_not_held")
		case <-session.done:
			return time.Time{}, failGolden("migration_destination_connection_not_held")
		case <-session.observer.stats.notify:
		}
	}
}

func waitGoldenMigrationAttemptOutcome(
	ctx context.Context,
	nextRequestPath string,
	livenessRequestPath string,
	commandDone <-chan struct{},
) (bool, []byte, error) {
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		info, err := os.Lstat(nextRequestPath)
		if err == nil {
			if !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > goldenActionEvidenceLimit {
				return false, nil, failGolden("deployment_handshake_file_invalid")
			}
			return true, nil, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return false, nil, failGolden("deployment_handshake_file_invalid")
		}
		info, err = os.Lstat(livenessRequestPath)
		if err == nil {
			if !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > goldenActionEvidenceLimit {
				return false, nil, failGolden("deployment_handshake_file_invalid")
			}
			data, readErr := os.ReadFile(livenessRequestPath)
			if readErr != nil {
				return false, nil, failGolden("deployment_handshake_file_invalid")
			}
			return false, data, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return false, nil, failGolden("deployment_handshake_file_invalid")
		}
		select {
		case <-ctx.Done():
			return false, nil, ctx.Err()
		case <-commandDone:
			return false, nil, failGolden("migration_destination_liveness_request_missing")
		case <-ticker.C:
		}
	}
}

func (r *goldenRunner) stopGoldenMigrationRejectedSession(session *goldenSession) error {
	if session == nil || session.command == nil || sessionDone(session) {
		return failGolden("migration_rejected_session_cleanup_failed")
	}
	interruptCommand(session.command)
	select {
	case <-session.done:
	case <-time.After(time.Second):
		killProcessGroup(session.command)
		select {
		case <-session.done:
		case <-time.After(2 * time.Second):
		}
	}
	if !sessionDone(session) {
		return failGolden("migration_rejected_session_cleanup_failed")
	}
	r.mu.Lock()
	for index, running := range r.sessions {
		if running != session {
			continue
		}
		r.sessions = append(r.sessions[:index], r.sessions[index+1:]...)
		break
	}
	r.mu.Unlock()
	return nil
}
