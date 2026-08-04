#!/usr/bin/env python3
"""Fail-closed semantic validation for GCP deployment evidence.

Usage:
  validate-deploy-evidence.py --expect slot-activation evidence.json
  validate-deploy-evidence.py --expect slot-rollback evidence.json
  validate-deploy-evidence.py --expect slot-retirement evidence.json
  validate-deploy-evidence.py --expect front-migration-preparation evidence.json
  validate-deploy-evidence.py --expect front-migration-cutover evidence.json
  validate-deploy-evidence.py --expect front-migration-rollback evidence.json
  validate-deploy-evidence.py --expect legacy-retirement evidence.json
  validate-deploy-evidence.py --expect deployment-preflight evidence.json
  validate-deploy-evidence.py --expect staging-predecessor-normalization evidence.json
  validate-deploy-evidence.py --expect vm-provision evidence.json
  validate-deploy-evidence.py --expect fresh-vm-acceptance evidence.json
"""

from __future__ import annotations

import argparse
import datetime as dt
import json
import re
import sys
from pathlib import Path
from typing import Any


SCHEMA = "subrouter.gcp.deploy-evidence/v1"
MAX_EVIDENCE_BYTES = 262_144
SLOT_MEMORY_LIMIT = 201_326_592
FRONT_MEMORY_LIMIT = 134_217_728
SHA256 = re.compile(r"^[0-9a-f]{64}$")
REVISION = re.compile(r"^[0-9a-f]{40}$")
TAG = re.compile(r"^v[0-9]+\.[0-9]+\.[0-9]+(?:[.-][0-9A-Za-z.-]+)?$")
GCE_INSTANCE_ID = re.compile(r"^[1-9][0-9]{0,19}$")
SLOTS = {"slot-a", "slot-b"}
FRESH_VM_MAX_AGE = dt.timedelta(hours=2)
EXPECTATIONS = {
    "slot-activation",
    "slot-rollback",
    "slot-retirement",
    "front-migration-preparation",
    "front-migration-cutover",
    "front-migration-rollback",
    "legacy-retirement",
    "deployment-preflight",
    "staging-predecessor-normalization",
    "vm-provision",
    "fresh-vm-acceptance",
}


class EvidenceError(ValueError):
    pass


def fail(message: str) -> None:
    raise EvidenceError(message)


def obj(value: Any, path: str) -> dict[str, Any]:
    if not isinstance(value, dict):
        fail(f"{path} must be an object")
    return value


def array(value: Any, path: str) -> list[Any]:
    if not isinstance(value, list):
        fail(f"{path} must be an array")
    return value


def text(value: Any, path: str) -> str:
    if not isinstance(value, str) or not value:
        fail(f"{path} must be a non-empty string")
    return value


def boolean(value: Any, path: str) -> bool:
    if not isinstance(value, bool):
        fail(f"{path} must be a boolean")
    return value


def integer(value: Any, path: str, *, minimum: int = 0) -> int:
    if isinstance(value, bool) or not isinstance(value, int) or value < minimum:
        fail(f"{path} must be an integer >= {minimum}")
    return value


def field(parent: dict[str, Any], name: str, path: str) -> Any:
    if name not in parent:
        fail(f"{path}.{name} is required")
    return parent[name]


def exact(value: Any, expected: Any, path: str) -> None:
    if value != expected:
        fail(f"{path} must equal {expected!r}, got {value!r}")


def sha(value: Any, path: str) -> str:
    result = text(value, path)
    if not SHA256.fullmatch(result):
        fail(f"{path} must be a lowercase SHA-256")
    return result


def revision(value: Any, path: str) -> str:
    result = text(value, path)
    if not REVISION.fullmatch(result):
        fail(f"{path} must be a full lowercase Git commit")
    return result


def timestamp(value: Any, path: str) -> dt.datetime:
    result = text(value, path)
    try:
        parsed = dt.datetime.fromisoformat(result.replace("Z", "+00:00"))
    except ValueError as error:
        fail(f"{path} must be an RFC3339 timestamp: {error}")
    if parsed.tzinfo is None or parsed.utcoffset() != dt.timedelta(0):
        fail(f"{path} must be UTC")
    return parsed


def validate_instance_identity(
    value: Any,
    path: str,
    *,
    expected_created: bool | None = None,
) -> tuple[dict[str, Any], dt.datetime]:
    result = obj(value, path)
    created = boolean(field(result, "created", path), f"{path}.created")
    if expected_created is not None:
        exact(created, expected_created, f"{path}.created")
    instance_id = text(field(result, "id", path), f"{path}.id")
    if not GCE_INSTANCE_ID.fullmatch(instance_id) or int(instance_id) > (2**64 - 1):
        fail(f"{path}.id must be a positive decimal GCE instance ID")
    created_at = timestamp(
        field(result, "creation_timestamp", path),
        f"{path}.creation_timestamp",
    )
    return result, created_at


def backend(value: Any, path: str, expected_id: str | None = None) -> dict[str, Any]:
    result = obj(value, path)
    backend_id = text(field(result, "id", path), f"{path}.id")
    if expected_id is not None:
        exact(backend_id, expected_id, f"{path}.id")
    exact(field(result, "network", path), "tcp", f"{path}.network")
    address = text(field(result, "address", path), f"{path}.address")
    expected_addresses = {"slot-a": "127.0.0.1:31417", "slot-b": "127.0.0.1:31418"}
    if backend_id in expected_addresses:
        exact(address, expected_addresses[backend_id], f"{path}.address")
    return result


def validate_run(value: Any) -> dict[str, Any]:
    result = obj(value, "run")
    for name in ("id", "project", "zone", "instance"):
        text(field(result, name, "run"), f"run.{name}")
    return result


def validate_release(value: Any) -> dict[str, Any]:
    result = obj(value, "release")
    tag = text(field(result, "tag", "release"), "release.tag")
    if not TAG.fullmatch(tag):
        fail("release.tag must be an explicit version tag")
    sha(field(result, "sha256", "release"), "release.sha256")
    revision(field(result, "source_revision", "release"), "release.source_revision")
    exact(boolean(field(result, "tag_on_main", "release"), "release.tag_on_main"), True, "release.tag_on_main")
    exact(
        boolean(field(result, "attestation_verified", "release"), "release.attestation_verified"),
        True,
        "release.attestation_verified",
    )
    exact(boolean(field(result, "immutable", "release"), "release.immutable"), True, "release.immutable")
    return result


def validate_predecessor(value: Any) -> dict[str, Any]:
    result = obj(value, "predecessor")
    tag = text(field(result, "tag", "predecessor"), "predecessor.tag")
    exact(tag, "v0.1.51", "predecessor.tag")
    exact(
        sha(field(result, "sha256", "predecessor"), "predecessor.sha256"),
        "99fcd10d912184c160370eb228b382795101f2b5b2467244f995aa2d10b0c323",
        "predecessor.sha256",
    )
    exact(
        revision(field(result, "source_revision", "predecessor"), "predecessor.source_revision"),
        "5eacb5411c0bd4a24f4e422d6366fa7bfd1843c8",
        "predecessor.source_revision",
    )
    exact(
        boolean(field(result, "tag_on_main", "predecessor"), "predecessor.tag_on_main"),
        True,
        "predecessor.tag_on_main",
    )
    for name in (
        "hard_pin_verified",
        "sha256sums_match",
        "embedded_revision_verified",
        "live_worker_checksum_match",
    ):
        exact(boolean(field(result, name, "predecessor"), f"predecessor.{name}"), True, f"predecessor.{name}")
    return result


def validate_migration_bootstrap(value: Any) -> dict[str, Any]:
    result = validate_release(value)
    exact(field(result, "tag", "bootstrap"), "v0.1.55", "bootstrap.tag")
    exact(
        sha(field(result, "sha256", "bootstrap"), "bootstrap.sha256"),
        "6261bda248a6afc84079ecd22ded35e71d3b4cfb5267a6db2871a35cdcf0bd0c",
        "bootstrap.sha256",
    )
    exact(
        revision(field(result, "source_revision", "bootstrap"), "bootstrap.source_revision"),
        "c4ea17e91ef6e9d0ab31cdd2774ca8d5387219bc",
        "bootstrap.source_revision",
    )
    return result


def validate_snapshot(value: Any, path: str) -> dict[str, Any]:
    result = obj(value, path)
    boolean(field(result, "accepting", path), f"{path}.accepting")
    boolean(field(result, "retiring", path), f"{path}.retiring")
    boolean(field(result, "front_active", path), f"{path}.front_active")
    text(field(result, "active_generation", path), f"{path}.active_generation")
    integer(field(result, "active_connections", path), f"{path}.active_connections")
    integer(field(result, "inactive_connections", path), f"{path}.inactive_connections")
    boolean(field(result, "service_active", path), f"{path}.service_active")
    return result


def validate_counter(value: Any, path: str) -> None:
    result = obj(value, path)
    before = integer(field(result, "before", path), f"{path}.before")
    after = integer(field(result, "after", path), f"{path}.after")
    exact(after, before, f"{path}.after")


def validate_service_metrics(value: Any, path: str, required_limit: int) -> None:
    result = obj(value, path)
    validate_counter(field(result, "nrestarts", path), f"{path}.nrestarts")
    validate_counter(field(result, "oom_kill", path), f"{path}.oom_kill")
    peak = integer(field(result, "run_scoped_peak_rss_bytes", path), f"{path}.run_scoped_peak_rss_bytes")
    limit = integer(field(result, "memory_max_bytes", path), f"{path}.memory_max_bytes", minimum=1)
    exact(limit, required_limit, f"{path}.memory_max_bytes")
    if peak > limit:
        fail(f"{path}.run_scoped_peak_rss_bytes exceeds MemoryMax")


def validate_activation(document: dict[str, Any], expected: str) -> None:
    expected_type = "slot-activation" if expected == "slot-activation" else "slot-rollback"
    exact(field(document, "evidence_type", "root"), expected_type, "evidence_type")
    expected_mode = "activation" if expected == "slot-activation" else "rollback-rehearsal"
    exact(field(document, "mode", "root"), expected_mode, "mode")
    intent = text(field(document, "intent", "root"), "intent")
    if intent not in {"rehearsal", "final"}:
        fail("activation intent must be rehearsal or final")
    exact(boolean(field(document, "success", "root"), "success"), True, "success")
    validate_run(field(document, "run", "root"))
    release = validate_release(field(document, "release", "root"))

    slots = obj(field(document, "slots", "root"), "slots")
    before = text(field(slots, "before", "slots"), "slots.before")
    candidate = text(field(slots, "candidate", "slots"), "slots.candidate")
    final = text(field(slots, "final", "slots"), "slots.final")
    if {before, candidate} != SLOTS or before == candidate:
        fail("slots.before and slots.candidate must identify opposite slots")
    old_generation = text(field(slots, "old_generation", "slots"), "slots.old_generation")
    candidate_generation = text(field(slots, "candidate_generation", "slots"), "slots.candidate_generation")
    if old_generation == candidate_generation:
        fail("old and candidate generations must differ")

    checksums = obj(field(document, "checksums", "root"), "checksums")
    sha(field(checksums, "installed_before", "checksums"), "checksums.installed_before")
    candidate_sum = sha(field(checksums, "candidate_installed", "checksums"), "checksums.candidate_installed")
    installed_after = sha(field(checksums, "installed_after", "checksums"), "checksums.installed_after")
    exact(candidate_sum, release["sha256"], "checksums.candidate_installed")
    if checksums["installed_before"] == candidate_sum:
        fail("activation must change the installed release checksum")

    timestamps = obj(field(document, "timestamps", "root"), "timestamps")
    requested = timestamp(field(timestamps, "upgrade_requested_at", "timestamps"), "timestamps.upgrade_requested_at")
    provisional = timestamp(
        field(timestamps, "provisional_switch_at", "timestamps"),
        "timestamps.provisional_switch_at",
    )
    activated = timestamp(field(timestamps, "activated_at", "timestamps"), "timestamps.activated_at")
    ack_received = timestamp(
        field(timestamps, "golden_ack_received_at", "timestamps"),
        "timestamps.golden_ack_received_at",
    )
    emitted = timestamp(field(timestamps, "evidence_emitted_at", "timestamps"), "timestamps.evidence_emitted_at")
    if not requested <= provisional <= activated <= ack_received <= emitted:
        fail("activation timestamps are out of order")
    if activated - requested >= dt.timedelta(seconds=30) or ack_received - requested >= dt.timedelta(seconds=30):
        fail("activation exceeded the 30-second phase boundary")

    golden_ack = obj(field(document, "golden_ack", "root"), "golden_ack")
    sha(field(golden_ack, "sha256", "golden_ack"), "golden_ack.sha256")
    challenge = text(field(golden_ack, "challenge", "golden_ack"), "golden_ack.challenge")
    if not re.fullmatch(r"[0-9a-f]{32}", challenge):
        fail("golden_ack.challenge must be a 128-bit lowercase hex challenge")
    text(
        field(golden_ack, "fresh_candidate_connection_id", "golden_ack"),
        "golden_ack.fresh_candidate_connection_id",
    )
    for name, expected_value in (
        ("configured_original_clients", 4),
        ("original_streams_crossed", 4),
        ("direct_original_connections_verified", 2),
        ("local_egress_clients_verified", 2),
    ):
        exact(integer(field(golden_ack, name, "golden_ack"), f"golden_ack.{name}"), expected_value, f"golden_ack.{name}")
    for name in (
        "all_original_streams_crossed_activation",
        "processes_stable",
        "sockets_stable",
        "local_egress_verified",
        "fresh_candidate_direct_connection",
    ):
        exact(boolean(field(golden_ack, name, "golden_ack"), f"golden_ack.{name}"), True, f"golden_ack.{name}")
    exact(
        timestamp(field(golden_ack, "activated_at", "golden_ack"), "golden_ack.activated_at"),
        activated,
        "golden_ack.activated_at",
    )
    exact(
        timestamp(field(golden_ack, "received_at", "golden_ack"), "golden_ack.received_at"),
        ack_received,
        "golden_ack.received_at",
    )

    front = obj(field(document, "front", "root"), "front")
    backend(field(front, "active_before", "front"), "front.active_before", before)
    backend(field(front, "active_after", "front"), "front.active_after", candidate)
    backend(field(front, "active_final", "front"), "front.active_final", final)

    old_slot = obj(field(document, "old_slot", "root"), "old_slot")
    old_before = validate_snapshot(field(old_slot, "before", "old_slot"), "old_slot.before")
    old_after = validate_snapshot(field(old_slot, "after", "old_slot"), "old_slot.after")
    exact(old_before["accepting"], True, "old_slot.before.accepting")
    exact(old_before["retiring"], False, "old_slot.before.retiring")
    exact(old_before["front_active"], True, "old_slot.before.front_active")
    exact(old_before["active_generation"], old_generation, "old_slot.before.active_generation")
    exact(old_after["front_active"], False, "old_slot.after.front_active")
    exact(old_after["active_generation"], old_generation, "old_slot.after.active_generation")
    exact(old_before["inactive_connections"], 0, "old_slot.before.inactive_connections")
    exact(old_after["inactive_connections"], 0, "old_slot.after.inactive_connections")

    metrics = obj(field(document, "metrics", "root"), "metrics")
    validate_service_metrics(field(metrics, "old_slot", "metrics"), "metrics.old_slot", SLOT_MEMORY_LIMIT)
    validate_service_metrics(field(metrics, "candidate_slot", "metrics"), "metrics.candidate_slot", SLOT_MEMORY_LIMIT)
    validate_service_metrics(field(metrics, "front", "metrics"), "metrics.front", FRONT_MEMORY_LIMIT)

    continuity = obj(field(document, "continuity", "root"), "continuity")
    clients = integer(
        field(continuity, "configured_original_clients", "continuity"),
        "continuity.configured_original_clients",
        minimum=2,
    )
    exact(clients, 4, "continuity.configured_original_clients")
    direct_connections = integer(
        field(continuity, "expected_original_slot_connections", "continuity"),
        "continuity.expected_original_slot_connections",
        minimum=1,
    )
    exact(direct_connections, 2, "continuity.expected_original_slot_connections")
    exact(
        integer(
            field(continuity, "expected_candidate_connections_for_rollback", "continuity"),
            "continuity.expected_candidate_connections_for_rollback",
            minimum=1,
        ),
        1,
        "continuity.expected_candidate_connections_for_rollback",
    )
    candidate_before = integer(
        field(continuity, "candidate_connections_before", "continuity"),
        "continuity.candidate_connections_before",
    )
    candidate_after = integer(
        field(continuity, "candidate_connections_after_ack", "continuity"),
        "continuity.candidate_connections_after_ack",
    )
    candidate_delta = integer(
        field(continuity, "candidate_connection_count_delta", "continuity"),
        "continuity.candidate_connection_count_delta",
        minimum=1,
    )
    exact(candidate_delta, candidate_after - candidate_before, "continuity.candidate_connection_count_delta")
    pinned = integer(
        field(continuity, "pinned_original_connections_at_switch", "continuity"),
        "continuity.pinned_original_connections_at_switch",
    )
    if pinned < direct_connections:
        fail("fewer than two direct hosted originals were pinned at the switch")
    exact(
        boolean(
            field(continuity, "all_expected_slot_connections_pinned", "continuity"),
            "continuity.all_expected_slot_connections_pinned",
        ),
        True,
        "continuity.all_expected_slot_connections_pinned",
    )
    transports = set(array(field(continuity, "transports", "continuity"), "continuity.transports"))
    if expected == "slot-activation":
        exact(transports, set(), "continuity.transports")
    else:
        exact(transports, {"http", "websocket"}, "continuity.transports")
    resumed_contexts = integer(field(continuity, "resumed_contexts", "continuity"), "continuity.resumed_contexts")
    resume_nonce_verified = boolean(
        field(continuity, "resume_nonce_verified", "continuity"),
        "continuity.resume_nonce_verified",
    )
    exact(field(continuity, "ci_evidence_role", "continuity"), "supplemental", "continuity.ci_evidence_role")
    exact(
        field(continuity, "golden_gate_role", "continuity"),
        "external-required",
        "continuity.golden_gate_role",
    )

    rollback = obj(field(document, "rollback", "root"), "rollback")
    retirement = obj(field(document, "retirement", "root"), "retirement")
    if expected == "slot-activation":
        exact(final, candidate, "slots.final")
        exact(installed_after, candidate_sum, "checksums.installed_after")
        exact(old_after["accepting"], True, "old_slot.after.accepting")
        exact(old_after["retiring"], False, "old_slot.after.retiring")
        exact(resumed_contexts, 0, "continuity.resumed_contexts")
        exact(resume_nonce_verified, False, "continuity.resume_nonce_verified")
        exact(boolean(field(rollback, "performed", "rollback"), "rollback.performed"), False, "rollback.performed")
        for name in ("requested_at", "activated_at", "from", "to"):
            exact(field(rollback, name, "rollback"), None, f"rollback.{name}")
        exact(field(retirement, "target", "retirement"), before, "retirement.target")
        exact(field(retirement, "requested_at", "retirement"), None, "retirement.requested_at")
        exact(field(retirement, "state", "retirement"), "not-requested", "retirement.state")
        exact(
            boolean(field(retirement, "evidence_file_required", "retirement"), "retirement.evidence_file_required"),
            True,
            "retirement.evidence_file_required",
        )
    else:
        exact(final, before, "slots.final")
        exact(boolean(field(rollback, "performed", "rollback"), "rollback.performed"), True, "rollback.performed")
        rollback_requested = timestamp(field(rollback, "requested_at", "rollback"), "rollback.requested_at")
        rollback_activated = timestamp(field(rollback, "activated_at", "rollback"), "rollback.activated_at")
        if not activated <= rollback_requested <= rollback_activated <= emitted:
            fail("rollback timestamps are out of order")
        exact(field(rollback, "from", "rollback"), candidate, "rollback.from")
        exact(field(rollback, "to", "rollback"), before, "rollback.to")
        exact(field(retirement, "target", "retirement"), candidate, "retirement.target")
        exact(field(retirement, "state", "retirement"), "complete", "retirement.state")


def validate_slot_retirement(document: dict[str, Any]) -> None:
    exact(field(document, "evidence_type", "root"), "slot-retirement", "evidence_type")
    mode = field(document, "mode", "root")
    if mode not in {"deploy", "rollback-rehearsal"}:
        fail("mode must be deploy or rollback-rehearsal")
    exact(boolean(field(document, "success", "root"), "success"), True, "success")
    transition_type = text(field(document, "transition_evidence_type", "root"), "transition_evidence_type")
    expected_transition = "slot-activation" if mode == "deploy" else "slot-rollback"
    exact(transition_type, expected_transition, "transition_evidence_type")
    sha(field(document, "transition_evidence_sha256", "root"), "transition_evidence_sha256")
    validate_run(field(document, "run", "root"))
    slots = obj(field(document, "slots", "root"), "slots")
    retired = text(field(slots, "retired", "slots"), "slots.retired")
    active = text(field(slots, "active", "slots"), "slots.active")
    if {retired, active} != SLOTS:
        fail("slot retirement must retain the opposite active slot")
    text(field(slots, "retired_generation", "slots"), "slots.retired_generation")
    front = obj(field(document, "front", "root"), "front")
    backend(field(front, "active", "front"), "front.active", active)
    exact(integer(field(front, "retired_connections_after", "front"), "front.retired_connections_after"), 0, "front.retired_connections_after")
    retirement = obj(field(document, "retirement", "root"), "retirement")
    requested = timestamp(field(retirement, "requested_at", "retirement"), "retirement.requested_at")
    closed = timestamp(
        field(retirement, "last_connection_closed_at", "retirement"),
        "retirement.last_connection_closed_at",
    )
    absent = timestamp(field(retirement, "absent_at", "retirement"), "retirement.absent_at")
    if not requested <= closed <= absent:
        fail("retirement timestamps are out of order")
    latency = integer(field(retirement, "absence_latency_ms", "retirement"), "retirement.absence_latency_ms")
    if latency >= 30_000:
        fail("retired slot was not absent within 30 seconds")
    observed_latency = int((absent - closed).total_seconds() * 1000)
    if abs(observed_latency - latency) > 1_000:
        fail("retirement.absence_latency_ms does not match its timestamps")
    for name in ("service_active_after", "control_socket_present_after", "enabled_after"):
        exact(boolean(field(retirement, name, "retirement"), f"retirement.{name}"), False, f"retirement.{name}")
    exact(field(retirement, "service_result", "retirement"), "success", "retirement.service_result")
    metrics = obj(field(document, "metrics", "root"), "metrics")
    validate_service_metrics(field(metrics, "old_slot", "metrics"), "metrics.old_slot", SLOT_MEMORY_LIMIT)
    emitted = timestamp(field(document, "evidence_emitted_at", "root"), "evidence_emitted_at")
    if emitted < absent:
        fail("evidence was emitted before absence was observed")


def validate_slot_rollback(document: dict[str, Any]) -> None:
    exact(field(document, "evidence_type", "root"), "slot-rollback", "evidence_type")
    exact(field(document, "mode", "root"), "rollback-rehearsal", "mode")
    exact(boolean(field(document, "success", "root"), "success"), True, "success")
    sha(field(document, "activation_evidence_sha256", "root"), "activation_evidence_sha256")
    validate_run(field(document, "run", "root"))
    validate_release(field(document, "release", "root"))

    slots = obj(field(document, "slots", "root"), "slots")
    source = text(field(slots, "from", "slots"), "slots.from")
    target = text(field(slots, "to", "slots"), "slots.to")
    final = text(field(slots, "final", "slots"), "slots.final")
    if {source, target} != SLOTS:
        fail("rollback must reverse between opposite slots")
    exact(final, target, "slots.final")
    source_generation = text(field(slots, "from_generation", "slots"), "slots.from_generation")
    target_generation = text(field(slots, "to_generation", "slots"), "slots.to_generation")
    if source_generation == target_generation:
        fail("rollback source and target generations must differ")

    checksums = obj(field(document, "checksums", "root"), "checksums")
    candidate_sum = sha(field(checksums, "candidate", "checksums"), "checksums.candidate")
    restored_sum = sha(field(checksums, "restored", "checksums"), "checksums.restored")
    if candidate_sum == restored_sum:
        fail("rollback must restore different release bytes")

    timestamps = obj(field(document, "timestamps", "root"), "timestamps")
    requested = timestamp(field(timestamps, "rollback_requested_at", "timestamps"), "timestamps.rollback_requested_at")
    activated = timestamp(field(timestamps, "activated_at", "timestamps"), "timestamps.activated_at")
    retired = timestamp(
        field(timestamps, "retirement_requested_at", "timestamps"),
        "timestamps.retirement_requested_at",
    )
    emitted = timestamp(field(timestamps, "evidence_emitted_at", "timestamps"), "timestamps.evidence_emitted_at")
    if not requested <= activated <= retired <= emitted:
        fail("rollback timestamps are out of order")
    if activated - requested >= dt.timedelta(seconds=30):
        fail("rollback exceeded the 30-second phase boundary")

    front = obj(field(document, "front", "root"), "front")
    backend(field(front, "active_before", "front"), "front.active_before", source)
    backend(field(front, "active_after", "front"), "front.active_after", target)
    retiring = obj(field(document, "retiring_slot", "root"), "retiring_slot")
    before = validate_snapshot(field(retiring, "before", "retiring_slot"), "retiring_slot.before")
    after = validate_snapshot(field(retiring, "after", "retiring_slot"), "retiring_slot.after")
    exact(before["active_generation"], source_generation, "retiring_slot.before.active_generation")
    exact(after["active_generation"], source_generation, "retiring_slot.after.active_generation")
    exact(before["accepting"], True, "retiring_slot.before.accepting")
    exact(before["retiring"], False, "retiring_slot.before.retiring")
    exact(before["front_active"], True, "retiring_slot.before.front_active")
    exact(after["accepting"], False, "retiring_slot.after.accepting")
    exact(after["retiring"], True, "retiring_slot.after.retiring")
    exact(after["front_active"], False, "retiring_slot.after.front_active")
    exact(before["inactive_connections"], 0, "retiring_slot.before.inactive_connections")
    exact(after["inactive_connections"], 0, "retiring_slot.after.inactive_connections")

    connections = obj(field(document, "connections", "root"), "connections")
    expected_connections = integer(
        field(connections, "expected_external", "connections"),
        "connections.expected_external",
        minimum=1,
    )
    exact(expected_connections, 1, "connections.expected_external")
    before_connections = integer(field(connections, "before", "connections"), "connections.before")
    after_connections = integer(field(connections, "after", "connections"), "connections.after")
    if before_connections < expected_connections or after_connections < expected_connections:
        fail("rollback did not preserve every expected externally held connection")

    metrics = obj(field(document, "metrics", "root"), "metrics")
    validate_service_metrics(field(metrics, "retiring_slot", "metrics"), "metrics.retiring_slot", SLOT_MEMORY_LIMIT)
    validate_service_metrics(field(metrics, "restored_slot", "metrics"), "metrics.restored_slot", SLOT_MEMORY_LIMIT)
    validate_service_metrics(field(metrics, "front", "metrics"), "metrics.front", FRONT_MEMORY_LIMIT)

    rollback = obj(field(document, "rollback", "root"), "rollback")
    exact(boolean(field(rollback, "performed", "rollback"), "rollback.performed"), True, "rollback.performed")
    exact(field(rollback, "from", "rollback"), source, "rollback.from")
    exact(field(rollback, "to", "rollback"), target, "rollback.to")
    exact(timestamp(field(rollback, "requested_at", "rollback"), "rollback.requested_at"), requested, "rollback.requested_at")
    exact(timestamp(field(rollback, "activated_at", "rollback"), "rollback.activated_at"), activated, "rollback.activated_at")
    retirement = obj(field(document, "retirement", "root"), "retirement")
    exact(field(retirement, "target", "retirement"), source, "retirement.target")
    exact(timestamp(field(retirement, "requested_at", "retirement"), "retirement.requested_at"), retired, "retirement.requested_at")
    exact(field(retirement, "state", "retirement"), "pending", "retirement.state")
    exact(
        boolean(field(retirement, "evidence_file_required", "retirement"), "retirement.evidence_file_required"),
        True,
        "retirement.evidence_file_required",
    )


def validate_front_migration_preparation(document: dict[str, Any]) -> None:
    exact(field(document, "evidence_type", "root"), "front-migration-preparation", "evidence_type")
    exact(field(document, "mode", "root"), "prepare", "mode")
    exact(boolean(field(document, "success", "root"), "success"), True, "success")
    validate_run(field(document, "run", "root"))
    release = validate_release(field(document, "release", "root"))
    bootstrap = validate_migration_bootstrap(field(document, "bootstrap", "root"))
    predecessor = validate_predecessor(field(document, "predecessor", "root"))
    if len({predecessor["sha256"], bootstrap["sha256"], release["sha256"]}) != 3:
        fail("predecessor, bootstrap worker, and control release must differ")
    routing = obj(field(document, "routing", "root"), "routing")
    for name in ("url_map", "legacy_backend", "front_backend"):
        text(field(routing, name, "routing"), f"routing.{name}")
    legacy_url = text(field(routing, "legacy_backend_url", "routing"), "routing.legacy_backend_url")
    front_url = text(field(routing, "front_backend_url", "routing"), "routing.front_backend_url")
    if legacy_url == front_url or not legacy_url.startswith("https://") or not front_url.startswith("https://"):
        fail("migration backend URLs must be distinct HTTPS resources")
    exact(field(routing, "current", "routing"), "legacy", "routing.current")
    legacy = obj(field(document, "legacy", "root"), "legacy")
    exact(field(legacy, "service", "legacy"), "subrouter.service", "legacy.service")
    text(field(legacy, "generation", "legacy"), "legacy.generation")
    exact(sha(field(legacy, "checksum", "legacy"), "legacy.checksum"), predecessor["sha256"], "legacy.checksum")
    exact(
        boolean(field(legacy, "accepting_new_public", "legacy"), "legacy.accepting_new_public"),
        True,
        "legacy.accepting_new_public",
    )
    front = obj(field(document, "front", "root"), "front")
    if field(front, "slot", "front") not in SLOTS:
        fail("front.slot must identify a private slot")
    text(field(front, "generation", "front"), "front.generation")
    exact(sha(field(front, "checksum", "front"), "front.checksum"), release["sha256"], "front.checksum")
    exact(
        sha(field(front, "control_checksum", "front"), "front.control_checksum"),
        release["sha256"],
        "front.control_checksum",
    )
    exact(
        sha(field(front, "worker_checksum", "front"), "front.worker_checksum"),
        bootstrap["sha256"],
        "front.worker_checksum",
    )
    exact(boolean(field(front, "ready", "front"), "front.ready"), True, "front.ready")
    timestamp(field(document, "evidence_emitted_at", "root"), "evidence_emitted_at")


def validate_migration_snapshot(value: Any, path: str, kind: str) -> dict[str, Any]:
    result = obj(value, path)
    exact(field(result, "kind", path), kind, f"{path}.kind")
    text(field(result, "generation", path), f"{path}.generation")
    integer(field(result, "public_connections", path), f"{path}.public_connections")
    integer(field(result, "generation_connections", path), f"{path}.generation_connections")
    exact(
        integer(field(result, "inactive_connections", path), f"{path}.inactive_connections"),
        0,
        f"{path}.inactive_connections",
    )
    return result


def validate_front_migration_transition(document: dict[str, Any], expected: str) -> None:
    exact(field(document, "evidence_type", "root"), expected, "evidence_type")
    mode = text(field(document, "mode", "root"), "mode")
    if expected == "front-migration-cutover":
        if mode not in {"rehearsal-cutover", "final-cutover"}:
            fail("front-migration-cutover mode must be rehearsal-cutover or final-cutover")
        source_kind, destination_kind = "legacy", "front"
        expected_prior = "front-migration-preparation" if mode == "rehearsal-cutover" else "front-migration-rollback"
    else:
        exact(mode, "rollback", "mode")
        source_kind, destination_kind = "front", "legacy"
        expected_prior = "front-migration-cutover"
    exact(boolean(field(document, "success", "root"), "success"), True, "success")
    exact(field(document, "prior_evidence_type", "root"), expected_prior, "prior_evidence_type")
    sha(field(document, "prior_evidence_sha256", "root"), "prior_evidence_sha256")
    sha(field(document, "preparation_evidence_sha256", "root"), "preparation_evidence_sha256")
    validate_run(field(document, "run", "root"))
    release = validate_release(field(document, "release", "root"))
    bootstrap = validate_migration_bootstrap(field(document, "bootstrap", "root"))
    predecessor = validate_predecessor(field(document, "predecessor", "root"))
    if len({predecessor["sha256"], bootstrap["sha256"], release["sha256"]}) != 3:
        fail("predecessor, bootstrap worker, and control release must differ")

    routing = obj(field(document, "routing", "root"), "routing")
    for name in ("url_map", "legacy_backend", "front_backend"):
        text(field(routing, name, "routing"), f"routing.{name}")
    legacy_url = text(field(routing, "legacy_backend_url", "routing"), "routing.legacy_backend_url")
    front_url = text(field(routing, "front_backend_url", "routing"), "routing.front_backend_url")
    exact(field(routing, "before", "routing"), source_kind, "routing.before")
    exact(field(routing, "after", "routing"), destination_kind, "routing.after")
    expected_source_url = legacy_url if source_kind == "legacy" else front_url
    expected_destination_url = front_url if destination_kind == "front" else legacy_url
    exact(field(routing, "source_backend_url", "routing"), expected_source_url, "routing.source_backend_url")
    exact(
        field(routing, "destination_backend_url", "routing"),
        expected_destination_url,
        "routing.destination_backend_url",
    )

    legacy = obj(field(document, "legacy", "root"), "legacy")
    exact(field(legacy, "service", "legacy"), "subrouter.service", "legacy.service")
    text(field(legacy, "generation", "legacy"), "legacy.generation")
    exact(sha(field(legacy, "checksum", "legacy"), "legacy.checksum"), predecessor["sha256"], "legacy.checksum")
    front = obj(field(document, "front", "root"), "front")
    if field(front, "slot", "front") not in SLOTS:
        fail("front.slot must identify a private slot")
    text(field(front, "generation", "front"), "front.generation")
    exact(sha(field(front, "checksum", "front"), "front.checksum"), release["sha256"], "front.checksum")
    exact(
        sha(field(front, "control_checksum", "front"), "front.control_checksum"),
        release["sha256"],
        "front.control_checksum",
    )
    exact(
        sha(field(front, "worker_checksum", "front"), "front.worker_checksum"),
        bootstrap["sha256"],
        "front.worker_checksum",
    )

    timestamps = obj(field(document, "timestamps", "root"), "timestamps")
    requested = timestamp(
        field(timestamps, "transition_requested_at", "timestamps"),
        "timestamps.transition_requested_at",
    )
    activated = timestamp(field(timestamps, "activated_at", "timestamps"), "timestamps.activated_at")
    emitted = timestamp(field(timestamps, "evidence_emitted_at", "timestamps"), "timestamps.evidence_emitted_at")
    if not requested <= activated <= emitted:
        fail("migration transition timestamps are out of order")
    if activated - requested >= dt.timedelta(seconds=30):
        fail("migration transition exceeded the 30-second phase boundary")

    proof = obj(field(document, "destination_proof", "root"), "destination_proof")
    sha(field(proof, "sha256", "destination_proof"), "destination_proof.sha256")
    challenge = text(field(proof, "challenge", "destination_proof"), "destination_proof.challenge")
    if not re.fullmatch(r"[0-9a-f]{32}", challenge):
        fail("destination_proof.challenge must be a 128-bit lowercase hex challenge")
    text(field(proof, "connection_id", "destination_proof"), "destination_proof.connection_id")
    session_id = text(field(proof, "session_id", "destination_proof"), "destination_proof.session_id")
    if re.fullmatch(r"[A-Za-z0-9._:-]{1,256}", session_id) is None:
        fail("destination_proof.session_id is invalid")
    exact(
        boolean(field(proof, "fresh_public_connection", "destination_proof"),
                "destination_proof.fresh_public_connection"),
        True,
        "destination_proof.fresh_public_connection",
    )
    exact(
        boolean(field(proof, "original_continuity_verified", "destination_proof"),
                "destination_proof.original_continuity_verified"),
        True,
        "destination_proof.original_continuity_verified",
    )
    exact(
        timestamp(field(proof, "observed_at", "destination_proof"), "destination_proof.observed_at"),
        activated,
        "destination_proof.observed_at",
    )
    proof_received = timestamp(
        field(proof, "received_at", "destination_proof"),
        "destination_proof.received_at",
    )
    if not activated <= proof_received <= emitted:
        fail("destination proof receipt timestamps are out of order")
    if proof_received - requested >= dt.timedelta(seconds=30):
        fail("destination proof was not received strictly before 30 seconds")

    source = obj(field(document, "source", "root"), "source")
    before = validate_migration_snapshot(field(source, "before", "source"), "source.before", source_kind)
    after = validate_migration_snapshot(field(source, "after", "source"), "source.after", source_kind)
    exact(before["generation"], after["generation"], "source.after.generation")
    exact(
        boolean(field(source, "accepting_new_public_before", "source"), "source.accepting_new_public_before"),
        True,
        "source.accepting_new_public_before",
    )
    exact(
        boolean(field(source, "accepting_new_public_after", "source"), "source.accepting_new_public_after"),
        False,
        "source.accepting_new_public_after",
    )
    continuity = obj(field(document, "continuity", "root"), "continuity")
    expected_connections = integer(
        field(continuity, "expected_external_connections", "continuity"),
        "continuity.expected_external_connections",
        minimum=1,
    )
    exact(expected_connections, 1 if mode == "rollback" else 2, "continuity.expected_external_connections")
    for path, snapshot in (("source.before", before), ("source.after", after)):
        if snapshot["public_connections"] < expected_connections or snapshot["generation_connections"] < expected_connections:
            fail(f"{path} does not preserve every external connection")
    exact(boolean(field(continuity, "preserved", "continuity"), "continuity.preserved"), True, "continuity.preserved")
    destination = obj(field(document, "destination", "root"), "destination")
    destination_before = validate_migration_snapshot(
        field(destination, "before", "destination"),
        "destination.before",
        destination_kind,
    )
    destination_after = validate_migration_snapshot(
        field(destination, "after", "destination"),
        "destination.after",
        destination_kind,
    )
    expected_destination_generation = front["generation"] if destination_kind == "front" else legacy["generation"]
    exact(destination_before["generation"], expected_destination_generation, "destination.before.generation")
    exact(destination_after["generation"], expected_destination_generation, "destination.after.generation")
    delta = integer(
        field(destination, "connection_count_delta", "destination"),
        "destination.connection_count_delta",
        minimum=1,
    )
    exact(
        delta,
        destination_after["generation_connections"] - destination_before["generation_connections"],
        "destination.connection_count_delta",
    )
    if destination_after["public_connections"] < destination_before["public_connections"] + 1:
        fail("destination public connection count did not increase for the golden proof")
    metrics = obj(field(document, "metrics", "root"), "metrics")
    exact(
        field(metrics, "source_service", "metrics"),
        "legacy" if source_kind == "legacy" else "slot",
        "metrics.source_service",
    )
    exact(
        field(metrics, "destination_service", "metrics"),
        "legacy" if destination_kind == "legacy" else "slot",
        "metrics.destination_service",
    )
    legacy_metrics = obj(field(metrics, "legacy", "metrics"), "metrics.legacy")
    validate_counter(field(legacy_metrics, "nrestarts", "metrics.legacy"), "metrics.legacy.nrestarts")
    validate_counter(field(legacy_metrics, "oom_kill", "metrics.legacy"), "metrics.legacy.oom_kill")
    legacy_peak = integer(
        field(legacy_metrics, "run_scoped_peak_rss_bytes", "metrics.legacy"),
        "metrics.legacy.run_scoped_peak_rss_bytes",
    )
    legacy_limit = integer(
        field(legacy_metrics, "rss_limit_bytes", "metrics.legacy"),
        "metrics.legacy.rss_limit_bytes",
        minimum=1,
    )
    exact(legacy_limit, SLOT_MEMORY_LIMIT, "metrics.legacy.rss_limit_bytes")
    if legacy_peak > legacy_limit:
        fail("metrics.legacy.run_scoped_peak_rss_bytes exceeds its limit")
    slot_metrics = obj(field(metrics, "slot", "metrics"), "metrics.slot")
    exact(field(slot_metrics, "id", "metrics.slot"), front["slot"], "metrics.slot.id")
    validate_service_metrics(slot_metrics, "metrics.slot", SLOT_MEMORY_LIMIT)
    validate_service_metrics(field(metrics, "front", "metrics"), "metrics.front", FRONT_MEMORY_LIMIT)
    rollback = obj(field(document, "rollback", "root"), "rollback")
    exact(
        boolean(field(rollback, "required", "rollback"), "rollback.required"),
        mode == "rehearsal-cutover",
        "rollback.required",
    )
    exact(
        boolean(field(rollback, "performed", "rollback"), "rollback.performed"),
        mode == "rollback",
        "rollback.performed",
    )


def validate_legacy_retirement(document: dict[str, Any]) -> None:
    exact(field(document, "evidence_type", "root"), "legacy-retirement", "evidence_type")
    exact(field(document, "mode", "root"), "final-cutover", "mode")
    exact(boolean(field(document, "success", "root"), "success"), True, "success")
    sha(field(document, "cutover_evidence_sha256", "root"), "cutover_evidence_sha256")
    sha(field(document, "preparation_evidence_sha256", "root"), "preparation_evidence_sha256")
    validate_run(field(document, "run", "root"))
    release = validate_release(field(document, "release", "root"))
    bootstrap = validate_migration_bootstrap(field(document, "bootstrap", "root"))
    predecessor = validate_predecessor(field(document, "predecessor", "root"))
    if len({predecessor["sha256"], bootstrap["sha256"], release["sha256"]}) != 3:
        fail("predecessor, bootstrap worker, and control release must differ")
    routing = obj(field(document, "routing", "root"), "routing")
    exact(field(routing, "active", "routing"), "front", "routing.active")
    exact(
        boolean(field(routing, "legacy_backend_retained", "routing"), "routing.legacy_backend_retained"),
        True,
        "routing.legacy_backend_retained",
    )
    exact(
        boolean(field(routing, "accepting_new_public", "routing"), "routing.accepting_new_public"),
        False,
        "routing.accepting_new_public",
    )
    legacy = obj(field(document, "legacy", "root"), "legacy")
    exact(field(legacy, "service", "legacy"), "subrouter.service", "legacy.service")
    text(field(legacy, "generation", "legacy"), "legacy.generation")
    exact(sha(field(legacy, "checksum", "legacy"), "legacy.checksum"), predecessor["sha256"], "legacy.checksum")
    connections = obj(field(document, "connections", "root"), "connections")
    before = obj(field(connections, "before", "connections"), "connections.before")
    after = obj(field(connections, "after", "connections"), "connections.after")
    for path, snapshot in (("connections.before", before), ("connections.after", after)):
        active = integer(field(snapshot, "active", path), f"{path}.active")
        inactive = integer(field(snapshot, "inactive", path), f"{path}.inactive")
        total = integer(field(snapshot, "total", path), f"{path}.total")
        exact(total, active + inactive, f"{path}.total")
    exact(after["total"], 0, "connections.after.total")

    retirement = obj(field(document, "retirement", "root"), "retirement")
    accepting_false = timestamp(
        field(retirement, "accepting_new_public_false_at", "retirement"),
        "retirement.accepting_new_public_false_at",
    )
    closed = timestamp(
        field(retirement, "last_connection_closed_at", "retirement"),
        "retirement.last_connection_closed_at",
    )
    stop_requested = timestamp(
        field(retirement, "stop_requested_at", "retirement"),
        "retirement.stop_requested_at",
    )
    absent = timestamp(field(retirement, "absent_at", "retirement"), "retirement.absent_at")
    if not accepting_false <= closed <= stop_requested <= absent:
        fail("legacy retirement timestamps are out of order")
    latency = integer(field(retirement, "absence_latency_ms", "retirement"), "retirement.absence_latency_ms")
    if latency >= 30_000:
        fail("legacy service was not absent strictly before 30 seconds")
    observed_latency = int((absent - closed).total_seconds() * 1000)
    if abs(observed_latency - latency) > 1_000:
        fail("legacy absence latency does not match its timestamps")
    for name in ("service_active_after", "control_socket_present_after", "enabled_after"):
        exact(boolean(field(retirement, name, "retirement"), f"retirement.{name}"), False, f"retirement.{name}")
    exact(field(retirement, "service_result", "retirement"), "success", "retirement.service_result")
    metrics = obj(field(document, "metrics", "root"), "metrics")
    validate_counter(field(metrics, "nrestarts", "metrics"), "metrics.nrestarts")
    validate_counter(field(metrics, "oom_kill", "metrics"), "metrics.oom_kill")
    peak = integer(
        field(metrics, "run_scoped_peak_rss_bytes", "metrics"),
        "metrics.run_scoped_peak_rss_bytes",
    )
    limit = integer(field(metrics, "rss_limit_bytes", "metrics"), "metrics.rss_limit_bytes", minimum=1)
    exact(limit, SLOT_MEMORY_LIMIT, "metrics.rss_limit_bytes")
    if peak > limit:
        fail("legacy retirement run-scoped RSS exceeds its limit")
    emitted = timestamp(field(document, "evidence_emitted_at", "root"), "evidence_emitted_at")
    if emitted < absent:
        fail("legacy retirement evidence was emitted before absence")


def validate_deployment_preflight(document: dict[str, Any]) -> None:
    exact(field(document, "evidence_type", "root"), "deployment-preflight", "evidence_type")
    mode = text(field(document, "mode", "root"), "mode")
    if mode not in {"slot", "migrate-front"}:
        fail("deployment preflight mode must be slot or migrate-front")
    exact(boolean(field(document, "success", "root"), "success"), True, "success")
    exact(
        boolean(field(document, "mutation_performed", "root"), "mutation_performed"),
        False,
        "mutation_performed",
    )
    exact(
        boolean(field(document, "local_golden_required", "root"), "local_golden_required"),
        True,
        "local_golden_required",
    )
    validate_run(field(document, "run", "root"))
    release = validate_release(field(document, "release", "root"))
    public = obj(field(document, "public", "root"), "public")
    exact(boolean(field(public, "health", "public"), "public.health"), True, "public.health")
    exact(boolean(field(public, "ready", "public"), "public.ready"), True, "public.ready")
    routing = obj(field(document, "routing", "root"), "routing")
    text(field(routing, "url_map", "routing"), "routing.url_map")
    legacy_refs = integer(field(routing, "legacy_backend_references", "routing"), "routing.legacy_backend_references")
    front_refs = integer(field(routing, "front_backend_references", "routing"), "routing.front_backend_references")
    topology = obj(field(document, "topology", "root"), "topology")
    exact(
        boolean(field(topology, "candidate_differs_from_active", "topology"),
                "topology.candidate_differs_from_active"),
        True,
        "topology.candidate_differs_from_active",
    )
    if mode == "migrate-front":
        exact(legacy_refs, 1, "routing.legacy_backend_references")
        exact(front_refs, 0, "routing.front_backend_references")
        exact(field(topology, "kind", "topology"), "legacy", "topology.kind")
        exact(field(topology, "routing_current", "topology"), "legacy", "topology.routing_current")
        exact(field(topology, "front_state", "topology"), "absent", "topology.front_state")
        legacy = obj(field(topology, "legacy", "topology"), "topology.legacy")
        exact(boolean(field(legacy, "service_active", "topology.legacy"), "topology.legacy.service_active"), True,
              "topology.legacy.service_active")
        text(field(legacy, "generation", "topology.legacy"), "topology.legacy.generation")
        active_sha = sha(field(legacy, "checksum", "topology.legacy"), "topology.legacy.checksum")
        if active_sha == release["sha256"]:
            fail("migration candidate is already the active legacy worker")
        integer(field(legacy, "active_connections", "topology.legacy"), "topology.legacy.active_connections")
        exact(integer(field(legacy, "inactive_connections", "topology.legacy"),
                      "topology.legacy.inactive_connections"), 0, "topology.legacy.inactive_connections")
    else:
        exact(legacy_refs, 0, "routing.legacy_backend_references")
        exact(front_refs, 1, "routing.front_backend_references")
        exact(field(topology, "kind", "topology"), "front-slots", "topology.kind")
        exact(field(topology, "routing_current", "topology"), "front", "topology.routing_current")
        front = obj(field(topology, "front", "topology"), "topology.front")
        exact(boolean(field(front, "service_active", "topology.front"), "topology.front.service_active"), True,
              "topology.front.service_active")
        if field(front, "active_slot", "topology.front") not in SLOTS:
            fail("topology.front.active_slot is invalid")
        sha(field(front, "checksum", "topology.front"), "topology.front.checksum")
        exact(integer(field(front, "memory_max_bytes", "topology.front"), "topology.front.memory_max_bytes"),
              FRONT_MEMORY_LIMIT, "topology.front.memory_max_bytes")
        slot = obj(field(topology, "slot", "topology"), "topology.slot")
        exact(boolean(field(slot, "service_active", "topology.slot"), "topology.slot.service_active"), True,
              "topology.slot.service_active")
        text(field(slot, "generation", "topology.slot"), "topology.slot.generation")
        worker_sha = sha(field(slot, "worker_checksum", "topology.slot"), "topology.slot.worker_checksum")
        if worker_sha == release["sha256"]:
            fail("slot candidate is already active")
        sha(field(slot, "control_checksum", "topology.slot"), "topology.slot.control_checksum")
        exact(integer(field(slot, "inactive_connections", "topology.slot"),
                      "topology.slot.inactive_connections"), 0, "topology.slot.inactive_connections")
        exact(integer(field(slot, "memory_max_bytes", "topology.slot"), "topology.slot.memory_max_bytes"),
              SLOT_MEMORY_LIMIT, "topology.slot.memory_max_bytes")
    timestamp(field(document, "evidence_emitted_at", "root"), "evidence_emitted_at")


def validate_staging_normalization(document: dict[str, Any]) -> None:
    exact(field(document, "evidence_type", "root"), "staging-predecessor-normalization", "evidence_type")
    exact(field(document, "mode", "root"), "staging-only", "mode")
    exact(boolean(field(document, "success", "root"), "success"), True, "success")
    performed = boolean(field(document, "normalization_performed", "root"), "normalization_performed")
    result = text(field(document, "normalization_result", "root"), "normalization_result")
    run = validate_run(field(document, "run", "root"))
    exact(run["instance"], "subrouter-staging", "run.instance")
    predecessor = validate_predecessor(field(document, "predecessor", "root"))
    checksums = obj(field(document, "checksums", "root"), "checksums")
    before_sha = sha(field(checksums, "before", "checksums"), "checksums.before")
    after_sha = sha(field(checksums, "after", "checksums"), "checksums.after")
    exact(after_sha, predecessor["sha256"], "checksums.after")
    generations = obj(field(document, "generations", "root"), "generations")
    before_generation = text(field(generations, "before", "generations"), "generations.before")
    after_generation = text(field(generations, "after", "generations"), "generations.after")
    connections = obj(field(document, "connections", "root"), "connections")
    exact(integer(field(connections, "inactive_after", "connections"),
                  "connections.inactive_after"), 0, "connections.inactive_after")
    public = obj(field(document, "public", "root"), "public")
    exact(boolean(field(public, "health", "public"), "public.health"), True, "public.health")
    exact(boolean(field(public, "ready", "public"), "public.ready"), True, "public.ready")
    timestamps = obj(field(document, "timestamps", "root"), "timestamps")
    if performed:
        exact(result, "replaced-worker", "normalization_result")
        if before_sha == after_sha:
            fail("performed staging normalization must replace different worker bytes")
        if before_generation == after_generation:
            fail("performed staging normalization must activate a new generation")
        integer(field(connections, "old_generation_before", "connections"), "connections.old_generation_before")
        exact(integer(field(connections, "old_generation_after", "connections"),
                      "connections.old_generation_after"), 0, "connections.old_generation_after")
        requested = timestamp(field(timestamps, "upgrade_requested_at", "timestamps"), "timestamps.upgrade_requested_at")
        activated = timestamp(field(timestamps, "activated_at", "timestamps"), "timestamps.activated_at")
        drained = timestamp(field(timestamps, "drained_at", "timestamps"), "timestamps.drained_at")
        emitted = timestamp(field(timestamps, "evidence_emitted_at", "timestamps"), "timestamps.evidence_emitted_at")
        if not requested <= activated <= drained <= emitted:
            fail("staging normalization timestamps are out of order")
    else:
        exact(result, "already-normalized", "normalization_result")
        exact(before_sha, after_sha, "checksums.before")
        exact(before_generation, after_generation, "generations.before")
        integer(field(connections, "active_generation_before", "connections"),
                "connections.active_generation_before")
        integer(field(connections, "active_generation_after", "connections"),
                "connections.active_generation_after")
        verified = timestamp(field(timestamps, "verified_at", "timestamps"), "timestamps.verified_at")
        emitted = timestamp(field(timestamps, "evidence_emitted_at", "timestamps"), "timestamps.evidence_emitted_at")
        if verified > emitted:
            fail("already-normalized verification timestamp follows evidence emission")
    metrics = obj(field(document, "metrics", "root"), "metrics")
    validate_counter(field(metrics, "nrestarts", "metrics"), "metrics.nrestarts")
    validate_counter(field(metrics, "oom_kill", "metrics"), "metrics.oom_kill")


def validate_vm_provision(document: dict[str, Any]) -> None:
    exact(field(document, "evidence_type", "root"), "vm-provision", "evidence_type")
    exact(field(document, "mode", "root"), "fresh-front-slots", "mode")
    exact(boolean(field(document, "success", "root"), "success"), True, "success")
    mutation_performed = boolean(field(document, "mutation_performed", "root"), "mutation_performed")
    validate_run(field(document, "run", "root"))
    release = validate_release(field(document, "release", "root"))

    startup_metadata = obj(field(document, "startup_metadata", "root"), "startup_metadata")
    exact(field(startup_metadata, "schema", "startup_metadata"),
          "subrouter.gcp.vm-release-metadata/v1", "startup_metadata.schema")
    sha(field(startup_metadata, "sha256", "startup_metadata"), "startup_metadata.sha256")
    sha(field(startup_metadata, "verification_evidence_sha256", "startup_metadata"),
        "startup_metadata.verification_evidence_sha256")

    binary_asset = f"subrouter_{release['tag'][1:]}_linux_amd64"
    expected_assets = {
        "SHA256SUMS", "SOURCE_PROVENANCE.json", "install.sh",
        "deployment-contract.py", "install-front-slots.sh", binary_asset,
    }
    artifacts = obj(field(document, "artifacts", "root"), "artifacts")
    exact(set(artifacts), expected_assets, "artifacts keys")
    for name, digest in artifacts.items():
        sha(digest, f"artifacts.{name}")
    exact(artifacts[binary_asset], release["sha256"], f"artifacts.{binary_asset}")

    instance, instance_created_at = validate_instance_identity(
        field(document, "instance", "root"),
        "instance",
    )
    created = instance["created"]
    exact(created, mutation_performed, "instance.created")

    topology = obj(field(document, "topology", "root"), "topology")
    exact(field(topology, "kind", "topology"), "front-slots", "topology.kind")
    state = text(field(topology, "state", "topology"), "topology.state")
    if state not in {"prepared", "active"}:
        fail("topology.state must be prepared or active")
    exact(field(topology, "release_tag", "topology"), release["tag"], "topology.release_tag")
    exact(field(topology, "initial_slot", "topology"), "slot-a", "topology.initial_slot")
    authenticated = boolean(field(topology, "authenticated", "topology"), "topology.authenticated")

    legacy = obj(field(topology, "legacy", "topology"), "topology.legacy")
    for name in ("service_active", "service_enabled", "socket_active", "socket_enabled"):
        exact(boolean(field(legacy, name, "topology.legacy"), f"topology.legacy.{name}"),
              False, f"topology.legacy.{name}")
    slot = obj(field(topology, "slot", "topology"), "topology.slot")
    exact(field(slot, "id", "topology.slot"), "slot-a", "topology.slot.id")
    slot_active = boolean(field(slot, "service_active", "topology.slot"), "topology.slot.service_active")
    slot_enabled = boolean(field(slot, "service_enabled", "topology.slot"), "topology.slot.service_enabled")
    exact(sha(field(slot, "worker_checksum", "topology.slot"), "topology.slot.worker_checksum"),
          release["sha256"], "topology.slot.worker_checksum")
    exact(integer(field(slot, "memory_max_bytes", "topology.slot"), "topology.slot.memory_max_bytes"),
          SLOT_MEMORY_LIMIT, "topology.slot.memory_max_bytes")
    front = obj(field(topology, "front", "topology"), "topology.front")
    front_active = boolean(field(front, "service_active", "topology.front"), "topology.front.service_active")
    front_enabled = boolean(field(front, "service_enabled", "topology.front"), "topology.front.service_enabled")
    exact(sha(field(front, "binary_checksum", "topology.front"), "topology.front.binary_checksum"),
          release["sha256"], "topology.front.binary_checksum")
    exact(integer(field(front, "memory_max_bytes", "topology.front"), "topology.front.memory_max_bytes"),
          FRONT_MEMORY_LIMIT, "topology.front.memory_max_bytes")
    control = obj(field(topology, "control", "topology"), "topology.control")
    exact(sha(field(control, "binary_checksum", "topology.control"), "topology.control.binary_checksum"),
          release["sha256"], "topology.control.binary_checksum")
    retained = obj(field(topology, "retained_release", "topology"), "topology.retained_release")
    exact(sha(field(retained, "binary_checksum", "topology.retained_release"),
              "topology.retained_release.binary_checksum"), release["sha256"],
          "topology.retained_release.binary_checksum")
    if state == "prepared":
        exact(authenticated, False, "topology.authenticated")
        for value, path in (
            (slot_active, "topology.slot.service_active"),
            (slot_enabled, "topology.slot.service_enabled"),
            (front_active, "topology.front.service_active"),
            (front_enabled, "topology.front.service_enabled"),
        ):
            exact(value, False, path)
    else:
        exact(authenticated, True, "topology.authenticated")
        for value, path in (
            (slot_active, "topology.slot.service_active"),
            (slot_enabled, "topology.slot.service_enabled"),
            (front_active, "topology.front.service_active"),
            (front_enabled, "topology.front.service_enabled"),
        ):
            exact(value, True, path)
    emitted_at = timestamp(field(document, "evidence_emitted_at", "root"), "evidence_emitted_at")
    if instance_created_at > emitted_at:
        fail("instance.creation_timestamp follows evidence_emitted_at")
    if created and emitted_at - instance_created_at >= FRESH_VM_MAX_AGE:
        fail("newly created VM evidence must be emitted less than two hours after instance creation")


def validate_fresh_vm_acceptance(document: dict[str, Any]) -> None:
    exact(field(document, "evidence_type", "root"), "fresh-vm-acceptance", "evidence_type")
    exact(field(document, "mode", "root"), "post-publish", "mode")
    exact(boolean(field(document, "success", "root"), "success"), True, "success")
    run = validate_run(field(document, "run", "root"))
    release = validate_release(field(document, "release", "root"))

    bootstrap = obj(field(document, "bootstrap_evidence", "root"), "bootstrap_evidence")
    sha(field(bootstrap, "sha256", "bootstrap_evidence"), "bootstrap_evidence.sha256")
    exact(field(bootstrap, "evidence_type", "bootstrap_evidence"),
          "vm-provision", "bootstrap_evidence.evidence_type")
    exact(field(bootstrap, "topology_state", "bootstrap_evidence"),
          "prepared", "bootstrap_evidence.topology_state")
    bootstrap_emitted_at = timestamp(
        field(bootstrap, "evidence_emitted_at", "bootstrap_evidence"),
        "bootstrap_evidence.evidence_emitted_at",
    )

    instance, instance_created_at = validate_instance_identity(
        field(document, "instance", "root"),
        "instance",
        expected_created=True,
    )
    text(field(instance, "bootstrap_run_id", "instance"), "instance.bootstrap_run_id")

    public = obj(field(document, "public", "root"), "public")
    base_url = text(field(public, "base_url", "public"), "public.base_url")
    if not re.fullmatch(r"https://[^/?#]+/?", base_url):
        fail("public.base_url must be an HTTPS origin")
    exact(boolean(field(public, "health", "public"), "public.health"), True, "public.health")
    exact(boolean(field(public, "ready", "public"), "public.ready"), True, "public.ready")

    topology = obj(field(document, "topology", "root"), "topology")
    exact(field(topology, "state", "topology"), "active", "topology.state")
    exact(boolean(field(topology, "authenticated", "topology"), "topology.authenticated"),
          True, "topology.authenticated")
    synthetic_provision = {
        "evidence_type": "vm-provision",
        "mode": "fresh-front-slots",
        "success": True,
        "mutation_performed": True,
        "run": run,
        "release": release,
        "startup_metadata": field(document, "startup_metadata", "root"),
        "artifacts": field(document, "artifacts", "root"),
        "instance": {
            "created": True,
            "id": instance["id"],
            "creation_timestamp": instance["creation_timestamp"],
        },
        "topology": topology,
        "evidence_emitted_at": field(document, "evidence_emitted_at", "root"),
    }
    validate_vm_provision(synthetic_provision)
    emitted_at = timestamp(field(document, "evidence_emitted_at", "root"), "evidence_emitted_at")
    if not instance_created_at <= bootstrap_emitted_at <= emitted_at:
        fail("fresh VM identity and evidence timestamps are out of order")
    if emitted_at - instance_created_at >= FRESH_VM_MAX_AGE:
        fail("fresh VM acceptance must be emitted less than two hours after instance creation")


def reject_secret_shaped_data(value: Any, path: str = "root") -> None:
    if isinstance(value, dict):
        for key, child in value.items():
            lowered = str(key).lower()
            if any(fragment in lowered for fragment in ("secret", "token", "password", "authorization", "credential")):
                fail(f"{path}.{key} is forbidden in deployment evidence")
            reject_secret_shaped_data(child, f"{path}.{key}")
    elif isinstance(value, list):
        for index, child in enumerate(value):
            reject_secret_shaped_data(child, f"{path}[{index}]")
    elif isinstance(value, str) and (value.startswith("srt_") or value.startswith("Bearer ")):
        fail(f"{path} contains secret-shaped data")


def validate(document: dict[str, Any], expected: str) -> None:
    exact(field(document, "schema", "root"), SCHEMA, "schema")
    reject_secret_shaped_data(document)
    if expected == "slot-activation":
        validate_activation(document, expected)
    elif expected == "slot-rollback":
        validate_slot_rollback(document)
    elif expected == "slot-retirement":
        validate_slot_retirement(document)
    elif expected == "front-migration-preparation":
        validate_front_migration_preparation(document)
    elif expected in {"front-migration-cutover", "front-migration-rollback"}:
        validate_front_migration_transition(document, expected)
    elif expected == "legacy-retirement":
        validate_legacy_retirement(document)
    elif expected == "deployment-preflight":
        validate_deployment_preflight(document)
    elif expected == "staging-predecessor-normalization":
        validate_staging_normalization(document)
    elif expected == "vm-provision":
        validate_vm_provision(document)
    elif expected == "fresh-vm-acceptance":
        validate_fresh_vm_acceptance(document)
    else:
        fail(f"validation for {expected} is not implemented")


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--expect", required=True, choices=sorted(EXPECTATIONS))
    parser.add_argument("evidence", type=Path)
    args = parser.parse_args()
    try:
        size = args.evidence.stat().st_size
        if size <= 0 or size > MAX_EVIDENCE_BYTES:
            fail(f"evidence size must be 1..{MAX_EVIDENCE_BYTES} bytes")
        raw = args.evidence.read_text(encoding="utf-8")
        parsed = json.loads(raw)
        document = obj(parsed, "root")
        validate(document, args.expect)
    except (OSError, UnicodeError, json.JSONDecodeError, EvidenceError) as error:
        print(f"invalid deployment evidence: {error}", file=sys.stderr)
        return 1
    print(json.dumps({"valid": True, "expect": args.expect, "evidence": str(args.evidence)}, separators=(",", ":")))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
