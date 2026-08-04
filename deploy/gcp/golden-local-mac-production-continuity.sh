#!/usr/bin/env bash
# Verify the immutable v0.1.51/v0.1.55/v0.1.56 release inputs, then run the complete
# legacy-to-front migration and slot-upgrade continuity gate from a local Mac.
set -euo pipefail
umask 077

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
deployment_contract="${root}/deploy/gcp/deployment-contract.py"
repository="manaflow-ai/subrouter"
predecessor_tag="v0.1.51"
predecessor_version="0.1.51"
predecessor_revision="5eacb5411c0bd4a24f4e422d6366fa7bfd1843c8"
predecessor_darwin_sha="74f4bfbbf6b8dcbe0509eaaa9f63b1eb688358a749ed3b451066e146591d2582"
predecessor_linux_sha="99fcd10d912184c160370eb228b382795101f2b5b2467244f995aa2d10b0c323"
bootstrap_tag="v0.1.55"
bootstrap_version="0.1.55"
bootstrap_revision="c4ea17e91ef6e9d0ab31cdd2774ca8d5387219bc"
bootstrap_linux_sha="6261bda248a6afc84079ecd22ded35e71d3b4cfb5267a6db2871a35cdcf0bd0c"
candidate_tag="v0.1.56"
candidate_version="0.1.56"

usage() {
  cat <<'EOF'
Usage: ./deploy/gcp/golden-local-mac-production-continuity.sh [options]

Requires SUBROUTER_GCP_PROJECT, SUBROUTER_GCP_ZONE,
SUBROUTER_GCP_INSTANCE, and SUBROUTER_PUBLIC_BASE_URL. The target must already
serve the v0.1.51 legacy topology. Staging is normalized to that exact worker
before the gate begins.

Options:
  --artifact-dir PATH
  --cloud-config PATH
  --codex-home PATH
  --codex-bin PATH
  --model MODEL
  --stream-lines N
  --timeout DURATION

The wrapper owns every migration and slot command. Phase command overrides are
not accepted.
EOF
}

artifact_dir=""
cloud_config_path=""
golden_args=()
while (( $# > 0 )); do
  case "$1" in
    --artifact-dir)
      (( $# >= 2 )) || { usage >&2; exit 2; }
      artifact_dir="$2"
      shift 2
      ;;
    --cloud-config)
      (( $# >= 2 )) || { usage >&2; exit 2; }
      cloud_config_path="$2"
      golden_args+=("$1" "$2")
      shift 2
      ;;
    --codex-home|--codex-bin|--model|--stream-lines|--timeout)
      (( $# >= 2 )) || { usage >&2; exit 2; }
      golden_args+=("$1" "$2")
      shift 2
      ;;
    --help|-h)
      usage
      exit 0
      ;;
    *)
      printf 'unknown option: %s\n' "$1" >&2
      usage >&2
      exit 2
      ;;
  esac
done

if [[ "$(uname -s)" != Darwin || "$(uname -m)" != arm64 ]]; then
  echo "golden continuity gate must run locally on macOS arm64" >&2
  exit 1
fi
for command in gh gcloud go jq python3; do
  command -v "${command}" >/dev/null 2>&1 || { echo "${command} is required" >&2; exit 1; }
done
if ! command -v sha256sum >/dev/null 2>&1 && ! command -v shasum >/dev/null 2>&1; then
  echo "sha256sum or shasum is required" >&2
  exit 1
fi

: "${SUBROUTER_GCP_PROJECT:?set SUBROUTER_GCP_PROJECT}"
: "${SUBROUTER_GCP_ZONE:?set SUBROUTER_GCP_ZONE}"
: "${SUBROUTER_GCP_INSTANCE:?set SUBROUTER_GCP_INSTANCE}"
: "${SUBROUTER_PUBLIC_BASE_URL:?set SUBROUTER_PUBLIC_BASE_URL}"

if [[ -z "${cloud_config_path}" ]]; then
  cloud_config_path="${SUBROUTER_CLOUD_CONFIG:-${HOME}/.config/subrouter/cloud.json}"
fi
[[ -f "${cloud_config_path}" ]] || {
  echo "cloud config is missing: ${cloud_config_path}" >&2
  exit 1
}
normalized_public_base_url="$(python3 "${deployment_contract}" validate-target \
  "${cloud_config_path}" "${SUBROUTER_GCP_INSTANCE}" "${SUBROUTER_PUBLIC_BASE_URL}")"
SUBROUTER_PUBLIC_BASE_URL="${normalized_public_base_url}"
export SUBROUTER_PUBLIC_BASE_URL

private_root="$(mktemp -d "${TMPDIR:-/tmp}/subrouter-golden-production.XXXXXX")"
cleanup() {
  if [[ -n "${private_root:-}" && -d "${private_root}" ]]; then
    rm -rf -- "${private_root}"
  fi
}
trap cleanup EXIT INT TERM

sha256_file() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | awk '{print $1}'
  else
    shasum -a 256 "$1" | awk '{print $1}'
  fi
}

manifest_sha() {
  python3 "${deployment_contract}" manifest-sha "$1" "$2"
}

require_release_revision_on_main() {
  local tag="$1"
  local expected_revision="${2:-}"
  "${root}/deploy/gcp/verify-release-on-main.sh" \
    "${repository}" "${tag}" "${expected_revision}"
}

verify_go_release_binary() {
  local path="$1"
  local revision="$2"
  bash "${root}/deploy/gcp/verify-go-release-binary.sh" "${path}" "${revision}"
}

predecessor_dir="${private_root}/predecessor"
bootstrap_dir="${private_root}/bootstrap"
candidate_dir="${private_root}/candidate"
mkdir -p "${predecessor_dir}" "${bootstrap_dir}" "${candidate_dir}"

if ! gh release view "${predecessor_tag}" --repo "${repository}" --json tagName,isDraft,publishedAt \
    | jq -e --arg tag "${predecessor_tag}" \
      '.tagName == $tag and (.isDraft | not) and (.publishedAt | type == "string" and length > 0)' \
      >/dev/null; then
  echo "v0.1.51 is not a published release" >&2
  exit 1
fi
resolved_predecessor_revision="$(require_release_revision_on_main "${predecessor_tag}" "${predecessor_revision}")"

predecessor_darwin_asset="subrouter_${predecessor_version}_darwin_arm64"
predecessor_linux_asset="subrouter_${predecessor_version}_linux_amd64"
gh release download "${predecessor_tag}" --repo "${repository}" --dir "${predecessor_dir}" \
  --pattern SHA256SUMS --pattern "${predecessor_darwin_asset}" --pattern "${predecessor_linux_asset}"
predecessor_manifest="${predecessor_dir}/SHA256SUMS"
predecessor_darwin="${predecessor_dir}/${predecessor_darwin_asset}"
predecessor_linux="${predecessor_dir}/${predecessor_linux_asset}"
[[ "$(sha256_file "${predecessor_darwin}")" == "${predecessor_darwin_sha}" &&
   "$(manifest_sha "${predecessor_manifest}" "${predecessor_darwin_asset}")" == "${predecessor_darwin_sha}" ]] \
  || { echo "v0.1.51 Darwin asset hard pin mismatch" >&2; exit 1; }
[[ "$(sha256_file "${predecessor_linux}")" == "${predecessor_linux_sha}" &&
   "$(manifest_sha "${predecessor_manifest}" "${predecessor_linux_asset}")" == "${predecessor_linux_sha}" ]] \
  || { echo "v0.1.51 Linux asset hard pin mismatch" >&2; exit 1; }
chmod 0700 "${predecessor_darwin}" "${predecessor_linux}"
verify_go_release_binary "${predecessor_darwin}" "${resolved_predecessor_revision}"
verify_go_release_binary "${predecessor_linux}" "${resolved_predecessor_revision}"

if ! gh release view "${bootstrap_tag}" --repo "${repository}" --json tagName,isDraft,isPrerelease,isImmutable,publishedAt \
    | jq -e --arg tag "${bootstrap_tag}" \
      '.tagName == $tag and (.isDraft | not) and (.isPrerelease | not) and .isImmutable == true and
       (.publishedAt | type == "string" and length > 0)' \
      >/dev/null; then
  echo "v0.1.55 is not a published immutable release" >&2
  exit 1
fi
resolved_bootstrap_revision="$(require_release_revision_on_main "${bootstrap_tag}" "${bootstrap_revision}")"
bootstrap_linux_asset="subrouter_${bootstrap_version}_linux_amd64"
bootstrap_assets=(SHA256SUMS SOURCE_PROVENANCE.json "${bootstrap_linux_asset}")
bootstrap_download_args=()
for asset in "${bootstrap_assets[@]}"; do
  bootstrap_download_args+=(--pattern "${asset}")
done
gh release download "${bootstrap_tag}" --repo "${repository}" --dir "${bootstrap_dir}" "${bootstrap_download_args[@]}"
bootstrap_manifest="${bootstrap_dir}/SHA256SUMS"
for asset in "${bootstrap_assets[@]}"; do
  path="${bootstrap_dir}/${asset}"
  [[ -f "${path}" && ! -L "${path}" ]] || { echo "bootstrap release asset is missing or unsafe: ${asset}" >&2; exit 1; }
  gh release verify-asset "${bootstrap_tag}" "${path}" --repo "${repository}" --format json >/dev/null
  if ! gh attestation verify "${path}" --repo "${repository}" \
      --signer-workflow "${repository}/.github/workflows/release.yml" \
      --source-ref "refs/tags/${bootstrap_tag}" --source-digest "${resolved_bootstrap_revision}" \
      --deny-self-hosted-runners --format json \
      | jq -e 'length > 0' >/dev/null; then
    echo "strict bootstrap build attestation verification failed: ${asset}" >&2
    exit 1
  fi
done
bootstrap_linux="${bootstrap_dir}/${bootstrap_linux_asset}"
[[ "$(sha256_file "${bootstrap_linux}")" == "${bootstrap_linux_sha}" &&
   "$(manifest_sha "${bootstrap_manifest}" "${bootstrap_linux_asset}")" == "${bootstrap_linux_sha}" ]] \
  || { echo "v0.1.55 Linux asset hard pin mismatch" >&2; exit 1; }
jq -e --arg tag "${bootstrap_tag}" --arg revision "${resolved_bootstrap_revision}" \
  '(. | keys | sort) == (["source_revision","tag","tag_on_main"] | sort) and
   .tag == $tag and .source_revision == $revision and .tag_on_main == true' \
  "${bootstrap_dir}/SOURCE_PROVENANCE.json" >/dev/null \
  || { echo "bootstrap source provenance is invalid" >&2; exit 1; }
chmod 0700 "${bootstrap_linux}"
verify_go_release_binary "${bootstrap_linux}" "${resolved_bootstrap_revision}"

if ! gh release view "${candidate_tag}" --repo "${repository}" --json tagName,isDraft,isPrerelease,isImmutable,publishedAt \
    | jq -e --arg tag "${candidate_tag}" \
      '.tagName == $tag and (.isDraft | not) and (.isPrerelease | not) and .isImmutable == true and
       (.publishedAt | type == "string" and length > 0)' \
      >/dev/null; then
  echo "v0.1.56 is not a published immutable release" >&2
  exit 1
fi
candidate_revision="$(require_release_revision_on_main "${candidate_tag}")"
[[ "${candidate_revision}" != "${resolved_bootstrap_revision}" &&
   "${resolved_bootstrap_revision}" != "${resolved_predecessor_revision}" ]] \
  || { echo "predecessor, bootstrap, and candidate revisions must differ" >&2; exit 1; }

candidate_linux_asset="subrouter_${candidate_version}_linux_amd64"
candidate_assets=(SHA256SUMS SOURCE_PROVENANCE.json deployment-contract.py install.sh install-front-slots.sh "${candidate_linux_asset}")
download_args=()
for asset in "${candidate_assets[@]}"; do
  download_args+=(--pattern "${asset}")
done
gh release download "${candidate_tag}" --repo "${repository}" --dir "${candidate_dir}" "${download_args[@]}"

candidate_manifest="${candidate_dir}/SHA256SUMS"
declare -A candidate_digests=()
for asset in "${candidate_assets[@]}"; do
  path="${candidate_dir}/${asset}"
  [[ -f "${path}" && ! -L "${path}" ]] || { echo "candidate release asset is missing or unsafe: ${asset}" >&2; exit 1; }
  candidate_digests["${asset}"]="$(sha256_file "${path}")"
  gh release verify-asset "${candidate_tag}" "${path}" --repo "${repository}" --format json >/dev/null
  if ! gh attestation verify "${path}" --repo "${repository}" \
      --signer-workflow "${repository}/.github/workflows/release.yml" \
      --source-ref "refs/tags/${candidate_tag}" --source-digest "${candidate_revision}" \
      --deny-self-hosted-runners --format json \
      | jq -e 'length > 0' >/dev/null; then
    echo "strict build attestation verification failed: ${asset}" >&2
    exit 1
  fi
done
for asset in SOURCE_PROVENANCE.json deployment-contract.py install.sh install-front-slots.sh "${candidate_linux_asset}"; do
  [[ "$(manifest_sha "${candidate_manifest}" "${asset}")" == "${candidate_digests[${asset}]}" ]] \
    || { echo "candidate SHA256SUMS mismatch: ${asset}" >&2; exit 1; }
done
jq -e --arg tag "${candidate_tag}" --arg revision "${candidate_revision}" \
  '(. | keys | sort) == (["source_revision","tag","tag_on_main"] | sort) and
   .tag == $tag and .source_revision == $revision and .tag_on_main == true' \
  "${candidate_dir}/SOURCE_PROVENANCE.json" >/dev/null \
  || { echo "candidate source provenance is invalid" >&2; exit 1; }
candidate_linux="${candidate_dir}/${candidate_linux_asset}"
chmod 0700 "${candidate_linux}"
verify_go_release_binary "${candidate_linux}" "${candidate_revision}"
candidate_linux_sha="${candidate_digests[${candidate_linux_asset}]}"
[[ "${candidate_linux_sha}" != "${predecessor_linux_sha}" ]] \
  || { echo "candidate and predecessor binaries must differ" >&2; exit 1; }
[[ "${candidate_linux_sha}" != "${bootstrap_linux_sha}" && "${bootstrap_linux_sha}" != "${predecessor_linux_sha}" ]] \
  || { echo "predecessor, bootstrap, and candidate binaries must differ" >&2; exit 1; }

release_verification="${private_root}/release-verification.json"
jq -n --arg schema 'subrouter.release-verification/v1' --arg tag "${candidate_tag}" \
  --arg revision "${candidate_revision}" \
  --arg sums "${candidate_digests[SHA256SUMS]}" \
  --arg provenance "${candidate_digests[SOURCE_PROVENANCE.json]}" \
  --arg deployment_contract "${candidate_digests[deployment-contract.py]}" \
  --arg installer "${candidate_digests[install.sh]}" \
  --arg front_installer "${candidate_digests[install-front-slots.sh]}" \
  --arg binary_name "${candidate_linux_asset}" --arg binary "${candidate_linux_sha}" \
  '{schema:$schema,release_tag:$tag,source_revision:$revision,tag_on_main:true,
    release_published:true,release_immutable:true,asset_digest_verified:true,
    strict_build_attestation_verified:true,provenance_verified:true,
    embedded_revision_verified:true,assets:{
      "SHA256SUMS":$sums,"SOURCE_PROVENANCE.json":$provenance,"install.sh":$installer,
      "deployment-contract.py":$deployment_contract,
      "install-front-slots.sh":$front_installer,($binary_name):$binary}}' \
  >"${release_verification}"
chmod 0600 "${release_verification}"

candidate_sha_file="${private_root}/candidate-linux.sha256"
bootstrap_sha_file="${private_root}/bootstrap-linux.sha256"
predecessor_sha_file="${private_root}/predecessor-linux.sha256"
printf '%s\n' "${candidate_linux_sha}" >"${candidate_sha_file}"
printf '%s\n' "${bootstrap_linux_sha}" >"${bootstrap_sha_file}"
printf '%s\n' "${predecessor_linux_sha}" >"${predecessor_sha_file}"

if [[ -z "${artifact_dir}" ]]; then
  artifact_dir="${root}/artifacts/golden-local-mac-continuity-$(date -u +%Y%m%dT%H%M%SZ)"
elif [[ "${artifact_dir}" != /* ]]; then
  artifact_dir="${PWD}/${artifact_dir}"
fi
mkdir -p "${artifact_dir}"
chmod 0700 "${artifact_dir}"
if [[ -e "${artifact_dir}/result.json" ]]; then
  echo "artifact directory already contains result.json" >&2
  exit 1
fi

export SUBROUTER_RELEASE_TAG="${candidate_tag}"
export SUBROUTER_RELEASE_ASSET_DIR="${candidate_dir}"
export SUBROUTER_RELEASE_VERIFICATION_JSON="${release_verification}"
export SUBROUTER_DEPLOY_BINARY="${candidate_linux}"
export SUBROUTER_RELEASE_SHA256_FILE="${candidate_sha_file}"
export SUBROUTER_DEPLOY_REVISION="${candidate_revision}"
export SUBROUTER_RELEASE_TAG_ON_MAIN=true
export SUBROUTER_RELEASE_ATTESTATION_VERIFIED=true
export SUBROUTER_RELEASE_IMMUTABLE=true
export SUBROUTER_PREDECESSOR_TAG="${predecessor_tag}"
export SUBROUTER_PREDECESSOR_BINARY="${predecessor_linux}"
export SUBROUTER_PREDECESSOR_SHA256_FILE="${predecessor_sha_file}"
export SUBROUTER_PREDECESSOR_SHA256SUMS_FILE="${predecessor_manifest}"
export SUBROUTER_PREDECESSOR_REVISION="${resolved_predecessor_revision}"
export SUBROUTER_PREDECESSOR_TAG_ON_MAIN=true
export SUBROUTER_BOOTSTRAP_TAG="${bootstrap_tag}"
export SUBROUTER_BOOTSTRAP_BINARY="${bootstrap_linux}"
export SUBROUTER_BOOTSTRAP_SHA256_FILE="${bootstrap_sha_file}"
export SUBROUTER_BOOTSTRAP_SHA256SUMS_FILE="${bootstrap_manifest}"
export SUBROUTER_BOOTSTRAP_REVISION="${resolved_bootstrap_revision}"
export SUBROUTER_BOOTSTRAP_TAG_ON_MAIN=true
export SUBROUTER_BOOTSTRAP_ATTESTATION_VERIFIED=true
export SUBROUTER_BOOTSTRAP_IMMUTABLE=true
export SUBROUTER_INSTALL_FRONT_SLOTS="${candidate_dir}/install-front-slots.sh"
export SUBROUTER_DEPLOYMENT_CONTRACT="${candidate_dir}/deployment-contract.py"
export SUBROUTER_DEPLOY_ARTIFACT_DIR="${private_root}/deploy-internal"
mkdir -p "${SUBROUTER_DEPLOY_ARTIFACT_DIR}"

if [[ "${SUBROUTER_GCP_INSTANCE}" == subrouter-staging ]]; then
  normalization_evidence="${artifact_dir}/staging-predecessor-normalization.json"
  [[ ! -e "${normalization_evidence}" ]] || { echo "staging normalization evidence already exists" >&2; exit 1; }
  "${root}/deploy/gcp/normalize-staging-predecessor.sh" --evidence-json "${normalization_evidence}"
  normalization_before="$(sha256_file "${normalization_evidence}")"
  python3 "${root}/deploy/gcp/validate-deploy-evidence.py" \
    --expect staging-predecessor-normalization "${normalization_evidence}" >/dev/null
  normalization_after="$(sha256_file "${normalization_evidence}")"
  [[ "${normalization_before}" == "${normalization_after}" ]] \
    || { echo "staging normalization evidence changed during validation" >&2; exit 1; }
  chmod 0600 "${normalization_evidence}"
fi

observer="${private_root}/subrouter-transport-observer"
(
  cd "${root}"
  go build -trimpath -o "${observer}" ./cmd/subrouter-transport-observer
)

"${observer}" golden \
  --predecessor-version "${predecessor_tag}" \
  --predecessor-sha256 "${predecessor_darwin_sha}" \
  --predecessor-client "${predecessor_darwin}" \
  --candidate-tag "${candidate_tag}" \
  --candidate-sha256 "${candidate_linux_sha}" \
  --candidate-revision "${candidate_revision}" \
  --deploy-evidence-validator "${root}/deploy/gcp/validate-deploy-evidence.py" \
  --artifact-dir "${artifact_dir}" \
  "${golden_args[@]}" \
  --migration-prepare "${root}/deploy/gcp/migrate-to-front-slots.sh" \
  --migration-switch "${root}/deploy/gcp/switch-front-migration.sh" \
  --legacy-retirement "${root}/deploy/gcp/finalize-legacy-retirement.sh" \
  --activate "${root}/deploy/gcp/deploy-live-upgrade.sh" \
  --rollback "${root}/deploy/gcp/rollback-slot.sh" \
  --old-generation-check "${root}/deploy/gcp/finalize-slot-retirement.sh"
