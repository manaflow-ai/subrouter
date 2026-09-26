#!/usr/bin/env python3
"""Behavior tests for the release package manifest boundary."""

from __future__ import annotations

import json
import sys
import tarfile
import tempfile
import unittest
import zipfile
from io import BytesIO
from pathlib import Path

import release_package


TAG = "v1.2.3"
VERSION = "1.2.3"
COMMIT = "0123456789abcdef0123456789abcdef01234567"


def write_npm_archive(path: Path) -> None:
    payload = json.dumps({"name": "subrouter", "version": VERSION}, separators=(",", ":")).encode()
    info = tarfile.TarInfo("package/package.json")
    info.size = len(payload)
    with tarfile.open(path, "w:gz") as archive:
        archive.addfile(info, BytesIO(payload))


def write_source_archive(path: Path) -> None:
    payload = f"Metadata-Version: 2.1\nName: subrouter\nVersion: {VERSION}\n".encode()
    info = tarfile.TarInfo(f"subrouter-{VERSION}/PKG-INFO")
    info.size = len(payload)
    with tarfile.open(path, "w:gz") as archive:
        archive.addfile(info, BytesIO(payload))


def write_wheel(path: Path) -> None:
    metadata = f"Metadata-Version: 2.1\nName: subrouter\nVersion: {VERSION}\n".encode()
    with zipfile.ZipFile(path, "w", compression=zipfile.ZIP_DEFLATED) as archive:
        archive.writestr(f"subrouter-{VERSION}.dist-info/METADATA", metadata)


class ReleasePackageTests(unittest.TestCase):
    def setUp(self) -> None:
        self.tempdir = tempfile.TemporaryDirectory()
        self.root = Path(self.tempdir.name)
        (self.root / "npm").mkdir()
        (self.root / "python").mkdir()
        write_npm_archive(self.root / "npm" / f"subrouter-{VERSION}.tgz")
        write_wheel(self.root / "python" / f"subrouter-{VERSION}-py3-none-any.whl")
        write_source_archive(self.root / "python" / f"subrouter-{VERSION}.tar.gz")
        manifest = release_package.manifest_for(self.root, TAG, COMMIT, VERSION)
        release_package.write_manifest(self.root, manifest)

    def tearDown(self) -> None:
        self.tempdir.cleanup()

    def test_valid_bundle_round_trips(self) -> None:
        release_package.validate_manifest(self.root, TAG, COMMIT, VERSION)

    def test_modified_package_is_rejected(self) -> None:
        with (self.root / "npm" / f"subrouter-{VERSION}.tgz").open("ab") as stream:
            stream.write(b"tampered")
        with self.assertRaises(release_package.ManifestError):
            release_package.validate_manifest(self.root, TAG, COMMIT, VERSION)

    def test_extra_file_is_rejected(self) -> None:
        (self.root / "python" / "unexpected.txt").write_text("unexpected", encoding="utf-8")
        with self.assertRaises(release_package.ManifestError):
            release_package.validate_manifest(self.root, TAG, COMMIT, VERSION)

    def test_identity_mismatch_is_rejected(self) -> None:
        with self.assertRaises(release_package.ManifestError):
            release_package.validate_manifest(self.root, TAG, COMMIT, "1.2.4")

    def test_duplicate_manifest_key_is_rejected(self) -> None:
        (self.root / release_package.MANIFEST_NAME).write_text(
            '{"schema":"subrouter.release-packages/v1","schema":"duplicate"}\n', encoding="utf-8"
        )
        with self.assertRaises(release_package.ManifestError):
            release_package.validate_manifest(self.root, TAG, COMMIT, VERSION)

    def test_create_command_writes_deterministic_manifest(self) -> None:
        first = (self.root / release_package.MANIFEST_NAME).read_bytes()
        manifest = release_package.manifest_for(self.root, TAG, COMMIT, VERSION)
        release_package.write_manifest(self.root, manifest)
        self.assertEqual(first, (self.root / release_package.MANIFEST_NAME).read_bytes())


if __name__ == "__main__":
    unittest.main(verbosity=2)
