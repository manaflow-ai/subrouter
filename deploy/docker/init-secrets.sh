#!/usr/bin/env sh
set -eu

script_dir=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
secret_dir="${SUBROUTER_DOCKER_SECRET_DIR:-${script_dir}/secrets}"
umask 077

die() {
  echo "init-secrets: $*" >&2
  exit 1
}

[ ! -L "${secret_dir}" ] || die "secret directory must not be a symlink: ${secret_dir}"
if [ -e "${secret_dir}" ] && [ ! -d "${secret_dir}" ]; then
  die "secret directory path is not a directory: ${secret_dir}"
fi
mkdir -p "${secret_dir}"
chmod 0700 "${secret_dir}"

temporary_secret=""
cleanup() {
  if [ -n "${temporary_secret}" ]; then
    rm -f -- "${temporary_secret}"
  fi
}
trap cleanup EXIT HUP INT TERM

generate_secret() {
  destination=$1
  [ ! -L "${destination}" ] || die "secret destination must not be a symlink: ${destination}"
  if [ -e "${destination}" ]; then
    [ -f "${destination}" ] || die "secret destination is not a regular file: ${destination}"
    if [ -s "${destination}" ]; then
      chmod 0600 "${destination}"
      return
    fi
  fi

  base=$(basename -- "${destination}")
  temporary_secret=$(mktemp "${secret_dir}/.${base}.tmp.XXXXXX")
  chmod 0600 "${temporary_secret}"
  openssl rand -hex 32 >"${temporary_secret}"
  [ -s "${temporary_secret}" ] || die "generated an empty secret for ${destination}"
  mv -f -- "${temporary_secret}" "${destination}"
  temporary_secret=""
}

generate_secret "${secret_dir}/proxy-token"
generate_secret "${secret_dir}/admin-token"
generate_secret "${secret_dir}/account-import-token"

echo "Docker control secrets are ready in ${secret_dir}."
echo "For team mode, copy an authenticated cloud.json to ${secret_dir}/team-cloud.json with mode 0600."
