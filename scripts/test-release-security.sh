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
python3 - "${workflow}" "${root}/scripts/verify-release-context.sh" <<'PY'
from __future__ import annotations

import re
import sys
from pathlib import Path

workflow = Path(sys.argv[1])
text = workflow.read_text(encoding="utf-8")
helper = Path(sys.argv[2])


def fail(message: str) -> None:
    raise SystemExit(f"release security check: {message}")


if re.search(r"(?m)^\s*workflow_dispatch\s*:", text):
    fail("secret-bearing release workflow must not expose workflow_dispatch")
if not re.search(r'''(?ms)^on:\s*\n\s+push:\s*\n\s+tags:\s*\n\s+-\s+["']v\*["']\s*$''', text):
    fail("release trigger must be a v* tag push")
if "github.event.inputs" in text or "inputs.publish_npm" in text or "inputs.publish_pypi" in text:
    fail("manual publish inputs remain in the release workflow")

helper_text = helper.read_text(encoding="utf-8")
if "GITHUB_TOKEN" not in helper_text or "http.extraheader" not in helper_text:
    fail("source verification must use a temporary authenticated fetch header")


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
    if "GITHUB_TOKEN: ${{ github.token }}" not in block:
        fail(f"{name} does not provide a token for private-repository ref verification")

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
tmp_dir="$(mktemp -d)"
cleanup() {
  rm -rf "${tmp_dir}"
}
trap cleanup EXIT
bad_workflow="${tmp_dir}/manual-trigger.yml"
cp "${workflow}" "${bad_workflow}"
printf '\nworkflow_dispatch:\n' >>"${bad_workflow}"
if "${BASH_SOURCE[0]}" "${bad_workflow}" >/dev/null 2>&1; then
  fail "manual-trigger regression fixture unexpectedly passed"
fi

# Exercise the source helper against a local remote. A detached commit and a
# moved tag must both fail, even when the caller supplies the old event SHA.
remote="${tmp_dir}/remote.git"
checkout="${tmp_dir}/checkout"
git init --bare "${remote}" >/dev/null
git clone -q "${remote}" "${checkout}"
git -C "${checkout}" config user.name "release-security-test"
git -C "${checkout}" config user.email "release-security-test@example.invalid"
git -C "${checkout}" config commit.gpgsign false
git -C "${checkout}" config tag.gpgsign false
printf 'base\n' >"${checkout}/marker"
git -C "${checkout}" add marker
git -C "${checkout}" commit -qm base
git -C "${checkout}" branch -M main
git -C "${checkout}" push -q origin main
git -C "${checkout}" tag v1.2.3
git -C "${checkout}" push -q origin refs/tags/v1.2.3
base_sha="$(git -C "${checkout}" rev-parse HEAD)"
if ! (cd "${checkout}" && env GIT_TERMINAL_PROMPT=0 GITHUB_TOKEN=test-token GITHUB_EVENT_NAME=push GITHUB_REF_PROTECTED=true GITHUB_SHA="${base_sha}" GITHUB_REF=refs/tags/v1.2.3 GITHUB_REF_NAME=v1.2.3 GITHUB_REF_TYPE=tag "${root}/scripts/verify-release-context.sh" >"${tmp_dir}/context.out"); then
  fail "valid protected-main tag was rejected"
fi
[[ "$(<"${tmp_dir}/context.out")" == "${base_sha}" ]] || fail "source helper returned the wrong commit"
if (cd "${checkout}" && env GIT_TERMINAL_PROMPT=0 GITHUB_TOKEN=test-token GITHUB_EVENT_NAME=push GITHUB_REF_PROTECTED=false GITHUB_SHA="${base_sha}" GITHUB_REF=refs/tags/v1.2.3 GITHUB_REF_NAME=v1.2.3 GITHUB_REF_TYPE=tag "${root}/scripts/verify-release-context.sh" >/dev/null 2>&1); then
  fail "unprotected tag context passed the source helper"
fi

git -C "${checkout}" checkout -qb untrusted
printf 'untrusted\n' >>"${checkout}/marker"
git -C "${checkout}" commit -qam untrusted
git -C "${checkout}" tag v1.2.4
git -C "${checkout}" push -q origin untrusted refs/tags/v1.2.4
untrusted_sha="$(git -C "${checkout}" rev-parse HEAD)"
if (cd "${checkout}" && env GIT_TERMINAL_PROMPT=0 GITHUB_TOKEN=test-token GITHUB_EVENT_NAME=push GITHUB_REF_PROTECTED=true GITHUB_SHA="${untrusted_sha}" GITHUB_REF=refs/tags/v1.2.4 GITHUB_REF_NAME=v1.2.4 GITHUB_REF_TYPE=tag "${root}/scripts/verify-release-context.sh" >/dev/null 2>&1); then
  fail "tag outside protected main passed the source helper"
fi

git -C "${checkout}" checkout -q "${base_sha}"
git -C "${checkout}" tag -f v1.2.3 "${untrusted_sha}" >/dev/null
git -C "${checkout}" push -q --force origin refs/tags/v1.2.3
if (cd "${checkout}" && env GIT_TERMINAL_PROMPT=0 GITHUB_TOKEN=test-token GITHUB_EVENT_NAME=push GITHUB_REF_PROTECTED=true GITHUB_SHA="${base_sha}" GITHUB_REF=refs/tags/v1.2.3 GITHUB_REF_NAME=v1.2.3 GITHUB_REF_TYPE=tag "${root}/scripts/verify-release-context.sh" >/dev/null 2>&1); then
  fail "moved release tag passed the source helper"
fi

echo "release security check: passed"
