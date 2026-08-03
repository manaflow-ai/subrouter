#!/usr/bin/env bash
# Bootstrap a fresh VM from one metadata-pinned immutable release. Services stay
# stopped until publish-subrouter.sh writes distinct control tokens over IAP.
set -euo pipefail

export DEBIAN_FRONTEND=noninteractive

if [[ "${SUBROUTER_STARTUP_SKIP_PACKAGES:-0}" != 1 ]]; then
  apt-get update
  apt-get install -y ca-certificates curl jq python3 util-linux
fi

metadata_base="${SUBROUTER_METADATA_BASE_URL:-http://metadata.google.internal/computeMetadata/v1/instance/attributes}"
metadata_header=( -H 'Metadata-Flavor: Google' )
if [[ "${metadata_base}" == file://* ]]; then
  metadata_header=()
fi
metadata_value() {
  curl -fsSL "${metadata_header[@]}" "${metadata_base}/$1"
}

work_dir="$(mktemp -d /tmp/subrouter-startup.XXXXXX)"
cleanup() {
  rm -rf -- "${work_dir}"
}
trap cleanup EXIT INT TERM

metadata_file="${work_dir}/release-metadata.json"
metadata_value subrouter-release-metadata >"${metadata_file}"
metadata_sha256="$(metadata_value subrouter-release-metadata-sha256 | tr -d '[:space:]')"
[[ "${metadata_sha256}" =~ ^[0-9a-f]{64}$ ]] || {
  echo "startup: release metadata digest is invalid" >&2
  exit 1
}
[[ "$(sha256sum "${metadata_file}" | awk '{print $1}')" == "${metadata_sha256}" ]] || {
  echo "startup: release metadata digest mismatch" >&2
  exit 1
}

repo="$(jq -r '.repository // empty' "${metadata_file}")"
release_tag="$(jq -r '.release_tag // empty' "${metadata_file}")"
release_revision="$(jq -r '.source_revision // empty' "${metadata_file}")"
verification_sha256="$(jq -r '.verification_evidence_sha256 // empty' "${metadata_file}")"
if [[ "${repo}" != manaflow-ai/subrouter ]]; then
  echo "startup: release metadata repository is not trusted" >&2
  exit 1
fi
if [[ ! "${release_tag}" =~ ^v[0-9]+\.[0-9]+\.[0-9]+([.-][0-9A-Za-z.-]+)?$ ]]; then
  echo "startup: release metadata tag is invalid" >&2
  exit 1
fi
if [[ ! "${release_revision}" =~ ^[0-9a-f]{40}$ || ! "${verification_sha256}" =~ ^[0-9a-f]{64}$ ]]; then
  echo "startup: release metadata provenance is invalid" >&2
  exit 1
fi
version="${release_tag#v}"
binary_asset="subrouter_${version}_linux_amd64"
required_assets=(SHA256SUMS SOURCE_PROVENANCE.json install.sh install-front-slots.sh "${binary_asset}")
jq -e --arg binary "${binary_asset}" '
  .schema == "subrouter.gcp.vm-release-metadata/v1" and
  .repository == "manaflow-ai/subrouter" and
  .tag_on_main == true and .release_immutable == true and
  .strict_build_attestation_verified == true and .asset_digest_verified == true and
  .provenance_verified == true and .embedded_revision_verified == true and
  (.assets | type) == "object" and
  ((.assets | keys | sort) == (["SHA256SUMS","SOURCE_PROVENANCE.json","install.sh","install-front-slots.sh",$binary] | sort)) and
  ([.assets[]] | all(type == "string" and test("^[0-9a-f]{64}$")))
' "${metadata_file}" >/dev/null || {
  echo "startup: release metadata trust proof or asset set is incomplete" >&2
  exit 1
}

api_base="${SUBROUTER_GITHUB_API_BASE:-https://api.github.com/repos/${repo}}"
release_api_url="${SUBROUTER_RELEASE_API_URL:-${api_base}/releases/tags/${release_tag}}"
compare_api_url="${SUBROUTER_COMPARE_API_URL:-${api_base}/compare/${release_revision}...main}"
release_state="${work_dir}/release-state.json"
compare_state="${work_dir}/compare-state.json"
curl -fsSL "${release_api_url}" >"${release_state}"
jq -e --arg tag "${release_tag}" '.tag_name == $tag and .draft == false and .immutable == true' \
  "${release_state}" >/dev/null || {
  echo "startup: GitHub release is missing, draft, or mutable" >&2
  exit 1
}
curl -fsSL "${compare_api_url}" >"${compare_state}"
jq -e --arg revision "${release_revision}" '
  .merge_base_commit.sha == $revision and (.status == "ahead" or .status == "identical")
' "${compare_state}" >/dev/null || {
  echo "startup: release revision is not on protected main" >&2
  exit 1
}

release_base="${SUBROUTER_RELEASE_BASE:-https://github.com/${repo}/releases/download/${release_tag}}"
for asset in "${required_assets[@]}"; do
  expected="$(jq -r --arg asset "${asset}" '.assets[$asset] // empty' "${metadata_file}")"
  remote_matches="$(jq -r --arg asset "${asset}" --arg digest "sha256:${expected}" \
    '[.assets[]? | select(.name == $asset and .digest == $digest)] | length' "${release_state}")"
  [[ "${remote_matches}" == 1 ]] || {
    echo "startup: immutable release digest is missing for ${asset}" >&2
    exit 1
  }
  curl -fsSL --retry 3 --retry-all-errors -o "${work_dir}/${asset}" "${release_base}/${asset}"
  [[ "$(sha256sum "${work_dir}/${asset}" | awk '{print $1}')" == "${expected}" ]] || {
    echo "startup: downloaded ${asset} digest mismatch" >&2
    exit 1
  }
done

manifest="${work_dir}/SHA256SUMS"
for asset in SOURCE_PROVENANCE.json install.sh install-front-slots.sh "${binary_asset}"; do
  expected="$(jq -r --arg asset "${asset}" '.assets[$asset]' "${metadata_file}")"
  manifest_matches="$(awk -v asset="${asset}" '$2 == asset || $2 == "*" asset {print $1}' "${manifest}")"
  [[ "$(wc -l <<<"${manifest_matches}" | tr -d '[:space:]')" == 1 && "${manifest_matches}" == "${expected}" ]] || {
    echo "startup: SHA256SUMS does not bind ${asset} to metadata" >&2
    exit 1
  }
done
jq -e --arg tag "${release_tag}" --arg revision "${release_revision}" '
  .tag == $tag and .source_revision == $revision and .tag_on_main == true
' "${work_dir}/SOURCE_PROVENANCE.json" >/dev/null || {
  echo "startup: source provenance does not match release metadata" >&2
  exit 1
}
install_dir="${SUBROUTER_SYSTEM_INSTALL_DIR:-/usr/local/bin}"
libexec_dir="${SUBROUTER_SYSTEM_LIBEXEC_DIR:-/usr/local/libexec}"
version_file="${SUBROUTER_VERSION_FILE:-/etc/subrouter-version}"
install -d -m 0755 "${install_dir}" "${libexec_dir}" "$(dirname "${version_file}")"
candidate="${install_dir}/.subrouter.startup.$$"
install -m 0755 "${work_dir}/${binary_asset}" "${candidate}"
[[ "$(sha256sum "${candidate}" | awk '{print $1}')" == "$(jq -r --arg asset "${binary_asset}" '.assets[$asset]' "${metadata_file}")" ]] || {
  echo "startup: staged binary digest changed" >&2
  exit 1
}
mv -f -- "${candidate}" "${install_dir}/subrouter"
ln -sfn subrouter "${install_dir}/sr"
ln -sfn subrouter "${install_dir}/cx"
printf '%s\n' "${release_tag}" >"${version_file}"
install -m 0755 "${work_dir}/install-front-slots.sh" "${libexec_dir}/subrouter-install-front-slots"

# Create the service user, state paths, defaults, and stopped compatibility
# units. The front/slot installer then makes those units the only boot topology.
"${install_dir}/sr" install-systemd --addr 0.0.0.0:31415 --cx-switch-interval 10m --start=false
binary_sha256="$(jq -r --arg asset "${binary_asset}" '.assets[$asset]' "${metadata_file}")"
installer_env=(
  "SUBROUTER_RELEASE_ROOT=${SUBROUTER_RELEASE_ROOT:-/opt/subrouter/releases}"
  "SUBROUTER_SLOT_ROOT=${SUBROUTER_SLOT_ROOT:-/opt/subrouter/slots}"
  "SUBROUTER_FRONT_ROOT=${SUBROUTER_FRONT_ROOT:-/opt/subrouter/front}"
  "SUBROUTER_CONTROL_ROOT=${SUBROUTER_CONTROL_ROOT:-/opt/subrouter/control}"
  "SUBROUTER_STATE_DIR=${SUBROUTER_STATE_DIR:-/var/lib/subrouter}"
  "SUBROUTER_FRONT_ENV=${SUBROUTER_FRONT_ENV:-/etc/default/subrouter-front}"
  "SUBROUTER_DEFAULTS_FILE=${SUBROUTER_DEFAULTS_FILE:-/etc/default/subrouter}"
  "SUBROUTER_SLOT_UNIT=${SUBROUTER_SLOT_UNIT:-/etc/systemd/system/subrouter-slot@.service}"
  "SUBROUTER_FRONT_UNIT=${SUBROUTER_FRONT_UNIT:-/etc/systemd/system/subrouter-front.service}"
)
env "${installer_env[@]}" "${libexec_dir}/subrouter-install-front-slots" \
  install-release "${release_tag}" "${work_dir}/${binary_asset}" "${binary_sha256}"
env "${installer_env[@]}" "${libexec_dir}/subrouter-install-front-slots" \
  prepare-fresh-topology "${release_tag}" slot-a

release_root="${SUBROUTER_RELEASE_ROOT:-/opt/subrouter/releases}/${release_tag}"
install -m 0644 "${metadata_file}" "${release_root}/VM_RELEASE_METADATA.json"
install -m 0644 "${manifest}" "${release_root}/SHA256SUMS"
install -m 0644 "${work_dir}/SOURCE_PROVENANCE.json" "${release_root}/SOURCE_PROVENANCE.json"
install -m 0644 "${work_dir}/install.sh" "${release_root}/install.sh"
install -m 0644 "${work_dir}/install-front-slots.sh" "${release_root}/install-front-slots.sh"

# Install the rate-limit reroute self-verifier from the exact release commit.
# This is supplemental monitoring, so transient GitHub failure stays nonfatal.
install_subrouter_verify() {
  local base="https://raw.githubusercontent.com/${repo}/${release_revision}/deploy/gcp"
  curl -fsSL "${base}/subrouter-verify.sh" -o "${install_dir}/subrouter-verify.sh" || return 1
  chmod 0755 "${install_dir}/subrouter-verify.sh"
  curl -fsSL "${base}/subrouter-verify.service" -o /etc/systemd/system/subrouter-verify.service || return 1
  curl -fsSL "${base}/subrouter-verify.timer" -o /etc/systemd/system/subrouter-verify.timer || return 1
  systemctl daemon-reload
  systemctl enable --now subrouter-verify.timer
}
if [[ "${SUBROUTER_STARTUP_SKIP_VERIFY_TIMER:-0}" != 1 ]]; then
  install_subrouter_verify || echo "startup: subrouter-verify install failed (non-fatal)"
fi

echo "startup: verified ${release_tag} (${release_revision}) and prepared front/slot topology"
