#!/usr/bin/env bash
# Verify Go build provenance without routing multi-line metadata through shell
# variables or here-strings. macOS Bash can block while constructing those
# redirections, so the metadata file is the sole owner of the command output.
set -euo pipefail
umask 077

usage() {
  cat <<'EOF'
Usage: verify-go-release-binary.sh BINARY EXPECTED_REVISION
EOF
}

if (( $# != 2 )); then
  usage >&2
  exit 2
fi

binary="$1"
expected_revision="$2"
[[ -f "${binary}" && ! -L "${binary}" ]] || {
  echo "release binary is missing or unsafe: ${binary}" >&2
  exit 1
}
[[ "${expected_revision}" =~ ^[0-9a-f]{40}$ ]] || {
  echo "expected release revision must be a full commit" >&2
  exit 1
}
for command in go grep mktemp; do
  command -v "${command}" >/dev/null 2>&1 || {
    echo "${command} is required" >&2
    exit 1
  }
done

metadata_file="$(mktemp "${TMPDIR:-/tmp}/subrouter-go-release-metadata.XXXXXX")"
cleanup() {
  unlink "${metadata_file}" 2>/dev/null || true
}
trap cleanup EXIT INT TERM

go version -m "${binary}" >"${metadata_file}" || {
  echo "could not read Go release metadata: ${binary}" >&2
  exit 1
}
grep -F "vcs.revision=${expected_revision}" "${metadata_file}" >/dev/null || {
  echo "release binary embedded revision mismatch: ${binary}" >&2
  exit 1
}
grep -F 'vcs.modified=false' "${metadata_file}" >/dev/null || {
  echo "release binary reports modified source: ${binary}" >&2
  exit 1
}
