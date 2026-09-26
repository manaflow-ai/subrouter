#!/usr/bin/env bash
set -euo pipefail

if (( $# < 3 )); then
  echo "usage: $0 <gcloud-binary> <source> <destination> [gcloud-arguments...]" >&2
  exit 2
fi

gcloud_binary="$1"
shift

command -v scp >/dev/null 2>&1 || {
  echo "scp is required" >&2
  exit 127
}
scp_probe="$(LC_ALL=C scp -O 2>&1 || true)"
case "${scp_probe}" in
  *"unknown option"*|*"illegal option"*|*"invalid option"*)
    exec "${gcloud_binary}" compute scp "$@"
    ;;
  *)
    exec "${gcloud_binary}" compute scp --scp-flag=-O "$@"
    ;;
esac
