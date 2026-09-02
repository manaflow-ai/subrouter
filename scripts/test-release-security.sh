#!/usr/bin/env bash
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
workflow="${1:-${root}/.github/workflows/release.yml}"

fail() {
  echo "release security check: $*" >&2
  exit 1
}

[[ -f "${workflow}" ]] || fail "workflow is missing: ${workflow}"

# Keep structural assertions in Python so indentation and quoted YAML do not
# make the checks depend on fragile grep ranges.
python3 - "${workflow}" <<'PY'
from __future__ import annotations

import re
import sys
from pathlib import Path

workflow = Path(sys.argv[1])
text = workflow.read_text(encoding="utf-8")


def fail(message: str) -> None:
    raise SystemExit(f"release security check: {message}")


if re.search(r"(?m)^\s*workflow_dispatch\s*:", text):
    fail("secret-bearing release workflow must not expose workflow_dispatch")
if not re.search(r'''(?ms)^on:\s*\n\s+push:\s*\n\s+tags:\s*\n\s+-\s+["']v\*["']\s*$''', text):
    fail("release trigger must be a v* tag push")
if "github.event.inputs" in text or "inputs.publish_npm" in text or "inputs.publish_pypi" in text:
    fail("manual publish inputs remain in the release workflow")


def job_block(name: str) -> str:
    match = re.search(rf"(?ms)^  {re.escape(name)}:\s*\n(?P<body>.*?)(?=^  [A-Za-z0-9_-]+:\s*$|\Z)", text)
    if not match:
        fail(f"job {name} is missing")
    return match.group("body")


build = job_block("build")
package_build = job_block("package-build")
github_release = job_block("github-release")
npm = job_block("npm-publish")
pypi = job_block("pypi-publish")

for name, block in (("build", build), ("package-build", package_build), ("github-release", github_release), ("npm-publish", npm), ("pypi-publish", pypi)):
    if "github.event_name == 'push'" not in block or "github.ref_type == 'tag'" not in block:
        fail(f"{name} is not restricted to a tag push")
    if "persist-credentials: false" not in block:
        fail(f"{name} checkout must disable persisted credentials")
    if "scripts/verify-release-context.sh" not in block:
        fail(f"{name} does not verify the event tag and protected-main ancestry")

if "contents: write" not in github_release:
    fail("github-release must retain contents: write for the immutable release")
for name, block in (("npm-publish", npm), ("pypi-publish", pypi)):
    environment = "npm" if name == "npm-publish" else "pypi"
    if f"environment: {environment}" not in block:
        fail(f"{name} must use its protected environment")
    if "id-token: write" not in block or "contents: read" not in block:
        fail(f"{name} must request only read contents and OIDC identity")
    if re.search(r"(?m)^\s+(?:contents|actions|packages|deployments):\s+write\s*$", block):
        fail(f"{name} requests an unnecessary write permission")
    if "needs.package-build.outputs.publishable == 'true'" not in block:
        fail(f"{name} is not gated by the package version/source gate")
    if "scripts/release_package.py verify" not in block:
        fail(f"{name} does not verify the immutable package artifact manifest")
if "npm publish" not in npm or "--provenance" not in npm:
    fail("npm publishing must use a verified tarball and npm provenance")
if "packages-dir:" not in pypi or "dist/packages/python" not in pypi:
    fail("PyPI publishing must consume the verified Python artifact directory")
if "actions/checkout@" not in text or "ref: ${{ github.sha }}" not in text:
    fail("every checkout must bind to the event commit")
if "artifact_id" not in text or "artifact-ids:" not in text:
    fail("publish jobs must download artifacts from this exact workflow run")
if "source_commit" not in package_build or "source_commit" not in github_release:
    fail("release jobs must carry the verified source commit")
PY

# The pin validator also runs actionlint and rejects mutable action refs.
"${root}/scripts/test-release-workflow.sh" "${workflow}"

# Verify that the security gate catches a reintroduced manual trigger.
bad_workflow="$(mktemp)"
trap 'rm -f "${bad_workflow}"' EXIT
cp "${workflow}" "${bad_workflow}"
printf '\nworkflow_dispatch:\n' >>"${bad_workflow}"
if "${BASH_SOURCE[0]}" "${bad_workflow}" >/dev/null 2>&1; then
  fail "manual-trigger regression fixture unexpectedly passed"
fi

echo "release security check: passed"
