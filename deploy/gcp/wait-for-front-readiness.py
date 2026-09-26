#!/usr/bin/env python3
"""Require backend health and a public front canary in every readiness sample."""

from __future__ import annotations

import argparse
import datetime as dt
import hashlib
import json
import math
import os
import re
import subprocess
import sys
import tempfile
import time

from backend_health import healthy_backend_membership


SESSION_PREFIX = re.compile(r"[A-Za-z0-9._-]{1,160}")


def timestamp(value: dt.datetime) -> str:
    return value.isoformat(timespec="milliseconds").replace("+00:00", "Z")


def truncate_to_milliseconds(value: dt.datetime) -> dt.datetime:
    return value.replace(microsecond=(value.microsecond // 1000) * 1000)


def session_sha256(value: str) -> str:
    return hashlib.sha256(value.encode()).hexdigest()


def write_sessions(path: str, sessions: list[str]) -> str:
    destination = os.path.abspath(path)
    parent = os.path.dirname(destination)
    payload = "".join(f"{session}\n" for session in sessions).encode()
    descriptor, temporary = tempfile.mkstemp(prefix=f".{os.path.basename(path)}.", dir=parent)
    try:
        os.fchmod(descriptor, 0o600)
        with os.fdopen(descriptor, "wb") as output:
            output.write(payload)
        os.replace(temporary, destination)
    finally:
        try:
            os.unlink(temporary)
        except FileNotFoundError:
            pass
    return hashlib.sha256(payload).hexdigest()


def record_probe_error(path: str | None, attempt: int, detail: str) -> None:
    if path is None:
        return
    flags = os.O_APPEND | os.O_CREAT | os.O_WRONLY | getattr(os, "O_NOFOLLOW", 0)
    descriptor = os.open(path, flags, 0o600)
    try:
        os.write(descriptor, f"attempt={attempt} {detail.rstrip()}\n".encode())
    finally:
        os.close(descriptor)


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--minimum-stable-seconds", type=float, required=True)
    parser.add_argument("--timeout-seconds", type=float, required=True)
    parser.add_argument("--poll-seconds", type=float, required=True)
    parser.add_argument("--maximum-sample-gap-seconds", type=float, required=True)
    parser.add_argument("--minimum-samples", type=int, required=True)
    parser.add_argument("--session-prefix", required=True)
    parser.add_argument("--sessions-file", required=True)
    parser.add_argument("--probe-stderr-log")
    parser.add_argument("command", nargs=argparse.REMAINDER)
    args = parser.parse_args()
    command = args.command[1:] if args.command[:1] == ["--"] else args.command
    maximum_attempts = math.ceil(args.timeout_seconds / args.poll_seconds) + 1 if args.poll_seconds > 0 else 0
    if (
        args.minimum_stable_seconds <= 0
        or args.timeout_seconds < args.minimum_stable_seconds
        or args.poll_seconds <= 0
        or args.maximum_sample_gap_seconds < args.poll_seconds
        or args.minimum_samples < 2
        or maximum_attempts > 600
        or SESSION_PREFIX.fullmatch(args.session_prefix) is None
        or not command
    ):
        parser.error("invalid timing bounds, session prefix, or probe command")
    sessions_parent = os.path.dirname(os.path.abspath(args.sessions_file))
    if not os.path.isdir(sessions_parent):
        parser.error("sessions-file parent does not exist")

    deadline = time.monotonic() + args.timeout_seconds
    stable_started_monotonic: float | None = None
    stable_started_wall: dt.datetime | None = None
    last_healthy_monotonic: float | None = None
    stable_membership: tuple[str, ...] | None = None
    stable_sessions: list[str] = []
    first_attempt = 0
    maximum_sample_gap = 0.0
    attempts = 0

    while time.monotonic() < deadline:
        attempts += 1
        session = f"{args.session_prefix}-{attempts}"
        environment = os.environ.copy()
        environment["SUBROUTER_CANARY_SESSION"] = session
        remaining = deadline - time.monotonic()
        try:
            completed = subprocess.run(
                command,
                check=False,
                capture_output=True,
                text=True,
                timeout=max(0.1, min(60.0, remaining)),
                env=environment,
            )
            if completed.stderr:
                record_probe_error(args.probe_stderr_log, attempts, completed.stderr)
            payload = json.loads(completed.stdout) if completed.returncode == 0 else None
            membership = healthy_backend_membership(payload)
            if completed.returncode != 0:
                record_probe_error(args.probe_stderr_log, attempts, f"probe exit={completed.returncode}")
        except (json.JSONDecodeError, OSError, subprocess.SubprocessError) as error:
            record_probe_error(args.probe_stderr_log, attempts, f"probe error={error}")
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
                or stable_started_wall is None
                or observed_wall < stable_started_wall
            ):
                stable_started_monotonic = observed_monotonic
                stable_started_wall = observed_wall
                stable_membership = membership
                stable_sessions = [session]
                first_attempt = attempts
                maximum_sample_gap = 0.0
            else:
                stable_sessions.append(session)
                maximum_sample_gap = max(maximum_sample_gap, sample_gap)
            last_healthy_monotonic = observed_monotonic

            monotonic_duration = observed_monotonic - stable_started_monotonic
            wall_duration = observed_wall - stable_started_wall
            if (
                monotonic_duration >= args.minimum_stable_seconds
                and wall_duration.total_seconds() >= args.minimum_stable_seconds
                and len(stable_sessions) >= args.minimum_samples
            ):
                duration_ms = wall_duration // dt.timedelta(milliseconds=1)
                gap_ms = math.ceil(maximum_sample_gap * 1000)
                membership_sha = hashlib.sha256(
                    json.dumps(stable_membership, separators=(",", ":")).encode()
                ).hexdigest()
                session_set_sha = write_sessions(args.sessions_file, stable_sessions)
                result = {
                    "backend_health": {
                        "all_healthy": True,
                        "stable_since": timestamp(stable_started_wall),
                        "verified_at": timestamp(observed_wall),
                        "duration_ms": duration_ms,
                        "healthy_samples": len(stable_sessions),
                        "max_sample_gap_ms": gap_ms,
                        "backend_membership_sha256": membership_sha,
                    },
                    "canary": {
                        "first_observed_at": timestamp(stable_started_wall),
                        "verified_at": timestamp(observed_wall),
                        "stable_duration_ms": duration_ms,
                        "healthy_samples": len(stable_sessions),
                        "max_sample_gap_ms": gap_ms,
                        "first_proof_attempts": first_attempt,
                        "verified_proof_attempts": attempts,
                        "first_session_sha256": session_sha256(stable_sessions[0]),
                        "verified_session_sha256": session_sha256(stable_sessions[-1]),
                        "session_set_sha256": session_set_sha,
                    },
                }
                json.dump(result, sys.stdout, separators=(",", ":"), sort_keys=True)
                sys.stdout.write("\n")
                return 0
        else:
            stable_started_monotonic = None
            stable_started_wall = None
            last_healthy_monotonic = None
            stable_membership = None
            stable_sessions = []
            first_attempt = 0
            maximum_sample_gap = 0.0

        remaining = deadline - time.monotonic()
        if remaining > 0:
            time.sleep(min(args.poll_seconds, remaining))

    print("front backend and public canary did not remain continuously healthy before timeout", file=sys.stderr)
    return 1


if __name__ == "__main__":
    raise SystemExit(main())
