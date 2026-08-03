package main

import (
	"context"
	"path/filepath"
	"time"
)

type goldenMigrationResult struct {
	initial, resumes, fresh []*goldenSession
	before, after           map[string]goldenProcessEvidence
	preparation             goldenActionSummary
	rehearsalCutover        goldenActionSummary
	rollback                goldenActionSummary
	finalCutover            goldenActionSummary
	cleanup                 goldenActionSummary
}

func (r *goldenRunner) runMigrationCycle(ctx context.Context, inputs goldenCycleInputs) (goldenMigrationResult, error) {
	var result goldenMigrationResult
	var err error
	result.preparation, err = r.runMigrationEvidenceAction(ctx, goldenMigrationActionOptions{
		label: "migration-preparation", argv: r.options.migrationPrepare,
		expect: "front-migration-preparation", evidenceName: "migration-preparation.json",
	})
	if err != nil {
		return result, err
	}
	if err := r.validateGoldenCandidateIdentity(result.preparation.migrationCanonical); err != nil {
		return result, err
	}

	result.initial, err = r.startCycleInitialSessions(ctx, inputs)
	if err != nil {
		return result, err
	}
	beforeEvidence, err := r.capturePhase("migration-before-rehearsal-cutover", result.initial, inputs.localDaemonPID)
	if err != nil {
		return result, err
	}
	result.before = evidenceByLabel(beforeEvidence)
	if err := requireLocalEgress(beforeEvidence); err != nil {
		return result, err
	}
	if err := requireBoundLocalEgress(result.initial, result.before); err != nil {
		return result, err
	}
	localEgressMonitor, err := startGoldenLocalEgressMonitor(
		ctx, r, inputs.localDaemonPID, "migration-local-egress-window", result.before["local-daemon"],
	)
	if err != nil {
		return result, err
	}
	localEgressMonitorStopped := false
	defer func() {
		if !localEgressMonitorStopped {
			_ = localEgressMonitor.stopAndValidate()
		}
	}()
	initialBaselineEnd := time.Now().UTC()
	initialMonitors, err := startGoldenContinuityMonitors(r, result.initial, initialBaselineEnd)
	if err != nil {
		return result, err
	}
	if err := localEgressMonitor.validate(); err != nil {
		return result, err
	}
	initialMonitorsStopped := false
	defer func() {
		if !initialMonitorsStopped {
			cancelGoldenContinuityMonitors(initialMonitors)
		}
	}()

	var frontProof *goldenSession
	result.rehearsalCutover, frontProof, err = r.runMigrationTransitionWithProof(
		ctx, "rehearsal-cutover", "migration-candidate-front-rehearsal", result.preparation,
		inputs, result.initial, initialMonitors,
	)
	if err != nil {
		return result, err
	}
	if err := localEgressMonitor.validate(); err != nil {
		return result, err
	}
	result.fresh = append(result.fresh, frontProof)
	if err := validateGoldenMigrationLink(result.preparation, result.rehearsalCutover, "front-migration-preparation"); err != nil {
		return result, err
	}
	frontBaselineEnd := time.Now().UTC()
	frontMonitors, err := startGoldenContinuityMonitors(r, []*goldenSession{frontProof}, frontBaselineEnd)
	if err != nil {
		return result, err
	}

	rollbackMonitors := append(append([]*goldenContinuityMonitor{}, initialMonitors...), frontMonitors...)
	var legacyProof *goldenSession
	result.rollback, legacyProof, err = r.runMigrationTransitionWithProof(
		ctx, "rollback", "migration-candidate-legacy-rollback", result.rehearsalCutover,
		inputs, []*goldenSession{frontProof}, rollbackMonitors,
	)
	if err != nil {
		cancelGoldenContinuityMonitors(frontMonitors)
		return result, err
	}
	if err := localEgressMonitor.validate(); err != nil {
		cancelGoldenContinuityMonitors(frontMonitors)
		return result, err
	}
	result.fresh = append(result.fresh, legacyProof)
	if err := validateGoldenMigrationLink(result.rehearsalCutover, result.rollback, "front-migration-cutover"); err != nil {
		cancelGoldenContinuityMonitors(frontMonitors)
		return result, err
	}
	if err := stopGoldenContinuityMonitors(frontMonitors, frontBaselineEnd, parseSummaryTime(result.rollback.ActivatedAt)); err != nil {
		return result, err
	}
	if sessionDone(frontProof) {
		return result, failGolden("migration_front_connection_not_held_through_rollback")
	}

	legacyBaselineEnd := time.Now().UTC()
	legacyMonitors, err := startGoldenContinuityMonitors(r, []*goldenSession{legacyProof}, legacyBaselineEnd)
	if err != nil {
		return result, err
	}
	finalMonitors := append(append([]*goldenContinuityMonitor{}, initialMonitors...), legacyMonitors...)
	var finalFrontProof *goldenSession
	result.finalCutover, finalFrontProof, err = r.runMigrationTransitionWithProof(
		ctx, "final-cutover", "migration-candidate-front-final", result.rollback,
		inputs, append(append([]*goldenSession{}, result.initial...), legacyProof), finalMonitors,
	)
	if err != nil {
		cancelGoldenContinuityMonitors(legacyMonitors)
		return result, err
	}
	if err := localEgressMonitor.validate(); err != nil {
		cancelGoldenContinuityMonitors(legacyMonitors)
		return result, err
	}
	result.fresh = append(result.fresh, finalFrontProof)
	if err := validateGoldenMigrationLink(result.rollback, result.finalCutover, "front-migration-rollback"); err != nil {
		cancelGoldenContinuityMonitors(legacyMonitors)
		return result, err
	}
	if result.finalCutover.migrationCanonical.PreparationEvidenceSHA256 != result.preparation.EvidenceSHA256 ||
		result.rollback.migrationCanonical.PreparationEvidenceSHA256 != result.preparation.EvidenceSHA256 ||
		result.rehearsalCutover.migrationCanonical.PreparationEvidenceSHA256 != result.preparation.EvidenceSHA256 {
		cancelGoldenContinuityMonitors(legacyMonitors)
		return result, failGolden("migration_preparation_hash_chain_invalid")
	}
	if err := stopGoldenContinuityMonitors(legacyMonitors, legacyBaselineEnd, parseSummaryTime(result.finalCutover.ActivatedAt)); err != nil {
		return result, err
	}

	afterEvidence, err := r.capturePhase("migration-after-final-cutover", append(append([]*goldenSession{}, result.initial...), result.fresh...), inputs.localDaemonPID)
	if err != nil {
		return result, err
	}
	result.after = evidenceByLabel(afterEvidence)
	if err := requireStableSessionSockets(result.initial, result.before, result.after); err != nil {
		return result, err
	}
	if err := requireStableLocalEgress(result.before, result.after); err != nil {
		return result, err
	}
	if err := requireBoundLocalEgress(result.initial, result.after); err != nil {
		return result, err
	}
	monitorErr := localEgressMonitor.stopAndValidate()
	localEgressMonitorStopped = true
	if monitorErr != nil {
		return result, monitorErr
	}
	retiringSessions := append(append([]*goldenSession{}, result.initial...), legacyProof)
	if err := waitGoldenSessions(ctx, retiringSessions); err != nil {
		return result, err
	}
	if err := stopGoldenContinuityMonitors(initialMonitors, initialBaselineEnd, parseSummaryTime(result.finalCutover.ActivatedAt)); err != nil {
		initialMonitorsStopped = true
		return result, err
	}
	initialMonitorsStopped = true
	if err := validateGoldenSessions(retiringSessions, false); err != nil {
		return result, err
	}
	if err := validateObserverTurns(retiringSessions, 1); err != nil {
		return result, err
	}
	closeGoldenSessionObservers(retiringSessions)
	directRetiring := goldenSessionsForRoute(retiringSessions, "direct-hosted")
	if len(directRetiring) < 3 {
		return result, failGolden("migration_retirement_direct_connection_count_invalid")
	}
	lastDirectClose, err := waitGoldenResponseConnectionsClosed(ctx, directRetiring)
	if err != nil {
		return result, err
	}
	result.cleanup, err = r.runMigrationEvidenceAction(ctx, goldenMigrationActionOptions{
		label: "legacy-retirement", argv: r.options.legacyRetirement, expect: "legacy-retirement",
		evidenceName: "migration-legacy-retirement.json", linkFlag: "--cutover-evidence",
		linkPath: filepath.Join(r.artifactDir, result.finalCutover.EvidenceFile),
	})
	if err != nil {
		return result, err
	}
	if result.cleanup.LinkedEvidenceSHA256 != result.finalCutover.EvidenceSHA256 ||
		result.cleanup.migrationCanonical.PreparationEvidenceSHA256 != result.preparation.EvidenceSHA256 {
		return result, failGolden("migration_retirement_hash_chain_invalid")
	}
	serverClosed := parseSummaryTime(result.cleanup.LastConnectionClosedAt)
	serverAbsent := parseSummaryTime(result.cleanup.AbsentAt)
	if serverClosed.Before(lastDirectClose) || serverAbsent.Before(serverClosed) || serverAbsent.Sub(serverClosed) >= goldenRetirementLimit {
		return result, failGolden("legacy_retirement_late")
	}
	result.cleanup.ObservedRetiredWithinMS = serverAbsent.Sub(lastDirectClose).Milliseconds()

	resumeState := goldenCycleResult{initial: result.initial, cleanup: result.cleanup}
	if err := r.resumeCycle(ctx, inputs.clientPath, &resumeState); err != nil {
		return result, err
	}
	result.resumes = resumeState.resumes
	if err := waitGoldenSessions(ctx, result.fresh); err != nil {
		return result, err
	}
	if err := validateGoldenSessions(result.fresh, false); err != nil {
		return result, err
	}
	if err := validateObserverTurns(result.fresh, 1); err != nil {
		return result, err
	}
	return result, nil
}

func validateGoldenMigrationLink(prior, current goldenActionSummary, priorType string) error {
	if !prior.EvidenceValid || !current.EvidenceValid || current.migrationCanonical == nil ||
		current.migrationCanonical.PriorEvidenceType != priorType ||
		current.LinkedEvidenceSHA256 != prior.EvidenceSHA256 {
		return failGolden("migration_evidence_hash_chain_invalid")
	}
	return nil
}

func (r *goldenRunner) validateGoldenCandidateIdentity(evidence *goldenMigrationEvidence) error {
	if evidence == nil {
		return failGolden("candidate_provenance_mismatch")
	}
	if r.testMode {
		return nil
	}
	if evidence.Release.Tag != r.options.candidateTag || evidence.Release.SHA256 != r.options.candidateSHA256 ||
		evidence.Release.SourceRevision != r.options.candidateRevision {
		return failGolden("candidate_provenance_mismatch")
	}
	return nil
}

func (r *goldenRunner) validateGoldenSlotCandidate(action goldenActionSummary) error {
	if r.testMode {
		return nil
	}
	if action.ReleaseTag != r.options.candidateTag || action.ToReleaseSHA256 != r.options.candidateSHA256 ||
		action.ReleaseSourceRevision != r.options.candidateRevision {
		return failGolden("candidate_provenance_mismatch")
	}
	return nil
}
