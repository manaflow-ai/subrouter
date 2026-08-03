#!/usr/bin/env bash
# Resolve the deployment contract used by a deployment action.
set -euo pipefail

[[ "$#" == 1 ]] || {
  echo "usage: $0 <checkout-default-contract>" >&2
  exit 2
}

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
exec bash "${script_dir}/resolve-release-asset.sh" \
  deployment-contract.py "$1" "${SUBROUTER_DEPLOYMENT_CONTRACT:-}"
