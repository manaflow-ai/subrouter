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
command -v ruby >/dev/null 2>&1 || fail "Ruby is required to parse workflow YAML"

uses_count=0
pypi_count=0
if ! action_refs="$(ruby -ryaml -e '
  def collect(value, refs)
    case value
    when Hash
      value.each do |key, child|
        refs << child if key.to_s == "uses"
        collect(child, refs)
      end
    when Array
      value.each { |child| collect(child, refs) }
    end
  end

  refs = []
  collect(YAML.safe_load(File.read(ARGV.fetch(0)), aliases: true), refs)
  refs.each { |ref| puts ref.to_s.strip }
' "${workflow}")"; then
  fail "Ruby could not parse workflow YAML"
fi
while IFS= read -r action_ref; do
  [[ -n "${action_ref}" ]] || continue
  uses_count=$((uses_count + 1))
  [[ "${action_ref}" =~ ^[^@[:space:]]+@[0-9a-f]{40}$ ]] || {
    fail "${workflow} uses a mutable or malformed action ref: ${action_ref}"
  }
  if [[ "${action_ref}" == "pypa/gh-action-pypi-publish@${expected_pypi_revision}" ]]; then
    pypi_count=$((pypi_count + 1))
  fi
done <<< "${action_refs}"

(( uses_count > 0 )) || fail "no action references found"
(( pypi_count == 1 )) || {
  fail "expected exactly one PyPI publisher pinned to ${expected_pypi_revision}, found ${pypi_count}"
}

command -v go >/dev/null 2>&1 || fail "Go is required to run the pinned actionlint revision"
go run "github.com/rhysd/actionlint/cmd/actionlint@${actionlint_revision}" "${workflow}"

echo "release workflow check: passed"
