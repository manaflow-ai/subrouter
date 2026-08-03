#!/usr/bin/env python3
"""Fail-closed semantic validation for GCP deployment evidence.

Usage:
  validate-deploy-evidence.py --expect slot-activation evidence.json
  validate-deploy-evidence.py --expect slot-rollback evidence.json
  validate-deploy-evidence.py --expect slot-retirement evidence.json
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
SLOTS = {"slot-a", "slot-b"}
EXPECTATIONS = {
    "slot-activation",
    "slot-rollback",
    "slot-retirement",
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
    activated = timestamp(field(timestamps, "activated_at", "timestamps"), "timestamps.activated_at")
    emitted = timestamp(field(timestamps, "evidence_emitted_at", "timestamps"), "timestamps.evidence_emitted_at")
    if not requested <= activated <= emitted:
        fail("activation timestamps are out of order")
    if activated - requested > dt.timedelta(seconds=30):
        fail("activation exceeded the 30-second phase boundary")

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
    pinned = integer(
        field(continuity, "pinned_original_connections_at_switch", "continuity"),
        "continuity.pinned_original_connections_at_switch",
    )
    if pinned < clients:
        fail("not every configured original client was pinned at the switch")
    exact(
        boolean(field(continuity, "all_original_clients_pinned", "continuity"), "continuity.all_original_clients_pinned"),
        True,
        "continuity.all_original_clients_pinned",
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
    if latency > 30_000:
        fail("retired slot was not absent within 30 seconds")
    observed_latency = int((absent - closed).total_seconds() * 1000)
    if abs(observed_latency - latency) > 1_000:
        fail("retirement.absence_latency_ms does not match its timestamps")
    for name in ("service_active_after", "control_socket_present_after", "enabled_after"):
        exact(boolean(field(retirement, name, "retirement"), f"retirement.{name}"), False, f"retirement.{name}")
    exact(field(retirement, "service_result", "retirement"), "success", "retirement.service_result")
    metrics = obj(field(document, "metrics", "root"), "metrics")
    old_metrics = obj(field(metrics, "old_slot", "metrics"), "metrics.old_slot")
    validate_counter(field(old_metrics, "nrestarts", "metrics.old_slot"), "metrics.old_slot.nrestarts")
    validate_counter(field(old_metrics, "oom_kill", "metrics.old_slot"), "metrics.old_slot.oom_kill")
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
    if activated - requested > dt.timedelta(seconds=30):
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
    before_connections = integer(field(connections, "before", "connections"), "connections.before")
    after_connections = integer(field(connections, "after", "connections"), "connections.after")
    if before_connections < expected_connections or after_connections < expected_connections:
        fail("rollback did not preserve every expected externally held connection")

    metrics = obj(field(document, "metrics", "root"), "metrics")
    for name in ("retiring_slot", "restored_slot", "front"):
        service = obj(field(metrics, name, "metrics"), f"metrics.{name}")
        validate_counter(field(service, "nrestarts", f"metrics.{name}"), f"metrics.{name}.nrestarts")
        validate_counter(field(service, "oom_kill", f"metrics.{name}"), f"metrics.{name}.oom_kill")

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
