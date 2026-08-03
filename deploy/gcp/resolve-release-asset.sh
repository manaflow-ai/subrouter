#!/usr/bin/env bash
# Resolve a deployment helper. Overrides must be bound to immutable release
# verification evidence from the local golden wrapper.
set -euo pipefail

[[ "$#" == 3 ]] || {
  echo "usage: $0 <asset-name> <checkout-default> <override>" >&2
  exit 2
}

asset="$1"
default_path="$2"
override_path="$3"
case "${asset}" in
  install-front-slots.sh|deployment-contract.py) ;;
  *) echo "unsupported release deployment asset: ${asset}" >&2; exit 2 ;;
esac

resolved="${override_path:-${default_path}}"
[[ -f "${resolved}" && ! -L "${resolved}" ]] || {
  echo "${asset} must be a regular non-symlink file: ${resolved}" >&2
  exit 1
}

if [[ -n "${override_path}" ]]; then
  verification="${SUBROUTER_RELEASE_VERIFICATION_JSON:-}"
  [[ -n "${verification}" && -f "${verification}" && ! -L "${verification}" ]] || {
    echo "${asset} override requires regular release-verification evidence" >&2
    exit 1
  }
  command -v jq >/dev/null 2>&1 || {
    echo "jq is required to verify ${asset}" >&2
    exit 1
  }
  command -v sha256sum >/dev/null 2>&1 || {
    echo "sha256sum is required to verify ${asset}" >&2
    exit 1
  }
  expected="$({
    jq -er --arg asset "${asset}" '
      select(
        .schema == "subrouter.release-verification/v1" and
        .release_published == true and
        .release_immutable == true and
        .asset_digest_verified == true and
        .strict_build_attestation_verified == true
      )
      | .assets[$asset]
      | select(type == "string" and test("^[0-9a-f]{64}$"))
    ' "${verification}"
  })" || {
    echo "release verification does not bind ${asset}" >&2
    exit 1
  }
  actual="$(sha256sum "${resolved}" | awk '{print $1}')"
  [[ "${actual}" == "${expected}" ]] || {
    echo "${asset} does not match verified release evidence" >&2
    exit 1
  }
fi

printf '%s\n' "${resolved}"
