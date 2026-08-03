#!/usr/bin/env bash
# Run the production continuity golden gate from an operator's local Apple
# Silicon Mac. The helper built from this checkout only observes metadata. The
# Subrouter client under test is downloaded from a checksum-verified release.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"

usage() {
  printf '%s\n' \
    'usage: ./deploy/gcp/golden-local-mac-production-continuity.sh [options] --activate COMMAND [ARGS...] --rollback COMMAND [ARGS...] --old-generation-check COMMAND [ARGS...]' \
    '' \
    'The three commands inherit the operator environment, but command arguments, output, and environment are never copied into evidence.' \
    'Options: --released-version VERSION --cloud-config PATH --codex-home PATH --codex-bin PATH --artifact-dir PATH --model MODEL --stream-lines N --timeout DURATION'
}

if [[ $# -eq 0 ]]; then
  usage >&2
  exit 2
fi
if [[ "${1}" == "-h" || "${1}" == "--help" ]]; then
  usage
  exit 0
fi

private_build="$(mktemp -d "${TMPDIR:-/tmp}/subrouter-golden-observer.XXXXXX")"
cleanup() {
  rm -rf "${private_build}"
}
trap cleanup EXIT INT TERM

if [[ "$(uname -s)" != "Darwin" || "$(uname -m)" != "arm64" ]]; then
  echo "golden continuity gate must run locally on macOS arm64" >&2
  exit 1
fi

(
  cd "${root}"
  go build -trimpath -o "${private_build}/subrouter-transport-observer" ./cmd/subrouter-transport-observer
)

"${private_build}/subrouter-transport-observer" golden "$@"
