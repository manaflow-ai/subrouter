#!/usr/bin/env bash
# Deploy one checksum-verified release into the inactive supervisor slot while
# real Codex HTTP and WebSocket sessions stay attached through the stable front.
set -euo pipefail

PROJECT_ID="${SUBROUTER_GCP_PROJECT:?set SUBROUTER_GCP_PROJECT}"
ZONE="${SUBROUTER_GCP_ZONE:?set SUBROUTER_GCP_ZONE}"
INSTANCE="${SUBROUTER_GCP_INSTANCE:?set SUBROUTER_GCP_INSTANCE}"
RELEASE_TAG="${SUBROUTER_RELEASE_TAG:?set SUBROUTER_RELEASE_TAG}"
DEPLOY_BINARY="${SUBROUTER_DEPLOY_BINARY:?set SUBROUTER_DEPLOY_BINARY}"
CLIENT_BINARY="${SUBROUTER_CLIENT_BINARY:-${DEPLOY_BINARY}}"
RELEASE_SHA256_FILE="${SUBROUTER_RELEASE_SHA256_FILE:-${DEPLOY_BINARY}.sha256}"
TRANSPORT_OBSERVER="${SUBROUTER_TRANSPORT_OBSERVER:?set SUBROUTER_TRANSPORT_OBSERVER}"
CODEX_BINARY="${SUBROUTER_CODEX_BIN:-codex}"
GCLOUD_BINARY="${GCLOUD_BIN:-gcloud}"
CLIENT_COUNT="${SUBROUTER_LIVE_CLIENTS:-4}"
MIN_DRAINED_CLIENTS="${SUBROUTER_MIN_DRAINED_CLIENTS:-2}"
CODEX_MODEL="${SUBROUTER_CODEX_MODEL:-gpt-5.6-sol}"
MAX_MEMORY_BYTES="${SUBROUTER_MAX_MEMORY_BYTES:-201326592}"
PUBLIC_BASE_URL="${SUBROUTER_PUBLIC_BASE_URL:?set SUBROUTER_PUBLIC_BASE_URL}"
DEPLOY_CLIENT_BASE_URL="${SUBROUTER_DEPLOY_CLIENT_BASE_URL:?set SUBROUTER_DEPLOY_CLIENT_BASE_URL}"
DEPLOY_TENANT_KEY="${SUBROUTER_DEPLOY_TENANT_KEY:?set SUBROUTER_DEPLOY_TENANT_KEY}"
DEPLOY_REVISION="${SUBROUTER_DEPLOY_REVISION:-${RELEASE_TAG}}"
DEPLOY_MODE="${SUBROUTER_DEPLOY_MODE:-deploy}"
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

for command in "${GCLOUD_BINARY}" "${CODEX_BINARY}" jq curl python3 sha256sum; do
  command -v "${command}" >/dev/null 2>&1 || die "required command not found: ${command}"
done
[[ -x "${DEPLOY_BINARY}" ]] || die "deploy binary is not executable: ${DEPLOY_BINARY}"
[[ -x "${CLIENT_BINARY}" ]] || die "client binary is not executable: ${CLIENT_BINARY}"
[[ -x "${TRANSPORT_OBSERVER}" ]] || die "transport observer is not executable: ${TRANSPORT_OBSERVER}"
[[ -f "${RELEASE_SHA256_FILE}" ]] || die "release checksum file is missing: ${RELEASE_SHA256_FILE}"
[[ "${RELEASE_TAG}" =~ ^v[0-9]+\.[0-9]+\.[0-9]+([.-][0-9A-Za-z.-]+)?$ ]] \
  || die "SUBROUTER_RELEASE_TAG must be an explicit version tag"
[[ "${CLIENT_COUNT}" =~ ^[0-9]+$ ]] || die "SUBROUTER_LIVE_CLIENTS must be an integer"
[[ "${MIN_DRAINED_CLIENTS}" =~ ^[0-9]+$ ]] || die "SUBROUTER_MIN_DRAINED_CLIENTS must be an integer"
[[ "${MAX_MEMORY_BYTES}" =~ ^[0-9]+$ ]] || die "SUBROUTER_MAX_MEMORY_BYTES must be an integer"
(( CLIENT_COUNT >= 2 )) || die "SUBROUTER_LIVE_CLIENTS must be at least 2"
(( MIN_DRAINED_CLIENTS > 0 )) || die "SUBROUTER_MIN_DRAINED_CLIENTS must be positive"
(( CLIENT_COUNT >= MIN_DRAINED_CLIENTS )) || die "live client count must cover the drain minimum"
(( MAX_MEMORY_BYTES > 0 )) || die "SUBROUTER_MAX_MEMORY_BYTES must be positive"
[[ "${DEPLOY_MODE}" == "deploy" || "${DEPLOY_MODE}" == "rollback-rehearsal" ]] \
  || die "SUBROUTER_DEPLOY_MODE must be deploy or rollback-rehearsal"
[[ "${PUBLIC_BASE_URL}" =~ ^https://[^/?#]+/?$ ]] \
  || die "SUBROUTER_PUBLIC_BASE_URL must be an HTTPS origin"
[[ "${DEPLOY_CLIENT_BASE_URL}" =~ ^https://[^/?#]+/?$ ]] \
  || die "SUBROUTER_DEPLOY_CLIENT_BASE_URL must be an HTTPS origin"
[[ "${DEPLOY_TENANT_KEY}" =~ ^srt_[0-9a-f]{32,}$ ]] \
  || die "SUBROUTER_DEPLOY_TENANT_KEY is not a valid tenant key"
PUBLIC_BASE_URL="${PUBLIC_BASE_URL%/}"
CLIENT_BASE_URL="${DEPLOY_CLIENT_BASE_URL%/}/t/${DEPLOY_TENANT_KEY}"
EXPECTED_SHA256="$(tr -d '[:space:]' <"${RELEASE_SHA256_FILE}")"
[[ "${EXPECTED_SHA256}" =~ ^[0-9a-f]{64}$ ]] || die "release checksum is invalid"
CANDIDATE_SHA256="$(sha256sum "${DEPLOY_BINARY}" | awk '{print $1}')"
CLIENT_SHA256="$(sha256sum "${CLIENT_BINARY}" | awk '{print $1}')"
[[ "${CANDIDATE_SHA256}" == "${EXPECTED_SHA256}" ]] \
  || die "deploy binary does not match the verified release checksum"
[[ "${CLIENT_SHA256}" == "${EXPECTED_SHA256}" ]] \
  || die "client wrapper is not the same verified release artifact"

mkdir -p "${ARTIFACT_DIR}"
ARTIFACT_DIR="$(cd "${ARTIFACT_DIR}" && pwd)"
WORK_DIR="${ARTIFACT_DIR}/work"
mkdir -p "${WORK_DIR}"

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
      return 0
    fi
    if (( iterations % 30 == 0 )); then
      log "waiting for ${slot} to drain naturally (${count} pinned connection(s))"
    fi
    iterations=$((iterations + 1))
    sleep 1
  done
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

front_memory_peak() {
  gcloud_ssh "set -eu; cg=\$(systemctl show subrouter-front.service -p ControlGroup --value); cat /sys/fs/cgroup\${cg}/memory.peak" | tail -n 1
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

free_local_port() {
  python3 - <<'PY'
import socket
s = socket.socket()
s.bind(("127.0.0.1", 0))
print(s.getsockname()[1])
s.close()
PY
}

wait_for_local_endpoint() {
  local endpoint="$1"
  local label="$2"
  for _ in $(seq 1 60); do
    if curl -fsS --max-time 2 "${TUNNEL_BASE_URL}${endpoint}" >/dev/null 2>&1; then
      return 0
    fi
    if [[ -z "${tunnel_pid:-}" ]] || ! kill -0 "${tunnel_pid}" 2>/dev/null; then
      die "IAP tunnel exited while waiting for ${label}"
    fi
    sleep 0.5
  done
  die "${label} did not pass through the IAP tunnel"
}

wait_for_client_thread_id() {
  local wrapper_pid="$1"
  local log_path="$2"
  local thread_id=""
  for _ in $(seq 1 200); do
    thread_id="$(jq -Rr 'fromjson? | select(.type == "thread.started") | .thread_id' "${log_path}" 2>/dev/null | head -n 1)"
    [[ -n "${thread_id}" ]] && printf '%s\n' "${thread_id}" && return 0
    kill -0 "${wrapper_pid}" 2>/dev/null || return 1
    sleep 0.05
  done
  return 1
}

wait_for_observed_transport() {
  local event_path="$1"
  local expected="$2"
  local observed=""
  for _ in $(seq 1 200); do
    observed="$(jq -Rr --arg expected "${expected}" \
      'fromjson? | select(.transport == $expected and (.path | endswith("/responses"))) | .transport' \
      "${event_path}" 2>/dev/null | head -n 1)"
    [[ "${observed}" == "${expected}" ]] && printf '%s\n' "${observed}" && return 0
    sleep 0.05
  done
  return 1
}

client_wrapper_pids=()
client_modes=()
client_homes=()
client_logs=()
client_last_messages=()
client_thread_ids=()
client_observer_events=()
observer_pids=()
tunnel_pid=""
lock_holder_pid=""
deployment_started=0
deployment_committed=0
rollback_completed=0
rollback_failed=0
old_slot=""
candidate_slot=""

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
  enable_slot "${old_slot}" || return 1
  # Persist first. If front restarts between these operations it selects the
  # same live rollback target that the control-plane switch will select.
  persist_front_slot "${old_slot}" || return 1
  switch_front "${old_slot}" || return 1
  disable_slot "${candidate_slot}" || return 1
  retire_slot "${candidate_slot}" || return 1
  wait_for_front_drained "${candidate_slot}" || return 1
  stop_drained_slot "${candidate_slot}" || return 1
  rollback_completed=1
  deployment_started=0
}

cleanup() {
  local status=$?
  trap - EXIT INT TERM
  set +e
  if [[ "${deployment_started}" == "1" && "${deployment_committed}" == "0" && "${rollback_completed}" == "0" ]]; then
    if ! rollback_deployment; then
      rollback_failed=1
      status=1
      log "rollback failed; candidate slot and release artifacts were preserved" >&2
    fi
  fi
  for pid in "${client_wrapper_pids[@]:-}"; do
    kill -TERM "${pid}" 2>/dev/null || true
  done
  for pid in "${observer_pids[@]:-}"; do
    kill -TERM "${pid}" 2>/dev/null || true
  done
  if [[ -n "${tunnel_pid}" ]]; then
    kill "${tunnel_pid}" 2>/dev/null || true
    wait "${tunnel_pid}" 2>/dev/null || true
  fi
  if [[ "${status}" == "0" && "${rollback_failed}" == "0" ]]; then
    gcloud_ssh "rm -f '${REMOTE_CANDIDATE}' '${REMOTE_INSTALLER}'" >/dev/null 2>&1 || true
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
old_slot_address="$(slot_address "${old_slot}")"
gcloud_ssh "systemctl is-active --quiet '$(slot_service "${old_slot}")' && systemctl is-enabled --quiet '$(slot_service "${old_slot}")' && grep -qx 'SUBROUTER_FRONT_BACKEND_ID=${old_slot}' /etc/default/subrouter-front && grep -qx 'SUBROUTER_FRONT_BACKEND_NETWORK=tcp' /etc/default/subrouter-front && grep -qx 'SUBROUTER_FRONT_BACKEND_ADDRESS=${old_slot_address}' /etc/default/subrouter-front" \
  || die "front live and persisted reboot targets do not both select ${old_slot}"

# A previous failed run may have left the inactive service enabled. It can be
# reused only after the front proves it has no pinned connections.
if gcloud_ssh "systemctl is-active --quiet '$(slot_service "${candidate_slot}")'"; then
  wait_for_front_drained "${candidate_slot}"
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
reset_service_memory_peak "${candidate_slot}"
restarts_before="$(service_restarts "${candidate_slot}")"
oom_before="$(service_oom_kills "${candidate_slot}")"
front_restarts_before="$(front_restarts)"
front_oom_before="$(front_oom_kills)"
[[ "${restarts_before}" =~ ^[0-9]+$ ]] || die "invalid candidate restart baseline"
[[ "${oom_before}" =~ ^[0-9]+$ ]] || die "invalid candidate OOM baseline"
[[ "${front_restarts_before}" =~ ^[0-9]+$ ]] || die "invalid front restart baseline"
[[ "${front_oom_before}" =~ ^[0-9]+$ ]] || die "invalid front OOM baseline"

local_port="$(free_local_port)"
TUNNEL_BASE_URL="http://127.0.0.1:${local_port}"
log "opening an IAP tunnel to the stable front"
"${GCLOUD_BINARY}" compute ssh "${INSTANCE}" \
  --project "${PROJECT_ID}" --zone "${ZONE}" --tunnel-through-iap --quiet \
  -- -N -o ExitOnForwardFailure=yes -o ServerAliveInterval=15 \
  -L "127.0.0.1:${local_port}:127.0.0.1:31416" &
tunnel_pid=$!
wait_for_local_endpoint "/_subrouter/health" "front health"
wait_for_local_endpoint "/_subrouter/ready" "front readiness"

start_live_client() {
  local index="$1"
  local mode="$2"
  local home="${WORK_DIR}/codex-${index}"
  local log_path="${ARTIFACT_DIR}/client-${index}-${mode}.jsonl"
  local last_path="${ARTIFACT_DIR}/client-${index}-${mode}.last.txt"
  local observer_log="${ARTIFACT_DIR}/observer-${index}-${mode}.log"
  local observer_events="${ARTIFACT_DIR}/observer-${index}-${mode}.jsonl"
  local observer_port observer_pid marker prompt
  local extra=()
  observer_port="$(free_local_port)"
  marker="CI_STREAM_${index}_COMPLETE"
  prompt="Do not use tools. Print 800 numbered lines. Prefix every line with CI_STREAM_${index} and finish with exactly ${marker}."
  [[ "${mode}" == "http" ]] && extra=(-c 'model_providers.subrouter.supports_websockets=false')
  mkdir -p "${home}"
  "${TRANSPORT_OBSERVER}" --listen "127.0.0.1:${observer_port}" \
    --upstream "${CLIENT_BASE_URL}" --events "${observer_events}" \
    >"${observer_log}" 2>&1 &
  observer_pid=$!
  observer_pids+=("${observer_pid}")
  client_observer_events+=("${observer_events}")
  for _ in $(seq 1 100); do
    curl -fsS --max-time 1 "http://127.0.0.1:${observer_port}/_observer/health" >/dev/null 2>&1 && break
    kill -0 "${observer_pid}" 2>/dev/null || die "transport observer ${index} exited"
    sleep 0.05
  done
  curl -fsS --max-time 1 "http://127.0.0.1:${observer_port}/_observer/health" >/dev/null \
    || die "transport observer ${index} did not become ready"
  (
    env CODEX_HOME="${home}" \
      SUBROUTER_CODEX_BASE_URL="http://127.0.0.1:${observer_port}/v1" \
      SUBROUTER_CODEX_USER_EMAIL="gcp-deploy-ci@manaflow.ai" \
      SUBROUTER_CODEX_BIN="${CODEX_BINARY}" \
      "${CLIENT_BINARY}" codex exec --json --ignore-user-config --ignore-rules \
      --skip-git-repo-check -C "${WORK_DIR}" -s read-only -m "${CODEX_MODEL}" \
      "${extra[@]}" -o "${last_path}" "${prompt}"
  ) >"${log_path}" 2>&1 &
  client_wrapper_pids+=("$!")
  client_modes+=("${mode}")
  client_homes+=("${home}")
  client_logs+=("${log_path}")
  client_last_messages+=("${last_path}")
}

log "starting ${CLIENT_COUNT} unpaused Codex sessions on ${old_slot}"
for index in $(seq 1 "${CLIENT_COUNT}"); do
  if (( index % 2 == 0 )); then
    start_live_client "${index}" http
  else
    start_live_client "${index}" websocket
  fi
done
wait_for_front_connections "${old_slot}" "${MIN_DRAINED_CLIENTS}" \
  || die "front did not pin ${MIN_DRAINED_CLIENTS} sessions to ${old_slot}"

: >"${ARTIFACT_DIR}/transport-evidence.jsonl"
for index in "${!client_wrapper_pids[@]}"; do
  wrapper_pid="${client_wrapper_pids[${index}]}"
  mode="${client_modes[${index}]}"
  thread_id="$(wait_for_client_thread_id "${wrapper_pid}" "${client_logs[${index}]}")" \
    || die "Codex client $((index + 1)) did not report a thread id"
  observed="$(wait_for_observed_transport "${client_observer_events[${index}]}" "${mode}")" \
    || die "Codex client $((index + 1)) did not establish ${mode}"
  client_thread_ids+=("${thread_id}")
  jq -n --argjson client "$((index + 1))" --arg thread_id "${thread_id}" \
    --arg configured "${mode}" --arg observed "${observed}" \
    '{client:$client,thread_id:$thread_id,configured:$configured,observed:$observed}' \
    >>"${ARTIFACT_DIR}/transport-evidence.jsonl"
done
observed_transports_json="$(jq -s '[.[].observed] | unique' "${ARTIFACT_DIR}/transport-evidence.jsonl")"
jq -e 'index("http") != null and index("websocket") != null' \
  <<<"${observed_transports_json}" >/dev/null \
  || die "transport evidence did not cover HTTP and WebSocket"

log "persisting and switching front to ${candidate_slot}"
deployment_started=1
persist_front_slot "${candidate_slot}"
if ! switch_front "${candidate_slot}"; then
  persist_front_slot "${old_slot}" || true
  die "front did not activate ${candidate_slot}"
fi
disable_slot "${old_slot}"
switched_status="$(front_status)"
old_connections_after_switch="$(front_connections "${switched_status}" "${old_slot}")"
(( old_connections_after_switch >= MIN_DRAINED_CLIENTS )) \
  || die "slot switch cut live connections: ${old_connections_after_switch} remain"

run_codex_turn() {
  local home="$1" mode="$2" marker="$3" log_path="$4" last_path="$5"
  local extra=()
  [[ "${mode}" == "http" ]] && extra=(-c 'model_providers.subrouter.supports_websockets=false')
  env CODEX_HOME="${home}" SUBROUTER_CODEX_BASE_URL="${CLIENT_BASE_URL}/v1" \
    SUBROUTER_CODEX_USER_EMAIL="gcp-deploy-ci@manaflow.ai" \
    SUBROUTER_CODEX_BIN="${CODEX_BINARY}" \
    "${CLIENT_BINARY}" codex exec --json --ignore-user-config --ignore-rules \
    --skip-git-repo-check -C "${WORK_DIR}" -s read-only -m "${CODEX_MODEL}" \
    "${extra[@]}" -o "${last_path}" "Reply exactly ${marker}" >"${log_path}" 2>&1
  grep -Fq "${marker}" "${last_path}" || die "Codex turn did not return ${marker}"
}

new_home="${WORK_DIR}/codex-new"
mkdir -p "${new_home}"
run_codex_turn "${new_home}" websocket CANDIDATE_SLOT_OK \
  "${ARTIFACT_DIR}/candidate-slot.jsonl" "${ARTIFACT_DIR}/candidate-slot.last.txt"

deployment_action="deployed"
active_slot="${candidate_slot}"

wait_for_process() {
  local pid="$1" timeout_seconds="$2" waited=0
  while kill -0 "${pid}" 2>/dev/null; do
    (( waited < timeout_seconds * 10 )) || return 1
    sleep 0.1
    waited=$((waited + 1))
  done
  wait "${pid}"
}

thread_ids=()
for index in "${!client_wrapper_pids[@]}"; do
  wrapper_pid="${client_wrapper_pids[${index}]}"
  if ! wait_for_process "${wrapper_pid}" 300; then
    tail -n 60 "${client_logs[${index}]}" >&2 || true
    die "Codex client $((index + 1)) did not finish after the slot switch"
  fi
  marker="CI_STREAM_$((index + 1))_COMPLETE"
  grep -Fq "${marker}" "${client_last_messages[${index}]}" \
    || die "Codex client $((index + 1)) lost its completion marker"
  if grep -Eiq 'reconnecting|falling back|timed out|stream disconnected|error sending request' "${client_logs[${index}]}"; then
    tail -n 60 "${client_logs[${index}]}" >&2 || true
    die "Codex client $((index + 1)) reported a transport failure"
  fi
  thread_ids+=("${client_thread_ids[${index}]}")
done
client_wrapper_pids=()

log "resuming saved Codex threads through ${active_slot}"
for index in "${!thread_ids[@]}"; do
  mode="${client_modes[${index}]}"
  extra=()
  [[ "${mode}" == "http" ]] && extra=(-c 'model_providers.subrouter.supports_websockets=false')
  marker="RESUME_$((index + 1))_OK"
  resume_log="${ARTIFACT_DIR}/resume-$((index + 1))-${mode}.jsonl"
  resume_last="${ARTIFACT_DIR}/resume-$((index + 1))-${mode}.last.txt"
  env CODEX_HOME="${client_homes[${index}]}" \
    SUBROUTER_CODEX_BASE_URL="${CLIENT_BASE_URL}/v1" \
    SUBROUTER_CODEX_USER_EMAIL="gcp-deploy-ci@manaflow.ai" \
    SUBROUTER_CODEX_BIN="${CODEX_BINARY}" \
    "${CLIENT_BINARY}" codex exec --json --ignore-user-config --ignore-rules \
    --skip-git-repo-check -C "${WORK_DIR}" -s read-only -m "${CODEX_MODEL}" \
    "${extra[@]}" resume -o "${resume_last}" "${thread_ids[${index}]}" \
    "Reply exactly ${marker}" >"${resume_log}" 2>&1
  grep -Fq "${marker}" "${resume_last}" || die "saved thread $((index + 1)) did not resume"
done

restarts_after="$(service_restarts "${candidate_slot}")"
oom_after="$(service_oom_kills "${candidate_slot}")"
memory_peak="$(service_memory_peak "${candidate_slot}")"
front_peak="$(front_memory_peak)"
front_restarts_after="$(front_restarts)"
front_oom_after="$(front_oom_kills)"
[[ "${restarts_after}" == "${restarts_before}" ]] \
  || die "candidate restart count changed: ${restarts_before} -> ${restarts_after}"
[[ "${oom_after}" == "${oom_before}" ]] \
  || die "candidate OOM count changed: ${oom_before} -> ${oom_after}"
[[ "${front_restarts_after}" == "${front_restarts_before}" ]] \
  || die "front restart count changed: ${front_restarts_before} -> ${front_restarts_after}"
[[ "${front_oom_after}" == "${front_oom_before}" ]] \
  || die "front OOM count changed: ${front_oom_before} -> ${front_oom_after}"
[[ "${memory_peak}" =~ ^[0-9]+$ ]] || die "invalid candidate memory peak"
[[ "${front_peak}" =~ ^[0-9]+$ ]] || die "invalid front memory peak"
(( memory_peak <= MAX_MEMORY_BYTES )) \
  || die "candidate slot memory peak ${memory_peak} exceeds ${MAX_MEMORY_BYTES}"

if [[ "${DEPLOY_MODE}" == "rollback-rehearsal" ]]; then
  rollback_deployment || die "rollback rehearsal failed; candidate slot was preserved"
  deployment_action="rolled_back"
  active_slot="${old_slot}"
  run_codex_turn "${new_home}" websocket RESTORED_SLOT_OK \
    "${ARTIFACT_DIR}/restored-slot.jsonl" "${ARTIFACT_DIR}/restored-slot.last.txt"
  deployment_committed=1
else
  # Candidate validation is complete. From here onward a cleanup failure keeps
  # the candidate selected and preserves the old process until it drains.
  deployment_committed=1
  retire_slot "${old_slot}"
  wait_for_front_drained "${old_slot}"
  stop_drained_slot "${old_slot}"
fi

final_status="$(front_status)"
final_active="$(jq -r '.active.id // empty' <<<"${final_status}")"
[[ "${final_active}" == "${active_slot}" ]] || die "front active slot changed unexpectedly"
validate_front_slot_status "${final_status}" "${active_slot}" \
  || die "front final active slot metadata is inconsistent"
active_slot_address="$(slot_address "${active_slot}")"
gcloud_ssh "grep -qx 'SUBROUTER_FRONT_BACKEND_ID=${active_slot}' /etc/default/subrouter-front && grep -qx 'SUBROUTER_FRONT_BACKEND_NETWORK=tcp' /etc/default/subrouter-front && grep -qx 'SUBROUTER_FRONT_BACKEND_ADDRESS=${active_slot_address}' /etc/default/subrouter-front" \
  || die "persisted front backend does not match active ${active_slot}"
gcloud_ssh "systemctl is-enabled --quiet '$(slot_service "${active_slot}")' && systemctl is-active --quiet '$(slot_service "${active_slot}")'"
curl -fsS --max-time 5 "${TUNNEL_BASE_URL}/_subrouter/health" >/dev/null
curl -fsS --max-time 5 "${TUNNEL_BASE_URL}/_subrouter/ready" >/dev/null
curl -fsS --max-time 10 "${PUBLIC_BASE_URL}/_subrouter/health" >/dev/null
curl -fsS --max-time 10 "${PUBLIC_BASE_URL}/_subrouter/ready" >/dev/null
remote_release="/opt/subrouter/releases/${RELEASE_TAG}/subrouter"
remote_sum="$(gcloud_ssh "sudo sha256sum '${remote_release}' | awk '{print \$1}'" | tail -n 1)"
[[ "${remote_sum}" == "${EXPECTED_SHA256}" ]] || die "retained release checksum changed"

jq -n \
  --arg action "${deployment_action}" --arg mode "${DEPLOY_MODE}" \
  --arg project "${PROJECT_ID}" --arg zone "${ZONE}" --arg instance "${INSTANCE}" \
  --arg release_tag "${RELEASE_TAG}" --arg release_sha256 "${EXPECTED_SHA256}" \
  --arg deploy_revision "${DEPLOY_REVISION}" --arg old_slot "${old_slot}" \
  --arg candidate_slot "${candidate_slot}" --arg active_slot "${active_slot}" \
  --argjson pinned_connections "${old_connections_after_switch}" \
  --argjson codex_sessions "${CLIENT_COUNT}" --argjson transports "${observed_transports_json}" \
  --argjson restarts "${restarts_after}" --argjson oom_kills "${oom_after}" \
  --argjson front_restarts "${front_restarts_after}" --argjson front_oom_kills "${front_oom_after}" \
  --argjson slot_memory_peak_bytes "${memory_peak}" --argjson front_memory_peak_bytes "${front_peak}" \
  '{action:$action,mode:$mode,project:$project,zone:$zone,instance:$instance,
    release_tag:$release_tag,release_sha256:$release_sha256,deploy_revision:$deploy_revision,
    old_slot:$old_slot,candidate_slot:$candidate_slot,active_slot:$active_slot,
    pinned_connections:$pinned_connections,codex_sessions:$codex_sessions,
    transports:$transports,resumed_threads:$codex_sessions,restarts:$restarts,
    oom_kills:$oom_kills,slot_memory_peak_bytes:$slot_memory_peak_bytes,
    front_restarts:$front_restarts,front_oom_kills:$front_oom_kills,
    front_memory_peak_bytes:$front_memory_peak_bytes,rollback_verified:($action == "rolled_back")}' \
  >"${ARTIFACT_DIR}/result.json"

gcloud_ssh "rm -f '${REMOTE_CANDIDATE}' '${REMOTE_INSTALLER}'"
log "live ${DEPLOY_MODE} passed: ${old_slot} -> ${active_slot}, ${CLIENT_COUNT} unpaused sessions resumed"
