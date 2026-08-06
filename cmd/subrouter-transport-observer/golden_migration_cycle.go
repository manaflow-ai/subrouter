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
	finalCutover            goldenActionSummary
	cleanup                 goldenActionSummary
}

func (r *goldenRunner) runMigrationCycle(ctx context.Context, inputs goldenCycleInputs) (goldenMigrationResult, error) {
	var result goldenMigrationResult
	var err error
	result.initial, err = r.startCycleInitialSessions(ctx, inputs, false)
	if err != nil {
		return result, err
	}
	beforeEvidence, err := r.capturePhase("migration-before-listener-handoff", result.initial, inputs.localDaemonPID)
	if err != nil {
		return result, err
	}
	result.before = evidenceByLabel(beforeEvidence)
	initialBaselineEnd := time.Now().UTC()
	initialMonitors, err := startGoldenContinuityMonitors(r, result.initial, initialBaselineEnd)
	if err != nil {
		return result, err
	}
	initialMonitorsStopped := false
	defer func() {
		if !initialMonitorsStopped {
			cancelGoldenContinuityMonitors(initialMonitors)
		}
	}()

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

	var frontProof *goldenSession
	result.finalCutover, frontProof, err = r.runMigrationTransitionWithProof(
		ctx, "final-cutover", "migration-candidate-front-final", result.preparation,
		inputs, result.initial, initialMonitors,
	)
	if err != nil {
		return result, err
	}
	result.fresh = append(result.fresh, frontProof)
	if err := validateGoldenMigrationLink(result.preparation, result.finalCutover, "front-migration-preparation"); err != nil {
		return result, err
	}
	if result.finalCutover.migrationCanonical.PreparationEvidenceSHA256 != result.preparation.EvidenceSHA256 {
		return result, failGolden("migration_preparation_hash_chain_invalid")
	}

	afterEvidence, err := r.capturePhase("migration-after-listener-handoff", append(append([]*goldenSession{}, result.initial...), result.fresh...), inputs.localDaemonPID)
	if err != nil {
		return result, err
	}
	result.after = evidenceByLabel(afterEvidence)
	if err := requireStableSessionSockets(result.initial, result.before, result.after); err != nil {
		return result, err
	}
	retiringSessions := append([]*goldenSession{}, result.initial...)
	if err := releaseGoldenTestSessions(retiringSessions); err != nil {
		return result, err
	}
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
	if err := closeGoldenSessionObservers(ctx, retiringSessions); err != nil {
		return result, err
	}
	directRetiring := goldenSessionsForRoute(retiringSessions, "direct-hosted")
	if len(directRetiring) < 2 {
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
	if err := releaseGoldenTestSessions([]*goldenSession{frontProof}); err != nil {
		return result, err
	}

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
		evidence.Release.SourceRevision != r.options.candidateRevision ||
		evidence.Bootstrap.Tag != goldenPinnedBootstrapTag ||
		evidence.Bootstrap.SHA256 != goldenPinnedBootstrapLinuxSHA256 ||
		evidence.Bootstrap.SourceRevision != goldenPinnedBootstrapRevision {
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
