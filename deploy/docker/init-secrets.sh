#!/usr/bin/env sh
set -eu

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
secret_dir="${script_dir}/secrets"
umask 077
mkdir -p "${secret_dir}"

generate_secret() {
  destination=$1
  if [ ! -s "${destination}" ]; then
    openssl rand -hex 32 >"${destination}"
  fi
  chmod 0600 "${destination}"
}

generate_secret "${secret_dir}/proxy-token"
generate_secret "${secret_dir}/admin-token"
generate_secret "${secret_dir}/account-import-token"

echo "Docker control secrets are ready in ${secret_dir}."
echo "For team mode, copy an authenticated cloud.json to ${secret_dir}/team-cloud.json with mode 0600."
