package main

import (
	"bufio"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

var (
	noncePattern  = regexp.MustCompile(`(?:fresh_)?nonce_[0-9a-f]+`)
	markerPattern = regexp.MustCompile(`SR_GOLDEN_(?:COMPLETE|FRESH|RESUME)_[0-9a-f]+`)
	linePattern   = regexp.MustCompile(`Then output ([0-9]+) numbered lines`)
)

func main() {
	if len(os.Args) < 2 {
		os.Exit(2)
	}
	unregister, err := registerFakeProcess()
	if err != nil {
		os.Exit(2)
	}
	defer unregister()
	switch os.Args[1] {
	case "serve":
		serve(os.Args[2:])
	case "codex":
		fakeCodex(os.Args[2:])
	case "action":
		fakeAction(os.Args[2:])
	default:
		os.Exit(2)
	}
}

func fakeAction(args []string) {
	if len(args) == 0 || os.Getenv("DEPLOY_ENV_SECRET") != "DEPLOY_ENV_VALUE_SECRET" {
		os.Exit(9)
	}
	operation := args[0]
	if logPath := os.Getenv("ACTION_LOG"); logPath != "" {
		file, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			os.Exit(9)
		}
		_, _ = fmt.Fprintln(file, operation)
		_ = file.Close()
	}
	delay := 10 * time.Millisecond
	if len(args) > 1 {
		if parsed, err := time.ParseDuration(args[1]); err == nil {
			delay = parsed
		}
	}
	evidencePath := argument(args, "--evidence-json")
	if evidencePath == "" {
		os.Exit(9)
	}
	requested := time.Now().UTC()
	time.Sleep(delay)
	activated := time.Now().UTC()
	predecessor := os.Getenv("FAKE_PREDECESSOR_SHA256")
	candidate := strings.Repeat("b", 64)
	revision := strings.Repeat("c", 40)
	metric := func(limit int64) map[string]any {
		return map[string]any{
			"nrestarts":                 map[string]any{"before": 0, "after": 0},
			"oom_kill":                  map[string]any{"before": 0, "after": 0},
			"run_scoped_peak_rss_bytes": 1 << 20, "memory_max_bytes": limit,
		}
	}
	backend := func(slot string) map[string]any {
		address := map[string]string{"slot-a": "127.0.0.1:31417", "slot-b": "127.0.0.1:31418"}[slot]
		return map[string]any{"id": slot, "network": "tcp", "address": address}
	}
	snapshot := func(generation string, accepting, retiring, frontActive bool, connections int) map[string]any {
		return map[string]any{
			"accepting": accepting, "retiring": retiring, "front_active": frontActive,
			"active_generation": generation, "active_connections": connections,
			"inactive_connections": 0, "service_active": true,
		}
	}
	var evidence map[string]any
	releaseStreams := false
	switch operation {
	case "migration-prepare":
		evidence = fakeMigrationPreparation(candidate, revision)
	case "migration-switch":
		migrationOperation := argument(args, "--operation")
		releaseStreams = migrationOperation == "final-cutover"
		priorPath := argument(args, "--prior-evidence")
		requestPath := argument(args, "--destination-proof-request")
		proofPath := argument(args, "--destination-proof")
		if priorPath == "" || requestPath == "" || proofPath == "" {
			os.Exit(9)
		}
		var prior map[string]any
		priorData, err := os.ReadFile(priorPath)
		if err != nil || json.Unmarshal(priorData, &prior) != nil {
			os.Exit(9)
		}
		priorType, _ := prior["evidence_type"].(string)
		priorSHA := fakeFileSHA256(priorPath)
		preparationSHA := priorSHA
		if value, ok := prior["preparation_evidence_sha256"].(string); ok {
			preparationSHA = value
		}
		source, destination, expected, evidenceType, mode := "legacy", "front", 2, "front-migration-cutover", migrationOperation
		if migrationOperation == "rollback" {
			source, destination, expected, evidenceType, mode = "front", "legacy", 1, "front-migration-rollback", "rollback"
		} else if migrationOperation != "rehearsal-cutover" && migrationOperation != "final-cutover" {
			os.Exit(9)
		}
		challengeByte := "3"
		if migrationOperation == "rollback" {
			challengeByte = "4"
		} else if migrationOperation == "final-cutover" {
			challengeByte = "5"
		}
		challenge := strings.Repeat(challengeByte, 32)
		sourceGeneration, destinationGeneration := "legacy-generation", "front-generation"
		if source == "front" {
			sourceGeneration, destinationGeneration = destinationGeneration, sourceGeneration
		}
		proofRequest := map[string]any{
			"schema": "subrouter.gcp.destination-proof-request/v1", "challenge": challenge,
			"operation": migrationOperation, "destination": destination, "destination_generation": destinationGeneration,
			"source": source, "source_generation": sourceGeneration, "source_snapshot_sha256": strings.Repeat("9", 64),
			"expected_source_connections": expected, "transition_requested_at": requested.Format(time.RFC3339Nano),
		}
		if fakeWriteJSON(requestPath, proofRequest) != nil {
			os.Exit(9)
		}
		proofData, err := fakeWaitFile(proofPath, 10*time.Second)
		if err != nil {
			os.Exit(9)
		}
		var proof struct {
			Schema       string `json:"schema"`
			Challenge    string `json:"challenge"`
			ConnectionID string `json:"connection_id"`
			ObservedAt   string `json:"observed_at"`
		}
		if json.Unmarshal(proofData, &proof) != nil || proof.Schema != "subrouter.gcp.destination-proof/v1" || proof.Challenge != challenge {
			os.Exit(9)
		}
		activated, err = time.Parse(time.RFC3339Nano, proof.ObservedAt)
		if err != nil {
			os.Exit(9)
		}
		proofReceived := time.Now().UTC()
		proofDigest := sha256.Sum256(proofData)
		predecessorLinux := "99fcd10d912184c160370eb228b382795101f2b5b2467244f995aa2d10b0c323"
		legacy := map[string]any{"service": "subrouter.service", "generation": "legacy-generation", "checksum": predecessorLinux}
		front := map[string]any{"slot": "slot-a", "generation": "front-generation", "checksum": candidate, "control_checksum": candidate, "worker_checksum": predecessorLinux}
		snapshot := func(kind, generation string, count int) map[string]any {
			return map[string]any{"kind": kind, "generation": generation, "public_connections": count, "generation_connections": count, "inactive_connections": 0}
		}
		legacyMetric := map[string]any{"nrestarts": map[string]any{"before": 0, "after": 0}, "oom_kill": map[string]any{"before": 0, "after": 0}, "run_scoped_peak_rss_bytes": 1 << 20, "rss_limit_bytes": 192 << 20}
		slotMetric := metric(192 << 20)
		slotMetric["id"] = "slot-a"
		evidence = map[string]any{
			"schema": "subrouter.gcp.deploy-evidence/v1", "evidence_type": evidenceType, "mode": mode, "success": true,
			"prior_evidence_type": priorType, "prior_evidence_sha256": priorSHA, "preparation_evidence_sha256": preparationSHA,
			"run":     map[string]any{"id": "golden-migration", "project": "test-project", "zone": "test-zone", "instance": "test-instance"},
			"release": fakeMigrationRelease(candidate, revision), "predecessor": fakeMigrationPredecessor(),
			"routing": map[string]any{
				"url_map": "test-map", "legacy_backend": "legacy-backend", "front_backend": "front-backend",
				"legacy_backend_url": "https://legacy.test", "front_backend_url": "https://front.test",
				"before": source, "after": destination,
				"source_backend_url":      map[string]string{"legacy": "https://legacy.test", "front": "https://front.test"}[source],
				"destination_backend_url": map[string]string{"legacy": "https://legacy.test", "front": "https://front.test"}[destination],
			},
			"legacy": legacy, "front": front,
			"timestamps": map[string]any{"transition_requested_at": requested.Format(time.RFC3339Nano), "activated_at": activated.Format(time.RFC3339Nano), "evidence_emitted_at": time.Now().UTC().Format(time.RFC3339Nano)},
			"destination_proof": map[string]any{
				"sha256": fmt.Sprintf("%x", proofDigest[:]), "challenge": challenge, "connection_id": proof.ConnectionID,
				"original_continuity_verified": true, "fresh_public_connection": true,
				"observed_at": activated.Format(time.RFC3339Nano), "received_at": proofReceived.Format(time.RFC3339Nano),
			},
			"source": map[string]any{
				"before": snapshot(source, sourceGeneration, expected), "after": snapshot(source, sourceGeneration, expected),
				"accepting_new_public_before": true, "accepting_new_public_after": false,
			},
			"destination": map[string]any{
				"before": snapshot(destination, destinationGeneration, 0), "after": snapshot(destination, destinationGeneration, 1),
				"connection_count_delta": 1,
			},
			"metrics": map[string]any{
				"source_service":      map[string]string{"legacy": "legacy", "front": "slot"}[source],
				"destination_service": map[string]string{"legacy": "legacy", "front": "slot"}[destination],
				"legacy":              legacyMetric, "slot": slotMetric, "front": metric(128 << 20),
			},
			"continuity": map[string]any{"expected_external_connections": expected, "preserved": true},
			"rollback":   map[string]any{"required": migrationOperation == "rehearsal-cutover", "performed": migrationOperation == "rollback"},
		}
	case "legacy-cleanup":
		cutoverPath := argument(args, "--cutover-evidence")
		data, err := os.ReadFile(cutoverPath)
		if err != nil {
			os.Exit(9)
		}
		var cutover map[string]any
		if json.Unmarshal(data, &cutover) != nil {
			os.Exit(9)
		}
		preparationSHA, _ := cutover["preparation_evidence_sha256"].(string)
		acceptingFalse := activated
		if timestamps, ok := cutover["timestamps"].(map[string]any); ok {
			if raw, ok := timestamps["activated_at"].(string); ok {
				acceptingFalse, _ = time.Parse(time.RFC3339Nano, raw)
			}
		}
		closed := time.Now().UTC()
		stopRequested := closed
		time.Sleep(10 * time.Millisecond)
		absent := time.Now().UTC()
		evidence = map[string]any{
			"schema": "subrouter.gcp.deploy-evidence/v1", "evidence_type": "legacy-retirement", "mode": "final-cutover", "success": true,
			"cutover_evidence_sha256": fakeFileSHA256(cutoverPath), "preparation_evidence_sha256": preparationSHA,
			"run":     map[string]any{"id": "golden-migration-cleanup", "project": "test-project", "zone": "test-zone", "instance": "test-instance"},
			"release": fakeMigrationRelease(candidate, revision), "predecessor": fakeMigrationPredecessor(),
			"routing":     map[string]any{"active": "front", "legacy_backend_retained": true, "accepting_new_public": false},
			"legacy":      map[string]any{"service": "subrouter.service", "generation": "legacy-generation", "checksum": "99fcd10d912184c160370eb228b382795101f2b5b2467244f995aa2d10b0c323"},
			"connections": map[string]any{"before": map[string]any{"active": 0, "inactive": 0, "total": 0}, "after": map[string]any{"active": 0, "inactive": 0, "total": 0}},
			"retirement": map[string]any{
				"accepting_new_public_false_at": acceptingFalse.Format(time.RFC3339Nano), "last_connection_closed_at": closed.Format(time.RFC3339Nano),
				"stop_requested_at": stopRequested.Format(time.RFC3339Nano), "absent_at": absent.Format(time.RFC3339Nano), "absence_latency_ms": absent.Sub(closed).Milliseconds(),
				"service_active_after": false, "control_socket_present_after": false, "enabled_after": false, "service_result": "success",
			},
			"metrics":             map[string]any{"nrestarts": map[string]any{"before": 0, "after": 0}, "oom_kill": map[string]any{"before": 0, "after": 0}, "run_scoped_peak_rss_bytes": 1 << 20, "rss_limit_bytes": 192 << 20},
			"evidence_emitted_at": time.Now().UTC().Format(time.RFC3339Nano),
		}
	case "activation":
		intent := argument(args, "--intent")
		releaseStreams = intent == "final"
		requestPath := argument(args, "--golden-ack-request")
		ackPath := argument(args, "--golden-ack")
		if (intent != "rehearsal" && intent != "final") || requestPath == "" || ackPath == "" {
			os.Exit(9)
		}
		challenge := strings.Repeat("a", 31) + map[bool]string{true: "1", false: "2"}[intent == "rehearsal"]
		request := map[string]any{
			"schema":    "subrouter.gcp.slot-activation-ack-request/v1",
			"challenge": challenge,
			"run":       map[string]any{"id": "golden-test", "project": "test-project", "zone": "test-zone", "instance": "test-instance"},
			"slots": map[string]any{
				"old": "slot-a", "candidate": "slot-b",
				"old_generation": "generation-a", "candidate_generation": "generation-b",
			},
			"configured_original_clients":                 4,
			"expected_original_slot_connections":          2,
			"expected_fresh_candidate_direct_connections": 1,
			"upgrade_requested_at":                        requested.Format(time.RFC3339Nano),
			"provisional_switch_at":                       activated.Format(time.RFC3339Nano),
		}
		if fakeWriteJSON(requestPath, request) != nil {
			os.Exit(9)
		}
		ackData, err := fakeWaitFile(ackPath, 10*time.Second)
		if err != nil {
			os.Exit(9)
		}
		var ack struct {
			Schema      string `json:"schema"`
			Challenge   string `json:"challenge"`
			ActivatedAt string `json:"activated_at"`
		}
		if json.Unmarshal(ackData, &ack) != nil || ack.Schema != "subrouter.gcp.slot-activation-ack/v1" || ack.Challenge != challenge {
			os.Exit(9)
		}
		var ackEvidence map[string]any
		if json.Unmarshal(ackData, &ackEvidence) != nil {
			os.Exit(9)
		}
		activated, err = time.Parse(time.RFC3339Nano, ack.ActivatedAt)
		if err != nil {
			os.Exit(9)
		}
		ackReceived := time.Now().UTC()
		ackDigest := sha256.Sum256(ackData)
		ackEvidence["sha256"] = fmt.Sprintf("%x", ackDigest[:])
		ackEvidence["received_at"] = ackReceived.Format(time.RFC3339Nano)
		evidence = map[string]any{
			"schema": "subrouter.gcp.deploy-evidence/v1", "evidence_type": "slot-activation", "mode": "activation", "intent": intent, "success": true,
			"run":       map[string]any{"id": "golden-test", "project": "test-project", "zone": "test-zone", "instance": "test-instance"},
			"release":   map[string]any{"tag": "v1.2.4", "sha256": candidate, "source_revision": revision, "tag_on_main": true, "attestation_verified": true, "immutable": true},
			"slots":     map[string]any{"before": "slot-a", "candidate": "slot-b", "final": "slot-b", "old_generation": "generation-a", "candidate_generation": "generation-b"},
			"checksums": map[string]any{"installed_before": predecessor, "candidate_installed": candidate, "installed_after": candidate},
			"timestamps": map[string]any{
				"upgrade_requested_at": requested.Format(time.RFC3339Nano), "provisional_switch_at": request["provisional_switch_at"],
				"activated_at": activated.Format(time.RFC3339Nano), "golden_ack_received_at": ackReceived.Format(time.RFC3339Nano),
				"evidence_emitted_at": time.Now().UTC().Format(time.RFC3339Nano),
			},
			"golden_ack": ackEvidence,
			"front": map[string]any{
				"active_before": backend("slot-a"), "active_after": backend("slot-b"), "active_final": backend("slot-b"),
			},
			"old_slot": map[string]any{
				"before": snapshot("generation-a", true, false, true, 2),
				"after":  snapshot("generation-a", true, false, false, 2),
			},
			"metrics": map[string]any{"old_slot": metric(192 << 20), "candidate_slot": metric(192 << 20), "front": metric(128 << 20)},
			"continuity": map[string]any{
				"configured_original_clients": 4, "expected_original_slot_connections": 2,
				"pinned_original_connections_at_switch": 2, "expected_candidate_connections_for_rollback": 1,
				"candidate_connections_before": 0, "candidate_connections_after_ack": 1,
				"candidate_connection_count_delta": 1, "all_expected_slot_connections_pinned": true,
				"transports": []string{}, "resumed_contexts": 0, "resume_nonce_verified": false,
				"ci_evidence_role": "supplemental", "golden_gate_role": "external-required",
			},
			"rollback": map[string]any{
				"performed": false, "requested_at": nil, "activated_at": nil, "from": nil, "to": nil,
			},
			"retirement": map[string]any{
				"target": "slot-a", "requested_at": nil, "state": "not-requested", "evidence_file_required": true,
			},
		}
	case "rollback":
		releaseStreams = true
		activationPath := argument(args, "--activation-evidence")
		activationSHA := fakeFileSHA256(activationPath)
		evidence = map[string]any{
			"schema": "subrouter.gcp.deploy-evidence/v1", "evidence_type": "slot-rollback", "mode": "rollback-rehearsal", "intent": "rehearsal", "success": true,
			"activation_evidence_sha256": activationSHA,
			"run":                        map[string]any{"id": "golden-test-rollback", "project": "test-project", "zone": "test-zone", "instance": "test-instance"},
			"release":                    map[string]any{"tag": "v1.2.4", "sha256": candidate, "source_revision": revision, "tag_on_main": true, "attestation_verified": true, "immutable": true},
			"slots":                      map[string]any{"from": "slot-b", "to": "slot-a", "final": "slot-a", "from_generation": "generation-b", "to_generation": "generation-a"},
			"checksums":                  map[string]any{"candidate": candidate, "restored": predecessor},
			"timestamps":                 map[string]any{"rollback_requested_at": requested.Format(time.RFC3339Nano), "activated_at": activated.Format(time.RFC3339Nano), "retirement_requested_at": activated.Format(time.RFC3339Nano), "evidence_emitted_at": time.Now().UTC().Format(time.RFC3339Nano)},
			"front": map[string]any{
				"active_before": backend("slot-b"), "active_after": backend("slot-a"),
			},
			"retiring_slot": map[string]any{
				"before": snapshot("generation-b", true, false, true, 1),
				"after":  snapshot("generation-b", false, true, false, 1),
			},
			"metrics":     map[string]any{"retiring_slot": metric(192 << 20), "restored_slot": metric(192 << 20), "front": metric(128 << 20)},
			"connections": map[string]any{"expected_external": 1, "before": 1, "after": 1},
			"rollback": map[string]any{
				"performed": true, "from": "slot-b", "to": "slot-a",
				"requested_at": requested.Format(time.RFC3339Nano), "activated_at": activated.Format(time.RFC3339Nano),
			},
			"retirement": map[string]any{
				"target": "slot-b", "requested_at": activated.Format(time.RFC3339Nano),
				"state": "pending", "evidence_file_required": true,
			},
		}
	case "cleanup":
		transitionPath := argument(args, "--transition-evidence")
		data, err := os.ReadFile(transitionPath)
		if err != nil {
			os.Exit(9)
		}
		var transition struct {
			EvidenceType string `json:"evidence_type"`
		}
		if json.Unmarshal(data, &transition) != nil {
			os.Exit(9)
		}
		mode, retired, active, generation := "deploy", "slot-a", "slot-b", "generation-a"
		if transition.EvidenceType == "slot-rollback" {
			mode, retired, active, generation = "rollback-rehearsal", "slot-b", "slot-a", "generation-b"
		}
		closed := time.Now().UTC()
		time.Sleep(10 * time.Millisecond)
		absent := time.Now().UTC()
		evidence = map[string]any{
			"schema": "subrouter.gcp.deploy-evidence/v1", "evidence_type": "slot-retirement", "mode": mode, "success": true,
			"transition_evidence_sha256": fakeFileSHA256(transitionPath),
			"transition_evidence_type":   transition.EvidenceType,
			"run":                        map[string]any{"id": "golden-test-cleanup", "project": "test-project", "zone": "test-zone", "instance": "test-instance"},
			"slots":                      map[string]any{"retired": retired, "active": active, "retired_generation": generation},
			"front":                      map[string]any{"active": backend(active), "retired_connections_after": 0},
			"retirement": map[string]any{
				"requested_at": requested.Format(time.RFC3339Nano), "last_connection_closed_at": closed.Format(time.RFC3339Nano),
				"absent_at": absent.Format(time.RFC3339Nano), "absence_latency_ms": absent.Sub(closed).Milliseconds(),
				"service_active_after": false, "control_socket_present_after": false, "enabled_after": false, "service_result": "success",
			},
			"metrics":             map[string]any{"old_slot": metric(192 << 20)},
			"evidence_emitted_at": time.Now().UTC().Format(time.RFC3339Nano),
		}
	default:
		os.Exit(9)
	}
	data, err := json.Marshal(evidence)
	if err != nil || os.WriteFile(evidencePath, append(data, '\n'), 0o600) != nil {
		os.Exit(9)
	}
	if releaseStreams && advanceFakeStreamGeneration() != nil {
		os.Exit(9)
	}
}

func registerFakeProcess() (func(), error) {
	directory := strings.TrimSpace(os.Getenv("SUBROUTER_GOLDEN_FAKE_PROCESS_STATE"))
	if directory == "" {
		return func() {}, nil
	}
	path := filepath.Join(directory, strconv.Itoa(os.Getpid()))
	temporary, err := os.CreateTemp(directory, ".process-*")
	if err != nil {
		return nil, err
	}
	temporaryPath := temporary.Name()
	removeTemporary := true
	defer func() {
		if removeTemporary {
			_ = os.Remove(temporaryPath)
		}
	}()
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return nil, err
	}
	if _, err := fmt.Fprintf(temporary, "%d %d S 1024\n", os.Getpid(), os.Getppid()); err != nil {
		temporary.Close()
		return nil, err
	}
	if err := temporary.Close(); err != nil {
		return nil, err
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return nil, err
	}
	removeTemporary = false
	if os.Getenv("SUBROUTER_GOLDEN_FAKE_PROCESS_PARENT_OWNED") == "1" {
		return func() {}, nil
	}
	return func() { _ = os.Remove(path) }, nil
}

func advanceFakeStreamGeneration() error {
	path := strings.TrimSpace(os.Getenv("SUBROUTER_GOLDEN_FAKE_STREAM_GENERATION"))
	if path == "" {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	generation, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || generation < 0 {
		return fmt.Errorf("invalid stream generation")
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".stream-generation-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return err
	}
	if _, err := fmt.Fprintf(temporary, "%d\n", generation+1); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryPath, path)
}

func fakeMigrationRelease(candidate, revision string) map[string]any {
	return map[string]any{"tag": "v1.2.4", "sha256": candidate, "source_revision": revision, "tag_on_main": true, "attestation_verified": true, "immutable": true}
}

func fakeMigrationPredecessor() map[string]any {
	return map[string]any{
		"tag": "v0.1.51", "sha256": "99fcd10d912184c160370eb228b382795101f2b5b2467244f995aa2d10b0c323",
		"source_revision": "5eacb5411c0bd4a24f4e422d6366fa7bfd1843c8", "tag_on_main": true,
		"hard_pin_verified": true, "sha256sums_match": true, "embedded_revision_verified": true, "live_worker_checksum_match": true,
	}
}

func fakeMigrationPreparation(candidate, revision string) map[string]any {
	return map[string]any{
		"schema": "subrouter.gcp.deploy-evidence/v1", "evidence_type": "front-migration-preparation", "mode": "prepare", "success": true,
		"run":     map[string]any{"id": "golden-migration-prepare", "project": "test-project", "zone": "test-zone", "instance": "test-instance"},
		"release": fakeMigrationRelease(candidate, revision), "predecessor": fakeMigrationPredecessor(),
		"routing": map[string]any{
			"url_map": "test-map", "legacy_backend": "legacy-backend", "front_backend": "front-backend",
			"legacy_backend_url": "https://legacy.test", "front_backend_url": "https://front.test", "current": "legacy",
		},
		"legacy": map[string]any{
			"service": "subrouter.service", "generation": "legacy-generation", "checksum": "99fcd10d912184c160370eb228b382795101f2b5b2467244f995aa2d10b0c323", "accepting_new_public": true,
		},
		"front": map[string]any{
			"slot": "slot-a", "generation": "front-generation", "checksum": candidate,
			"control_checksum": candidate, "worker_checksum": "99fcd10d912184c160370eb228b382795101f2b5b2467244f995aa2d10b0c323", "ready": true,
		},
		"evidence_emitted_at": time.Now().UTC().Format(time.RFC3339Nano),
	}
}

func fakeWriteJSON(path string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".golden-handshake-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(append(data, '\n')); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryPath, path)
}

func fakeWaitFile(path string, timeout time.Duration) ([]byte, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		if err == nil {
			return data, nil
		}
		if !os.IsNotExist(err) {
			return nil, err
		}
		time.Sleep(10 * time.Millisecond)
	}
	return nil, fmt.Errorf("timed out waiting for %s", path)
}

func fakeFileSHA256(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	digest := sha256.Sum256(data)
	return fmt.Sprintf("%x", digest[:])
}

func argument(args []string, name string) string {
	for index := 0; index+1 < len(args); index++ {
		if args[index] == name {
			return args[index+1]
		}
	}
	return ""
}

type fakeSocketRegistry struct {
	mu      sync.Mutex
	path    string
	next    int
	sockets map[int]string
}

func newFakeSocketRegistry(path string) *fakeSocketRegistry {
	registry := &fakeSocketRegistry{path: path, sockets: make(map[int]string)}
	registry.writeLocked()
	return registry
}

func (registry *fakeSocketRegistry) open() func() {
	if registry.path == "" {
		return func() {}
	}
	registry.mu.Lock()
	registry.next++
	id := registry.next
	registry.sockets[id] = fmt.Sprintf("127.0.0.1:%d->203.0.113.10:443", 42000+id)
	registry.writeLocked()
	registry.mu.Unlock()
	return func() {
		registry.mu.Lock()
		delete(registry.sockets, id)
		registry.writeLocked()
		registry.mu.Unlock()
	}
}

func (registry *fakeSocketRegistry) writeLocked() {
	if registry.path == "" {
		return
	}
	lines := make([]string, 0, len(registry.sockets))
	for id := 1; id <= registry.next; id++ {
		if socket := registry.sockets[id]; socket != "" {
			lines = append(lines, socket)
		}
	}
	content := strings.Join(lines, "\n")
	if content != "" {
		content += "\n"
	}
	temporary := registry.path + ".tmp"
	if os.WriteFile(temporary, []byte(content), 0o600) == nil {
		_ = os.Rename(temporary, registry.path)
	}
}

func serve(args []string) {
	address := argument(args, "--addr")
	configPath := argument(args, "--cloud-config")
	data, err := os.ReadFile(configPath)
	if err != nil {
		os.Exit(3)
	}
	var config struct {
		BaseURL string `json:"baseUrl"`
	}
	if json.Unmarshal(data, &config) != nil {
		os.Exit(3)
	}
	if pidPath := os.Getenv("SUBROUTER_GOLDEN_FAKE_DAEMON_PID"); pidPath != "" {
		if os.WriteFile(pidPath, []byte(strconv.Itoa(os.Getpid())), 0o600) != nil {
			os.Exit(3)
		}
	}
	sockets := newFakeSocketRegistry(os.Getenv("SUBROUTER_GOLDEN_FAKE_SOCKET_STATE"))
	handler := http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/_subrouter/health", "/_subrouter/ready":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("ok"))
			return
		}
		if !strings.HasSuffix(request.URL.Path, "/responses") {
			http.NotFound(w, request)
			return
		}
		closeSocket := sockets.open()
		defer closeSocket()
		leaseURL := strings.TrimRight(config.BaseURL, "/") + "/api/subrouter/leases"
		leaseRequest, _ := http.NewRequestWithContext(request.Context(), http.MethodPost, leaseURL, strings.NewReader("LEASE_REQUEST_BODY_SECRET"))
		leaseRequest.Header.Set("Authorization", "Bearer LEASE_HEADER_SECRET")
		if response, leaseErr := http.DefaultClient.Do(leaseRequest); leaseErr == nil {
			_, _ = io.Copy(io.Discard, response.Body)
			response.Body.Close()
		}
		streamResponse(w, request)
	})
	server := &http.Server{Addr: address, Handler: handler, ReadHeaderTimeout: time.Second}
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		os.Exit(4)
	}
}

func streamResponse(w http.ResponseWriter, request *http.Request) {
	lifetime := newFakeStreamLifetime(request.Header.Get("X-Golden-Short") == "1")
	if strings.EqualFold(request.Header.Get("Upgrade"), "websocket") {
		hijacker, ok := w.(http.Hijacker)
		if !ok {
			http.Error(w, "upgrade", http.StatusInternalServerError)
			return
		}
		connection, buffered, err := hijacker.Hijack()
		if err != nil {
			return
		}
		defer connection.Close()
		key := request.Header.Get("Sec-WebSocket-Key")
		digest := sha1.Sum([]byte(key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
		_, _ = fmt.Fprintf(buffered, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: %s\r\n\r\n", base64.StdEncoding.EncodeToString(digest[:]))
		_ = buffered.Flush()
		for lifetime.keepOpen() {
			if _, err := connection.Write([]byte{0x81, 0x01, 'x'}); err != nil {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	flusher, _ := w.(http.Flusher)
	for lifetime.keepOpen() {
		if _, err := w.Write([]byte("data:x\n\n")); err != nil {
			return
		}
		if flusher != nil {
			flusher.Flush()
		}
		time.Sleep(20 * time.Millisecond)
	}
}

type fakeStreamLifetime struct {
	generationPath string
	generation     string
	deadline       time.Time
	releasedAt     time.Time
}

func newFakeStreamLifetime(short bool) *fakeStreamLifetime {
	duration := 4 * time.Second
	if short {
		duration = 120 * time.Millisecond
	}
	lifetime := &fakeStreamLifetime{deadline: time.Now().Add(duration)}
	if short {
		return lifetime
	}
	path := strings.TrimSpace(os.Getenv("SUBROUTER_GOLDEN_FAKE_STREAM_GENERATION"))
	if generation, err := os.ReadFile(path); path != "" && err == nil {
		lifetime.generationPath = path
		lifetime.generation = string(generation)
	}
	return lifetime
}

func (lifetime *fakeStreamLifetime) keepOpen() bool {
	if lifetime.generationPath == "" {
		return time.Now().Before(lifetime.deadline)
	}
	generation, err := os.ReadFile(lifetime.generationPath)
	if err != nil || string(generation) == lifetime.generation {
		return err == nil
	}
	now := time.Now()
	if lifetime.releasedAt.IsZero() {
		lifetime.releasedAt = now
	}
	return now.Sub(lifetime.releasedAt) < 200*time.Millisecond
}

type fakeState struct {
	Nonce    string `json:"nonce"`
	ThreadID string `json:"thread_id"`
}

func fakeCodex(args []string) {
	resumeIndex := -1
	for index, arg := range args {
		if arg == "resume" {
			resumeIndex = index
			break
		}
	}
	prompt := args[len(args)-1]
	codexHome := os.Getenv("CODEX_HOME")
	statePath := filepath.Join(codexHome, "fake-state.json")
	var state fakeState
	if resumeIndex >= 0 {
		data, err := os.ReadFile(statePath)
		if err != nil || json.Unmarshal(data, &state) != nil {
			os.Exit(5)
		}
	} else {
		state.Nonce = noncePattern.FindString(prompt)
		hash := sha1.Sum([]byte(codexHome))
		state.ThreadID = fmt.Sprintf("thread-%x", hash[:8])
		data, _ := json.Marshal(state)
		_ = os.WriteFile(statePath, data, 0o600)
	}
	marker := markerPattern.FindString(prompt)
	if state.Nonce == "" || state.ThreadID == "" || marker == "" {
		os.Exit(6)
	}
	encoder := json.NewEncoder(os.Stdout)
	_ = encoder.Encode(map[string]any{"type": "thread.started", "thread_id": state.ThreadID})
	transport := "websocket"
	for _, arg := range args {
		if strings.Contains(arg, "supports_websockets=false") {
			transport = "http"
		}
	}
	short := strings.Contains(marker, "_RESUME_")
	if err := makeRequest(os.Getenv("SUBROUTER_CODEX_BASE_URL"), transport, short); err != nil {
		os.Exit(7)
	}
	lineCount := 0
	if match := linePattern.FindStringSubmatch(prompt); len(match) == 2 {
		lineCount, _ = strconv.Atoi(match[1])
	}
	var text strings.Builder
	text.WriteString(state.Nonce)
	for line := 1; line <= lineCount; line++ {
		_, _ = fmt.Fprintf(&text, "\n%d x", line)
	}
	text.WriteString("\n")
	text.WriteString(marker)
	_ = encoder.Encode(map[string]any{
		"type": "item.completed",
		"item": map[string]any{"type": "agent_message", "text": text.String()},
	})
}

func makeRequest(baseURL, transport string, short bool) error {
	target := strings.TrimRight(baseURL, "/") + "/responses"
	if transport == "http" {
		request, err := http.NewRequest(http.MethodPost, target, strings.NewReader("REQUEST_BODY_SECRET"))
		if err != nil {
			return err
		}
		request.Header.Set("Authorization", "Bearer REQUEST_HEADER_SECRET")
		if short {
			request.Header.Set("X-Golden-Short", "1")
		}
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			return err
		}
		_, err = io.Copy(io.Discard, response.Body)
		response.Body.Close()
		return err
	}
	parsed, err := url.Parse(target)
	if err != nil {
		return err
	}
	connection, err := net.Dial("tcp", parsed.Host)
	if err != nil {
		return err
	}
	defer connection.Close()
	shortHeader := ""
	if short {
		shortHeader = "X-Golden-Short: 1\r\n"
	}
	_, err = fmt.Fprintf(connection, "GET %s HTTP/1.1\r\nHost: %s\r\nConnection: Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Key: ZmFrZS1nb2xkZW4ta2V5\r\nAuthorization: Bearer REQUEST_HEADER_SECRET\r\n%s\r\n", parsed.RequestURI(), parsed.Host, shortHeader)
	if err != nil {
		return err
	}
	reader := bufio.NewReader(connection)
	response, err := http.ReadResponse(reader, &http.Request{Method: http.MethodGet})
	if err != nil {
		return err
	}
	if response.StatusCode != http.StatusSwitchingProtocols {
		return fmt.Errorf("upgrade status")
	}
	_, err = io.Copy(io.Discard, reader)
	return err
}
