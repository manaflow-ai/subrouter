package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const goldenPinnedPredecessorLinuxSHA256 = "99fcd10d912184c160370eb228b382795101f2b5b2467244f995aa2d10b0c323"

type goldenMigrationEvidence struct {
	Schema                    string                          `json:"schema"`
	EvidenceType              string                          `json:"evidence_type"`
	Mode                      string                          `json:"mode"`
	Success                   bool                            `json:"success"`
	PriorEvidenceType         string                          `json:"prior_evidence_type"`
	PriorEvidenceSHA256       string                          `json:"prior_evidence_sha256"`
	PreparationEvidenceSHA256 string                          `json:"preparation_evidence_sha256"`
	CutoverEvidenceSHA256     string                          `json:"cutover_evidence_sha256"`
	Run                       goldenDeployRun                 `json:"run"`
	Release                   goldenDeployRelease             `json:"release"`
	Bootstrap                 goldenDeployRelease             `json:"bootstrap"`
	Predecessor               goldenMigrationPredecessor      `json:"predecessor"`
	Routing                   goldenMigrationRouting          `json:"routing"`
	Legacy                    goldenMigrationLegacy           `json:"legacy"`
	Front                     goldenMigrationFront            `json:"front"`
	Timestamps                goldenMigrationTimestamps       `json:"timestamps"`
	DestinationProof          goldenMigrationDestinationProof `json:"destination_proof"`
	Source                    goldenMigrationTransitionSide   `json:"source"`
	Destination               goldenMigrationTransitionSide   `json:"destination"`
	Metrics                   goldenMigrationMetrics          `json:"metrics"`
	Continuity                goldenMigrationContinuity       `json:"continuity"`
	Rollback                  goldenMigrationRollback         `json:"rollback"`
	Connections               goldenMigrationConnections      `json:"connections"`
	Retirement                goldenMigrationRetirement       `json:"retirement"`
	EvidenceEmittedAt         string                          `json:"evidence_emitted_at"`
}

type goldenMigrationPredecessor struct {
	Tag                      string `json:"tag"`
	SHA256                   string `json:"sha256"`
	SourceRevision           string `json:"source_revision"`
	TagOnMain                bool   `json:"tag_on_main"`
	HardPinVerified          bool   `json:"hard_pin_verified"`
	SHA256SumsMatch          bool   `json:"sha256sums_match"`
	EmbeddedRevisionVerified bool   `json:"embedded_revision_verified"`
	LiveWorkerChecksumMatch  bool   `json:"live_worker_checksum_match"`
}

type goldenMigrationRouting struct {
	URLMap                string `json:"url_map"`
	LegacyBackend         string `json:"legacy_backend"`
	FrontBackend          string `json:"front_backend"`
	LegacyBackendURL      string `json:"legacy_backend_url"`
	FrontBackendURL       string `json:"front_backend_url"`
	Current               string `json:"current"`
	Before                string `json:"before"`
	After                 string `json:"after"`
	SourceBackendURL      string `json:"source_backend_url"`
	DestinationBackendURL string `json:"destination_backend_url"`
	Active                string `json:"active"`
	LegacyBackendRetained bool   `json:"legacy_backend_retained"`
	AcceptingNewPublic    bool   `json:"accepting_new_public"`
}

type goldenMigrationLegacy struct {
	Service            string `json:"service"`
	Generation         string `json:"generation"`
	Checksum           string `json:"checksum"`
	AcceptingNewPublic bool   `json:"accepting_new_public"`
}

type goldenMigrationFront struct {
	Slot            string                       `json:"slot"`
	Generation      string                       `json:"generation"`
	Checksum        string                       `json:"checksum"`
	ControlChecksum string                       `json:"control_checksum"`
	WorkerChecksum  string                       `json:"worker_checksum"`
	Ready           bool                         `json:"ready"`
	BackendHealth   goldenMigrationBackendHealth `json:"backend_health"`
}

type goldenMigrationBackendHealth struct {
	AllHealthy              bool   `json:"all_healthy"`
	StableSince             string `json:"stable_since"`
	VerifiedAt              string `json:"verified_at"`
	DurationMillis          int64  `json:"duration_ms"`
	HealthySamples          int64  `json:"healthy_samples"`
	MaxSampleGapMillis      int64  `json:"max_sample_gap_ms"`
	BackendMembershipSHA256 string `json:"backend_membership_sha256"`
}

type goldenMigrationTimestamps struct {
	TransitionRequestedAt string `json:"transition_requested_at"`
	ActivatedAt           string `json:"activated_at"`
	EvidenceEmittedAt     string `json:"evidence_emitted_at"`
}

type goldenMigrationDestinationProof struct {
	SHA256                     string `json:"sha256"`
	Challenge                  string `json:"challenge"`
	ConnectionID               string `json:"connection_id"`
	SessionID                  string `json:"session_id"`
	OriginalContinuityVerified bool   `json:"original_continuity_verified"`
	FreshPublicConnection      bool   `json:"fresh_public_connection"`
	ObservedAt                 string `json:"observed_at"`
	ReceivedAt                 string `json:"received_at"`
}

type goldenMigrationSnapshot struct {
	Kind                  string `json:"kind"`
	Generation            string `json:"generation"`
	PublicConnections     int64  `json:"public_connections"`
	GenerationConnections int64  `json:"generation_connections"`
	InactiveConnections   int64  `json:"inactive_connections"`
}

type goldenMigrationTransitionSide struct {
	Before                   goldenMigrationSnapshot `json:"before"`
	After                    goldenMigrationSnapshot `json:"after"`
	ConnectionCountDelta     int64                   `json:"connection_count_delta"`
	AcceptingNewPublicBefore bool                    `json:"accepting_new_public_before"`
	AcceptingNewPublicAfter  bool                    `json:"accepting_new_public_after"`
}

type goldenMigrationLegacyMetrics struct {
	NRestarts             goldenDeployCounter `json:"nrestarts"`
	OOMKill               goldenDeployCounter `json:"oom_kill"`
	RunScopedPeakRSSBytes *int64              `json:"run_scoped_peak_rss_bytes"`
	RSSLimitBytes         *int64              `json:"rss_limit_bytes"`
}

type goldenMigrationSlotMetrics struct {
	ID                    string              `json:"id"`
	NRestarts             goldenDeployCounter `json:"nrestarts"`
	OOMKill               goldenDeployCounter `json:"oom_kill"`
	RunScopedPeakRSSBytes *int64              `json:"run_scoped_peak_rss_bytes"`
	MemoryMaxBytes        *int64              `json:"memory_max_bytes"`
}

type goldenMigrationMetrics struct {
	SourceService         string                       `json:"source_service"`
	DestinationService    string                       `json:"destination_service"`
	Legacy                goldenMigrationLegacyMetrics `json:"legacy"`
	Slot                  goldenMigrationSlotMetrics   `json:"slot"`
	Front                 goldenDeployServiceMetrics   `json:"front"`
	NRestarts             goldenDeployCounter          `json:"nrestarts"`
	OOMKill               goldenDeployCounter          `json:"oom_kill"`
	RunScopedPeakRSSBytes *int64                       `json:"run_scoped_peak_rss_bytes"`
	RSSLimitBytes         *int64                       `json:"rss_limit_bytes"`
}

type goldenMigrationContinuity struct {
	ExpectedExternalConnections *int64 `json:"expected_external_connections"`
	Preserved                   *bool  `json:"preserved"`
}

type goldenMigrationRollback struct {
	Required  *bool `json:"required"`
	Performed *bool `json:"performed"`
}

type goldenMigrationConnectionSnapshot struct {
	Active   int64 `json:"active"`
	Inactive int64 `json:"inactive"`
	Total    int64 `json:"total"`
}

type goldenMigrationConnections struct {
	Before goldenMigrationConnectionSnapshot `json:"before"`
	After  goldenMigrationConnectionSnapshot `json:"after"`
}

type goldenMigrationRetirement struct {
	AcceptingNewPublicFalseAt string `json:"accepting_new_public_false_at"`
	LastConnectionClosedAt    string `json:"last_connection_closed_at"`
	StopRequestedAt           string `json:"stop_requested_at"`
	AbsentAt                  string `json:"absent_at"`
	AbsenceLatencyMillis      *int64 `json:"absence_latency_ms"`
	ServiceActiveAfter        *bool  `json:"service_active_after"`
	ControlSocketPresentAfter *bool  `json:"control_socket_present_after"`
	EnabledAfter              *bool  `json:"enabled_after"`
	ServiceResult             string `json:"service_result"`
}

func loadGoldenMigrationEvidence(ctx context.Context, validatorPath, expected, path string) (*goldenMigrationEvidence, string, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > goldenActionEvidenceLimit {
		return nil, "", failGolden("deployment_evidence_file_invalid")
	}
	if strings.TrimSpace(validatorPath) == "" {
		return nil, "", failGolden("deployment_evidence_validator_missing")
	}
	before, err := readGoldenEvidenceFile(path)
	if err != nil {
		return nil, "", err
	}
	validation := exec.CommandContext(ctx, "python3", validatorPath, "--expect", expected, path)
	validation.Stdout, validation.Stderr = io.Discard, io.Discard
	if err := validation.Run(); err != nil {
		return nil, "", failGolden("deployment_evidence_invalid")
	}
	after, err := readGoldenEvidenceFile(path)
	if err != nil || !bytes.Equal(before, after) {
		return nil, "", failGolden("deployment_evidence_changed_during_validation")
	}
	var evidence goldenMigrationEvidence
	decoder := json.NewDecoder(bytes.NewReader(after))
	if err := decoder.Decode(&evidence); err != nil {
		return nil, "", failGolden("deployment_evidence_invalid")
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, "", failGolden("deployment_evidence_invalid")
	}
	if err := validateGoldenMigrationEvidence(&evidence, expected); err != nil {
		return nil, "", err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return nil, "", failGolden("deployment_evidence_protect_failed")
	}
	digest := sha256.Sum256(after)
	return &evidence, hex.EncodeToString(digest[:]), nil
}

func validateGoldenMigrationEvidence(evidence *goldenMigrationEvidence, expected string) error {
	if evidence.Schema != goldenDeployEvidenceSchema || evidence.EvidenceType != expected || !evidence.Success {
		return failGolden("migration_evidence_invalid")
	}
	if err := validateGoldenMigrationIdentity(evidence); err != nil {
		return err
	}
	switch expected {
	case "front-migration-preparation":
		if evidence.Mode != "prepare" || evidence.Routing.Current != "legacy" ||
			evidence.Routing.URLMap == "" || evidence.Routing.LegacyBackend == "" || evidence.Routing.FrontBackend == "" ||
			evidence.Routing.LegacyBackendURL == evidence.Routing.FrontBackendURL ||
			!strings.HasPrefix(evidence.Routing.LegacyBackendURL, "https://") ||
			!strings.HasPrefix(evidence.Routing.FrontBackendURL, "https://") ||
			evidence.Legacy.Service != "subrouter.service" || evidence.Legacy.Generation == "" ||
			evidence.Legacy.Checksum != goldenPinnedPredecessorLinuxSHA256 || !evidence.Legacy.AcceptingNewPublic ||
			(evidence.Front.Slot != "slot-a" && evidence.Front.Slot != "slot-b") || evidence.Front.Generation == "" ||
			evidence.Front.Checksum != evidence.Release.SHA256 || evidence.Front.ControlChecksum != evidence.Release.SHA256 ||
			evidence.Front.WorkerChecksum != evidence.Bootstrap.SHA256 || !evidence.Front.Ready {
			return failGolden("migration_preparation_invalid")
		}
		stableSince, err := parseGoldenEvidenceTime(evidence.Front.BackendHealth.StableSince)
		if err != nil {
			return err
		}
		verifiedAt, err := parseGoldenEvidenceTime(evidence.Front.BackendHealth.VerifiedAt)
		if err != nil {
			return err
		}
		emittedAt, err := parseGoldenEvidenceTime(evidence.EvidenceEmittedAt)
		stableDuration := verifiedAt.Sub(stableSince)
		samplesCoverDuration := false
		if evidence.Front.BackendHealth.HealthySamples >= 1 &&
			evidence.Front.BackendHealth.MaxSampleGapMillis >= 0 &&
			evidence.Front.BackendHealth.MaxSampleGapMillis <= 15_000 {
			intervalWidth := evidence.Front.BackendHealth.MaxSampleGapMillis + 1
			requiredIntervals := evidence.Front.BackendHealth.DurationMillis / intervalWidth
			if evidence.Front.BackendHealth.DurationMillis%intervalWidth != 0 {
				requiredIntervals++
			}
			samplesCoverDuration = evidence.Front.BackendHealth.HealthySamples-1 >= requiredIntervals
		}
		if err != nil || !evidence.Front.BackendHealth.AllHealthy || verifiedAt.Before(stableSince) ||
			stableDuration < goldenBackendHealthStabilityLimit ||
			stableDuration > 15*time.Minute ||
			evidence.Front.BackendHealth.DurationMillis != stableDuration.Milliseconds() ||
			evidence.Front.BackendHealth.DurationMillis < goldenBackendHealthStabilityLimit.Milliseconds() ||
			evidence.Front.BackendHealth.HealthySamples < 21 ||
			evidence.Front.BackendHealth.MaxSampleGapMillis < 0 ||
			evidence.Front.BackendHealth.MaxSampleGapMillis > 15_000 ||
			!samplesCoverDuration ||
			!validGoldenSHA256(evidence.Front.BackendHealth.BackendMembershipSHA256) || emittedAt.Before(verifiedAt) {
			return failGolden("migration_backend_health_invalid")
		}
		return nil
	case "front-migration-cutover", "front-migration-rollback":
		return validateGoldenMigrationTransition(evidence, expected)
	case "legacy-retirement":
		return validateGoldenLegacyRetirement(evidence)
	default:
		return failGolden("migration_evidence_type_invalid")
	}
}

func validateGoldenMigrationIdentity(evidence *goldenMigrationEvidence) error {
	if evidence.Run.ID == "" || evidence.Run.Project == "" || evidence.Run.Zone == "" || evidence.Run.Instance == "" ||
		evidence.Release.Tag == "" || !validGoldenSHA256(evidence.Release.SHA256) ||
		!validGoldenRevision(evidence.Release.SourceRevision) || !evidence.Release.TagOnMain ||
		!evidence.Release.AttestationVerified || !evidence.Release.Immutable ||
		evidence.Bootstrap.Tag != goldenPinnedBootstrapTag ||
		evidence.Bootstrap.SHA256 != goldenPinnedBootstrapLinuxSHA256 ||
		evidence.Bootstrap.SourceRevision != goldenPinnedBootstrapRevision ||
		!evidence.Bootstrap.TagOnMain || !evidence.Bootstrap.AttestationVerified || !evidence.Bootstrap.Immutable ||
		evidence.Predecessor.Tag != "v0.1.51" || evidence.Predecessor.SHA256 != goldenPinnedPredecessorLinuxSHA256 ||
		evidence.Predecessor.SourceRevision != goldenPinnedPredecessorRevision || !evidence.Predecessor.TagOnMain ||
		!evidence.Predecessor.HardPinVerified || !evidence.Predecessor.SHA256SumsMatch ||
		!evidence.Predecessor.EmbeddedRevisionVerified || !evidence.Predecessor.LiveWorkerChecksumMatch ||
		evidence.Release.SHA256 == evidence.Predecessor.SHA256 ||
		evidence.Release.SHA256 == evidence.Bootstrap.SHA256 ||
		evidence.Bootstrap.SHA256 == evidence.Predecessor.SHA256 {
		return failGolden("migration_provenance_invalid")
	}
	return nil
}

func validateGoldenMigrationTransition(evidence *goldenMigrationEvidence, expected string) error {
	wantMode, source, destination, prior := "rollback", "front", "legacy", "front-migration-cutover"
	expectedConnections := int64(1)
	if expected == "front-migration-cutover" {
		source, destination, expectedConnections = "legacy", "front", 2
		if evidence.Mode == "rehearsal-cutover" {
			prior = "front-migration-preparation"
		} else if evidence.Mode == "final-cutover" {
			prior = "front-migration-rollback"
		} else {
			return failGolden("migration_transition_invalid")
		}
	} else if evidence.Mode != wantMode {
		return failGolden("migration_transition_invalid")
	}
	if evidence.PriorEvidenceType != prior || !validGoldenSHA256(evidence.PriorEvidenceSHA256) ||
		!validGoldenSHA256(evidence.PreparationEvidenceSHA256) || evidence.Routing.Before != source ||
		evidence.Routing.After != destination || evidence.Source.Before.Kind != source || evidence.Source.After.Kind != source ||
		evidence.Destination.Before.Kind != destination || evidence.Destination.After.Kind != destination ||
		evidence.Source.Before.Generation == "" || evidence.Source.Before.Generation != evidence.Source.After.Generation ||
		evidence.Source.Before.InactiveConnections != 0 || evidence.Source.After.InactiveConnections != 0 ||
		!evidence.Source.AcceptingNewPublicBefore || evidence.Source.AcceptingNewPublicAfter ||
		evidence.Continuity.ExpectedExternalConnections == nil || *evidence.Continuity.ExpectedExternalConnections != expectedConnections ||
		evidence.Continuity.Preserved == nil || !*evidence.Continuity.Preserved {
		return failGolden("migration_transition_invalid")
	}
	for _, snapshot := range []goldenMigrationSnapshot{evidence.Source.Before, evidence.Source.After} {
		if snapshot.PublicConnections < expectedConnections || snapshot.GenerationConnections < expectedConnections {
			return failGolden("migration_source_connection_count_invalid")
		}
	}
	if evidence.Destination.Before.Generation == "" ||
		evidence.Destination.Before.Generation != evidence.Destination.After.Generation ||
		evidence.Destination.Before.InactiveConnections != 0 || evidence.Destination.After.InactiveConnections != 0 ||
		evidence.Destination.ConnectionCountDelta < 1 ||
		evidence.Destination.After.GenerationConnections-evidence.Destination.Before.GenerationConnections != evidence.Destination.ConnectionCountDelta ||
		evidence.Destination.After.PublicConnections < evidence.Destination.Before.PublicConnections+1 {
		return failGolden("migration_destination_connection_count_invalid")
	}
	requested, activated, err := goldenPhaseDurationWithin(
		evidence.Timestamps.TransitionRequestedAt, evidence.Timestamps.ActivatedAt, goldenMigrationPropagationLimit,
	)
	if err != nil {
		return err
	}
	proofReceived, proofErr := parseGoldenEvidenceTime(evidence.DestinationProof.ReceivedAt)
	emitted, emittedErr := parseGoldenEvidenceTime(evidence.Timestamps.EvidenceEmittedAt)
	proofObserved, observedErr := parseGoldenEvidenceTime(evidence.DestinationProof.ObservedAt)
	if proofErr != nil || emittedErr != nil || observedErr != nil || proofObserved != activated ||
		proofReceived.Before(activated) || emitted.Before(proofReceived) || proofReceived.Sub(requested) >= goldenMigrationPropagationLimit ||
		!validGoldenSHA256(evidence.DestinationProof.SHA256) || !validGoldenChallenge(evidence.DestinationProof.Challenge) ||
		!validGoldenSHA256(evidence.DestinationProof.ConnectionID) || !validGoldenOpaqueID(evidence.DestinationProof.SessionID) ||
		!evidence.DestinationProof.OriginalContinuityVerified ||
		!evidence.DestinationProof.FreshPublicConnection {
		return failGolden("migration_destination_proof_invalid")
	}
	if err := validateGoldenMigrationMetrics(evidence); err != nil {
		return err
	}
	if evidence.Rollback.Required == nil || evidence.Rollback.Performed == nil ||
		*evidence.Rollback.Required != (evidence.Mode == "rehearsal-cutover") ||
		*evidence.Rollback.Performed != (evidence.Mode == "rollback") {
		return failGolden("migration_rollback_metadata_invalid")
	}
	return nil
}

func validateGoldenMigrationMetrics(evidence *goldenMigrationEvidence) error {
	wantSource, wantDestination := "slot", "legacy"
	if evidence.Routing.Before == "legacy" {
		wantSource, wantDestination = "legacy", "slot"
	}
	if evidence.Metrics.SourceService != wantSource || evidence.Metrics.DestinationService != wantDestination ||
		evidence.Metrics.Slot.ID != evidence.Front.Slot {
		return failGolden("migration_server_process_evidence_invalid")
	}
	if err := validateGoldenLegacyMetrics(evidence.Metrics.Legacy); err != nil {
		return err
	}
	slot := goldenDeployServiceMetrics{
		NRestarts: evidence.Metrics.Slot.NRestarts, OOMKill: evidence.Metrics.Slot.OOMKill,
		RunScopedPeakRSSBytes: evidence.Metrics.Slot.RunScopedPeakRSSBytes, MemoryMaxBytes: evidence.Metrics.Slot.MemoryMaxBytes,
	}
	if err := validateGoldenServerMetrics(slot, goldenRSSLimitBytes); err != nil {
		return err
	}
	return validateGoldenServerMetrics(evidence.Metrics.Front, goldenFrontRSSLimitBytes)
}

func validateGoldenLegacyMetrics(metrics goldenMigrationLegacyMetrics) error {
	if metrics.NRestarts.Before == nil || metrics.NRestarts.After == nil || metrics.OOMKill.Before == nil || metrics.OOMKill.After == nil ||
		metrics.RunScopedPeakRSSBytes == nil || metrics.RSSLimitBytes == nil ||
		*metrics.NRestarts.Before != *metrics.NRestarts.After || *metrics.OOMKill.Before != *metrics.OOMKill.After ||
		*metrics.RSSLimitBytes != goldenRSSLimitBytes || *metrics.RunScopedPeakRSSBytes <= 0 ||
		*metrics.RunScopedPeakRSSBytes > goldenRSSLimitBytes {
		return failGolden("migration_server_process_evidence_invalid")
	}
	return nil
}

func validateGoldenLegacyRetirement(evidence *goldenMigrationEvidence) error {
	if evidence.Mode != "final-cutover" || !validGoldenSHA256(evidence.CutoverEvidenceSHA256) ||
		!validGoldenSHA256(evidence.PreparationEvidenceSHA256) || evidence.Routing.Active != "front" ||
		!evidence.Routing.LegacyBackendRetained || evidence.Routing.AcceptingNewPublic ||
		evidence.Legacy.Service != "subrouter.service" || evidence.Legacy.Generation == "" ||
		evidence.Legacy.Checksum != goldenPinnedPredecessorLinuxSHA256 ||
		evidence.Connections.After.Active != 0 || evidence.Connections.After.Inactive != 0 || evidence.Connections.After.Total != 0 ||
		evidence.Connections.Before.Total != evidence.Connections.Before.Active+evidence.Connections.Before.Inactive ||
		evidence.Retirement.AbsenceLatencyMillis == nil || *evidence.Retirement.AbsenceLatencyMillis < 0 ||
		*evidence.Retirement.AbsenceLatencyMillis >= goldenRetirementLimit.Milliseconds() ||
		evidence.Retirement.ServiceActiveAfter == nil || *evidence.Retirement.ServiceActiveAfter ||
		evidence.Retirement.ControlSocketPresentAfter == nil || *evidence.Retirement.ControlSocketPresentAfter ||
		evidence.Retirement.EnabledAfter == nil || *evidence.Retirement.EnabledAfter ||
		evidence.Retirement.ServiceResult != "success" {
		return failGolden("legacy_retirement_evidence_invalid")
	}
	acceptingFalse, err := parseGoldenEvidenceTime(evidence.Retirement.AcceptingNewPublicFalseAt)
	if err != nil {
		return err
	}
	closed, err := parseGoldenEvidenceTime(evidence.Retirement.LastConnectionClosedAt)
	if err != nil {
		return err
	}
	stopRequested, err := parseGoldenEvidenceTime(evidence.Retirement.StopRequestedAt)
	if err != nil {
		return err
	}
	absent, err := parseGoldenEvidenceTime(evidence.Retirement.AbsentAt)
	if err != nil || closed.Before(acceptingFalse) || stopRequested.Before(closed) || absent.Before(stopRequested) ||
		absent.Sub(closed) >= goldenRetirementLimit {
		return failGolden("legacy_retirement_evidence_invalid")
	}
	rootMetrics := goldenMigrationLegacyMetrics{
		NRestarts: evidence.Metrics.NRestarts, OOMKill: evidence.Metrics.OOMKill,
		RunScopedPeakRSSBytes: evidence.Metrics.RunScopedPeakRSSBytes, RSSLimitBytes: evidence.Metrics.RSSLimitBytes,
	}
	return validateGoldenLegacyMetrics(rootMetrics)
}

func validGoldenRevision(value string) bool {
	return len(value) == 40 && value == strings.ToLower(value) && func() bool {
		_, err := hex.DecodeString(value)
		return err == nil
	}()
}

func validateGoldenMigrationSummary(summary goldenSummary, testMode bool) error {
	preparation := summary.MigrationPreparation
	if err := validateGoldenMigrationActionSummary(preparation, "front-migration-preparation"); err != nil {
		return err
	}
	if !testMode && (preparation.ReleaseTag != goldenPinnedCandidateTag ||
		preparation.ReleaseSourceRevision != summary.Activation.ReleaseSourceRevision) {
		return failGolden("migration_candidate_provenance_mismatch")
	}
	if preparation.FromSlot != "legacy" || preparation.ActiveSlot != "legacy" ||
		preparation.FromReleaseSHA256 != goldenPinnedPredecessorLinuxSHA256 ||
		preparation.ToReleaseSHA256 != goldenPinnedBootstrapLinuxSHA256 {
		return failGolden("migration_preparation_summary_invalid")
	}

	rehearsal := summary.MigrationRehearsalCutover
	rollback := summary.MigrationRollback
	final := summary.MigrationFinalCutover
	for _, item := range []struct {
		action       goldenActionSummary
		evidenceType string
		mode         string
	}{
		{rehearsal, "front-migration-cutover", "rehearsal-cutover"},
		{rollback, "front-migration-rollback", "rollback"},
		{final, "front-migration-cutover", "final-cutover"},
	} {
		if err := validateGoldenMigrationActionSummary(item.action, item.evidenceType); err != nil {
			return err
		}
		if item.action.Mode != item.mode {
			return failGolden("migration_transition_summary_invalid")
		}
	}
	if err := validateGoldenMigrationLink(preparation, rehearsal, "front-migration-preparation"); err != nil {
		return err
	}
	if err := validateGoldenMigrationLink(rehearsal, rollback, "front-migration-cutover"); err != nil {
		return err
	}
	if err := validateGoldenMigrationLink(rollback, final, "front-migration-rollback"); err != nil {
		return err
	}
	if rehearsal.FromSlot != "legacy" || rehearsal.ToSlot != "front" ||
		rollback.FromSlot != rehearsal.ToSlot || rollback.ToSlot != rehearsal.FromSlot ||
		rollback.FromGenerationIDHash != rehearsal.ToGenerationIDHash ||
		rollback.ToGenerationIDHash != rehearsal.FromGenerationIDHash ||
		rollback.FromReleaseSHA256 != rehearsal.ToReleaseSHA256 ||
		rollback.ToReleaseSHA256 != rehearsal.FromReleaseSHA256 ||
		final.FromSlot != rehearsal.FromSlot || final.ToSlot != rehearsal.ToSlot ||
		final.FromGenerationIDHash != rehearsal.FromGenerationIDHash ||
		final.ToGenerationIDHash != rehearsal.ToGenerationIDHash ||
		final.FromReleaseSHA256 != rehearsal.FromReleaseSHA256 ||
		final.ToReleaseSHA256 != rehearsal.ToReleaseSHA256 ||
		final.ReleaseTag != rehearsal.ReleaseTag || final.ReleaseSourceRevision != rehearsal.ReleaseSourceRevision {
		return failGolden("migration_transition_chain_invalid")
	}
	for _, action := range []goldenActionSummary{rehearsal, rollback, final} {
		if action.migrationCanonical.PreparationEvidenceSHA256 != preparation.EvidenceSHA256 ||
			action.ReleaseTag != preparation.ReleaseTag ||
			action.ReleaseSourceRevision != preparation.ReleaseSourceRevision {
			return failGolden("migration_preparation_hash_chain_invalid")
		}
	}
	if preparation.ReleaseTag != summary.Activation.ReleaseTag ||
		preparation.ReleaseSourceRevision != summary.Activation.ReleaseSourceRevision ||
		preparation.migrationCanonical.Release.SHA256 != summary.Activation.ToReleaseSHA256 ||
		preparation.ToReleaseSHA256 != summary.Activation.FromReleaseSHA256 {
		return failGolden("migration_slot_candidate_mismatch")
	}

	cleanup := summary.LegacyCleanup
	if err := validateGoldenMigrationActionSummary(cleanup, "legacy-retirement"); err != nil {
		return err
	}
	if cleanup.LinkedEvidenceSHA256 != final.EvidenceSHA256 ||
		cleanup.migrationCanonical.PreparationEvidenceSHA256 != preparation.EvidenceSHA256 ||
		cleanup.OldGenerationIDHash != rehearsal.FromGenerationIDHash ||
		cleanup.FromSlot != "legacy" || cleanup.ActiveSlot != "front" ||
		cleanup.OldGenerationActive || cleanup.OldGenerationAccepting || cleanup.OldGenerationConnections != 0 ||
		cleanup.ReportedRetiredWithinMS < 0 || cleanup.ReportedRetiredWithinMS >= goldenRetirementLimit.Milliseconds() ||
		cleanup.ObservedRetiredWithinMS < 0 {
		return failGolden("legacy_retirement_summary_invalid")
	}
	return nil
}

func validateGoldenMigrationActionSummary(action goldenActionSummary, expected string) error {
	if action.ExitCode != 0 || !action.EvidenceValid || action.EvidenceType != expected ||
		action.migrationCanonical == nil || !validGoldenSHA256(action.EvidenceSHA256) ||
		action.EvidenceFile == "" || filepath.Base(action.EvidenceFile) != action.EvidenceFile ||
		action.StartedAt == "" || action.FinishedAt == "" {
		return failGolden("migration_action_summary_invalid")
	}
	if err := validateGoldenMigrationEvidence(action.migrationCanonical, expected); err != nil {
		return err
	}
	started, finished := parseSummaryTime(action.StartedAt), parseSummaryTime(action.FinishedAt)
	if started.IsZero() || finished.Before(started) || action.DurationMillis != finished.Sub(started).Milliseconds() {
		return failGolden("migration_action_timing_invalid")
	}
	evidence := action.migrationCanonical
	if action.Mode != evidence.Mode || action.ReleaseTag != evidence.Release.Tag ||
		action.ReleaseSourceRevision != evidence.Release.SourceRevision || action.RestartDelta != 0 || action.OOMDelta != 0 {
		return failGolden("migration_action_summary_invalid")
	}
	switch expected {
	case "front-migration-preparation":
		if action.FromReleaseSHA256 != evidence.Predecessor.SHA256 || action.ToReleaseSHA256 != evidence.Bootstrap.SHA256 {
			return failGolden("migration_preparation_summary_invalid")
		}
	case "front-migration-cutover", "front-migration-rollback":
		requested, activated, err := goldenPhaseDuration(action.RequestedAt, action.ActivatedAt)
		if err != nil || action.PhaseDurationMillis != activated.Sub(requested).Milliseconds() ||
			action.LinkedEvidenceSHA256 != evidence.PriorEvidenceSHA256 ||
			action.FromSlot != evidence.Routing.Before || action.ToSlot != evidence.Routing.After ||
			action.ActiveSlot != evidence.Routing.After ||
			action.FromGenerationIDHash != hashGoldenValue(evidence.Source.Before.Generation) ||
			action.ToGenerationIDHash != hashGoldenValue(evidence.Destination.After.Generation) ||
			action.ActiveGenerationIDHash != action.ToGenerationIDHash ||
			action.ServerOldPeakRSSBytes != *evidence.Metrics.Legacy.RunScopedPeakRSSBytes ||
			action.ServerNewPeakRSSBytes != *evidence.Metrics.Slot.RunScopedPeakRSSBytes ||
			action.ServerFrontPeakRSSBytes != *evidence.Metrics.Front.RunScopedPeakRSSBytes {
			return failGolden("migration_transition_summary_invalid")
		}
		fromRelease, toRelease := evidence.Predecessor.SHA256, evidence.Bootstrap.SHA256
		if evidence.Routing.Before == "front" {
			fromRelease, toRelease = toRelease, fromRelease
		}
		if action.FromReleaseSHA256 != fromRelease || action.ToReleaseSHA256 != toRelease {
			return failGolden("migration_transition_summary_invalid")
		}
	case "legacy-retirement":
		if action.LinkedEvidenceSHA256 != evidence.CutoverEvidenceSHA256 ||
			action.OldGenerationIDHash != hashGoldenValue(evidence.Legacy.Generation) ||
			action.LastConnectionClosedAt != evidence.Retirement.LastConnectionClosedAt ||
			action.AbsentAt != evidence.Retirement.AbsentAt ||
			action.ReportedRetiredWithinMS != *evidence.Retirement.AbsenceLatencyMillis ||
			action.ServerRSSBytes != *evidence.Metrics.RunScopedPeakRSSBytes {
			return failGolden("legacy_retirement_summary_invalid")
		}
	}
	return nil
}

type goldenMigrationActionOptions struct {
	label        string
	argv         []string
	expect       string
	evidenceName string
	linkFlag     string
	linkPath     string
}

func (r *goldenRunner) runMigrationEvidenceAction(ctx context.Context, options goldenMigrationActionOptions) (goldenActionSummary, error) {
	started := time.Now().UTC()
	result := goldenActionSummary{
		StartedAt: started.Format(time.RFC3339Nano), ExitCode: -1,
		EvidenceType: options.expect, EvidenceFile: options.evidenceName,
	}
	finish := func() {
		finished := time.Now().UTC()
		result.FinishedAt = finished.Format(time.RFC3339Nano)
		result.DurationMillis = finished.Sub(started).Milliseconds()
	}
	if len(options.argv) == 0 || options.evidenceName == "" || strings.Contains(options.evidenceName, string(filepath.Separator)) {
		finish()
		return result, failGolden("migration_action_missing")
	}
	for _, argument := range options.argv[1:] {
		if argument == "--evidence-json" || argument == "--prior-evidence" || argument == "--cutover-evidence" {
			finish()
			return result, failGolden("migration_action_controls_reserved_argument")
		}
	}
	evidencePath := filepath.Join(r.artifactDir, options.evidenceName)
	if _, err := os.Lstat(evidencePath); !errors.Is(err, os.ErrNotExist) {
		finish()
		return result, failGolden("migration_evidence_path_exists")
	}
	argv := append([]string(nil), options.argv...)
	if options.linkFlag != "" {
		argv = append(argv, options.linkFlag, options.linkPath)
	}
	argv = append(argv, "--evidence-json", evidencePath)
	command := exec.CommandContext(ctx, argv[0], argv[1:]...)
	configureGoldenDeploymentAction(command)
	command.Stdin = os.Stdin
	if r.testMode {
		command.Stdout, command.Stderr = io.Discard, io.Discard
	} else {
		command.Stdout, command.Stderr = os.Stdout, os.Stderr
	}
	command.Env = goldenActionEnvironment(nil)
	err := command.Run()
	finish()
	result.ExitCode = commandExitCode(err)
	if err != nil {
		return result, failGolden("migration_action_failed")
	}
	evidence, digest, err := loadGoldenMigrationEvidence(ctx, r.options.evidenceValidator, options.expect, evidencePath)
	if err != nil {
		return result, err
	}
	result.ExitCode, result.EvidenceValid, result.EvidenceSHA256 = 0, true, digest
	result.migrationCanonical = evidence
	populateGoldenMigrationActionSummary(&result, evidence)
	return result, nil
}

func populateGoldenMigrationActionSummary(result *goldenActionSummary, evidence *goldenMigrationEvidence) {
	result.Mode = evidence.Mode
	result.ReleaseTag = evidence.Release.Tag
	result.ReleaseSourceRevision = evidence.Release.SourceRevision
	switch evidence.EvidenceType {
	case "front-migration-preparation":
		result.FromSlot, result.ActiveSlot = "legacy", "legacy"
		result.FromReleaseSHA256 = evidence.Predecessor.SHA256
		result.ToReleaseSHA256 = evidence.Bootstrap.SHA256
	case "front-migration-cutover", "front-migration-rollback":
		requested, activated, _ := goldenPhaseDuration(evidence.Timestamps.TransitionRequestedAt, evidence.Timestamps.ActivatedAt)
		result.RequestedAt, result.ActivatedAt = requested.Format(time.RFC3339Nano), activated.Format(time.RFC3339Nano)
		result.PhaseDurationMillis = activated.Sub(requested).Milliseconds()
		result.LinkedEvidenceSHA256 = evidence.PriorEvidenceSHA256
		result.FromSlot, result.ToSlot, result.ActiveSlot = evidence.Routing.Before, evidence.Routing.After, evidence.Routing.After
		result.FromGenerationIDHash = hashGoldenValue(evidence.Source.Before.Generation)
		result.ToGenerationIDHash = hashGoldenValue(evidence.Destination.After.Generation)
		result.ActiveGenerationIDHash = result.ToGenerationIDHash
		result.FromReleaseSHA256 = evidence.Predecessor.SHA256
		result.ToReleaseSHA256 = evidence.Bootstrap.SHA256
		if evidence.Routing.Before == "front" {
			result.FromReleaseSHA256, result.ToReleaseSHA256 = result.ToReleaseSHA256, result.FromReleaseSHA256
		}
		result.ServerOldPeakRSSBytes = *evidence.Metrics.Legacy.RunScopedPeakRSSBytes
		result.ServerNewPeakRSSBytes = *evidence.Metrics.Slot.RunScopedPeakRSSBytes
		result.ServerFrontPeakRSSBytes = *evidence.Metrics.Front.RunScopedPeakRSSBytes
	case "legacy-retirement":
		result.LinkedEvidenceSHA256 = evidence.CutoverEvidenceSHA256
		result.FromSlot, result.ActiveSlot = "legacy", "front"
		result.OldGenerationIDHash = hashGoldenValue(evidence.Legacy.Generation)
		result.LastConnectionClosedAt = evidence.Retirement.LastConnectionClosedAt
		result.AbsentAt = evidence.Retirement.AbsentAt
		result.ReportedRetiredWithinMS = *evidence.Retirement.AbsenceLatencyMillis
		result.ServerRSSBytes = *evidence.Metrics.RunScopedPeakRSSBytes
	}
}
