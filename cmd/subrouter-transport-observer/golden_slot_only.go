package main

import (
	"fmt"
	"strings"
	"time"
)

// validateGoldenSlotOnlySummary validates the continuity evidence produced
// after listener-fd handoff. It deliberately shares the same release,
// transition, counter, session, and process requirements as the full gate,
// but does not accept or require the one-time legacy migration evidence.
func validateGoldenSlotOnlySummary(summary goldenSummary, testMode bool, bootstrapSHA, candidateTag, candidateSHA, candidateRevision string) error {
	if summary.MigrationPreparation != (goldenActionSummary{}) ||
		summary.MigrationFinalCutover != (goldenActionSummary{}) ||
		summary.LegacyCleanup != (goldenActionSummary{}) {
		return failGolden("slot_only_migration_evidence_present")
	}
	if summary.ReleasedVersion == "" || len(summary.ReleasedSHA256) != 64 ||
		!summary.ReleaseChecksumVerified || summary.ReleasePlatform != "darwin/arm64" {
		return failGolden("release_evidence_incomplete")
	}
	if !testMode && (summary.ExpectedPredecessorSHA256 != summary.ReleasedSHA256 ||
		!validGoldenSHA256(summary.ExpectedPredecessorSHA256)) {
		return failGolden("predecessor_evidence_incomplete")
	}
	if !testMode && (summary.ReleasedVersion != goldenPinnedPredecessorVersion ||
		summary.ReleasedSHA256 != goldenPinnedPredecessorSHA256 ||
		summary.PredecessorRevision != goldenPinnedPredecessorRevision ||
		!summary.PredecessorRevisionVerified) {
		return failGolden("predecessor_evidence_incomplete")
	}
	if !testMode && summary.ReleasedVersion == "test-override" {
		return failGolden("candidate_client_forbidden")
	}

	bootstrapSHA = strings.ToLower(strings.TrimSpace(bootstrapSHA))
	if testMode && bootstrapSHA == "" {
		bootstrapSHA = summary.Activation.FromReleaseSHA256
	}
	if !testMode && bootstrapSHA != goldenPinnedBootstrapLinuxSHA256 {
		return failGolden("deployment_provenance_mismatch")
	}
	if err := validateGoldenTransitionAction(summary.Activation, true); err != nil {
		return err
	}
	if err := validateGoldenProvenance(bootstrapSHA, summary.Activation); err != nil {
		return err
	}
	if err := validateGoldenSlotCandidateStandalone(summary.Activation, testMode, candidateTag, candidateSHA, candidateRevision); err != nil {
		return err
	}
	if err := validateGoldenTransitionAction(summary.Rollback, false); err != nil {
		return err
	}
	if err := validateGoldenRollback(summary.Activation, summary.Rollback); err != nil {
		return err
	}
	if err := validateGoldenTransitionAction(summary.FinalActivation, true); err != nil {
		return err
	}
	if err := validateGoldenProvenance(bootstrapSHA, summary.FinalActivation); err != nil {
		return err
	}
	if err := validateGoldenSlotCandidateStandalone(summary.FinalActivation, testMode, candidateTag, candidateSHA, candidateRevision); err != nil {
		return err
	}
	if err := validateGoldenSameActivation(summary.Activation, summary.FinalActivation); err != nil {
		return err
	}
	if err := validateGoldenCleanupSummary(summary.OldGenerationCleanup, summary.Activation.ToGenerationIDHash); err != nil {
		return err
	}
	if summary.OldGenerationCleanup.LinkedEvidenceSHA256 != summary.Rollback.EvidenceSHA256 {
		return failGolden("old_generation_evidence_invalid")
	}
	if err := validateGoldenCleanupSummary(summary.FinalOldGenerationCleanup, summary.Activation.FromGenerationIDHash); err != nil {
		return err
	}
	if summary.FinalOldGenerationCleanup.LinkedEvidenceSHA256 != summary.FinalActivation.EvidenceSHA256 {
		return failGolden("old_generation_evidence_invalid")
	}
	if err := validateGoldenCounterContinuity(summary); err != nil {
		return err
	}
	if err := validateGoldenSlotOnlyHealth(summary); err != nil {
		return err
	}
	finalCandidateSocket, err := validateGoldenSlotOnlySessions(summary.Sessions)
	if err != nil {
		return err
	}
	if !summary.FreshLocalLeaseObserved || !summary.HostedTenantLeaseObserved || summary.DeploymentEnvironmentRead {
		return failGolden("golden_evidence_incomplete")
	}
	if err := validateGoldenSlotOnlyProcessSnapshots(summary, finalCandidateSocket); err != nil {
		return err
	}
	if summary.LocalDaemonRSSSamples == 0 || summary.LocalDaemonProcessSamples == 0 ||
		summary.LocalDaemonPausedSamples != 0 ||
		summary.LocalDaemonMaxSampleGapMS > goldenProcessSampleMaxGap.Milliseconds() ||
		summary.LocalDaemonPeakRSSBytes <= 0 || summary.LocalDaemonPeakRSSBytes > goldenRSSLimitBytes {
		return failGolden("local_daemon_rss_missing")
	}
	return nil
}

func validateGoldenSlotCandidateStandalone(action goldenActionSummary, testMode bool, candidateTag, candidateSHA, candidateRevision string) error {
	if testMode {
		return nil
	}
	if action.ReleaseTag != candidateTag || action.ToReleaseSHA256 != candidateSHA ||
		action.ReleaseSourceRevision != candidateRevision {
		return failGolden("candidate_provenance_mismatch")
	}
	return nil
}

func validateGoldenSlotOnlyHealth(summary goldenSummary) error {
	if summary.ProbeFrequencyHz != 10 || len(summary.Health) != 4 {
		return failGolden("health_evidence_incomplete")
	}
	for _, health := range summary.Health {
		if health.Label == "" || health.Samples == 0 || health.Failures != 0 || health.MaxStartGapMillis > 250 {
			return failGolden("health_evidence_incomplete")
		}
	}
	return nil
}

func validateGoldenSlotOnlySessions(sessions []goldenSessionSummary) (string, error) {
	expected := make(map[string]struct{ route, transport string })
	for _, cycle := range []string{"rehearsal", "final"} {
		expected[cycle+"-direct-websocket"] = struct{ route, transport string }{"direct-hosted", "websocket"}
		expected[cycle+"-direct-http"] = struct{ route, transport string }{"direct-hosted", "http"}
		expected[cycle+"-local-websocket"] = struct{ route, transport string }{"local-egress", "websocket"}
		expected[cycle+"-local-http"] = struct{ route, transport string }{"local-egress", "http"}
		expected[cycle+"-candidate-direct"] = struct{ route, transport string }{"direct-hosted", "websocket"}
		expected[cycle+"-candidate-local"] = struct{ route, transport string }{"local-egress", "websocket"}
	}
	if len(sessions) != len(expected) {
		return "", fmt.Errorf("%w: got %d sessions, want %d", failGolden("session_evidence_incomplete"), len(sessions), len(expected))
	}
	finalCandidateTransportSocket := ""
	finalCandidateSocketStable := false
	for _, session := range sessions {
		want, ok := expected[session.Label]
		if !ok || session.Route != want.route || session.Transport != want.transport ||
			session.ProcessID <= 0 || len(session.ThreadIDHash) != 64 || len(session.NonceHash) != 64 ||
			session.ResponseRequests == 0 || session.ResponseConnections == 0 ||
			len(session.ResponseTransportSocket) != 64 ||
			(!strings.Contains(session.Label, "-candidate-") && !session.TransportSocketStable) ||
			session.ResponseBytes <= 0 || session.MarkerCount != 1 || session.RetryCount != 0 ||
			session.ReconnectCount != 0 || session.FallbackCount != 0 || session.ErrorCount != 0 ||
			session.NonzeroExitCount != 0 || session.DuplicateMarkerCount != 0 || session.PeakRSSBytes <= 0 ||
			session.PeakRSSBytes > goldenCodexRSSLimitBytes || session.RSSSamples == 0 ||
			session.ProcessSamples == 0 || session.PausedProcessSamples != 0 ||
			session.MaxProcessSampleGapMS > goldenProcessSampleMaxGap.Milliseconds() ||
			session.MaxChunkGapMillis > session.AllowedChunkGapMillis ||
			session.AllowedChunkGapMillis < goldenChunkGapFloor.Milliseconds() ||
			session.DeployMaxChunkGapMillis > session.AllowedChunkGapMillis {
			return "", fmt.Errorf("%w: invalid session %q", failGolden("session_evidence_incomplete"), session.Label)
		}
		if strings.Contains(session.Label, "-candidate-") {
			if session.ResumeMarkerCount != 0 || session.ResumeNonceCount != 0 {
				return "", failGolden("activation_session_evidence_invalid")
			}
		} else if session.ResumeMarkerCount != 1 || session.ResumeNonceCount != 1 ||
			len(session.SocketIDsBefore) == 0 || len(session.SocketIDsAfterRollback) == 0 {
			return "", failGolden("resume_evidence_incomplete")
		}
		if session.Route == "local-egress" && len(session.LocalUpstreamSocket) != 64 {
			return "", failGolden("local_upstream_evidence_missing")
		}
		if session.Route == "local-egress" &&
			(!session.LocalEgressCorrelated || len(session.LocalEgressSocket) != 64) {
			return "", failGolden("local_egress_correlation_missing")
		}
		allowed := 2 * session.PreDeployP99GapMillis
		if allowed < goldenChunkGapFloor.Milliseconds() {
			allowed = goldenChunkGapFloor.Milliseconds()
		}
		if !strings.Contains(session.Label, "-candidate-") && session.AllowedChunkGapMillis != allowed {
			return "", failGolden("chunk_gap_threshold_invalid")
		}
		if session.Label == "final-candidate-direct" {
			finalCandidateTransportSocket = session.ResponseTransportSocket
			finalCandidateSocketStable = session.TransportSocketStable
		}
		delete(expected, session.Label)
	}
	if len(expected) != 0 || !finalCandidateSocketStable || len(finalCandidateTransportSocket) != 64 {
		return "", failGolden("final_candidate_socket_continuity_invalid")
	}
	return finalCandidateTransportSocket, nil
}

func validateGoldenSlotOnlyProcessSnapshots(summary goldenSummary, finalCandidateSocket string) error {
	if len(summary.ProcessSnapshots) == 0 {
		return failGolden("process_evidence_incomplete")
	}
	required := make(map[string]bool)
	for _, cycle := range []string{"rehearsal", "final"} {
		phases := []string{cycle + "-before-activation", cycle + "-after-activation"}
		if cycle == "rehearsal" {
			phases = append(phases, cycle+"-after-rollback")
		}
		for _, phase := range phases {
			for _, suffix := range []string{"direct-websocket", "direct-http", "local-websocket", "local-http"} {
				required[phase+"\x00"+cycle+"-"+suffix] = false
			}
			required[phase+"\x00local-daemon"] = false
			if phase != cycle+"-before-activation" {
				required[phase+"\x00"+cycle+"-candidate-direct"] = false
				required[phase+"\x00"+cycle+"-candidate-local"] = false
			}
		}
	}
	required["final-candidate-after-retirement\x00final-candidate-direct"] = false
	retirementFinishedAt, err := time.Parse(time.RFC3339Nano, summary.FinalOldGenerationCleanup.FinishedAt)
	if err != nil {
		return failGolden("final_candidate_socket_continuity_invalid")
	}
	finalCandidateSnapshots := map[string]bool{
		"final-after-activation":           false,
		"final-candidate-after-retirement": false,
	}
	for _, item := range summary.ProcessSnapshots {
		key := item.Phase + "\x00" + item.Label
		if item.ProcessID <= 0 || item.Timestamp == "" || len(item.ProcessStates) == 0 {
			return failGolden("process_evidence_incomplete")
		}
		for _, state := range item.ProcessStates {
			if state == "" || strings.HasPrefix(strings.ToUpper(state), "T") {
				return failGolden("paused_process_detected")
			}
		}
		isObserver := strings.HasPrefix(item.Label, "observer-")
		if !goldenSlotOnlyProcessSnapshotAllowed(item, required) {
			return failGolden("process_evidence_incomplete")
		}
		if !isObserver && (len(item.DescendantPIDs) == 0 || item.RSSBytes <= 0 ||
			item.RSSBytes > goldenProcessRSSLimit(item.Label)) {
			return failGolden("socket_evidence_incomplete")
		}
		if !isObserver && len(item.SocketIDs) == 0 {
			return failGolden("socket_evidence_incomplete")
		}
		if item.Label == "local-daemon" && len(item.RemoteSocketIDs) == 0 {
			return failGolden("egress_evidence_incomplete")
		}
		if item.Label == "final-candidate-direct" {
			if _, tracked := finalCandidateSnapshots[item.Phase]; tracked {
				containsTransport := false
				for _, socketID := range item.SocketIDs {
					containsTransport = containsTransport || socketID == finalCandidateSocket
				}
				if !containsTransport {
					return failGolden("final_candidate_socket_continuity_invalid")
				}
				if item.Phase == "final-candidate-after-retirement" {
					capturedAt, parseErr := time.Parse(time.RFC3339Nano, item.Timestamp)
					if parseErr != nil || capturedAt.Before(retirementFinishedAt) {
						return failGolden("final_candidate_socket_continuity_invalid")
					}
				}
				finalCandidateSnapshots[item.Phase] = true
			}
		}
		if _, ok := required[key]; ok {
			required[key] = true
		}
	}
	for _, present := range finalCandidateSnapshots {
		if !present {
			return failGolden("final_candidate_socket_continuity_invalid")
		}
	}
	for _, present := range required {
		if !present {
			return failGolden("process_evidence_incomplete")
		}
	}
	return nil
}

func goldenSlotOnlyProcessSnapshotAllowed(item goldenProcessEvidence, required map[string]bool) bool {
	if strings.HasPrefix(item.Label, "observer-") {
		return item.Phase != "" && !strings.HasPrefix(item.Phase, "migration-")
	}
	key := item.Phase + "\x00" + item.Label
	if _, ok := required[key]; ok {
		return true
	}
	for _, cycle := range []string{"rehearsal", "final"} {
		if item.Phase == cycle+"-provisional-activation" {
			for _, label := range []string{
				cycle + "-direct-websocket", cycle + "-direct-http",
				cycle + "-local-websocket", cycle + "-local-http",
				cycle + "-candidate-local", "local-daemon",
			} {
				if item.Label == label {
					return true
				}
			}
		}
		if item.Phase == cycle+"-candidate-local-ready" &&
			(item.Label == cycle+"-candidate-local" || item.Label == "local-daemon") {
			return true
		}
	}
	return false
}
