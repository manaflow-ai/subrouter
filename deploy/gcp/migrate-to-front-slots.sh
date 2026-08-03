#!/usr/bin/env bash
# One-time, operator-explicit migration from the legacy LB port to a stable
# front process backed by replaceable loopback supervisor slots.
set -euo pipefail

PROJECT_ID="${SUBROUTER_GCP_PROJECT:?set SUBROUTER_GCP_PROJECT}"
ZONE="${SUBROUTER_GCP_ZONE:?set SUBROUTER_GCP_ZONE}"
INSTANCE="${SUBROUTER_GCP_INSTANCE:?set SUBROUTER_GCP_INSTANCE}"
RELEASE_TAG="${SUBROUTER_RELEASE_TAG:?set SUBROUTER_RELEASE_TAG}"
DEPLOY_BINARY="${SUBROUTER_DEPLOY_BINARY:?set SUBROUTER_DEPLOY_BINARY}"
RELEASE_SHA256_FILE="${SUBROUTER_RELEASE_SHA256_FILE:-${DEPLOY_BINARY}.sha256}"
PUBLIC_BASE_URL="${SUBROUTER_PUBLIC_BASE_URL:?set SUBROUTER_PUBLIC_BASE_URL}"
GCLOUD_BINARY="${GCLOUD_BIN:-gcloud}"
FRONT_HEALTH_CHECK="${SUBROUTER_GCP_FRONT_HEALTH_CHECK:-subrouter-front-hc}"
FIREWALL_RULE="${SUBROUTER_GCP_LB_FIREWALL_RULE:-subrouter-allow-lb}"
URL_MAP="${SUBROUTER_GCP_URL_MAP:-subrouter-urlmap}"
DEPLOY_LOCK_FILE="${SUBROUTER_DEPLOY_LOCK_FILE:-/run/lock/subrouter-deploy.lock}"
ARTIFACT_DIR="${SUBROUTER_DEPLOY_ARTIFACT_DIR:-${PWD}/artifacts/gcp-front-migration}"
RUN_LABEL="${GITHUB_RUN_ID:-local}-${GITHUB_RUN_ATTEMPT:-0}-$$"
RUN_LABEL="${RUN_LABEL//[^a-zA-Z0-9._-]/-}"
REMOTE_CANDIDATE="/tmp/subrouter-front-migration-${RUN_LABEL}"
REMOTE_INSTALLER="/tmp/install-front-slots-${RUN_LABEL}.sh"
REMOTE_LOCK_SENTINEL="/tmp/subrouter-deploy-lock-${RUN_LABEL}"

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

for command in "${GCLOUD_BINARY}" curl jq python3 sha256sum; do
  command -v "${command}" >/dev/null 2>&1 || die "required command not found: ${command}"
done
[[ "${RELEASE_TAG}" =~ ^v[0-9]+\.[0-9]+\.[0-9]+([.-][0-9A-Za-z.-]+)?$ ]] \
  || die "SUBROUTER_RELEASE_TAG must be an explicit version tag"
[[ -x "${DEPLOY_BINARY}" ]] || die "release binary is not executable: ${DEPLOY_BINARY}"
[[ -f "${RELEASE_SHA256_FILE}" ]] || die "release checksum file is missing: ${RELEASE_SHA256_FILE}"
EXPECTED_SHA256="$(tr -d '[:space:]' <"${RELEASE_SHA256_FILE}")"
[[ "${EXPECTED_SHA256}" =~ ^[0-9a-f]{64}$ ]] || die "release checksum is invalid"
[[ "$(sha256sum "${DEPLOY_BINARY}" | awk '{print $1}')" == "${EXPECTED_SHA256}" ]] \
  || die "release binary does not match its verified checksum"
[[ "${PUBLIC_BASE_URL}" =~ ^https://[^/?#]+/?$ ]] \
  || die "SUBROUTER_PUBLIC_BASE_URL must be an HTTPS origin"
PUBLIC_BASE_URL="${PUBLIC_BASE_URL%/}"

mkdir -p "${ARTIFACT_DIR}"
ARTIFACT_DIR="$(cd "${ARTIFACT_DIR}" && pwd)"

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
  gcloud_ssh "rm -f '${REMOTE_CANDIDATE}' '${REMOTE_INSTALLER}'" >/dev/null 2>&1 || true
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
map_state="$(python3 - "${ARTIFACT_DIR}/url-map-before.yaml" \
  "${legacy_backend_url}" "${front_backend_url}" <<'PY'
from pathlib import Path
import sys

body = Path(sys.argv[1]).read_text()
legacy_count = body.count(sys.argv[2])
front_count = body.count(sys.argv[3])
if (legacy_count, front_count) == (1, 0):
    print("legacy")
elif (legacy_count, front_count) == (0, 1):
    print("front")
else:
    raise SystemExit(
        f"ambiguous URL-map references: legacy={legacy_count}, front={front_count}"
    )
PY
)"
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
    gcloud_scp "$(dirname "${BASH_SOURCE[0]}")/install-front-slots.sh" "${REMOTE_INSTALLER}"
    gcloud_ssh "set -e; printf '%s  %s\n' '${EXPECTED_SHA256}' '${REMOTE_CANDIDATE}' | sha256sum -c - >/dev/null; sudo bash '${REMOTE_INSTALLER}' install-release '${RELEASE_TAG}' '${REMOTE_CANDIDATE}' '${EXPECTED_SHA256}'; sudo bash '${REMOTE_INSTALLER}' install-topology '${RELEASE_TAG}' slot-a"
    ;;
  active)
    log "resuming an earlier explicit migration with an already healthy front topology"
    ;;
  *)
    die "front topology is partially installed; repair it before changing the URL map"
    ;;
esac
gcloud_ssh "set -e; status=\$(sudo curl -fsS --unix-socket /var/lib/subrouter/front.sock http://localhost/_subrouter/front-status); slot=\$(printf '%s' \"\${status}\" | jq -r '.active.id // empty'); case \"\${slot}\" in slot-a|slot-b) ;; *) exit 1 ;; esac; test \"\$(sudo sha256sum /opt/subrouter/front/subrouter | awk '{print \$1}')\" = '${EXPECTED_SHA256}'; test \"\$(sudo sha256sum /opt/subrouter/slots/\${slot}/subrouter | awk '{print \$1}')\" = '${EXPECTED_SHA256}'; curl -fsS http://127.0.0.1:31416/_subrouter/health >/dev/null; curl -fsS http://127.0.0.1:31416/_subrouter/ready >/dev/null; systemctl is-active --quiet subrouter.service subrouter-front.service subrouter-slot@\${slot}.service; systemctl is-enabled --quiet subrouter-front.service subrouter-slot@\${slot}.service" \
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

python3 - "${ARTIFACT_DIR}/url-map-before.yaml" "${ARTIFACT_DIR}/url-map-candidate.yaml" \
  "${legacy_backend_url}" "${front_backend_url}" <<'PY'
from pathlib import Path
import sys

source, destination, legacy, front = sys.argv[1:]
body = Path(source).read_text()
if body.count(legacy) != 1:
    raise SystemExit(f"expected exactly one URL-map reference to {legacy}")
Path(destination).write_text(body.replace(legacy, front, 1))
PY
url_map_switched=1
"${GCLOUD_BINARY}" compute url-maps import "${URL_MAP}" \
  --project "${PROJECT_ID}" --global \
  --source "${ARTIFACT_DIR}/url-map-candidate.yaml" --quiet
"${GCLOUD_BINARY}" compute url-maps export "${URL_MAP}" \
  --project "${PROJECT_ID}" --global \
  --destination "${ARTIFACT_DIR}/url-map-applied.yaml" --quiet
python3 - "${ARTIFACT_DIR}/url-map-applied.yaml" \
  "${legacy_backend_url}" "${front_backend_url}" <<'PY'
from pathlib import Path
import sys

body = Path(sys.argv[1]).read_text()
if body.count(sys.argv[2]) != 0 or body.count(sys.argv[3]) != 1:
    raise SystemExit("the imported URL map does not contain the exact front cutover")
PY

for _ in $(seq 1 120); do
  if curl -fsS --max-time 5 "${PUBLIC_BASE_URL}/_subrouter/health" >/dev/null 2>&1 &&
      curl -fsS --max-time 5 "${PUBLIC_BASE_URL}/_subrouter/ready" >/dev/null 2>&1; then
    migration_committed=1
    break
  fi
  sleep 1
done
[[ "${migration_committed}" == "1" ]] || die "public front did not pass health and readiness after cutover"

"${GCLOUD_BINARY}" compute backend-services describe "${FRONT_BACKEND_SERVICE}" \
  --project "${PROJECT_ID}" --global --format=json >"${ARTIFACT_DIR}/front-backend-after.json"
"${GCLOUD_BINARY}" compute instance-groups describe "${INSTANCE_GROUP}" \
  --project "${PROJECT_ID}" --zone "${ZONE}" --format=json >"${ARTIFACT_DIR}/instance-group-after.json"
"${GCLOUD_BINARY}" compute url-maps export "${URL_MAP}" \
  --project "${PROJECT_ID}" --global \
  --destination "${ARTIFACT_DIR}/url-map-after.yaml" --quiet
gcloud_ssh "rm -f '${REMOTE_CANDIDATE}' '${REMOTE_INSTALLER}'"
log "front migration passed; the URL map sends new traffic to front:31416 while the legacy backend and http:31415 remain available"
