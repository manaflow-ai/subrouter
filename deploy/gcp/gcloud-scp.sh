#!/usr/bin/env bash
set -euo pipefail

if (( $# < 3 )); then
  echo "usage: $0 <gcloud-binary> <source> <destination> [gcloud-arguments...]" >&2
  exit 2
fi

gcloud_binary="$1"
shift
exec "${gcloud_binary}" compute scp --scp-flag=-O "$@"
