#!/usr/bin/env python3
"""Validate deployment inputs and transitions for the GCP shell entrypoints."""

from __future__ import annotations

import argparse
from datetime import datetime, timedelta, timezone
import json
import os
from pathlib import Path
import re
import socket
import stat
import sys
from typing import Any, NoReturn
from urllib.parse import urlsplit


MAX_PRIVATE_FILE_BYTES = 131_072
MAX_TEXT_FILE_BYTES = 8 * 1024 * 1024
UINT64_MAX = 2**64 - 1
TOKEN_KEYS = (
    "SUBROUTER_ADMIN_TOKEN",
    "SUBROUTER_ACCOUNT_IMPORT_TOKEN",
)


class ContractError(Exception):
    """A deployment input does not satisfy its contract."""


def fail(message: str) -> NoReturn:
    raise ContractError(message)


def reject_duplicate_keys(pairs: list[tuple[str, Any]]) -> dict[str, Any]:
    result: dict[str, Any] = {}
    for key, value in pairs:
        if key in result:
            fail(f"JSON object contains duplicate key: {key}")
        result[key] = value
    return result


def parse_json(raw: str, label: str) -> dict[str, Any]:
    try:
        document = json.loads(raw, object_pairs_hook=reject_duplicate_keys)
    except (json.JSONDecodeError, UnicodeError) as error:
        fail(f"{label} is not valid JSON: {error}")
    if not isinstance(document, dict):
        fail(f"{label} must contain a JSON object")
    return document


def read_text(path: Path, label: str) -> str:
    try:
        metadata = path.stat()
        if not stat.S_ISREG(metadata.st_mode):
            fail(f"{label} must be a regular file")
        if metadata.st_size > MAX_TEXT_FILE_BYTES:
            fail(f"{label} exceeds the {MAX_TEXT_FILE_BYTES}-byte limit")
        return path.read_text(encoding="utf-8")
    except (OSError, UnicodeError) as error:
        fail(f"could not read {label} {path}: {error}")


def load_json(path: Path, label: str) -> dict[str, Any]:
    return parse_json(read_text(path, label), label)


def parse_timestamp(value: Any, label: str) -> datetime:
    if not isinstance(value, str) or not value:
        fail(f"{label} must be a timestamp string")
    try:
        parsed = datetime.fromisoformat(value.replace("Z", "+00:00"))
    except ValueError as error:
        fail(f"{label} is invalid: {error}")
    if parsed.tzinfo is None:
        fail(f"{label} must include a timezone")
    return parsed


def canonical_timestamp(value: Any, label: str) -> str:
    return (
        parse_timestamp(value, label)
        .astimezone(timezone.utc)
        .isoformat(timespec="milliseconds")
        .replace("+00:00", "Z")
    )


def canonical_origin(value: Any, label: str) -> str:
    if not isinstance(value, str) or not value.strip():
        fail(f"{label} must be a non-empty HTTPS origin")
    parsed = urlsplit(value.strip())
    if parsed.scheme.lower() != "https" or not parsed.netloc:
        fail(f"{label} must be an HTTPS origin")
    if parsed.username is not None or parsed.password is not None:
        fail(f"{label} must not contain user information")
    if parsed.path not in ("", "/") or parsed.query or parsed.fragment:
        fail(f"{label} must not contain a path, query, or fragment")
    try:
        host = parsed.hostname
        port = parsed.port
    except ValueError as error:
        fail(f"{label} has an invalid host or port: {error}")
    if host is None:
        fail(f"{label} must contain a hostname")
    normalized_host = host.lower()
    if ":" in normalized_host:
        normalized_host = f"[{normalized_host}]"
    port_suffix = "" if port in (None, 443) else f":{port}"
    return f"https://{normalized_host}{port_suffix}"


def command_validate_target(args: argparse.Namespace) -> None:
    config = load_json(args.cloud_config, "cloud config")
    hosted_url = canonical_origin(config.get("hostedUrl"), "cloud config hostedUrl")
    public_origin = canonical_origin(args.public_url, "SUBROUTER_PUBLIC_BASE_URL")
    expected_by_instance = {
        "subrouter-staging": "https://staging.sr.cmux.com",
        "subrouter-team": "https://sr.cmux.com",
    }
    expected = expected_by_instance.get(args.instance)
    if expected is None:
        fail(f"unsupported golden continuity instance: {args.instance}")
    if public_origin != expected:
        fail(f"{args.instance} must use SUBROUTER_PUBLIC_BASE_URL={expected}")
    if hosted_url != public_origin:
        fail(
            f"cloud config hostedUrl ({hosted_url}) does not match "
            f"SUBROUTER_PUBLIC_BASE_URL ({public_origin})"
        )
    print(public_origin)


def command_manifest_sha(args: argparse.Namespace) -> None:
    lines = read_text(args.manifest, "SHA256SUMS").splitlines()
    matches: list[str] = []
    for line in lines:
        fields = line.split()
        if len(fields) == 2 and fields[1].lstrip("*") == args.asset:
            matches.append(fields[0].lower())
    if len(matches) != 1 or re.fullmatch(r"[0-9a-f]{64}", matches[0]) is None:
        fail(f"SHA256SUMS must contain exactly one valid entry for {args.asset}")
    print(matches[0])


def command_gce_instance_identity(args: argparse.Namespace) -> None:
    document = load_json(args.document, "GCE instance response")
    if document.get("name") != args.expected_name:
        fail("GCE instance response name does not match the deployment target")
    returned_zone = document.get("zone")
    if not isinstance(returned_zone, str) or returned_zone.rsplit("/", 1)[-1] != args.expected_zone:
        fail("GCE instance response zone does not match the deployment target")
    raw_id = document.get("id")
    if isinstance(raw_id, bool) or not isinstance(raw_id, (str, int)):
        fail("GCE instance ID is missing or invalid")
    instance_id = str(raw_id)
    if re.fullmatch(r"[1-9][0-9]{0,19}", instance_id) is None or int(instance_id) > UINT64_MAX:
        fail("GCE instance ID is missing or invalid")
    created = canonical_timestamp(document.get("creationTimestamp"), "GCE instance creationTimestamp")
    print(
        json.dumps(
            {"creation_timestamp": created, "id": instance_id},
            separators=(",", ":"),
            sort_keys=True,
        )
    )


def command_validate_private_file(args: argparse.Namespace) -> None:
    flags = os.O_RDONLY
    if hasattr(os, "O_NOFOLLOW"):
        flags |= os.O_NOFOLLOW
    try:
        descriptor = os.open(args.path, flags)
    except OSError as error:
        fail(f"verification evidence must be a readable regular non-symlink file: {error}")
    try:
        metadata = os.fstat(descriptor)
        mode = stat.S_IMODE(metadata.st_mode)
        if not stat.S_ISREG(metadata.st_mode):
            fail("verification evidence must be a regular file")
        if mode & 0o077:
            fail(f"verification evidence must not be group/world accessible: {mode:o}")
        if metadata.st_size <= 0 or metadata.st_size > MAX_PRIVATE_FILE_BYTES:
            fail("verification evidence size is invalid")
        if not os.read(descriptor, 1):
            fail("verification evidence size is invalid")
    finally:
        os.close(descriptor)


def command_validate_instance_binding(args: argparse.Namespace) -> None:
    bootstrap = load_json(args.bootstrap, "fresh VM bootstrap evidence")
    live = parse_json(args.live_identity, "live GCE instance identity")
    instance = bootstrap.get("instance")
    if not isinstance(instance, dict):
        fail("fresh VM bootstrap instance is missing or invalid")
    bootstrap_id = instance.get("id")
    live_id = live.get("id")
    if not isinstance(bootstrap_id, str) or not isinstance(live_id, str) or bootstrap_id != live_id:
        fail("fresh VM bootstrap GCE instance ID does not match the live target")
    bootstrap_created = parse_timestamp(
        instance.get("creation_timestamp"), "fresh VM bootstrap creation timestamp"
    )
    live_created = parse_timestamp(
        live.get("creation_timestamp"), "live GCE instance creation timestamp"
    )
    if bootstrap_created != live_created:
        fail("fresh VM bootstrap creation timestamp does not match the live target")
    now = parse_timestamp(args.now, "current time") if args.now is not None else datetime.now(timezone.utc)
    age = now - live_created
    if age < -timedelta(minutes=5):
        fail("live GCE instance creation timestamp is too far in the future")
    if age >= timedelta(hours=2):
        fail("live GCE instance is too old for fresh VM acceptance")


def command_validate_legacy_supervisor_status(args: argparse.Namespace) -> None:
    document = load_json(args.path, "legacy supervisor status")
    has_accepting = "accepting" in document
    has_retiring = "retiring" in document
    if has_accepting != has_retiring:
        fail("legacy supervisor lifecycle fields must both be present or both be absent")
    if has_accepting:
        if document["accepting"] is not True:
            fail("legacy supervisor accepting must be true")
        if document["retiring"] is not False:
            fail("legacy supervisor retiring must be false")
    active = document.get("active")
    if not isinstance(active, dict):
        fail("legacy supervisor active generation is missing")
    active_id = active.get("id")
    if not isinstance(active_id, str) or not active_id:
        fail("legacy supervisor active generation ID is invalid")
    backends = document.get("backends")
    if not isinstance(backends, list) or not backends:
        fail("legacy supervisor backends are missing")
    active_matches = 0
    for backend in backends:
        if not isinstance(backend, dict):
            fail("legacy supervisor backend is invalid")
        connections = backend.get("connections")
        if isinstance(connections, bool) or not isinstance(connections, int) or connections < 0:
            fail("legacy supervisor backend connections are invalid")
        if backend.get("id") == active_id:
            active_matches += 1
            if backend.get("active") is not True:
                fail("legacy supervisor active backend is not marked active")
    if active_matches != 1:
        fail("legacy supervisor active generation must match exactly one backend")


def command_probe_slot_endpoint(args: argparse.Namespace) -> None:
    if args.port < 1 or args.port > 65535:
        fail("slot endpoint port is invalid")
    if not args.path.startswith("/") or "\r" in args.path or "\n" in args.path:
        fail("slot endpoint path is invalid")
    try:
        request = (
            f"PROXY TCP4 127.0.0.1 127.0.0.1 12345 {args.port}\r\n"
            f"GET {args.path} HTTP/1.1\r\nHost: localhost\r\nConnection: close\r\n\r\n"
        ).encode("ascii")
    except UnicodeEncodeError:
        fail("slot endpoint path must contain only ASCII characters")
    try:
        with socket.create_connection(("127.0.0.1", args.port), timeout=2) as connection:
            connection.sendall(request)
            response = connection.recv(4096)
    except OSError as error:
        fail(f"slot endpoint probe failed: {error}")
    if not response.startswith(b"HTTP/1.1 200"):
        fail("slot endpoint did not return HTTP/1.1 200")


def command_validate_auth_defaults(args: argparse.Namespace) -> None:
    values: dict[str, str] = {}
    pattern = re.compile(r"(SUBROUTER_ADMIN_TOKEN|SUBROUTER_ACCOUNT_IMPORT_TOKEN)=(.*)")
    for line in read_text(args.path, "authenticated defaults").splitlines():
        match = pattern.fullmatch(line)
        if match is None:
            continue
        key = match.group(1)
        if key in values:
            fail(f"{key} occurs more than once in authenticated defaults")
        value = match.group(2).strip()
        if len(value) >= 2 and value[0] == value[-1] == '"':
            value = value[1:-1]
        values[key] = value
    for key in TOKEN_KEYS:
        if not values.get(key):
            fail(f"{key} is missing from authenticated defaults")
    if values[TOKEN_KEYS[0]] == values[TOKEN_KEYS[1]]:
        fail("admin and account-import tokens must be distinct")


def url_map_counts(path: Path, first: str, second: str) -> tuple[int, int]:
    if not first or not second or first == second:
        fail("URL-map reference markers must be non-empty and distinct")
    body = read_text(path, "URL map")
    return body.count(first), body.count(second)


def command_classify_url_map(args: argparse.Namespace) -> None:
    legacy_count, front_count = url_map_counts(args.path, args.legacy_url, args.front_url)
    if (legacy_count, front_count) == (1, 0):
        print("legacy")
    elif (legacy_count, front_count) == (0, 1):
        print("front")
    else:
        fail(f"ambiguous URL-map references: legacy={legacy_count}, front={front_count}")


def command_assert_url_map(args: argparse.Namespace) -> None:
    actual_first, actual_second = url_map_counts(args.path, args.first_url, args.second_url)
    if (actual_first, actual_second) != (args.first_count, args.second_count):
        fail(
            "URL-map references do not match the exact transition: "
            f"first={actual_first}, second={actual_second}"
        )


def command_rewrite_url_map(args: argparse.Namespace) -> None:
    body = read_text(args.source, "source URL map")
    old_count = body.count(args.old_url)
    new_count = body.count(args.new_url)
    if (old_count, new_count) != (1, 0):
        fail("expected exact URL-map source once and destination zero times")
    try:
        if args.destination.is_symlink():
            fail("candidate URL-map destination must not be a symlink")
        if args.source.resolve() == args.destination.resolve():
            fail("candidate URL-map destination must differ from its source")
        args.destination.write_text(body.replace(args.old_url, args.new_url, 1), encoding="utf-8")
    except OSError as error:
        fail(f"could not write candidate URL map {args.destination}: {error}")


def expect_exact(document: dict[str, Any], expected: dict[str, Any], label: str) -> None:
    for key, value in expected.items():
        actual = document.get(key)
        if type(actual) is not type(value) or actual != value:
            fail(f"{label} {key} does not match its request")


def validate_ordered_window(
    requested: datetime,
    intermediate: datetime,
    observed: datetime,
    received: datetime,
    label: str,
    limit: timedelta = timedelta(seconds=30),
) -> None:
    if not requested <= intermediate <= observed <= received:
        fail(f"{label} timestamps are out of order")
    if observed - requested >= limit or received - requested >= limit:
        fail(f"{label} exceeded its phase boundary")


def command_validate_activation_ack(args: argparse.Namespace) -> None:
    document = load_json(args.path, "golden activation acknowledgement")
    expect_exact(
        document,
        {
            "schema": "subrouter.gcp.slot-activation-ack/v1",
            "challenge": args.challenge,
            "candidate_slot": args.candidate_slot,
            "candidate_generation": args.candidate_generation,
            "configured_original_clients": 4,
            "original_streams_crossed": 4,
            "direct_original_connections_verified": 2,
            "local_egress_clients_verified": 2,
            "all_original_streams_crossed_activation": True,
            "processes_stable": True,
            "sockets_stable": True,
            "local_egress_verified": True,
            "fresh_candidate_direct_connection": True,
        },
        "golden acknowledgement",
    )
    connection_id = document.get("fresh_candidate_connection_id")
    if not isinstance(connection_id, str) or not connection_id:
        fail("golden acknowledgement fresh_candidate_connection_id is required")
    requested = parse_timestamp(args.requested_at, "upgrade requested time")
    switched = parse_timestamp(args.switched_at, "provisional switch time")
    activated = parse_timestamp(document.get("activated_at"), "golden acknowledgement activation time")
    received = parse_timestamp(args.received_at, "golden acknowledgement received time")
    validate_ordered_window(
        requested,
        switched,
        activated,
        received,
        "golden activation acknowledgement",
    )


def command_validate_destination_proof(args: argparse.Namespace) -> None:
    if re.fullmatch(r"[0-9a-f]{64}", args.source_snapshot_sha256) is None:
        fail("source snapshot SHA-256 is invalid")
    document = load_json(args.path, "destination proof")
    expect_exact(
        document,
        {
            "schema": "subrouter.gcp.destination-proof/v1",
            "challenge": args.challenge,
            "operation": args.operation,
            "destination": args.destination,
            "destination_generation": args.destination_generation,
            "source": args.source,
            "source_generation": args.source_generation,
            "source_snapshot_sha256": args.source_snapshot_sha256,
            "expected_source_connections": args.expected_connections,
            "original_continuity_verified": True,
            "fresh_public_connection": True,
        },
        "destination proof",
    )
    connection_id = document.get("connection_id")
    if not isinstance(connection_id, str) or not connection_id:
        fail("destination proof connection_id must be a non-empty string")
    session_id = document.get("session_id")
    if not isinstance(session_id, str) or re.fullmatch(r"[A-Za-z0-9._:-]{1,256}", session_id) is None:
        fail("destination proof session_id is invalid")
    requested = parse_timestamp(args.requested_at, "transition requested time")
    observed = parse_timestamp(document.get("observed_at"), "destination proof observed time")
    received = parse_timestamp(args.received_at, "destination proof received time")
    validate_ordered_window(
        requested,
        requested,
        observed,
        received,
        "destination proof",
        timedelta(minutes=5),
    )


def command_validate_destination_liveness(args: argparse.Namespace) -> None:
    if re.fullmatch(r"[0-9a-f]{64}", args.connection_id) is None:
        fail("destination liveness connection_id is invalid")
    if re.fullmatch(r"[A-Za-z0-9._:-]{1,256}", args.session_id) is None:
        fail("destination liveness session_id is invalid")
    if re.fullmatch(r"[0-9a-f]{64}", args.destination_snapshot_sha256) is None:
        fail("destination liveness snapshot SHA-256 is invalid")
    document = load_json(args.path, "destination liveness proof")
    expect_exact(
        document,
        {
            "schema": "subrouter.gcp.destination-liveness/v1",
            "challenge": args.challenge,
            "operation": args.operation,
            "destination": args.destination,
            "destination_generation": args.destination_generation,
            "connection_id": args.connection_id,
            "session_id": args.session_id,
            "destination_snapshot_sha256": args.destination_snapshot_sha256,
            "requested_at": args.requested_at,
        },
        "destination liveness proof",
    )
    requested = parse_timestamp(args.requested_at, "destination liveness requested time")
    response_chunk = parse_timestamp(document.get("response_chunk_at"), "destination liveness response chunk time")
    received = parse_timestamp(args.received_at, "destination liveness received time")
    validate_ordered_window(
        requested,
        requested,
        response_chunk,
        received,
        "destination liveness proof",
        timedelta(seconds=10),
    )


def command_validate_front_handoff_checkpoint(args: argparse.Namespace) -> None:
    document = load_json(args.path, "front handoff checkpoint")

    def exact_object(value: Any, keys: set[str], label: str) -> dict[str, Any]:
        if not isinstance(value, dict) or set(value) != keys:
            fail(f"{label} has invalid fields")
        return value

    def nonnegative_integer(value: Any, label: str) -> int:
        if isinstance(value, bool) or not isinstance(value, int) or value < 0:
            fail(f"{label} must be a non-negative integer")
        return value

    def snapshot(value: Any, label: str) -> dict[str, Any]:
        result = exact_object(
            value,
            {
                "kind",
                "generation",
                "public_connections",
                "generation_connections",
                "inactive_connections",
            },
            label,
        )
        if result["kind"] != "legacy":
            fail(f"{label} kind must be legacy")
        generation = result["generation"]
        if not isinstance(generation, str) or not generation or len(generation) > 256:
            fail(f"{label} generation is invalid")
        public = nonnegative_integer(result["public_connections"], f"{label} public_connections")
        active = nonnegative_integer(
            result["generation_connections"], f"{label} generation_connections"
        )
        inactive = nonnegative_integer(
            result["inactive_connections"], f"{label} inactive_connections"
        )
        if public < args.expected_connections or active < args.expected_connections or inactive != 0:
            fail(f"{label} does not preserve the expected source connections")
        return result

    exact_object(
        document,
        {
            "schema",
            "preparation_evidence_sha256",
            "run",
            "slot",
            "listener",
            "source",
            "metrics",
            "handoff_completed_at",
        },
        "front handoff checkpoint",
    )
    if document["schema"] != "subrouter.gcp.front-handoff-checkpoint/v1":
        fail("front handoff checkpoint schema is invalid")
    if document["preparation_evidence_sha256"] != args.preparation_sha256:
        fail("front handoff checkpoint preparation evidence does not match")
    if re.fullmatch(r"[0-9a-f]{64}", args.preparation_sha256) is None:
        fail("expected preparation evidence digest is invalid")

    run = exact_object(document["run"], {"id", "project", "zone", "instance"}, "checkpoint run")
    run_id = run["id"]
    if (
        not isinstance(run_id, str)
        or len(run_id) > 128
        or re.fullmatch(r"[A-Za-z0-9._-]+", run_id) is None
    ):
        fail("checkpoint run ID is invalid")
    if (run["project"], run["zone"], run["instance"]) != (
        args.project,
        args.zone,
        args.instance,
    ):
        fail("front handoff checkpoint target does not match")
    if document["slot"] != args.slot or args.slot not in ("slot-a", "slot-b"):
        fail("front handoff checkpoint slot does not match")

    listener = exact_object(
        document["listener"],
        {
            "source_pid",
            "source_fd",
            "inode",
        },
        "checkpoint listener",
    )
    if nonnegative_integer(listener["source_pid"], "checkpoint source PID") <= 1:
        fail("checkpoint source PID is invalid")
    nonnegative_integer(listener["source_fd"], "checkpoint source descriptor")
    inode = listener["inode"]
    if not isinstance(inode, str) or re.fullmatch(r"socket:\[[1-9][0-9]*\]", inode) is None:
        fail("checkpoint listener inode is invalid")

    source = exact_object(document["source"], {"before", "after"}, "checkpoint source")
    before = snapshot(source["before"], "checkpoint source before")
    after = snapshot(source["after"], "checkpoint source after")
    if before["generation"] != after["generation"]:
        fail("checkpoint source generation changed during handoff")

    metrics = exact_object(document["metrics"], {"legacy", "slot", "front"}, "checkpoint metrics")
    for service in ("legacy", "slot", "front"):
        service_metrics = exact_object(
            metrics[service], {"nrestarts", "oom_kill"}, f"checkpoint {service} metrics"
        )
        nonnegative_integer(service_metrics["nrestarts"], f"checkpoint {service} NRestarts")
        nonnegative_integer(service_metrics["oom_kill"], f"checkpoint {service} oom_kill")

    completed_at = canonical_timestamp(
        document["handoff_completed_at"], "front handoff checkpoint completion time"
    )
    if completed_at != document["handoff_completed_at"]:
        fail("front handoff checkpoint completion time is not canonical")
    print(json.dumps(document, separators=(",", ":"), sort_keys=True))


def add_path(parser: argparse.ArgumentParser, name: str) -> None:
    parser.add_argument(name, type=Path)


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser()
    commands = parser.add_subparsers(dest="command", required=True)

    target = commands.add_parser("validate-target")
    add_path(target, "cloud_config")
    target.add_argument("instance")
    target.add_argument("public_url")
    target.set_defaults(handler=command_validate_target)

    manifest = commands.add_parser("manifest-sha")
    add_path(manifest, "manifest")
    manifest.add_argument("asset")
    manifest.set_defaults(handler=command_manifest_sha)

    identity = commands.add_parser("gce-instance-identity")
    add_path(identity, "document")
    identity.add_argument("expected_name")
    identity.add_argument("expected_zone")
    identity.set_defaults(handler=command_gce_instance_identity)

    private_file = commands.add_parser("validate-private-file")
    add_path(private_file, "path")
    private_file.set_defaults(handler=command_validate_private_file)

    binding = commands.add_parser("validate-instance-binding")
    add_path(binding, "bootstrap")
    binding.add_argument("live_identity")
    binding.add_argument("--now")
    binding.set_defaults(handler=command_validate_instance_binding)

    legacy_status = commands.add_parser("validate-legacy-supervisor-status")
    add_path(legacy_status, "path")
    legacy_status.set_defaults(handler=command_validate_legacy_supervisor_status)

    probe = commands.add_parser("probe-slot-endpoint")
    probe.add_argument("port", type=int)
    probe.add_argument("path")
    probe.set_defaults(handler=command_probe_slot_endpoint)

    defaults = commands.add_parser("validate-auth-defaults")
    add_path(defaults, "path")
    defaults.set_defaults(handler=command_validate_auth_defaults)

    classify = commands.add_parser("classify-url-map")
    add_path(classify, "path")
    classify.add_argument("legacy_url")
    classify.add_argument("front_url")
    classify.set_defaults(handler=command_classify_url_map)

    assertion = commands.add_parser("assert-url-map")
    add_path(assertion, "path")
    assertion.add_argument("first_url")
    assertion.add_argument("first_count", type=int)
    assertion.add_argument("second_url")
    assertion.add_argument("second_count", type=int)
    assertion.set_defaults(handler=command_assert_url_map)

    rewrite = commands.add_parser("rewrite-url-map")
    add_path(rewrite, "source")
    add_path(rewrite, "destination")
    rewrite.add_argument("old_url")
    rewrite.add_argument("new_url")
    rewrite.set_defaults(handler=command_rewrite_url_map)

    activation = commands.add_parser("validate-activation-ack")
    add_path(activation, "path")
    activation.add_argument("challenge")
    activation.add_argument("candidate_slot")
    activation.add_argument("candidate_generation")
    activation.add_argument("requested_at")
    activation.add_argument("switched_at")
    activation.add_argument("received_at")
    activation.set_defaults(handler=command_validate_activation_ack)

    proof = commands.add_parser("validate-destination-proof")
    add_path(proof, "path")
    proof.add_argument("challenge")
    proof.add_argument("operation")
    proof.add_argument("destination")
    proof.add_argument("destination_generation")
    proof.add_argument("source")
    proof.add_argument("source_generation")
    proof.add_argument("source_snapshot_sha256")
    proof.add_argument("expected_connections", type=int)
    proof.add_argument("requested_at")
    proof.add_argument("received_at")
    proof.set_defaults(handler=command_validate_destination_proof)

    liveness = commands.add_parser("validate-destination-liveness")
    add_path(liveness, "path")
    liveness.add_argument("challenge")
    liveness.add_argument("operation")
    liveness.add_argument("destination")
    liveness.add_argument("destination_generation")
    liveness.add_argument("connection_id")
    liveness.add_argument("session_id")
    liveness.add_argument("destination_snapshot_sha256")
    liveness.add_argument("requested_at")
    liveness.add_argument("received_at")
    liveness.set_defaults(handler=command_validate_destination_liveness)

    checkpoint = commands.add_parser("validate-front-handoff-checkpoint")
    add_path(checkpoint, "path")
    checkpoint.add_argument("preparation_sha256")
    checkpoint.add_argument("project")
    checkpoint.add_argument("zone")
    checkpoint.add_argument("instance")
    checkpoint.add_argument("slot")
    checkpoint.add_argument("expected_connections", type=int)
    checkpoint.set_defaults(handler=command_validate_front_handoff_checkpoint)

    return parser


def main() -> None:
    args = build_parser().parse_args()
    if args.command == "assert-url-map" and (args.first_count < 0 or args.second_count < 0):
        fail("URL-map reference counts must be non-negative")
    if args.command == "validate-destination-proof" and args.expected_connections <= 0:
        fail("expected source connections must be positive")
    if args.command == "validate-front-handoff-checkpoint" and args.expected_connections <= 0:
        fail("expected checkpoint source connections must be positive")
    args.handler(args)


if __name__ == "__main__":
    try:
        main()
    except ContractError as error:
        print(f"deployment-contract: {error}", file=sys.stderr)
        raise SystemExit(1) from None
