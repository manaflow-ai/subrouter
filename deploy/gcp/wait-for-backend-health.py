#!/usr/bin/env python3
"""Wait until every reported load-balancer backend is continuously healthy."""

from __future__ import annotations

import argparse
import datetime as dt
import hashlib
import json
import subprocess
import sys
import time
from typing import Any


def healthy_backend_membership(value: Any) -> tuple[str, ...] | None:
    if not isinstance(value, list) or not value:
        return None
    identities: list[str] = []
    for backend in value:
        if not isinstance(backend, dict):
            return None
        backend_id = backend.get("backend")
        if not isinstance(backend_id, str) or not backend_id:
            return None
        status = backend.get("status")
        if not isinstance(status, dict):
            return None
        health_statuses = status.get("healthStatus")
        if not isinstance(health_statuses, list) or not health_statuses:
            return None
        for item in health_statuses:
            if not isinstance(item, dict) or item.get("healthState") != "HEALTHY":
                return None
            instance = item.get("instance")
            ip_address = item.get("ipAddress")
            port = item.get("port")
            if (
                not isinstance(instance, str)
                or not instance
                or not isinstance(ip_address, str)
                or not ip_address
                or isinstance(port, bool)
                or not isinstance(port, int)
                or port <= 0
                or port > 65535
            ):
                return None
            identities.append(
                json.dumps(
                    {
                        "backend": backend_id,
                        "instance": instance,
                        "ip_address": ip_address,
                        "port": port,
                    },
                    separators=(",", ":"),
                    sort_keys=True,
                )
            )
    membership = tuple(sorted(identities))
    if not membership or len(set(membership)) != len(membership):
        return None
    return membership


def timestamp(value: dt.datetime) -> str:
    return value.isoformat(timespec="milliseconds").replace("+00:00", "Z")


def truncate_to_milliseconds(value: dt.datetime) -> dt.datetime:
    return value.replace(microsecond=(value.microsecond // 1000) * 1000)


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--minimum-stable-seconds", type=float, required=True)
    parser.add_argument("--timeout-seconds", type=float, required=True)
    parser.add_argument("--poll-seconds", type=float, required=True)
    parser.add_argument("--maximum-sample-gap-seconds", type=float, required=True)
    parser.add_argument("command", nargs=argparse.REMAINDER)
    args = parser.parse_args()
    command = args.command[1:] if args.command[:1] == ["--"] else args.command
    if (
        args.minimum_stable_seconds <= 0
        or args.timeout_seconds < args.minimum_stable_seconds
        or args.poll_seconds <= 0
        or args.maximum_sample_gap_seconds < args.poll_seconds
        or not command
    ):
        parser.error("invalid timing bounds or missing health command")

    deadline = time.monotonic() + args.timeout_seconds
    stable_started_monotonic: float | None = None
    stable_started_wall: dt.datetime | None = None
    last_healthy_monotonic: float | None = None
    healthy_samples = 0
    maximum_sample_gap = 0.0
    stable_membership: tuple[str, ...] | None = None

    while time.monotonic() < deadline:
        remaining = deadline - time.monotonic()
        try:
            completed = subprocess.run(
                command,
                check=False,
                capture_output=True,
                text=True,
                timeout=max(0.1, min(60.0, remaining)),
            )
            payload = json.loads(completed.stdout) if completed.returncode == 0 else None
            membership = healthy_backend_membership(payload)
        except (json.JSONDecodeError, OSError, subprocess.SubprocessError):
            membership = None

        observed_monotonic = time.monotonic()
        observed_wall = truncate_to_milliseconds(dt.datetime.now(dt.timezone.utc))
        if membership is not None:
            sample_gap = 0.0
            if last_healthy_monotonic is not None:
                sample_gap = observed_monotonic - last_healthy_monotonic
            if (
                stable_started_monotonic is None
                or membership != stable_membership
                or sample_gap > args.maximum_sample_gap_seconds
            ):
                stable_started_monotonic = observed_monotonic
                stable_started_wall = observed_wall
                stable_membership = membership
                healthy_samples = 1
                maximum_sample_gap = 0.0
            else:
                healthy_samples += 1
                maximum_sample_gap = max(maximum_sample_gap, sample_gap)
            last_healthy_monotonic = observed_monotonic

            monotonic_duration = observed_monotonic - stable_started_monotonic
            wall_duration = observed_wall - stable_started_wall
            if (
                monotonic_duration >= args.minimum_stable_seconds
                and wall_duration.total_seconds() >= args.minimum_stable_seconds
            ):
                result = {
                    "all_healthy": True,
                    "stable_since": timestamp(stable_started_wall),
                    "verified_at": timestamp(observed_wall),
                    "duration_ms": int(wall_duration.total_seconds() * 1000),
                    "healthy_samples": healthy_samples,
                    "max_sample_gap_ms": int(maximum_sample_gap * 1000),
                    "backend_membership_sha256": hashlib.sha256(
                        json.dumps(membership, separators=(",", ":")).encode()
                    ).hexdigest(),
                }
                json.dump(result, sys.stdout, separators=(",", ":"), sort_keys=True)
                sys.stdout.write("\n")
                return 0
        else:
            stable_started_monotonic = None
            stable_started_wall = None
            last_healthy_monotonic = None
            healthy_samples = 0
            maximum_sample_gap = 0.0
            stable_membership = None

        remaining = deadline - time.monotonic()
        if remaining > 0:
            time.sleep(min(args.poll_seconds, remaining))

    print("backend health did not remain continuously healthy before timeout", file=sys.stderr)
    return 1


if __name__ == "__main__":
    raise SystemExit(main())
