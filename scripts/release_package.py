#!/usr/bin/env python3
"""Create and verify the package bundle passed between release jobs.

The bundle carries a small, deterministic manifest. Publishers verify both the
manifest and the package metadata before they receive an OIDC token. No
network access is needed for verification.
"""

from __future__ import annotations

import argparse
import hashlib
import json
import os
import re
import sys
import tarfile
import tempfile
import zipfile
from pathlib import Path
from typing import Any, Iterable, Mapping, NoReturn


MANIFEST_NAME = "PACKAGE_PROVENANCE.json"
SCHEMA = "subrouter.release-packages/v1"
SHA_RE = re.compile(r"^[0-9a-f]{40}$")
VERSION_RE = re.compile(r"^[0-9]+\.[0-9]+\.[0-9]+(?:[.-][0-9A-Za-z.-]+)?$")


class ManifestError(ValueError):
    """Raised when a release package bundle is not self-consistent."""


def fail(message: str) -> NoReturn:
    raise ManifestError(message)


def _reject_duplicate_keys(pairs: list[tuple[str, Any]]) -> dict[str, Any]:
    result: dict[str, Any] = {}
    for key, value in pairs:
        if key in result:
            fail(f"duplicate JSON key: {key}")
        result[key] = value
    return result


def load_json(path: Path) -> Any:
    try:
        with path.open("r", encoding="utf-8") as stream:
            return json.load(stream, object_pairs_hook=_reject_duplicate_keys)
    except (OSError, UnicodeDecodeError, json.JSONDecodeError) as error:
        fail(f"cannot read JSON {path}: {error}")


def sha256(path: Path) -> tuple[str, int]:
    digest = hashlib.sha256()
    size = 0
    try:
        with path.open("rb") as stream:
            for chunk in iter(lambda: stream.read(1024 * 1024), b""):
                digest.update(chunk)
                size += len(chunk)
    except OSError as error:
        fail(f"cannot read package file {path}: {error}")
    return digest.hexdigest(), size


def package_files(root: Path) -> list[Path]:
    if not root.is_dir():
        fail(f"package directory is missing: {root}")
    files: list[Path] = []
    for path in sorted(root.rglob("*")):
        if path.name == MANIFEST_NAME and path.parent == root:
            continue
        if path.is_symlink():
            fail(f"package bundle contains a symlink: {path}")
        if path.is_file():
            relative = path.relative_to(root)
            if any(part in ("", ".", "..") for part in relative.parts):
                fail(f"invalid package path: {relative}")
            files.append(path)
    if not files:
        fail("package bundle is empty")
    return files


def validate_identity(tag: str, source_commit: str, version: str) -> None:
    if not re.fullmatch(r"v[0-9]+\.[0-9]+\.[0-9]+(?:[.-][0-9A-Za-z.-]+)?", tag):
        fail(f"invalid release tag: {tag}")
    if not SHA_RE.fullmatch(source_commit):
        fail("source commit must be a full lowercase SHA")
    if not VERSION_RE.fullmatch(version):
        fail(f"invalid package version: {version}")
    if tag[1:] != version:
        fail(f"tag {tag} does not match package version {version}")


def relative_name(path: Path, root: Path) -> str:
    value = path.relative_to(root).as_posix()
    if value.startswith("/") or ".." in Path(value).parts:
        fail(f"invalid package path: {value}")
    return value


def manifest_for(root: Path, tag: str, source_commit: str, version: str) -> dict[str, Any]:
    validate_identity(tag, source_commit, version)
    files = package_files(root)
    entries = []
    for path in files:
        digest, size = sha256(path)
        entries.append({"name": relative_name(path, root), "sha256": digest, "size": size})
    entries.sort(key=lambda entry: entry["name"])
    return {
        "schema": SCHEMA,
        "tag": tag,
        "source_commit": source_commit,
        "version": version,
        "files": entries,
    }


def write_manifest(root: Path, manifest: Mapping[str, Any]) -> None:
    destination = root / MANIFEST_NAME
    try:
        with tempfile.NamedTemporaryFile(
            "w", encoding="utf-8", dir=root, prefix=f".{MANIFEST_NAME}.", delete=False
        ) as stream:
            json.dump(manifest, stream, sort_keys=True, separators=(",", ":"))
            stream.write("\n")
            temporary = Path(stream.name)
        os.replace(temporary, destination)
    except OSError as error:
        try:
            temporary.unlink(missing_ok=True)
        except (NameError, OSError):
            pass
        fail(f"cannot write package manifest: {error}")


def _archive_member_names(names: Iterable[str]) -> None:
    for name in names:
        path = Path(name)
        if path.is_absolute() or ".." in path.parts:
            fail(f"archive contains an unsafe path: {name}")


def _metadata_version(lines: Iterable[str], archive: Path) -> tuple[str | None, str | None]:
    name: str | None = None
    version: str | None = None
    for line in lines:
        key, separator, value = line.partition(":")
        if not separator:
            continue
        if key.lower() == "name":
            name = value.strip()
        elif key.lower() == "version":
            version = value.strip()
    if name is None or version is None:
        fail(f"package metadata is incomplete in {archive}")
    return name, version


def verify_npm(path: Path, version: str) -> None:
    try:
        with tarfile.open(path, mode="r:gz") as archive:
            _archive_member_names(member.name for member in archive.getmembers())
            members = [member for member in archive.getmembers() if member.name == "package/package.json"]
            if len(members) != 1 or not members[0].isfile():
                fail(f"npm archive has no unique package/package.json: {path}")
            stream = archive.extractfile(members[0])
            if stream is None:
                fail(f"cannot read npm metadata: {path}")
            data = json.loads(stream.read(1024 * 1024), object_pairs_hook=_reject_duplicate_keys)
    except (OSError, tarfile.TarError, UnicodeDecodeError, json.JSONDecodeError) as error:
        fail(f"invalid npm archive {path}: {error}")
    if not isinstance(data, dict) or data.get("name") != "subrouter" or data.get("version") != version:
        fail(f"npm metadata does not match subrouter {version}: {path}")


def verify_python_archive(path: Path, version: str) -> None:
    if path.suffix == ".whl":
        try:
            with zipfile.ZipFile(path) as archive:
                _archive_member_names(archive.namelist())
                metadata = [name for name in archive.namelist() if name.endswith(".dist-info/METADATA")]
                if len(metadata) != 1:
                    fail(f"wheel has no unique dist-info/METADATA: {path}")
                lines = archive.read(metadata[0]).decode("utf-8").splitlines()
        except (OSError, zipfile.BadZipFile, UnicodeDecodeError) as error:
            fail(f"invalid wheel {path}: {error}")
    else:
        try:
            with tarfile.open(path, mode="r:gz") as archive:
                _archive_member_names(member.name for member in archive.getmembers())
                metadata = [member for member in archive.getmembers() if member.name.endswith("/PKG-INFO")]
                if len(metadata) != 1 or not metadata[0].isfile():
                    fail(f"source archive has no unique PKG-INFO: {path}")
                stream = archive.extractfile(metadata[0])
                if stream is None:
                    fail(f"cannot read source metadata: {path}")
                lines = stream.read(1024 * 1024).decode("utf-8").splitlines()
        except (OSError, tarfile.TarError, UnicodeDecodeError) as error:
            fail(f"invalid source archive {path}: {error}")
    name, actual_version = _metadata_version(lines, path)
    if name != "subrouter" or actual_version != version:
        fail(f"Python metadata does not match subrouter {version}: {path}")


def validate_manifest(root: Path, expected_tag: str, expected_commit: str, expected_version: str) -> None:
    validate_identity(expected_tag, expected_commit, expected_version)
    manifest_path = root / MANIFEST_NAME
    value = load_json(manifest_path)
    if not isinstance(value, dict):
        fail("package manifest must be an object")
    required = {"schema", "tag", "source_commit", "version", "files"}
    if set(value) != required:
        fail(f"package manifest keys must be exactly {sorted(required)}")
    if value["schema"] != SCHEMA:
        fail("unsupported package manifest schema")
    if value["tag"] != expected_tag or value["source_commit"] != expected_commit or value["version"] != expected_version:
        fail("package manifest identity does not match the release context")
    entries = value["files"]
    if not isinstance(entries, list) or not entries:
        fail("package manifest files must be a non-empty list")

    expected: dict[str, tuple[str, int]] = {}
    for entry in entries:
        if not isinstance(entry, dict) or set(entry) != {"name", "sha256", "size"}:
            fail("package manifest contains a malformed file entry")
        name = entry["name"]
        digest = entry["sha256"]
        size = entry["size"]
        if not isinstance(name, str) or not name or Path(name).is_absolute() or ".." in Path(name).parts:
            fail(f"package manifest contains an unsafe path: {name}")
        if not (name.startswith("npm/") or name.startswith("python/")):
            fail(f"package manifest contains an unexpected path: {name}")
        if name in expected:
            fail(f"package manifest contains duplicate file: {name}")
        if not isinstance(digest, str) or not re.fullmatch(r"[0-9a-f]{64}", digest):
            fail(f"invalid SHA-256 for {name}")
        if not isinstance(size, int) or isinstance(size, bool) or size < 0:
            fail(f"invalid size for {name}")
        expected[name] = (digest, size)

    actual_paths = {relative_name(path, root): path for path in package_files(root)}
    if set(expected) != set(actual_paths):
        missing = sorted(set(expected) - set(actual_paths))
        extra = sorted(set(actual_paths) - set(expected))
        fail(f"package file set changed, missing={missing}, extra={extra}")
    for name, path in actual_paths.items():
        actual_digest, actual_size = sha256(path)
        expected_digest, expected_size = expected[name]
        if (actual_digest, actual_size) != (expected_digest, expected_size):
            fail(f"package digest or size mismatch: {name}")

    npm_files = [path for name, path in actual_paths.items() if name.startswith("npm/") and name.endswith(".tgz")]
    wheel_files = [path for name, path in actual_paths.items() if name.startswith("python/") and name.endswith(".whl")]
    source_files = [path for name, path in actual_paths.items() if name.startswith("python/") and name.endswith(".tar.gz")]
    if len(npm_files) != 1 or len(wheel_files) != 1 or len(source_files) != 1:
        fail("package bundle must contain exactly one npm tarball, wheel, and source archive")
    verify_npm(npm_files[0], expected_version)
    verify_python_archive(wheel_files[0], expected_version)
    verify_python_archive(source_files[0], expected_version)


def command_create(args: argparse.Namespace) -> None:
    root = Path(args.directory).resolve()
    manifest = manifest_for(root, args.tag, args.source_commit, args.version)
    write_manifest(root, manifest)
    validate_manifest(root, args.tag, args.source_commit, args.version)


def command_verify(args: argparse.Namespace) -> None:
    validate_manifest(Path(args.directory).resolve(), args.tag, args.source_commit, args.version)


def parser() -> argparse.ArgumentParser:
    result = argparse.ArgumentParser(description=__doc__)
    subparsers = result.add_subparsers(dest="command", required=True)
    for command, handler in (("create", command_create), ("verify", command_verify)):
        subparser = subparsers.add_parser(command)
        subparser.add_argument("--tag", required=True)
        subparser.add_argument("--source-commit", required=True)
        subparser.add_argument("--version", required=True)
        subparser.add_argument("--directory", required=True)
        subparser.set_defaults(handler=handler)
    return result


def main(argv: list[str] | None = None) -> int:
    try:
        args = parser().parse_args(argv)
        args.handler(args)
    except ManifestError as error:
        print(f"release package check: {error}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
