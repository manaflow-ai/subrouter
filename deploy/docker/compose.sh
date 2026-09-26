#!/usr/bin/env sh
set -eu

script_dir=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)

die() {
  echo "compose: $*" >&2
  exit 1
}

[ "$#" -ge 2 ] || die "usage: compose.sh <local|team> <docker compose arguments>"
profile=$1
shift
case "${profile}" in
  local|team) ;;
  *) die "profile must be local or team" ;;
esac

runtime_uid=$(id -u)
runtime_gid=$(id -g)
[ "${runtime_uid}" -ne 0 ] || die "refusing to run Subrouter as root; invoke this script as a non-root user"

secret_dir="${SUBROUTER_DOCKER_SECRET_DIR:-${script_dir}/secrets}"
state_dir="${SUBROUTER_DOCKER_STATE_DIR:-${script_dir}/state}"
[ ! -L "${secret_dir}" ] || die "secret directory must not be a symlink: ${secret_dir}"
[ ! -L "${state_dir}" ] || die "state directory must not be a symlink: ${state_dir}"
[ -d "${secret_dir}" ] || die "secret directory is missing; run ${script_dir}/init-secrets.sh"
[ ! -L "${state_dir}/${profile}" ] || die "profile state directory must not be a symlink: ${state_dir}/${profile}"
[ -d "${state_dir}/${profile}" ] || die "state directory is missing; run ${script_dir}/init-secrets.sh"
[ -w "${state_dir}/${profile}" ] || die "state directory is not writable by UID ${runtime_uid}: ${state_dir}/${profile}"
secret_dir=$(CDPATH='' cd -- "${secret_dir}" && pwd)
state_dir=$(CDPATH='' cd -- "${state_dir}" && pwd)

required_secrets="admin-token account-import-token"
if [ "${profile}" = local ]; then
  required_secrets="proxy-token ${required_secrets}"
else
  required_secrets="team-cloud.json ${required_secrets}"
fi
for name in ${required_secrets}; do
  path="${secret_dir}/${name}"
  [ ! -L "${path}" ] || die "secret must not be a symlink: ${path}"
  [ -f "${path}" ] && [ -s "${path}" ] || die "secret is missing or empty: ${path}"
  [ -r "${path}" ] || die "secret is not readable by UID ${runtime_uid}: ${path}"
done

export SUBROUTER_DOCKER_UID="${runtime_uid}"
export SUBROUTER_DOCKER_GID="${runtime_gid}"
export SUBROUTER_DOCKER_SECRET_DIR="${secret_dir}"
export SUBROUTER_DOCKER_STATE_DIR="${state_dir}"

exec docker compose -f "${script_dir}/compose.yaml" --profile "${profile}" "$@"
