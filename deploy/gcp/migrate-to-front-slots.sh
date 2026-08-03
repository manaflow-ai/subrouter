#!/usr/bin/env bash
# One-time, operator-explicit migration from the legacy LB port to a stable
# front process backed by replaceable loopback supervisor slots.
set -euo pipefail

usage() {
  cat <<'EOF'
Usage: migrate-to-front-slots.sh [--evidence-json PATH]

Prepares and verifies the front, private slot, health check, backend, firewall,
and named port without changing the URL map. Continue with
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
PUBLIC_BASE_URL="${SUBROUTER_PUBLIC_BASE_URL:?set SUBROUTER_PUBLIC_BASE_URL}"
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
REMOTE_LOCK_SENTINEL="/tmp/subrouter-deploy-lock-${RUN_LABEL}"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

case "${INSTANCE}" in
  subrouter-team)
    LEGACY_BACKEND_SERVICE="${SUBROUTER_GCP_BACKEND_SERVICE:-subrouter-backend}"
    FRONT_BACKEND_SERVICE="${SUBROUTER_GCP_FRONT_BACKEND_SERVICE:-subrouter-front-backend}"
    INSTANCE_GROUP="${SUBROUTER_GCP_INSTANCE_GROUP:-subrouter-ig}"
    ;;
  subrouter-staging)
    LEGACY_BACKEND_SERVICE="${SUBROUTER_GCP_BACKEND_SERVICE:-subrouter-staging-backend}"
    FRONT_BACKEND_SERVICE="${SUBROUTER_GCP_FRONT_BACKEND_SERVICE:-subrouter-staging-front-backend}"
    INSTANCE_GROUP="${SUBROUTER_GCP_INSTANCE_GROUP:-subrouter-staging-ig}"
    ;;
  *)
    LEGACY_BACKEND_SERVICE="${SUBROUTER_GCP_BACKEND_SERVICE:?set SUBROUTER_GCP_BACKEND_SERVICE}"
    FRONT_BACKEND_SERVICE="${SUBROUTER_GCP_FRONT_BACKEND_SERVICE:?set SUBROUTER_GCP_FRONT_BACKEND_SERVICE}"
    INSTANCE_GROUP="${SUBROUTER_GCP_INSTANCE_GROUP:?set SUBROUTER_GCP_INSTANCE_GROUP}"
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
[[ "${PREDECESSOR_SHA256}" != "${EXPECTED_SHA256}" ]] \
  || die "predecessor worker must differ from the control release"
[[ "${DEPLOY_REVISION}" =~ ^[0-9a-f]{40}$ ]] || die "SUBROUTER_DEPLOY_REVISION must be a full verified commit"
bash "${SCRIPT_DIR}/verify-go-release-binary.sh" "${DEPLOY_BINARY}" "${DEPLOY_REVISION}" \
  || die "candidate embedded metadata is invalid"
[[ "${PREDECESSOR_REVISION}" == 5eacb5411c0bd4a24f4e422d6366fa7bfd1843c8 ]] \
  || die "predecessor revision does not match the compiled-in v0.1.51 hard pin"
bash "${SCRIPT_DIR}/verify-go-release-binary.sh" \
  "${PREDECESSOR_BINARY}" 5eacb5411c0bd4a24f4e422d6366fa7bfd1843c8 \
  || die "predecessor embedded metadata is invalid"
[[ "${TAG_ON_MAIN}" == true ]] || die "release tag commit was not proven to be on main"
[[ "${ATTESTATION_VERIFIED}" == true ]] || die "release artifact attestation was not verified"
[[ "${RELEASE_IMMUTABLE}" == true ]] || die "release was not proven published and immutable"
[[ "${PREDECESSOR_TAG_ON_MAIN}" == true ]] || die "predecessor tag commit was not proven to be on main"
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

lock_holder_pid=""
acquire_deploy_lock() {
  gcloud_ssh "umask 077; : > '${REMOTE_LOCK_SENTINEL}'; command -v flock >/dev/null"
  "${GCLOUD_BINARY}" compute ssh "${INSTANCE}" \
    --project "${PROJECT_ID}" --zone "${ZONE}" --tunnel-through-iap --quiet \
    --command "sudo flock -x -w 300 '${DEPLOY_LOCK_FILE}' sh -c 'echo LOCKED; while test -e \"${REMOTE_LOCK_SENTINEL}\"; do sleep 1; done'" \
    >"${ARTIFACT_DIR}/deploy-lock.log" 2>&1 &
  lock_holder_pid=$!
  for _ in $(seq 1 3100); do
    grep -qx LOCKED "${ARTIFACT_DIR}/deploy-lock.log" 2>/dev/null && return 0
    kill -0 "${lock_holder_pid}" 2>/dev/null \
      || die "remote deployment lock holder exited"
    sleep 0.1
  done
  die "timed out acquiring ${DEPLOY_LOCK_FILE}"
}

release_deploy_lock() {
  [[ -n "${lock_holder_pid}" ]] || return 0
  gcloud_ssh "rm -f '${REMOTE_LOCK_SENTINEL}'" >/dev/null 2>&1 || true
  wait "${lock_holder_pid}" || true
  lock_holder_pid=""
}

old_drain_timeout=""
migration_started=0
migration_committed=0
url_map_switched=0

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
map_state="$(python3 "${DEPLOYMENT_CONTRACT}" classify-url-map \
  "${ARTIFACT_DIR}/url-map-before.yaml" "${legacy_backend_url}" "${front_backend_url}")"
[[ "${map_state}" == "legacy" ]] \
  || die "${URL_MAP} already routes this environment to ${FRONT_BACKEND_SERVICE}; migration is complete"

http_named_port_count="$(jq -r '[.namedPorts[]? | select(.name == "http" and .port == 31415)] | length' <<<"${group_json}")"
front_named_port="$(jq -r '[.namedPorts[]? | select(.name == "front") | .port][0] // empty' <<<"${group_json}")"
old_drain_timeout="$(jq -r '.connectionDraining.drainingTimeoutSec // 0' <<<"${backend_json}")"
[[ "${http_named_port_count}" == "1" ]] || die "expected exactly one legacy named port http:31415"
[[ -z "${front_named_port}" || "${front_named_port}" == "31416" ]] \
  || die "existing front named port is ${front_named_port}, expected 31416"
[[ "${old_drain_timeout}" =~ ^[0-9]+$ ]] || die "invalid existing connection-draining timeout"

front_state="$(gcloud_ssh "if sudo test -S /var/lib/subrouter/front.sock && systemctl is-active --quiet subrouter-front.service; then echo active; elif sudo test ! -S /var/lib/subrouter/front.sock && ! systemctl is-active --quiet subrouter-front.service; then echo absent; else echo partial; fi" | tail -n 1)"
case "${front_state}" in
  absent)
    gcloud_scp "${DEPLOY_BINARY}" "${REMOTE_CANDIDATE}"
    gcloud_scp "${PREDECESSOR_BINARY}" "${REMOTE_WORKER_CANDIDATE}"
    gcloud_scp "${INSTALL_FRONT_SLOTS}" "${REMOTE_INSTALLER}"
    gcloud_scp "${DEPLOYMENT_CONTRACT}" "${REMOTE_DEPLOYMENT_CONTRACT}"
    gcloud_ssh "printf '%s  %s\n%s  %s\n' '${INSTALL_FRONT_SLOTS_SHA256}' '${REMOTE_INSTALLER}' '${DEPLOYMENT_CONTRACT_SHA256}' '${REMOTE_DEPLOYMENT_CONTRACT}' | sha256sum -c - >/dev/null"
    gcloud_ssh "set -e; printf '%s  %s\n%s  %s\n' '${EXPECTED_SHA256}' '${REMOTE_CANDIDATE}' '${PREDECESSOR_SHA256}' '${REMOTE_WORKER_CANDIDATE}' | sha256sum -c - >/dev/null; ${REMOTE_INSTALL_COMMAND} install-release '${RELEASE_TAG}' '${REMOTE_CANDIDATE}' '${EXPECTED_SHA256}'; ${REMOTE_INSTALL_COMMAND} install-release '${PREDECESSOR_TAG}' '${REMOTE_WORKER_CANDIDATE}' '${PREDECESSOR_SHA256}'; ${REMOTE_INSTALL_COMMAND} install-topology '${RELEASE_TAG}' '${PREDECESSOR_TAG}' slot-a"
    ;;
  active)
    log "resuming an earlier explicit migration with an already healthy front topology"
    ;;
  *)
    die "front topology is partially installed; repair it before changing the URL map"
    ;;
esac
gcloud_ssh "set -e; status=\$(sudo curl -fsS --unix-socket /var/lib/subrouter/front.sock http://localhost/_subrouter/front-status); slot=\$(printf '%s' \"\${status}\" | jq -r '.active.id // empty'); case \"\${slot}\" in slot-a|slot-b) ;; *) exit 1 ;; esac; test \"\$(sudo sha256sum /opt/subrouter/front/subrouter | awk '{print \$1}')\" = '${EXPECTED_SHA256}'; test \"\$(sudo sha256sum /opt/subrouter/control/subrouter | awk '{print \$1}')\" = '${EXPECTED_SHA256}'; test \"\$(sudo sha256sum /opt/subrouter/slots/\${slot}/worker | awk '{print \$1}')\" = '${PREDECESSOR_SHA256}'; curl -fsS http://127.0.0.1:31416/_subrouter/health >/dev/null; curl -fsS http://127.0.0.1:31416/_subrouter/ready >/dev/null; systemctl is-active --quiet subrouter.service subrouter-front.service subrouter-slot@\${slot}.service; systemctl is-enabled --quiet subrouter-front.service subrouter-slot@\${slot}.service" \
  || die "front topology does not match the verified migration release"

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
    <<<"${front_hc_json}" >/dev/null \
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
' <<<"${group_json}")"
"${GCLOUD_BINARY}" compute instance-groups set-named-ports "${INSTANCE_GROUP}" \
  --project "${PROJECT_ID}" --zone "${ZONE}" \
  --named-ports "${desired_named_ports}"
updated_group_json="$("${GCLOUD_BINARY}" compute instance-groups describe "${INSTANCE_GROUP}" \
  --project "${PROJECT_ID}" --zone "${ZONE}" --format=json)"
jq -e '.namedPorts | any(.name == "http" and .port == 31415) and any(.name == "front" and .port == 31416)' \
  <<<"${updated_group_json}" >/dev/null || die "instance-group named ports were not preserved"

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
  <<<"${front_backend_json}" >/dev/null \
  || die "existing ${FRONT_BACKEND_SERVICE} does not match the front topology"
if [[ "$(jq -r '.backends | length' <<<"${front_backend_json}")" == "0" ]]; then
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
  <<<"${front_backend_json}" >/dev/null \
  || die "${FRONT_BACKEND_SERVICE} was not configured atomically"

log "waiting for the fixed-port front health check before routing traffic"
for _ in $(seq 1 120); do
  health_json="$("${GCLOUD_BINARY}" compute backend-services get-health "${FRONT_BACKEND_SERVICE}" \
    --project "${PROJECT_ID}" --global --format=json 2>/dev/null || true)"
  if jq -e '[.[].status.healthStatus[]? | select(.healthState == "HEALTHY")] | length > 0' \
      <<<"${health_json:-[]}" >/dev/null 2>&1; then
    break
  fi
  sleep 1
done
health_json="$("${GCLOUD_BINARY}" compute backend-services get-health "${FRONT_BACKEND_SERVICE}" \
  --project "${PROJECT_ID}" --global --format=json)"
jq -e '[.[].status.healthStatus[]? | select(.healthState == "HEALTHY")] | length > 0' \
  <<<"${health_json}" >/dev/null || die "front did not become healthy in the load balancer"

legacy_status="$(gcloud_ssh "sudo curl -fsS --unix-socket /var/lib/subrouter/supervisor.sock http://localhost/_subrouter/supervisor-status")"
jq -e '(.active.id|type)=="string" and (.backends|type)=="array" and ([.backends[].connections] | all(type=="number" and . >= 0))' \
  <<<"${legacy_status}" >/dev/null || die "legacy supervisor status is unavailable or invalid"
legacy_generation="$(jq -r '.active.id' <<<"${legacy_status}")"
legacy_inactive_connections="$(jq -r --arg id "${legacy_generation}" '[.backends[] | select(.id != $id) | .connections] | add // 0' <<<"${legacy_status}")"
(( legacy_inactive_connections == 0 )) || die "legacy supervisor has inactive generations with held connections"
legacy_checksum="$(gcloud_ssh "sudo sha256sum /usr/local/bin/subrouter | awk '{print \$1}'" | tail -n 1)"
[[ "${legacy_checksum}" =~ ^[0-9a-f]{64}$ ]] || die "legacy installed checksum is invalid"
[[ "${legacy_checksum}" == "${PREDECESSOR_SHA256}" ]] \
  || die "legacy worker bytes do not match the separately verified predecessor"
front_status="$(gcloud_ssh "sudo curl -fsS --unix-socket /var/lib/subrouter/front.sock http://localhost/_subrouter/front-status")"
front_slot="$(jq -r '.active.id' <<<"${front_status}")"
front_generation_status="$(gcloud_ssh "sudo curl -fsS --unix-socket /var/lib/subrouter/${front_slot}.sock http://localhost/_subrouter/supervisor-status")"
front_generation="$(jq -r '.active.id' <<<"${front_generation_status}")"
front_checksum="$(gcloud_ssh "sudo sha256sum /opt/subrouter/front/subrouter | awk '{print \$1}'" | tail -n 1)"
[[ "${front_checksum}" == "${EXPECTED_SHA256}" ]] || die "front checksum changed after preparation"
control_checksum="$(gcloud_ssh "sudo sha256sum /opt/subrouter/control/subrouter | awk '{print \$1}'" | tail -n 1)"
worker_checksum="$(gcloud_ssh "sudo sha256sum /opt/subrouter/slots/${front_slot}/worker | awk '{print \$1}'" | tail -n 1)"
[[ "${control_checksum}" == "${EXPECTED_SHA256}" ]] || die "slot supervisor control checksum changed after preparation"
[[ "${worker_checksum}" == "${PREDECESSOR_SHA256}" ]] || die "initial slot worker is not the predecessor release"

migration_committed=1
evidence_emitted_at="$(python3 -c 'from datetime import datetime, timezone; print(datetime.now(timezone.utc).isoformat(timespec="milliseconds").replace("+00:00", "Z"))')"
evidence_tmp="$(mktemp "${EVIDENCE_JSON}.tmp.XXXXXX")"
jq -n --arg schema 'subrouter.gcp.deploy-evidence/v1' --arg evidence_type front-migration-preparation \
  --arg run_id "${RUN_LABEL}" --arg project "${PROJECT_ID}" --arg zone "${ZONE}" --arg instance "${INSTANCE}" \
  --arg tag "${RELEASE_TAG}" --arg sha "${EXPECTED_SHA256}" --arg revision "${DEPLOY_REVISION}" \
  --arg predecessor_tag "${PREDECESSOR_TAG}" --arg predecessor_sha "${PREDECESSOR_SHA256}" \
  --arg predecessor_revision "${PREDECESSOR_REVISION}" \
  --arg url_map "${URL_MAP}" --arg legacy_backend "${LEGACY_BACKEND_SERVICE}" \
  --arg front_backend "${FRONT_BACKEND_SERVICE}" --arg legacy_url "${legacy_backend_url}" \
  --arg front_url "${front_backend_url}" --arg legacy_generation "${legacy_generation}" \
  --arg legacy_checksum "${legacy_checksum}" --arg front_slot "${front_slot}" \
  --arg front_generation "${front_generation}" --arg front_checksum "${front_checksum}" \
  --arg control_checksum "${control_checksum}" --arg worker_checksum "${worker_checksum}" \
  --arg emitted_at "${evidence_emitted_at}" \
  '{schema:$schema,evidence_type:$evidence_type,mode:"prepare",success:true,
    run:{id:$run_id,project:$project,zone:$zone,instance:$instance},
    release:{tag:$tag,sha256:$sha,source_revision:$revision,tag_on_main:true,
      attestation_verified:true,immutable:true},
    predecessor:{tag:$predecessor_tag,sha256:$predecessor_sha,source_revision:$predecessor_revision,
      tag_on_main:true,hard_pin_verified:true,sha256sums_match:true,
      embedded_revision_verified:true,live_worker_checksum_match:true},
    routing:{url_map:$url_map,legacy_backend:$legacy_backend,front_backend:$front_backend,
      legacy_backend_url:$legacy_url,front_backend_url:$front_url,current:"legacy"},
    legacy:{service:"subrouter.service",generation:$legacy_generation,checksum:$legacy_checksum,
      accepting_new_public:true},
    front:{slot:$front_slot,generation:$front_generation,checksum:$front_checksum,
      control_checksum:$control_checksum,worker_checksum:$worker_checksum,ready:true},
    evidence_emitted_at:$emitted_at}' >"${evidence_tmp}"
python3 "$(dirname "${BASH_SOURCE[0]}")/validate-deploy-evidence.py" \
  --expect front-migration-preparation "${evidence_tmp}" >/dev/null
chmod 0600 "${evidence_tmp}"
mv -f -- "${evidence_tmp}" "${EVIDENCE_JSON}"
gcloud_ssh "rm -f '${REMOTE_CANDIDATE}' '${REMOTE_WORKER_CANDIDATE}' '${REMOTE_INSTALLER}' '${REMOTE_DEPLOYMENT_CONTRACT}'"
log "front resources prepared; URL map still routes new public traffic to legacy:31415"
jq -c . "${EVIDENCE_JSON}"
