#!/usr/bin/env bash
# One-time, operator-explicit migration from the legacy LB port to a stable
# front process backed by replaceable loopback supervisor slots.
set -euo pipefail

usage() {
  cat <<'EOF'
Usage: migrate-to-front-slots.sh [--evidence-json PATH]

Prepares and verifies the front, private slot, health check, backend, firewall,
and named port, then pre-warms the front through an unadvertised canary host
without changing the active user route. Continue with
switch-front-migration.sh so cutover, rollback, and final cutover remain
separately observable by the external golden gate.
EOF
}

EVIDENCE_JSON=""
while (( $# > 0 )); do
  case "$1" in
    --evidence-json) (( $# >= 2 )) || { usage >&2; exit 2; }; EVIDENCE_JSON="$2"; shift 2 ;;
    --help|-h) usage; exit 0 ;;
    *) usage >&2; exit 2 ;;
  esac
done

PROJECT_ID="${SUBROUTER_GCP_PROJECT:?set SUBROUTER_GCP_PROJECT}"
ZONE="${SUBROUTER_GCP_ZONE:?set SUBROUTER_GCP_ZONE}"
INSTANCE="${SUBROUTER_GCP_INSTANCE:?set SUBROUTER_GCP_INSTANCE}"
RELEASE_TAG="${SUBROUTER_RELEASE_TAG:?set SUBROUTER_RELEASE_TAG}"
DEPLOY_BINARY="${SUBROUTER_DEPLOY_BINARY:?set SUBROUTER_DEPLOY_BINARY}"
RELEASE_SHA256_FILE="${SUBROUTER_RELEASE_SHA256_FILE:-${DEPLOY_BINARY}.sha256}"
PREDECESSOR_TAG="${SUBROUTER_PREDECESSOR_TAG:?set SUBROUTER_PREDECESSOR_TAG}"
PREDECESSOR_BINARY="${SUBROUTER_PREDECESSOR_BINARY:?set SUBROUTER_PREDECESSOR_BINARY to the separately verified worker asset}"
PREDECESSOR_SHA256_FILE="${SUBROUTER_PREDECESSOR_SHA256_FILE:-${PREDECESSOR_BINARY}.sha256}"
PREDECESSOR_SHA256SUMS_FILE="${SUBROUTER_PREDECESSOR_SHA256SUMS_FILE:?set SUBROUTER_PREDECESSOR_SHA256SUMS_FILE to the downloaded v0.1.51 checksum manifest}"
PREDECESSOR_REVISION="${SUBROUTER_PREDECESSOR_REVISION:?set SUBROUTER_PREDECESSOR_REVISION to the verified predecessor tag commit}"
PREDECESSOR_TAG_ON_MAIN="${SUBROUTER_PREDECESSOR_TAG_ON_MAIN:?set SUBROUTER_PREDECESSOR_TAG_ON_MAIN from the predecessor ancestry gate}"
BOOTSTRAP_TAG="${SUBROUTER_BOOTSTRAP_TAG:?set SUBROUTER_BOOTSTRAP_TAG}"
BOOTSTRAP_BINARY="${SUBROUTER_BOOTSTRAP_BINARY:?set SUBROUTER_BOOTSTRAP_BINARY to the verified lease-capable worker asset}"
BOOTSTRAP_SHA256_FILE="${SUBROUTER_BOOTSTRAP_SHA256_FILE:-${BOOTSTRAP_BINARY}.sha256}"
BOOTSTRAP_SHA256SUMS_FILE="${SUBROUTER_BOOTSTRAP_SHA256SUMS_FILE:?set SUBROUTER_BOOTSTRAP_SHA256SUMS_FILE}"
BOOTSTRAP_REVISION="${SUBROUTER_BOOTSTRAP_REVISION:?set SUBROUTER_BOOTSTRAP_REVISION}"
BOOTSTRAP_TAG_ON_MAIN="${SUBROUTER_BOOTSTRAP_TAG_ON_MAIN:?set SUBROUTER_BOOTSTRAP_TAG_ON_MAIN}"
BOOTSTRAP_ATTESTATION_VERIFIED="${SUBROUTER_BOOTSTRAP_ATTESTATION_VERIFIED:?set SUBROUTER_BOOTSTRAP_ATTESTATION_VERIFIED}"
BOOTSTRAP_IMMUTABLE="${SUBROUTER_BOOTSTRAP_IMMUTABLE:?set SUBROUTER_BOOTSTRAP_IMMUTABLE}"
PUBLIC_BASE_URL="${SUBROUTER_PUBLIC_BASE_URL:?set SUBROUTER_PUBLIC_BASE_URL}"
CLOUD_CONFIG="${SUBROUTER_CLOUD_CONFIG:?set SUBROUTER_CLOUD_CONFIG}"
DEPLOY_REVISION="${SUBROUTER_DEPLOY_REVISION:?set SUBROUTER_DEPLOY_REVISION to the verified tag commit}"
TAG_ON_MAIN="${SUBROUTER_RELEASE_TAG_ON_MAIN:?set SUBROUTER_RELEASE_TAG_ON_MAIN from the ancestry gate}"
ATTESTATION_VERIFIED="${SUBROUTER_RELEASE_ATTESTATION_VERIFIED:?set SUBROUTER_RELEASE_ATTESTATION_VERIFIED from gh attestation verify}"
RELEASE_IMMUTABLE="${SUBROUTER_RELEASE_IMMUTABLE:?set SUBROUTER_RELEASE_IMMUTABLE from gh release view and verify-asset}"
GCLOUD_BINARY="${GCLOUD_BIN:-gcloud}"
FRONT_HEALTH_CHECK="${SUBROUTER_GCP_FRONT_HEALTH_CHECK:-subrouter-front-hc}"
FIREWALL_RULE="${SUBROUTER_GCP_LB_FIREWALL_RULE:-subrouter-allow-lb}"
URL_MAP="${SUBROUTER_GCP_URL_MAP:-subrouter-urlmap}"
DEPLOY_LOCK_FILE="${SUBROUTER_DEPLOY_LOCK_FILE:-/run/lock/subrouter-deploy.lock}"
ARTIFACT_DIR="${SUBROUTER_DEPLOY_ARTIFACT_DIR:-${PWD}/artifacts/gcp-front-migration}"
RUN_LABEL="${GITHUB_RUN_ID:-local}-${GITHUB_RUN_ATTEMPT:-0}-$$"
RUN_LABEL="${RUN_LABEL//[^a-zA-Z0-9._-]/-}"
REMOTE_CANDIDATE="/tmp/subrouter-front-migration-${RUN_LABEL}"
REMOTE_WORKER_CANDIDATE="/tmp/subrouter-front-worker-${RUN_LABEL}"
REMOTE_INSTALLER="/tmp/install-front-slots-${RUN_LABEL}.sh"
REMOTE_DEPLOYMENT_CONTRACT="/tmp/deployment-contract-${RUN_LABEL}.py"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
FRONT_READINESS_WAITER="${SCRIPT_DIR}/wait-for-front-readiness.py"
FRONT_READINESS_PROBE="${SCRIPT_DIR}/probe-front-readiness.sh"
CANARY_SECURITY_POLICY_HELPER="${SCRIPT_DIR}/canary-security-policy.py"
URL_MAP_ROUTING="${SCRIPT_DIR}/url-map-routing.py"
# shellcheck source=deploy/gcp/stream-shell-value.sh
source "${SCRIPT_DIR}/stream-shell-value.sh"
# shellcheck source=deploy/gcp/deploy-lock.sh
source "${SCRIPT_DIR}/deploy-lock.sh"

case "${INSTANCE}" in
  subrouter-team)
    LEGACY_BACKEND_SERVICE="${SUBROUTER_GCP_BACKEND_SERVICE:-subrouter-backend}"
    FRONT_BACKEND_SERVICE="${SUBROUTER_GCP_FRONT_BACKEND_SERVICE:-subrouter-front-backend}"
    INSTANCE_GROUP="${SUBROUTER_GCP_INSTANCE_GROUP:-subrouter-ig}"
    ACTIVE_MATCHER="__root__"
    CANARY_MATCHER="subrouter-front-canary"
    CANARY_HOST="front-canary.sr.cmux.internal"
    CANARY_SECURITY_POLICY="subrouter-front-canary-policy"
    ;;
  subrouter-staging)
    LEGACY_BACKEND_SERVICE="${SUBROUTER_GCP_BACKEND_SERVICE:-subrouter-staging-backend}"
    FRONT_BACKEND_SERVICE="${SUBROUTER_GCP_FRONT_BACKEND_SERVICE:-subrouter-staging-front-backend}"
    INSTANCE_GROUP="${SUBROUTER_GCP_INSTANCE_GROUP:-subrouter-staging-ig}"
    ACTIVE_MATCHER="staging-subrouter"
    CANARY_MATCHER="staging-subrouter-front-canary"
    CANARY_HOST="front-canary.staging.sr.cmux.internal"
    CANARY_SECURITY_POLICY="subrouter-staging-front-canary-policy"
    ;;
  *)
    printf 'gcp-front-migration: unsupported instance: %s\n' "${INSTANCE}" >&2
    exit 1
    ;;
esac

log() { printf 'gcp-front-migration: %s\n' "$*"; }
die() { log "$*" >&2; exit 1; }
INSTALL_FRONT_SLOTS="$(bash "${SCRIPT_DIR}/resolve-release-installer.sh" "${SCRIPT_DIR}/install-front-slots.sh")"
DEPLOYMENT_CONTRACT="$(bash "${SCRIPT_DIR}/resolve-release-contract.sh" "${SCRIPT_DIR}/deployment-contract.py")"
REMOTE_INSTALL_COMMAND="sudo env SUBROUTER_DEPLOYMENT_CONTRACT='${REMOTE_DEPLOYMENT_CONTRACT}' bash '${REMOTE_INSTALLER}'"

for command in "${GCLOUD_BINARY}" curl go jq python3 sha256sum; do
  command -v "${command}" >/dev/null 2>&1 || die "required command not found: ${command}"
done
[[ -f "${FRONT_READINESS_WAITER}" ]] || die "front readiness waiter is missing"
[[ -f "${FRONT_READINESS_PROBE}" ]] || die "front readiness probe is missing"
[[ -f "${CANARY_SECURITY_POLICY_HELPER}" ]] || die "canary security policy helper is missing"
[[ -f "${URL_MAP_ROUTING}" ]] || die "URL-map routing helper is missing"
[[ -f "${CLOUD_CONFIG}" && ! -L "${CLOUD_CONFIG}" ]] || die "cloud config is missing or unsafe"
tenant_key="$(jq -r '.tenantKey // empty' "${CLOUD_CONFIG}")"
[[ "${tenant_key}" =~ ^srt_[A-Za-z0-9_-]{16,}$ ]] || die "cloud config tenant key is invalid"
INSTALL_FRONT_SLOTS_SHA256="$(sha256sum "${INSTALL_FRONT_SLOTS}" | awk '{print $1}')"
DEPLOYMENT_CONTRACT_SHA256="$(sha256sum "${DEPLOYMENT_CONTRACT}" | awk '{print $1}')"
[[ "${RELEASE_TAG}" =~ ^v[0-9]+\.[0-9]+\.[0-9]+([.-][0-9A-Za-z.-]+)?$ ]] \
  || die "SUBROUTER_RELEASE_TAG must be an explicit version tag"
[[ -x "${DEPLOY_BINARY}" ]] || die "release binary is not executable: ${DEPLOY_BINARY}"
[[ -f "${RELEASE_SHA256_FILE}" ]] || die "release checksum file is missing: ${RELEASE_SHA256_FILE}"
EXPECTED_SHA256="$(tr -d '[:space:]' <"${RELEASE_SHA256_FILE}")"
[[ "${EXPECTED_SHA256}" =~ ^[0-9a-f]{64}$ ]] || die "release checksum is invalid"
[[ "$(sha256sum "${DEPLOY_BINARY}" | awk '{print $1}')" == "${EXPECTED_SHA256}" ]] \
  || die "release binary does not match its verified checksum"
[[ "${PREDECESSOR_TAG}" == v0.1.51 ]] || die "historical predecessor must be the operator-pinned v0.1.51 release"
[[ -x "${PREDECESSOR_BINARY}" ]] || die "predecessor binary is not executable: ${PREDECESSOR_BINARY}"
[[ -f "${PREDECESSOR_SHA256_FILE}" ]] || die "predecessor checksum file is missing: ${PREDECESSOR_SHA256_FILE}"
PREDECESSOR_SHA256="$(tr -d '[:space:]' <"${PREDECESSOR_SHA256_FILE}")"
[[ "${PREDECESSOR_SHA256}" =~ ^[0-9a-f]{64}$ ]] || die "predecessor checksum is invalid"
[[ "${PREDECESSOR_SHA256}" == 99fcd10d912184c160370eb228b382795101f2b5b2467244f995aa2d10b0c323 ]] \
  || die "predecessor Linux bytes do not match the compiled-in v0.1.51 hard pin"
[[ -f "${PREDECESSOR_SHA256SUMS_FILE}" ]] || die "predecessor SHA256SUMS manifest is missing"
manifest_predecessor_sha="$(awk '$2 == "subrouter_0.1.51_linux_amd64" {print $1}' "${PREDECESSOR_SHA256SUMS_FILE}")"
[[ "${manifest_predecessor_sha}" == "${PREDECESSOR_SHA256}" ]] \
  || die "predecessor SHA256SUMS does not match the hard-pinned Linux asset"
[[ "$(sha256sum "${PREDECESSOR_BINARY}" | awk '{print $1}')" == "${PREDECESSOR_SHA256}" ]] \
  || die "predecessor binary does not match its verified checksum"
[[ "${BOOTSTRAP_TAG}" == v0.1.60 ]] || die "migration bootstrap must be the operator-pinned v0.1.60 release"
[[ -x "${BOOTSTRAP_BINARY}" ]] || die "bootstrap binary is not executable: ${BOOTSTRAP_BINARY}"
[[ -f "${BOOTSTRAP_SHA256_FILE}" ]] || die "bootstrap checksum file is missing: ${BOOTSTRAP_SHA256_FILE}"
BOOTSTRAP_SHA256="$(tr -d '[:space:]' <"${BOOTSTRAP_SHA256_FILE}")"
[[ "${BOOTSTRAP_SHA256}" == 6a8daa1361030311bdbe25a06cd4940e4dd07a45758c13c2dc8d687e70d87303 ]] \
  || die "bootstrap Linux bytes do not match the compiled-in v0.1.60 hard pin"
[[ -f "${BOOTSTRAP_SHA256SUMS_FILE}" ]] || die "bootstrap SHA256SUMS manifest is missing"
manifest_bootstrap_sha="$(awk '$2 == "subrouter_0.1.60_linux_amd64" {print $1}' "${BOOTSTRAP_SHA256SUMS_FILE}")"
[[ "${manifest_bootstrap_sha}" == "${BOOTSTRAP_SHA256}" ]] \
  || die "bootstrap SHA256SUMS does not match the hard-pinned Linux asset"
[[ "$(sha256sum "${BOOTSTRAP_BINARY}" | awk '{print $1}')" == "${BOOTSTRAP_SHA256}" ]] \
  || die "bootstrap binary does not match its verified checksum"
[[ "${PREDECESSOR_SHA256}" != "${BOOTSTRAP_SHA256}" && "${BOOTSTRAP_SHA256}" != "${EXPECTED_SHA256}" && "${PREDECESSOR_SHA256}" != "${EXPECTED_SHA256}" ]] \
  || die "predecessor, bootstrap worker, and control release must differ"
[[ "${DEPLOY_REVISION}" =~ ^[0-9a-f]{40}$ ]] || die "SUBROUTER_DEPLOY_REVISION must be a full verified commit"
bash "${SCRIPT_DIR}/verify-go-release-binary.sh" "${DEPLOY_BINARY}" "${DEPLOY_REVISION}" \
  || die "candidate embedded metadata is invalid"
[[ "${PREDECESSOR_REVISION}" == 5eacb5411c0bd4a24f4e422d6366fa7bfd1843c8 ]] \
  || die "predecessor revision does not match the compiled-in v0.1.51 hard pin"
bash "${SCRIPT_DIR}/verify-go-release-binary.sh" \
  "${PREDECESSOR_BINARY}" 5eacb5411c0bd4a24f4e422d6366fa7bfd1843c8 \
  || die "predecessor embedded metadata is invalid"
[[ "${BOOTSTRAP_REVISION}" == e169e94f2bea9a0455a5831631fcbac220bd65f2 ]] \
  || die "bootstrap revision does not match the compiled-in v0.1.60 hard pin"
bash "${SCRIPT_DIR}/verify-go-release-binary.sh" "${BOOTSTRAP_BINARY}" "${BOOTSTRAP_REVISION}" \
  || die "bootstrap embedded metadata is invalid"
[[ "${TAG_ON_MAIN}" == true ]] || die "release tag commit was not proven to be on main"
[[ "${ATTESTATION_VERIFIED}" == true ]] || die "release artifact attestation was not verified"
[[ "${RELEASE_IMMUTABLE}" == true ]] || die "release was not proven published and immutable"
[[ "${PREDECESSOR_TAG_ON_MAIN}" == true ]] || die "predecessor tag commit was not proven to be on main"
[[ "${BOOTSTRAP_TAG_ON_MAIN}" == true ]] || die "bootstrap tag commit was not proven to be on main"
[[ "${BOOTSTRAP_ATTESTATION_VERIFIED}" == true ]] || die "bootstrap artifact attestation was not verified"
[[ "${BOOTSTRAP_IMMUTABLE}" == true ]] || die "bootstrap release was not proven published and immutable"
[[ "${PUBLIC_BASE_URL}" =~ ^https://[^/?#]+/?$ ]] \
  || die "SUBROUTER_PUBLIC_BASE_URL must be an HTTPS origin"
PUBLIC_BASE_URL="${PUBLIC_BASE_URL%/}"

mkdir -p "${ARTIFACT_DIR}"
ARTIFACT_DIR="$(cd "${ARTIFACT_DIR}" && pwd)"
EVIDENCE_JSON="${EVIDENCE_JSON:-${SUBROUTER_DEPLOY_EVIDENCE_JSON:-${ARTIFACT_DIR}/preparation.json}}"
mkdir -p "$(dirname "${EVIDENCE_JSON}")"
EVIDENCE_JSON="$(cd "$(dirname "${EVIDENCE_JSON}")" && pwd)/$(basename "${EVIDENCE_JSON}")"

gcloud_ssh() {
  "${GCLOUD_BINARY}" compute ssh "${INSTANCE}" \
    --project "${PROJECT_ID}" --zone "${ZONE}" --tunnel-through-iap --quiet \
    --command "$1"
}

gcloud_scp() {
  "${GCLOUD_BINARY}" compute scp "$1" "${INSTANCE}:$2" \
    --project "${PROJECT_ID}" --zone "${ZONE}" --tunnel-through-iap --quiet
}

utc_now() { python3 -c 'from datetime import datetime, timezone; print(datetime.now(timezone.utc).isoformat(timespec="milliseconds").replace("+00:00", "Z"))'; }
epoch_seconds() { date +%s; }
canary_sessions_observed() {
  local sessions_file="$1" expected_count="$2" service="subrouter-slot@${front_slot}.service"
  [[ "${expected_count}" =~ ^[0-9]+$ ]] || return 1
  (( expected_count >= 21 )) || return 1
  [[ "$(wc -l <"${sessions_file}" | tr -d '[:space:]')" == "${expected_count}" ]] || return 1
  awk 'NF == 0 || $0 !~ /^[A-Za-z0-9._-]+$/ {exit 1}' "${sessions_file}" || return 1
  "${GCLOUD_BINARY}" compute ssh "${INSTANCE}" \
    --project "${PROJECT_ID}" --zone "${ZONE}" --tunnel-through-iap --quiet \
    --command "set -eu
sessions_file=\$(mktemp)
journal_file=\$(mktemp)
trap 'rm -f \"\${sessions_file}\" \"\${journal_file}\"' EXIT INT TERM
cat >\"\${sessions_file}\"
sudo journalctl --sync
sudo journalctl --unit='${service}' --since='@${map_updated_epoch}' --no-pager --output=cat >\"\${journal_file}\"
count=0
while IFS= read -r session_id; do
  case \"\${session_id}\" in ''|*[!A-Za-z0-9._-]*) exit 1 ;; esac
  grep -F 'INFO proxy request agent=codex' \"\${journal_file}\" | grep -F 'method=POST path=/v1/responses' | grep -F -e \"session=\${session_id} \" -e \"session=\${session_id}:\" -e \"session=\\\"\${session_id}\\\" \" -e \"session=\\\"\${session_id}:\" >/dev/null
  count=\$((count + 1))
done <\"\${sessions_file}\"
test \"\${count}\" -eq '${expected_count}'
session_set_sha256=\$(sha256sum \"\${sessions_file}\" | cut -d ' ' -f 1)
printf '{\"journal_correlated_samples\":%s,\"session_set_sha256\":\"%s\"}\\n' \"\${count}\" \"\${session_set_sha256}\"" \
    <"${sessions_file}" 2>>"${ARTIFACT_DIR}/canary-journal-ssh.log"
}

lock_holder_pid=""
acquire_deploy_lock() {
  subrouter_acquire_deploy_lock "${ARTIFACT_DIR}/deploy-lock.log" \
    "${GCLOUD_BINARY}" "${INSTANCE}" "${PROJECT_ID}" "${ZONE}" "${DEPLOY_LOCK_FILE}" "${RUN_LABEL}" \
    || die "could not acquire ${DEPLOY_LOCK_FILE}"
}

release_deploy_lock() {
  subrouter_release_deploy_lock
}

old_drain_timeout=""
migration_started=0
migration_committed=0
url_map_switched=0
canary_token_file=""
canary_policy_config_file=""

install_canary_access_boundary() {
  local policy_exists=0 current_policy expected_policy_url policy_json backend_policy_json token_sha policy_fingerprint=""
  canary_token_file="$(mktemp "${ARTIFACT_DIR}/.front-canary-token.XXXXXX")"
  chmod 0600 "${canary_token_file}"
  python3 -c 'import secrets, sys; print(secrets.token_hex(32), file=sys.stdout)' >"${canary_token_file}"
  canary_policy_config_file="$(mktemp "${ARTIFACT_DIR}/.front-canary-policy.XXXXXX.json")"
  chmod 0600 "${canary_policy_config_file}"
  if policy_json="$("${GCLOUD_BINARY}" compute security-policies describe "${CANARY_SECURITY_POLICY}" \
    --project "${PROJECT_ID}" --global --format=json 2>/dev/null)"
  then
    policy_exists=1
    policy_fingerprint="$(jq -er '.fingerprint | select(type=="string" and length>0)' \
      < <(stream_shell_value "${policy_json}"))" \
      || die "existing canary security policy has no update fingerprint"
    python3 "${CANARY_SECURITY_POLICY_HELPER}" render \
      "${canary_policy_config_file}" "${CANARY_SECURITY_POLICY}" "${CANARY_HOST}" \
      --token-file "${canary_token_file}" --fingerprint "${policy_fingerprint}"
  else
    python3 "${CANARY_SECURITY_POLICY_HELPER}" render \
      "${canary_policy_config_file}" "${CANARY_SECURITY_POLICY}" "${CANARY_HOST}" \
      --token-file "${canary_token_file}"
  fi
  if [[ "${policy_exists}" == 1 ]]; then
    "${GCLOUD_BINARY}" compute security-policies import "${CANARY_SECURITY_POLICY}" \
      --project "${PROJECT_ID}" --global --file-name "${canary_policy_config_file}" \
      --file-format json --quiet
  else
    "${GCLOUD_BINARY}" compute security-policies create "${CANARY_SECURITY_POLICY}" \
      --project "${PROJECT_ID}" --global --file-name "${canary_policy_config_file}" \
      --file-format json --quiet
  fi
  rm -f -- "${canary_policy_config_file}"
  canary_policy_config_file=""
  policy_json="$("${GCLOUD_BINARY}" compute security-policies describe "${CANARY_SECURITY_POLICY}" \
    --project "${PROJECT_ID}" --global --format=json)"
  canary_policy_evidence="$(printf '%s\n' "${policy_json}" | python3 "${CANARY_SECURITY_POLICY_HELPER}" \
    assert-ready - "${CANARY_SECURITY_POLICY}" "${CANARY_HOST}" --token-file "${canary_token_file}")"
  token_sha="$(python3 -c 'import hashlib, sys; print(hashlib.sha256(open(sys.argv[1], "rb").read().strip()).hexdigest())' "${canary_token_file}")"
  [[ "$(jq -r '.key_fingerprint_sha256' < <(stream_shell_value "${canary_policy_evidence}"))" == "${token_sha}" ]] \
    || die "canary security policy token proof does not match its protected token file"

  expected_policy_url="https://www.googleapis.com/compute/v1/projects/${PROJECT_ID}/global/securityPolicies/${CANARY_SECURITY_POLICY}"
  backend_policy_json="$("${GCLOUD_BINARY}" compute backend-services describe "${FRONT_BACKEND_SERVICE}" \
    --project "${PROJECT_ID}" --global --format=json)"
  current_policy="$(jq -r '.securityPolicy // empty' < <(stream_shell_value "${backend_policy_json}"))"
  [[ -z "${current_policy}" || "${current_policy}" == "${expected_policy_url}" ]] \
    || die "${FRONT_BACKEND_SERVICE} has an unrelated security policy"
  "${GCLOUD_BINARY}" compute backend-services update "${FRONT_BACKEND_SERVICE}" \
    --project "${PROJECT_ID}" --global --security-policy "${CANARY_SECURITY_POLICY}" --quiet
  backend_policy_json="$("${GCLOUD_BINARY}" compute backend-services describe "${FRONT_BACKEND_SERVICE}" \
    --project "${PROJECT_ID}" --global --format=json)"
  [[ "$(jq -r '.securityPolicy // empty' < <(stream_shell_value "${backend_policy_json}"))" == "${expected_policy_url}" ]] \
    || die "canary security policy was not attached to ${FRONT_BACKEND_SERVICE}"
}

rollback_lb() {
  log "restoring the URL map to the legacy backend"
  if [[ "${url_map_switched}" == "1" ]]; then
    "${GCLOUD_BINARY}" compute url-maps import "${URL_MAP}" \
      --project "${PROJECT_ID}" --global \
      --source "${ARTIFACT_DIR}/url-map-before.yaml" --quiet
  fi
  "${GCLOUD_BINARY}" compute backend-services update "${LEGACY_BACKEND_SERVICE}" \
    --project "${PROJECT_ID}" --global --connection-draining-timeout "${old_drain_timeout}s"
}

cleanup() {
  local status=$?
  trap - EXIT INT TERM
  set +e
  if [[ "${migration_started}" == "1" && "${migration_committed}" == "0" ]]; then
    rollback_lb || status=1
  fi
  [[ -z "${canary_token_file}" ]] || rm -f -- "${canary_token_file}"
  [[ -z "${canary_policy_config_file}" ]] || rm -f -- "${canary_policy_config_file}"
  gcloud_ssh "rm -f '${REMOTE_CANDIDATE}' '${REMOTE_WORKER_CANDIDATE}' '${REMOTE_INSTALLER}' '${REMOTE_DEPLOYMENT_CONTRACT}'" >/dev/null 2>&1 || true
  release_deploy_lock
  exit "${status}"
}
trap cleanup EXIT INT TERM

curl -fsS --max-time 10 "${PUBLIC_BASE_URL}/_subrouter/health" >/dev/null \
  || die "legacy public health check failed before migration"
curl -fsS --max-time 10 "${PUBLIC_BASE_URL}/_subrouter/ready" >/dev/null \
  || die "legacy public readiness check failed before migration"

acquire_deploy_lock
backend_json="$("${GCLOUD_BINARY}" compute backend-services describe "${LEGACY_BACKEND_SERVICE}" \
  --project "${PROJECT_ID}" --global --format=json)"
group_json="$("${GCLOUD_BINARY}" compute instance-groups describe "${INSTANCE_GROUP}" \
  --project "${PROJECT_ID}" --zone "${ZONE}" --format=json)"
printf '%s\n' "${backend_json}" >"${ARTIFACT_DIR}/backend-before.json"
printf '%s\n' "${group_json}" >"${ARTIFACT_DIR}/instance-group-before.json"
"${GCLOUD_BINARY}" compute url-maps export "${URL_MAP}" \
  --project "${PROJECT_ID}" --global \
  --destination "${ARTIFACT_DIR}/url-map-before.yaml" --quiet
legacy_backend_url="https://www.googleapis.com/compute/v1/projects/${PROJECT_ID}/global/backendServices/${LEGACY_BACKEND_SERVICE}"
front_backend_url="https://www.googleapis.com/compute/v1/projects/${PROJECT_ID}/global/backendServices/${FRONT_BACKEND_SERVICE}"
active_backend_url="$(python3 "${URL_MAP_ROUTING}" active-backend \
  "${ARTIFACT_DIR}/url-map-before.yaml" "${ACTIVE_MATCHER}")"
[[ "${active_backend_url}" == "${legacy_backend_url}" ]] \
  || die "${URL_MAP} already routes this environment to ${FRONT_BACKEND_SERVICE}; migration is complete"

http_named_port_count="$(jq -r '[.namedPorts[]? | select(.name == "http" and .port == 31415)] | length' < <(stream_shell_value "${group_json}"))"
front_named_port="$(jq -r '[.namedPorts[]? | select(.name == "front") | .port][0] // empty' < <(stream_shell_value "${group_json}"))"
old_drain_timeout="$(jq -r '.connectionDraining.drainingTimeoutSec // 0' < <(stream_shell_value "${backend_json}"))"
[[ "${http_named_port_count}" == "1" ]] || die "expected exactly one legacy named port http:31415"
[[ -z "${front_named_port}" || "${front_named_port}" == "31416" ]] \
  || die "existing front named port is ${front_named_port}, expected 31416"
[[ "${old_drain_timeout}" =~ ^[0-9]+$ ]] || die "invalid existing connection-draining timeout"

gcloud_scp "${DEPLOY_BINARY}" "${REMOTE_CANDIDATE}"
gcloud_scp "${BOOTSTRAP_BINARY}" "${REMOTE_WORKER_CANDIDATE}"
gcloud_scp "${INSTALL_FRONT_SLOTS}" "${REMOTE_INSTALLER}"
gcloud_scp "${DEPLOYMENT_CONTRACT}" "${REMOTE_DEPLOYMENT_CONTRACT}"
gcloud_ssh "printf '%s  %s\n%s  %s\n' '${INSTALL_FRONT_SLOTS_SHA256}' '${REMOTE_INSTALLER}' '${DEPLOYMENT_CONTRACT_SHA256}' '${REMOTE_DEPLOYMENT_CONTRACT}' | sha256sum -c - >/dev/null"
gcloud_ssh "set -e; printf '%s  %s\n%s  %s\n' '${EXPECTED_SHA256}' '${REMOTE_CANDIDATE}' '${BOOTSTRAP_SHA256}' '${REMOTE_WORKER_CANDIDATE}' | sha256sum -c - >/dev/null; ${REMOTE_INSTALL_COMMAND} install-release '${RELEASE_TAG}' '${REMOTE_CANDIDATE}' '${EXPECTED_SHA256}'; ${REMOTE_INSTALL_COMMAND} install-release '${BOOTSTRAP_TAG}' '${REMOTE_WORKER_CANDIDATE}' '${BOOTSTRAP_SHA256}'; ${REMOTE_INSTALL_COMMAND} ensure-migration-topology '${RELEASE_TAG}' '${BOOTSTRAP_TAG}' slot-a"
gcloud_ssh "set -e; status=\$(sudo curl -fsS --unix-socket /var/lib/subrouter/front.sock http://localhost/_subrouter/front-status); slot=\$(printf '%s' \"\${status}\" | jq -r '.active.id // empty'); case \"\${slot}\" in slot-a|slot-b) ;; *) exit 1 ;; esac; test \"\$(sudo sha256sum /opt/subrouter/front/subrouter | awk '{print \$1}')\" = '${EXPECTED_SHA256}'; test \"\$(sudo sha256sum /opt/subrouter/control/subrouter | awk '{print \$1}')\" = '${EXPECTED_SHA256}'; test \"\$(sudo sha256sum /opt/subrouter/slots/\${slot}/worker | awk '{print \$1}')\" = '${BOOTSTRAP_SHA256}'; curl -fsS http://127.0.0.1:31416/_subrouter/health >/dev/null; curl -fsS http://127.0.0.1:31416/_subrouter/ready >/dev/null; systemctl is-active --quiet subrouter.service; systemctl is-active --quiet subrouter-front.service; systemctl is-active --quiet subrouter-slot@\${slot}.service; systemctl is-enabled --quiet subrouter-front.service; systemctl is-enabled --quiet subrouter-slot@\${slot}.service" \
  || die "front topology does not match the verified migration release"
curl -fsS --max-time 10 "${PUBLIC_BASE_URL}/_subrouter/health" >/dev/null \
  || die "legacy public health failed after front topology reconciliation"
curl -fsS --max-time 10 "${PUBLIC_BASE_URL}/_subrouter/ready" >/dev/null \
  || die "legacy public readiness failed after front topology reconciliation"

migration_started=1
log "setting one-hour connection draining before the named-port cutover"
"${GCLOUD_BINARY}" compute backend-services update "${LEGACY_BACKEND_SERVICE}" \
  --project "${PROJECT_ID}" --global --connection-draining-timeout 3600s
verified_drain="$("${GCLOUD_BINARY}" compute backend-services describe "${LEGACY_BACKEND_SERVICE}" \
  --project "${PROJECT_ID}" --global --format='value(connectionDraining.drainingTimeoutSec)')"
[[ "${verified_drain}" == "3600" ]] || die "backend connection draining did not become 3600 seconds"

if "${GCLOUD_BINARY}" compute health-checks describe "${FRONT_HEALTH_CHECK}" \
  --project "${PROJECT_ID}" --global >/dev/null 2>&1; then
  front_hc_json="$("${GCLOUD_BINARY}" compute health-checks describe "${FRONT_HEALTH_CHECK}" \
    --project "${PROJECT_ID}" --global --format=json)"
  jq -e '.type == "HTTP" and .httpHealthCheck.port == 31416 and .httpHealthCheck.portSpecification == "USE_FIXED_PORT" and .httpHealthCheck.requestPath == "/_subrouter/ready"' \
    < <(stream_shell_value "${front_hc_json}") >/dev/null \
    || die "existing ${FRONT_HEALTH_CHECK} does not probe fixed port 31416 readiness"
else
  "${GCLOUD_BINARY}" compute health-checks create http "${FRONT_HEALTH_CHECK}" \
    --project "${PROJECT_ID}" --global --port 31416 \
    --request-path /_subrouter/ready --check-interval 15s --timeout 5s \
    --healthy-threshold 1 --unhealthy-threshold 3
fi

"${GCLOUD_BINARY}" compute firewall-rules update "${FIREWALL_RULE}" \
  --project "${PROJECT_ID}" --allow tcp:31415,tcp:31416 >/dev/null

# `set-named-ports` replaces the full list, so carry forward every unrelated
# entry while adding front:31416 and preserving http:31415. The legacy backend
# and every established connection remain addressable through rollback.
desired_named_ports="$(jq -r '
  ((.namedPorts // []) | map(select(.name != "front"))) + [{name:"front",port:31416}]
  | map(.name + ":" + (.port | tostring))
  | join(",")
' < <(stream_shell_value "${group_json}"))"
"${GCLOUD_BINARY}" compute instance-groups set-named-ports "${INSTANCE_GROUP}" \
  --project "${PROJECT_ID}" --zone "${ZONE}" \
  --named-ports "${desired_named_ports}"
updated_group_json="$("${GCLOUD_BINARY}" compute instance-groups describe "${INSTANCE_GROUP}" \
  --project "${PROJECT_ID}" --zone "${ZONE}" --format=json)"
jq -e '.namedPorts | any(.name == "http" and .port == 31415) and any(.name == "front" and .port == 31416)' \
  < <(stream_shell_value "${updated_group_json}") >/dev/null || die "instance-group named ports were not preserved"

if ! "${GCLOUD_BINARY}" compute backend-services describe "${FRONT_BACKEND_SERVICE}" \
    --project "${PROJECT_ID}" --global >/dev/null 2>&1; then
  "${GCLOUD_BINARY}" compute backend-services create "${FRONT_BACKEND_SERVICE}" \
    --project "${PROJECT_ID}" --global --load-balancing-scheme EXTERNAL \
    --protocol HTTP --port-name front --health-checks "${FRONT_HEALTH_CHECK}" \
    --global-health-checks --timeout 3600s --connection-draining-timeout 3600s
fi
front_backend_json="$("${GCLOUD_BINARY}" compute backend-services describe "${FRONT_BACKEND_SERVICE}" \
  --project "${PROJECT_ID}" --global --format=json)"
jq -e --arg hc "${FRONT_HEALTH_CHECK}" --arg group "${INSTANCE_GROUP}" \
  '.portName == "front" and .protocol == "HTTP" and .loadBalancingScheme == "EXTERNAL" and
   (.timeoutSec == 3600) and (.connectionDraining.drainingTimeoutSec == 3600) and
   (.healthChecks | length) == 1 and (.healthChecks[0] | endswith("/" + $hc)) and
   ((.backends | length) == 0 or
    ((.backends | length) == 1 and (.backends[0].group | endswith("/" + $group))))' \
  < <(stream_shell_value "${front_backend_json}") >/dev/null \
  || die "existing ${FRONT_BACKEND_SERVICE} does not match the front topology"
if [[ "$(jq -r '.backends | length' < <(stream_shell_value "${front_backend_json}"))" == "0" ]]; then
  "${GCLOUD_BINARY}" compute backend-services add-backend "${FRONT_BACKEND_SERVICE}" \
    --project "${PROJECT_ID}" --global --instance-group "${INSTANCE_GROUP}" \
    --instance-group-zone "${ZONE}" --balancing-mode UTILIZATION --capacity-scaler 1
fi
front_backend_json="$("${GCLOUD_BINARY}" compute backend-services describe "${FRONT_BACKEND_SERVICE}" \
  --project "${PROJECT_ID}" --global --format=json)"
jq -e --arg hc "${FRONT_HEALTH_CHECK}" --arg group "${INSTANCE_GROUP}" \
  '.portName == "front" and .protocol == "HTTP" and .loadBalancingScheme == "EXTERNAL" and
   (.timeoutSec == 3600) and (.connectionDraining.drainingTimeoutSec == 3600) and
   (.healthChecks | length) == 1 and (.healthChecks[0] | endswith("/" + $hc)) and
   (.backends | length) == 1 and (.backends[0].group | endswith("/" + $group))' \
  < <(stream_shell_value "${front_backend_json}") >/dev/null \
  || die "${FRONT_BACKEND_SERVICE} was not configured atomically"

front_status="$(gcloud_ssh "sudo curl -fsS --unix-socket /var/lib/subrouter/front.sock http://localhost/_subrouter/front-status")"
front_slot="$(jq -r '.active.id // empty' < <(stream_shell_value "${front_status}"))"
[[ "${front_slot}" == slot-a || "${front_slot}" == slot-b ]] || die "front active slot is invalid"
install_canary_access_boundary

# Keep the front backend referenced by a Cloud Armor-protected canary host
# before changing the active route. Paired denied and authenticated public
# requests prove both isolation and routability before user traffic can move.
canary_candidate="${ARTIFACT_DIR}/url-map-canary-candidate.yaml"
canary_applied="${ARTIFACT_DIR}/url-map-canary-applied.yaml"
python3 "${URL_MAP_ROUTING}" prepare-canary \
  "${ARTIFACT_DIR}/url-map-before.yaml" "${canary_candidate}" \
  "${ACTIVE_MATCHER}" "${legacy_backend_url}" \
  "${CANARY_MATCHER}" "${CANARY_HOST}" "${front_backend_url}"
map_updated_at="$(utc_now)"
map_updated_epoch="$(epoch_seconds)"
url_map_switched=1
"${GCLOUD_BINARY}" compute url-maps import "${URL_MAP}" \
  --project "${PROJECT_ID}" --global --source "${canary_candidate}" --quiet
"${GCLOUD_BINARY}" compute url-maps export "${URL_MAP}" \
  --project "${PROJECT_ID}" --global --destination "${canary_applied}" --quiet
python3 "${URL_MAP_ROUTING}" assert-state \
  "${canary_applied}" "${ACTIVE_MATCHER}" "${legacy_backend_url}" \
  "${CANARY_MATCHER}" "${CANARY_HOST}" "${front_backend_url}"
log "requiring the public front canary and every backend health status in each sample for five minutes"
canary_sessions_file="${ARTIFACT_DIR}/front-canary-sessions.txt"
front_readiness_json="$(
  SUBROUTER_GCP_PROJECT="${PROJECT_ID}" \
  SUBROUTER_GCP_FRONT_BACKEND_SERVICE="${FRONT_BACKEND_SERVICE}" \
  SUBROUTER_CANARY_PUBLIC_BASE_URL="${PUBLIC_BASE_URL}" \
  SUBROUTER_CANARY_HOST="${CANARY_HOST}" \
  SUBROUTER_CANARY_TOKEN_FILE="${canary_token_file}" \
  SUBROUTER_CLOUD_CONFIG="${CLOUD_CONFIG}" \
  GCLOUD_BIN="${GCLOUD_BINARY}" \
  python3 "${FRONT_READINESS_WAITER}" \
    --minimum-stable-seconds 300 --timeout-seconds 900 --poll-seconds 2 \
    --maximum-sample-gap-seconds 15 --minimum-samples 21 \
    --session-prefix "canary-${RUN_LABEL}" --sessions-file "${canary_sessions_file}" \
    --probe-stderr-log "${ARTIFACT_DIR}/front-readiness-probe.log" -- \
    bash "${FRONT_READINESS_PROBE}"
)" || die "front backend and public canary did not remain continuously healthy together"
jq -e '.backend_health.all_healthy == true and .backend_health.duration_ms >= 300000 and
  .backend_health.healthy_samples >= 21 and .backend_health.max_sample_gap_ms <= 15000 and
  (.backend_health.backend_membership_sha256 | test("^[0-9a-f]{64}$")) and
  .canary.stable_duration_ms == .backend_health.duration_ms and
  .canary.healthy_samples == .backend_health.healthy_samples and
  .canary.max_sample_gap_ms == .backend_health.max_sample_gap_ms and
  .canary.first_observed_at == .backend_health.stable_since and
  .canary.verified_at == .backend_health.verified_at and
  (.canary.first_session_sha256 | test("^[0-9a-f]{64}$")) and
  (.canary.verified_session_sha256 | test("^[0-9a-f]{64}$")) and
  (.canary.session_set_sha256 | test("^[0-9a-f]{64}$"))' \
  < <(stream_shell_value "${front_readiness_json}") >/dev/null \
  || die "combined front readiness evidence is invalid"
backend_health_json="$(jq -c '.backend_health' < <(stream_shell_value "${front_readiness_json}"))"
first_canary_attempts="$(jq -r '.canary.first_proof_attempts' < <(stream_shell_value "${front_readiness_json}"))"
first_canary_observed_at="$(jq -r '.canary.first_observed_at' < <(stream_shell_value "${front_readiness_json}"))"
first_canary_session_sha256="$(jq -r '.canary.first_session_sha256' < <(stream_shell_value "${front_readiness_json}"))"
verified_canary_attempts="$(jq -r '.canary.verified_proof_attempts' < <(stream_shell_value "${front_readiness_json}"))"
verified_canary_observed_at="$(jq -r '.canary.verified_at' < <(stream_shell_value "${front_readiness_json}"))"
verified_canary_session_sha256="$(jq -r '.canary.verified_session_sha256' < <(stream_shell_value "${front_readiness_json}"))"
canary_stable_duration_ms="$(jq -r '.canary.stable_duration_ms' < <(stream_shell_value "${front_readiness_json}"))"
canary_healthy_samples="$(jq -r '.canary.healthy_samples' < <(stream_shell_value "${front_readiness_json}"))"
canary_max_sample_gap_ms="$(jq -r '.canary.max_sample_gap_ms' < <(stream_shell_value "${front_readiness_json}"))"
canary_session_set_sha256="$(jq -r '.canary.session_set_sha256' < <(stream_shell_value "${front_readiness_json}"))"
canary_correlation_json="$(canary_sessions_observed "${canary_sessions_file}" "${canary_healthy_samples}")" \
  || die "not every continuous public canary sample was correlated in the front slot journal"
jq -e --argjson expected_count "${canary_healthy_samples}" --arg expected_sha "${canary_session_set_sha256}" \
  '.journal_correlated_samples == $expected_count and .session_set_sha256 == $expected_sha' \
  < <(stream_shell_value "${canary_correlation_json}") >/dev/null \
  || die "front canary journal correlation evidence does not match the sampled sessions"
canary_journal_correlated_samples="$(jq -r '.journal_correlated_samples' < <(stream_shell_value "${canary_correlation_json}"))"

legacy_status="$(gcloud_ssh "sudo curl -fsS --unix-socket /var/lib/subrouter/supervisor.sock http://localhost/_subrouter/supervisor-status")"
jq -e '(.active.id|type)=="string" and (.backends|type)=="array" and ([.backends[].connections] | all(type=="number" and . >= 0))' \
  < <(stream_shell_value "${legacy_status}") >/dev/null || die "legacy supervisor status is unavailable or invalid"
legacy_generation="$(jq -r '.active.id' < <(stream_shell_value "${legacy_status}"))"
legacy_inactive_connections="$(jq -r --arg id "${legacy_generation}" '[.backends[] | select(.id != $id) | .connections] | add // 0' < <(stream_shell_value "${legacy_status}"))"
(( legacy_inactive_connections == 0 )) || die "legacy supervisor has inactive generations with held connections"
legacy_checksum="$(gcloud_ssh "sudo sha256sum /usr/local/bin/subrouter | awk '{print \$1}'" | tail -n 1)"
[[ "${legacy_checksum}" =~ ^[0-9a-f]{64}$ ]] || die "legacy installed checksum is invalid"
[[ "${legacy_checksum}" == "${PREDECESSOR_SHA256}" ]] \
  || die "legacy worker bytes do not match the separately verified predecessor"
front_status="$(gcloud_ssh "sudo curl -fsS --unix-socket /var/lib/subrouter/front.sock http://localhost/_subrouter/front-status")"
front_slot="$(jq -r '.active.id' < <(stream_shell_value "${front_status}"))"
front_generation_status="$(gcloud_ssh "sudo curl -fsS --unix-socket /var/lib/subrouter/${front_slot}.sock http://localhost/_subrouter/supervisor-status")"
front_generation="$(jq -r '.active.id' < <(stream_shell_value "${front_generation_status}"))"
front_checksum="$(gcloud_ssh "sudo sha256sum /opt/subrouter/front/subrouter | awk '{print \$1}'" | tail -n 1)"
[[ "${front_checksum}" == "${EXPECTED_SHA256}" ]] || die "front checksum changed after preparation"
control_checksum="$(gcloud_ssh "sudo sha256sum /opt/subrouter/control/subrouter | awk '{print \$1}'" | tail -n 1)"
worker_checksum="$(gcloud_ssh "sudo sha256sum /opt/subrouter/slots/${front_slot}/worker | awk '{print \$1}'" | tail -n 1)"
[[ "${control_checksum}" == "${EXPECTED_SHA256}" ]] || die "slot supervisor control checksum changed after preparation"
[[ "${worker_checksum}" == "${BOOTSTRAP_SHA256}" ]] || die "initial slot worker is not the lease-capable bootstrap release"

migration_committed=1
evidence_emitted_at="$(python3 -c 'from datetime import datetime, timezone; print(datetime.now(timezone.utc).isoformat(timespec="milliseconds").replace("+00:00", "Z"))')"
evidence_tmp="$(mktemp "${EVIDENCE_JSON}.tmp.XXXXXX")"
jq -n --arg schema 'subrouter.gcp.deploy-evidence/v1' --arg evidence_type front-migration-preparation \
  --arg run_id "${RUN_LABEL}" --arg project "${PROJECT_ID}" --arg zone "${ZONE}" --arg instance "${INSTANCE}" \
  --arg tag "${RELEASE_TAG}" --arg sha "${EXPECTED_SHA256}" --arg revision "${DEPLOY_REVISION}" \
  --arg predecessor_tag "${PREDECESSOR_TAG}" --arg predecessor_sha "${PREDECESSOR_SHA256}" \
  --arg predecessor_revision "${PREDECESSOR_REVISION}" \
  --arg bootstrap_tag "${BOOTSTRAP_TAG}" --arg bootstrap_sha "${BOOTSTRAP_SHA256}" \
  --arg bootstrap_revision "${BOOTSTRAP_REVISION}" \
  --arg url_map "${URL_MAP}" --arg legacy_backend "${LEGACY_BACKEND_SERVICE}" \
  --arg front_backend "${FRONT_BACKEND_SERVICE}" --arg legacy_url "${legacy_backend_url}" \
  --arg front_url "${front_backend_url}" --arg active_matcher "${ACTIVE_MATCHER}" \
  --arg canary_matcher "${CANARY_MATCHER}" --arg canary_host "${CANARY_HOST}" \
  --argjson canary_access_control "${canary_policy_evidence}" \
  --arg map_updated_at "${map_updated_at}" --arg first_canary_observed_at "${first_canary_observed_at}" \
  --arg verified_canary_observed_at "${verified_canary_observed_at}" \
  --arg first_canary_session_sha "${first_canary_session_sha256}" \
  --arg verified_canary_session_sha "${verified_canary_session_sha256}" \
  --argjson canary_duration "${canary_stable_duration_ms}" \
  --argjson canary_healthy_samples "${canary_healthy_samples}" \
  --argjson canary_max_sample_gap "${canary_max_sample_gap_ms}" \
  --argjson canary_journal_correlated_samples "${canary_journal_correlated_samples}" \
  --arg canary_session_set_sha "${canary_session_set_sha256}" \
  --argjson first_canary_attempts "${first_canary_attempts}" \
  --argjson verified_canary_attempts "${verified_canary_attempts}" \
  --arg legacy_generation "${legacy_generation}" \
  --arg legacy_checksum "${legacy_checksum}" --arg front_slot "${front_slot}" \
  --arg front_generation "${front_generation}" --arg front_checksum "${front_checksum}" \
  --arg control_checksum "${control_checksum}" --arg worker_checksum "${worker_checksum}" \
  --argjson backend_health "${backend_health_json}" \
  --arg emitted_at "${evidence_emitted_at}" \
  '{schema:$schema,evidence_type:$evidence_type,mode:"prepare",success:true,
    run:{id:$run_id,project:$project,zone:$zone,instance:$instance},
    release:{tag:$tag,sha256:$sha,source_revision:$revision,tag_on_main:true,
      attestation_verified:true,immutable:true},
    bootstrap:{tag:$bootstrap_tag,sha256:$bootstrap_sha,source_revision:$bootstrap_revision,
      tag_on_main:true,attestation_verified:true,immutable:true},
    predecessor:{tag:$predecessor_tag,sha256:$predecessor_sha,source_revision:$predecessor_revision,
      tag_on_main:true,hard_pin_verified:true,sha256sums_match:true,
      embedded_revision_verified:true,live_worker_checksum_match:true},
    routing:{url_map:$url_map,active_matcher:$active_matcher,
      legacy_backend:$legacy_backend,front_backend:$front_backend,
      legacy_backend_url:$legacy_url,front_backend_url:$front_url,current:"legacy",
      canary:{host:$canary_host,matcher:$canary_matcher,backend_url:$front_url,
        access_control:$canary_access_control,
        map_updated_at:$map_updated_at,first_observed_at:$first_canary_observed_at,
        verified_at:$verified_canary_observed_at,stable_duration_ms:$canary_duration,
        healthy_samples:$canary_healthy_samples,max_sample_gap_ms:$canary_max_sample_gap,
        journal_correlated_samples:$canary_journal_correlated_samples,
        session_set_sha256:$canary_session_set_sha,
        first_proof_attempts:$first_canary_attempts,
        verified_proof_attempts:$verified_canary_attempts,
        first_session_sha256:$first_canary_session_sha,
        verified_session_sha256:$verified_canary_session_sha}},
    legacy:{service:"subrouter.service",generation:$legacy_generation,checksum:$legacy_checksum,
      accepting_new_public:true},
    front:{slot:$front_slot,generation:$front_generation,checksum:$front_checksum,
      control_checksum:$control_checksum,worker_checksum:$worker_checksum,ready:true,
      backend_health:$backend_health},
    evidence_emitted_at:$emitted_at}' >"${evidence_tmp}"
python3 "$(dirname "${BASH_SOURCE[0]}")/validate-deploy-evidence.py" \
  --expect front-migration-preparation "${evidence_tmp}" >/dev/null
chmod 0600 "${evidence_tmp}"
mv -f -- "${evidence_tmp}" "${EVIDENCE_JSON}"
gcloud_ssh "rm -f '${REMOTE_CANDIDATE}' '${REMOTE_WORKER_CANDIDATE}' '${REMOTE_INSTALLER}' '${REMOTE_DEPLOYMENT_CONTRACT}'"
log "front resources prepared; the active user route still points to legacy:31415"
jq -c . "${EVIDENCE_JSON}"
