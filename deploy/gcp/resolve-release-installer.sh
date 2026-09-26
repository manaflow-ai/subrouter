#!/usr/bin/env bash
# Resolve the front/slot installer used by a deployment action.
set -euo pipefail

[[ "$#" == 1 ]] || {
  echo "usage: $0 <checkout-default-installer>" >&2
  exit 2
}

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
exec bash "${script_dir}/resolve-release-asset.sh" \
  install-front-slots.sh "$1" "${SUBROUTER_INSTALL_FRONT_SLOTS:-}"
