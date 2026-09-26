#!/usr/bin/env python3
"""Run the deployment-specific functional gates for a supervised cutover.

The generic LaunchAgent migration intentionally knows nothing about hosts,
accounts, sessions, or credentials.  This runner preserves that boundary while
making the deployment callback reviewable and fail closed: a private manifest
names one executable and one private config file for each required gate, and
every gate must return a tiny, exact JSON success record.
"""

from __future__ import annotations

import argparse
import ctypes
import errno
import fcntl
import hashlib
import json
import os
import secrets
import signal
import stat
import subprocess
import sys
import tempfile
import time
from dataclasses import dataclass
from pathlib import Path


SCHEMA = "subrouter.launchagent-functional-canary/v1"
LEG_SCHEMA = "subrouter.launchagent-functional-canary-leg/v1"
REQUIRED_LEGS = (
    "peer-health-readiness",
    "authenticated-routed-codex",
    "sticky-reuse",
    "safe-failover-reuse",
    "authenticated-routed-claude",
    "existing-session-next-turn",
)
MAX_TOTAL_TIMEOUT = 270
MAX_LEG_OUTPUT = 64 * 1024
_active_child: subprocess.Popen[bytes] | None = None
_active_child_token: str | None = None
_active_child_pgid: int | None = None
_active_child_identity: str | None = None
_tracked_child_identities: dict[int, str] = {}


@dataclass(frozen=True)
class _ProcessInfo:
    pid: int
    ppid: int
    pgid: int
    started: str
    command: bytes
    identity: str


class _DarwinBSDInfo(ctypes.Structure):
    _fields_ = [
        ("pbi_flags", ctypes.c_uint32),
        ("pbi_status", ctypes.c_uint32),
        ("pbi_xstatus", ctypes.c_uint32),
        ("pbi_pid", ctypes.c_uint32),
        ("pbi_ppid", ctypes.c_uint32),
        ("pbi_uid", ctypes.c_uint32),
        ("pbi_gid", ctypes.c_uint32),
        ("pbi_ruid", ctypes.c_uint32),
        ("pbi_rgid", ctypes.c_uint32),
        ("pbi_svuid", ctypes.c_uint32),
        ("pbi_svgid", ctypes.c_uint32),
        ("rfu_1", ctypes.c_uint32),
        ("pbi_comm", ctypes.c_char * 16),
        ("pbi_name", ctypes.c_char * 32),
        ("pbi_nfiles", ctypes.c_uint32),
        ("pbi_pgid", ctypes.c_uint32),
        ("pbi_pjobc", ctypes.c_uint32),
        ("e_tdev", ctypes.c_uint32),
        ("e_tpgid", ctypes.c_uint32),
        ("pbi_nice", ctypes.c_int32),
        ("pbi_start_tvsec", ctypes.c_uint64),
        ("pbi_start_tvusec", ctypes.c_uint64),
    ]


class _DarwinAuditToken(ctypes.Structure):
    _fields_ = [("val", ctypes.c_uint32 * 8)]


class CanaryError(Exception):
    pass


class CanarySignal(CanaryError):
    pass


def _fail(message: str) -> None:
    raise CanaryError(message)


def _sha256(path: Path) -> str:
    digest = hashlib.sha256()
    flags = os.O_RDONLY | getattr(os, "O_NOFOLLOW", 0)
    descriptor = os.open(path, flags)
    try:
        opened = os.fstat(descriptor)
        named = path.lstat()
        if (opened.st_dev, opened.st_ino) != (named.st_dev, named.st_ino):
            _fail("secure file identity changed while opening")
        with os.fdopen(descriptor, "rb", closefd=False) as source:
            for chunk in iter(lambda: source.read(1024 * 1024), b""):
                digest.update(chunk)
    finally:
        os.close(descriptor)
    return digest.hexdigest()


def _secure_file(raw: object, label: str, *, executable: bool = False) -> Path:
    if not isinstance(raw, str) or not raw or not os.path.isabs(raw):
        _fail(f"{label} must be an absolute path")
    path = Path(raw)
    try:
        info = path.lstat()
    except OSError as error:
        _fail(f"{label} is unavailable: {error.strerror}")
    if stat.S_ISLNK(info.st_mode) or not stat.S_ISREG(info.st_mode):
        _fail(f"{label} must be a regular non-symlink file")
    if info.st_uid != os.getuid():
        _fail(f"{label} must be owned by the current user")
    if executable:
        if not info.st_mode & stat.S_IXUSR:
            _fail(f"{label} must be executable by its owner")
        if info.st_mode & 0o022 or info.st_mode & (stat.S_ISUID | stat.S_ISGID):
            _fail(f"{label} must not be writable by group/other or privileged")
    else:
        if info.st_mode & 0o077:
            _fail(f"{label} must not grant group or other permissions")
        if info.st_mode & 0o111:
            _fail(f"{label} must not be executable")
    return path


def _load_json(path: Path, label: str, limit: int = MAX_LEG_OUTPUT) -> tuple[object, bytes]:
    try:
        descriptor = os.open(path, os.O_RDONLY | getattr(os, "O_NOFOLLOW", 0))
        try:
            opened = os.fstat(descriptor)
            named = path.lstat()
            if (opened.st_dev, opened.st_ino) != (named.st_dev, named.st_ino):
                _fail(f"{label} identity changed while opening")
            chunks = bytearray()
            while len(chunks) <= limit:
                chunk = os.read(descriptor, min(64 * 1024, limit + 1 - len(chunks)))
                if not chunk:
                    break
                chunks.extend(chunk)
            body = bytes(chunks)
        finally:
            os.close(descriptor)
    except OSError as error:
        _fail(f"could not securely read {label}")
    if len(body) > limit:
        _fail(f"{label} is too large")
    def reject_duplicates(pairs: list[tuple[str, object]]) -> dict[str, object]:
        value: dict[str, object] = {}
        for key, item in pairs:
            if key in value:
                _fail(f"{label} contains a duplicate field")
            value[key] = item
        return value
    try:
        return json.loads(body, object_pairs_hook=reject_duplicates), body
    except (UnicodeDecodeError, json.JSONDecodeError):
        _fail(f"{label} is not valid JSON")


def _strict_keys(value: object, expected: set[str], label: str) -> dict[str, object]:
    if not isinstance(value, dict):
        _fail(f"{label} must be an object")
    actual = set(value)
    if actual != expected:
        _fail(f"{label} has missing or unknown fields")
    return value


def _valid_sha256(value: object) -> bool:
    return isinstance(value, str) and len(value) == 64 and all(char in "0123456789abcdef" for char in value)


def _canonical_path(path: Path) -> str:
    return os.path.realpath(os.path.abspath(path))


def _reject_evidence_alias(evidence: Path, inputs: list[Path]) -> None:
    evidence_canonical = _canonical_path(evidence)
    evidence_identity: tuple[int, int] | None = None
    try:
        evidence_info = evidence.lstat()
        evidence_identity = (evidence_info.st_dev, evidence_info.st_ino)
    except FileNotFoundError:
        pass
    except OSError:
        _fail("evidence file identity is unavailable")
    for input_path in inputs:
        if evidence_canonical == _canonical_path(input_path):
            _fail("evidence_file aliases a canary input")
        if evidence_identity is None:
            continue
        try:
            input_info = input_path.lstat()
        except OSError:
            _fail("canary input identity is unavailable")
        if evidence_identity == (input_info.st_dev, input_info.st_ino):
            _fail("evidence_file aliases a canary input")


def _recognized_prior_evidence(path: Path) -> bytes | None:
    if not os.path.lexists(path):
        return None
    _secure_file(str(path), "evidence file")
    try:
        parsed, body = _load_json(path, "evidence file")
    except CanaryError:
        _fail("evidence file is not recognized prior canary evidence")
    if not isinstance(parsed, dict):
        _fail("evidence file is not recognized prior canary evidence")
    status = parsed.get("status")
    expected = {
        "schema",
        "source_git_oid_unverified",
        "manifest_sha256",
        "candidate_worker_sha256",
        "run_id",
        "started_at_epoch",
        "status",
        "legs",
    }
    if status in {"passed", "failed"}:
        expected.add("completed_at_epoch")
    if status == "failed":
        expected.add("failure")
    if set(parsed) != expected or parsed.get("schema") != SCHEMA:
        _fail("evidence file is not recognized prior canary evidence")
    source_oid = parsed.get("source_git_oid_unverified")
    if (
        not isinstance(source_oid, str)
        or len(source_oid) != 40
        or any(char not in "0123456789abcdef" for char in source_oid)
        or not _valid_sha256(parsed.get("manifest_sha256"))
        or not _valid_sha256(parsed.get("candidate_worker_sha256"))
        or not _valid_sha256(parsed.get("run_id"))
        or not isinstance(parsed.get("started_at_epoch"), int)
        or isinstance(parsed.get("started_at_epoch"), bool)
    ):
        _fail("evidence file is not recognized prior canary evidence")
    if status in {"passed", "failed"} and (
        not isinstance(parsed.get("completed_at_epoch"), int)
        or isinstance(parsed.get("completed_at_epoch"), bool)
    ):
        _fail("evidence file is not recognized prior canary evidence")
    if status == "failed" and (
        not isinstance(parsed.get("failure"), str) or not parsed.get("failure")
    ):
        _fail("evidence file is not recognized prior canary evidence")
    legs = parsed.get("legs")
    if not isinstance(legs, list) or len(legs) > len(REQUIRED_LEGS):
        _fail("evidence file is not recognized prior canary evidence")
    if status == "passed" and len(legs) != len(REQUIRED_LEGS):
        _fail("evidence file is not recognized prior canary evidence")
    for index, raw_leg in enumerate(legs):
        if not isinstance(raw_leg, dict) or set(raw_leg) != {
            "name", "ok", "duration_ms", "executable_sha256", "config_sha256"
        }:
            _fail("evidence file is not recognized prior canary evidence")
        duration = raw_leg.get("duration_ms")
        if (
            raw_leg.get("name") != REQUIRED_LEGS[index]
            or raw_leg.get("ok") is not True
            or not isinstance(duration, int)
            or isinstance(duration, bool)
            or duration < 0
            or not _valid_sha256(raw_leg.get("executable_sha256"))
            or not _valid_sha256(raw_leg.get("config_sha256"))
        ):
            _fail("evidence file is not recognized prior canary evidence")
    return body


def _load_manifest(path: Path) -> tuple[dict[str, object], list[dict[str, object]], str]:
    parsed, manifest_bytes = _load_json(path, "canary manifest")
    manifest = _strict_keys(
        parsed,
        {
            "schema",
            "source_git_oid_unverified",
            "candidate_worker",
            "evidence_file",
            "total_timeout_seconds",
            "legs",
        },
        "canary manifest",
    )
    if manifest["schema"] != SCHEMA:
        _fail("canary manifest schema is unsupported")
    source_oid = manifest["source_git_oid_unverified"]
    if not isinstance(source_oid, str) or len(source_oid) != 40 or any(
        char not in "0123456789abcdef" for char in source_oid
    ):
        _fail("source_git_oid_unverified must be a lowercase full Git object ID")
    candidate = _strict_keys(manifest["candidate_worker"], {"path", "sha256"}, "candidate_worker")
    candidate_path = _secure_file(candidate["path"], "candidate worker", executable=True)
    candidate_hash = candidate["sha256"]
    if not _valid_sha256(candidate_hash):
        _fail("candidate worker sha256 is invalid")
    if _sha256(candidate_path) != candidate_hash:
        _fail("candidate worker sha256 does not match")
    timeout = manifest["total_timeout_seconds"]
    if not isinstance(timeout, int) or isinstance(timeout, bool) or not 1 <= timeout <= MAX_TOTAL_TIMEOUT:
        _fail(f"total_timeout_seconds must be between 1 and {MAX_TOTAL_TIMEOUT}")
    evidence_raw = manifest["evidence_file"]
    if not isinstance(evidence_raw, str) or not os.path.isabs(evidence_raw):
        _fail("evidence_file must be an absolute path")
    evidence = Path(evidence_raw)
    parent = evidence.parent
    try:
        parent_info = parent.lstat()
    except OSError as error:
        _fail(f"evidence directory is unavailable: {error.strerror}")
    if not stat.S_ISDIR(parent_info.st_mode) or stat.S_ISLNK(parent_info.st_mode):
        _fail("evidence directory must be a real directory")
    if parent_info.st_uid != os.getuid() or parent_info.st_mode & 0o077:
        _fail("evidence directory must be current-user-owned and private")
    raw_legs = manifest["legs"]
    if not isinstance(raw_legs, list) or len(raw_legs) != len(REQUIRED_LEGS):
        _fail("canary manifest must contain every required leg exactly once")
    legs: list[dict[str, object]] = []
    helper_identity: tuple[str, str] | None = None
    for index, raw_leg in enumerate(raw_legs):
        leg = _strict_keys(
            raw_leg,
            {"name", "executable", "executable_sha256", "config_file", "config_sha256", "timeout_seconds"},
            f"leg {index + 1}",
        )
        if leg["name"] != REQUIRED_LEGS[index]:
            _fail("canary legs are missing, duplicated, or out of order")
        leg_timeout = leg["timeout_seconds"]
        if not isinstance(leg_timeout, int) or isinstance(leg_timeout, bool) or leg_timeout < 1:
            _fail(f"leg {index + 1} timeout must be a positive integer")
        executable = _secure_file(leg["executable"], f"leg {index + 1} executable", executable=True)
        config = _secure_file(leg["config_file"], f"leg {index + 1} config")
        for hash_field, hashed_path in (("executable_sha256", executable), ("config_sha256", config)):
            expected_hash = leg[hash_field]
            if not _valid_sha256(expected_hash):
                _fail(f"leg {index + 1} {hash_field} is invalid")
            if _sha256(hashed_path) != expected_hash:
                _fail(f"leg {index + 1} {hash_field} does not match")
        current_helper = (str(executable), str(leg["executable_sha256"]))
        if helper_identity is None:
            helper_identity = current_helper
        elif current_helper != helper_identity:
            _fail("all canary legs must use one identical reviewed helper")
        leg["executable"] = str(executable)
        leg["config_file"] = str(config)
        legs.append(leg)
    input_paths = [path, candidate_path]
    for leg in legs:
        input_paths.extend((Path(str(leg["executable"])), Path(str(leg["config_file"]))))
    _reject_evidence_alias(evidence, input_paths)
    _recognized_prior_evidence(evidence)
    return manifest, legs, hashlib.sha256(manifest_bytes).hexdigest()


def _open_transaction_worker_binding(
    manifest: dict[str, object],
) -> tuple[int, dict[str, object]]:
    """Open and retain the exact migration worker identity through all legs."""
    expected_path = os.environ.get("SUBROUTER_CANARY_TRANSACTION_WORKER_PATH", "")
    expected_hash = os.environ.get("SUBROUTER_CANARY_TRANSACTION_WORKER_SHA256", "")
    if not expected_path or not expected_hash:
        _fail("migration transaction worker binding is required")
    if not os.path.isabs(expected_path) or not _valid_sha256(expected_hash):
        _fail("migration transaction worker binding is invalid")
    candidate = manifest["candidate_worker"]
    if not isinstance(candidate, dict):
        _fail("candidate worker identity is unavailable")
    if candidate.get("path") != expected_path:
        _fail("candidate worker path does not match migration transaction")
    if candidate.get("sha256") != expected_hash:
        _fail("candidate worker sha256 does not match migration transaction")
    flags = os.O_RDONLY | getattr(os, "O_NOFOLLOW", 0)
    descriptor = -1
    try:
        descriptor = os.open(expected_path, flags)
        opened = os.fstat(descriptor)
        named = Path(expected_path).lstat()
        if (
            not stat.S_ISREG(opened.st_mode)
            or stat.S_ISLNK(named.st_mode)
            or (opened.st_dev, opened.st_ino) != (named.st_dev, named.st_ino)
        ):
            os.close(descriptor)
            _fail("migration transaction worker identity changed while opening")
        digest = hashlib.sha256()
        while True:
            chunk = os.read(descriptor, 1024 * 1024)
            if not chunk:
                break
            digest.update(chunk)
        os.lseek(descriptor, 0, os.SEEK_SET)
        if digest.hexdigest() != expected_hash:
            os.close(descriptor)
            _fail("migration transaction worker bytes do not match captured sha256")
        return descriptor, {
            "device": opened.st_dev,
            "inode": opened.st_ino,
            "path": expected_path,
            "sha256": expected_hash,
        }
    except CanaryError:
        raise
    except OSError:
        if descriptor >= 0:
            os.close(descriptor)
        _fail("migration transaction worker could not be bound")


def _verify_transaction_worker_binding(
    descriptor: int, identity: dict[str, object]
) -> None:
    try:
        opened_before = os.fstat(descriptor)
        named = Path(str(identity["path"])).lstat()
        if (
            not stat.S_ISREG(opened_before.st_mode)
            or stat.S_ISLNK(named.st_mode)
            or (opened_before.st_dev, opened_before.st_ino)
            != (identity["device"], identity["inode"])
            or (named.st_dev, named.st_ino)
            != (identity["device"], identity["inode"])
        ):
            _fail("migration transaction worker path identity changed")
        os.lseek(descriptor, 0, os.SEEK_SET)
        digest = hashlib.sha256()
        while True:
            chunk = os.read(descriptor, 1024 * 1024)
            if not chunk:
                break
            digest.update(chunk)
        os.lseek(descriptor, 0, os.SEEK_SET)
        opened_after = os.fstat(descriptor)
        if (
            digest.hexdigest() != identity["sha256"]
            or opened_before.st_size != opened_after.st_size
            or opened_before.st_mtime_ns != opened_after.st_mtime_ns
        ):
            _fail("migration transaction worker bytes changed after binding")
    except CanaryError:
        raise
    except (KeyError, OSError):
        _fail("migration transaction worker binding could not be revalidated")


def _private_lease_directory() -> Path:
    # This namespace must not follow TMPDIR: two otherwise identical operators
    # can legitimately inherit different temp roots, but they must still
    # contend on the same candidate/resource leases.
    directory = Path("/tmp") / f"subrouter-functional-canary-leases-{os.getuid()}"
    try:
        directory.mkdir(mode=0o700)
    except FileExistsError:
        pass
    except OSError:
        _fail("could not prepare functional canary lease directory")
    try:
        info = directory.lstat()
    except OSError:
        _fail("could not inspect functional canary lease directory")
    if (
        not stat.S_ISDIR(info.st_mode)
        or stat.S_ISLNK(info.st_mode)
        or info.st_uid != os.getuid()
        or info.st_mode & 0o077
    ):
        _fail("functional canary lease directory is not private and safe")
    return directory


def _acquire_one_run_lease(lock_path: Path) -> int:
    flags = os.O_RDWR | os.O_CREAT | getattr(os, "O_NOFOLLOW", 0)
    descriptor = -1
    try:
        descriptor = os.open(lock_path, flags, 0o600)
        opened = os.fstat(descriptor)
        named = lock_path.lstat()
        if (
            not stat.S_ISREG(opened.st_mode)
            or stat.S_ISLNK(named.st_mode)
            or (opened.st_dev, opened.st_ino) != (named.st_dev, named.st_ino)
            or opened.st_uid != os.getuid()
            or opened.st_mode & 0o077
        ):
            os.close(descriptor)
            _fail("canary lease file is not private and safe")
        try:
            fcntl.flock(descriptor, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError:
            os.close(descriptor)
            _fail("functional canary is already running for this evidence file")
        return descriptor
    except CanaryError:
        raise
    except OSError:
        if descriptor >= 0:
            os.close(descriptor)
        _fail("could not acquire functional canary lease")


def _acquire_run_leases(
    manifest: dict[str, object],
    legs: list[dict[str, object]],
    evidence_path: Path,
    candidate_identity: dict[str, object],
) -> list[int]:
    """Lock both the output identity and the reviewed shared-input identity."""
    candidate = manifest["candidate_worker"]
    if not isinstance(candidate, dict):
        _fail("candidate worker identity is unavailable")
    def file_identity(raw_path: object, digest: object) -> dict[str, object]:
        if not isinstance(raw_path, str):
            _fail("canary lease input identity is unavailable")
        try:
            info = Path(raw_path).lstat()
        except OSError:
            _fail("canary lease input identity is unavailable")
        return {"device": info.st_dev, "inode": info.st_ino, "sha256": digest}

    try:
        evidence_parent = evidence_path.parent.lstat()
    except OSError:
        _fail("canary evidence identity is unavailable")
    evidence_identity = {
        "parent_device": evidence_parent.st_dev,
        "parent_inode": evidence_parent.st_ino,
        "name": evidence_path.name,
    }
    resource_identity = {
        "candidate": {
            "device": candidate_identity["device"],
            "inode": candidate_identity["inode"],
            "sha256": candidate_identity["sha256"],
        },
        "legs": [
            {
                "executable": file_identity(leg["executable"], leg["executable_sha256"]),
                "config": file_identity(leg["config_file"], leg["config_sha256"]),
            }
            for leg in legs
        ],
    }
    identities = (
        b"evidence\0" + json.dumps(evidence_identity, sort_keys=True, separators=(",", ":")).encode(),
        b"resources\0" + json.dumps(resource_identity, sort_keys=True, separators=(",", ":")).encode(),
    )
    lease_directory = _private_lease_directory()
    lock_paths = sorted(
        lease_directory / f"{hashlib.sha256(identity).hexdigest()}.lock"
        for identity in identities
    )
    descriptors: list[int] = []
    try:
        for lock_path in lock_paths:
            descriptors.append(_acquire_one_run_lease(lock_path))
        return descriptors
    except Exception:
        for descriptor in descriptors:
            os.close(descriptor)
        raise


def _check_total_deadline(deadline: float) -> None:
    if time.monotonic() >= deadline:
        _fail("functional canary total timeout exceeded")


def _child_environment(name: str, config_file: str, run_id: str, child_token: str) -> dict[str, str]:
    environment: dict[str, str] = {}
    for key in ("HOME", "PATH", "TMPDIR", "LANG", "LC_ALL", "SSH_AUTH_SOCK"):
        value = os.environ.get(key)
        if value:
            environment[key] = value
    environment["SUBROUTER_CANARY_LEG_NAME"] = name
    environment["SUBROUTER_CANARY_LEG_CONFIG_FILE"] = config_file
    environment["SUBROUTER_CANARY_RUN_ID"] = run_id
    environment["SUBROUTER_CANARY_CHILD_TOKEN"] = child_token
    bounded_token = os.environ.get("SUBROUTER_BOUNDED_CALLBACK_TOKEN", "")
    if bounded_token:
        if len(bounded_token) != 64 or any(
            char not in "0123456789abcdef" for char in bounded_token
        ):
            _fail("bounded callback token is invalid")
        environment["SUBROUTER_BOUNDED_CALLBACK_TOKEN"] = bounded_token
    return environment


def _process_snapshot(*, include_environment: bool = False) -> dict[int, _ProcessInfo]:
    # Bind every relation/environment observation to a process generation that
    # existed both before and after ps produced it.  Sampling the relation
    # first and the start identity second can otherwise attach a reused PID's
    # identity to the departed process's parent/group/marker observation.
    try:
        candidates = subprocess.run(
            ["/bin/ps", "-U", str(os.getuid()), "-o", "pid="],
            check=False,
            stdout=subprocess.PIPE,
            stderr=subprocess.DEVNULL,
            timeout=3,
            start_new_session=True,
        )
    except (OSError, subprocess.SubprocessError):
        _fail("could not inspect canary processes")
    if candidates.returncode != 0 or len(candidates.stdout) > 8 * 1024 * 1024:
        _fail("could not inspect canary processes")
    identities_before: dict[int, str] = {}
    for raw_pid in candidates.stdout.splitlines():
        try:
            candidate_pid = int(raw_pid.strip())
        except ValueError:
            continue
        identity = _process_start_identity(candidate_pid)
        if identity is not None:
            identities_before[candidate_pid] = identity

    arguments = [
        "/bin/ps", "-U", str(os.getuid()),
        "-o", "pid=", "-o", "ppid=", "-o", "pgid=", "-o", "lstart=",
    ]
    if include_environment:
        arguments[1:1] = ["eww"]
        arguments.extend(["-o", "command="])
    try:
        result = subprocess.run(
            arguments,
            check=False,
            stdout=subprocess.PIPE,
            stderr=subprocess.DEVNULL,
            timeout=3,
            start_new_session=True,
        )
    except (OSError, subprocess.SubprocessError):
        _fail("could not inspect canary processes")
    if result.returncode != 0 or len(result.stdout) > 32 * 1024 * 1024:
        _fail("could not inspect canary processes")
    processes: dict[int, _ProcessInfo] = {}
    for line in result.stdout.splitlines():
        fields = line.lstrip().split(None, 8)
        if len(fields) < 8:
            continue
        try:
            pid, ppid, pgid = (int(value) for value in fields[:3])
        except ValueError:
            continue
        started = b" ".join(fields[3:8]).decode("ascii", "strict")
        command = fields[8] if len(fields) == 9 else b""
        identity = identities_before.get(pid)
        if identity is None or _process_start_identity(pid, started) != identity:
            continue
        processes[pid] = _ProcessInfo(pid, ppid, pgid, started, command, identity)
    return processes


def _process_start_identity(pid: int, ps_started: str = "") -> str | None:
    """Return a start identity finer than PID lifetime on supported cutover hosts."""
    if sys.platform == "darwin":
        try:
            libproc = ctypes.CDLL("/usr/lib/libproc.dylib", use_errno=True)
            proc_pidinfo = libproc.proc_pidinfo
            proc_pidinfo.argtypes = [ctypes.c_int, ctypes.c_int, ctypes.c_uint64, ctypes.c_void_p, ctypes.c_int]
            proc_pidinfo.restype = ctypes.c_int
            info = _DarwinBSDInfo()
            size = ctypes.sizeof(info)
            if proc_pidinfo(pid, 3, 0, ctypes.byref(info), size) != size:
                return None
            return f"darwin:{info.pbi_start_tvsec}:{info.pbi_start_tvusec}"
        except (AttributeError, OSError):
            return None
    if sys.platform.startswith("linux"):
        try:
            fields = Path(f"/proc/{pid}/stat").read_text().rsplit(")", 1)[1].split()
            return f"linux:{fields[19]}"
        except (IndexError, OSError):
            return None
    # The runner is deployed on macOS; this fallback keeps validation usable on
    # other POSIX development hosts without weakening Darwin signal decisions.
    return f"ps:{ps_started}" if ps_started else None


def _capture_identity(process: _ProcessInfo) -> str | None:
    return process.identity


def _process_group_identities(
    pgid: int,
    snapshot: dict[int, _ProcessInfo] | None = None,
    *,
    leader_identity: str | None = None,
) -> dict[int, str]:
    if snapshot is None:
        snapshot = _process_snapshot()
    if leader_identity is not None:
        leader = snapshot.get(pgid)
        # A numeric PGID is reusable after its leader exits. Only inspect the
        # group while its original leader is still the exact process generation
        # that created it. Detached descendants remain covered by their tracked
        # identities and inherited marker below.
        if (
            leader is None
            or leader.pid != pgid
            or leader.pgid != pgid
            or leader.identity != leader_identity
        ):
            return {}
    identities: dict[int, str] = {}
    anchor_raw = os.environ.get("SUBROUTER_BOUNDED_GROUP_ANCHOR_PID", "")
    anchor_pid = int(anchor_raw) if anchor_raw.isdigit() else -1
    for pid, process in snapshot.items():
        if process.pgid != pgid or pid in (os.getpid(), anchor_pid):
            continue
        identity = _capture_identity(process)
        if identity is not None:
            identities[pid] = identity
    return identities


def _process_group_exists(
    pgid: int,
    *,
    leader_identity: str | None = None,
    snapshot: dict[int, _ProcessInfo] | None = None,
) -> bool:
    return bool(_process_group_identities(pgid, snapshot, leader_identity=leader_identity))


def _refresh_tracked_descendants(root_pid: int) -> None:
    snapshot = _process_snapshot()
    pending = [root_pid] if (
        root_pid in snapshot
        and _tracked_child_identities.get(root_pid) == snapshot[root_pid].identity
    ) else []
    pending.extend(
        pid
        for pid, identity in _tracked_child_identities.items()
        if pid != root_pid and pid in snapshot and snapshot[pid].identity == identity
    )
    visited: set[int] = set()
    while pending:
        parent = pending.pop()
        if parent in visited:
            continue
        visited.add(parent)
        process = snapshot.get(parent)
        if process is not None and parent != os.getpid():
            _tracked_child_identities[parent] = process.identity
        for child in snapshot.values():
            if child.ppid == parent and child.pid != os.getpid():
                _tracked_child_identities[child.pid] = child.identity
                pending.append(child.pid)


def _identity_exists(pid: int, started: str, snapshot: dict[int, _ProcessInfo] | None = None) -> bool:
    current = (snapshot or _process_snapshot()).get(pid)
    return current is not None and _capture_identity(current) == started


def _signal_process_identity(pid: int, started: str, sent_signal: signal.Signals) -> bool:
    """Signal one exact process generation, never a numerically reused PID."""
    if _process_start_identity(pid) != started:
        return False
    if sys.platform == "darwin":
        libsystem = ctypes.CDLL("/usr/lib/libSystem.B.dylib", use_errno=True)
        libproc = ctypes.CDLL("/usr/lib/libproc.dylib", use_errno=True)
        self_task = ctypes.c_uint.in_dll(libsystem, "mach_task_self_").value
        task = ctypes.c_uint()
        task_name_for_pid = libsystem.task_name_for_pid
        task_name_for_pid.argtypes = [ctypes.c_uint, ctypes.c_int, ctypes.POINTER(ctypes.c_uint)]
        task_name_for_pid.restype = ctypes.c_int
        if task_name_for_pid(self_task, pid, ctypes.byref(task)) != 0:
            if _process_start_identity(pid) != started:
                return False
            raise CanaryError("could not bind canary child process identity")
        try:
            token = _DarwinAuditToken()
            count = ctypes.c_uint32(8)
            task_info = libsystem.task_info
            task_info.argtypes = [
                ctypes.c_uint, ctypes.c_int,
                ctypes.POINTER(ctypes.c_uint32), ctypes.POINTER(ctypes.c_uint32),
            ]
            task_info.restype = ctypes.c_int
            if task_info(
                task.value,
                15,  # TASK_AUDIT_TOKEN
                ctypes.cast(ctypes.byref(token), ctypes.POINTER(ctypes.c_uint32)),
                ctypes.byref(count),
            ) != 0 or count.value != 8:
                raise CanaryError("could not read canary child process identity token")
        finally:
            libsystem.mach_port_deallocate(self_task, task.value)
        if token.val[5] != pid or _process_start_identity(pid) != started:
            return False
        proc_signal = libproc.proc_signal_with_audittoken
        proc_signal.argtypes = [ctypes.POINTER(_DarwinAuditToken), ctypes.c_int]
        proc_signal.restype = ctypes.c_int
        if proc_signal(ctypes.byref(token), int(sent_signal)) != 0:
            error = ctypes.get_errno()
            if error == errno.ESRCH:
                return False
            raise OSError(error, "identity-bound canary child signal failed")
        return True
    try:
        os.kill(pid, sent_signal)
        return True
    except ProcessLookupError:
        return False


def _marked_child_identities(child_token: str) -> dict[int, str]:
    """Find surviving descendants by their per-leg inherited environment marker.

    Process-group and ancestry inspection alone cannot observe a child that forks,
    creates a new session, and lets its leader exit between samples.  Legitimate
    canary descendants inherit this unguessable marker, so a post-exit scan closes
    that race without relying on parentage.  The command text is never emitted.
    """
    snapshot = _process_snapshot(include_environment=True)
    marker = b"SUBROUTER_CANARY_CHILD_TOKEN=" + child_token.encode("ascii")
    identities: dict[int, str] = {}
    for pid, process in snapshot.items():
        if pid == os.getpid() or marker not in process.command:
            continue
        identity = _capture_identity(process)
        if identity is not None:
            identities[pid] = identity
    return identities


def _signal_matching_identities(identities: dict[int, str], sent_signal: signal.Signals) -> None:
    """Signal only PIDs whose kernel-visible start identity still matches."""
    for pid, started in identities.items():
        _signal_process_identity(pid, started, sent_signal)


def _terminate_child(
    child: subprocess.Popen[bytes],
    child_token: str,
    *,
    child_pgid: int,
    child_identity: str,
) -> None:
    _refresh_tracked_descendants(child.pid)
    members = dict(_tracked_child_identities)
    members.update(_process_group_identities(child_pgid, leader_identity=child_identity))
    members.update(_marked_child_identities(child_token))
    _signal_matching_identities(members, signal.SIGTERM)
    deadline = time.monotonic() + 2
    while time.monotonic() < deadline:
        snapshot = _process_snapshot()
        if not any(_identity_exists(pid, started, snapshot) for pid, started in members.items()):
            break
        _refresh_tracked_descendants(child.pid)
        members.update(_tracked_child_identities)
        members.update(_marked_child_identities(child_token))
        time.sleep(0.02)
    members.update(_process_group_identities(child_pgid, leader_identity=child_identity))
    _signal_matching_identities(members, signal.SIGKILL)
    if child.poll() is None:
        child.wait()
    _tracked_child_identities.clear()


def _signal_handler(signum: int, _frame: object) -> None:
    if (
        _active_child is not None
        and _active_child_token is not None
        and _active_child_pgid is not None
        and _active_child_identity is not None
    ):
        _terminate_child(
            _active_child,
            _active_child_token,
            child_pgid=_active_child_pgid,
            child_identity=_active_child_identity,
        )
    raise CanarySignal(f"interrupted by signal {signum}")


def _run_leg(leg: dict[str, object], run_id: str, total_deadline: float) -> dict[str, object]:
    global _active_child, _active_child_token, _active_child_pgid, _active_child_identity
    _check_total_deadline(total_deadline)
    name = str(leg["name"])
    executable = _secure_file(leg["executable"], f"leg {name} executable", executable=True)
    config = _secure_file(leg["config_file"], f"leg {name} config")
    started = time.monotonic()
    with tempfile.TemporaryDirectory(prefix="subrouter-canary-pinned-") as pinned_root:
        pinned_dir = Path(pinned_root)
        pinned_executable = _copy_pinned_input(
            executable, pinned_dir / "leg", str(leg["executable_sha256"]), executable=True
        )
        _check_total_deadline(total_deadline)
        pinned_config = _copy_pinned_input(
            config, pinned_dir / "config.json", str(leg["config_sha256"]), executable=False
        )
        _check_total_deadline(total_deadline)
        with tempfile.TemporaryFile() as stdout_file, tempfile.TemporaryFile() as stderr_file:
            child_token = secrets.token_hex(32)
            child = subprocess.Popen(
                [str(pinned_executable)],
                stdin=subprocess.DEVNULL,
                stdout=stdout_file,
                stderr=stderr_file,
                env=_child_environment(name, str(pinned_config), run_id, child_token),
                start_new_session=True,
            )
            _tracked_child_identities.clear()
            child_identity = _process_start_identity(child.pid)
            if child_identity is None:
                child.kill()
                child.wait()
                _fail(f"leg {name} process identity unavailable")
            try:
                child_pgid = os.getpgid(child.pid)
            except ProcessLookupError:
                # The Popen handle still owns this unreaped process. If it
                # exited before inspection, its detached descendants remain
                # discoverable through the start identity and marker paths.
                child_pgid = child.pid
            if child_pgid != child.pid:
                _signal_matching_identities({child.pid: child_identity}, signal.SIGKILL)
                child.wait()
                _fail(f"leg {name} process did not create an isolated process group")
            _tracked_child_identities[child.pid] = child_identity
            _active_child = child
            _active_child_token = child_token
            _active_child_pgid = child_pgid
            _active_child_identity = child_identity
            timed_out = False
            total_timed_out = False
            oversized = False
            try:
                deadline = min(total_deadline, time.monotonic() + int(leg["timeout_seconds"]))
                while child.poll() is None:
                    _refresh_tracked_descendants(child.pid)
                    if os.fstat(stdout_file.fileno()).st_size + os.fstat(stderr_file.fileno()).st_size > MAX_LEG_OUTPUT:
                        oversized = True
                        _terminate_child(
                            child,
                            child_token,
                            child_pgid=child_pgid,
                            child_identity=child_identity,
                        )
                        break
                    if time.monotonic() >= deadline:
                        timed_out = True
                        total_timed_out = time.monotonic() >= total_deadline
                        _terminate_child(
                            child,
                            child_token,
                            child_pgid=child_pgid,
                            child_identity=child_identity,
                        )
                        break
                    time.sleep(0.02)
                if child.poll() is None:
                    timed_out = True
                    total_timed_out = time.monotonic() >= total_deadline
                    _terminate_child(
                        child,
                        child_token,
                        child_pgid=child_pgid,
                        child_identity=child_identity,
                    )
            finally:
                try:
                    _refresh_tracked_descendants(child.pid)
                    marked_descendants = _marked_child_identities(child_token)
                    snapshot = _process_snapshot()
                    descendants_remained = (
                        _process_group_exists(
                            child_pgid,
                            leader_identity=child_identity,
                            snapshot=snapshot,
                        )
                        or any(
                            _identity_exists(pid, started, snapshot)
                            for pid, started in _tracked_child_identities.items()
                        )
                        or bool(marked_descendants)
                    )
                    if descendants_remained:
                        _tracked_child_identities.update(marked_descendants)
                        _terminate_child(
                            child,
                            child_token,
                            child_pgid=child_pgid,
                            child_identity=child_identity,
                        )
                    else:
                        _tracked_child_identities.clear()
                finally:
                    _active_child = None
                    _active_child_token = None
                    _active_child_pgid = None
                    _active_child_identity = None
            if timed_out:
                if total_timed_out:
                    _fail("functional canary total timeout exceeded")
                _fail(f"leg {name} timed out")
            if oversized:
                _fail(f"leg {name} returned oversized output")
            if descendants_remained:
                _fail(f"leg {name} left descendant processes")
            stdout_size = os.fstat(stdout_file.fileno()).st_size
            stderr_size = os.fstat(stderr_file.fileno()).st_size
            if stdout_size + stderr_size > MAX_LEG_OUTPUT:
                _fail(f"leg {name} returned oversized output")
            stdout_file.seek(0)
            stdout = stdout_file.read(MAX_LEG_OUTPUT + 1)
    _check_total_deadline(total_deadline)
    if child.returncode != 0:
        _fail(f"leg {name} failed")
    try:
        result = json.loads(stdout, object_pairs_hook=lambda pairs: _strict_unique_pairs(pairs, name))
    except (UnicodeDecodeError, json.JSONDecodeError):
        _fail(f"leg {name} returned malformed evidence")
    result = _strict_keys(result, {"schema", "leg", "ok"}, f"leg {name} evidence")
    if result.get("schema") != LEG_SCHEMA or result.get("leg") != name or result.get("ok") is not True:
        _fail(f"leg {name} did not return its exact success record")
    return {
        "name": name,
        "ok": True,
        "duration_ms": round((time.monotonic() - started) * 1000),
        "executable_sha256": leg["executable_sha256"],
        "config_sha256": leg["config_sha256"],
    }


def _copy_pinned_input(source: Path, destination: Path, expected_hash: str, *, executable: bool) -> Path:
    flags = os.O_RDONLY | getattr(os, "O_NOFOLLOW", 0)
    source_fd = os.open(source, flags)
    output_fd = -1
    digest = hashlib.sha256()
    try:
        opened = os.fstat(source_fd)
        named = source.lstat()
        if (opened.st_dev, opened.st_ino) != (named.st_dev, named.st_ino):
            _fail("canary input identity changed while pinning")
        output_fd = os.open(destination, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o700 if executable else 0o600)
        while True:
            chunk = os.read(source_fd, 1024 * 1024)
            if not chunk:
                break
            digest.update(chunk)
            view = memoryview(chunk)
            while view:
                written = os.write(output_fd, view)
                view = view[written:]
        os.fsync(output_fd)
    except OSError:
        _fail("could not pin canary input")
    finally:
        os.close(source_fd)
        if output_fd >= 0:
            os.close(output_fd)
    if digest.hexdigest() != expected_hash:
        _fail("canary input changed after validation")
    return destination


def _ensure_isolated_process_group() -> None:
    anchor_raw = os.environ.get("SUBROUTER_BOUNDED_GROUP_ANCHOR_PID", "")
    if anchor_raw:
        if not anchor_raw.isdigit() or int(anchor_raw) <= 1 or os.getpgrp() != int(anchor_raw):
            _fail("bounded canary process group identity is invalid")
        return
    if os.getpgrp() != os.getpid():
        try:
            os.setsid()
        except OSError:
            _fail("could not isolate canary process group")


def _strict_unique_pairs(pairs: list[tuple[str, object]], leg_name: str) -> dict[str, object]:
    value: dict[str, object] = {}
    for key, item in pairs:
        if key in value:
            _fail(f"leg {leg_name} evidence contains a duplicate field")
        value[key] = item
    return value


def _write_evidence(
    path: Path, evidence: dict[str, object], expected_previous: bytes | None
) -> bytes:
    body = (json.dumps(evidence, sort_keys=True, separators=(",", ":")) + "\n").encode()
    descriptor, temporary = tempfile.mkstemp(prefix=f".{path.name}.", dir=path.parent)
    try:
        os.fchmod(descriptor, 0o600)
        with os.fdopen(descriptor, "wb", closefd=True) as output:
            output.write(body)
            output.flush()
            os.fsync(output.fileno())
        current = _recognized_prior_evidence(path)
        if current != expected_previous:
            _fail("evidence file changed before publication")
        os.replace(temporary, path)
        directory = os.open(path.parent, os.O_RDONLY)
        try:
            os.fsync(directory)
        finally:
            os.close(directory)
    finally:
        try:
            os.unlink(temporary)
        except FileNotFoundError:
            pass
    return body


def main() -> int:
    parser = argparse.ArgumentParser(description="Run strict Subrouter functional cutover canaries")
    parser.add_argument("--manifest", help="absolute private manifest path; defaults to SUBROUTER_CANARY_MANIFEST_FILE")
    parser.add_argument("--validate-only", action="store_true", help="validate inputs without running legs")
    arguments = parser.parse_args()
    manifest_raw = arguments.manifest or os.environ.get("SUBROUTER_CANARY_MANIFEST_FILE", "")
    manifest_path = _secure_file(manifest_raw, "canary manifest")
    manifest, legs, manifest_hash = _load_manifest(manifest_path)
    worker_descriptor, candidate_identity = _open_transaction_worker_binding(manifest)
    if arguments.validate_only:
        os.close(worker_descriptor)
        print("functional-canary: manifest valid")
        return 0

    evidence_path = Path(str(manifest["evidence_file"]))
    previous_evidence = _recognized_prior_evidence(evidence_path)
    try:
        _verify_transaction_worker_binding(worker_descriptor, candidate_identity)
        leases = _acquire_run_leases(manifest, legs, evidence_path, candidate_identity)
    except Exception:
        os.close(worker_descriptor)
        raise
    run_id = hashlib.sha256(os.urandom(32)).hexdigest()
    total_deadline = time.monotonic() + int(manifest["total_timeout_seconds"])
    evidence: dict[str, object] = {
        "schema": SCHEMA,
        "source_git_oid_unverified": manifest["source_git_oid_unverified"],
        "manifest_sha256": manifest_hash,
        "candidate_worker_sha256": manifest["candidate_worker"]["sha256"],  # type: ignore[index]
        "run_id": run_id,
        "started_at_epoch": int(time.time()),
        "status": "running",
        "legs": [],
    }
    try:
        _check_total_deadline(total_deadline)
        previous_evidence = _write_evidence(evidence_path, evidence, previous_evidence)
        _check_total_deadline(total_deadline)
        for leg in legs:
            _verify_transaction_worker_binding(worker_descriptor, candidate_identity)
            result = _run_leg(leg, run_id, total_deadline)
            evidence["legs"].append(result)  # type: ignore[union-attr]
            _check_total_deadline(total_deadline)
            previous_evidence = _write_evidence(evidence_path, evidence, previous_evidence)
            _check_total_deadline(total_deadline)
            print(f"functional-canary: {result['name']} passed")
        evidence["status"] = "passed"
        evidence["completed_at_epoch"] = int(time.time())
        _check_total_deadline(total_deadline)
        previous_evidence = _write_evidence(evidence_path, evidence, previous_evidence)
        _check_total_deadline(total_deadline)
        print("functional-canary: all required legs passed")
        return 0
    except Exception as error:
        safe_error = error if isinstance(error, CanaryError) else CanaryError("internal runner failure")
        evidence["status"] = "failed"
        evidence["failure"] = str(safe_error)
        evidence["completed_at_epoch"] = int(time.time())
        previous_evidence = _write_evidence(evidence_path, evidence, previous_evidence)
        raise safe_error
    finally:
        for lease in leases:
            os.close(lease)
        os.close(worker_descriptor)


if __name__ == "__main__":
    try:
        _ensure_isolated_process_group()
        for caught_signal in (signal.SIGINT, signal.SIGTERM, signal.SIGHUP):
            signal.signal(caught_signal, _signal_handler)
        raise SystemExit(main())
    except CanaryError as error:
        print(f"functional-canary: {error}", file=sys.stderr)
        raise SystemExit(1)
    except Exception:
        print("functional-canary: internal runner failure", file=sys.stderr)
        raise SystemExit(1)
