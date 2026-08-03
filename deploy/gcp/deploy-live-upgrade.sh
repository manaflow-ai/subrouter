#!/usr/bin/env bash
# Prepare one checksum-verified release in the inactive slot, atomically switch
# the stable front, and return while externally held original connections live.
set -euo pipefail

usage() {
  cat <<'EOF'
Usage: deploy-live-upgrade.sh --intent rehearsal|final \
  --golden-ack-request PATH --golden-ack PATH \
  [--evidence-json PATH]

Prepares and activates the candidate, then writes one bounded v1 JSON object
atomically to PATH and prints it compactly as the final stdout record. This
command never starts synthetic clients, rolls back, retires, drains, or stops a
slot. The external golden gate owns the live session window. Continue with
rollback-slot.sh for a rehearsal or finalize-slot-retirement.sh for a final
deployment.
EOF
}

EVIDENCE_JSON=""
ACTIVATION_INTENT=""
GOLDEN_ACK_REQUEST=""
GOLDEN_ACK=""
while (( $# > 0 )); do
  case "$1" in
    --intent)
      (( $# >= 2 )) || { usage >&2; exit 2; }
      ACTIVATION_INTENT="$2"
      shift 2
      ;;
    --evidence-json)
      (( $# >= 2 )) || { usage >&2; exit 2; }
      EVIDENCE_JSON="$2"
      shift 2
      ;;
    --golden-ack-request)
      (( $# >= 2 )) || { usage >&2; exit 2; }
      GOLDEN_ACK_REQUEST="$2"
      shift 2
      ;;
    --golden-ack)
      (( $# >= 2 )) || { usage >&2; exit 2; }
      GOLDEN_ACK="$2"
      shift 2
      ;;
    --help|-h)
      usage
      exit 0
      ;;
    *)
      usage >&2
      exit 2
      ;;
  esac
done
[[ "${ACTIVATION_INTENT}" == rehearsal || "${ACTIVATION_INTENT}" == final ]] || { usage >&2; exit 2; }
[[ -n "${GOLDEN_ACK_REQUEST}" && -n "${GOLDEN_ACK}" ]] || { usage >&2; exit 2; }

PROJECT_ID="${SUBROUTER_GCP_PROJECT:?set SUBROUTER_GCP_PROJECT}"
ZONE="${SUBROUTER_GCP_ZONE:?set SUBROUTER_GCP_ZONE}"
INSTANCE="${SUBROUTER_GCP_INSTANCE:?set SUBROUTER_GCP_INSTANCE}"
RELEASE_TAG="${SUBROUTER_RELEASE_TAG:?set SUBROUTER_RELEASE_TAG}"
DEPLOY_BINARY="${SUBROUTER_DEPLOY_BINARY:?set SUBROUTER_DEPLOY_BINARY}"
RELEASE_SHA256_FILE="${SUBROUTER_RELEASE_SHA256_FILE:-${DEPLOY_BINARY}.sha256}"
GCLOUD_BINARY="${GCLOUD_BIN:-gcloud}"
CONFIGURED_CODEX_CLIENTS=4
EXPECTED_ORIGINAL_CONNECTIONS="${SUBROUTER_EXPECTED_ORIGINAL_CONNECTIONS:-2}"
EXPECTED_ROLLBACK_CONNECTIONS="${SUBROUTER_EXPECTED_ROLLBACK_CONNECTIONS:-1}"
MAX_MEMORY_BYTES="${SUBROUTER_MAX_MEMORY_BYTES:-201326592}"
PUBLIC_BASE_URL="${SUBROUTER_PUBLIC_BASE_URL:?set SUBROUTER_PUBLIC_BASE_URL}"
DEPLOY_REVISION="${SUBROUTER_DEPLOY_REVISION:?set SUBROUTER_DEPLOY_REVISION to the verified tag commit}"
TAG_ON_MAIN="${SUBROUTER_RELEASE_TAG_ON_MAIN:?set SUBROUTER_RELEASE_TAG_ON_MAIN from the ancestry gate}"
ATTESTATION_VERIFIED="${SUBROUTER_RELEASE_ATTESTATION_VERIFIED:?set SUBROUTER_RELEASE_ATTESTATION_VERIFIED from gh attestation verify}"
RELEASE_IMMUTABLE="${SUBROUTER_RELEASE_IMMUTABLE:?set SUBROUTER_RELEASE_IMMUTABLE from gh release view and verify-asset}"
FRONT_SOCKET="${SUBROUTER_FRONT_CONTROL_SOCKET:-/var/lib/subrouter/front.sock}"
STATE_DIR="${SUBROUTER_STATE_DIR:-/var/lib/subrouter}"
DEPLOY_LOCK_FILE="${SUBROUTER_DEPLOY_LOCK_FILE:-/run/lock/subrouter-deploy.lock}"
ARTIFACT_DIR="${SUBROUTER_DEPLOY_ARTIFACT_DIR:-${PWD}/artifacts/gcp-live-upgrade}"
RUN_LABEL="${GITHUB_RUN_ID:-local}-${GITHUB_RUN_ATTEMPT:-0}-$$"
RUN_LABEL="${RUN_LABEL//[^a-zA-Z0-9._-]/-}"
REMOTE_CANDIDATE="/tmp/subrouter-slot-${RUN_LABEL}"
REMOTE_INSTALLER="/tmp/install-front-slots-${RUN_LABEL}.sh"
REMOTE_LOCK_SENTINEL="/tmp/subrouter-deploy-lock-${RUN_LABEL}"

log() { printf 'gcp-slot-deploy: %s\n' "$*"; }
die() { log "$*" >&2; exit 1; }

for command in "${GCLOUD_BINARY}" go jq curl python3 sha256sum; do
  command -v "${command}" >/dev/null 2>&1 || die "required command not found: ${command}"
done
[[ -x "${DEPLOY_BINARY}" ]] || die "deploy binary is not executable: ${DEPLOY_BINARY}"
[[ -f "${RELEASE_SHA256_FILE}" ]] || die "release checksum file is missing: ${RELEASE_SHA256_FILE}"
[[ "${RELEASE_TAG}" =~ ^v[0-9]+\.[0-9]+\.[0-9]+([.-][0-9A-Za-z.-]+)?$ ]] \
  || die "SUBROUTER_RELEASE_TAG must be an explicit version tag"
[[ "${EXPECTED_ORIGINAL_CONNECTIONS}" =~ ^[0-9]+$ ]] || die "SUBROUTER_EXPECTED_ORIGINAL_CONNECTIONS must be an integer"
[[ "${EXPECTED_ROLLBACK_CONNECTIONS}" =~ ^[0-9]+$ ]] || die "SUBROUTER_EXPECTED_ROLLBACK_CONNECTIONS must be an integer"
[[ "${MAX_MEMORY_BYTES}" =~ ^[0-9]+$ ]] || die "SUBROUTER_MAX_MEMORY_BYTES must be an integer"
(( EXPECTED_ORIGINAL_CONNECTIONS > 0 )) || die "SUBROUTER_EXPECTED_ORIGINAL_CONNECTIONS must be positive"
(( EXPECTED_ORIGINAL_CONNECTIONS == 2 )) || die "the external golden gate requires exactly two direct hosted connections"
(( EXPECTED_ROLLBACK_CONNECTIONS == 1 )) || die "the rollback gate requires exactly one candidate-spanning direct connection"
(( MAX_MEMORY_BYTES > 0 )) || die "SUBROUTER_MAX_MEMORY_BYTES must be positive"
(( MAX_MEMORY_BYTES == 201326592 )) || die "SUBROUTER_MAX_MEMORY_BYTES must match the 192 MiB slot MemoryMax"
[[ "${DEPLOY_REVISION}" =~ ^[0-9a-f]{40}$ ]] || die "SUBROUTER_DEPLOY_REVISION must be a full verified commit"
candidate_metadata="$(go version -m "${DEPLOY_BINARY}")"
grep -Fq "vcs.revision=${DEPLOY_REVISION}" <<<"${candidate_metadata}" \
  || die "candidate embedded revision does not match the verified release commit"
grep -Fq 'vcs.modified=false' <<<"${candidate_metadata}" \
  || die "candidate embedded metadata reports modified source"
[[ "${TAG_ON_MAIN}" == "true" ]] || die "release tag commit was not proven to be on main"
[[ "${ATTESTATION_VERIFIED}" == "true" ]] || die "release artifact attestation was not verified"
[[ "${RELEASE_IMMUTABLE}" == "true" ]] || die "release was not proven published and immutable"
[[ "${PUBLIC_BASE_URL}" =~ ^https://[^/?#]+/?$ ]] \
  || die "SUBROUTER_PUBLIC_BASE_URL must be an HTTPS origin"
PUBLIC_BASE_URL="${PUBLIC_BASE_URL%/}"
EXPECTED_SHA256="$(tr -d '[:space:]' <"${RELEASE_SHA256_FILE}")"
[[ "${EXPECTED_SHA256}" =~ ^[0-9a-f]{64}$ ]] || die "release checksum is invalid"
CANDIDATE_SHA256="$(sha256sum "${DEPLOY_BINARY}" | awk '{print $1}')"
[[ "${CANDIDATE_SHA256}" == "${EXPECTED_SHA256}" ]] \
  || die "deploy binary does not match the verified release checksum"

mkdir -p "${ARTIFACT_DIR}"
ARTIFACT_DIR="$(cd "${ARTIFACT_DIR}" && pwd)"
EVIDENCE_JSON="${EVIDENCE_JSON:-${SUBROUTER_DEPLOY_EVIDENCE_JSON:-${ARTIFACT_DIR}/result.json}}"
mkdir -p "$(dirname "${EVIDENCE_JSON}")"
EVIDENCE_JSON="$(cd "$(dirname "${EVIDENCE_JSON}")" && pwd)/$(basename "${EVIDENCE_JSON}")"
mkdir -p "$(dirname "${GOLDEN_ACK_REQUEST}")" "$(dirname "${GOLDEN_ACK}")"
GOLDEN_ACK_REQUEST="$(cd "$(dirname "${GOLDEN_ACK_REQUEST}")" && pwd)/$(basename "${GOLDEN_ACK_REQUEST}")"
GOLDEN_ACK="$(cd "$(dirname "${GOLDEN_ACK}")" && pwd)/$(basename "${GOLDEN_ACK}")"
[[ ! -e "${GOLDEN_ACK_REQUEST}" && ! -L "${GOLDEN_ACK_REQUEST}" ]] \
  || die "golden acknowledgement request path must not already exist"
[[ ! -e "${GOLDEN_ACK}" && ! -L "${GOLDEN_ACK}" ]] \
  || die "golden acknowledgement path must not already exist"

gcloud_ssh() {
  "${GCLOUD_BINARY}" compute ssh "${INSTANCE}" \
    --project "${PROJECT_ID}" --zone "${ZONE}" --tunnel-through-iap --quiet \
    --command "$1"
}

gcloud_scp() {
  "${GCLOUD_BINARY}" compute scp "$1" "${INSTANCE}:$2" \
    --project "${PROJECT_ID}" --zone "${ZONE}" --tunnel-through-iap --quiet
}

front_status() {
  gcloud_ssh "sudo curl -fsS --unix-socket '${FRONT_SOCKET}' http://localhost/_subrouter/front-status"
}

slot_control_socket() {
  printf '%s/%s.sock\n' "${STATE_DIR}" "$1"
}

slot_service() {
  printf 'subrouter-slot@%s.service\n' "$1"
}

slot_address() {
  case "$1" in
    slot-a) printf '127.0.0.1:31417\n' ;;
    slot-b) printf '127.0.0.1:31418\n' ;;
    *) return 1 ;;
  esac
}

front_connections() {
  local status="$1"
  local slot="$2"
  jq -r --arg id "${slot}" '[.backends[]? | select(.id == $id) | .connections][0] // 0' <<<"${status}"
}

utc_now() {
  python3 -c 'from datetime import datetime, timezone; print(datetime.now(timezone.utc).isoformat(timespec="milliseconds").replace("+00:00", "Z"))'
}

epoch_millis() {
  python3 -c 'import time; print(time.time_ns() // 1_000_000)'
}

front_active_json() {
  jq -c '.active | {id,network,address}' <<<"$1"
}

front_backend_active() {
  local status="$1" slot="$2"
  jq -r --arg id "${slot}" '[.backends[]? | select(.id == $id) | .active][0] // false' <<<"${status}"
}

slot_status() {
  local slot="$1" socket
  socket="$(slot_control_socket "${slot}")"
  gcloud_ssh "sudo curl -fsS --unix-socket '${socket}' http://localhost/_subrouter/supervisor-status"
}

slot_snapshot() {
  local slot="$1" front="$2" fallback_generation="${3:-}"
  local status service_active front_active active_id active_connections inactive_connections
  front_active="$(front_backend_active "${front}" "${slot}")"
  if status="$(slot_status "${slot}" 2>/dev/null)"; then
    jq -e '
      (.accepting | type) == "boolean" and (.retiring | type) == "boolean" and
      (.active.id | type) == "string" and (.backends | type) == "array" and
      ([.backends[].connections] | all(type == "number" and . >= 0))
    ' <<<"${status}" >/dev/null || die "${slot} supervisor returned an invalid status schema"
    service_active="$(gcloud_ssh "if systemctl is-active --quiet '$(slot_service "${slot}")'; then echo true; else echo false; fi" | tail -n 1)"
    active_id="$(jq -r '.active.id' <<<"${status}")"
    active_connections="$(jq -r --arg id "${active_id}" '[.backends[] | select(.id == $id) | .connections][0] // -1' <<<"${status}")"
    inactive_connections="$(jq -r --arg id "${active_id}" '[.backends[] | select(.id != $id) | .connections] | add // 0' <<<"${status}")"
    [[ "${active_connections}" =~ ^[0-9]+$ && "${inactive_connections}" =~ ^[0-9]+$ ]] \
      || die "${slot} supervisor returned invalid generation connection counts"
    jq -nc --argjson status "${status}" --argjson front_active "${front_active}" \
      --argjson service_active "${service_active}" --arg active_id "${active_id}" \
      --argjson active_connections "${active_connections}" \
      --argjson inactive_connections "${inactive_connections}" \
      '{present:true,accepting:$status.accepting,retiring:$status.retiring,
        front_active:$front_active,active_generation:$active_id,
        active_connections:$active_connections,inactive_connections:$inactive_connections,
        service_active:$service_active}'
    return
  fi
  [[ -n "${fallback_generation}" ]] || die "${slot} supervisor status is unavailable"
  gcloud_ssh "! systemctl is-active --quiet '$(slot_service "${slot}")' && sudo test ! -S '$(slot_control_socket "${slot}")'" \
    || die "${slot} status disappeared without the service becoming absent"
  jq -nc --argjson front_active "${front_active}" --arg generation "${fallback_generation}" \
    '{present:false,accepting:false,retiring:true,front_active:$front_active,
      active_generation:$generation,active_connections:0,inactive_connections:0,
      service_active:false}'
}

validate_front_slot_status() {
  local status="$1"
  local slot="$2"
  local address
  address="$(slot_address "${slot}")"
  jq -e --arg id "${slot}" --arg address "${address}" \
    '.active.id == $id and .active.network == "tcp" and .active.address == $address' \
    <<<"${status}" >/dev/null
}

wait_for_front_active() {
  local expected="$1"
  local status active
  for _ in $(seq 1 300); do
    status="$(front_status)"
    active="$(jq -r '.active.id // empty' <<<"${status}")"
    [[ "${active}" == "${expected}" ]] && return 0
    sleep 0.1
  done
  return 1
}

wait_for_front_connections() {
  local slot="$1"
  local minimum="$2"
  local status count
  for _ in $(seq 1 300); do
    status="$(front_status)"
    count="$(front_connections "${status}" "${slot}")"
    [[ "${count}" =~ ^[0-9]+$ ]] || die "front returned an invalid connection count"
    (( count >= minimum )) && return 0
    sleep 0.1
  done
  return 1
}

# Deliberately no timeout. A slot with a live pinned connection is never
# stopped or force-cleared. Retiring its worker disables keepalive reuse, so the
# count reaches zero when real work closes naturally.
wait_for_front_drained() {
  local slot="$1"
  local status count iterations=0
  while true; do
    status="$(front_status)"
    count="$(front_connections "${status}" "${slot}")"
    [[ "${count}" =~ ^[0-9]+$ ]] || die "front returned an invalid connection count"
    if (( count == 0 )); then
      last_connection_closed_ms="$(epoch_millis)"
      return 0
    fi
    if (( iterations % 30 == 0 )); then
      log "waiting for ${slot} to drain naturally (${count} pinned connection(s))"
    fi
    iterations=$((iterations + 1))
    sleep 1
  done
}

wait_for_slot_absent() {
  local slot="$1" absent_ms
  [[ -n "${last_connection_closed_ms:-}" ]] || die "missing last-connection timestamp for ${slot}"
  for _ in $(seq 1 300); do
    if gcloud_ssh "! systemctl is-active --quiet '$(slot_service "${slot}")' && sudo test ! -S '$(slot_control_socket "${slot}")'"; then
      absent_ms="$(epoch_millis)"
      slot_absence_latency_ms=$((absent_ms - last_connection_closed_ms))
      (( slot_absence_latency_ms >= 0 && slot_absence_latency_ms < 30000 )) \
        || die "${slot} absence exceeded 30 seconds after its last held connection closed"
      return 0
    fi
    sleep 0.1
  done
  return 1
}

service_restarts() {
  gcloud_ssh "systemctl show '$(slot_service "$1")' -p NRestarts --value" | tail -n 1
}

service_oom_kills() {
  local service
  service="$(slot_service "$1")"
  gcloud_ssh "set -eu; cg=\$(systemctl show '${service}' -p ControlGroup --value); grep '^oom_kill ' /sys/fs/cgroup\${cg}/memory.events | cut -d' ' -f2" | tail -n 1
}

service_memory_peak() {
  local service
  service="$(slot_service "$1")"
  gcloud_ssh "set -eu; cg=\$(systemctl show '${service}' -p ControlGroup --value); cat /sys/fs/cgroup\${cg}/memory.peak" | tail -n 1
}

service_memory_max() {
  gcloud_ssh "systemctl show '$(slot_service "$1")' -p MemoryMax --value" | tail -n 1
}

front_memory_peak() {
  gcloud_ssh "set -eu; cg=\$(systemctl show subrouter-front.service -p ControlGroup --value); cat /sys/fs/cgroup\${cg}/memory.peak" | tail -n 1
}

front_memory_max() {
  gcloud_ssh "systemctl show subrouter-front.service -p MemoryMax --value" | tail -n 1
}

front_restarts() {
  gcloud_ssh "systemctl show subrouter-front.service -p NRestarts --value" | tail -n 1
}

front_oom_kills() {
  gcloud_ssh "set -eu; cg=\$(systemctl show subrouter-front.service -p ControlGroup --value); grep '^oom_kill ' /sys/fs/cgroup\${cg}/memory.events | cut -d' ' -f2" | tail -n 1
}

reset_service_memory_peak() {
  local service
  service="$(slot_service "$1")"
  gcloud_ssh "set -eu; cg=\$(systemctl show '${service}' -p ControlGroup --value); echo 0 | sudo tee /sys/fs/cgroup\${cg}/memory.peak >/dev/null"
}

declare -A rss_sampler_pids=()
declare -A rss_sampler_sentinels=()
declare -A rss_sampler_results=()

start_rss_sampler() {
  local target="$1" sentinel result log_path
  sentinel="/tmp/subrouter-rss-${RUN_LABEL}-${target}.running"
  result="/tmp/subrouter-rss-${RUN_LABEL}-${target}.peak"
  log_path="${ARTIFACT_DIR}/rss-${target}.log"
  gcloud_ssh "sudo rm -f '${result}' '${result}.tmp'; sudo touch '${sentinel}'"
  "${GCLOUD_BINARY}" compute ssh "${INSTANCE}" \
    --project "${PROJECT_ID}" --zone "${ZONE}" --tunnel-through-iap --quiet \
    --command "sudo bash '${REMOTE_INSTALLER}' sample-service-rss '${target}' '${RUN_LABEL}'" \
    >"${log_path}" 2>&1 &
  rss_sampler_pids["${target}"]=$!
  rss_sampler_sentinels["${target}"]="${sentinel}"
  rss_sampler_results["${target}"]="${result}"
  for _ in $(seq 1 100); do
    gcloud_ssh "sudo test -s '${result}'" >/dev/null 2>&1 && return 0
    kill -0 "${rss_sampler_pids[${target}]}" 2>/dev/null \
      || die "${target} RSS sampler exited before its first sample"
    sleep 0.05
  done
  die "${target} RSS sampler did not produce a sample"
}

stop_rss_sampler() {
  local target="$1" pid sentinel result peak
  pid="${rss_sampler_pids[${target}]:-}"
  sentinel="${rss_sampler_sentinels[${target}]:-}"
  result="${rss_sampler_results[${target}]:-}"
  [[ -n "${pid}" && -n "${sentinel}" && -n "${result}" ]] \
    || die "${target} RSS sampler was not started"
  gcloud_ssh "sudo rm -f '${sentinel}'"
  wait "${pid}" || die "${target} RSS sampler failed"
  peak="$(gcloud_ssh "sudo cat '${result}'" | tail -n 1)"
  [[ "${peak}" =~ ^[0-9]+$ ]] || die "${target} RSS sampler returned an invalid peak"
  unset "rss_sampler_pids[${target}]"
  printf '%s\n' "${peak}"
}

stop_all_rss_samplers() {
  local target pid sentinel
  for target in "${!rss_sampler_pids[@]}"; do
    pid="${rss_sampler_pids[${target}]}"
    sentinel="${rss_sampler_sentinels[${target}]}"
    gcloud_ssh "sudo rm -f '${sentinel}'" >/dev/null 2>&1 || true
    wait "${pid}" >/dev/null 2>&1 || true
    unset "rss_sampler_pids[${target}]"
  done
}

lock_holder_pid=""
deployment_started=0
deployment_committed=0
rollback_completed=0
rollback_failed=0
old_slot=""
candidate_slot=""
upgrade_requested_at=""
activated_at=""
last_connection_closed_ms=""
slot_absence_latency_ms=""

acquire_deploy_lock() {
  gcloud_ssh "umask 077; : > '${REMOTE_LOCK_SENTINEL}'; command -v flock >/dev/null"
  "${GCLOUD_BINARY}" compute ssh "${INSTANCE}" \
    --project "${PROJECT_ID}" --zone "${ZONE}" --tunnel-through-iap --quiet \
    --command "sudo flock -x -w 300 '${DEPLOY_LOCK_FILE}' sh -c 'echo LOCKED; while test -e \"${REMOTE_LOCK_SENTINEL}\"; do sleep 1; done'" \
    >"${ARTIFACT_DIR}/deploy-lock.log" 2>&1 &
  lock_holder_pid=$!
  for _ in $(seq 1 3100); do
    grep -qx LOCKED "${ARTIFACT_DIR}/deploy-lock.log" 2>/dev/null && return 0
    kill -0 "${lock_holder_pid}" 2>/dev/null || die "remote deployment lock holder exited"
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

persist_front_slot() {
  gcloud_ssh "sudo bash '${REMOTE_INSTALLER}' set-front-default '$1'"
}

switch_front() {
  local slot="$1"
  local address payload
  address="$(slot_address "${slot}")"
  payload="$(jq -nc --arg id "${slot}" --arg address "${address}" \
    '{id:$id,network:"tcp",address:$address}')"
  gcloud_ssh "sudo curl -fsS --unix-socket '${FRONT_SOCKET}' -X POST -H 'Content-Type: application/json' --data '${payload}' http://localhost/_subrouter/switch >/dev/null"
  wait_for_front_active "${slot}"
}

retire_slot() {
  gcloud_ssh "sudo bash '${REMOTE_INSTALLER}' retire-slot '$1'"
}

stop_drained_slot() {
  gcloud_ssh "sudo bash '${REMOTE_INSTALLER}' stop-drained-slot '$1'"
}

enable_slot() {
  gcloud_ssh "sudo bash '${REMOTE_INSTALLER}' enable-slot '$1'"
}

disable_slot() {
  gcloud_ssh "sudo bash '${REMOTE_INSTALLER}' disable-slot '$1'"
}

rollback_deployment() {
  [[ -n "${old_slot}" && -n "${candidate_slot}" ]] || return 1
  log "rolling front back to ${old_slot}"
  candidate_connections_before_rollback="$(front_connections "$(front_status)" "${candidate_slot}")" || return 1
  enable_slot "${old_slot}" || return 1
  # Persist first. If front restarts between these operations it selects the
  # same live rollback target that the control-plane switch will select.
  persist_front_slot "${old_slot}" || return 1
  switch_front "${old_slot}" || return 1
  disable_slot "${candidate_slot}" || return 1
  retire_slot "${candidate_slot}" || return 1
  restored_status="$(front_status)" || return 1
  [[ "$(jq -r '.active.id // empty' <<<"${restored_status}")" == "${old_slot}" ]] || return 1
  candidate_connections_after_rollback="$(front_connections "${restored_status}" "${candidate_slot}")" || return 1
  (( candidate_connections_after_rollback >= candidate_connections_before_rollback )) || return 1
  rollback_completed=1
  deployment_started=0
}

cleanup() {
  local status=$?
  trap - EXIT INT TERM
  set +e
  stop_all_rss_samplers
  if [[ "${deployment_started}" == "1" && "${deployment_committed}" == "0" && "${rollback_completed}" == "0" ]]; then
    if ! rollback_deployment; then
      rollback_failed=1
      status=1
      log "rollback failed; candidate slot and release artifacts were preserved" >&2
    fi
  fi
  if [[ "${status}" == "0" && "${rollback_failed}" == "0" ]]; then
    gcloud_ssh "rm -f '${REMOTE_CANDIDATE}' '${REMOTE_INSTALLER}' /tmp/subrouter-rss-${RUN_LABEL}-*" >/dev/null 2>&1 || true
  else
    log "preserving ${REMOTE_CANDIDATE} and ${REMOTE_INSTALLER} for recovery" >&2
  fi
  release_deploy_lock
  exit "${status}"
}
trap cleanup EXIT INT TERM

curl -fsS --max-time 10 "${PUBLIC_BASE_URL}/_subrouter/health" >/dev/null \
  || die "public health failed before ${DEPLOY_MODE}"
curl -fsS --max-time 10 "${PUBLIC_BASE_URL}/_subrouter/ready" >/dev/null \
  || die "public readiness failed before ${DEPLOY_MODE}"

acquire_deploy_lock
gcloud_ssh "sudo test -S '${FRONT_SOCKET}' && systemctl is-active --quiet subrouter-front.service" \
  || die "front topology is not installed; run the explicit migrate-front operation first"
initial_front_status="$(front_status)"
old_slot="$(jq -r '.active.id // empty' <<<"${initial_front_status}")"
[[ "${old_slot}" == "slot-a" || "${old_slot}" == "slot-b" ]] \
  || die "front has no valid active slot"
validate_front_slot_status "${initial_front_status}" "${old_slot}" \
  || die "front active slot metadata is inconsistent"
if [[ "${old_slot}" == "slot-a" ]]; then candidate_slot="slot-b"; else candidate_slot="slot-a"; fi
initial_front_active_json="$(front_active_json "${initial_front_status}")"
old_snapshot_before="$(slot_snapshot "${old_slot}" "${initial_front_status}")"
old_generation="$(jq -r '.active_generation' <<<"${old_snapshot_before}")"
jq -e '.accepting and (.retiring | not) and .front_active and .service_active and .inactive_connections == 0' \
  <<<"${old_snapshot_before}" >/dev/null \
  || die "old slot is not a clean accepting active generation"
old_installed_before="$(gcloud_ssh "sudo sha256sum '/opt/subrouter/slots/${old_slot}/worker' | awk '{print \$1}'" | tail -n 1)"
[[ "${old_installed_before}" =~ ^[0-9a-f]{64}$ ]] || die "old slot installed checksum is invalid"
old_slot_address="$(slot_address "${old_slot}")"
gcloud_ssh "systemctl is-active --quiet '$(slot_service "${old_slot}")' && systemctl is-enabled --quiet '$(slot_service "${old_slot}")' && grep -qx 'SUBROUTER_FRONT_BACKEND_ID=${old_slot}' /etc/default/subrouter-front && grep -qx 'SUBROUTER_FRONT_BACKEND_NETWORK=tcp' /etc/default/subrouter-front && grep -qx 'SUBROUTER_FRONT_BACKEND_ADDRESS=${old_slot_address}' /etc/default/subrouter-front" \
  || die "front live and persisted reboot targets do not both select ${old_slot}"

# A previous failed run may have left the inactive service enabled. It can be
# reused only after the front proves it has no pinned connections.
if gcloud_ssh "systemctl is-active --quiet '$(slot_service "${candidate_slot}")'"; then
  disable_slot "${candidate_slot}"
  retire_slot "${candidate_slot}"
  wait_for_front_drained "${candidate_slot}"
  wait_for_slot_absent "${candidate_slot}" \
    || die "inactive ${candidate_slot} did not exit after retirement"
  stop_drained_slot "${candidate_slot}" \
    || die "inactive ${candidate_slot} is still live and cannot be replaced"
fi

gcloud_scp "${DEPLOY_BINARY}" "${REMOTE_CANDIDATE}"
gcloud_scp "$(dirname "${BASH_SOURCE[0]}")/install-front-slots.sh" "${REMOTE_INSTALLER}"
gcloud_ssh "set -e; printf '%s  %s\n' '${EXPECTED_SHA256}' '${REMOTE_CANDIDATE}' | sha256sum -c - >/dev/null; sudo bash '${REMOTE_INSTALLER}' install-release '${RELEASE_TAG}' '${REMOTE_CANDIDATE}' '${EXPECTED_SHA256}'; sudo bash '${REMOTE_INSTALLER}' prepare-slot '${candidate_slot}' '${RELEASE_TAG}'"
gcloud_ssh "systemctl is-enabled --quiet '$(slot_service "${candidate_slot}")' && systemctl is-active --quiet '$(slot_service "${candidate_slot}")'" \
  || die "candidate slot is not enabled and active"
candidate_control_socket="$(slot_control_socket "${candidate_slot}")"
gcloud_ssh "sudo curl -fsS --unix-socket '${candidate_control_socket}' http://localhost/_subrouter/supervisor-status >/dev/null"
prepared_front_status="$(front_status)"
candidate_snapshot_before="$(slot_snapshot "${candidate_slot}" "${prepared_front_status}")"
candidate_generation="$(jq -r '.active_generation' <<<"${candidate_snapshot_before}")"
jq -e '.accepting and (.retiring | not) and (.front_active | not) and .service_active and .inactive_connections == 0' \
  <<<"${candidate_snapshot_before}" >/dev/null \
  || die "candidate slot is not a clean accepting generation"
[[ "${candidate_generation}" != "${old_generation}" ]] || die "candidate and old supervisor generations are identical"
candidate_installed="$(gcloud_ssh "sudo sha256sum '/opt/subrouter/slots/${candidate_slot}/worker' | awk '{print \$1}'" | tail -n 1)"
[[ "${candidate_installed}" == "${EXPECTED_SHA256}" ]] || die "candidate slot installed checksum changed"
reset_service_memory_peak "${candidate_slot}"
candidate_restarts_before="$(service_restarts "${candidate_slot}")"
candidate_oom_before="$(service_oom_kills "${candidate_slot}")"
old_restarts_before="$(service_restarts "${old_slot}")"
old_oom_before="$(service_oom_kills "${old_slot}")"
front_restarts_before="$(front_restarts)"
front_oom_before="$(front_oom_kills)"
candidate_memory_max="$(service_memory_max "${candidate_slot}")"
old_memory_max="$(service_memory_max "${old_slot}")"
front_memory_max_bytes="$(front_memory_max)"
[[ "${candidate_restarts_before}" =~ ^[0-9]+$ ]] || die "invalid candidate restart baseline"
[[ "${candidate_oom_before}" =~ ^[0-9]+$ ]] || die "invalid candidate OOM baseline"
[[ "${old_restarts_before}" =~ ^[0-9]+$ ]] || die "invalid old-slot restart baseline"
[[ "${old_oom_before}" =~ ^[0-9]+$ ]] || die "invalid old-slot OOM baseline"
[[ "${front_restarts_before}" =~ ^[0-9]+$ ]] || die "invalid front restart baseline"
[[ "${front_oom_before}" =~ ^[0-9]+$ ]] || die "invalid front OOM baseline"
[[ "${candidate_memory_max}" == "201326592" && "${old_memory_max}" == "201326592" ]] \
  || die "slot MemoryMax must be exactly 192 MiB"
[[ "${front_memory_max_bytes}" == "134217728" ]] || die "front MemoryMax must be exactly 128 MiB"
start_rss_sampler "${old_slot}"
start_rss_sampler "${candidate_slot}"
start_rss_sampler front

before_switch_status="$(front_status)"
old_connections_before_switch="$(front_connections "${before_switch_status}" "${old_slot}")"
candidate_connections_before_switch="$(front_connections "${before_switch_status}" "${candidate_slot}")"
(( old_connections_before_switch >= EXPECTED_ORIGINAL_CONNECTIONS )) \
  || die "only ${old_connections_before_switch}/${EXPECTED_ORIGINAL_CONNECTIONS} externally held original connections were pinned before the switch"
old_snapshot_at_switch="$(slot_snapshot "${old_slot}" "${before_switch_status}")"
old_generation_connections="$(jq -r '.active_connections' <<<"${old_snapshot_at_switch}")"
(( old_generation_connections >= EXPECTED_ORIGINAL_CONNECTIONS )) \
  || die "old supervisor did not correlate every original front connection to its active generation"
jq -e '.inactive_connections == 0 and .accepting and (.retiring | not)' \
  <<<"${old_snapshot_at_switch}" >/dev/null \
  || die "old supervisor had an accepting or inactive-generation inconsistency at the switch"

log "persisting and switching front to ${candidate_slot}"
deployment_started=1
upgrade_requested_at="$(utc_now)"
upgrade_requested_ms="$(epoch_millis)"
persist_front_slot "${candidate_slot}"
if ! switch_front "${candidate_slot}"; then
  persist_front_slot "${old_slot}" || true
  die "front did not activate ${candidate_slot}"
fi
provisional_switch_at="$(utc_now)"
disable_slot "${old_slot}"
switched_status="$(front_status)"
old_connections_after_switch="$(front_connections "${switched_status}" "${old_slot}")"
(( old_connections_after_switch >= EXPECTED_ORIGINAL_CONNECTIONS )) \
  || die "slot switch cut live connections: ${old_connections_after_switch} remain"
switched_front_active_json="$(front_active_json "${switched_status}")"
old_snapshot_after_switch="$(slot_snapshot "${old_slot}" "${switched_status}")"
jq -e '.accepting and (.retiring | not) and (.front_active | not) and .inactive_connections == 0' \
  <<<"${old_snapshot_after_switch}" >/dev/null \
  || die "old slot state changed unexpectedly during the front switch"

ack_challenge="$(python3 -c 'import secrets; print(secrets.token_hex(16))')"
ack_request_tmp="$(mktemp "${GOLDEN_ACK_REQUEST}.tmp.XXXXXX")"
jq -n --arg schema 'subrouter.gcp.slot-activation-ack-request/v1' \
  --arg challenge "${ack_challenge}" --arg run_id "${RUN_LABEL}" \
  --arg project "${PROJECT_ID}" --arg zone "${ZONE}" --arg instance "${INSTANCE}" \
  --arg old_slot "${old_slot}" --arg candidate_slot "${candidate_slot}" \
  --arg old_generation "${old_generation}" --arg candidate_generation "${candidate_generation}" \
  --arg requested_at "${upgrade_requested_at}" --arg switched_at "${provisional_switch_at}" \
  --argjson clients "${CONFIGURED_CODEX_CLIENTS}" \
  --argjson originals "${EXPECTED_ORIGINAL_CONNECTIONS}" \
  --argjson candidate_connections "${EXPECTED_ROLLBACK_CONNECTIONS}" \
  '{schema:$schema,challenge:$challenge,
    run:{id:$run_id,project:$project,zone:$zone,instance:$instance},
    slots:{old:$old_slot,candidate:$candidate_slot,old_generation:$old_generation,
      candidate_generation:$candidate_generation},
    configured_original_clients:$clients,expected_original_slot_connections:$originals,
    expected_fresh_candidate_direct_connections:$candidate_connections,
    upgrade_requested_at:$requested_at,provisional_switch_at:$switched_at}' \
  >"${ack_request_tmp}"
chmod 0600 "${ack_request_tmp}"
mv -f -- "${ack_request_tmp}" "${GOLDEN_ACK_REQUEST}"

while [[ ! -f "${GOLDEN_ACK}" || -L "${GOLDEN_ACK}" ]]; do
  (( $(epoch_millis) - upgrade_requested_ms < 30000 )) \
    || die "golden activation acknowledgement was not observed strictly before 30 seconds"
  sleep 0.05
done
ack_received_at="$(utc_now)"
ack_received_ms="$(epoch_millis)"
(( ack_received_ms - upgrade_requested_ms < 30000 )) \
  || die "golden activation acknowledgement arrived at or after 30 seconds"
python3 - "${GOLDEN_ACK}" "${ack_challenge}" "${candidate_slot}" "${candidate_generation}" \
  "${upgrade_requested_at}" "${provisional_switch_at}" "${ack_received_at}" <<'PY'
from datetime import datetime, timedelta
import json
from pathlib import Path
import sys

path, challenge, candidate_slot, candidate_generation, requested_raw, switched_raw, received_raw = sys.argv[1:]
document = json.loads(Path(path).read_text())
expected = {
    "schema": "subrouter.gcp.slot-activation-ack/v1",
    "challenge": challenge,
    "candidate_slot": candidate_slot,
    "candidate_generation": candidate_generation,
    "configured_original_clients": 4,
    "original_streams_crossed": 4,
    "direct_original_connections_verified": 2,
    "local_egress_clients_verified": 2,
    "all_original_streams_crossed_activation": True,
    "processes_stable": True,
    "sockets_stable": True,
    "local_egress_verified": True,
    "fresh_candidate_direct_connection": True,
}
for key, value in expected.items():
    if document.get(key) != value:
        raise SystemExit(f"golden acknowledgement {key} does not match the request")
connection_id = document.get("fresh_candidate_connection_id")
if not isinstance(connection_id, str) or not connection_id:
    raise SystemExit("golden acknowledgement fresh_candidate_connection_id is required")
parse = lambda value: datetime.fromisoformat(value.replace("Z", "+00:00"))
requested = parse(requested_raw)
switched = parse(switched_raw)
activated = parse(document.get("activated_at", ""))
received = parse(received_raw)
if not requested <= switched <= activated <= received:
    raise SystemExit("golden activation acknowledgement timestamps are out of order")
if activated - requested >= timedelta(seconds=30) or received - requested >= timedelta(seconds=30):
    raise SystemExit("golden activation acknowledgement was not completed strictly before 30 seconds")
PY
activated_at="$(jq -r '.activated_at' "${GOLDEN_ACK}")"
golden_ack_sha256="$(sha256sum "${GOLDEN_ACK}" | awk '{print $1}')"
fresh_candidate_connection_id="$(jq -r '.fresh_candidate_connection_id' "${GOLDEN_ACK}")"
acknowledged_front_status="$(front_status)"
candidate_connections_after_ack="$(front_connections "${acknowledged_front_status}" "${candidate_slot}")"
(( candidate_connections_after_ack >= candidate_connections_before_switch + EXPECTED_ROLLBACK_CONNECTIONS )) \
  || die "golden fresh direct connection was not correlated to the candidate front backend"
candidate_connection_count_delta=$((candidate_connections_after_ack - candidate_connections_before_switch))
candidate_snapshot_after_ack="$(slot_snapshot "${candidate_slot}" "${acknowledged_front_status}")"
jq -e --arg generation "${candidate_generation}" --argjson minimum "${EXPECTED_ROLLBACK_CONNECTIONS}" \
  '.active_generation == $generation and .active_connections >= $minimum and
   .inactive_connections == 0 and .accepting and (.retiring | not) and .front_active' \
  <<<"${candidate_snapshot_after_ack}" >/dev/null \
  || die "golden fresh direct connection was not correlated to the candidate generation"

retirement_target="${old_slot}"
retirement_evidence_file_required=true
retirement_requested_json=null
retirement_state="not-requested"

# Return promptly after the externally observed phase boundary. Health probes
# are bounded and do not create or wait for synthetic Codex sessions.
gcloud_ssh "curl -fsS --max-time 5 http://127.0.0.1:31416/_subrouter/health >/dev/null && curl -fsS --max-time 5 http://127.0.0.1:31416/_subrouter/ready >/dev/null"
curl -fsS --max-time 10 "${PUBLIC_BASE_URL}/_subrouter/health" >/dev/null
curl -fsS --max-time 10 "${PUBLIC_BASE_URL}/_subrouter/ready" >/dev/null

deployment_action="activated"
active_slot="${candidate_slot}"

candidate_restarts_after="$(service_restarts "${candidate_slot}")"
candidate_oom_after="$(service_oom_kills "${candidate_slot}")"
old_restarts_after="$(service_restarts "${old_slot}")"
old_oom_after="$(service_oom_kills "${old_slot}")"
front_restarts_after="$(front_restarts)"
front_oom_after="$(front_oom_kills)"
[[ "${candidate_restarts_after}" == "${candidate_restarts_before}" ]] \
  || die "candidate restart count changed: ${candidate_restarts_before} -> ${candidate_restarts_after}"
[[ "${candidate_oom_after}" == "${candidate_oom_before}" ]] \
  || die "candidate OOM count changed: ${candidate_oom_before} -> ${candidate_oom_after}"
[[ "${old_restarts_after}" == "${old_restarts_before}" ]] \
  || die "old-slot restart count changed: ${old_restarts_before} -> ${old_restarts_after}"
[[ "${old_oom_after}" == "${old_oom_before}" ]] \
  || die "old-slot OOM count changed: ${old_oom_before} -> ${old_oom_after}"
[[ "${front_restarts_after}" == "${front_restarts_before}" ]] \
  || die "front restart count changed: ${front_restarts_before} -> ${front_restarts_after}"
[[ "${front_oom_after}" == "${front_oom_before}" ]] \
  || die "front OOM count changed: ${front_oom_before} -> ${front_oom_after}"
old_peak_rss="$(stop_rss_sampler "${old_slot}")"
candidate_peak_rss="$(stop_rss_sampler "${candidate_slot}")"
front_peak_rss="$(stop_rss_sampler front)"
(( old_peak_rss <= old_memory_max )) \
  || die "old slot peak RSS ${old_peak_rss} exceeds ${old_memory_max}"
(( candidate_peak_rss <= candidate_memory_max )) \
  || die "candidate slot peak RSS ${candidate_peak_rss} exceeds ${candidate_memory_max}"
(( front_peak_rss <= front_memory_max_bytes )) \
  || die "front peak RSS ${front_peak_rss} exceeds ${front_memory_max_bytes}"

old_snapshot_after="${old_snapshot_after_switch}"

final_status="$(front_status)"
final_active="$(jq -r '.active.id // empty' <<<"${final_status}")"
[[ "${final_active}" == "${active_slot}" ]] || die "front active slot changed unexpectedly"
validate_front_slot_status "${final_status}" "${active_slot}" \
  || die "front final active slot metadata is inconsistent"
active_slot_address="$(slot_address "${active_slot}")"
gcloud_ssh "grep -qx 'SUBROUTER_FRONT_BACKEND_ID=${active_slot}' /etc/default/subrouter-front && grep -qx 'SUBROUTER_FRONT_BACKEND_NETWORK=tcp' /etc/default/subrouter-front && grep -qx 'SUBROUTER_FRONT_BACKEND_ADDRESS=${active_slot_address}' /etc/default/subrouter-front" \
  || die "persisted front backend does not match active ${active_slot}"
gcloud_ssh "systemctl is-enabled --quiet '$(slot_service "${active_slot}")' && systemctl is-active --quiet '$(slot_service "${active_slot}")'"
curl -fsS --max-time 10 "${PUBLIC_BASE_URL}/_subrouter/health" >/dev/null
curl -fsS --max-time 10 "${PUBLIC_BASE_URL}/_subrouter/ready" >/dev/null
remote_release="/opt/subrouter/releases/${RELEASE_TAG}/subrouter"
remote_sum="$(gcloud_ssh "sudo sha256sum '${remote_release}' | awk '{print \$1}'" | tail -n 1)"
[[ "${remote_sum}" == "${EXPECTED_SHA256}" ]] || die "retained release checksum changed"
installed_after="$(gcloud_ssh "sudo sha256sum '/opt/subrouter/slots/${active_slot}/worker' | awk '{print \$1}'" | tail -n 1)"
[[ "${installed_after}" =~ ^[0-9a-f]{64}$ ]] || die "final active slot checksum is invalid"
if [[ "${active_slot}" == "${candidate_slot}" ]]; then
  [[ "${installed_after}" == "${candidate_installed}" ]] || die "final candidate checksum changed"
else
  [[ "${installed_after}" == "${old_installed_before}" ]] || die "rollback did not restore the original checksum"
fi
final_front_active_json="$(front_active_json "${final_status}")"
evidence_emitted_at="$(utc_now)"
evidence_type="slot-activation"
rollback_performed=false
rollback_requested_json=null
rollback_activated_json=null
rollback_from=""
rollback_to=""
last_connection_closed_json=null
absent_at_json=null
absence_latency_json=null

evidence_tmp="$(mktemp "${EVIDENCE_JSON}.tmp.XXXXXX")"
jq -n \
  --arg schema "subrouter.gcp.deploy-evidence/v1" \
  --arg evidence_type "${evidence_type}" --arg action "${deployment_action}" --arg mode "activation" \
  --arg intent "${ACTIVATION_INTENT}" \
  --arg run_id "${RUN_LABEL}" \
  --arg project "${PROJECT_ID}" --arg zone "${ZONE}" --arg instance "${INSTANCE}" \
  --arg release_tag "${RELEASE_TAG}" --arg release_sha256 "${EXPECTED_SHA256}" \
  --arg deploy_revision "${DEPLOY_REVISION}" --arg old_slot "${old_slot}" \
  --arg candidate_slot "${candidate_slot}" --arg active_slot "${active_slot}" \
  --arg old_generation "${old_generation}" --arg candidate_generation "${candidate_generation}" \
  --arg installed_before "${old_installed_before}" --arg candidate_installed "${candidate_installed}" \
  --arg installed_after "${installed_after}" \
  --arg upgrade_requested_at "${upgrade_requested_at}" --arg provisional_switch_at "${provisional_switch_at}" \
  --arg activated_at "${activated_at}" --arg ack_received_at "${ack_received_at}" \
  --arg evidence_emitted_at "${evidence_emitted_at}" \
  --arg golden_ack_sha "${golden_ack_sha256}" --arg ack_challenge "${ack_challenge}" \
  --arg fresh_connection_id "${fresh_candidate_connection_id}" \
  --argjson front_before "${initial_front_active_json}" \
  --argjson front_after "${switched_front_active_json}" \
  --argjson front_final "${final_front_active_json}" \
  --argjson old_before "${old_snapshot_at_switch}" --argjson old_after "${old_snapshot_after}" \
  --argjson pinned_connections "${old_connections_after_switch}" \
  --argjson codex_sessions "${CONFIGURED_CODEX_CLIENTS}" \
  --argjson direct_connections "${EXPECTED_ORIGINAL_CONNECTIONS}" --argjson transports '[]' \
  --argjson rollback_sessions "${EXPECTED_ROLLBACK_CONNECTIONS}" \
  --argjson candidate_connections_before "${candidate_connections_before_switch}" \
  --argjson candidate_connections_after "${candidate_connections_after_ack}" \
  --argjson candidate_connection_delta "${candidate_connection_count_delta}" \
  --argjson old_restarts_before "${old_restarts_before}" --argjson old_restarts_after "${old_restarts_after}" \
  --argjson old_oom_before "${old_oom_before}" --argjson old_oom_after "${old_oom_after}" \
  --argjson candidate_restarts_before "${candidate_restarts_before}" \
  --argjson candidate_restarts_after "${candidate_restarts_after}" \
  --argjson candidate_oom_before "${candidate_oom_before}" --argjson candidate_oom_after "${candidate_oom_after}" \
  --argjson front_restarts_before "${front_restarts_before}" --argjson front_restarts_after "${front_restarts_after}" \
  --argjson front_oom_before "${front_oom_before}" --argjson front_oom_after "${front_oom_after}" \
  --argjson old_peak_rss "${old_peak_rss}" --argjson candidate_peak_rss "${candidate_peak_rss}" \
  --argjson front_peak_rss "${front_peak_rss}" --argjson slot_memory_max "${MAX_MEMORY_BYTES}" \
  --argjson front_memory_max "${front_memory_max_bytes}" \
  --argjson rollback_performed "${rollback_performed}" \
  --argjson rollback_requested_at "${rollback_requested_json}" \
  --argjson rollback_activated_at "${rollback_activated_json}" \
  --arg rollback_from "${rollback_from}" --arg rollback_to "${rollback_to}" \
  --arg retirement_target "${retirement_target}" --argjson retirement_requested_at "${retirement_requested_json}" \
  --arg retirement_state "${retirement_state}" \
  --argjson retirement_evidence_required "${retirement_evidence_file_required}" \
  --argjson last_connection_closed_at "${last_connection_closed_json}" \
  --argjson absent_at "${absent_at_json}" --argjson absence_latency_ms "${absence_latency_json}" \
  '{schema:$schema,evidence_type:$evidence_type,mode:$mode,intent:$intent,success:true,action:$action,
    run:{id:$run_id,project:$project,zone:$zone,instance:$instance},
    release:{tag:$release_tag,sha256:$release_sha256,source_revision:$deploy_revision,
      tag_on_main:true,attestation_verified:true,immutable:true},
    slots:{before:$old_slot,candidate:$candidate_slot,final:$active_slot,
      old_generation:$old_generation,candidate_generation:$candidate_generation},
    checksums:{installed_before:$installed_before,candidate_installed:$candidate_installed,
      installed_after:$installed_after},
    timestamps:{upgrade_requested_at:$upgrade_requested_at,activated_at:$activated_at,
      provisional_switch_at:$provisional_switch_at,golden_ack_received_at:$ack_received_at,
      evidence_emitted_at:$evidence_emitted_at},
    golden_ack:{sha256:$golden_ack_sha,challenge:$ack_challenge,
      fresh_candidate_connection_id:$fresh_connection_id,
      configured_original_clients:4,original_streams_crossed:4,
      direct_original_connections_verified:2,local_egress_clients_verified:2,
      all_original_streams_crossed_activation:true,processes_stable:true,
      sockets_stable:true,local_egress_verified:true,
      fresh_candidate_direct_connection:true,activated_at:$activated_at,
      received_at:$ack_received_at},
    front:{active_before:$front_before,active_after:$front_after,active_final:$front_final},
    old_slot:{before:$old_before,after:$old_after},
    metrics:{
      old_slot:{nrestarts:{before:$old_restarts_before,after:$old_restarts_after},
        oom_kill:{before:$old_oom_before,after:$old_oom_after},
        run_scoped_peak_rss_bytes:$old_peak_rss,memory_max_bytes:$slot_memory_max},
      candidate_slot:{nrestarts:{before:$candidate_restarts_before,after:$candidate_restarts_after},
        oom_kill:{before:$candidate_oom_before,after:$candidate_oom_after},
        run_scoped_peak_rss_bytes:$candidate_peak_rss,memory_max_bytes:$slot_memory_max},
      front:{nrestarts:{before:$front_restarts_before,after:$front_restarts_after},
        oom_kill:{before:$front_oom_before,after:$front_oom_after},
        run_scoped_peak_rss_bytes:$front_peak_rss,memory_max_bytes:$front_memory_max}},
    continuity:{configured_original_clients:$codex_sessions,
      expected_original_slot_connections:$direct_connections,
      pinned_original_connections_at_switch:$pinned_connections,
      expected_candidate_connections_for_rollback:$rollback_sessions,
      candidate_connections_before:$candidate_connections_before,
      candidate_connections_after_ack:$candidate_connections_after,
      candidate_connection_count_delta:$candidate_connection_delta,
      all_expected_slot_connections_pinned:($pinned_connections >= $direct_connections),
      transports:$transports,resumed_contexts:0,resume_nonce_verified:false,
      ci_evidence_role:"supplemental",golden_gate_role:"external-required"},
    rollback:{performed:$rollback_performed,requested_at:$rollback_requested_at,
      activated_at:$rollback_activated_at,
      from:(if $rollback_performed then $rollback_from else null end),
      to:(if $rollback_performed then $rollback_to else null end)},
    retirement:{target:$retirement_target,requested_at:$retirement_requested_at,
      state:$retirement_state,evidence_file_required:$retirement_evidence_required,
      last_connection_closed_at:$last_connection_closed_at,absent_at:$absent_at,
      absence_latency_ms:$absence_latency_ms}}' >"${evidence_tmp}"
python3 "$(dirname "${BASH_SOURCE[0]}")/validate-deploy-evidence.py" \
  --expect "${evidence_type}" "${evidence_tmp}" >/dev/null
chmod 0600 "${evidence_tmp}"
mv -f -- "${evidence_tmp}" "${EVIDENCE_JSON}"
deployment_committed=1

gcloud_ssh "rm -f '${REMOTE_CANDIDATE}' '${REMOTE_INSTALLER}'"
log "slot activation passed: ${old_slot} -> ${active_slot}; external originals remain live"
jq -c . "${EVIDENCE_JSON}"
