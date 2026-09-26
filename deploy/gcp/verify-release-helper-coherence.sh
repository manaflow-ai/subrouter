#!/usr/bin/env bash
# Refuse to run a golden deployment with release helpers that differ from the
# checkout whose observer and deployment actions will consume them.
set -euo pipefail

if (( $# < 2 )); then
  echo "usage: $0 <release-asset-dir> <checkout-helper> [...]" >&2
  exit 2
fi

release_dir="$1"
shift
[[ -d "${release_dir}" && ! -L "${release_dir}" ]] || {
  echo "candidate release asset directory is missing or unsafe" >&2
  exit 1
}

for checkout_helper in "$@"; do
  helper_name="$(basename "${checkout_helper}")"
  release_helper="${release_dir}/${helper_name}"
  [[ -f "${checkout_helper}" && ! -L "${checkout_helper}" ]] || {
    echo "checkout release helper is missing or unsafe: ${helper_name}" >&2
    exit 1
  }
  [[ -f "${release_helper}" && ! -L "${release_helper}" ]] || {
    echo "candidate release helper is missing or unsafe: ${helper_name}" >&2
    exit 1
  }
  cmp -s -- "${checkout_helper}" "${release_helper}" || {
    echo "candidate release helper differs from checkout: ${helper_name}; cut and pin a new release before running golden continuity" >&2
    exit 1
  }
done
