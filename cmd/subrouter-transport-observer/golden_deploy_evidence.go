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

const (
	goldenDeployEvidenceSchema = "subrouter.gcp.deploy-evidence/v1"
	goldenFrontRSSLimitBytes   = 128 << 20
)

type goldenDeployEvidence struct {
	Schema                   string                  `json:"schema"`
	EvidenceType             string                  `json:"evidence_type"`
	Mode                     string                  `json:"mode"`
	Intent                   string                  `json:"intent"`
	Success                  bool                    `json:"success"`
	ActivationEvidenceSHA256 string                  `json:"activation_evidence_sha256"`
	TransitionEvidenceType   string                  `json:"transition_evidence_type"`
	TransitionEvidenceSHA256 string                  `json:"transition_evidence_sha256"`
	EvidenceEmittedAt        string                  `json:"evidence_emitted_at"`
	Run                      goldenDeployRun         `json:"run"`
	Release                  goldenDeployRelease     `json:"release"`
	Slots                    goldenDeploySlots       `json:"slots"`
	Checksums                goldenDeployChecksums   `json:"checksums"`
	Timestamps               goldenDeployTimestamps  `json:"timestamps"`
	GoldenAck                goldenDeployGoldenAck   `json:"golden_ack"`
	Metrics                  goldenDeployMetrics     `json:"metrics"`
	Connections              goldenDeployConnections `json:"connections"`
	Continuity               goldenDeployContinuity  `json:"continuity"`
	Retirement               goldenDeployRetirement  `json:"retirement"`
}

type goldenDeployRun struct {
	ID       string `json:"id"`
	Project  string `json:"project"`
	Zone     string `json:"zone"`
	Instance string `json:"instance"`
}

type goldenDeployRelease struct {
	Tag                 string `json:"tag"`
	SHA256              string `json:"sha256"`
	SourceRevision      string `json:"source_revision"`
	TagOnMain           bool   `json:"tag_on_main"`
	AttestationVerified bool   `json:"attestation_verified"`
	Immutable           bool   `json:"immutable"`
}

type goldenDeploySlots struct {
	Before              string `json:"before"`
	Candidate           string `json:"candidate"`
	Final               string `json:"final"`
	OldGeneration       string `json:"old_generation"`
	CandidateGeneration string `json:"candidate_generation"`
	From                string `json:"from"`
	To                  string `json:"to"`
	FromGeneration      string `json:"from_generation"`
	ToGeneration        string `json:"to_generation"`
	Retired             string `json:"retired"`
	Active              string `json:"active"`
	RetiredGeneration   string `json:"retired_generation"`
}

type goldenDeployChecksums struct {
	InstalledBefore    string `json:"installed_before"`
	CandidateInstalled string `json:"candidate_installed"`
	InstalledAfter     string `json:"installed_after"`
	Candidate          string `json:"candidate"`
	Restored           string `json:"restored"`
}

type goldenDeployTimestamps struct {
	UpgradeRequestedAt    string `json:"upgrade_requested_at"`
	ProvisionalSwitchAt   string `json:"provisional_switch_at"`
	RollbackRequestedAt   string `json:"rollback_requested_at"`
	ActivatedAt           string `json:"activated_at"`
	GoldenAckReceivedAt   string `json:"golden_ack_received_at"`
	RetirementRequestedAt string `json:"retirement_requested_at"`
	EvidenceEmittedAt     string `json:"evidence_emitted_at"`
}

type goldenDeployGoldenAck struct {
	SHA256                              string `json:"sha256"`
	Challenge                           string `json:"challenge"`
	FreshCandidateConnectionID          string `json:"fresh_candidate_connection_id"`
	ConfiguredOriginalClients           int64  `json:"configured_original_clients"`
	OriginalStreamsCrossed              int64  `json:"original_streams_crossed"`
	DirectOriginalConnectionsVerified   int64  `json:"direct_original_connections_verified"`
	LocalEgressClientsVerified          int64  `json:"local_egress_clients_verified"`
	AllOriginalStreamsCrossedActivation bool   `json:"all_original_streams_crossed_activation"`
	ProcessesStable                     bool   `json:"processes_stable"`
	SocketsStable                       bool   `json:"sockets_stable"`
	LocalEgressVerified                 bool   `json:"local_egress_verified"`
	FreshCandidateDirectConnection      bool   `json:"fresh_candidate_direct_connection"`
	ActivatedAt                         string `json:"activated_at"`
	ReceivedAt                          string `json:"received_at"`
}

type goldenDeployCounter struct {
	Before *int64 `json:"before"`
	After  *int64 `json:"after"`
}

type goldenDeployServiceMetrics struct {
	NRestarts             goldenDeployCounter `json:"nrestarts"`
	OOMKill               goldenDeployCounter `json:"oom_kill"`
	RunScopedPeakRSSBytes *int64              `json:"run_scoped_peak_rss_bytes"`
	MemoryMaxBytes        *int64              `json:"memory_max_bytes"`
}

type goldenDeployMetrics struct {
	OldSlot       goldenDeployServiceMetrics `json:"old_slot"`
	CandidateSlot goldenDeployServiceMetrics `json:"candidate_slot"`
	RetiringSlot  goldenDeployServiceMetrics `json:"retiring_slot"`
	RestoredSlot  goldenDeployServiceMetrics `json:"restored_slot"`
	Front         goldenDeployServiceMetrics `json:"front"`
}

type goldenDeployConnections struct {
	ExpectedExternal *int64 `json:"expected_external"`
	Before           *int64 `json:"before"`
	After            *int64 `json:"after"`
}

type goldenDeployContinuity struct {
	ConfiguredOriginalClients               *int64 `json:"configured_original_clients"`
	ExpectedOriginalSlotConnections         *int64 `json:"expected_original_slot_connections"`
	PinnedOriginalConnectionsAtSwitch       *int64 `json:"pinned_original_connections_at_switch"`
	ExpectedCandidateConnectionsForRollback *int64 `json:"expected_candidate_connections_for_rollback"`
	CandidateConnectionsBefore              *int64 `json:"candidate_connections_before"`
	CandidateConnectionsAfterAck            *int64 `json:"candidate_connections_after_ack"`
	CandidateConnectionCountDelta           *int64 `json:"candidate_connection_count_delta"`
	AllExpectedSlotConnectionsPinned        *bool  `json:"all_expected_slot_connections_pinned"`
}

type goldenDeployRetirement struct {
	RequestedAt               string `json:"requested_at"`
	LastConnectionClosedAt    string `json:"last_connection_closed_at"`
	AbsentAt                  string `json:"absent_at"`
	AbsenceLatencyMillis      *int64 `json:"absence_latency_ms"`
	ServiceActiveAfter        *bool  `json:"service_active_after"`
	ControlSocketPresentAfter *bool  `json:"control_socket_present_after"`
	EnabledAfter              *bool  `json:"enabled_after"`
	ServiceResult             string `json:"service_result"`
}

type goldenEvidenceActionOptions struct {
	label        string
	argv         []string
	expect       string
	evidenceName string
	linkFlag     string
	linkPath     string
	environment  map[string]string
}

func (r *goldenRunner) runEvidenceAction(ctx context.Context, options goldenEvidenceActionOptions) (result goldenActionSummary) {
	started := time.Now().UTC()
	result = goldenActionSummary{
		StartedAt: started.Format(time.RFC3339Nano), ExitCode: -1,
		EvidenceType: options.expect, EvidenceFile: options.evidenceName,
	}
	finish := func() {
		finished := time.Now().UTC()
		result.FinishedAt = finished.Format(time.RFC3339Nano)
		result.DurationMillis = finished.Sub(started).Milliseconds()
	}
	defer finish()
	if len(options.argv) == 0 || options.evidenceName == "" || strings.Contains(options.evidenceName, string(filepath.Separator)) {
		return result
	}
	for _, argument := range options.argv[1:] {
		if argument == "--evidence-json" || argument == "--activation-evidence" || argument == "--transition-evidence" {
			return result
		}
	}
	evidencePath := filepath.Join(r.artifactDir, options.evidenceName)
	if _, err := os.Lstat(evidencePath); !errors.Is(err, os.ErrNotExist) {
		return result
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
		command.Stdout = io.Discard
		command.Stderr = io.Discard
	} else {
		command.Stdout = os.Stdout
		command.Stderr = os.Stderr
	}
	command.Env = goldenActionEnvironment(options.environment)
	if err := command.Run(); err != nil {
		if ctx.Err() != nil {
			killProcessGroup(command)
		}
		result.ExitCode = commandExitCode(err)
		return result
	}
	result.ExitCode = 0
	evidence, digest, err := loadGoldenDeployEvidence(ctx, r.options.evidenceValidator, options.expect, evidencePath)
	if err != nil {
		result.ExitCode = -1
		return result
	}
	result.EvidenceValid = true
	result.EvidenceSHA256 = digest
	result.canonical = evidence
	populateGoldenActionSummary(&result, evidence)
	_ = r.evidence.write(map[string]any{
		"kind": "deployment_evidence_validated", "timestamp": time.Now().UTC().Format(time.RFC3339Nano),
		"action": options.label, "evidence_type": options.expect, "evidence_sha256": digest,
	})
	return result
}

func goldenActionEnvironment(overrides map[string]string) []string {
	values := make(map[string]string)
	for _, entry := range os.Environ() {
		key, value, ok := strings.Cut(entry, "=")
		if ok {
			values[key] = value
		}
	}
	for key, value := range overrides {
		values[key] = value
	}
	result := make([]string, 0, len(values))
	for key, value := range values {
		result = append(result, key+"="+value)
	}
	return result
}

func loadGoldenDeployEvidence(ctx context.Context, validatorPath, expected, path string) (*goldenDeployEvidence, string, error) {
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
	validation.Stdout = io.Discard
	validation.Stderr = io.Discard
	if err := validation.Run(); err != nil {
		return nil, "", failGolden("deployment_evidence_invalid")
	}
	after, err := readGoldenEvidenceFile(path)
	if err != nil || !bytes.Equal(before, after) {
		return nil, "", failGolden("deployment_evidence_changed_during_validation")
	}
	var evidence goldenDeployEvidence
	decoder := json.NewDecoder(bytes.NewReader(after))
	if err := decoder.Decode(&evidence); err != nil {
		return nil, "", failGolden("deployment_evidence_invalid")
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, "", failGolden("deployment_evidence_invalid")
	}
	if err := validateGoldenDeployEvidence(&evidence, expected); err != nil {
		return nil, "", err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return nil, "", failGolden("deployment_evidence_protect_failed")
	}
	digest := sha256.Sum256(after)
	return &evidence, hex.EncodeToString(digest[:]), nil
}

func readGoldenEvidenceFile(path string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, failGolden("deployment_evidence_unreadable")
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, goldenActionEvidenceLimit+1))
	if err != nil || len(data) == 0 || len(data) > goldenActionEvidenceLimit {
		return nil, failGolden("deployment_evidence_file_invalid")
	}
	return data, nil
}

func validateGoldenDeployEvidence(evidence *goldenDeployEvidence, expected string) error {
	if evidence.Schema != goldenDeployEvidenceSchema || evidence.EvidenceType != expected || !evidence.Success {
		return failGolden("deployment_evidence_invalid")
	}
	switch expected {
	case "slot-activation":
		if evidence.Mode != "activation" || (evidence.Intent != "rehearsal" && evidence.Intent != "final") ||
			!validGoldenSHA256(evidence.Release.SHA256) || !evidence.Release.TagOnMain ||
			!evidence.Release.AttestationVerified || !evidence.Release.Immutable ||
			!validGoldenSHA256(evidence.Checksums.InstalledBefore) ||
			evidence.Checksums.CandidateInstalled != evidence.Release.SHA256 ||
			evidence.Checksums.InstalledAfter != evidence.Release.SHA256 ||
			evidence.Checksums.InstalledBefore == evidence.Release.SHA256 {
			return failGolden("transition_evidence_invalid")
		}
		if evidence.Continuity.ConfiguredOriginalClients == nil ||
			evidence.Continuity.ExpectedOriginalSlotConnections == nil ||
			evidence.Continuity.PinnedOriginalConnectionsAtSwitch == nil ||
			evidence.Continuity.ExpectedCandidateConnectionsForRollback == nil ||
			evidence.Continuity.CandidateConnectionsBefore == nil ||
			evidence.Continuity.CandidateConnectionsAfterAck == nil ||
			evidence.Continuity.CandidateConnectionCountDelta == nil ||
			evidence.Continuity.AllExpectedSlotConnectionsPinned == nil ||
			*evidence.Continuity.ConfiguredOriginalClients != 4 ||
			*evidence.Continuity.ExpectedOriginalSlotConnections != 2 ||
			*evidence.Continuity.PinnedOriginalConnectionsAtSwitch < 2 ||
			*evidence.Continuity.ExpectedCandidateConnectionsForRollback != 1 ||
			!*evidence.Continuity.AllExpectedSlotConnectionsPinned ||
			*evidence.Continuity.CandidateConnectionCountDelta < 1 ||
			*evidence.Continuity.CandidateConnectionsAfterAck-*evidence.Continuity.CandidateConnectionsBefore !=
				*evidence.Continuity.CandidateConnectionCountDelta {
			return failGolden("activation_connection_count_invalid")
		}
		requested, activated, err := goldenPhaseDuration(evidence.Timestamps.UpgradeRequestedAt, evidence.Timestamps.ActivatedAt)
		if err != nil {
			return err
		}
		provisional, provisionalErr := parseGoldenEvidenceTime(evidence.Timestamps.ProvisionalSwitchAt)
		received, receivedErr := parseGoldenEvidenceTime(evidence.Timestamps.GoldenAckReceivedAt)
		emitted, emittedErr := parseGoldenEvidenceTime(evidence.Timestamps.EvidenceEmittedAt)
		ackActivated, ackActivatedErr := parseGoldenEvidenceTime(evidence.GoldenAck.ActivatedAt)
		ackReceived, ackReceivedErr := parseGoldenEvidenceTime(evidence.GoldenAck.ReceivedAt)
		if provisionalErr != nil || receivedErr != nil || emittedErr != nil || ackActivatedErr != nil || ackReceivedErr != nil ||
			provisional.Before(requested) || activated.Before(provisional) || received.Before(activated) || emitted.Before(received) ||
			ackActivated != activated || ackReceived != received || received.Sub(requested) >= goldenActivationLimit ||
			!validGoldenSHA256(evidence.GoldenAck.SHA256) || !validGoldenChallenge(evidence.GoldenAck.Challenge) ||
			!validGoldenSHA256(evidence.GoldenAck.FreshCandidateConnectionID) ||
			evidence.GoldenAck.ConfiguredOriginalClients != 4 || evidence.GoldenAck.OriginalStreamsCrossed != 4 ||
			evidence.GoldenAck.DirectOriginalConnectionsVerified != 2 || evidence.GoldenAck.LocalEgressClientsVerified != 2 ||
			!evidence.GoldenAck.AllOriginalStreamsCrossedActivation || !evidence.GoldenAck.ProcessesStable ||
			!evidence.GoldenAck.SocketsStable || !evidence.GoldenAck.LocalEgressVerified ||
			!evidence.GoldenAck.FreshCandidateDirectConnection {
			return failGolden("slot_activation_ack_evidence_invalid")
		}
		if err := validateGoldenServerMetrics(evidence.Metrics.OldSlot, goldenRSSLimitBytes); err != nil {
			return err
		}
		if err := validateGoldenServerMetrics(evidence.Metrics.CandidateSlot, goldenRSSLimitBytes); err != nil {
			return err
		}
		return validateGoldenServerMetrics(evidence.Metrics.Front, goldenFrontRSSLimitBytes)
	case "slot-rollback":
		if evidence.Mode != "rollback-rehearsal" || !validGoldenSHA256(evidence.ActivationEvidenceSHA256) ||
			!validGoldenSHA256(evidence.Checksums.Candidate) || !validGoldenSHA256(evidence.Checksums.Restored) ||
			evidence.Checksums.Candidate == evidence.Checksums.Restored {
			return failGolden("transition_evidence_invalid")
		}
		if _, _, err := goldenPhaseDuration(evidence.Timestamps.RollbackRequestedAt, evidence.Timestamps.ActivatedAt); err != nil {
			return err
		}
		if err := validateGoldenServerMetrics(evidence.Metrics.RetiringSlot, goldenRSSLimitBytes); err != nil {
			return err
		}
		if err := validateGoldenServerMetrics(evidence.Metrics.RestoredSlot, goldenRSSLimitBytes); err != nil {
			return err
		}
		return validateGoldenServerMetrics(evidence.Metrics.Front, goldenFrontRSSLimitBytes)
	case "slot-retirement":
		if !validGoldenSHA256(evidence.TransitionEvidenceSHA256) ||
			(evidence.Mode != "deploy" && evidence.Mode != "rollback-rehearsal") ||
			evidence.Retirement.AbsenceLatencyMillis == nil || *evidence.Retirement.AbsenceLatencyMillis < 0 ||
			*evidence.Retirement.AbsenceLatencyMillis > goldenRetirementLimit.Milliseconds() {
			return failGolden("retirement_evidence_invalid")
		}
		closed, err := parseGoldenEvidenceTime(evidence.Retirement.LastConnectionClosedAt)
		if err != nil {
			return err
		}
		absent, err := parseGoldenEvidenceTime(evidence.Retirement.AbsentAt)
		if err != nil || absent.Before(closed) || absent.Sub(closed) >= goldenRetirementLimit {
			return failGolden("retirement_evidence_invalid")
		}
		return nil
	default:
		return failGolden("deployment_evidence_type_invalid")
	}
}

func validateGoldenServerMetrics(metrics goldenDeployServiceMetrics, limit int64) error {
	if metrics.NRestarts.Before == nil || metrics.NRestarts.After == nil ||
		metrics.OOMKill.Before == nil || metrics.OOMKill.After == nil ||
		metrics.RunScopedPeakRSSBytes == nil || metrics.MemoryMaxBytes == nil ||
		*metrics.NRestarts.Before != *metrics.NRestarts.After ||
		*metrics.OOMKill.Before != *metrics.OOMKill.After ||
		*metrics.MemoryMaxBytes != limit || *metrics.RunScopedPeakRSSBytes <= 0 ||
		*metrics.RunScopedPeakRSSBytes > limit {
		return failGolden("server_process_evidence_invalid")
	}
	return nil
}

func goldenPhaseDuration(requestedRaw, activatedRaw string) (time.Time, time.Time, error) {
	requested, err := parseGoldenEvidenceTime(requestedRaw)
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	activated, err := parseGoldenEvidenceTime(activatedRaw)
	if err != nil || activated.Before(requested) {
		return time.Time{}, time.Time{}, failGolden("action_timing_invalid")
	}
	if activated.Sub(requested) >= goldenActivationLimit {
		return time.Time{}, time.Time{}, failGolden("activation_duration_exceeded")
	}
	return requested, activated, nil
}

func parseGoldenEvidenceTime(raw string) (time.Time, error) {
	parsed, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil || parsed.Location() != time.UTC {
		return time.Time{}, failGolden("deployment_evidence_timestamp_invalid")
	}
	return parsed, nil
}

func populateGoldenActionSummary(result *goldenActionSummary, evidence *goldenDeployEvidence) {
	result.Mode = evidence.Mode
	result.ReleaseTag = evidence.Release.Tag
	result.ReleaseSourceRevision = evidence.Release.SourceRevision
	switch evidence.EvidenceType {
	case "slot-activation":
		requested, activated, _ := goldenPhaseDuration(evidence.Timestamps.UpgradeRequestedAt, evidence.Timestamps.ActivatedAt)
		result.RequestedAt = requested.Format(time.RFC3339Nano)
		result.ActivatedAt = activated.Format(time.RFC3339Nano)
		result.PhaseDurationMillis = activated.Sub(requested).Milliseconds()
		result.FromSlot, result.ToSlot, result.ActiveSlot = evidence.Slots.Before, evidence.Slots.Candidate, evidence.Slots.Final
		result.FromGenerationIDHash = hashGoldenValue(evidence.Slots.OldGeneration)
		result.ToGenerationIDHash = hashGoldenValue(evidence.Slots.CandidateGeneration)
		result.ActiveGenerationIDHash = result.ToGenerationIDHash
		result.FromReleaseSHA256 = evidence.Checksums.InstalledBefore
		result.ToReleaseSHA256 = evidence.Checksums.CandidateInstalled
		result.ServerOldPeakRSSBytes = *evidence.Metrics.OldSlot.RunScopedPeakRSSBytes
		result.ServerNewPeakRSSBytes = *evidence.Metrics.CandidateSlot.RunScopedPeakRSSBytes
		result.ServerFrontPeakRSSBytes = *evidence.Metrics.Front.RunScopedPeakRSSBytes
	case "slot-rollback":
		requested, activated, _ := goldenPhaseDuration(evidence.Timestamps.RollbackRequestedAt, evidence.Timestamps.ActivatedAt)
		result.RequestedAt = requested.Format(time.RFC3339Nano)
		result.ActivatedAt = activated.Format(time.RFC3339Nano)
		result.PhaseDurationMillis = activated.Sub(requested).Milliseconds()
		result.LinkedEvidenceSHA256 = evidence.ActivationEvidenceSHA256
		result.FromSlot, result.ToSlot, result.ActiveSlot = evidence.Slots.From, evidence.Slots.To, evidence.Slots.Final
		result.FromGenerationIDHash = hashGoldenValue(evidence.Slots.FromGeneration)
		result.ToGenerationIDHash = hashGoldenValue(evidence.Slots.ToGeneration)
		result.ActiveGenerationIDHash = result.ToGenerationIDHash
		result.FromReleaseSHA256 = evidence.Checksums.Candidate
		result.ToReleaseSHA256 = evidence.Checksums.Restored
		result.ServerOldPeakRSSBytes = *evidence.Metrics.RetiringSlot.RunScopedPeakRSSBytes
		result.ServerNewPeakRSSBytes = *evidence.Metrics.RestoredSlot.RunScopedPeakRSSBytes
		result.ServerFrontPeakRSSBytes = *evidence.Metrics.Front.RunScopedPeakRSSBytes
	case "slot-retirement":
		result.LinkedEvidenceSHA256 = evidence.TransitionEvidenceSHA256
		result.FromSlot, result.ActiveSlot = evidence.Slots.Retired, evidence.Slots.Active
		result.OldGenerationIDHash = hashGoldenValue(evidence.Slots.RetiredGeneration)
		result.OldGenerationActive = false
		result.OldGenerationAccepting = false
		result.OldGenerationConnections = 0
		result.ReportedRetiredWithinMS = *evidence.Retirement.AbsenceLatencyMillis
		result.LastConnectionClosedAt = evidence.Retirement.LastConnectionClosedAt
		result.AbsentAt = evidence.Retirement.AbsentAt
	}
}
