package main

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestGoldenSummaryRejectsActivationAtOrAboveThirtySeconds(t *testing.T) {
	summary := validGoldenAcceptanceSummary()
	summary.Activation.RequestedAt = "2026-08-02T00:00:00Z"
	summary.Activation.ActivatedAt = "2026-08-02T00:00:30Z"
	summary.Activation.PhaseDurationMillis = 30_000
	if got := fixedGoldenFailure(validateGoldenSummary(summary, false)); got != "activation_duration_exceeded" {
		t.Fatalf("failure = %q, want activation_duration_exceeded", got)
	}
}

func TestGoldenMigrationUsesBoundedRoutePropagationWindow(t *testing.T) {
	evidence := validGoldenMigrationTransitionEvidence(
		"final-cutover", "front-migration-rollback", strings.Repeat("1", 64), strings.Repeat("2", 64),
	)
	evidence.Timestamps.ActivatedAt = "2026-08-02T00:04:59.000Z"
	evidence.DestinationProof.ObservedAt = evidence.Timestamps.ActivatedAt
	evidence.DestinationProof.ReceivedAt = "2026-08-02T00:04:59.500Z"
	evidence.Timestamps.EvidenceEmittedAt = "2026-08-02T00:04:59.750Z"
	if err := validateGoldenMigrationTransition(evidence, "front-migration-cutover"); err != nil {
		t.Fatalf("sub-five-minute route propagation was rejected: %v", err)
	}

	evidence.Timestamps.ActivatedAt = "2026-08-02T00:05:00.000Z"
	evidence.DestinationProof.ObservedAt = evidence.Timestamps.ActivatedAt
	evidence.DestinationProof.ReceivedAt = evidence.Timestamps.ActivatedAt
	evidence.Timestamps.EvidenceEmittedAt = "2026-08-02T00:05:00.001Z"
	if err := validateGoldenMigrationTransition(evidence, "front-migration-cutover"); err == nil {
		t.Fatal("five-minute route propagation boundary was accepted")
	}
}

func TestGoldenMigrationPreparationRequiresCompatibleBootstrapAndStableBackendHealth(t *testing.T) {
	document := func() map[string]any {
		evidence := validGoldenMigrationPreparationEvidence()
		evidence.EvidenceEmittedAt = "2026-08-02T00:05:01Z"
		encoded, err := json.Marshal(evidence)
		if err != nil {
			t.Fatal(err)
		}
		var result map[string]any
		if err := json.Unmarshal(encoded, &result); err != nil {
			t.Fatal(err)
		}
		result["bootstrap"] = map[string]any{
			"tag":                  "v0.1.60",
			"sha256":               "6a8daa1361030311bdbe25a06cd4940e4dd07a45758c13c2dc8d687e70d87303",
			"source_revision":      "e169e94f2bea9a0455a5831631fcbac220bd65f2",
			"tag_on_main":          true,
			"attestation_verified": true,
			"immutable":            true,
		}
		front := result["front"].(map[string]any)
		front["worker_checksum"] = "6a8daa1361030311bdbe25a06cd4940e4dd07a45758c13c2dc8d687e70d87303"
		front["backend_health"] = map[string]any{
			"all_healthy":       true,
			"stable_since":      "2026-08-02T00:00:00Z",
			"verified_at":       "2026-08-02T00:05:00Z",
			"duration_ms":       300_000,
			"healthy_samples":   61,
			"max_sample_gap_ms": 5_000,
		}
		return result
	}
	validateGo := func(value map[string]any) error {
		encoded, err := json.Marshal(value)
		if err != nil {
			return err
		}
		var evidence goldenMigrationEvidence
		if err := json.Unmarshal(encoded, &evidence); err != nil {
			return err
		}
		return validateGoldenMigrationEvidence(&evidence, "front-migration-preparation")
	}
	validatePython := func(value map[string]any) error {
		encoded, err := json.Marshal(value)
		if err != nil {
			return err
		}
		path := filepath.Join(t.TempDir(), "preparation.json")
		if err := os.WriteFile(path, encoded, 0o600); err != nil {
			return err
		}
		validator := filepath.Join("..", "..", "deploy", "gcp", "validate-deploy-evidence.py")
		return exec.Command("python3", validator, "--expect", "front-migration-preparation", path).Run()
	}
	clone := func(value map[string]any) map[string]any {
		encoded, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		var result map[string]any
		if err := json.Unmarshal(encoded, &result); err != nil {
			t.Fatal(err)
		}
		return result
	}

	valid := document()
	if err := validateGo(valid); err != nil {
		t.Fatalf("Go validator rejected five minutes of compatible front readiness: %v", err)
	}
	if err := validatePython(valid); err != nil {
		t.Fatalf("Python validator rejected five minutes of compatible front readiness: %v", err)
	}

	missing := clone(valid)
	delete(missing["front"].(map[string]any), "backend_health")
	if err := validateGo(missing); err == nil {
		t.Fatal("Go validator accepted preparation without backend readiness evidence")
	}
	if err := validatePython(missing); err == nil {
		t.Fatal("Python validator accepted preparation without backend readiness evidence")
	}

	short := clone(valid)
	health := short["front"].(map[string]any)["backend_health"].(map[string]any)
	health["verified_at"] = "2026-08-02T00:04:59.999Z"
	health["duration_ms"] = 299_999
	if err := validateGo(short); err == nil {
		t.Fatal("Go validator accepted a sub-five-minute backend readiness window")
	}
	if err := validatePython(short); err == nil {
		t.Fatal("Python validator accepted a sub-five-minute backend readiness window")
	}

	negativeGap := clone(valid)
	negativeGap["front"].(map[string]any)["backend_health"].(map[string]any)["max_sample_gap_ms"] = -1
	if err := validateGo(negativeGap); err == nil {
		t.Fatal("Go validator accepted a negative backend health sample gap")
	}
	if err := validatePython(negativeGap); err == nil {
		t.Fatal("Python validator accepted a negative backend health sample gap")
	}

	oldBootstrap := clone(valid)
	oldBootstrap["bootstrap"] = map[string]any{
		"tag":                  "v0.1.55",
		"sha256":               "6261bda248a6afc84079ecd22ded35e71d3b4cfb5267a6db2871a35cdcf0bd0c",
		"source_revision":      "c4ea17e91ef6e9d0ab31cdd2774ca8d5387219bc",
		"tag_on_main":          true,
		"attestation_verified": true,
		"immutable":            true,
	}
	oldBootstrap["front"].(map[string]any)["worker_checksum"] = "6261bda248a6afc84079ecd22ded35e71d3b4cfb5267a6db2871a35cdcf0bd0c"
	if err := validateGo(oldBootstrap); err == nil {
		t.Fatal("Go validator accepted the hosted-tenant-incompatible v0.1.55 bootstrap")
	}
	if err := validatePython(oldBootstrap); err == nil {
		t.Fatal("Python validator accepted the hosted-tenant-incompatible v0.1.55 bootstrap")
	}
}

func TestGoldenSummaryRejectsStalledOriginalStream(t *testing.T) {
	summary := validGoldenAcceptanceSummary()
	for index := range summary.Sessions {
		if summary.Sessions[index].Label == "rehearsal-direct-websocket" {
			summary.Sessions[index].MaxChunkGapMillis = 5_001
			summary.Sessions[index].DeployMaxChunkGapMillis = 5_001
		}
	}
	if got := fixedGoldenFailure(validateGoldenSummary(summary, false)); got != "session_evidence_incomplete" {
		t.Fatalf("failure = %q, want session_evidence_incomplete", got)
	}
}

func TestGoldenSummaryRequiresFinalCandidateSocketContinuityEvidence(t *testing.T) {
	tests := []struct {
		name string
		edit func(*goldenSummary)
	}{
		{
			name: "missing post-retirement snapshot",
			edit: func(summary *goldenSummary) {
				filtered := summary.ProcessSnapshots[:0]
				for _, item := range summary.ProcessSnapshots {
					if item.Phase != "final-candidate-after-retirement" || item.Label != "final-candidate-direct" {
						filtered = append(filtered, item)
					}
				}
				summary.ProcessSnapshots = filtered
			},
		},
		{
			name: "post-retirement socket changed",
			edit: func(summary *goldenSummary) {
				for index := range summary.ProcessSnapshots {
					item := &summary.ProcessSnapshots[index]
					if item.Phase == "final-candidate-after-retirement" && item.Label == "final-candidate-direct" {
						item.SocketIDs = []string{strings.Repeat("9", 64)}
					}
				}
			},
		},
		{
			name: "snapshot predates retirement",
			edit: func(summary *goldenSummary) {
				for index := range summary.ProcessSnapshots {
					item := &summary.ProcessSnapshots[index]
					if item.Phase == "final-candidate-after-retirement" && item.Label == "final-candidate-direct" {
						item.Timestamp = "2026-08-01T00:00:00Z"
					}
				}
			},
		},
		{
			name: "stable flag missing",
			edit: func(summary *goldenSummary) {
				for index := range summary.Sessions {
					if summary.Sessions[index].Label == "final-candidate-direct" {
						summary.Sessions[index].TransportSocketStable = false
					}
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			summary := validGoldenAcceptanceSummary()
			test.edit(&summary)
			if got := fixedGoldenFailure(validateGoldenSummary(summary, false)); got != "final_candidate_socket_continuity_invalid" {
				t.Fatalf("failure = %q, want final_candidate_socket_continuity_invalid", got)
			}
		})
	}
}

func TestGoldenSummaryOrdersCandidateSnapshotByLocalRetirementCompletion(t *testing.T) {
	summary := validGoldenAcceptanceSummary()
	summary.FinalOldGenerationCleanup.AbsentAt = "2030-08-02T00:00:01Z"
	if err := validateGoldenSummary(summary, false); err != nil {
		t.Fatalf("cross-host absent_at clock skew rejected valid local ordering: %v", err)
	}
}

func TestGoldenActionRejectsSleepOnlySuccess(t *testing.T) {
	script := filepath.Join(t.TempDir(), "sleep-only.sh")
	writeGoldenExecutable(t, script, "#!/bin/sh\nsleep 0.01\n")
	runner := &goldenRunner{
		artifactDir: t.TempDir(), testMode: true,
		options:  goldenOptions{evidenceValidator: "/usr/bin/true"},
		evidence: &jsonlRecorder{writer: io.Discard},
	}
	result := runner.runEvidenceAction(context.Background(), goldenEvidenceActionOptions{
		label: "sleep-only", argv: []string{script}, expect: "slot-activation", evidenceName: "activation.json",
	})
	if result.ExitCode == 0 {
		t.Fatal("accepted a successful sleep without canonical file evidence")
	}
}

func TestGoldenEvidenceRejectsRestartAndOOMDeltas(t *testing.T) {
	for _, test := range []struct {
		name string
		edit func(*goldenDeployEvidence)
	}{
		{name: "restart", edit: func(e *goldenDeployEvidence) { *e.Metrics.OldSlot.NRestarts.After = 1 }},
		{name: "oom", edit: func(e *goldenDeployEvidence) { *e.Metrics.CandidateSlot.OOMKill.After = 1 }},
	} {
		t.Run(test.name, func(t *testing.T) {
			evidence := validGoldenActivationEvidence()
			test.edit(evidence)
			if err := validateGoldenDeployEvidence(evidence, "slot-activation"); err == nil {
				t.Fatalf("accepted %s delta", test.name)
			}
		})
	}
}

func TestGoldenEvidenceRejectsMissingServerRSS(t *testing.T) {
	evidence := validGoldenActivationEvidence()
	evidence.Metrics.CandidateSlot.RunScopedPeakRSSBytes = nil
	if err := validateGoldenDeployEvidence(evidence, "slot-activation"); err == nil {
		t.Fatal("accepted activation without candidate server RSS")
	}
	rollback := validGoldenRollbackEvidence()
	rollback.Metrics.RestoredSlot.RunScopedPeakRSSBytes = nil
	if err := validateGoldenDeployEvidence(rollback, "slot-rollback"); err == nil {
		t.Fatal("accepted rollback without restored server RSS")
	}
}

func TestGoldenSummaryRejectsRollbackThatDoesNotReverseActivation(t *testing.T) {
	summary := validGoldenAcceptanceSummary()
	summary.Rollback.ToGenerationIDHash = hashGoldenValue("generation-c")
	summary.Rollback.ActiveGenerationIDHash = summary.Rollback.ToGenerationIDHash
	if got := fixedGoldenFailure(validateGoldenSummary(summary, false)); got != "rollback_not_exact_reversal" {
		t.Fatalf("failure = %q, want rollback_not_exact_reversal", got)
	}
}

func TestGoldenSummaryBindsBootstrapAndCandidate(t *testing.T) {
	summary := validGoldenAcceptanceSummary()
	summary.Activation.FromReleaseSHA256 = strings.Repeat("9", 64)
	if got := fixedGoldenFailure(validateGoldenSummary(summary, false)); got != "migration_slot_candidate_mismatch" {
		t.Fatalf("failure = %q, want migration_slot_candidate_mismatch", got)
	}
	summary = validGoldenAcceptanceSummary()
	summary.FinalActivation.ToReleaseSHA256 = strings.Repeat("8", 64)
	if got := fixedGoldenFailure(validateGoldenSummary(summary, false)); got != "final_activation_candidate_changed" {
		t.Fatalf("failure = %q, want final_activation_candidate_changed", got)
	}
}

func TestGoldenEvidenceRejectsMissingRetirementLink(t *testing.T) {
	evidence := validGoldenRetirementEvidence("rollback-rehearsal")
	evidence.TransitionEvidenceSHA256 = ""
	if err := validateGoldenDeployEvidence(evidence, "slot-retirement"); err == nil {
		t.Fatal("accepted retirement without transition evidence hash")
	}
}

func TestGoldenProcessEvidenceAllowsCodexAboveServerSlotLimit(t *testing.T) {
	const observedCodexRSS int64 = 200_000 << 10
	pid := os.Getpid()
	previous := goldenTestHooks
	goldenTestHooks.enabled = true
	goldenTestHooks.processTable = func([]int) (goldenProcessTable, error) {
		return goldenProcessTable{
			processes: map[int]goldenProcessSample{pid: {parent: 1, state: "S", rss: observedCodexRSS}},
			children:  map[int][]int{},
		}, nil
	}
	goldenTestHooks.socketSnapshot = func(int) ([]byte, error) {
		return []byte("n127.0.0.1:41000->203.0.113.10:443\n"), nil
	}
	t.Cleanup(func() { goldenTestHooks = previous })

	evidence, err := captureProcessEvidence("before-activation", "direct-websocket", pid)
	if err != nil {
		t.Fatalf("rejected ordinary Codex RSS above the server slot limit: %v", err)
	}
	if evidence.RSSBytes <= goldenRSSLimitBytes {
		t.Fatalf("test process RSS = %d, want above server slot limit %d", evidence.RSSBytes, goldenRSSLimitBytes)
	}
}

func TestGoldenSummaryUsesSeparateCodexAndServerRSSBudgets(t *testing.T) {
	const observedCodexRSS int64 = 200_000 << 10
	if goldenRSSLimitBytes != 192<<20 || observedCodexRSS <= goldenRSSLimitBytes {
		t.Fatalf("invalid test budgets: server=%d observed Codex=%d", goldenRSSLimitBytes, observedCodexRSS)
	}

	summary := validGoldenAcceptanceSummary()
	for index := range summary.Sessions {
		summary.Sessions[index].PeakRSSBytes = observedCodexRSS
	}
	for index := range summary.ProcessSnapshots {
		item := &summary.ProcessSnapshots[index]
		if item.Label != "local-daemon" && !strings.HasPrefix(item.Label, "observer-") {
			item.RSSBytes = observedCodexRSS
		}
	}
	if err := validateGoldenSummary(summary, false); err != nil {
		t.Fatalf("rejected ordinary Codex RSS above the server slot limit: %v", err)
	}

	server := validGoldenActivationEvidence()
	server.Metrics.CandidateSlot.RunScopedPeakRSSBytes = goldenInt64(observedCodexRSS)
	if err := validateGoldenDeployEvidence(server, "slot-activation"); err == nil {
		t.Fatal("accepted server slot RSS above its 192 MiB MemoryMax")
	}
}

func TestGoldenContinuousSamplerDiscoversNewHeavyAndStoppedChildren(t *testing.T) {
	bin := t.TempDir()
	ps := filepath.Join(bin, "ps")
	writeGoldenExecutable(t, ps, `#!/bin/sh
case "$*" in
  *-axo*) printf '100 1 S 1024\n101 100 T 200000\n' ;;
  *) exit 1 ;;
esac
`)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	table, err := loadGoldenProcessTable(nil)
	if err != nil {
		t.Fatal(err)
	}
	rss, processes, paused, err := measureGoldenProcessTree(table, 100)
	if err != nil {
		t.Fatal(err)
	}
	if processes != 2 || rss <= goldenRSSLimitBytes || !paused {
		t.Fatalf("rss=%d processes=%d paused=%v", rss, processes, paused)
	}
}

func TestGoldenStableSocketMustBeResponseTransportSocket(t *testing.T) {
	actual := strings.Repeat("a", 64)
	unrelated := strings.Repeat("b", 64)
	stats := newObserverStats()
	stats.observe(transportEvent{
		Kind: "request_started", Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
		Method: "POST", Path: "/v1/responses", RequestID: "request-1", ConnectionID: actual,
	})
	session := &goldenSession{label: "direct-websocket", observer: &runningGoldenObserver{stats: stats}}
	before := map[string]goldenProcessEvidence{
		session.label: {Label: session.label, Phase: "before-activation", SocketIDs: []string{unrelated}},
	}
	after := map[string]goldenProcessEvidence{
		session.label: {Label: session.label, Phase: "after-activation", SocketIDs: []string{unrelated}},
	}
	if got := fixedGoldenFailure(requireStableSessionSockets([]*goldenSession{session}, before, after)); got != "session_socket_identity_changed" {
		t.Fatalf("failure = %q, want session_socket_identity_changed", got)
	}
}

func TestGoldenStableSocketRequiresDistinctSnapshots(t *testing.T) {
	transportID := strings.Repeat("a", 64)
	stats := newObserverStats()
	stats.observe(transportEvent{
		Kind: "request_started", Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
		Method: "POST", Path: "/v1/responses", RequestID: "request-1", ConnectionID: transportID,
	})
	session := &goldenSession{label: "candidate-direct", observer: &runningGoldenObserver{stats: stats}}
	snapshot := map[string]goldenProcessEvidence{
		session.label: {Label: session.label, Phase: "after-activation", SocketIDs: []string{transportID}},
	}
	if got := fixedGoldenFailure(requireStableSessionSockets([]*goldenSession{session}, snapshot, snapshot)); got != "session_socket_evidence_not_distinct" {
		t.Fatalf("failure = %q, want session_socket_evidence_not_distinct", got)
	}
}

func TestGoldenPostActivationRequestMustStartAfterObservedActivation(t *testing.T) {
	boundary := time.Now().UTC()
	stats := newObserverStats()
	stats.observe(transportEvent{
		Kind: "request_started", Timestamp: boundary.Add(-time.Second).Format(time.RFC3339Nano),
		Path: "/responses", RequestID: "request-1", ConnectionID: strings.Repeat("a", 64),
	})
	session := &goldenSession{observer: &runningGoldenObserver{stats: stats}, done: make(chan struct{})}
	if err := requireGoldenSessionStartsAfter(session, boundary); err == nil {
		t.Fatal("accepted a fresh request that began before activation")
	}
}

func TestGoldenActivationSpanRequiresChunksOnBothSides(t *testing.T) {
	boundary := time.Now().UTC()
	stats := newObserverStats()
	stats.observe(transportEvent{
		Kind: "request_started", Timestamp: boundary.Add(-time.Second).Format(time.RFC3339Nano),
		Path: "/responses", RequestID: "request-1", ConnectionID: strings.Repeat("a", 64),
	})
	stats.observe(transportEvent{Kind: "response_chunk", Timestamp: boundary.Add(-500 * time.Millisecond).Format(time.RFC3339Nano), Path: "/responses", RequestID: "request-1", Bytes: 1})
	session := &goldenSession{observer: &runningGoldenObserver{stats: stats}, done: make(chan struct{})}
	if err := requireGoldenSessionSpans(session, boundary); err == nil {
		t.Fatal("accepted a request with only old-side response bytes")
	}
}

func TestGoldenContinuityFollowsSessionAcrossMultipleResponses(t *testing.T) {
	baselineEnd := time.Now().UTC()
	stats := newObserverStats()
	stats.observe(transportEvent{
		Kind: "request_started", Timestamp: baselineEnd.Add(-time.Second).Format(time.RFC3339Nano),
		Transport: "websocket", Method: "GET", Path: "/v1/responses",
		RequestID: "request-1", ConnectionID: strings.Repeat("a", 64),
	})
	for index := goldenBaselineChunkSamples; index >= 0; index-- {
		stats.observe(transportEvent{
			Kind: "response_chunk", Timestamp: baselineEnd.Add(-time.Duration(index) * 100 * time.Millisecond).Format(time.RFC3339Nano),
			Transport: "websocket", Method: "GET", Path: "/v1/responses",
			RequestID: "request-1", ConnectionID: strings.Repeat("a", 64), Bytes: 1,
		})
	}
	session := &goldenSession{
		label: "multi-response", transport: "websocket", done: make(chan struct{}),
		observer: &runningGoldenObserver{stats: stats},
	}
	runner := &goldenRunner{evidence: &jsonlRecorder{writer: io.Discard}}
	monitors, err := startGoldenContinuityMonitors(runner, []*goldenSession{session}, baselineEnd)
	if err != nil {
		t.Fatal(err)
	}

	stats.observe(transportEvent{
		Kind: "request_started", Timestamp: baselineEnd.Add(50 * time.Millisecond).Format(time.RFC3339Nano),
		Transport: "websocket", Method: "GET", Path: "/v1/responses",
		RequestID: "request-2", ConnectionID: strings.Repeat("b", 64),
	})
	for offset := 100 * time.Millisecond; offset <= 6*time.Second; offset += 100 * time.Millisecond {
		stats.observe(transportEvent{
			Kind: "response_chunk", Timestamp: baselineEnd.Add(offset).Format(time.RFC3339Nano),
			Transport: "websocket", Method: "GET", Path: "/v1/responses",
			RequestID: "request-2", ConnectionID: strings.Repeat("b", 64), Bytes: 1,
		})
	}
	boundary := baselineEnd.Add(3 * time.Second)
	if err := waitGoldenContinuityBoundary(context.Background(), monitors, boundary); err != nil {
		cancelGoldenContinuityMonitors(monitors)
		t.Fatalf("session-level continuity rejected a normal follow-on response: %v", err)
	}
	if err := stopGoldenContinuityMonitors(monitors, baselineEnd.Add(time.Second), baselineEnd.Add(6*time.Second)); err != nil {
		t.Fatalf("session-level continuity rejected aggregate response bytes: %v", err)
	}
}

func TestGoldenLocalLeaseObserverRejectsHostedResponse(t *testing.T) {
	now := time.Now().UTC()
	stats := newObserverStats()
	stats.observe(transportEvent{Kind: "request_started", Timestamp: now.Format(time.RFC3339Nano), Path: "/responses"})
	stats.observe(transportEvent{Kind: "request_started", Timestamp: now.Format(time.RFC3339Nano), Path: "/_subrouter/leases"})
	if err := requireGoldenLeaseWindow(&runningGoldenObserver{stats: stats}, now.Add(-time.Second), now.Add(time.Second), 0); err == nil {
		t.Fatal("accepted local proof whose hosted observer saw /responses")
	}
}

func TestGoldenLocalLeaseObserverRequiresLegacyPost(t *testing.T) {
	now := time.Now().UTC()
	stats := newObserverStats()
	stats.observe(transportEvent{
		Kind: "request_started", Timestamp: now.Format(time.RFC3339Nano),
		Method: "GET", Path: "/api/subrouter/leases",
	})
	if err := requireGoldenLeaseWindow(
		&runningGoldenObserver{stats: stats},
		now.Add(-time.Second),
		now.Add(time.Second),
		0,
	); err == nil {
		t.Fatal("accepted a non-POST v0.1.51 lease request")
	}
}

func writeGoldenExecutable(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o700); err != nil {
		t.Fatal(err)
	}
}

func goldenInt64(value int64) *int64 { return &value }
func goldenBool(value bool) *bool    { return &value }

func validGoldenServiceMetrics(limit int64) goldenDeployServiceMetrics {
	return goldenDeployServiceMetrics{
		NRestarts:             goldenDeployCounter{Before: goldenInt64(0), After: goldenInt64(0)},
		OOMKill:               goldenDeployCounter{Before: goldenInt64(0), After: goldenInt64(0)},
		RunScopedPeakRSSBytes: goldenInt64(1 << 20), MemoryMaxBytes: goldenInt64(limit),
	}
}

func validGoldenActivationEvidence() *goldenDeployEvidence {
	return &goldenDeployEvidence{
		Schema: goldenDeployEvidenceSchema, EvidenceType: "slot-activation", Mode: "activation", Intent: "rehearsal", Success: true,
		Release: goldenDeployRelease{
			Tag: goldenPinnedCandidateTag, SHA256: strings.Repeat("b", 64), SourceRevision: strings.Repeat("c", 40),
			TagOnMain: true, AttestationVerified: true, Immutable: true,
		},
		Slots:     goldenDeploySlots{Before: "slot-a", Candidate: "slot-b", Final: "slot-b", OldGeneration: "generation-a", CandidateGeneration: "generation-b"},
		Checksums: goldenDeployChecksums{InstalledBefore: goldenPinnedBootstrapLinuxSHA256, CandidateInstalled: strings.Repeat("b", 64), InstalledAfter: strings.Repeat("b", 64)},
		Timestamps: goldenDeployTimestamps{
			UpgradeRequestedAt: "2026-08-02T00:00:00Z", ProvisionalSwitchAt: "2026-08-02T00:00:00.25Z",
			ActivatedAt: "2026-08-02T00:00:01Z", GoldenAckReceivedAt: "2026-08-02T00:00:01.25Z",
			EvidenceEmittedAt: "2026-08-02T00:00:01.5Z",
		},
		GoldenAck: goldenDeployGoldenAck{
			SHA256: strings.Repeat("d", 64), Challenge: strings.Repeat("e", 32), FreshCandidateConnectionID: strings.Repeat("f", 64),
			ConfiguredOriginalClients: 4, OriginalStreamsCrossed: 4, DirectOriginalConnectionsVerified: 2,
			LocalEgressClientsVerified: 2, AllOriginalStreamsCrossedActivation: true, ProcessesStable: true,
			SocketsStable: true, LocalEgressVerified: true, FreshCandidateDirectConnection: true,
			ActivatedAt: "2026-08-02T00:00:01Z", ReceivedAt: "2026-08-02T00:00:01.25Z",
		},
		Metrics: goldenDeployMetrics{OldSlot: validGoldenServiceMetrics(goldenRSSLimitBytes), CandidateSlot: validGoldenServiceMetrics(goldenRSSLimitBytes), Front: validGoldenServiceMetrics(goldenFrontRSSLimitBytes)},
		Continuity: goldenDeployContinuity{
			ConfiguredOriginalClients: goldenInt64(4), ExpectedOriginalSlotConnections: goldenInt64(2),
			PinnedOriginalConnectionsAtSwitch: goldenInt64(2), ExpectedCandidateConnectionsForRollback: goldenInt64(1),
			CandidateConnectionsBefore: goldenInt64(0), CandidateConnectionsAfterAck: goldenInt64(1),
			CandidateConnectionCountDelta: goldenInt64(1), AllExpectedSlotConnectionsPinned: goldenBool(true),
		},
	}
}

func validGoldenRollbackEvidence() *goldenDeployEvidence {
	return &goldenDeployEvidence{
		Schema: goldenDeployEvidenceSchema, EvidenceType: "slot-rollback", Mode: "rollback-rehearsal", Intent: "rehearsal", Success: true,
		ActivationEvidenceSHA256: strings.Repeat("d", 64),
		Release: goldenDeployRelease{
			Tag: goldenPinnedCandidateTag, SHA256: strings.Repeat("b", 64), SourceRevision: strings.Repeat("c", 40),
			TagOnMain: true, AttestationVerified: true, Immutable: true,
		},
		Slots:       goldenDeploySlots{From: "slot-b", To: "slot-a", Final: "slot-a", FromGeneration: "generation-b", ToGeneration: "generation-a"},
		Checksums:   goldenDeployChecksums{Candidate: strings.Repeat("b", 64), Restored: goldenPinnedBootstrapLinuxSHA256},
		Timestamps:  goldenDeployTimestamps{RollbackRequestedAt: "2026-08-02T00:00:02Z", ActivatedAt: "2026-08-02T00:00:03Z"},
		Metrics:     goldenDeployMetrics{RetiringSlot: validGoldenServiceMetrics(goldenRSSLimitBytes), RestoredSlot: validGoldenServiceMetrics(goldenRSSLimitBytes), Front: validGoldenServiceMetrics(goldenFrontRSSLimitBytes)},
		Connections: goldenDeployConnections{ExpectedExternal: goldenInt64(1), Before: goldenInt64(1), After: goldenInt64(1)},
	}
}

func validGoldenRetirementEvidence(mode string) *goldenDeployEvidence {
	return &goldenDeployEvidence{
		Schema: goldenDeployEvidenceSchema, EvidenceType: "slot-retirement", Mode: mode, Success: true,
		TransitionEvidenceSHA256: strings.Repeat("e", 64),
		Slots:                    goldenDeploySlots{Retired: "slot-b", Active: "slot-a", RetiredGeneration: "generation-b"},
		Retirement: goldenDeployRetirement{
			RequestedAt: "2026-08-02T00:00:03Z", LastConnectionClosedAt: "2026-08-02T00:00:04Z",
			AbsentAt: "2026-08-02T00:00:05Z", AbsenceLatencyMillis: goldenInt64(1_000),
		},
		Metrics: goldenDeployMetrics{OldSlot: validGoldenServiceMetrics(goldenRSSLimitBytes)},
	}
}

func validGoldenMigrationBaseEvidence(evidenceType, mode string) *goldenMigrationEvidence {
	return &goldenMigrationEvidence{
		Schema: goldenDeployEvidenceSchema, EvidenceType: evidenceType, Mode: mode, Success: true,
		Run: goldenDeployRun{ID: "golden-run", Project: "project", Zone: "zone", Instance: "instance"},
		Release: goldenDeployRelease{
			Tag: goldenPinnedCandidateTag, SHA256: strings.Repeat("b", 64), SourceRevision: strings.Repeat("c", 40),
			TagOnMain: true, AttestationVerified: true, Immutable: true,
		},
		Bootstrap: goldenDeployRelease{
			Tag: goldenPinnedBootstrapTag, SHA256: goldenPinnedBootstrapLinuxSHA256,
			SourceRevision: goldenPinnedBootstrapRevision, TagOnMain: true, AttestationVerified: true, Immutable: true,
		},
		Predecessor: goldenMigrationPredecessor{
			Tag: "v0.1.51", SHA256: goldenPinnedPredecessorLinuxSHA256,
			SourceRevision: goldenPinnedPredecessorRevision, TagOnMain: true, HardPinVerified: true,
			SHA256SumsMatch: true, EmbeddedRevisionVerified: true, LiveWorkerChecksumMatch: true,
		},
		Legacy: goldenMigrationLegacy{
			Service: "subrouter.service", Generation: "legacy-generation",
			Checksum: goldenPinnedPredecessorLinuxSHA256, AcceptingNewPublic: true,
		},
		Front: goldenMigrationFront{
			Slot: "slot-a", Generation: "front-generation", Checksum: strings.Repeat("b", 64),
			ControlChecksum: strings.Repeat("b", 64), WorkerChecksum: goldenPinnedBootstrapLinuxSHA256, Ready: true,
			BackendHealth: goldenMigrationBackendHealth{
				AllHealthy: true, StableSince: "2026-08-02T00:00:00Z", VerifiedAt: "2026-08-02T00:05:00Z",
				DurationMillis: 300_000, HealthySamples: 61, MaxSampleGapMillis: 5_000,
			},
		},
	}
}

func validGoldenMigrationPreparationEvidence() *goldenMigrationEvidence {
	evidence := validGoldenMigrationBaseEvidence("front-migration-preparation", "prepare")
	evidence.Routing = goldenMigrationRouting{
		URLMap: "url-map", LegacyBackend: "legacy-backend", FrontBackend: "front-backend",
		LegacyBackendURL: "https://example.test/legacy", FrontBackendURL: "https://example.test/front",
		Current: "legacy",
	}
	evidence.EvidenceEmittedAt = "2026-08-02T00:05:01Z"
	return evidence
}

func validGoldenMigrationTransitionEvidence(mode, priorType, priorSHA, preparationSHA string) *goldenMigrationEvidence {
	evidenceType, source, destination, expected := "front-migration-cutover", "legacy", "front", int64(2)
	if mode == "rollback" {
		evidenceType, source, destination, expected = "front-migration-rollback", "front", "legacy", 1
	}
	evidence := validGoldenMigrationBaseEvidence(evidenceType, mode)
	evidence.PriorEvidenceType = priorType
	evidence.PriorEvidenceSHA256 = priorSHA
	evidence.PreparationEvidenceSHA256 = preparationSHA
	evidence.Routing.Before, evidence.Routing.After = source, destination
	sourceGeneration, destinationGeneration := "legacy-generation", "front-generation"
	if source == "front" {
		sourceGeneration, destinationGeneration = destinationGeneration, sourceGeneration
	}
	evidence.Source = goldenMigrationTransitionSide{
		Before:                   goldenMigrationSnapshot{Kind: source, Generation: sourceGeneration, PublicConnections: expected, GenerationConnections: expected},
		After:                    goldenMigrationSnapshot{Kind: source, Generation: sourceGeneration, PublicConnections: expected, GenerationConnections: expected},
		AcceptingNewPublicBefore: true,
	}
	evidence.Destination = goldenMigrationTransitionSide{
		Before:               goldenMigrationSnapshot{Kind: destination, Generation: destinationGeneration},
		After:                goldenMigrationSnapshot{Kind: destination, Generation: destinationGeneration, PublicConnections: 1, GenerationConnections: 1},
		ConnectionCountDelta: 1,
	}
	evidence.Timestamps = goldenMigrationTimestamps{
		TransitionRequestedAt: "2026-08-02T00:00:00Z", ActivatedAt: "2026-08-02T00:00:01Z",
		EvidenceEmittedAt: "2026-08-02T00:00:02Z",
	}
	evidence.DestinationProof = goldenMigrationDestinationProof{
		SHA256: strings.Repeat("8", 64), Challenge: strings.Repeat("9", 32), ConnectionID: strings.Repeat("a", 64),
		SessionID:                  "golden-session",
		OriginalContinuityVerified: true, FreshPublicConnection: true,
		ObservedAt: "2026-08-02T00:00:01Z", ReceivedAt: "2026-08-02T00:00:01.5Z",
	}
	legacyMetrics := goldenMigrationLegacyMetrics{
		NRestarts:             goldenDeployCounter{Before: goldenInt64(0), After: goldenInt64(0)},
		OOMKill:               goldenDeployCounter{Before: goldenInt64(0), After: goldenInt64(0)},
		RunScopedPeakRSSBytes: goldenInt64(1 << 20), RSSLimitBytes: goldenInt64(goldenRSSLimitBytes),
	}
	evidence.Metrics = goldenMigrationMetrics{
		SourceService: "legacy", DestinationService: "slot", Legacy: legacyMetrics,
		Slot: goldenMigrationSlotMetrics{
			ID: "slot-a", NRestarts: goldenDeployCounter{Before: goldenInt64(0), After: goldenInt64(0)},
			OOMKill:               goldenDeployCounter{Before: goldenInt64(0), After: goldenInt64(0)},
			RunScopedPeakRSSBytes: goldenInt64(1 << 20), MemoryMaxBytes: goldenInt64(goldenRSSLimitBytes),
		},
		Front: validGoldenServiceMetrics(goldenFrontRSSLimitBytes),
	}
	if source == "front" {
		evidence.Metrics.SourceService, evidence.Metrics.DestinationService = "slot", "legacy"
	}
	evidence.Continuity = goldenMigrationContinuity{ExpectedExternalConnections: goldenInt64(expected), Preserved: goldenBool(true)}
	required, performed := mode == "rehearsal-cutover", mode == "rollback"
	evidence.Rollback = goldenMigrationRollback{Required: goldenBool(required), Performed: goldenBool(performed)}
	return evidence
}

func validGoldenLegacyRetirementEvidence(cutoverSHA, preparationSHA string) *goldenMigrationEvidence {
	evidence := validGoldenMigrationBaseEvidence("legacy-retirement", "final-cutover")
	evidence.CutoverEvidenceSHA256 = cutoverSHA
	evidence.PreparationEvidenceSHA256 = preparationSHA
	evidence.Routing = goldenMigrationRouting{Active: "front", LegacyBackendRetained: true}
	evidence.Routing.AcceptingNewPublic = false
	evidence.Connections = goldenMigrationConnections{
		Before: goldenMigrationConnectionSnapshot{Active: 1, Total: 1},
		After:  goldenMigrationConnectionSnapshot{},
	}
	evidence.Retirement = goldenMigrationRetirement{
		AcceptingNewPublicFalseAt: "2026-08-02T00:00:00Z", LastConnectionClosedAt: "2026-08-02T00:00:01Z",
		StopRequestedAt: "2026-08-02T00:00:01.25Z", AbsentAt: "2026-08-02T00:00:02Z",
		AbsenceLatencyMillis: goldenInt64(1_000), ServiceActiveAfter: goldenBool(false),
		ControlSocketPresentAfter: goldenBool(false), EnabledAfter: goldenBool(false), ServiceResult: "success",
	}
	evidence.Metrics = goldenMigrationMetrics{
		NRestarts:             goldenDeployCounter{Before: goldenInt64(0), After: goldenInt64(0)},
		OOMKill:               goldenDeployCounter{Before: goldenInt64(0), After: goldenInt64(0)},
		RunScopedPeakRSSBytes: goldenInt64(1 << 20), RSSLimitBytes: goldenInt64(goldenRSSLimitBytes),
	}
	return evidence
}

func validGoldenMigrationAction(evidence *goldenMigrationEvidence, digest, file string) goldenActionSummary {
	action := goldenActionSummary{
		EvidenceType: evidence.EvidenceType, EvidenceFile: file, EvidenceSHA256: digest,
		StartedAt: "2026-08-02T00:00:00Z", FinishedAt: "2026-08-02T00:00:01Z",
		DurationMillis: 1_000, ExitCode: 0, EvidenceValid: true, migrationCanonical: evidence,
	}
	populateGoldenMigrationActionSummary(&action, evidence)
	return action
}

func validGoldenAcceptanceSummary() goldenSummary {
	stamp := "2026-08-02T00:00:00Z"
	finished := "2026-08-02T00:00:01Z"
	releaseA := goldenPinnedPredecessorSHA256
	bootstrap := goldenPinnedBootstrapLinuxSHA256
	releaseB := strings.Repeat("b", 64)
	activationEvidence := validGoldenActivationEvidence()
	rollbackEvidence := validGoldenRollbackEvidence()
	action := goldenActionSummary{
		EvidenceType: "slot-activation", EvidenceFile: "activation.json", EvidenceSHA256: strings.Repeat("d", 64),
		Mode: "activation", StartedAt: stamp, FinishedAt: finished, DurationMillis: 1_000,
		RequestedAt: stamp, ActivatedAt: finished, PhaseDurationMillis: 1_000, ExitCode: 0, EvidenceValid: true,
		ReleaseTag: goldenPinnedCandidateTag, ReleaseSourceRevision: strings.Repeat("c", 40),
		FromSlot: "slot-a", ToSlot: "slot-b", ActiveSlot: "slot-b",
		FromGenerationIDHash: hashGoldenValue("generation-a"), ToGenerationIDHash: hashGoldenValue("generation-b"),
		ActiveGenerationIDHash: hashGoldenValue("generation-b"), FromReleaseSHA256: bootstrap, ToReleaseSHA256: releaseB,
		ServerOldPeakRSSBytes: 1 << 20, ServerNewPeakRSSBytes: 1 << 20, ServerFrontPeakRSSBytes: 1 << 20,
		canonical: activationEvidence,
	}
	rollbackEvidence.ActivationEvidenceSHA256 = action.EvidenceSHA256
	rollback := goldenActionSummary{
		EvidenceType: "slot-rollback", EvidenceFile: "rollback.json", EvidenceSHA256: strings.Repeat("e", 64),
		LinkedEvidenceSHA256: action.EvidenceSHA256, Mode: "rollback-rehearsal",
		StartedAt: stamp, FinishedAt: finished, DurationMillis: 1_000,
		RequestedAt: stamp, ActivatedAt: finished, PhaseDurationMillis: 1_000, ExitCode: 0, EvidenceValid: true,
		ReleaseTag: action.ReleaseTag, ReleaseSourceRevision: action.ReleaseSourceRevision,
		FromSlot: "slot-b", ToSlot: "slot-a", ActiveSlot: "slot-a",
		FromGenerationIDHash: action.ToGenerationIDHash, ToGenerationIDHash: action.FromGenerationIDHash,
		ActiveGenerationIDHash: action.FromGenerationIDHash, FromReleaseSHA256: releaseB, ToReleaseSHA256: bootstrap,
		ServerOldPeakRSSBytes: 1 << 20, ServerNewPeakRSSBytes: 1 << 20, ServerFrontPeakRSSBytes: 1 << 20,
		canonical: rollbackEvidence,
	}
	cleanup := func(generation, retired, active, linked, digest, mode string) goldenActionSummary {
		evidence := validGoldenRetirementEvidence(mode)
		evidence.TransitionEvidenceSHA256 = linked
		evidence.Slots = goldenDeploySlots{Retired: retired, Active: active, RetiredGeneration: generation}
		return goldenActionSummary{
			EvidenceType: "slot-retirement", EvidenceFile: digest + ".json", EvidenceSHA256: digest,
			LinkedEvidenceSHA256: linked, Mode: mode, StartedAt: stamp, FinishedAt: finished,
			DurationMillis: 1_000, ExitCode: 0, EvidenceValid: true,
			FromSlot: retired, ActiveSlot: active, OldGenerationIDHash: hashGoldenValue(generation),
			ReportedRetiredWithinMS: 1_000, ObservedRetiredWithinMS: 1_000,
			LastConnectionClosedAt: stamp, AbsentAt: finished, canonical: evidence,
		}
	}
	labels := []struct{ label, route, transport string }{}
	labels = append(labels,
		struct{ label, route, transport string }{"migration-direct-websocket", "direct-hosted", "websocket"},
		struct{ label, route, transport string }{"migration-direct-http", "direct-hosted", "http"},
		struct{ label, route, transport string }{"migration-candidate-front-rehearsal-destination-direct", "direct-hosted", "http"},
		struct{ label, route, transport string }{"migration-candidate-legacy-rollback-destination-direct", "direct-hosted", "http"},
		struct{ label, route, transport string }{"migration-candidate-front-final-destination-direct", "direct-hosted", "http"},
	)
	for _, cycle := range []string{"rehearsal", "final"} {
		labels = append(labels,
			struct{ label, route, transport string }{cycle + "-direct-websocket", "direct-hosted", "websocket"},
			struct{ label, route, transport string }{cycle + "-direct-http", "direct-hosted", "http"},
			struct{ label, route, transport string }{cycle + "-local-websocket", "local-egress", "websocket"},
			struct{ label, route, transport string }{cycle + "-local-http", "local-egress", "http"},
			struct{ label, route, transport string }{cycle + "-candidate-local", "local-egress", "websocket"},
			struct{ label, route, transport string }{cycle + "-candidate-direct", "direct-hosted", "websocket"},
		)
	}
	summary := goldenSummary{
		ReleasedVersion: goldenPinnedPredecessorVersion, ReleasedSHA256: releaseA, ExpectedPredecessorSHA256: releaseA,
		PredecessorRevision: goldenPinnedPredecessorRevision, PredecessorRevisionVerified: true,
		ReleaseChecksumVerified: true, ReleasePlatform: "darwin/arm64", Activation: action, Rollback: rollback,
		OldGenerationCleanup: cleanup("generation-b", "slot-b", "slot-a", rollback.EvidenceSHA256, strings.Repeat("1", 64), "rollback-rehearsal"),
		FinalActivation:      action, ProbeFrequencyHz: 10, FreshLocalLeaseObserved: true,
		LegacyBrokerLeaseObserved: true,
		LocalDaemonPeakRSSBytes:   1 << 20, LocalDaemonRSSSamples: 10,
		LocalDaemonProcessSamples: 10, LocalDaemonMaxSampleGapMS: 50,
	}
	summary.FinalActivation.EvidenceFile = "final-activation.json"
	summary.FinalActivation.EvidenceSHA256 = strings.Repeat("f", 64)
	summary.FinalOldGenerationCleanup = cleanup("generation-a", "slot-a", "slot-b", summary.FinalActivation.EvidenceSHA256, strings.Repeat("2", 64), "deploy")
	preparationSHA := strings.Repeat("3", 64)
	rehearsalSHA := strings.Repeat("4", 64)
	migrationRollbackSHA := strings.Repeat("5", 64)
	finalCutoverSHA := strings.Repeat("6", 64)
	summary.MigrationPreparation = validGoldenMigrationAction(validGoldenMigrationPreparationEvidence(), preparationSHA, "migration-preparation.json")
	summary.MigrationRehearsalCutover = validGoldenMigrationAction(
		validGoldenMigrationTransitionEvidence("rehearsal-cutover", "front-migration-preparation", preparationSHA, preparationSHA),
		rehearsalSHA, "migration-rehearsal.json",
	)
	summary.MigrationRollback = validGoldenMigrationAction(
		validGoldenMigrationTransitionEvidence("rollback", "front-migration-cutover", rehearsalSHA, preparationSHA),
		migrationRollbackSHA, "migration-rollback.json",
	)
	summary.MigrationFinalCutover = validGoldenMigrationAction(
		validGoldenMigrationTransitionEvidence("final-cutover", "front-migration-rollback", migrationRollbackSHA, preparationSHA),
		finalCutoverSHA, "migration-final.json",
	)
	summary.LegacyCleanup = validGoldenMigrationAction(
		validGoldenLegacyRetirementEvidence(finalCutoverSHA, preparationSHA), strings.Repeat("7", 64), "legacy-cleanup.json",
	)
	summary.LegacyCleanup.ObservedRetiredWithinMS = 1_000
	for _, label := range []string{"public-health", "public-ready", "local-health", "local-ready"} {
		summary.Health = append(summary.Health, goldenProbeSummary{Label: label, Samples: 10, MaxStartGapMillis: 100})
	}
	for index, item := range labels {
		session := goldenSessionSummary{
			Label: item.label, Route: item.route, Transport: item.transport, ProcessID: index + 1,
			ThreadIDHash: releaseA, NonceHash: releaseA, ResponseRequests: 1, ResponseConnections: 1,
			ResponseTransportSocket: releaseA, TransportSocketStable: true, ResponseBytes: 1,
			MaxChunkGapMillis: 1, AllowedChunkGapMillis: 5_000, DeployMaxChunkGapMillis: 1,
			PeakRSSBytes: 1 << 20, RSSSamples: 10, ProcessSamples: 10, MaxProcessSampleGapMS: 50, MarkerCount: 1,
		}
		if item.route == "local-egress" {
			session.LocalUpstreamSocket = releaseA
			session.LocalEgressCorrelated = true
			session.LocalEgressSocket = releaseB
		}
		if !strings.Contains(item.label, "-candidate-") {
			session.ResponseRequests = 2
			session.ResponseConnections = 2
			session.ResumeMarkerCount = 1
			session.ResumeNonceCount = 1
			session.SocketIDsBefore = []string{releaseA}
			session.SocketIDsAfterRollback = []string{releaseA}
		}
		summary.Sessions = append(summary.Sessions, session)
	}
	for _, phase := range []string{"migration-before-rehearsal-cutover", "migration-after-final-cutover"} {
		migrationLabels := []string{
			"migration-direct-websocket", "migration-direct-http", "local-daemon",
		}
		if phase == "migration-after-final-cutover" {
			migrationLabels = append(migrationLabels,
				"migration-candidate-front-rehearsal-destination-direct",
				"migration-candidate-legacy-rollback-destination-direct",
				"migration-candidate-front-final-destination-direct",
			)
		}
		for _, label := range migrationLabels {
			evidence := goldenProcessEvidence{
				Timestamp: stamp, Phase: phase, Label: label, ProcessID: 1,
				DescendantPIDs: []int{1}, ProcessStates: []string{"S"}, SocketIDs: []string{releaseA}, RSSBytes: 1 << 20,
			}
			if label == "local-daemon" {
				evidence.RemoteSocketIDs = []string{releaseA}
			}
			summary.ProcessSnapshots = append(summary.ProcessSnapshots, evidence)
		}
	}
	for _, phase := range []string{
		"rehearsal-before-activation", "rehearsal-after-activation", "rehearsal-after-rollback",
		"final-before-activation", "final-after-activation",
	} {
		cycle := strings.SplitN(phase, "-", 2)[0]
		initial := []string{cycle + "-direct-websocket", cycle + "-direct-http", cycle + "-local-websocket", cycle + "-local-http", "local-daemon"}
		if !strings.HasSuffix(phase, "before-activation") {
			initial = append(initial, cycle+"-candidate-direct", cycle+"-candidate-local")
		}
		for _, label := range initial {
			evidence := goldenProcessEvidence{
				Timestamp: stamp, Phase: phase, Label: label, ProcessID: 1,
				DescendantPIDs: []int{1}, ProcessStates: []string{"S"}, SocketIDs: []string{releaseA}, RSSBytes: 1 << 20,
			}
			if label == "local-daemon" {
				evidence.RemoteSocketIDs = []string{releaseA}
			}
			summary.ProcessSnapshots = append(summary.ProcessSnapshots, evidence)
		}
	}
	summary.ProcessSnapshots = append(summary.ProcessSnapshots, goldenProcessEvidence{
		Timestamp: finished, Phase: "final-candidate-after-retirement", Label: "final-candidate-direct", ProcessID: 1,
		DescendantPIDs: []int{1}, ProcessStates: []string{"S"}, SocketIDs: []string{releaseA}, RSSBytes: 1 << 20,
	})
	return summary
}

func TestGoldenAcceptanceSummaryFixtureIsValid(t *testing.T) {
	if err := validateGoldenSummary(validGoldenAcceptanceSummary(), false); err != nil {
		t.Fatalf("valid fixture rejected: %v", err)
	}
}
