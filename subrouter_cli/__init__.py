from __future__ import annotations

import hashlib
import os
import platform
import stat
import subprocess
import sys
import urllib.request
from pathlib import Path

VERSION = "0.1.51"


def _release_platform() -> str:
    system = platform.system().lower()
    mapping = {
        "darwin": "darwin",
        "linux": "linux",
        "windows": "windows",
    }
    if system not in mapping:
        raise SystemExit(f"Unsupported platform: {system}")
    return mapping[system]


def _release_arch() -> str:
    machine = platform.machine().lower()
    mapping = {
        "x86_64": "amd64",
        "amd64": "amd64",
        "aarch64": "arm64",
        "arm64": "arm64",
    }
    if machine not in mapping:
        raise SystemExit(f"Unsupported architecture: {machine}")
    return mapping[machine]


def _download(url: str) -> bytes:
    with urllib.request.urlopen(url, timeout=60) as response:
        return response.read()


def _verify_checksum(binary: Path, asset_name: str, checksum_body: bytes) -> None:
    expected = None
    for line in checksum_body.decode("utf-8").splitlines():
        parts = line.strip().split()
        if len(parts) >= 2 and parts[1] == asset_name:
            expected = parts[0]
            break
    if expected is None:
        raise SystemExit(f"Missing checksum for {asset_name}")
    actual = hashlib.sha256(binary.read_bytes()).hexdigest()
    if actual != expected:
        raise SystemExit(f"Checksum mismatch for {asset_name}")


def _binary() -> Path:
    override = os.environ.get("SUBROUTER_BIN")
    if override:
        return Path(override)

    release_platform = _release_platform()
    release_arch = _release_arch()
    suffix = ".exe" if release_platform == "windows" else ""
    asset_name = f"subrouter_{VERSION}_{release_platform}_{release_arch}{suffix}"
    cache_root = Path(os.environ.get("SUBROUTER_INSTALL_DIR", Path.home() / ".cache" / "subrouter"))
    binary = cache_root / VERSION / asset_name
    if binary.exists():
        return binary

    binary.parent.mkdir(parents=True, exist_ok=True)
    base_url = os.environ.get(
        "SUBROUTER_DOWNLOAD_BASE",
        f"https://github.com/manaflow-ai/subrouter/releases/download/v{VERSION}",
    )
    tmp = binary.with_name(f"{binary.name}.{os.getpid()}.tmp")
    tmp.write_bytes(_download(f"{base_url}/{asset_name}"))
    try:
        _verify_checksum(tmp, asset_name, _download(f"{base_url}/SHA256SUMS"))
    except BaseException:
        tmp.unlink(missing_ok=True)
        raise
    tmp.chmod(tmp.stat().st_mode | stat.S_IXUSR | stat.S_IXGRP | stat.S_IXOTH)
    tmp.replace(binary)
    return binary


def _run(command_name: str) -> None:
    os.execv(_binary(), [command_name, *sys.argv[1:]])


def subrouter() -> None:
    _run("subrouter")


def sr() -> None:
    _run("sr")


def cx() -> None:
    _run("cx")
