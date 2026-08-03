#!/usr/bin/env bash
# Resolve the front/slot installer used by a deployment action. An explicit
# release override is accepted only when its digest is bound to the verified,
# immutable release evidence produced by the local golden wrapper.
set -euo pipefail

[[ "$#" == 1 ]] || {
  echo "usage: $0 <checkout-default-installer>" >&2
  exit 2
}

default_installer="$1"
override_installer="${SUBROUTER_INSTALL_FRONT_SLOTS:-}"
installer="${override_installer:-${default_installer}}"
[[ -f "${installer}" && ! -L "${installer}" ]] || {
  echo "front/slot installer must be a regular non-symlink file: ${installer}" >&2
  exit 1
}

if [[ -n "${override_installer}" ]]; then
  verification="${SUBROUTER_RELEASE_VERIFICATION_JSON:-}"
  [[ -n "${verification}" && -f "${verification}" && ! -L "${verification}" ]] || {
    echo "release installer override requires regular release-verification evidence" >&2
    exit 1
  }
  command -v jq >/dev/null 2>&1 || {
    echo "jq is required to verify the release installer" >&2
    exit 1
  }
  command -v sha256sum >/dev/null 2>&1 || {
    echo "sha256sum is required to verify the release installer" >&2
    exit 1
  }
  expected="$({
    jq -er '
      select(
        .schema == "subrouter.release-verification/v1" and
        .release_published == true and
        .release_immutable == true and
        .asset_digest_verified == true and
        .strict_build_attestation_verified == true
      )
      | .assets["install-front-slots.sh"]
      | select(type == "string" and test("^[0-9a-f]{64}$"))
    ' "${verification}"
  })" || {
    echo "release verification does not bind the front/slot installer" >&2
    exit 1
  }
  actual="$(sha256sum "${installer}" | awk '{print $1}')"
  [[ "${actual}" == "${expected}" ]] || {
    echo "front/slot installer does not match verified release evidence" >&2
    exit 1
  }
fi

printf '%s\n' "${installer}"
