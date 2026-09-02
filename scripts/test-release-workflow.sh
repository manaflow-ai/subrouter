#!/usr/bin/env bash
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
workflow="${1:-${root}/.github/workflows/release.yml}"
expected_pypi_revision="dc37677b2e1c63e2034f94d8a5b11f265b73ba33"
# Upstream actionlint v1.7.8. It supports the Go 1.24 toolchain used by CI;
# the commit pin keeps the linter itself stable.
actionlint_revision="e7d448ef7507c20fc4c88a95d0c448b848cd6127"

fail() {
  echo "release workflow check: $*" >&2
  exit 1
}

[[ -f "${workflow}" ]] || fail "workflow is missing: ${workflow}"

uses_count=0
pypi_count=0
while IFS=$'\t' read -r line_number action_ref; do
  [[ -n "${action_ref}" ]] || continue
  uses_count=$((uses_count + 1))
  [[ "${action_ref}" =~ ^[^@[:space:]]+@[0-9a-f]{40}$ ]] || {
    fail "${workflow}:${line_number} uses a mutable or malformed action ref: ${action_ref}"
  }
  if [[ "${action_ref}" == "pypa/gh-action-pypi-publish@${expected_pypi_revision}" ]]; then
    pypi_count=$((pypi_count + 1))
  fi
done < <(
  awk '
    /^[[:space:]]*uses:[[:space:]]*/ {
      value = $0
      sub(/^[[:space:]]*uses:[[:space:]]*/, "", value)
      sub(/[[:space:]]+#.*$/, "", value)
      sub(/[[:space:]]+$/, "", value)
      printf "%d\t%s\n", NR, value
    }
  ' "${workflow}"
)

(( uses_count > 0 )) || fail "no action references found"
(( pypi_count == 1 )) || {
  fail "expected exactly one PyPI publisher pinned to ${expected_pypi_revision}, found ${pypi_count}"
}

if [[ -n "${ACTIONLINT_BIN:-}" ]]; then
  "${ACTIONLINT_BIN}" "${workflow}"
elif command -v actionlint >/dev/null 2>&1; then
  actionlint "${workflow}"
elif command -v go >/dev/null 2>&1; then
  go run "github.com/rhysd/actionlint/cmd/actionlint@${actionlint_revision}" "${workflow}"
else
  fail "actionlint or Go is required"
fi

echo "release workflow check: passed"
